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
	"github.com/Azure/azure-sdk-for-go/services/storage/mgmt/2021-09-01/storage"

	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/fileclient"
)

// create file share
func (az *Cloud) createFileShare(subsID, resourceGroupName, accountName string, shareOptions *fileclient.ShareOptions) error {
	return az.FileClient.WithSubscriptionID(subsID).CreateFileShare(resourceGroupName, accountName, shareOptions)
}

func (az *Cloud) deleteFileShare(subsID, resourceGroupName, accountName, name string) error {
	return az.FileClient.WithSubscriptionID(subsID).DeleteFileShare(resourceGroupName, accountName, name)
}

func (az *Cloud) resizeFileShare(subsID, resourceGroupName, accountName, name string, sizeGiB int) error {
	return az.FileClient.WithSubscriptionID(subsID).ResizeFileShare(resourceGroupName, accountName, name, sizeGiB)
}

func (az *Cloud) getFileShare(subsID, resourceGroupName, accountName, name string) (storage.FileShare, error) {
	return az.FileClient.WithSubscriptionID(subsID).GetFileShare(resourceGroupName, accountName, name)
}
