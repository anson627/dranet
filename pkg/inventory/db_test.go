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

package inventory

import (
	"testing"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/dynamic-resource-allocation/deviceattribute"
)

func TestSetPCIeRootAttribute_NoFallback(t *testing.T) {
	// This test verifies that when both standard PCIe root resolution AND
	// parent bridge lookup fail, no pcieRoot attribute is set.
	tests := []struct {
		name        string
		pciAddress  string
		wantHasAttr bool
	}{
		{
			name:        "no attribute when standard resolution and parent bridge lookup fail",
			pciAddress:  "invalid:address", // Will fail both standard resolution and parent bridge lookup
			wantHasAttr: false,
		},
		{
			name:        "no attribute for non-existent device",
			pciAddress:  "9999:00:00.0", // Valid format but doesn't exist in sysfs
			wantHasAttr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			device := resourceapi.Device{
				Attributes: make(map[resourceapi.QualifiedName]resourceapi.DeviceAttribute),
			}

			setPCIeRootAttribute(&device, tt.pciAddress)

			_, exists := device.Attributes[deviceattribute.StandardDeviceAttributePCIeRoot]
			if exists != tt.wantHasAttr {
				t.Errorf("attribute exists = %v, want %v", exists, tt.wantHasAttr)
			}
		})
	}
}
