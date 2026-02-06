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
	"testing"

	"github.com/google/dranet/pkg/cloudprovider"
)

func TestVMSizeInfiniBandMap(t *testing.T) {
	tests := []struct {
		vmSize   string
		expected InfiniBandSupport
	}{
		{"standard_nd96isr_h100_v5", InfiniBandNDR},
		{"standard_nd96asr_v4", InfiniBandHDR},
		{"standard_nd128isr_ndr_gb200_v6", InfiniBandNDR},
		{"standard_nd128isr_gb300_v6", InfiniBandNDR},
		{"standard_d16s_v3", ""}, // No InfiniBand support (unsupported VM)
	}

	for _, tt := range tests {
		t.Run(tt.vmSize, func(t *testing.T) {
			got := VMSizeInfiniBandMap[tt.vmSize]
			if got != tt.expected {
				t.Errorf("VMSizeInfiniBandMap[%s] = %v, want %v", tt.vmSize, got, tt.expected)
			}
		})
	}
}

func TestGetAzureAttributes(t *testing.T) {
	tests := []struct {
		name     string
		mac      string
		instance *cloudprovider.CloudInstance
		wantNil  bool
	}{
		{
			name:     "nil instance",
			mac:      "00:00:00:00:00:00",
			instance: nil,
			wantNil:  true,
		},
		{
			name: "valid instance",
			mac:  "00:0d:3a:12:34:56",
			instance: &cloudprovider.CloudInstance{
				Name:                "test-vm",
				Type:                "standard_nd96asr_v4",
				Provider:            cloudprovider.CloudProviderAzure,
				AcceleratorProtocol: string(InfiniBandHDR),
				Interfaces: []cloudprovider.NetworkInterface{
					{
						Mac:  "00:0d:3a:12:34:56",
						IPv4: "10.0.0.4",
					},
				},
			},
			wantNil: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Note: GetAzureAttributes makes an IMDS call, so in a real test environment
			// without Azure IMDS, it will return an empty map (not nil)
			// This test just verifies it doesn't panic and handles nil instance correctly
			attrs := GetAzureAttributes(tt.mac, tt.instance)
			if tt.wantNil && attrs != nil {
				t.Errorf("GetAzureAttributes() = %v, want nil", attrs)
			}
		})
	}
}

func TestIsOnAzure(t *testing.T) {
	// This test will fail in non-Azure environments, which is expected
	// In CI/CD, we expect this to return false
	result := IsOnAzure()
	t.Logf("IsOnAzure() = %v (expected false in non-Azure environments)", result)
}

func TestInfiniBandSupportString(t *testing.T) {
	tests := []struct {
		support  InfiniBandSupport
		expected string
	}{
		{InfiniBandNDR, "NDR"},
		{InfiniBandHDR, "HDR"},
		{InfiniBandEDR, "EDR"},
		{InfiniBandNone, "None"},
	}

	for _, tt := range tests {
		t.Run(string(tt.support), func(t *testing.T) {
			got := string(tt.support)
			if got != tt.expected {
				t.Errorf("string(%v) = %v, want %v", tt.support, got, tt.expected)
			}
		})
	}
}
