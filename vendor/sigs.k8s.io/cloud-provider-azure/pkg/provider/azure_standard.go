/*
Copyright 2020 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package provider

import (
	"context"
	"errors"
	"fmt"
	"hash/crc32"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"sigs.k8s.io/cloud-provider-azure/pkg/consts"

	"github.com/Azure/azure-sdk-for-go/services/compute/mgmt/2021-07-01/compute"
	"github.com/Azure/azure-sdk-for-go/services/network/mgmt/2021-08-01/network"
	"github.com/Azure/go-autorest/autorest/to"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/uuid"
	cloudprovider "k8s.io/cloud-provider"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"

	azcache "sigs.k8s.io/cloud-provider-azure/pkg/cache"
	"sigs.k8s.io/cloud-provider-azure/pkg/metrics"
)

var (
	errNotInVMSet      = errors.New("vm is not in the vmset")
	providerIDRE       = regexp.MustCompile(`.*/subscriptions/(?:.*)/Microsoft.Compute/virtualMachines/(.+)$`)
	backendPoolIDRE    = regexp.MustCompile(`^/subscriptions/(?:.*)/resourceGroups/(?:.*)/providers/Microsoft.Network/loadBalancers/(.+)/backendAddressPools/(?:.*)`)
	nicResourceGroupRE = regexp.MustCompile(`.*/subscriptions/(?:.*)/resourceGroups/(.+)/providers/Microsoft.Network/networkInterfaces/(?:.*)`)
	nicIDRE            = regexp.MustCompile(`(?i)/subscriptions/(?:.*)/resourceGroups/(.+)/providers/Microsoft.Network/networkInterfaces/(.+)/ipConfigurations/(?:.*)`)
	vmIDRE             = regexp.MustCompile(`(?i)/subscriptions/(?:.*)/resourceGroups/(?:.*)/providers/Microsoft.Compute/virtualMachines/(.+)`)
	vmasIDRE           = regexp.MustCompile(`/subscriptions/(?:.*)/resourceGroups/(?:.*)/providers/Microsoft.Compute/availabilitySets/(.+)`)
)

// getStandardMachineID returns the full identifier of a virtual machine.
func (az *Cloud) getStandardMachineID(subscriptionID, resourceGroup, machineName string) string {
	return fmt.Sprintf(
		consts.MachineIDTemplate,
		subscriptionID,
		strings.ToLower(resourceGroup),
		machineName)
}

// returns the full identifier of an availabilitySet
func (az *Cloud) getAvailabilitySetID(resourceGroup, availabilitySetName string) string {
	return fmt.Sprintf(
		consts.AvailabilitySetIDTemplate,
		az.SubscriptionID,
		resourceGroup,
		availabilitySetName)
}

// returns the full identifier of a loadbalancer frontendipconfiguration.
func (az *Cloud) getFrontendIPConfigID(lbName, rgName, fipConfigName string) string {
	return fmt.Sprintf(
		consts.FrontendIPConfigIDTemplate,
		az.getNetworkResourceSubscriptionID(),
		rgName,
		lbName,
		fipConfigName)
}

// returns the full identifier of a loadbalancer backendpool.
func (az *Cloud) getBackendPoolID(lbName, rgName, backendPoolName string) string {
	return fmt.Sprintf(
		consts.BackendPoolIDTemplate,
		az.getNetworkResourceSubscriptionID(),
		rgName,
		lbName,
		backendPoolName)
}

// returns the full identifier of a loadbalancer probe.
func (az *Cloud) getLoadBalancerProbeID(lbName, rgName, lbRuleName string) string {
	return fmt.Sprintf(
		consts.LoadBalancerProbeIDTemplate,
		az.getNetworkResourceSubscriptionID(),
		rgName,
		lbName,
		lbRuleName)
}

// getNetworkResourceSubscriptionID returns the subscription id which hosts network resources
func (az *Cloud) getNetworkResourceSubscriptionID() string {
	if az.Config.UsesNetworkResourceInDifferentTenantOrSubscription() {
		return az.NetworkResourceSubscriptionID
	}
	return az.SubscriptionID
}

func (az *Cloud) mapLoadBalancerNameToVMSet(lbName string, clusterName string) (vmSetName string) {
	vmSetName = strings.TrimSuffix(lbName, consts.InternalLoadBalancerNameSuffix)
	if strings.EqualFold(clusterName, vmSetName) {
		vmSetName = az.VMSet.GetPrimaryVMSetName()
	}

	return vmSetName
}

// For a load balancer, all frontend ip should reference either a subnet or publicIpAddress.
// Thus Azure do not allow mixed type (public and internal) load balancer.
// So we'd have a separate name for internal load balancer.
// This would be the name for Azure LoadBalancer resource.
func (az *Cloud) getAzureLoadBalancerName(clusterName string, vmSetName string, isInternal bool) string {
	if az.LoadBalancerName != "" {
		clusterName = az.LoadBalancerName
	}
	lbNamePrefix := vmSetName
	// The LB name prefix is set to the name of the cluster when:
	// 1. the LB belongs to the primary agent pool.
	// 2. using the single SLB.
	useSingleSLB := az.useStandardLoadBalancer() && !az.EnableMultipleStandardLoadBalancers
	if strings.EqualFold(vmSetName, az.VMSet.GetPrimaryVMSetName()) || useSingleSLB {
		lbNamePrefix = clusterName
	}
	// 3. using multiple SLBs while the vmSet is sharing the primary SLB
	useMultipleSLB := az.useStandardLoadBalancer() && az.EnableMultipleStandardLoadBalancers
	if useMultipleSLB && az.getVMSetNamesSharingPrimarySLB().Has(strings.ToLower(vmSetName)) {
		lbNamePrefix = clusterName
	}
	if isInternal {
		return fmt.Sprintf("%s%s", lbNamePrefix, consts.InternalLoadBalancerNameSuffix)
	}
	return lbNamePrefix
}

// isControlPlaneNode returns true if the node has a control-plane role label.
// The control-plane role is determined by looking for:
// * a node-role.kubernetes.io/control-plane or node-role.kubernetes.io/master="" label
func isControlPlaneNode(node *v1.Node) bool {
	if _, ok := node.Labels[consts.ControlPlaneNodeRoleLabel]; ok {
		return true
	}
	// include master role labels for k8s < 1.19
	if _, ok := node.Labels[consts.MasterNodeRoleLabel]; ok {
		return true
	}
	if val, ok := node.Labels[consts.NodeLabelRole]; ok && val == "master" {
		return true
	}
	return false
}

// returns the deepest child's identifier from a full identifier string.
func getLastSegment(ID, separator string) (string, error) {
	parts := strings.Split(ID, separator)
	name := parts[len(parts)-1]
	if len(name) == 0 {
		return "", fmt.Errorf("resource name was missing from identifier")
	}

	return name, nil
}

// returns the equivalent LoadBalancerRule, SecurityRule and LoadBalancerProbe
// protocol types for the given Kubernetes protocol type.
func getProtocolsFromKubernetesProtocol(protocol v1.Protocol) (*network.TransportProtocol, *network.SecurityRuleProtocol, *network.ProbeProtocol, error) {
	var transportProto network.TransportProtocol
	var securityProto network.SecurityRuleProtocol
	var probeProto network.ProbeProtocol

	switch protocol {
	case v1.ProtocolTCP:
		transportProto = network.TransportProtocolTCP
		securityProto = network.SecurityRuleProtocolTCP
		probeProto = network.ProbeProtocolTCP
		return &transportProto, &securityProto, &probeProto, nil
	case v1.ProtocolUDP:
		transportProto = network.TransportProtocolUDP
		securityProto = network.SecurityRuleProtocolUDP
		return &transportProto, &securityProto, nil, nil
	case v1.ProtocolSCTP:
		transportProto = network.TransportProtocolAll
		securityProto = network.SecurityRuleProtocolAsterisk
		return &transportProto, &securityProto, nil, nil
	default:
		return &transportProto, &securityProto, &probeProto, fmt.Errorf("only TCP, UDP and SCTP are supported for Azure LoadBalancers")
	}

}

// This returns the full identifier of the primary NIC for the given VM.
func getPrimaryInterfaceID(machine compute.VirtualMachine) (string, error) {
	if len(*machine.NetworkProfile.NetworkInterfaces) == 1 {
		return *(*machine.NetworkProfile.NetworkInterfaces)[0].ID, nil
	}

	for _, ref := range *machine.NetworkProfile.NetworkInterfaces {
		if to.Bool(ref.Primary) {
			return *ref.ID, nil
		}
	}

	return "", fmt.Errorf("failed to find a primary nic for the vm. vmname=%q", *machine.Name)
}

func getPrimaryIPConfig(nic network.Interface) (*network.InterfaceIPConfiguration, error) {
	if nic.IPConfigurations == nil {
		return nil, fmt.Errorf("nic.IPConfigurations for nic (nicname=%q) is nil", *nic.Name)
	}

	if len(*nic.IPConfigurations) == 1 {
		return &((*nic.IPConfigurations)[0]), nil
	}

	for _, ref := range *nic.IPConfigurations {
		if *ref.Primary {
			return &ref, nil
		}
	}

	return nil, fmt.Errorf("failed to determine the primary ipconfig. nicname=%q", *nic.Name)
}

// returns first ip configuration on a nic by family
func getIPConfigByIPFamily(nic network.Interface, IPv6 bool) (*network.InterfaceIPConfiguration, error) {
	if nic.IPConfigurations == nil {
		return nil, fmt.Errorf("nic.IPConfigurations for nic (nicname=%q) is nil", *nic.Name)
	}

	var ipVersion network.IPVersion
	if IPv6 {
		ipVersion = network.IPVersionIPv6
	} else {
		ipVersion = network.IPVersionIPv4
	}
	for _, ref := range *nic.IPConfigurations {
		if ref.PrivateIPAddress != nil && ref.PrivateIPAddressVersion == ipVersion {
			return &ref, nil
		}
	}
	return nil, fmt.Errorf("failed to determine the ipconfig(IPv6=%v). nicname=%q", IPv6, to.String(nic.Name))
}

func isInternalLoadBalancer(lb *network.LoadBalancer) bool {
	return strings.HasSuffix(*lb.Name, consts.InternalLoadBalancerNameSuffix)
}

// getBackendPoolName the LB BackendPool name for a service.
// to ensure backward and forward compat:
// SingleStack -v4 (pre v1.16) => BackendPool name == clusterName
// SingleStack -v6 => BackendPool name == <clusterName>-IPv6 (all cluster bootstrap uses this name)
// DualStack
// => IPv4 BackendPool name == clusterName
// => IPv6 BackendPool name == <clusterName>-IPv6
// This means:
// clusters moving from IPv4 to dualstack will require no changes
// clusters moving from IPv6 to dualstack will require no changes as the IPv4 backend pool will created with <clusterName>
func getBackendPoolName(clusterName string, service *v1.Service) string {
	IPv6 := utilnet.IsIPv6String(service.Spec.ClusterIP)
	if IPv6 {
		return fmt.Sprintf("%v-IPv6", clusterName)
	}

	return clusterName
}

func (az *Cloud) getLoadBalancerRuleName(service *v1.Service, protocol v1.Protocol, port int32) string {
	prefix := az.getRulePrefix(service)
	ruleName := fmt.Sprintf("%s-%s-%d", prefix, protocol, port)
	subnet := subnet(service)
	if subnet == nil {
		return ruleName
	}

	// Load balancer rule name must be less or equal to 80 characters, so excluding the hyphen two segments cannot exceed 79
	subnetSegment := *subnet
	if len(ruleName)+len(subnetSegment)+1 > consts.LoadBalancerRuleNameMaxLength {
		subnetSegment = subnetSegment[:consts.LoadBalancerRuleNameMaxLength-len(ruleName)-1]
	}

	return fmt.Sprintf("%s-%s-%s-%d", prefix, subnetSegment, protocol, port)
}

func (az *Cloud) getloadbalancerHAmodeRuleName(service *v1.Service) string {
	return az.getLoadBalancerRuleName(service, service.Spec.Ports[0].Protocol, service.Spec.Ports[0].Port)
}

func (az *Cloud) getSecurityRuleName(service *v1.Service, port v1.ServicePort, sourceAddrPrefix string) string {
	if useSharedSecurityRule(service) {
		safePrefix := strings.Replace(sourceAddrPrefix, "/", "_", -1)
		return fmt.Sprintf("shared-%s-%d-%s", port.Protocol, port.Port, safePrefix)
	}
	safePrefix := strings.Replace(sourceAddrPrefix, "/", "_", -1)
	rulePrefix := az.getRulePrefix(service)
	return fmt.Sprintf("%s-%s-%d-%s", rulePrefix, port.Protocol, port.Port, safePrefix)
}

// This returns a human-readable version of the Service used to tag some resources.
// This is only used for human-readable convenience, and not to filter.
func getServiceName(service *v1.Service) string {
	return fmt.Sprintf("%s/%s", service.Namespace, service.Name)
}

// This returns a prefix for loadbalancer/security rules.
func (az *Cloud) getRulePrefix(service *v1.Service) string {
	return az.GetLoadBalancerName(context.TODO(), "", service)
}

func (az *Cloud) getPublicIPName(clusterName string, service *v1.Service) string {
	pipName := fmt.Sprintf("%s-%s", clusterName, az.GetLoadBalancerName(context.TODO(), clusterName, service))
	if prefixID, ok := service.Annotations[consts.ServiceAnnotationPIPPrefixID]; ok && prefixID != "" {
		prefixName, err := getLastSegment(prefixID, "/")
		if err != nil {
			return pipName
		}
		pipName = fmt.Sprintf("%s-%s", pipName, prefixName)
	}
	return pipName
}

func (az *Cloud) serviceOwnsRule(service *v1.Service, rule string) bool {
	prefix := az.getRulePrefix(service)
	return strings.HasPrefix(strings.ToUpper(rule), strings.ToUpper(prefix))
}

// There are two cases when a service owns the frontend IP config:
// 1. The primary service, which means the frontend IP config is created after the creation of the service.
// This means the name of the config can be tracked by the service UID.
// 2. The secondary services must have their loadBalancer IP set if they want to share the same config as the primary
// service. Hence, it can be tracked by the loadBalancer IP.
func (az *Cloud) serviceOwnsFrontendIP(fip network.FrontendIPConfiguration, service *v1.Service, pips *[]network.PublicIPAddress) (bool, bool, error) {
	var isPrimaryService bool
	baseName := az.GetLoadBalancerName(context.TODO(), "", service)
	if strings.HasPrefix(to.String(fip.Name), baseName) {
		klog.V(6).Infof("serviceOwnsFrontendIP: found primary service %s of the frontend IP config %s", service.Name, *fip.Name)
		isPrimaryService = true
		return true, isPrimaryService, nil
	}

	loadBalancerIP := service.Spec.LoadBalancerIP
	if loadBalancerIP == "" {
		// it is a must that the secondary services set the loadBalancer IP
		return false, isPrimaryService, nil
	}

	// for external secondary service the public IP address should be checked
	if !requiresInternalLoadBalancer(service) {
		pipResourceGroup := az.getPublicIPAddressResourceGroup(service)
		pip, err := az.findMatchedPIPByLoadBalancerIP(service, loadBalancerIP, pipResourceGroup, pips)
		if err != nil {
			klog.Warningf("serviceOwnsFrontendIP: unexpected error when finding match public IP of the service %s with loadBalancerLP %s: %v", service.Name, loadBalancerIP, err)
			return false, isPrimaryService, nil
		}

		if pip != nil &&
			pip.ID != nil &&
			pip.PublicIPAddressPropertiesFormat != nil &&
			pip.IPAddress != nil &&
			fip.FrontendIPConfigurationPropertiesFormat != nil &&
			fip.FrontendIPConfigurationPropertiesFormat.PublicIPAddress != nil {
			if strings.EqualFold(to.String(pip.ID), to.String(fip.PublicIPAddress.ID)) {
				klog.V(4).Infof("serviceOwnsFrontendIP: found secondary service %s of the frontend IP config %s", service.Name, *fip.Name)

				return true, isPrimaryService, nil
			}
			klog.V(4).Infof("serviceOwnsFrontendIP: the public IP with ID %s is being referenced by other service with public IP address %s", *pip.ID, *pip.IPAddress)
		}

		return false, isPrimaryService, nil
	}

	// for internal secondary service the private IP address on the frontend IP config should be checked
	if fip.PrivateIPAddress == nil {
		return false, isPrimaryService, nil
	}

	return strings.EqualFold(*fip.PrivateIPAddress, loadBalancerIP), isPrimaryService, nil
}

func (az *Cloud) getDefaultFrontendIPConfigName(service *v1.Service) string {
	baseName := az.GetLoadBalancerName(context.TODO(), "", service)
	subnetName := subnet(service)
	if subnetName != nil {
		ipcName := fmt.Sprintf("%s-%s", baseName, *subnetName)

		// Azure lb front end configuration name must not exceed 80 characters
		if len(ipcName) > consts.FrontendIPConfigNameMaxLength {
			ipcName = ipcName[:consts.FrontendIPConfigNameMaxLength]
			// Cutting the string may result in char like "-" as the string end.
			// If the last char is not a letter or '_', replace it with "_".
			if !unicode.IsLetter(rune(ipcName[len(ipcName)-1:][0])) && ipcName[len(ipcName)-1:] != "_" {
				ipcName = ipcName[:len(ipcName)-1] + "_"
			}
		}
		return ipcName
	}
	return baseName
}

// This returns the next available rule priority level for a given set of security rules.
func getNextAvailablePriority(rules []network.SecurityRule) (int32, error) {
	var smallest int32 = consts.LoadBalancerMinimumPriority
	var spread int32 = 1

outer:
	for smallest < consts.LoadBalancerMaximumPriority {
		for _, rule := range rules {
			if *rule.Priority == smallest {
				smallest += spread
				continue outer
			}
		}
		// no one else had it
		return smallest, nil
	}

	return -1, fmt.Errorf("securityGroup priorities are exhausted")
}

var polyTable = crc32.MakeTable(crc32.Koopman)

// MakeCRC32 : convert string to CRC32 format
func MakeCRC32(str string) string {
	crc := crc32.New(polyTable)
	_, _ = crc.Write([]byte(str))
	hash := crc.Sum32()
	return strconv.FormatUint(uint64(hash), 10)
}

// availabilitySet implements VMSet interface for Azure availability sets.
type availabilitySet struct {
	*Cloud

	vmasCache *azcache.TimedCache
}

type availabilitySetEntry struct {
	vmas          *compute.AvailabilitySet
	resourceGroup string
}

func (as *availabilitySet) newVMASCache() (*azcache.TimedCache, error) {
	getter := func(key string) (interface{}, error) {
		localCache := &sync.Map{}

		allResourceGroups, err := as.GetResourceGroups()
		if err != nil {
			return nil, err
		}

		for _, resourceGroup := range allResourceGroups.List() {
			allAvailabilitySets, rerr := as.AvailabilitySetsClient.List(context.Background(), resourceGroup)
			if rerr != nil {
				klog.Errorf("AvailabilitySetsClient.List failed: %v", rerr)
				return nil, rerr.Error()
			}

			for i := range allAvailabilitySets {
				vmas := allAvailabilitySets[i]
				if strings.EqualFold(to.String(vmas.Name), "") {
					klog.Warning("failed to get the name of the VMAS")
					continue
				}
				localCache.Store(to.String(vmas.Name), &availabilitySetEntry{
					vmas:          &vmas,
					resourceGroup: resourceGroup,
				})
			}
		}

		return localCache, nil
	}

	if as.Config.AvailabilitySetsCacheTTLInSeconds == 0 {
		as.Config.AvailabilitySetsCacheTTLInSeconds = consts.VMASCacheTTLDefaultInSeconds
	}

	return azcache.NewTimedcache(time.Duration(as.Config.AvailabilitySetsCacheTTLInSeconds)*time.Second, getter)
}

// newStandardSet creates a new availabilitySet.
func newAvailabilitySet(az *Cloud) (VMSet, error) {
	as := &availabilitySet{
		Cloud: az,
	}

	var err error
	as.vmasCache, err = as.newVMASCache()
	if err != nil {
		return nil, err
	}

	return as, nil
}

// GetInstanceIDByNodeName gets the cloud provider ID by node name.
// It must return ("", cloudprovider.InstanceNotFound) if the instance does
// not exist or is no longer running.
func (as *availabilitySet) GetInstanceIDByNodeName(name string) (string, error) {
	var machine compute.VirtualMachine
	var err error

	machine, err = as.getVirtualMachine(types.NodeName(name), azcache.CacheReadTypeUnsafe)
	if errors.Is(err, cloudprovider.InstanceNotFound) {
		klog.Warningf("Unable to find node %s: %v", name, cloudprovider.InstanceNotFound)
		return "", cloudprovider.InstanceNotFound
	}
	if err != nil {
		if as.CloudProviderBackoff {
			klog.V(2).Infof("GetInstanceIDByNodeName(%s) backing off", name)
			machine, err = as.GetVirtualMachineWithRetry(types.NodeName(name), azcache.CacheReadTypeUnsafe)
			if err != nil {
				klog.V(2).Infof("GetInstanceIDByNodeName(%s) abort backoff", name)
				return "", err
			}
		} else {
			return "", err
		}
	}

	resourceID := *machine.ID
	convertedResourceID, err := ConvertResourceGroupNameToLower(resourceID)
	if err != nil {
		klog.Errorf("ConvertResourceGroupNameToLower failed with error: %v", err)
		return "", err
	}
	return convertedResourceID, nil
}

// GetPowerStatusByNodeName returns the power state of the specified node.
func (as *availabilitySet) GetPowerStatusByNodeName(name string) (powerState string, err error) {
	vm, err := as.getVirtualMachine(types.NodeName(name), azcache.CacheReadTypeDefault)
	if err != nil {
		return powerState, err
	}

	if vm.InstanceView != nil && vm.InstanceView.Statuses != nil {
		statuses := *vm.InstanceView.Statuses
		for _, status := range statuses {
			state := to.String(status.Code)
			if strings.HasPrefix(state, vmPowerStatePrefix) {
				return strings.TrimPrefix(state, vmPowerStatePrefix), nil
			}
		}
	}

	// vm.InstanceView or vm.InstanceView.Statuses are nil when the VM is under deleting.
	klog.V(3).Infof("InstanceView for node %q is nil, assuming it's stopped", name)
	return vmPowerStateStopped, nil
}

// GetProvisioningStateByNodeName returns the provisioningState for the specified node.
func (as *availabilitySet) GetProvisioningStateByNodeName(name string) (provisioningState string, err error) {
	vm, err := as.getVirtualMachine(types.NodeName(name), azcache.CacheReadTypeDefault)
	if err != nil {
		return provisioningState, err
	}

	if vm.VirtualMachineProperties == nil || vm.VirtualMachineProperties.ProvisioningState == nil {
		return provisioningState, nil
	}

	return to.String(vm.VirtualMachineProperties.ProvisioningState), nil
}

// GetNodeNameByProviderID gets the node name by provider ID.
func (as *availabilitySet) GetNodeNameByProviderID(providerID string) (types.NodeName, error) {
	// NodeName is part of providerID for standard instances.
	matches := providerIDRE.FindStringSubmatch(providerID)
	if len(matches) != 2 {
		return "", errors.New("error splitting providerID")
	}

	return types.NodeName(matches[1]), nil
}

// GetInstanceTypeByNodeName gets the instance type by node name.
func (as *availabilitySet) GetInstanceTypeByNodeName(name string) (string, error) {
	machine, err := as.getVirtualMachine(types.NodeName(name), azcache.CacheReadTypeUnsafe)
	if err != nil {
		klog.Errorf("as.GetInstanceTypeByNodeName(%s) failed: as.getVirtualMachine(%s) err=%v", name, name, err)
		return "", err
	}

	if machine.HardwareProfile == nil {
		return "", fmt.Errorf("HardwareProfile of node(%s) is nil", name)
	}
	return string(machine.HardwareProfile.VMSize), nil
}

// GetZoneByNodeName gets availability zone for the specified node. If the node is not running
// with availability zone, then it returns fault domain.
// for details, refer to https://kubernetes-sigs.github.io/cloud-provider-azure/topics/availability-zones/#node-labels
func (as *availabilitySet) GetZoneByNodeName(name string) (cloudprovider.Zone, error) {
	vm, err := as.getVirtualMachine(types.NodeName(name), azcache.CacheReadTypeUnsafe)
	if err != nil {
		return cloudprovider.Zone{}, err
	}

	var failureDomain string
	if vm.Zones != nil && len(*vm.Zones) > 0 {
		// Get availability zone for the node.
		zones := *vm.Zones
		zoneID, err := strconv.Atoi(zones[0])
		if err != nil {
			return cloudprovider.Zone{}, fmt.Errorf("failed to parse zone %q: %w", zones, err)
		}

		failureDomain = as.makeZone(to.String(vm.Location), zoneID)
	} else {
		// Availability zone is not used for the node, falling back to fault domain.
		failureDomain = strconv.Itoa(int(to.Int32(vm.VirtualMachineProperties.InstanceView.PlatformFaultDomain)))
	}

	zone := cloudprovider.Zone{
		FailureDomain: strings.ToLower(failureDomain),
		Region:        strings.ToLower(to.String(vm.Location)),
	}
	return zone, nil
}

// GetPrimaryVMSetName returns the VM set name depending on the configured vmType.
// It returns config.PrimaryScaleSetName for vmss and config.PrimaryAvailabilitySetName for standard vmType.
func (as *availabilitySet) GetPrimaryVMSetName() string {
	return as.Config.PrimaryAvailabilitySetName
}

// GetIPByNodeName gets machine private IP and public IP by node name.
func (as *availabilitySet) GetIPByNodeName(name string) (string, string, error) {
	nic, err := as.GetPrimaryInterface(name)
	if err != nil {
		return "", "", err
	}

	ipConfig, err := getPrimaryIPConfig(nic)
	if err != nil {
		klog.Errorf("as.GetIPByNodeName(%s) failed: getPrimaryIPConfig(%v), err=%v", name, nic, err)
		return "", "", err
	}

	privateIP := *ipConfig.PrivateIPAddress
	publicIP := ""
	if ipConfig.PublicIPAddress != nil && ipConfig.PublicIPAddress.ID != nil {
		pipID := *ipConfig.PublicIPAddress.ID
		pipName, err := getLastSegment(pipID, "/")
		if err != nil {
			return "", "", fmt.Errorf("failed to publicIP name for node %q with pipID %q", name, pipID)
		}
		pip, existsPip, err := as.getPublicIPAddress(as.ResourceGroup, pipName, azcache.CacheReadTypeDefault)
		if err != nil {
			return "", "", err
		}
		if existsPip {
			publicIP = *pip.IPAddress
		}
	}

	return privateIP, publicIP, nil
}

// returns a list of private ips assigned to node
// TODO (khenidak): This should read all nics, not just the primary
// allowing users to split ipv4/v6 on multiple nics
func (as *availabilitySet) GetPrivateIPsByNodeName(name string) ([]string, error) {
	ips := make([]string, 0)
	nic, err := as.GetPrimaryInterface(name)
	if err != nil {
		return ips, err
	}

	if nic.IPConfigurations == nil {
		return ips, fmt.Errorf("nic.IPConfigurations for nic (nicname=%q) is nil", *nic.Name)
	}

	for _, ipConfig := range *(nic.IPConfigurations) {
		if ipConfig.PrivateIPAddress != nil {
			ips = append(ips, *(ipConfig.PrivateIPAddress))
		}
	}

	return ips, nil
}

// getAgentPoolAvailabilitySets lists the virtual machines for the resource group and then builds
// a list of availability sets that match the nodes available to k8s.
func (as *availabilitySet) getAgentPoolAvailabilitySets(vms []compute.VirtualMachine, nodes []*v1.Node) (agentPoolAvailabilitySets *[]string, err error) {
	vmNameToAvailabilitySetID := make(map[string]string, len(vms))
	for vmx := range vms {
		vm := vms[vmx]
		if vm.AvailabilitySet != nil {
			vmNameToAvailabilitySetID[*vm.Name] = *vm.AvailabilitySet.ID
		}
	}
	agentPoolAvailabilitySets = &[]string{}
	for nx := range nodes {
		nodeName := (*nodes[nx]).Name
		if isControlPlaneNode(nodes[nx]) {
			continue
		}
		asID, ok := vmNameToAvailabilitySetID[nodeName]
		if !ok {
			klog.Warningf("as.getNodeAvailabilitySet - Node(%s) has no availability sets", nodeName)
			continue
		}
		asName, err := getLastSegment(asID, "/")
		if err != nil {
			klog.Errorf("as.getNodeAvailabilitySet - Node (%s)- getLastSegment(%s), err=%v", nodeName, asID, err)
			return nil, err
		}
		// AvailabilitySet ID is currently upper cased in a non-deterministic way
		// We want to keep it lower case, before the ID get fixed
		asName = strings.ToLower(asName)

		*agentPoolAvailabilitySets = append(*agentPoolAvailabilitySets, asName)
	}

	return agentPoolAvailabilitySets, nil
}

// GetVMSetNames selects all possible availability sets or scale sets
// (depending vmType configured) for service load balancer, if the service has
// no loadbalancer mode annotation returns the primary VMSet. If service annotation
// for loadbalancer exists then returns the eligible VMSet. The mode selection
// annotation would be ignored when using one SLB per cluster.
func (as *availabilitySet) GetVMSetNames(service *v1.Service, nodes []*v1.Node) (availabilitySetNames *[]string, err error) {
	hasMode, isAuto, serviceAvailabilitySetName := as.getServiceLoadBalancerMode(service)
	useSingleSLB := as.useStandardLoadBalancer() && !as.EnableMultipleStandardLoadBalancers
	if !hasMode || useSingleSLB {
		// no mode specified in service annotation or use single SLB mode
		// default to PrimaryAvailabilitySetName
		availabilitySetNames = &[]string{as.Config.PrimaryAvailabilitySetName}
		return availabilitySetNames, nil
	}

	vms, err := as.ListVirtualMachines(as.ResourceGroup)
	if err != nil {
		klog.Errorf("as.getNodeAvailabilitySet - ListVirtualMachines failed, err=%v", err)
		return nil, err
	}
	availabilitySetNames, err = as.getAgentPoolAvailabilitySets(vms, nodes)
	if err != nil {
		klog.Errorf("as.GetVMSetNames - getAgentPoolAvailabilitySets failed err=(%v)", err)
		return nil, err
	}
	if len(*availabilitySetNames) == 0 {
		klog.Errorf("as.GetVMSetNames - No availability sets found for nodes in the cluster, node count(%d)", len(nodes))
		return nil, fmt.Errorf("no availability sets found for nodes, node count(%d)", len(nodes))
	}
	if !isAuto {
		found := false
		for asx := range *availabilitySetNames {
			if strings.EqualFold((*availabilitySetNames)[asx], serviceAvailabilitySetName) {
				found = true
				break
			}
		}
		if !found {
			klog.Errorf("as.GetVMSetNames - Availability set (%s) in service annotation not found", serviceAvailabilitySetName)
			return nil, fmt.Errorf("availability set (%s) - not found", serviceAvailabilitySetName)
		}
		return &[]string{serviceAvailabilitySetName}, nil
	}

	return availabilitySetNames, nil
}

func (as *availabilitySet) GetNodeVMSetName(node *v1.Node) (string, error) {
	var hostName string
	for _, nodeAddress := range node.Status.Addresses {
		if strings.EqualFold(string(nodeAddress.Type), string(v1.NodeHostName)) {
			hostName = nodeAddress.Address
		}
	}
	if hostName == "" {
		if name, ok := node.Labels[consts.NodeLabelHostName]; ok {
			hostName = name
		}
	}
	if hostName == "" {
		klog.Warningf("as.GetNodeVMSetName: cannot get host name from node %s", node.Name)
		return "", nil
	}

	vms, err := as.ListVirtualMachines(as.ResourceGroup)
	if err != nil {
		klog.Errorf("as.GetNodeVMSetName - ListVirtualMachines failed, err=%v", err)
		return "", err
	}

	var asName string
	for _, vm := range vms {
		if strings.EqualFold(to.String(vm.Name), hostName) {
			if vm.AvailabilitySet != nil && to.String(vm.AvailabilitySet.ID) != "" {
				klog.V(4).Infof("as.GetNodeVMSetName: found vm %s", hostName)

				asName, err = getLastSegment(to.String(vm.AvailabilitySet.ID), "/")
				if err != nil {
					klog.Errorf("as.GetNodeVMSetName: failed to get last segment of ID %s: %s", to.String(vm.AvailabilitySet.ID), err)
					return "", err
				}
			}

			break
		}
	}

	klog.V(4).Infof("as.GetNodeVMSetName: found availability set name %s from node name %s", asName, node.Name)
	return asName, nil
}

// GetPrimaryInterface gets machine primary network interface by node name.
func (as *availabilitySet) GetPrimaryInterface(nodeName string) (network.Interface, error) {
	nic, _, err := as.getPrimaryInterfaceWithVMSet(nodeName, "")
	return nic, err
}

// extractResourceGroupByNicID extracts the resource group name by nicID.
func extractResourceGroupByNicID(nicID string) (string, error) {
	matches := nicResourceGroupRE.FindStringSubmatch(nicID)
	if len(matches) != 2 {
		return "", fmt.Errorf("error of extracting resourceGroup from nicID %q", nicID)
	}

	return matches[1], nil
}

// getPrimaryInterfaceWithVMSet gets machine primary network interface by node name and vmSet.
func (as *availabilitySet) getPrimaryInterfaceWithVMSet(nodeName, vmSetName string) (network.Interface, string, error) {
	var machine compute.VirtualMachine

	machine, err := as.GetVirtualMachineWithRetry(types.NodeName(nodeName), azcache.CacheReadTypeDefault)
	if err != nil {
		klog.V(2).Infof("GetPrimaryInterface(%s, %s) abort backoff", nodeName, vmSetName)
		return network.Interface{}, "", err
	}

	primaryNicID, err := getPrimaryInterfaceID(machine)
	if err != nil {
		return network.Interface{}, "", err
	}
	nicName, err := getLastSegment(primaryNicID, "/")
	if err != nil {
		return network.Interface{}, "", err
	}
	nodeResourceGroup, err := as.GetNodeResourceGroup(nodeName)
	if err != nil {
		return network.Interface{}, "", err
	}

	// Check availability set name. Note that vmSetName is empty string when getting
	// the Node's IP address. While vmSetName is not empty, it should be checked with
	// Node's real availability set name:
	// - For basic SKU load balancer, errNotInVMSet should be returned if the node's
	//   availability set is mismatched with vmSetName.
	// - For single standard SKU load balancer, backend could belong to multiple VMAS, so we
	//   don't check vmSet for it.
	// - For multiple standard SKU load balancers, the behavior is similar to the basic LB.
	needCheck := false
	if !as.useStandardLoadBalancer() {
		// need to check the vmSet name when using the basic LB
		needCheck = true
	} else if as.EnableMultipleStandardLoadBalancers {
		// need to check the vmSet name when using multiple standard LBs
		needCheck = true

		// ensure the vm that is supposed to share the primary SLB in the backendpool of the primary SLB
		if machine.AvailabilitySet != nil {
			vmasName, _ := getLastSegment(to.String(machine.AvailabilitySet.ID), "/")
			if strings.EqualFold(as.GetPrimaryVMSetName(), vmSetName) &&
				as.getVMSetNamesSharingPrimarySLB().Has(strings.ToLower(vmasName)) {
				klog.V(4).Infof("getPrimaryInterfaceWithVMSet: the vm %s in the vmSet %s is supposed to share the primary SLB", nodeName, vmasName)
				needCheck = false
			}
		}
	}
	if vmSetName != "" && needCheck {
		expectedAvailabilitySetID := as.getAvailabilitySetID(nodeResourceGroup, vmSetName)
		if machine.AvailabilitySet == nil || !strings.EqualFold(*machine.AvailabilitySet.ID, expectedAvailabilitySetID) {
			klog.V(3).Infof(
				"GetPrimaryInterface: nic (%s) is not in the availabilitySet(%s)", nicName, vmSetName)
			return network.Interface{}, "", errNotInVMSet
		}
	}

	nicResourceGroup, err := extractResourceGroupByNicID(primaryNicID)
	if err != nil {
		return network.Interface{}, "", err
	}

	ctx, cancel := getContextWithCancel()
	defer cancel()
	nic, rerr := as.InterfacesClient.Get(ctx, nicResourceGroup, nicName, "")
	if rerr != nil {
		return network.Interface{}, "", rerr.Error()
	}

	var availabilitySetID string
	if machine.VirtualMachineProperties != nil && machine.AvailabilitySet != nil {
		availabilitySetID = to.String(machine.AvailabilitySet.ID)
	}
	return nic, availabilitySetID, nil
}

// EnsureHostInPool ensures the given VM's Primary NIC's Primary IP Configuration is
// participating in the specified LoadBalancer Backend Pool.
func (as *availabilitySet) EnsureHostInPool(service *v1.Service, nodeName types.NodeName, backendPoolID string, vmSetName string) (string, string, string, *compute.VirtualMachineScaleSetVM, error) {
	vmName := mapNodeNameToVMName(nodeName)
	serviceName := getServiceName(service)
	nic, _, err := as.getPrimaryInterfaceWithVMSet(vmName, vmSetName)
	if err != nil {
		if errors.Is(err, errNotInVMSet) {
			klog.V(3).Infof("EnsureHostInPool skips node %s because it is not in the vmSet %s", nodeName, vmSetName)
			return "", "", "", nil, nil
		}

		klog.Errorf("error: az.EnsureHostInPool(%s), az.VMSet.GetPrimaryInterface.Get(%s, %s), err=%v", nodeName, vmName, vmSetName, err)
		return "", "", "", nil, err
	}

	if nic.ProvisioningState == consts.NicFailedState {
		klog.Warningf("EnsureHostInPool skips node %s because its primary nic %s is in Failed state", nodeName, *nic.Name)
		return "", "", "", nil, nil
	}

	var primaryIPConfig *network.InterfaceIPConfiguration
	ipv6 := utilnet.IsIPv6String(service.Spec.ClusterIP)
	if !as.Cloud.ipv6DualStackEnabled && !ipv6 {
		primaryIPConfig, err = getPrimaryIPConfig(nic)
		if err != nil {
			return "", "", "", nil, err
		}
	} else {
		primaryIPConfig, err = getIPConfigByIPFamily(nic, ipv6)
		if err != nil {
			return "", "", "", nil, err
		}
	}

	foundPool := false
	newBackendPools := []network.BackendAddressPool{}
	if primaryIPConfig.LoadBalancerBackendAddressPools != nil {
		newBackendPools = *primaryIPConfig.LoadBalancerBackendAddressPools
	}
	for _, existingPool := range newBackendPools {
		if strings.EqualFold(backendPoolID, *existingPool.ID) {
			foundPool = true
			break
		}
	}
	if !foundPool {
		if as.useStandardLoadBalancer() && len(newBackendPools) > 0 {
			// Although standard load balancer supports backends from multiple availability
			// sets, the same network interface couldn't be added to more than one load balancer of
			// the same type. Omit those nodes (e.g. masters) so Azure ARM won't complain
			// about this.
			newBackendPoolsIDs := make([]string, 0, len(newBackendPools))
			for _, pool := range newBackendPools {
				if pool.ID != nil {
					newBackendPoolsIDs = append(newBackendPoolsIDs, *pool.ID)
				}
			}
			isSameLB, oldLBName, err := isBackendPoolOnSameLB(backendPoolID, newBackendPoolsIDs)
			if err != nil {
				return "", "", "", nil, err
			}
			if !isSameLB {
				klog.V(4).Infof("Node %q has already been added to LB %q, omit adding it to a new one", nodeName, oldLBName)
				return "", "", "", nil, nil
			}
		}

		newBackendPools = append(newBackendPools,
			network.BackendAddressPool{
				ID: to.StringPtr(backendPoolID),
			})

		primaryIPConfig.LoadBalancerBackendAddressPools = &newBackendPools

		nicName := *nic.Name
		klog.V(3).Infof("nicupdate(%s): nic(%s) - updating", serviceName, nicName)
		err := as.CreateOrUpdateInterface(service, nic)
		if err != nil {
			return "", "", "", nil, err
		}
	}
	return "", "", "", nil, nil
}

// EnsureHostsInPool ensures the given Node's primary IP configurations are
// participating in the specified LoadBalancer Backend Pool.
func (as *availabilitySet) EnsureHostsInPool(service *v1.Service, nodes []*v1.Node, backendPoolID string, vmSetName string) error {
	mc := metrics.NewMetricContext("services", "vmas_ensure_hosts_in_pool", as.ResourceGroup, as.SubscriptionID, getServiceName(service))
	isOperationSucceeded := false
	defer func() {
		mc.ObserveOperationWithResult(isOperationSucceeded)
	}()

	hostUpdates := make([]func() error, 0, len(nodes))
	for _, node := range nodes {
		localNodeName := node.Name
		if as.useStandardLoadBalancer() && as.excludeMasterNodesFromStandardLB() && isControlPlaneNode(node) {
			klog.V(4).Infof("Excluding master node %q from load balancer backendpool %q", localNodeName, backendPoolID)
			continue
		}

		shouldExcludeLoadBalancer, err := as.ShouldNodeExcludedFromLoadBalancer(localNodeName)
		if err != nil {
			klog.Errorf("ShouldNodeExcludedFromLoadBalancer(%s) failed with error: %v", localNodeName, err)
			return err
		}
		if shouldExcludeLoadBalancer {
			klog.V(4).Infof("Excluding unmanaged/external-resource-group node %q", localNodeName)
			continue
		}

		f := func() error {
			_, _, _, _, err := as.EnsureHostInPool(service, types.NodeName(localNodeName), backendPoolID, vmSetName)
			if err != nil {
				return fmt.Errorf("ensure(%s): backendPoolID(%s) - failed to ensure host in pool: %w", getServiceName(service), backendPoolID, err)
			}
			return nil
		}
		hostUpdates = append(hostUpdates, f)
	}

	errs := utilerrors.AggregateGoroutines(hostUpdates...)
	if errs != nil {
		return utilerrors.Flatten(errs)
	}

	isOperationSucceeded = true
	return nil
}

// EnsureBackendPoolDeleted ensures the loadBalancer backendAddressPools deleted from the specified nodes.
func (as *availabilitySet) EnsureBackendPoolDeleted(service *v1.Service, backendPoolID, vmSetName string, backendAddressPools *[]network.BackendAddressPool, deleteFromVMSet bool) error {
	// Returns nil if backend address pools already deleted.
	if backendAddressPools == nil {
		return nil
	}

	mc := metrics.NewMetricContext("services", "vmas_ensure_backend_pool_deleted", as.ResourceGroup, as.SubscriptionID, getServiceName(service))
	isOperationSucceeded := false
	defer func() {
		mc.ObserveOperationWithResult(isOperationSucceeded)
	}()

	ipConfigurationIDs := []string{}
	for _, backendPool := range *backendAddressPools {
		if strings.EqualFold(to.String(backendPool.ID), backendPoolID) &&
			backendPool.BackendAddressPoolPropertiesFormat != nil &&
			backendPool.BackendIPConfigurations != nil {
			for _, ipConf := range *backendPool.BackendIPConfigurations {
				if ipConf.ID == nil {
					continue
				}

				ipConfigurationIDs = append(ipConfigurationIDs, *ipConf.ID)
			}
		}
	}
	nicUpdaters := make([]func() error, 0)
	allErrs := make([]error, 0)
	for i := range ipConfigurationIDs {
		ipConfigurationID := ipConfigurationIDs[i]
		nodeName, _, err := as.GetNodeNameByIPConfigurationID(ipConfigurationID)
		if err != nil && !errors.Is(err, cloudprovider.InstanceNotFound) {
			klog.Errorf("Failed to GetNodeNameByIPConfigurationID(%s): %v", ipConfigurationID, err)
			allErrs = append(allErrs, err)
			continue
		}
		if nodeName == "" {
			continue
		}

		vmName := mapNodeNameToVMName(types.NodeName(nodeName))
		nic, vmasID, err := as.getPrimaryInterfaceWithVMSet(vmName, vmSetName)
		if err != nil {
			if errors.Is(err, errNotInVMSet) {
				klog.V(3).Infof("EnsureBackendPoolDeleted skips node %s because it is not in the vmSet %s", nodeName, vmSetName)
				return nil
			}

			klog.Errorf("error: az.EnsureBackendPoolDeleted(%s), az.VMSet.GetPrimaryInterface.Get(%s, %s), err=%v", nodeName, vmName, vmSetName, err)
			return err
		}
		vmasName, err := getAvailabilitySetNameByID(vmasID)
		if err != nil {
			return fmt.Errorf("EnsureBackendPoolDeleted: failed to parse the VMAS ID %s: %w", vmasID, err)
		}
		// Only remove nodes belonging to specified vmSet to basic LB backends.
		if !strings.EqualFold(vmasName, vmSetName) {
			klog.V(2).Infof("EnsureBackendPoolDeleted: skipping the node %s belonging to another vm set %s", nodeName, vmasName)
			continue
		}

		if nic.ProvisioningState == consts.NicFailedState {
			klog.Warningf("EnsureBackendPoolDeleted skips node %s because its primary nic %s is in Failed state", nodeName, *nic.Name)
			return nil
		}

		if nic.InterfacePropertiesFormat != nil && nic.InterfacePropertiesFormat.IPConfigurations != nil {
			newIPConfigs := *nic.IPConfigurations
			for j, ipConf := range newIPConfigs {
				if !to.Bool(ipConf.Primary) {
					continue
				}
				// found primary ip configuration
				if ipConf.LoadBalancerBackendAddressPools != nil {
					newLBAddressPools := *ipConf.LoadBalancerBackendAddressPools
					for k := len(newLBAddressPools) - 1; k >= 0; k-- {
						pool := newLBAddressPools[k]
						if strings.EqualFold(to.String(pool.ID), backendPoolID) {
							newLBAddressPools = append(newLBAddressPools[:k], newLBAddressPools[k+1:]...)
							break
						}
					}
					newIPConfigs[j].LoadBalancerBackendAddressPools = &newLBAddressPools
				}
			}
			nic.IPConfigurations = &newIPConfigs
			nicUpdaters = append(nicUpdaters, func() error {
				ctx, cancel := getContextWithCancel()
				defer cancel()
				klog.V(2).Infof("EnsureBackendPoolDeleted begins to CreateOrUpdate for NIC(%s, %s) with backendPoolID %s", as.resourceGroup, to.String(nic.Name), backendPoolID)
				rerr := as.InterfacesClient.CreateOrUpdate(ctx, as.ResourceGroup, to.String(nic.Name), nic)
				if rerr != nil {
					klog.Errorf("EnsureBackendPoolDeleted CreateOrUpdate for NIC(%s, %s) failed with error %v", as.resourceGroup, to.String(nic.Name), rerr.Error())
					return rerr.Error()
				}
				return nil
			})
		}
	}
	errs := utilerrors.AggregateGoroutines(nicUpdaters...)
	if errs != nil {
		return utilerrors.Flatten(errs)
	}
	// Fail if there are other errors.
	if len(allErrs) > 0 {
		return utilerrors.Flatten(utilerrors.NewAggregate(allErrs))
	}

	isOperationSucceeded = true
	return nil
}

func getAvailabilitySetNameByID(asID string) (string, error) {
	// for standalone VM
	if asID == "" {
		return "", nil
	}

	matches := vmasIDRE.FindStringSubmatch(asID)
	if len(matches) != 2 {
		return "", fmt.Errorf("getAvailabilitySetNameByID: failed to parse the VMAS ID %s", asID)
	}
	vmasName := matches[1]
	return vmasName, nil
}

// get a storage account by UUID
func generateStorageAccountName(accountNamePrefix string) string {
	uniqueID := strings.Replace(string(uuid.NewUUID()), "-", "", -1)
	accountName := strings.ToLower(accountNamePrefix + uniqueID)
	if len(accountName) > consts.StorageAccountNameMaxLength {
		return accountName[:consts.StorageAccountNameMaxLength-1]
	}
	return accountName
}

// GetNodeNameByIPConfigurationID gets the node name and the availabilitySet name by IP configuration ID.
func (as *availabilitySet) GetNodeNameByIPConfigurationID(ipConfigurationID string) (string, string, error) {
	matches := nicIDRE.FindStringSubmatch(ipConfigurationID)
	if len(matches) != 3 {
		klog.V(4).Infof("Can not extract VM name from ipConfigurationID (%s)", ipConfigurationID)
		return "", "", fmt.Errorf("invalid ip config ID %s", ipConfigurationID)
	}

	nicResourceGroup, nicName := matches[1], matches[2]
	if nicResourceGroup == "" || nicName == "" {
		return "", "", fmt.Errorf("invalid ip config ID %s", ipConfigurationID)
	}
	nic, rerr := as.InterfacesClient.Get(context.Background(), nicResourceGroup, nicName, "")
	if rerr != nil {
		return "", "", fmt.Errorf("GetNodeNameByIPConfigurationID(%s): failed to get interface of name %s: %w", ipConfigurationID, nicName, rerr.Error())
	}
	vmID := ""
	if nic.InterfacePropertiesFormat != nil && nic.VirtualMachine != nil {
		vmID = to.String(nic.VirtualMachine.ID)
	}
	if vmID == "" {
		klog.V(2).Infof("GetNodeNameByIPConfigurationID(%s): empty vmID", ipConfigurationID)
		return "", "", nil
	}

	matches = vmIDRE.FindStringSubmatch(vmID)
	if len(matches) != 2 {
		return "", "", fmt.Errorf("invalid virtual machine ID %s", vmID)
	}
	vmName := matches[1]

	vm, err := as.getVirtualMachine(types.NodeName(vmName), azcache.CacheReadTypeDefault)
	if err != nil {
		klog.Errorf("Unable to get the virtual machine by node name %s: %v", vmName, err)
		return "", "", err
	}
	asID := ""
	if vm.VirtualMachineProperties != nil && vm.AvailabilitySet != nil {
		asID = to.String(vm.AvailabilitySet.ID)
	}
	if asID == "" {
		return vmName, "", nil
	}

	asName, err := getAvailabilitySetNameByID(asID)
	if err != nil {
		return "", "", fmt.Errorf("cannot get the availability set name by the availability set ID %s: %v", asID, err)
	}
	return vmName, strings.ToLower(asName), nil
}

func (as *availabilitySet) getAvailabilitySetByNodeName(nodeName string, crt azcache.AzureCacheReadType) (*compute.AvailabilitySet, error) {
	cached, err := as.vmasCache.Get(consts.VMASKey, crt)
	if err != nil {
		return nil, err
	}
	vmasList := cached.(*sync.Map)

	if vmasList == nil {
		klog.Warning("Couldn't get all vmas from cache")
		return nil, nil
	}

	var result *compute.AvailabilitySet
	vmasList.Range(func(_, value interface{}) bool {
		vmasEntry := value.(*availabilitySetEntry)
		vmas := vmasEntry.vmas
		if vmas != nil && vmas.AvailabilitySetProperties != nil && vmas.VirtualMachines != nil {
			for _, vmIDRef := range *vmas.VirtualMachines {
				if vmIDRef.ID != nil {
					matches := vmIDRE.FindStringSubmatch(to.String(vmIDRef.ID))
					if len(matches) != 2 {
						err = fmt.Errorf("invalid vm ID %s", to.String(vmIDRef.ID))
						return false
					}

					vmName := matches[1]
					if strings.EqualFold(vmName, nodeName) {
						result = vmas
						return false
					}
				}
			}
		}

		return true
	})

	if err != nil {
		return nil, err
	}

	if result == nil {
		klog.Warningf("Unable to find node %s: %v", nodeName, cloudprovider.InstanceNotFound)
		return nil, cloudprovider.InstanceNotFound
	}

	return result, nil
}

// GetNodeCIDRMaskByProviderID returns the node CIDR subnet mask by provider ID.
func (as *availabilitySet) GetNodeCIDRMasksByProviderID(providerID string) (int, int, error) {
	nodeName, err := as.GetNodeNameByProviderID(providerID)
	if err != nil {
		return 0, 0, err
	}

	vmas, err := as.getAvailabilitySetByNodeName(string(nodeName), azcache.CacheReadTypeDefault)
	if err != nil {
		if errors.Is(err, cloudprovider.InstanceNotFound) {
			return consts.DefaultNodeMaskCIDRIPv4, consts.DefaultNodeMaskCIDRIPv6, nil
		}
		return 0, 0, err
	}

	var ipv4Mask, ipv6Mask int
	if v4, ok := vmas.Tags[consts.VMSetCIDRIPV4TagKey]; ok && v4 != nil {
		ipv4Mask, err = strconv.Atoi(to.String(v4))
		if err != nil {
			klog.Errorf("GetNodeCIDRMasksByProviderID: error when paring the value of the ipv4 mask size %s: %v", to.String(v4), err)
		}
	}
	if v6, ok := vmas.Tags[consts.VMSetCIDRIPV6TagKey]; ok && v6 != nil {
		ipv6Mask, err = strconv.Atoi(to.String(v6))
		if err != nil {
			klog.Errorf("GetNodeCIDRMasksByProviderID: error when paring the value of the ipv6 mask size%s: %v", to.String(v6), err)
		}
	}

	return ipv4Mask, ipv6Mask, nil
}

// EnsureBackendPoolDeletedFromVMSets ensures the loadBalancer backendAddressPools deleted from the specified VMAS
func (as *availabilitySet) EnsureBackendPoolDeletedFromVMSets(vmasNamesMap map[string]bool, backendPoolID string) error {
	return nil
}

// GetAgentPoolVMSetNames returns all VMAS names according to the nodes
func (as *availabilitySet) GetAgentPoolVMSetNames(nodes []*v1.Node) (*[]string, error) {
	vms, err := as.ListVirtualMachines(as.ResourceGroup)
	if err != nil {
		klog.Errorf("as.getNodeAvailabilitySet - ListVirtualMachines failed, err=%v", err)
		return nil, err
	}

	return as.getAgentPoolAvailabilitySets(vms, nodes)
}
