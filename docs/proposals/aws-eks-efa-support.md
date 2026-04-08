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
- **PCI device discovery** (`pkg/inventory/db.go:255-299`) — EFA devices will already be discovered as PCI network class devices
- **RDMA device discovery** (`pkg/inventory/db.go:448-474`) — EFA RDMA devices are discoverable via `rdmamap`
- **RDMA device attachment** (`pkg/driver/rdmadevice.go`) — moving RDMA links to pod namespaces and injecting char devices
- **Cloud provider plugin pattern** (`pkg/cloudprovider/`) — GCE, Azure, OKE implementations to follow
- **Host device movement** (`pkg/driver/hostdevice.go`) — moving network interfaces into pod namespaces

What's missing is the **AWS cloud provider plugin** to enrich devices with AWS-specific metadata, **EFA-specific device identification** attributes, and **example manifests** for EKS users.

### Current EFA Kubernetes Device Plugin Limitations

The existing `aws-efa-k8s-device-plugin` uses the legacy Kubernetes Device Plugin API:
- Registers `vpc.amazonaws.com/efa` as an extended resource (simple integer count)
- No topology awareness (NUMA, PCIe root alignment with GPUs)
- No per-device attributes for scheduling (no CEL selectors)
- No cloud metadata enrichment (instance type, placement group, AZ)
- Cannot express complex allocation policies (e.g., "give me an EFA on the same NUMA node as my GPU")

DraNet with DRA solves all of these by exposing rich per-device attributes.

## Detailed Design

### Phase 1: AWS Cloud Provider Plugin

Create `pkg/cloudprovider/aws/aws.go` following the established pattern (GCE, Azure, OKE).

#### 1.1 AWS Instance Metadata Service (IMDS) Integration

Query EC2 IMDS v2 (`http://169.254.169.254/latest/`) with IMDSv2 token-based authentication:

```
PUT /latest/api/token (TTL header) -> session token
GET /latest/meta-data/instance-type -> "p5.48xlarge"
GET /latest/meta-data/placement/availability-zone -> "us-west-2a"
GET /latest/meta-data/placement/group-name -> "my-placement-group"
GET /latest/meta-data/network/interfaces/macs/ -> list of MACs
GET /latest/meta-data/network/interfaces/macs/{mac}/device-number -> "0", "1", ...
GET /latest/meta-data/network/interfaces/macs/{mac}/interface-id -> "eni-xxx"
GET /latest/meta-data/network/interfaces/macs/{mac}/subnet-id -> "subnet-xxx"
GET /latest/meta-data/network/interfaces/macs/{mac}/vpc-id -> "vpc-xxx"
GET /latest/meta-data/network/interfaces/macs/{mac}/local-ipv4s -> "10.0.1.5"
```

#### 1.2 Cloud Provider Detection

```go
// pkg/cloudprovider/aws/aws.go
func OnAWS(ctx context.Context) bool {
    // Probe IMDS v2 token endpoint with short timeout
    // PUT http://169.254.169.254/latest/api/token
    // with X-aws-ec2-metadata-token-ttl-seconds: 5
}
```

#### 1.3 AWS-Specific Attributes

| Attribute | Type | Source | Description |
|-----------|------|--------|-------------|
| `aws.dra.net/instanceType` | string | IMDS | EC2 instance type (e.g., `p5.48xlarge`) |
| `aws.dra.net/availabilityZone` | string | IMDS | AZ for topology-aware scheduling |
| `aws.dra.net/placementGroup` | string | IMDS | Cluster placement group name |
| `aws.dra.net/eniId` | string | IMDS per-MAC | ENI ID for the interface |
| `aws.dra.net/subnetId` | string | IMDS per-MAC | Subnet the interface belongs to |
| `aws.dra.net/vpcId` | string | IMDS per-MAC | VPC ID |
| `aws.dra.net/deviceNumber` | int | IMDS per-MAC | Network interface device index |

#### 1.4 Implementation Structure

```go
type AWSInstance struct {
    InstanceType     string
    AvailabilityZone string
    PlacementGroup   string
    Interfaces       map[string]awsNetworkInterface // keyed by MAC
}

type awsNetworkInterface struct {
    MAC          string
    DeviceNumber int
    InterfaceID  string
    SubnetID     string
    VPCID        string
    LocalIPv4s   []string
}
```

The `GetDeviceAttributes(id DeviceIdentifiers)` method matches devices by MAC address to correlate local PCI/network devices with IMDS metadata.

`GetDeviceConfig(id DeviceIdentifiers)` returns `nil` initially (EFA devices get their IP config from the host, similar to Azure/OKE).

#### 1.5 Files to Create/Modify

| File | Action | Description |
|------|--------|-------------|
| `pkg/cloudprovider/aws/aws.go` | **Create** | AWS cloud provider implementation |
| `pkg/cloudprovider/aws/aws_test.go` | **Create** | Unit tests with mock IMDS server |
| `pkg/inventory/cloud.go` | **Modify** | Add `CloudProviderHintAWS`, wire up detection and instance retrieval |
| `pkg/inventory/db.go` | **Modify** | Add `CloudProviderHintAWS` to validation in `WithCloudProviderHint` |
| `cmd/dranet/app.go` | **Modify** | Update `--cloud-provider-hint` help text to include `AWS` |

### Phase 2: EFA Device Identification

#### 2.1 EFA Attribute

Add an `efa` boolean attribute so users can select EFA devices in DeviceClass CEL expressions:

```go
// pkg/apis/attributes.go
AttrEFA = AttrPrefix + "/" + "efa"  // dra.net/efa
```

#### 2.2 EFA Detection Logic

EFA devices can be identified by their PCI vendor/device ID or kernel driver. Two approaches (implement both for robustness):

**Approach A — PCI Device ID check** (in `discoverPCIDevices`):
```go
// Amazon EFA PCI vendor: 0x1d0f, device: 0xefa0/0xefa1/0xefa2
func isEFADevice(dev *ghw.PCIDevice) bool {
    if dev.Vendor == nil {
        return false
    }
    return dev.Vendor.ID == "1d0f" && strings.HasPrefix(dev.Product.ID, "efa")
}
```

**Approach B — sysfs driver check** (in `addLinkAttributes`):
```go
// /sys/class/net/<ifname>/device/driver -> .../efa
func isEFAInterface(ifName string) bool {
    driverPath, _ := os.Readlink(filepath.Join(sysnetPath, ifName, "device", "driver"))
    return filepath.Base(driverPath) == "efa"
}
```

#### 2.3 Files to Create/Modify

| File | Action | Description |
|------|--------|-------------|
| `pkg/apis/attributes.go` | **Modify** | Add `AttrEFA` constant |
| `pkg/inventory/db.go` | **Modify** | Set `AttrEFA` in `discoverPCIDevices` or `addLinkAttributes` |
| `pkg/inventory/sysfs.go` | **Modify** | Add `isEFADevice()` and/or `isEFAInterface()` helpers |
| `pkg/inventory/sysfs_test.go` | **Modify** | Add tests for EFA detection |

### Phase 3: Registration and Wiring

#### 3.1 Cloud Provider Registration

In `pkg/inventory/cloud.go`:

```go
const CloudProviderHintAWS CloudProviderHint = "AWS"

// Add to discoverCloudProvider():
CloudProviderHintAWS: aws.OnAWS,

// Add to getInstanceProperties():
CloudProviderHintAWS: aws.GetInstance,
```

#### 3.2 Cloud Provider Hint Validation

In `pkg/inventory/db.go`, add `CloudProviderHintAWS` to the `WithCloudProviderHint` validation:

```go
if h != CloudProviderHintGCE && h != CloudProviderHintAzure && h != CloudProviderHintOKE && h != CloudProviderHintAWS && h != CloudProviderHintNone {
    klog.Fatalf("unknown cloud provider hint %q", hint)
}
```

#### 3.3 CLI Help Text

In `cmd/dranet/app.go`, update the `--cloud-provider-hint` flag description:
```
Supported values: (GCE, AZURE, OKE, AWS, NONE)
```

### Phase 4: EKS Example Manifests

Create `examples/demo_eks_efa/` with working example manifests demonstrating EFA usage on EKS.

#### 4.1 DeviceClass for EFA

```yaml
apiVersion: resource.k8s.io/v1
kind: DeviceClass
metadata:
  name: efa
spec:
  selectors:
  - cel:
      expression: |
        device.driver == "dra.net" &&
        attributes["dra.net/efa"].BoolValue == true
```

#### 4.2 ResourceClaimTemplate for EFA

```yaml
apiVersion: resource.k8s.io/v1
kind: ResourceClaimTemplate
metadata:
  name: efa-claim
spec:
  spec:
    devices:
      requests:
      - name: efa
        deviceClassName: efa
        count: 4  # Request 4 EFA interfaces
```

#### 4.3 NCCL Test Job

A sample MPIJob or Job that uses EFA for multi-node NCCL all-reduce testing, demonstrating:
- EFA device allocation via DRA ResourceClaims
- GPU + EFA topology alignment via NUMA node matching
- `/dev/shm` volume mount for shared memory
- Huge pages resource requests

#### 4.4 Files to Create

| File | Action | Description |
|------|--------|-------------|
| `examples/demo_eks_efa/README.md` | **Create** | Setup guide for EKS with EFA |
| `examples/demo_eks_efa/deviceclass.yaml` | **Create** | DeviceClass selecting EFA devices |
| `examples/demo_eks_efa/resourceclaim.yaml` | **Create** | ResourceClaim requesting EFA devices |
| `examples/demo_eks_efa/resourceclaimtemplate.yaml` | **Create** | Template for pod-level claims |
| `examples/demo_eks_efa/nccl-test.yaml` | **Create** | Sample NCCL test workload |

### Phase 5: Testing

#### 5.1 Unit Tests

| Test | File | Description |
|------|------|-------------|
| IMDS mock server | `pkg/cloudprovider/aws/aws_test.go` | Test `OnAWS()`, `GetInstance()`, `GetDeviceAttributes()` with mock HTTP IMDS |
| IMDSv2 token flow | `pkg/cloudprovider/aws/aws_test.go` | Verify token acquisition and header passing |
| EFA PCI detection | `pkg/inventory/sysfs_test.go` | Test `isEFADevice()` with various PCI IDs |
| EFA sysfs detection | `pkg/inventory/sysfs_test.go` | Test `isEFAInterface()` with mock sysfs |
| Attribute population | `pkg/inventory/db_test.go` | Verify `dra.net/efa` attribute is set correctly |

#### 5.2 Integration / E2E Tests

- Deploy DraNet on an EKS cluster with EFA-enabled nodes (e.g., `p5.48xlarge`)
- Verify EFA devices appear in ResourceSlice with correct attributes
- Verify EFA devices can be allocated to pods via ResourceClaim
- Verify RDMA character devices (`/dev/infiniband/uverbs*`) are injected into containers
- Verify NCCL all-reduce test completes successfully across nodes using EFA

## Implementation Sequence

```
Phase 1: AWS Cloud Provider Plugin
  1.1 Create pkg/cloudprovider/aws/aws.go
      - OnAWS() detection
      - GetInstance() with IMDSv2 token auth
      - AWSInstance struct with GetDeviceAttributes() and GetDeviceConfig()
  1.2 Create pkg/cloudprovider/aws/aws_test.go
      - Mock IMDS server tests

Phase 2: EFA Device Identification
  2.1 Add AttrEFA to pkg/apis/attributes.go
  2.2 Add EFA detection in pkg/inventory/sysfs.go
  2.3 Set AttrEFA in pkg/inventory/db.go during device scan
  2.4 Add tests in pkg/inventory/sysfs_test.go

Phase 3: Registration and Wiring
  3.1 Add CloudProviderHintAWS to pkg/inventory/cloud.go
  3.2 Update validation in pkg/inventory/db.go
  3.3 Update CLI help in cmd/dranet/app.go

Phase 4: Examples
  4.1 Create examples/demo_eks_efa/ directory
  4.2 Write DeviceClass, ResourceClaim, and workload manifests
  4.3 Write README with setup instructions

Phase 5: Testing
  5.1 Unit tests for all new code
  5.2 Integration testing on EKS with EFA-capable instances
```

## Compatibility Notes

- **No breaking changes** — this is purely additive
- **Existing EFA device plugin** — DraNet can coexist with the `aws-efa-k8s-device-plugin`, but users should migrate to DRA-based allocation for topology-aware scheduling benefits
- **VPC CNI** — DraNet does not replace the VPC CNI; it manages the secondary EFA interfaces that are used for high-performance inter-node communication, while the VPC CNI handles the primary pod networking
- **Kernel requirements** — the `efa` kernel module must be loaded on the node (standard on EKS-optimized AMIs)
- **RDMA netns mode** — EFA supports both shared and exclusive RDMA modes, which DraNet already handles via `rdmaSharedMode` detection in `pkg/driver/driver.go:110-116`

## Resolved Questions

1. **EFA-only interfaces** — AWS supports "EFA-only" interface types (no IP connectivity, only OS-bypass). **No new attribute is needed.** EFA-only interfaces still appear as network interfaces with RDMA capability but have no IP addresses assigned. Since `addLinkAttributes` (`pkg/inventory/db.go:393-413`) only sets `dra.net/ipv4` and `dra.net/ipv6` when global unicast addresses exist, their absence naturally signals "EFA-only". Users can distinguish via CEL:
   ```cel
   # Regular EFA (with IP)
   attributes["dra.net/efa"].BoolValue == true && "dra.net/ipv4" in attributes

   # EFA-only (OS-bypass only, no IP)
   attributes["dra.net/efa"].BoolValue == true && !("dra.net/ipv4" in attributes)
   ```

## Open Questions

1. **EFA-only interface correlation** — The current design assumes AWS metadata enrichment can match devices by MAC address. How should DraNet identify and enrich EFA-only attachments that may present as RDMA-capable PCI devices without a usable Linux netdev or without the same metadata surface as ENA-backed interfaces? Should v1 explicitly scope EFA-only out, or should we add a PCI-address-based correlation path?

2. **Coexistence with `aws-efa-k8s-device-plugin`** — The proposal currently says DraNet can coexist with the legacy EFA device plugin, but both components would advertise and prepare access to the same hardware. Do we want to support true coexistence, or should the design require mutual exclusivity and define a migration path from the device plugin to DraNet?

3. **Ownership boundaries with the VPC CNI** — Filtering only the default gateway interface may not be sufficient on EKS, especially when the VPC CNI is managing additional ENIs or additional network cards. What AWS-specific rules should DraNet use to exclude interfaces that remain owned by the cluster networking stack?

4. **Node and cluster prerequisites** — What must remain a documented prerequisite outside of DraNet itself for EKS examples to be reproducible? At minimum this likely includes security group rules for EFA traffic, EFA-capable AMIs/drivers, and any required user-space libraries or runtime setup for MPI/NCCL workloads.

5. **Huge pages integration** — EFA pre-allocates 5128 x 2MiB huge pages. Should DraNet expose this as a device capacity attribute, or leave huge pages to be handled via standard Kubernetes resource requests?

6. **Multi-NIC topology** — Instances like `p5.48xlarge` have 32 EFA interfaces (represented as `vpc.amazonaws.com/efa: 32` in the device plugin). Should DraNet group these or expose them individually?

7. **GPUDirect RDMA** — Should DraNet surface GPU-NIC affinity information (e.g., matching NUMA nodes) to enable topology-aware co-scheduling with GPU DRA drivers?
