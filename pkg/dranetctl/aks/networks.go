/*
Copyright The Kubernetes Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    https://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package aks

import (
	"context"
	"fmt"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v5"
	"k8s.io/klog/v2"
)

const (
	// Prefix for dranetctl-managed resources
	wellKnownPrefix = "dranetctl"
)

// createProximityPlacementGroup creates a proximity placement group for InfiniBand connectivity
func createProximityPlacementGroup(ctx context.Context, acceleratorpodName string) (string, error) {
	ppgName := fmt.Sprintf("%s-ppg-%s", wellKnownPrefix, acceleratorpodName)

	klog.Infof("Creating proximity placement group: %s\n", ppgName)

	ppgType := armcompute.ProximityPlacementGroupTypeStandard
	ppg := armcompute.ProximityPlacementGroup{
		Location: &location,
		Properties: &armcompute.ProximityPlacementGroupProperties{
			ProximityPlacementGroupType: &ppgType,
		},
		Tags: map[string]*string{
			"dra.net/acceleratorpod": toPtr("true"),
			"dra.net/managed-by":     toPtr("dranetctl"),
		},
	}

	if dryRun {
		klog.Infof("Dry run: would create proximity placement group '%s' in location '%s'\n", ppgName, location)
		return fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Compute/proximityPlacementGroups/%s",
			subscriptionID, resourceGroup, ppgName), nil
	}

	result, err := ProximityPlacementGroupsClient.CreateOrUpdate(ctx, resourceGroup, ppgName, ppg, nil)
	if err != nil {
		return "", fmt.Errorf("failed to create proximity placement group: %w", err)
	}

	if result.ID == nil {
		return "", fmt.Errorf("proximity placement group created but ID is nil")
	}

	klog.Infof("Successfully created proximity placement group: %s\n", *result.ID)
	return *result.ID, nil
}

// deleteProximityPlacementGroup deletes a proximity placement group
func deleteProximityPlacementGroup(ctx context.Context, ppgName string) error {
	klog.Infof("Deleting proximity placement group: %s\n", ppgName)

	if dryRun {
		klog.Infof("Dry run: would delete proximity placement group '%s'\n", ppgName)
		return nil
	}

	_, err := ProximityPlacementGroupsClient.Delete(ctx, resourceGroup, ppgName, nil)
	if err != nil {
		return fmt.Errorf("failed to delete proximity placement group: %w", err)
	}

	klog.Infof("Successfully deleted proximity placement group: %s\n", ppgName)
	return nil
}

// createAcceleratorSubnets creates additional subnets for multi-NIC configurations
// Note: In Azure AKS, additional network interfaces for InfiniBand are typically
// handled automatically by the VM size and Azure's InfiniBand driver.
// This function is a placeholder for future enhancements if custom subnet
// configurations are needed.
func createAcceleratorSubnets(ctx context.Context, acceleratorpodName string, subnetCount int) ([]string, error) {
	klog.V(2).Infof("Azure AKS handles InfiniBand networking automatically for ND-series VMs\n")
	klog.V(2).Infof("No additional subnet creation needed for acceleratorpod: %s\n", acceleratorpodName)

	// In Azure, InfiniBand is configured automatically on supported VM sizes
	// The InfiniBand network is separate from the Azure VNet and is managed by Azure
	// This is different from GCP where additional networks need to be created

	// If custom VNet/subnet configuration is needed in the future, it would go here
	return nil, nil
}

// deleteNetwork is a placeholder for network cleanup
// In Azure AKS, most networking resources are managed by the cluster itself
// and are automatically cleaned up when the cluster or node pool is deleted.
func deleteNetwork(ctx context.Context, networkName string) error {
	klog.V(2).Infof("Azure AKS manages network resources automatically\n")
	klog.V(2).Infof("Network cleanup for %s is handled by Azure when the node pool is deleted\n", networkName)
	return nil
}
