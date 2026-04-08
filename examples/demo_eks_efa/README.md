# EKS EFA Baseline Demo

Deploy DraNet on an EKS cluster with A100 GPU nodes (p4d.24xlarge) to discover
EFA network devices via DRA and inspect the resulting ResourceSlice.

## Prerequisites

- AWS CLI v2 configured with appropriate credentials
- `eksctl` >= 0.200.0
- `kubectl` >= 1.34
- Helm v3
- An AWS region with p4d.24xlarge availability (e.g., `us-east-1`, `us-west-2`)

## VM: p4d.24xlarge

Each node has:

| Resource | Count | Detail |
|---|---|---|
| GPU | 8 x NVIDIA A100 40GB | NVSwitch all-to-all |
| EFA | 4 x Elastic Fabric Adapter | 100 Gbps each, RDMA-capable |
| vCPU | 96 | Intel Cascade Lake |
| Memory | 1152 GiB | |
| NUMA nodes | 2 | 4 GPU + 2 EFA per NUMA node |

Network interfaces:
- Network card 0: Primary ENI (eth0) -- standard networking, not EFA
- Network cards 1-3: EFA interfaces -- dual ENA + OS-bypass RDMA

EFA devices appear as PCI devices with:
- PCI Vendor: `1d0f` (Amazon)
- Kernel driver: `efa`
- RDMA devices under `/sys/class/infiniband/efa_*`
- Character devices: `/dev/infiniband/uverbs*`, `/dev/infiniband/rdma_cm`

## Step 1: Create EKS Cluster

Create a cluster with Kubernetes 1.34 (DRA `resource.k8s.io/v1` is GA):

```bash
eksctl create cluster -f cluster.yaml
```

See `cluster.yaml` for the full cluster configuration. Key points:
- Kubernetes 1.34 with DRA support
- Fixed-size managed node group with 2x p4d.24xlarge instances
- EFA enabled on the node group (4 EFA interfaces)
- Placement group for optimal inter-node bandwidth
- AL2023 GPU AMI with EFA drivers pre-installed

## Step 2: Deploy DraNet

EKS 1.34 ships with containerd v2, which enables NRI by default -- no extra
configuration is needed.

Install DraNet using the Helm chart:

```bash
helm install dranet oci://registry.k8s.io/networking/charts/dranet \
  --namespace kube-system \
  --set args.cloudProviderHint=NONE
```

Or use the static install manifest:

```bash
kubectl apply -f https://raw.githubusercontent.com/kubernetes-sigs/dranet/main/install.yaml
```

Wait for DraNet pods to be ready:

```bash
kubectl rollout status daemonset/dranet -n kube-system --timeout=120s
```

## Step 3: Inspect ResourceSlices

Once DraNet is running, it discovers all network devices on each node and
publishes them as ResourceSlice objects.

```bash
# List all resource slices
kubectl get resourceslices

# View the full device inventory for a specific node
kubectl get resourceslices -l node.kubernetes.io/instance-type=p4d.24xlarge -o yaml
```

### Expected ResourceSlice Output

On a p4d.24xlarge node, DraNet will discover the following devices:

**EFA interfaces** (3 devices -- eth0/primary ENI is filtered as the default gateway):

```yaml
- name: pci-0000-XX-XX-0    # varies by instance
  attributes:
    dra.net/ifName:
      string: "eth1"         # or ens6, etc.
    dra.net/pciVendor:
      string: "Amazon.com, Inc."
    dra.net/pciDevice:
      string: "Elastic Fabric Adapter (EFA)"
    dra.net/mac:
      string: "0a:5b:..."
    dra.net/mtu:
      int: 9001
    dra.net/numaNode:
      int: 0                  # NUMA 0 or 1 depending on PCIe topology
    dra.net/rdma:
      bool: true              # EFA supports RDMA
    dra.net/sriov:
      bool: false
    dra.net/state:
      string: "up"
    dra.net/type:
      string: "device"
    dra.net/virtual:
      bool: false
    dra.net/ipv4:
      string: "10.0.1.x/24"  # VPC subnet IP
    dra.net/encapsulation:
      string: "ether"
    dra.net/ebpf:
      bool: false
    dra.net/pciAddress:
      string: "0000:XX:XX.0"
```

Key observations:
- `dra.net/rdma: true` -- EFA devices have RDMA capability
- `dra.net/pciVendor: "Amazon.com, Inc."` -- Amazon PCI vendor
- `dra.net/numaNode` -- topology info for GPU-NIC alignment
- The primary ENI (eth0) is automatically filtered out as the default gateway interface
- Each EFA interface has both IP connectivity and RDMA capability

### Useful Inspection Commands

```bash
# Show only RDMA-capable devices
kubectl get resourceslices -o json | \
  jq '.items[].spec.devices[] | select(.attributes["dra.net/rdma"].bool == true) | .name'

# Show NUMA node distribution
kubectl get resourceslices -o json | \
  jq '.items[].spec.devices[] | {name: .name, numa: .attributes["dra.net/numaNode"].int, rdma: .attributes["dra.net/rdma"].bool}'

# Show PCI vendor info (identify EFA vs other NICs)
kubectl get resourceslices -o json | \
  jq '.items[].spec.devices[] | {name: .name, vendor: .attributes["dra.net/pciVendor"].string, device: .attributes["dra.net/pciDevice"].string}'
```

## Files

| File | Description |
|---|---|
| `cluster.yaml` | eksctl cluster config with p4d.24xlarge node group and EFA |
| `resourceslice-expected.yaml` | Reference ResourceSlice from a live p4d.24xlarge node |

## Notes

- **No capacity block needed** -- this demo uses standard on-demand p4d.24xlarge instances
- **EFA drivers** -- included in EKS-optimized AL2023 GPU AMIs; no additional driver install required
- **VPC CNI** -- remains responsible for primary pod networking (eth0); DraNet manages the secondary EFA interfaces
- **RDMA mode** -- check with `rdma system` inside a pod; EFA supports both shared and exclusive netns modes
- **GPU DRA driver** -- this demo focuses on network devices only; for GPU + NIC topology alignment, deploy a GPU DRA driver (e.g., NVIDIA DRA driver) alongside DraNet
