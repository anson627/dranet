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
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestParsePCIAddress(t *testing.T) {
	testCases := []struct {
		name    string
		input   string
		want    *pciAddress
		wantErr bool
	}{
		{
			name:  "valid with domain",
			input: "0000:00:04.0",
			want: &pciAddress{
				domain:   "0000",
				bus:      "00",
				device:   "04",
				function: "0",
			},
			wantErr: false,
		},
		{
			name:  "valid without domain",
			input: "00:04.0",
			want: &pciAddress{
				domain:   "",
				bus:      "00",
				device:   "04",
				function: "0",
			},
			wantErr: false,
		},
		{
			name:    "invalid format",
			input:   "not-a-pci-address",
			wantErr: true,
		},
		{
			name:    "empty string",
			input:   "",
			wantErr: true,
		},
		{
			name:    "embedded in string",
			input:   "pci-0000:8c:00.0-device",
			wantErr: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parsePCIAddress(tc.input)
			if (err != nil) != tc.wantErr {
				t.Fatalf("pciAddressFromString() error = %v, wantErr %v", err, tc.wantErr)
				return
			}
			if diff := cmp.Diff(tc.want, got, cmp.AllowUnexported(pciAddress{})); diff != "" {
				t.Errorf("pciAddressFromString() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestPCIAddressFromPath(t *testing.T) {
	testCases := []struct {
		name    string
		input   string
		want    *pciAddress
		wantErr bool
	}{
		{
			name:  "simple path",
			input: "/sys/devices/pci0000:00/0000:00:04.0/virtio1/net/eth0",
			want: &pciAddress{
				domain:   "0000",
				bus:      "00",
				device:   "04",
				function: "0",
			},
			wantErr: false,
		},
		{
			name:  "hierarchical path",
			input: "/sys/devices/pci0000:8c/0000:8c:00.0/0000:8d:00.0/0000:8e:02.0/0000:91:00.0/net/eth3",
			want: &pciAddress{
				domain:   "0000",
				bus:      "91",
				device:   "00",
				function: "0",
			},
			wantErr: false,
		},
		{
			name:    "no pci address in path",
			input:   "/sys/devices/virtual/net/lo",
			wantErr: true,
		},
		{
			name:    "empty path",
			input:   "",
			wantErr: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := pciAddressFromPath(tc.input)
			if (err != nil) != tc.wantErr {
				t.Errorf("pciAddressFromPath() error = %v, wantErr %v", err, tc.wantErr)
				return
			}
			if diff := cmp.Diff(tc.want, got, cmp.AllowUnexported(pciAddress{})); diff != "" {
				t.Errorf("pciAddressFromPath() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestGetParentBridgeBusID(t *testing.T) {
	testCases := []struct {
		name       string
		pciAddr    string
		setupSysfs func(t *testing.T, basePath string) // Function to create mock sysfs structure
		want       string
	}{
		{
			name:    "Azure VMBUS - NIC with parent bridge",
			pciAddr: "0101:00:00.0",
			setupSysfs: func(t *testing.T, basePath string) {
				// Simulate Azure VMBUS sysfs structure:
				// /sys/bus/pci/devices/0101:00:00.0 -> /sys/devices/.../ffff:ff:01.0/0101:00:00.0
				realDevicePath := filepath.Join(basePath, "devices", "VMBUS", "ffff:ff:01.0", "0101:00:00.0")
				if err := os.MkdirAll(realDevicePath, 0755); err != nil {
					t.Fatalf("Failed to create mock device path: %v", err)
				}

				symlinkPath := filepath.Join(basePath, "bus", "pci", "devices", "0101:00:00.0")
				if err := os.MkdirAll(filepath.Dir(symlinkPath), 0755); err != nil {
					t.Fatalf("Failed to create symlink parent dir: %v", err)
				}
				if err := os.Symlink(realDevicePath, symlinkPath); err != nil {
					t.Fatalf("Failed to create symlink: %v", err)
				}
			},
			want: "ffff:ff:01.0",
		},
		{
			name:    "Azure VMBUS - GPU with parent bridge",
			pciAddr: "0001:00:00.0",
			setupSysfs: func(t *testing.T, basePath string) {
				// GPU 0001 shares PCIe switch ffff:ff:01.0 with NIC 0101
				realDevicePath := filepath.Join(basePath, "devices", "VMBUS", "ffff:ff:01.0", "0001:00:00.0")
				if err := os.MkdirAll(realDevicePath, 0755); err != nil {
					t.Fatalf("Failed to create mock device path: %v", err)
				}

				symlinkPath := filepath.Join(basePath, "bus", "pci", "devices", "0001:00:00.0")
				if err := os.MkdirAll(filepath.Dir(symlinkPath), 0755); err != nil {
					t.Fatalf("Failed to create symlink parent dir: %v", err)
				}
				if err := os.Symlink(realDevicePath, symlinkPath); err != nil {
					t.Fatalf("Failed to create symlink: %v", err)
				}
			},
			want: "ffff:ff:01.0",
		},
		{
			name:    "Azure VMBUS - different PCIe switch",
			pciAddr: "0104:00:00.0",
			setupSysfs: func(t *testing.T, basePath string) {
				// GPU 0008 and NIC 0104 share PCIe switch ffff:ff:04.0
				realDevicePath := filepath.Join(basePath, "devices", "VMBUS", "ffff:ff:04.0", "0104:00:00.0")
				if err := os.MkdirAll(realDevicePath, 0755); err != nil {
					t.Fatalf("Failed to create mock device path: %v", err)
				}

				symlinkPath := filepath.Join(basePath, "bus", "pci", "devices", "0104:00:00.0")
				if err := os.MkdirAll(filepath.Dir(symlinkPath), 0755); err != nil {
					t.Fatalf("Failed to create symlink parent dir: %v", err)
				}
				if err := os.Symlink(realDevicePath, symlinkPath); err != nil {
					t.Fatalf("Failed to create symlink: %v", err)
				}
			},
			want: "ffff:ff:04.0",
		},
		{
			name:    "standard PCI - hierarchical path with parent bridge",
			pciAddr: "0000:91:00.0",
			setupSysfs: func(t *testing.T, basePath string) {
				// Standard PCI hierarchical path:
				// /sys/devices/pci0000:8c/0000:8c:00.0/.../0000:8e:02.0/0000:91:00.0
				realDevicePath := filepath.Join(basePath, "devices", "pci0000:8c", "0000:8c:00.0", "0000:8d:00.0", "0000:8e:02.0", "0000:91:00.0")
				if err := os.MkdirAll(realDevicePath, 0755); err != nil {
					t.Fatalf("Failed to create mock device path: %v", err)
				}

				symlinkPath := filepath.Join(basePath, "bus", "pci", "devices", "0000:91:00.0")
				if err := os.MkdirAll(filepath.Dir(symlinkPath), 0755); err != nil {
					t.Fatalf("Failed to create symlink parent dir: %v", err)
				}
				if err := os.Symlink(realDevicePath, symlinkPath); err != nil {
					t.Fatalf("Failed to create symlink: %v", err)
				}
			},
			want: "0000:8e:02.0",
		},
		{
			name:    "parent is pciXXXX:XX root - not a valid PCI address",
			pciAddr: "0000:00:04.0",
			setupSysfs: func(t *testing.T, basePath string) {
				// Device directly under PCI root (no bridge)
				realDevicePath := filepath.Join(basePath, "devices", "pci0000:00", "0000:00:04.0")
				if err := os.MkdirAll(realDevicePath, 0755); err != nil {
					t.Fatalf("Failed to create mock device path: %v", err)
				}

				symlinkPath := filepath.Join(basePath, "bus", "pci", "devices", "0000:00:04.0")
				if err := os.MkdirAll(filepath.Dir(symlinkPath), 0755); err != nil {
					t.Fatalf("Failed to create symlink parent dir: %v", err)
				}
				if err := os.Symlink(realDevicePath, symlinkPath); err != nil {
					t.Fatalf("Failed to create symlink: %v", err)
				}
			},
			want: "", // pci0000:00 is not a valid PCI bus ID format
		},
		{
			name:    "device not found in sysfs",
			pciAddr: "9999:00:00.0",
			setupSysfs: func(t *testing.T, basePath string) {
				// Don't create any symlink - device doesn't exist
			},
			want: "",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Create temp directory for mock sysfs
			tmpDir, err := os.MkdirTemp("", "sysfs-test")
			if err != nil {
				t.Fatalf("Failed to create temp dir: %v", err)
			}
			defer os.RemoveAll(tmpDir)

			// Setup mock sysfs structure
			tc.setupSysfs(t, tmpDir)

			// Use the testable version with custom base path
			sysfsPath := filepath.Join(tmpDir, "bus", "pci", "devices")
			got := getParentBridgeBusIDWithPath(tc.pciAddr, sysfsPath)

			if got != tc.want {
				t.Errorf("getParentBridgeBusIDWithPath(%q) = %q, want %q", tc.pciAddr, got, tc.want)
			}
		})
	}
}
