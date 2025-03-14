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

package azurefile

import (
	"context"
	"fmt"
	"io/ioutil"
	"os"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/Azure/azure-sdk-for-go/services/network/mgmt/2021-08-01/network"
	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"

	"sigs.k8s.io/azurefile-csi-driver/test/utils/testutil"
	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/subnetclient/mocksubnetclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/retry"

	azureprovider "sigs.k8s.io/cloud-provider-azure/pkg/provider"
)

func skipIfTestingOnWindows(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Skipping test on Windows")
	}
}

// TestGetCloudProvider tests the func GetCloudProvider().
// To run this unit test successfully, need to ensure /etc/kubernetes/azure.json nonexistent.
func TestGetCloudProvider(t *testing.T) {
	var (
		fakeCredFile       = testutil.GetWorkDirPath("fake-cred-file.json", t)
		fakeKubeConfig     = testutil.GetWorkDirPath("fake-kube-config", t)
		emptyKubeConfig    = testutil.GetWorkDirPath("empty-kube-config", t)
		notExistKubeConfig = testutil.GetWorkDirPath("non-exist.json", t)
	)

	fakeContent := `apiVersion: v1
clusters:
- cluster:
    server: https://localhost:8080
  name: foo-cluster
contexts:
- context:
    cluster: foo-cluster
    user: foo-user
    namespace: bar
  name: foo-context
current-context: foo-context
kind: Config
users:
- name: foo-user
  user:
    exec:
      apiVersion: client.authentication.k8s.io/v1beta1
      args:
      - arg-1
      - arg-2
      command: foo-command
`

	if err := createTestFile(emptyKubeConfig); err != nil {
		t.Error(err)
	}
	defer func() {
		if err := os.Remove(emptyKubeConfig); err != nil {
			t.Error(err)
		}
	}()

	tests := []struct {
		desc                  string
		createFakeCredFile    bool
		createFakeKubeConfig  bool
		kubeconfig            string
		userAgent             string
		allowEmptyCloudConfig bool
		expectedErr           testutil.TestError
	}{
		{
			desc:                  "out of cluster, no kubeconfig, no credential file",
			kubeconfig:            "",
			allowEmptyCloudConfig: true,
			expectedErr:           testutil.TestError{},
		},
		{
			desc:                  "[failure][disallowEmptyCloudConfig] out of cluster, no kubeconfig, no credential file",
			kubeconfig:            "",
			allowEmptyCloudConfig: false,
			expectedErr: testutil.TestError{
				DefaultError: fmt.Errorf("no cloud config provided, error"),
			},
		},
		{
			desc:                  "[failure] out of cluster & in cluster, specify a non-exist kubeconfig, no credential file",
			kubeconfig:            notExistKubeConfig,
			allowEmptyCloudConfig: true,
			expectedErr:           testutil.TestError{},
		},
		{
			desc:                  "[failure] out of cluster & in cluster, specify a empty kubeconfig, no credential file",
			kubeconfig:            emptyKubeConfig,
			allowEmptyCloudConfig: true,
			expectedErr: testutil.TestError{
				DefaultError: fmt.Errorf("failed to get KubeClient: invalid configuration: no configuration has been provided, try setting KUBERNETES_MASTER environment variable"),
			},
		},
		{
			desc:                  "[failure] out of cluster & in cluster, specify a fake kubeconfig, no credential file",
			createFakeKubeConfig:  true,
			kubeconfig:            fakeKubeConfig,
			allowEmptyCloudConfig: true,
			expectedErr:           testutil.TestError{},
		},
		{
			desc:                  "[success] out of cluster & in cluster, no kubeconfig, a fake credential file",
			createFakeCredFile:    true,
			kubeconfig:            "",
			userAgent:             "useragent",
			allowEmptyCloudConfig: true,
			expectedErr:           testutil.TestError{},
		},
	}

	for _, test := range tests {
		if test.createFakeKubeConfig {
			if err := createTestFile(fakeKubeConfig); err != nil {
				t.Error(err)
			}
			defer func() {
				if err := os.Remove(fakeKubeConfig); err != nil {
					t.Error(err)
				}
			}()

			if err := ioutil.WriteFile(fakeKubeConfig, []byte(fakeContent), 0666); err != nil {
				t.Error(err)
			}
		}
		if test.createFakeCredFile {
			if err := createTestFile(fakeCredFile); err != nil {
				t.Error(err)
			}
			defer func() {
				if err := os.Remove(fakeCredFile); err != nil {
					t.Error(err)
				}
			}()

			originalCredFile, ok := os.LookupEnv(DefaultAzureCredentialFileEnv)
			if ok {
				defer os.Setenv(DefaultAzureCredentialFileEnv, originalCredFile)
			} else {
				defer os.Unsetenv(DefaultAzureCredentialFileEnv)
			}
			os.Setenv(DefaultAzureCredentialFileEnv, fakeCredFile)
		}
		cloud, err := getCloudProvider(test.kubeconfig, "", "", "", test.userAgent, test.allowEmptyCloudConfig)
		if !testutil.AssertError(err, &test.expectedErr) && !strings.Contains(err.Error(), test.expectedErr.DefaultError.Error()) {
			t.Errorf("desc: %s,\n input: %q, getCloudProvider err: %v, expectedErr: %v", test.desc, test.kubeconfig, err, test.expectedErr)
		}
		if cloud == nil {
			t.Errorf("return value of getCloudProvider should not be nil even there is error")
		} else {
			assert.Equal(t, cloud.UserAgent, test.userAgent)
		}
	}
}

func createTestFile(path string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	return nil
}

func TestUpdateSubnetServiceEndpoints(t *testing.T) {
	d := NewFakeDriver()
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	mockSubnetClient := mocksubnetclient.NewMockInterface(ctrl)

	config := azureprovider.Config{
		ResourceGroup: "rg",
		Location:      "loc",
		VnetName:      "fake-vnet",
		SubnetName:    "fake-subnet",
	}

	d.cloud = &azureprovider.Cloud{
		SubnetsClient: mockSubnetClient,
		Config:        config,
	}
	ctx := context.TODO()

	testCases := []struct {
		name     string
		testFunc func(t *testing.T)
	}{
		{
			name: "[fail] no subnet",
			testFunc: func(t *testing.T) {
				retErr := retry.NewError(false, fmt.Errorf("the subnet does not exist"))
				mockSubnetClient.EXPECT().Get(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(network.Subnet{}, retErr).Times(1)
				expectedErr := fmt.Errorf("failed to get the subnet %s under vnet %s: %v", config.SubnetName, config.VnetName, retErr)
				err := d.updateSubnetServiceEndpoints(ctx, "", "", "")
				if !reflect.DeepEqual(err, expectedErr) {
					t.Errorf("Unexpected error: %v", err)
				}
			},
		},
		{
			name: "[success] subnetPropertiesFormat is nil",
			testFunc: func(t *testing.T) {
				mockSubnetClient.EXPECT().Get(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(network.Subnet{}, nil).Times(1)
				mockSubnetClient.EXPECT().CreateOrUpdate(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).Times(1)

				err := d.updateSubnetServiceEndpoints(ctx, "", "", "")
				if !reflect.DeepEqual(err, nil) {
					t.Errorf("Unexpected error: %v", err)
				}
			},
		},
		{
			name: "[success] ServiceEndpoints is nil",
			testFunc: func(t *testing.T) {
				fakeSubnet := network.Subnet{
					SubnetPropertiesFormat: &network.SubnetPropertiesFormat{},
				}

				mockSubnetClient.EXPECT().Get(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(fakeSubnet, nil).Times(1)
				mockSubnetClient.EXPECT().CreateOrUpdate(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).Times(1)

				err := d.updateSubnetServiceEndpoints(ctx, "", "", "")
				if !reflect.DeepEqual(err, nil) {
					t.Errorf("Unexpected error: %v", err)
				}
			},
		},
		{
			name: "[success] storageService does not exists",
			testFunc: func(t *testing.T) {
				fakeSubnet := network.Subnet{
					SubnetPropertiesFormat: &network.SubnetPropertiesFormat{
						ServiceEndpoints: &[]network.ServiceEndpointPropertiesFormat{},
					},
				}

				mockSubnetClient.EXPECT().Get(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(fakeSubnet, nil).Times(1)
				mockSubnetClient.EXPECT().CreateOrUpdate(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).Times(1)

				err := d.updateSubnetServiceEndpoints(ctx, "", "", "")
				if !reflect.DeepEqual(err, nil) {
					t.Errorf("Unexpected error: %v", err)
				}
			},
		},
		{
			name: "[success] storageService already exists",
			testFunc: func(t *testing.T) {
				fakeSubnet := network.Subnet{
					SubnetPropertiesFormat: &network.SubnetPropertiesFormat{
						ServiceEndpoints: &[]network.ServiceEndpointPropertiesFormat{
							{
								Service: &storageService,
							},
						},
					},
				}

				mockSubnetClient.EXPECT().Get(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(fakeSubnet, nil).Times(1)

				err := d.updateSubnetServiceEndpoints(ctx, "", "", "")
				if !reflect.DeepEqual(err, nil) {
					t.Errorf("Unexpected error: %v", err)
				}
			},
		},
		{
			name: "[fail] SubnetsClient is nil",
			testFunc: func(t *testing.T) {
				d.cloud.SubnetsClient = nil
				expectedErr := fmt.Errorf("SubnetsClient is nil")
				err := d.updateSubnetServiceEndpoints(ctx, "", "", "")
				if !reflect.DeepEqual(err, expectedErr) {
					t.Errorf("Unexpected error: %v", err)
				}
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, tc.testFunc)
	}
}
