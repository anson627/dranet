# Proposal: AWS/EKS EFA Support for DraNet

## Summary

Add support for AWS Elastic Fabric Adapter (EFA) to DraNet, enabling Kubernetes workloads on EKS to leverage EFA devices through the DRA (Dynamic Resource Allocation) API. This replaces the need for the existing `aws-efa-k8s-device-plugin` DaemonSet by integrating EFA device discovery, cloud metadata enrichment, and pod attachment natively into DraNet's architecture.

## Background

### What is EFA?

AWS Elastic Fabric Adapter (EFA) is a network interface for EC2 instances that provides:
- **OS-bypass hardware interface** for low-latency, high-bandwidth inter-node communication
- **AWS Scalable Reliable Datagram (SRD) protocol** for reliable, high-performance transport
- **Combined ENA + RDMA interface** — each EFA device functions as both a standard Elastic Network Adapter (ENA) and an RDMA-capable OS-bypass interface
- Critical for **MPI** (Message Passing Interface) and **NCCL** (NVIDIA Collective Communications Library) workloads at scale

### How EFA Devices Appear on the System

| Property | Value |
|----------|-------|
| PCI Vendor | `0x1d0f` (Amazon) |
| PCI Device IDs | `0xefa0`, `0xefa1`, `0xefa2` (varies by generation) |
| Kernel Module | `efa` |
| RDMA Devices | `/sys/class/infiniband/efa_*` |
| Character Devices | `/dev/infiniband/uverbs*`, `/dev/infiniband/rdma_cm` |
| Network Interface | `eth*` or `ens*` (like regular ENA, but EFA-capable) |
| sysfs driver | `/sys/class/net/<ifname>/device/driver` -> `efa` |
| Huge Pages | 5128 x 2MiB pre-allocated per EFA-enabled instance |

### Current State in DraNet

DraNet already has strong foundations that EFA support can build on:
- **PCI device discovery** (`pkg/inventory/db.go`) — EFA devices are discovered as PCI network class devices
- **RDMA device discovery** (`pkg/inventory/db.go`) — EFA RDMA devices are discoverable via `rdmamap`
- **RDMA device attachment** (`pkg/driver/rdmadevice.go`) — moving RDMA links to pod namespaces and injecting char devices
- **Cloud provider plugin pattern** (`pkg/cloudprovider/`) — GCE, Azure, OKE implementations to follow
- **Host device movement** (`pkg/driver/hostdevice.go`) — moving network interfaces into pod namespaces

What's missing is **EFA-specific device identification** attributes, **full AWS metadata enrichment** (AZ, placement group, per-ENI metadata), and **workload scheduling example manifests** (DeviceClass, ResourceClaimTemplate).

### Current EFA Kubernetes Device Plugin Limitations

The existing `aws-efa-k8s-device-plugin` uses the legacy Kubernetes Device Plugin API:
- Registers `vpc.amazonaws.com/efa` as an extended resource (simple integer count)
- No topology awareness (NUMA, PCIe root alignment with GPUs)
- No per-device attributes for scheduling (no CEL selectors)
- No cloud metadata enrichment (instance type, placement group, AZ)
- Cannot express complex allocation policies (e.g., "give me an EFA on the same NUMA node as my GPU")

DraNet with DRA solves all of these by exposing rich per-device attributes.

## Detailed Design

### Phase 1: AWS Cloud Provider Plugin [IMPLEMENTED]

Created `pkg/cloudprovider/aws/aws.go` following the established pattern (GCE, Azure, OKE).

#### 1.1 AWS Instance Metadata Service (IMDS) Integration

Uses the AWS SDK v2 IMDS client (`github.com/aws/aws-sdk-go-v2/feature/ec2/imds`) with automatic IMDSv2 token handling, rather than raw HTTP requests. The client is configured with:
- HTTP timeout: 10 seconds
- Max retries: 10 (with SDK exponential backoff)
- Cached singleton client to avoid repeated initialization

The primary metadata endpoint used is `GetInstanceIdentityDocument`, which returns instance type, region, and other identity data in a single call.

#### 1.2 Cloud Provider Detection

```go
// pkg/cloudprovider/aws/aws.go
func OnAWS(ctx context.Context) bool {
    // Probes IMDS via GetInstanceIdentityDocument with a 5-second timeout.
    // Uses the AWS SDK v2 IMDS client which handles IMDSv2 token flow automatically.
}
```

#### 1.3 Neuron Instance Detection and EFA Device Group Mapping

The implementation focuses on **AWS Neuron instances** (Trainium/Inferentia) as the initial use case. For Neuron instances, EFA devices are enriched with device group attributes using the `aws-neuron/connected-device-maps-over-efa-for-neuron` library:

```go
func isNeuronInstance(instanceType string) bool {
    // Returns true for trn* and inf* instance type prefixes
}

var isEFADevice = func(pciAddress string) bool {
    // Checks /sys/bus/pci/devices/{pciAddress}/driver symlink -> "efa"
}
```

For Neuron instances with EFA-bound PCI devices, `GetDeviceAttributes` returns device group IDs from the Neuron EFA lookup library. These attributes enable topology-aware scheduling of Neuron accelerators connected over EFA.

For non-Neuron instances (e.g., `p4d.24xlarge`, `p5.48xlarge`), EFA devices are still fully discovered through DraNet's existing PCI and RDMA device discovery — they appear with `dra.net/rdma: true`, PCI vendor/device info, and NUMA topology. The cloud provider plugin returns no additional attributes for these instances in the current implementation.

#### 1.4 Implementation Structure

```go
type AWSInstance struct {
    InstanceType     string
    IsNeuronInstance bool
}
```

`GetDeviceAttributes(id DeviceIdentifiers)` — For Neuron instances, checks if the device's PCI address is bound to the EFA driver, then returns device group attributes from the Neuron lookup library. Returns empty attributes for non-Neuron instances.

`GetDeviceConfig(id DeviceIdentifiers)` returns `nil` (EFA devices get their IP config from the host, similar to Azure/OKE).

#### 1.5 Files Created/Modified

| File | Action | Description |
|------|--------|-------------|
| `pkg/cloudprovider/aws/aws.go` | **Created** | AWS cloud provider with IMDS detection, Neuron instance identification, and EFA device group attribute enrichment |
| `pkg/cloudprovider/aws/aws_test.go` | **Created** | Comprehensive unit tests (13 test functions) with mock IMDS server, timeout tests, and Neuron/EFA attribute tests |
| `pkg/inventory/cloud.go` | **Modified** | Added `CloudProviderHintAWS`, wired up `aws.OnAWS` detection and `aws.GetInstance` retrieval |
| `pkg/inventory/db.go` | **Modified** | Added `CloudProviderHintAWS` to validation in `WithCloudProviderHint` |
| `cmd/dranet/app.go` | **Modified** | Updated `--cloud-provider-hint` help text to include `AWS` |

### Phase 2: EFA Device Identification [DEFERRED]

EFA devices are already discoverable through DraNet's existing mechanisms:
- **PCI discovery** identifies them as Amazon PCI devices (`dra.net/pciVendor: "Amazon.com, Inc."`, `dra.net/pciDevice: "Elastic Fabric Adapter (EFA)"`)
- **RDMA discovery** marks them as RDMA-capable (`dra.net/rdma: true`)
- **NUMA topology** is exposed via `dra.net/numaNode`

Users can select EFA devices today using CEL expressions on existing attributes:

```cel
device.driver == "dra.net" &&
attributes["dra.net/rdma"].BoolValue == true &&
attributes["dra.net/pciVendor"].StringValue == "Amazon.com, Inc."
```

A dedicated `dra.net/efa` boolean attribute may be added in a future iteration for convenience, but is not required for EFA functionality.

### Phase 3: Registration and Wiring [IMPLEMENTED]

#### 3.1 Cloud Provider Registration

In `pkg/inventory/cloud.go`:

```go
const CloudProviderHintAWS CloudProviderHint = "AWS"

// In discoverCloudProvider():
CloudProviderHintAWS: aws.OnAWS,

// In getInstanceProperties():
CloudProviderHintAWS: aws.GetInstance,
```

#### 3.2 Cloud Provider Hint Validation

In `pkg/inventory/db.go`, `CloudProviderHintAWS` is included in the `WithCloudProviderHint` validation:

```go
if h != CloudProviderHintGCE && h != CloudProviderHintAWS && h != CloudProviderHintAzure && h != CloudProviderHintOKE && h != CloudProviderHintNone {
    klog.Fatalf("unknown cloud provider hint %q", hint)
}
```

#### 3.3 CLI Help Text

In `cmd/dranet/app.go`, the `--cloud-provider-hint` flag description includes AWS:
```
Supported values: (AWS, GCE, AZURE, OKE, NONE)
```

### Phase 4: EKS Example Manifests [IMPLEMENTED]

Created `examples/demo_eks_efa/` with discovery-focused example manifests demonstrating EFA device inspection on EKS. The demo targets **trn1.32xlarge** (Trainium) instances to validate the Neuron device group attribute enrichment from the AWS cloud provider plugin.

#### 4.1 Cluster Configuration

`cluster.yaml` — eksctl cluster configuration for EKS 1.34 with:
- 2x `trn1.32xlarge` Neuron + EFA nodes in a placement group (us-east-1b / use1-az4)
- 2x `m5.xlarge` system nodes
- EFA enabled (`efaEnabled: true`)
- Capacity Block for ML for guaranteed Trainium availability
- AL2023 Neuron AMI with pre-installed EFA and Neuron drivers
- Neuron taints for scheduling isolation

#### 4.2 Setup and Cleanup Scripts

`setup.sh` — Automated setup that searches for Capacity Block offerings, purchases one, patches `cluster.yaml` with the reservation ID, and creates the EKS cluster. Supports flags for region, AZ, instance type, count, duration, and start time filtering.

`cleanup.sh` — Deletes the EKS cluster and cancels the Capacity Block reservation.

#### 4.3 Expected ResourceSlice Reference

`resourceslice-expected.yaml` — Reference ResourceSlice showing expected output from a trn1.32xlarge node:
- 7 EFA devices exposed (primary ENI eth0 filtered as default gateway)
- Each device includes: interface name, PCI address/vendor/device, MAC, MTU, NUMA node, RDMA capability, IP address, encapsulation, state, and type attributes
- Demonstrates NUMA topology distribution (4 EFA on NUMA 0, 3 EFA on NUMA 1)
- Includes `resource.aws.com/devicegroup*_id` Neuron device group attributes from the AWS cloud provider plugin

#### 4.4 README

`README.md` — Setup guide covering:
- Prerequisites (AWS CLI, eksctl, kubectl, Helm)
- trn1.32xlarge hardware details (16 Trainium chips, 8x EFA interfaces, NUMA layout)
- Step-by-step Capacity Block purchase, cluster creation, DraNet deployment, and ResourceSlice inspection
- Useful `jq` commands for filtering RDMA-capable devices, NUMA distribution, and Neuron device group attributes

#### 4.5 Files Created

| File | Action | Description |
|------|--------|-------------|
| `examples/demo_eks_efa/README.md` | **Created** | Setup guide for EKS with EFA discovery |
| `examples/demo_eks_efa/setup.sh` | **Created** | Capacity Block purchase and EKS cluster creation |
| `examples/demo_eks_efa/cleanup.sh` | **Created** | EKS cluster deletion and Capacity Block cancellation |
| `examples/demo_eks_efa/cluster.yaml` | **Created** | eksctl cluster config with trn1.32xlarge + EFA |
| `examples/demo_eks_efa/resourceslice-expected.yaml` | **Created** | Reference ResourceSlice from a trn1.32xlarge node |

### Phase 5: Testing [PARTIALLY IMPLEMENTED]

#### 5.1 Unit Tests (Implemented)

| Test | File | Description |
|------|------|-------------|
| `TestIsNeuronInstance` | `pkg/cloudprovider/aws/aws_test.go` | 9 test cases for Neuron instance type detection (trn*, inf*, p5, g5, m5, c6i, empty) |
| `TestGetDeviceAttributes_NonNeuron` | `pkg/cloudprovider/aws/aws_test.go` | Verifies empty attributes for non-Neuron instances |
| `TestGetDeviceAttributes_NeuronSuccess` | `pkg/cloudprovider/aws/aws_test.go` | Verifies device group attributes returned for Neuron + EFA devices |
| `TestGetDeviceAttributes_NeuronLookupError` | `pkg/cloudprovider/aws/aws_test.go` | Verifies graceful handling of EFA lookup failures |
| `TestGetDeviceAttributes_NeuronNotEFA` | `pkg/cloudprovider/aws/aws_test.go` | Verifies empty attributes when Neuron device is not EFA-bound |
| `TestGetDeviceConfig` | `pkg/cloudprovider/aws/aws_test.go` | Verifies nil config returned |
| `TestAWSInstanceImplementsCloudInstance` | `pkg/cloudprovider/aws/aws_test.go` | Compile-time interface check |
| `TestGetInstance` | `pkg/cloudprovider/aws/aws_test.go` | 4 test cases with mock IMDS server (Neuron, GPU, standard, error) |
| `TestGetInstance_IMDSClientError` | `pkg/cloudprovider/aws/aws_test.go` | Error handling for IMDS client creation |
| `TestGetInstance_Timeout` | `pkg/cloudprovider/aws/aws_test.go` | Timeout behavior with slow IMDS |
| `TestOnAWS` | `pkg/cloudprovider/aws/aws_test.go` | 2 test cases (on EC2, not on EC2) with mock IMDS |
| `TestOnAWS_IMDSClientError` | `pkg/cloudprovider/aws/aws_test.go` | Error handling |
| `TestOnAWS_Timeout` | `pkg/cloudprovider/aws/aws_test.go` | Timeout behavior with 100ms parent context |

Test infrastructure includes a `fakeIMDSServer` that mimics the EC2 IMDS token and identity document endpoints, plus `overrideIMDSClient` / `overrideIMDSClientError` helpers for test isolation.

#### 5.2 Integration Tests (Not Yet Implemented)

- Deploy DraNet on an EKS cluster with EFA-enabled Neuron nodes (trn1.32xlarge)
- Verify EFA devices appear in ResourceSlice with correct attributes
- Verify Neuron device group attributes are populated by the AWS cloud provider plugin
- Verify EFA devices can be allocated to pods via ResourceClaim
- Verify RDMA character devices (`/dev/infiniband/uverbs*`) are injected into containers

## Implementation Sequence

```
Phase 1: AWS Cloud Provider Plugin [DONE]
  1.1 Created pkg/cloudprovider/aws/aws.go
      - OnAWS() detection via AWS SDK v2 IMDS client
      - GetInstance() with GetInstanceIdentityDocument
      - AWSInstance struct with Neuron-aware GetDeviceAttributes()
      - EFA driver detection via sysfs for Neuron device group mapping
  1.2 Created pkg/cloudprovider/aws/aws_test.go
      - 13 test functions with mock IMDS server

Phase 2: EFA Device Identification [DEFERRED]
  - Existing PCI/RDMA discovery already exposes EFA devices with
    vendor, device, RDMA, and NUMA attributes
  - Dedicated dra.net/efa attribute deferred to future iteration

Phase 3: Registration and Wiring [DONE]
  3.1 Added CloudProviderHintAWS to pkg/inventory/cloud.go
  3.2 Updated validation in pkg/inventory/db.go
  3.3 Updated CLI help in cmd/dranet/app.go

Phase 4: Examples [DONE]
  4.1 Created examples/demo_eks_efa/ directory
  4.2 Cluster config (trn1.32xlarge, us-east-1b, Capacity Block)
  4.3 Setup/cleanup scripts for Capacity Block lifecycle
  4.4 Expected ResourceSlice with Neuron device group attributes
  4.5 README with step-by-step guide
  4.6 Workload scheduling manifests (DeviceClass, ResourceClaim) deferred

Phase 5: Testing [PARTIAL]
  5.1 Unit tests for AWS cloud provider (13 test functions)
  5.2 Integration testing on EKS with EFA-capable instances (not yet done)
```

## Future Work

The following items are candidates for future iterations:

1. **Full AWS metadata enrichment** — Add per-interface attributes from IMDS (ENI ID, subnet ID, VPC ID, device number) and instance-level attributes (availability zone, placement group). This would require querying IMDS per-MAC metadata endpoints and correlating by MAC address.

2. **Dedicated `dra.net/efa` attribute** — Add a boolean attribute for convenient EFA device selection without relying on PCI vendor/device string matching.

3. **Workload scheduling examples** — Add DeviceClass, ResourceClaimTemplate, and NCCL test job manifests to the `examples/demo_eks_efa/` directory.

4. **Non-Neuron device attributes** — Extend `GetDeviceAttributes` to return useful attributes (instance type, AZ) for all AWS instances, not just Neuron.

## Compatibility Notes

- **No breaking changes** — this is purely additive
- **Existing EFA device plugin** — DraNet can coexist with the `aws-efa-k8s-device-plugin`, but users should migrate to DRA-based allocation for topology-aware scheduling benefits
- **VPC CNI** — DraNet does not replace the VPC CNI; it manages the secondary EFA interfaces that are used for high-performance inter-node communication, while the VPC CNI handles the primary pod networking
- **Kernel requirements** — the `efa` kernel module must be loaded on the node (standard on EKS-optimized AMIs)
- **RDMA netns mode** — EFA supports both shared and exclusive RDMA modes, which DraNet already handles via `rdmaSharedMode` detection in `pkg/driver/driver.go`

## Resolved Questions

1. **EFA-only interfaces** — AWS supports "EFA-only" interface types (no IP connectivity, only OS-bypass). **No new attribute is needed.** EFA-only interfaces still appear as network interfaces with RDMA capability but have no IP addresses assigned. Since `addLinkAttributes` (`pkg/inventory/db.go`) only sets `dra.net/ipv4` and `dra.net/ipv6` when global unicast addresses exist, their absence naturally signals "EFA-only". Users can distinguish via CEL:
   ```cel
   # Regular EFA (with IP)
   attributes["dra.net/rdma"].BoolValue == true && "dra.net/ipv4" in attributes

   # EFA-only (OS-bypass only, no IP)
   attributes["dra.net/rdma"].BoolValue == true && !("dra.net/ipv4" in attributes)
   ```

2. **IMDS integration approach** — Resolved by using the AWS SDK v2 IMDS client rather than raw HTTP requests. The SDK handles IMDSv2 token lifecycle, retries, and timeouts automatically, reducing implementation complexity and improving reliability.

3. **Neuron-first scope** — The initial implementation scopes cloud-provider-specific attributes to Neuron instances (Trainium/Inferentia), where EFA device group mapping is critical for topology-aware accelerator scheduling. The demo targets trn1.32xlarge to validate this path. Non-Neuron EFA instances (p4d, p5) are fully functional via DraNet's existing PCI/RDMA discovery.

## Open Questions

1. **EFA-only interface correlation** — The current design assumes AWS metadata enrichment can match devices by MAC address. How should DraNet identify and enrich EFA-only attachments that may present as RDMA-capable PCI devices without a usable Linux netdev or without the same metadata surface as ENA-backed interfaces? Should a future iteration add a PCI-address-based correlation path?

2. **Coexistence with `aws-efa-k8s-device-plugin`** — The proposal currently says DraNet can coexist with the legacy EFA device plugin, but both components would advertise and prepare access to the same hardware. Do we want to support true coexistence, or should the design require mutual exclusivity and define a migration path from the device plugin to DraNet?

3. **Ownership boundaries with the VPC CNI** — Filtering only the default gateway interface may not be sufficient on EKS, especially when the VPC CNI is managing additional ENIs or additional network cards. What AWS-specific rules should DraNet use to exclude interfaces that remain owned by the cluster networking stack?

4. **Node and cluster prerequisites** — What must remain a documented prerequisite outside of DraNet itself for EKS examples to be reproducible? At minimum this likely includes security group rules for EFA traffic, EFA-capable AMIs/drivers, and any required user-space libraries or runtime setup for MPI/NCCL workloads.

5. **Huge pages integration** — EFA pre-allocates 5128 x 2MiB huge pages. Should DraNet expose this as a device capacity attribute, or leave huge pages to be handled via standard Kubernetes resource requests?

6. **Multi-NIC topology** — Instances like `p5.48xlarge` have 32 EFA interfaces (represented as `vpc.amazonaws.com/efa: 32` in the device plugin). Should DraNet group these or expose them individually?

7. **GPUDirect RDMA** — Should DraNet surface GPU-NIC affinity information (e.g., matching NUMA nodes) to enable topology-aware co-scheduling with GPU DRA drivers?
