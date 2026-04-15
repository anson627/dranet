# EKS EFA Baseline Demo

Deploy DraNet on an EKS cluster with Trainium nodes (trn1.32xlarge) to discover
EFA network devices via DRA and inspect the resulting ResourceSlice. This demo
focuses on deploying the DRA driver and DraNet and dumping the ResourceSlice --
no end-to-end NCCL or collective communication tests are included.

## Prerequisites

- AWS CLI v2 configured with appropriate credentials
- `eksctl` >= 0.200.0
- `kubectl` >= 1.34
- An active Capacity Block reservation for trn1.32xlarge in `us-east-1b` (use1-az4)

## VM: trn1.32xlarge

Each node has:

| Resource | Count | Detail |
|---|---|---|
| Trainium chips | 16 | 32 NeuronCores total |
| EFA | 8 x Elastic Fabric Adapter | 100 Gbps each, RDMA-capable |
| vCPU | 128 | |
| Memory | 512 GiB | |
| NUMA nodes | 2 | 8 Trainium chips + 4 EFA per NUMA node |

Network interfaces:
- Network card 0: Primary ENI (ens32) -- standard networking
- Network cards 1-7: EFA interfaces -- dual ENA + OS-bypass RDMA

EFA devices appear as PCI devices with:
- PCI Vendor: `1d0f` (Amazon)
- Kernel driver: `efa`
- RDMA devices under `/sys/class/infiniband/efa_*`
- Character devices: `/dev/infiniband/uverbs*`, `/dev/infiniband/rdma_cm`

## Step 1: Create EKS Cluster

This demo assumes you already have an active Capacity Block reservation for
trn1.32xlarge instances. Create one via the AWS console or CLI before proceeding.

The `setup.sh` script auto-detects the active reservation, patches `cluster.yaml`
with the reservation ID, and creates the EKS cluster.

```bash
./setup.sh
```

Or pass the reservation ID explicitly:

```bash
./setup.sh -i cr-XXXXXXXXXXXXXXXXX
```

See `cluster.yaml` for the full cluster configuration. Key points:
- Kubernetes 1.34 with DRA support
- Fixed-size managed node group with 2x trn1.32xlarge instances in us-east-1b
- Capacity Block for ML for guaranteed Trainium availability
- EFA enabled on the node group (8 EFA interfaces)
- Placement group for optimal inter-node bandwidth
- AL2023 Neuron AMI with EFA and Neuron drivers pre-installed

## Step 2: Deploy DraNet

EKS 1.34 ships with containerd v2, which enables NRI by default -- no extra
configuration is needed.

Install DraNet using the static install manifest:

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
kubectl get resourceslices -o yaml
```

### Expected ResourceSlice Output

On a trn1.32xlarge node, DraNet discovers 8 EFA devices (the primary ENI
ens32 is filtered out as an uplink interface):

```yaml
- name: pci-0000-10-19-0    # varies by instance
  attributes:
    dra.net/numaNode:
      int: 0                  # NUMA 0 or 1 depending on PCIe topology
    dra.net/pciAddress:
      string: "0000:10:19.0"
    dra.net/pciDevice:
      string: "Elastic Fabric Adapter (EFA)"
    dra.net/pciSubsystem:
      string: "efa1"
    dra.net/pciVendor:
      string: "Amazon.com, Inc."
    dra.net/rdma:
      bool: true              # EFA supports RDMA
    dra.net/rdmaDevice:
      string: "rdmap16s25"    # RDMA device name
    resource.aws.com/devicegroup1_id:
      string: "0000:10:19.0"  # individual EFA device
    resource.aws.com/devicegroup4_id:
      string: "178a9e264a26793e"  # group of 4 Neuron devices
    resource.aws.com/devicegroup8_id:
      string: "aa86c05423d6e8cd"  # group of 8 Neuron devices
    resource.aws.com/devicegroup16_id:
      string: "fb838249c99a6341"  # all 16 Neuron devices on instance
    resource.kubernetes.io/pcieRoot:
      string: "pci0000:10"
```

See `resourceslice-expected.yaml` for the full reference output with all 8 devices.

Key observations:
- `dra.net/rdma: true` -- EFA devices have RDMA capability
- `dra.net/pciVendor: "Amazon.com, Inc."` -- Amazon PCI vendor
- `dra.net/numaNode` -- topology info for Neuron-NIC alignment
- `resource.aws.com/devicegroup*_id` -- Neuron device group attributes for topology-aware scheduling (auto-detected via IMDS)
- `resource.kubernetes.io/pcieRoot` -- PCIe root complex
- The primary ENI (ens32) is automatically filtered out as an uplink interface
- 4 EFA on NUMA 0 (PCIe roots 0x10, 0x20), 4 EFA on NUMA 1 (PCIe roots 0x90, 0xa0)

### Useful Inspection Commands

```bash
# Show only RDMA-capable devices
kubectl get resourceslices -o json | \
  jq '.items[].spec.devices[] | select(.attributes["dra.net/rdma"].bool == true) | .name'

# Show NUMA node distribution
kubectl get resourceslices -o json | \
  jq '.items[].spec.devices[] | {name: .name, numa: .attributes["dra.net/numaNode"].int, rdma: .attributes["dra.net/rdma"].bool}'

# Show PCI topology
kubectl get resourceslices -o json | \
  jq '.items[].spec.devices[] | {name: .name, pciRoot: .attributes["resource.kubernetes.io/pcieRoot"].string, numa: .attributes["dra.net/numaNode"].int}'
```

## Cleanup

Delete the EKS cluster:

```bash
./cleanup.sh
```

Note: this only deletes the cluster. Capacity Block reservations are managed
separately via the AWS console or CLI.

## Files

| File | Description |
|---|---|
| `setup.sh` | Creates the EKS cluster and neuron-efa node group with Capacity Block + EFA |
| `cleanup.sh` | Deletes the EKS cluster and launch template (capacity reservation managed separately) |
| `cluster.yaml` | eksctl cluster config for the control plane and system node group |
| `resourceslice-expected.yaml` | Reference ResourceSlice from a trn1.32xlarge node |

## Notes

- **Capacity Block** -- this demo uses a Capacity Block for ML for guaranteed trn1.32xlarge availability; the reservation is managed outside of this demo and billed whether instances are running or not
- **EFA drivers** -- included in EKS-optimized AL2023 Neuron AMIs; no additional driver install required
- **VPC CNI** -- remains responsible for primary pod networking (eth0); DraNet manages the secondary EFA interfaces
- **RDMA mode** -- check with `rdma system` inside a pod; EFA supports both shared and exclusive netns modes
- **Neuron DRA driver** -- this demo focuses on network devices only; for Neuron + NIC topology alignment, deploy a Neuron DRA driver alongside DraNet
- **eksctl capacity block bug** -- eksctl does not correctly pass the capacity reservation ID into the launch template, so `setup.sh` creates the neuron-efa node group via CloudFormation directly instead of eksctl
