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
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/containerservice/armcontainerservice/v4"
	"github.com/google/dranet/pkg/cloudprovider/azure"
	"github.com/spf13/cobra"

	"k8s.io/klog/v2"
)

// acceleratorpodCmd represents the acceleratorpod command
var acceleratorpodCmd = &cobra.Command{
	Use:   "acceleratorpod",
	Short: "Manage accelerator pods (InfiniBand-enabled node pools)",
	Long: `The 'acceleratorpod' command allows you to create and manage
InfiniBand-enabled node pools on AKS, which we refer to as accelerator pods.`,
}

func init() {
	acceleratorpodCmd.AddCommand(acceleratorpodCreateCmd)
	acceleratorpodCmd.AddCommand(acceleratorpodGetCmd)
	acceleratorpodCmd.AddCommand(acceleratorpodDeleteCmd)
	acceleratorpodCmd.AddCommand(acceleratorpodListCmd)
}

var (
	vmSize                      string
	nodeCount                   int
	enableProximityPlacementGroup bool
)

// acceleratorpodListCmd represents the list command for accelerator pods (node pools)
var acceleratorpodListCmd = &cobra.Command{
	Use:   "list",
	Short: "List accelerator node pools in an AKS cluster",
	Long: `Lists all AKS node pools that were created and tagged by dranetctl
as accelerator pods. It identifies these node pools by looking for the
'dra.net/acceleratorpod: "true"' tag.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		if clusterName == "" {
			return fmt.Errorf("cluster name not explicitly provided")
		}
		ctx := context.Background()

		// Get the cluster to list the node pools
		cluster, err := ManagedClustersClient.Get(ctx, resourceGroup, clusterName, nil)
		if err != nil {
			return fmt.Errorf("failed to get cluster: %w", err)
		}

		var acceleratorNodePools []string
		if cluster.Properties != nil && cluster.Properties.AgentPoolProfiles != nil {
			for _, pool := range cluster.Properties.AgentPoolProfiles {
				if pool.Tags != nil {
					if val, ok := pool.Tags["dra.net/acceleratorpod"]; ok && *val == "true" {
						acceleratorNodePools = append(acceleratorNodePools, *pool.Name)
					}
				}
			}
		}

		if len(acceleratorNodePools) == 0 {
			fmt.Printf("No accelerator node pools found in cluster %s with tag dra.net/acceleratorpod: \"true\".\n", clusterName)
			return nil
		}

		fmt.Printf("There are %d dranet accelerator node pools in cluster %s:\n", len(acceleratorNodePools), clusterName)
		fmt.Println("---")
		for _, name := range acceleratorNodePools {
			fmt.Println(name)
		}

		return nil
	},
}

// acceleratorpodCreateCmd represents the create subcommand for acceleratorpod
var acceleratorpodCreateCmd = &cobra.Command{
	Use:   "create <acceleratorpod_name>",
	Short: "Create a new accelerator pod (InfiniBand-enabled node pool)",
	Long: `Creates a new InfiniBand-enabled node pool on the specified AKS cluster,
creating necessary network and subnet resources and optionally configuring
proximity placement groups. This group of machines is referred to as an accelerator pod.`,
	Args: cobra.ExactArgs(1), // Expects the acceleratorpod name as an argument
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		acceleratorpodName := args[0]
		if clusterName == "" {
			return fmt.Errorf("cluster name not explicitly provided")
		}
		if location == "" {
			return fmt.Errorf("location for accelerator pod %s not specified", acceleratorpodName)
		}
		if vmSize == "" {
			return fmt.Errorf("vm-size must be specified")
		}

		// Check if VM size supports InfiniBand
		vmSizeLower := strings.ToLower(vmSize)
		infinibandSupport := azure.VMSizeInfiniBandMap[vmSizeLower]
		if infinibandSupport == "" || infinibandSupport == azure.InfiniBandNone {
			klog.Warningf("VM size %s does not have known InfiniBand support", vmSize)
		} else {
			klog.Infof("VM size %s supports InfiniBand: %s", vmSize, infinibandSupport)
		}

		klog.Infof("Creating acceleratorpod '%s'...\n", acceleratorpodName)
		klog.Infof("  Subscription: %s\n", subscriptionID)
		klog.Infof("  Resource Group: %s\n", resourceGroup)
		klog.Infof("  Location: %s\n", location)
		klog.Infof("  Cluster: %s\n", clusterName)
		klog.Infof("  VM Size: %s\n", vmSize)
		klog.Infof("  Node Count: %d\n", nodeCount)

		// Create proximity placement group if requested
		var proximityPlacementGroupID *string
		if enableProximityPlacementGroup {
			ppgID, err := createProximityPlacementGroup(ctx, acceleratorpodName)
			if err != nil {
				return fmt.Errorf("failed to create proximity placement group: %w", err)
			}
			proximityPlacementGroupID = &ppgID
			klog.Infof("Created proximity placement group: %s\n", ppgID)
		}

		// Create agent pool
		count32 := int32(nodeCount)
		agentPoolProfile := armcontainerservice.AgentPool{
			Properties: &armcontainerservice.ManagedClusterAgentPoolProfileProperties{
				Count:  &count32,
				VMSize: &vmSize,
				OSType: toPtr(armcontainerservice.OSTypeLinux),
				Mode:   toPtr(armcontainerservice.AgentPoolModeUser),
				Tags: map[string]*string{
					"dra.net/acceleratorpod": toPtr("true"),
				},
				ProximityPlacementGroupID: proximityPlacementGroupID,
				// Enable accelerated networking for InfiniBand support
				EnableNodePublicIP: toPtr(false),
			},
		}

		klog.Infof("Creating agent pool '%s' in cluster '%s'...\n", acceleratorpodName, clusterName)

		if dryRun {
			klog.Infof("Dry run: would create agent pool with profile: %+v\n", agentPoolProfile)
			return nil
		}

		poller, err := AgentPoolsClient.BeginCreateOrUpdate(
			ctx,
			resourceGroup,
			clusterName,
			acceleratorpodName,
			agentPoolProfile,
			nil,
		)
		if err != nil {
			return fmt.Errorf("failed to begin agent pool creation: %w", err)
		}

		klog.Infof("Waiting for agent pool creation to complete...\n")
		_, err = poller.PollUntilDone(ctx, nil)
		if err != nil {
			return fmt.Errorf("failed to create agent pool: %w", err)
		}

		klog.Infof("Agent pool '%s' created successfully.\n", acceleratorpodName)
		return nil
	},
}

func init() {
	// Flags for the 'acceleratorpod create' command
	acceleratorpodCreateCmd.Flags().StringVar(&vmSize, "vm-size", "", "The Azure VM size for the nodes (e.g., Standard_ND96asr_v4) (required)")
	acceleratorpodCreateCmd.Flags().IntVar(&nodeCount, "node-count", 0, "The number of VMs (nodes) to create in the node pool (required)")
	acceleratorpodCreateCmd.Flags().BoolVar(&enableProximityPlacementGroup, "enable-ppg", false, "Create and use a proximity placement group for InfiniBand connectivity")

	// Mark required flags for the create command
	_ = acceleratorpodCreateCmd.MarkFlagRequired("vm-size")
	_ = acceleratorpodCreateCmd.MarkFlagRequired("node-count")
}

// acceleratorpodGetCmd represents the get subcommand for acceleratorpod
var acceleratorpodGetCmd = &cobra.Command{
	Use:   "get [acceleratorpod_name]",
	Short: "Get details about an accelerator pod",
	Long: `Retrieves and displays detailed information about the specified accelerator pod
(AKS node pool). You can optionally provide the name of the accelerator pod. If not
provided, all node pools will be displayed.`,
	Args: cobra.MaximumNArgs(1), // Expects the acceleratorpod name as an optional argument
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()

		var acceleratorpodName string
		if len(args) > 0 {
			acceleratorpodName = args[0]
		}

		if acceleratorpodName != "" {
			// Get specific agent pool
			pool, err := AgentPoolsClient.Get(ctx, resourceGroup, clusterName, acceleratorpodName, nil)
			if err != nil {
				return fmt.Errorf("failed to get agent pool: %w", err)
			}

			printAgentPoolDetails(pool.AgentPool)
		} else {
			// List all agent pools
			cluster, err := ManagedClustersClient.Get(ctx, resourceGroup, clusterName, nil)
			if err != nil {
				return fmt.Errorf("failed to get cluster: %w", err)
			}

			fmt.Printf("Cluster Name: %s\n", *cluster.Name)
			if cluster.Location != nil {
				fmt.Printf("  Location: %s\n", *cluster.Location)
			}
			fmt.Printf("  Agent Pools:\n")

			if cluster.Properties != nil && cluster.Properties.AgentPoolProfiles != nil {
				for _, profile := range cluster.Properties.AgentPoolProfiles {
					// Convert AgentPoolProfile to AgentPool for display
					fmt.Printf("    - Name: %s\n", *profile.Name)
					if profile.Count != nil {
						fmt.Printf("      Node Count: %d\n", *profile.Count)
					}
					if profile.VMSize != nil {
						fmt.Printf("      VM Size: %s\n", *profile.VMSize)
					}
					if profile.ProximityPlacementGroupID != nil {
						fmt.Printf("      Proximity Placement Group: %s\n", *profile.ProximityPlacementGroupID)
					}
					fmt.Println("      ---")
				}
			}
		}
		return nil
	},
}

func printAgentPoolDetails(pool armcontainerservice.AgentPool) {
	if pool.Name != nil {
		fmt.Printf("Name: %s\n", *pool.Name)
	}
	if pool.Properties != nil {
		if pool.Properties.Count != nil {
			fmt.Printf("  Node Count: %d\n", *pool.Properties.Count)
		}
		if pool.Properties.VMSize != nil {
			fmt.Printf("  VM Size: %s\n", *pool.Properties.VMSize)
		}
		if pool.Properties.ProximityPlacementGroupID != nil {
			fmt.Printf("  Proximity Placement Group: %s\n", *pool.Properties.ProximityPlacementGroupID)
		}
		if pool.Properties.ProvisioningState != nil {
			fmt.Printf("  Provisioning State: %s\n", *pool.Properties.ProvisioningState)
		}
	}
}

// acceleratorpodDeleteCmd represents the delete subcommand for acceleratorpod
var acceleratorpodDeleteCmd = &cobra.Command{
	Use:   "delete <acceleratorpod_name>",
	Short: "Delete an accelerator pod (node pool)",
	Long: `Deletes the specified accelerator pod (which corresponds to an AKS node pool).
You must specify the name of the accelerator pod to delete.`,
	Args: cobra.ExactArgs(1), // Expects the acceleratorpod name as an argument
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		acceleratorpodName := args[0]
		if clusterName == "" {
			return fmt.Errorf("cluster name not explicitly provided")
		}

		klog.Infof("Deleting acceleratorpod '%s'...\n", acceleratorpodName)
		klog.Infof("  Subscription: %s\n", subscriptionID)
		klog.Infof("  Resource Group: %s\n", resourceGroup)
		klog.Infof("  Cluster: %s\n", clusterName)

		// Get the agent pool to check its properties before deletion
		pool, err := AgentPoolsClient.Get(ctx, resourceGroup, clusterName, acceleratorpodName, nil)
		if err != nil {
			return fmt.Errorf("failed to get agent pool: %w", err)
		}

		if dryRun {
			klog.Infof("Dry run: would delete agent pool '%s'\n", acceleratorpodName)
			if pool.Properties != nil && pool.Properties.ProximityPlacementGroupID != nil {
				klog.Infof("Dry run: would delete proximity placement group: %s\n", *pool.Properties.ProximityPlacementGroupID)
			}
			return nil
		}

		// Delete the agent pool
		poller, err := AgentPoolsClient.BeginDelete(ctx, resourceGroup, clusterName, acceleratorpodName, nil)
		if err != nil {
			return fmt.Errorf("failed to begin agent pool deletion: %w", err)
		}

		klog.Infof("Waiting for agent pool deletion to complete...\n")
		_, err = poller.PollUntilDone(ctx, nil)
		if err != nil {
			return fmt.Errorf("failed to delete agent pool: %w", err)
		}

		klog.Infof("Agent pool '%s' deleted successfully.\n", acceleratorpodName)

		// Clean up proximity placement group if it was created by dranetctl
		if pool.Properties != nil && pool.Properties.ProximityPlacementGroupID != nil {
			ppgID := *pool.Properties.ProximityPlacementGroupID
			if strings.Contains(ppgID, "dranetctl-ppg-") || strings.Contains(ppgID, acceleratorpodName) {
				klog.Infof("Cleaning up proximity placement group: %s\n", ppgID)
				// Parse PPG name from resource ID
				parts := strings.Split(ppgID, "/")
				if len(parts) > 0 {
					ppgName := parts[len(parts)-1]
					err := deleteProximityPlacementGroup(ctx, ppgName)
					if err != nil {
						klog.Warningf("Failed to delete proximity placement group: %v\n", err)
					}
				}
			}
		}

		return nil
	},
}

func waitForOperation(ctx context.Context, operationName string) error {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()

	klog.V(2).Infof("Waiting for operation to complete: %s\n", operationName)

	// In Azure, operations are typically handled via polling
	// This is a placeholder for custom operation tracking if needed
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("context cancelled while waiting for operation %s: %w", operationName, ctx.Err())
		case <-ticker.C:
			// Azure SDK pollers handle the waiting, so this is mainly for custom operations
			klog.V(2).Infof("Checking operation status...\n")
			// Operation-specific logic would go here
			return nil
		}
	}
}

// toPtr is a helper function to create pointers to literal values
func toPtr[T any](v T) *T {
	return &v
}
