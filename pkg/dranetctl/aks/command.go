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
	"os"

	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v5"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/containerservice/armcontainerservice/v4"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v5"
	"github.com/spf13/cobra"
)

var (
	// Azure SDK clients
	ManagedClustersClient       *armcontainerservice.ManagedClustersClient
	AgentPoolsClient            *armcontainerservice.AgentPoolsClient
	VirtualNetworksClient       *armnetwork.VirtualNetworksClient
	SubnetsClient               *armnetwork.SubnetsClient
	ProximityPlacementGroupsClient *armcompute.ProximityPlacementGroupsClient

	// Global flags
	subscriptionID  string
	resourceGroup   string
	clusterName     string
	location        string
	dryRun          bool
)

func init() {
	AksCmd.AddCommand(acceleratorpodCmd)

	AksCmd.PersistentFlags().StringVar(&subscriptionID, "subscription", "", "Azure Subscription ID")
	AksCmd.PersistentFlags().StringVar(&resourceGroup, "resource-group", "", "Azure Resource Group")
	AksCmd.PersistentFlags().StringVar(&clusterName, "cluster", "", "The name of the target AKS cluster")
	AksCmd.PersistentFlags().StringVar(&location, "location", "", "Azure region (e.g., eastus, westus2)")
	AksCmd.PersistentFlags().BoolVar(&dryRun, "dry-run", false, "The command will print the write actions without executing them")
}

var AksCmd = &cobra.Command{
	Use:   "aks",
	Short: "Manage resources on Azure Kubernetes Service (AKS)",
	Long:  `This command allows you to manage resources on AKS.`,
	PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
		// This function runs before any subcommand of aks
		if subscriptionID == "" {
			subscriptionID = os.Getenv("AZURE_SUBSCRIPTION_ID")
			if subscriptionID == "" {
				return fmt.Errorf("missing subscription ID (use --subscription or set AZURE_SUBSCRIPTION_ID)")
			}
		}

		if resourceGroup == "" {
			resourceGroup = os.Getenv("AZURE_RESOURCE_GROUP")
			if resourceGroup == "" {
				return fmt.Errorf("missing resource group (use --resource-group or set AZURE_RESOURCE_GROUP)")
			}
		}

		ctx := context.Background()

		// Create Azure credential using DefaultAzureCredential
		// This supports multiple authentication methods:
		// - Environment variables (AZURE_CLIENT_ID, AZURE_CLIENT_SECRET, AZURE_TENANT_ID)
		// - Managed Identity
		// - Azure CLI credentials
		cred, err := azidentity.NewDefaultAzureCredential(nil)
		if err != nil {
			return fmt.Errorf("failed to create Azure credential: %w", err)
		}

		// Initialize AKS Managed Clusters client
		managedClustersClient, err := armcontainerservice.NewManagedClustersClient(subscriptionID, cred, nil)
		if err != nil {
			return fmt.Errorf("failed to create managed clusters client: %w", err)
		}
		ManagedClustersClient = managedClustersClient

		// Initialize AKS Agent Pools client
		agentPoolsClient, err := armcontainerservice.NewAgentPoolsClient(subscriptionID, cred, nil)
		if err != nil {
			return fmt.Errorf("failed to create agent pools client: %w", err)
		}
		AgentPoolsClient = agentPoolsClient

		// Initialize Virtual Networks client
		virtualNetworksClient, err := armnetwork.NewVirtualNetworksClient(subscriptionID, cred, nil)
		if err != nil {
			return fmt.Errorf("failed to create virtual networks client: %w", err)
		}
		VirtualNetworksClient = virtualNetworksClient

		// Initialize Subnets client
		subnetsClient, err := armnetwork.NewSubnetsClient(subscriptionID, cred, nil)
		if err != nil {
			return fmt.Errorf("failed to create subnets client: %w", err)
		}
		SubnetsClient = subnetsClient

		// Initialize Proximity Placement Groups client
		proximityPlacementGroupsClient, err := armcompute.NewProximityPlacementGroupsClient(subscriptionID, cred, nil)
		if err != nil {
			return fmt.Errorf("failed to create proximity placement groups client: %w", err)
		}
		ProximityPlacementGroupsClient = proximityPlacementGroupsClient

		// If location is not specified, try to get it from the cluster
		if location == "" && clusterName != "" {
			cluster, err := ManagedClustersClient.Get(ctx, resourceGroup, clusterName, nil)
			if err != nil {
				return fmt.Errorf("failed to get cluster location: %w", err)
			}
			if cluster.Location != nil {
				location = *cluster.Location
			}
		}

		return nil
	},
}
