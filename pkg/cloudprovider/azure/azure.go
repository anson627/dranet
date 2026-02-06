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

package azure

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog/v2"

	"github.com/google/dranet/pkg/cloudprovider"
	resourceapi "k8s.io/api/resource/v1"
)

// InfiniBandSupport represents the type of InfiniBand support for a given VM size.
type InfiniBandSupport string

const (
	InfiniBandNDR  InfiniBandSupport = "NDR"  // 400 Gb/s
	InfiniBandHDR  InfiniBandSupport = "HDR"  // 200 Gb/s
	InfiniBandEDR  InfiniBandSupport = "EDR"  // 100 Gb/s
	InfiniBandNone InfiniBandSupport = "None" // No InfiniBand support
)

const (
	AzureAttrPrefix = "azure.dra.net"

	AttrAzureVMSize              = AzureAttrPrefix + "/" + "vmSize"
	AttrAzureLocation            = AzureAttrPrefix + "/" + "location"
	AttrAzureZone                = AzureAttrPrefix + "/" + "zone"
	AttrAzurePlatformFaultDomain = AzureAttrPrefix + "/" + "platformFaultDomain"
	AttrAzurePlacementGroupId    = AzureAttrPrefix + "/" + "placementGroupId"
	AttrAzureVnetName            = AzureAttrPrefix + "/" + "vnetName"
	AttrAzureSubnetName          = AzureAttrPrefix + "/" + "subnetName"
	AttrAzureResourceGroup       = AzureAttrPrefix + "/" + "resourceGroup"
	AttrAzureSubscriptionId      = AzureAttrPrefix + "/" + "subscriptionId"
	AttrAzureInfiniBandSupport   = AzureAttrPrefix + "/" + "infinibandSupport"
)

const (
	// Azure IMDS endpoint
	imdsURL        = "http://169.254.169.254/metadata/instance?api-version=2021-02-01"
	imdsAPIVersion = "2021-02-01"
)

var (
	// VMSizeInfiniBandMap maps Azure VM sizes to their InfiniBand support level
	// Only includes officially supported VM sizes for dranet
	// https://learn.microsoft.com/en-us/azure/virtual-machines/sizes-gpu
	VMSizeInfiniBandMap = map[string]InfiniBandSupport{
		// ND H100 v5 series - NDR (400 Gb/s)
		"standard_nd96isr_h100_v5": InfiniBandNDR,

		// ND A100 v4 series - HDR (200 Gb/s)
		"standard_nd96asr_v4": InfiniBandHDR,

		// ND GB200 v6 series - NDR (400 Gb/s)
		"standard_nd128isr_ndr_gb200_v6": InfiniBandNDR,

		// ND GB300 v6 series - NDR (400 Gb/s)
		"standard_nd128isr_gb300_v6": InfiniBandNDR,
	}
)

// AzureIMDSResponse represents the Azure Instance Metadata Service response
type AzureIMDSResponse struct {
	Compute struct {
		VMSize              string `json:"vmSize"`
		Location            string `json:"location"`
		Zone                string `json:"zone"`
		PlatformFaultDomain string `json:"platformFaultDomain"`
		PlacementGroupId    string `json:"placementGroupId"`
		ResourceGroupName   string `json:"resourceGroupName"`
		SubscriptionId      string `json:"subscriptionId"`
		VMId                string `json:"vmId"`
		Name                string `json:"name"`
	} `json:"compute"`
	Network struct {
		Interfaces []struct {
			IPv4       []string `json:"ipv4,omitempty"`
			IPv6       []string `json:"ipv6,omitempty"`
			MacAddress string   `json:"macAddress,omitempty"`
		} `json:"interface,omitempty"`
	} `json:"network"`
}

// IsOnAzure checks if the current environment is running on Azure
// by attempting to query the Azure Instance Metadata Service.
func IsOnAzure() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", imdsURL, nil)
	if err != nil {
		return false
	}
	req.Header.Add("Metadata", "true")

	client := &http.Client{
		Timeout: 2 * time.Second,
	}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()

	return resp.StatusCode == http.StatusOK
}

// GetInstance retrieves Azure instance properties by querying the Instance Metadata Service.
func GetInstance(ctx context.Context) (*cloudprovider.CloudInstance, error) {
	var instance *cloudprovider.CloudInstance

	// IMDS may not be available immediately during startup
	err := wait.PollUntilContextTimeout(ctx, 1*time.Second, 15*time.Second, true, func(ctx context.Context) (done bool, err error) {
		req, err := http.NewRequestWithContext(ctx, "GET", imdsURL, nil)
		if err != nil {
			klog.Infof("could not create IMDS request ... retrying: %v", err)
			return false, nil
		}
		req.Header.Add("Metadata", "true")

		client := &http.Client{
			Timeout: 5 * time.Second,
		}
		resp, err := client.Do(req)
		if err != nil {
			klog.Infof("could not query Azure IMDS ... retrying: %v", err)
			return false, nil
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			klog.Infof("Azure IMDS returned status %d ... retrying", resp.StatusCode)
			return false, nil
		}

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			klog.Infof("could not read IMDS response body ... retrying: %v", err)
			return false, nil
		}

		var imdsResp AzureIMDSResponse
		if err := json.Unmarshal(body, &imdsResp); err != nil {
			klog.Infof("could not parse IMDS response ... retrying: %v", err)
			return false, nil
		}

		// Normalize VM size to lowercase for map lookup
		vmSize := strings.ToLower(imdsResp.Compute.VMSize)
		infinibandSupport := VMSizeInfiniBandMap[vmSize]
		if infinibandSupport == "" {
			infinibandSupport = InfiniBandNone
		}

		instance = &cloudprovider.CloudInstance{
			Name:                imdsResp.Compute.Name,
			Type:                vmSize,
			Provider:            cloudprovider.CloudProviderAzure,
			AcceleratorProtocol: string(infinibandSupport),
			Topology:            fmt.Sprintf("%s/%s/%s", imdsResp.Compute.Location, imdsResp.Compute.Zone, imdsResp.Compute.PlatformFaultDomain),
		}

		// Convert network interfaces
		for _, iface := range imdsResp.Network.Interfaces {
			netInterface := cloudprovider.NetworkInterface{
				Mac: iface.MacAddress,
			}
			if len(iface.IPv4) > 0 {
				netInterface.IPv4 = iface.IPv4[0]
			}
			if len(iface.IPv6) > 0 {
				netInterface.IPv6 = iface.IPv6
			}
			instance.Interfaces = append(instance.Interfaces, netInterface)
		}

		return true, nil
	})
	if err != nil {
		return nil, fmt.Errorf("failed to get Azure instance metadata: %w", err)
	}

	return instance, nil
}

// GetAzureAttributes fetches all Azure-specific attributes for a network interface
// identified by its MAC address.
func GetAzureAttributes(mac string, instance *cloudprovider.CloudInstance) map[resourceapi.QualifiedName]resourceapi.DeviceAttribute {
	if instance == nil {
		klog.Warningf("instance metadata is nil, cannot get Azure attributes")
		return nil
	}

	attributes := make(map[resourceapi.QualifiedName]resourceapi.DeviceAttribute)

	// Query IMDS again for detailed network and compute information
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", imdsURL, nil)
	if err != nil {
		klog.Warningf("could not create IMDS request: %v", err)
		return attributes
	}
	req.Header.Add("Metadata", "true")

	client := &http.Client{
		Timeout: 5 * time.Second,
	}
	resp, err := client.Do(req)
	if err != nil {
		klog.Warningf("could not query Azure IMDS: %v", err)
		return attributes
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		klog.Warningf("Azure IMDS returned status %d", resp.StatusCode)
		return attributes
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		klog.Warningf("could not read IMDS response body: %v", err)
		return attributes
	}

	var imdsResp AzureIMDSResponse
	if err := json.Unmarshal(body, &imdsResp); err != nil {
		klog.Warningf("could not parse IMDS response: %v", err)
		return attributes
	}

	// Add VM-level attributes
	vmSize := imdsResp.Compute.VMSize
	attributes[AttrAzureVMSize] = resourceapi.DeviceAttribute{StringValue: &vmSize}

	if imdsResp.Compute.Location != "" {
		location := imdsResp.Compute.Location
		attributes[AttrAzureLocation] = resourceapi.DeviceAttribute{StringValue: &location}
	}

	if imdsResp.Compute.Zone != "" {
		zone := imdsResp.Compute.Zone
		attributes[AttrAzureZone] = resourceapi.DeviceAttribute{StringValue: &zone}
	}

	if imdsResp.Compute.PlatformFaultDomain != "" {
		faultDomain := imdsResp.Compute.PlatformFaultDomain
		attributes[AttrAzurePlatformFaultDomain] = resourceapi.DeviceAttribute{StringValue: &faultDomain}
	}

	if imdsResp.Compute.PlacementGroupId != "" {
		placementGroupId := imdsResp.Compute.PlacementGroupId
		attributes[AttrAzurePlacementGroupId] = resourceapi.DeviceAttribute{StringValue: &placementGroupId}
	}

	if imdsResp.Compute.ResourceGroupName != "" {
		resourceGroup := imdsResp.Compute.ResourceGroupName
		attributes[AttrAzureResourceGroup] = resourceapi.DeviceAttribute{StringValue: &resourceGroup}
	}

	if imdsResp.Compute.SubscriptionId != "" {
		subscriptionId := imdsResp.Compute.SubscriptionId
		attributes[AttrAzureSubscriptionId] = resourceapi.DeviceAttribute{StringValue: &subscriptionId}
	}

	// Add InfiniBand support attribute
	vmSizeLower := strings.ToLower(vmSize)
	infinibandSupport := VMSizeInfiniBandMap[vmSizeLower]
	if infinibandSupport == "" {
		infinibandSupport = InfiniBandNone
	}
	infinibandStr := string(infinibandSupport)
	attributes[AttrAzureInfiniBandSupport] = resourceapi.DeviceAttribute{StringValue: &infinibandStr}

	// For network-specific attributes (vnet, subnet), we would need to query
	// the network interface metadata endpoint or parse from ARM resource IDs
	// This is left for future enhancement as it requires additional IMDS calls

	klog.V(4).Infof("Generated Azure attributes for MAC %s: %+v", mac, attributes)

	return attributes
}
