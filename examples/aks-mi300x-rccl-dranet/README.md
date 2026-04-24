# AKS MI300X RCCL + dranet Demo

End-to-end demo of RDMA NIC allocation using
[Dynamic Resource Allocation (DRA)](https://kubernetes.io/docs/concepts/scheduling-eviction/dynamic-resource-allocation/)
on Azure Kubernetes Service (AKS) with the [ND MI300X-v5 size][mi300x]
(AMD Instinct MI300X GPUs + Mellanox ConnectX-7 VFs).

GPUs are scheduled via the AMD GPU Operator (`amd.com/gpu` device plugin), and
the eight ConnectX VFs on each node are scheduled via dranet as DRA devices.

[mi300x]: https://learn.microsoft.com/en-us/azure/virtual-machines/sizes/gpu-accelerated/nd-mi300x-v5-series

## Context

### VM: ND MI300X v5 (`Standard_ND96isr_MI300X_v5`)

Each node has:

| Resource | Count | Detail |
|---|---|---|
| GPU | 8 × AMD Instinct MI300X | 192 GB HBM3, Infinity Fabric all-to-all |
| NIC | 8 × Mellanox ConnectX-7 VF | 400 Gb/s InfiniBand each |
| NUMA nodes | 2 | 4 GPU + 4 NIC per NUMA node |

### Cluster prerequisites

This example assumes the following are already installed on the AKS cluster
(see `demo/aks/gpu/amd.sh` in this repo's companion scripts):

- dranet DaemonSet running on the GPU nodes
- `amdgpu` kernel driver installed on the nodes
- AMD GPU Operator (`rocm/gpu-operator-charts`) with `DeviceConfig` applied,
  advertising `amd.com/gpu` as an extended resource
- MPI Operator v0.7.0

Verify with:

```bash
kubectl get resourceslices -l ''                       # dranet NIC slices
kubectl get nodes -l accelerator=amd \
  -o custom-columns='NODE:.metadata.name,GPU:.status.allocatable.amd\.com/gpu'
```

### DRA device attributes (dranet)

Live `ResourceSlice` for one node (`resourceslice-dranet.yaml`):

| Device | pciAddress | rdmaDevice | NUMA |
|---|---|---|---|
| pci-0101-00-00-0 | 0101:00:00.0 | mlx5_0 | 0 |
| pci-0102-00-00-0 | 0102:00:00.0 | mlx5_1 | 0 |
| pci-0103-00-00-0 | 0103:00:00.0 | mlx5_2 | 0 |
| pci-0104-00-00-0 | 0104:00:00.0 | mlx5_3 | 0 |
| pci-0105-00-00-0 | 0105:00:00.0 | mlx5_4 | 1 |
| pci-0106-00-00-0 | 0106:00:00.0 | mlx5_5 | 1 |
| pci-0107-00-00-0 | 0107:00:00.0 | mlx5_6 | 1 |
| pci-0108-00-00-0 | 0108:00:00.0 | mlx5_7 | 1 |

Azure metadata attached to every device:

- `azure.dra.net/vmSize` — `Standard_ND96isr_MI300X_v5`
- `azure.dra.net/placementGroupId` — shared across VMs in the same IB fabric

The VFs are advertised in Ethernet (RoCE) mode on MI300X, so they have an
`ifName` in addition to `rdmaDevice`. The dranet NRI plugin injects only the
`/dev/infiniband/uverbsN` and `/dev/infiniband/rdma_cm` char devices that
correspond to the DRA-allocated NIC(s) — without `privileged: true`, other
`uverbs*` devices are not visible inside the pod.

## Files

| File | Description |
|---|---|
| `resource-claim-template.yaml` | Three `ResourceClaimTemplate` objects for the three test cases |
| `mpi-job.yaml` | `MPIJob` that runs `rccl-tests/all_reduce_perf` across 2 workers × 8 GPUs |
| `resourceslice-dranet.yaml` | Live NIC `ResourceSlice` from an MI300X node (reference) |

The RCCL test container image is built by `demo/aks/gpu/rccl.sh` from
`demo/aks/gpu/rccl/Dockerfile` (ROCm 6.3.4 + `rccl-tests` built with
`GPU_TARGETS=gfx942` for MI300X).

## ResourceClaimTemplates

Three templates are defined. Change `mpi-job.yaml` `resourceClaimTemplateName:`
to switch between them.

### `8nic-all` — all 8 RDMA NICs per worker (default)

Each worker pod requests all 8 ConnectX VFs, pinned to the MI300X VM size so
the scheduler never lands the workload on a foreign SKU:

```yaml
- name: nic
  exactly:
    deviceClassName: dranet.net
    count: 8
    selectors:
    - cel:
        expression: 'device.attributes["dra.net"]["rdma"] == true'
    - cel:
        expression: >-
          device.attributes["azure.dra.net"]["vmSize"] == "Standard_ND96isr_MI300X_v5"
```

This is the shape most MI300X RCCL jobs want: 8 GPUs paired with 8 NICs,
full fabric bandwidth per worker.

### `4nic-numa0` — 4 NICs on NUMA 0

```yaml
- name: nic
  exactly:
    deviceClassName: dranet.net
    count: 4
    selectors:
    - cel:
        expression: >-
          device.attributes["dra.net"]["rdma"] == true &&
          device.attributes["dra.net"]["numaNode"] == 0
```

DRA picks 4 distinct NUMA-0 devices (`mlx5_0`–`mlx5_3`). Use when pinning a
partial workload to one NUMA domain.

### `1nic-single` — single specific NIC

```yaml
- name: nic
  exactly:
    deviceClassName: dranet.net
    count: 1
    selectors:
    - cel:
        expression: 'device.attributes["dra.net"]["rdmaDevice"] == "mlx5_0"'
```

Useful for verifying NRI-based device isolation — only `/dev/infiniband/uverbs0`
should appear inside the pod.

## Usage

```bash
# Install MPI Operator (if not already installed)
kubectl apply --server-side -k "https://github.com/kubeflow/mpi-operator/manifests/overlays/standalone?ref=v0.7.0"

# Apply templates
kubectl apply -f resource-claim-template.yaml

# (Optional) switch templates by editing mpi-job.yaml:
#   resourceClaimTemplateName: 8nic-all | 4nic-numa0 | 1nic-single

kubectl apply -f mpi-job.yaml

# Wait for workers, then stream launcher logs
kubectl wait --for=condition=ready \
  pod -l training.kubeflow.org/job-name=rccl-test-dra,training.kubeflow.org/job-role=worker \
  --timeout=600s
launcher=$(kubectl get pods \
  -l training.kubeflow.org/job-name=rccl-test-dra,training.kubeflow.org/job-role=launcher \
  -o jsonpath='{.items[0].metadata.name}')
kubectl logs -f "${launcher}"

# Verify device isolation in a worker
kubectl exec rccl-test-dra-worker-0 -- ls /dev/infiniband/
```

## Verifying DRA allocation

`ResourceClaims` created from the template will show the allocated devices:

```bash
kubectl get resourceclaims
kubectl get resourceclaims -o yaml | grep -E 'name:|device:' | head -40
```

For the default `8nic-all` template, each worker's claim should list all eight
`pci-010N-00-00-0` devices from the node it landed on.

## Notes

- `amd.com/gpu: 8` in the worker spec reserves all 8 MI300X GPUs via the AMD
  device plugin — GPUs are *not* (yet) managed as DRA devices in this demo.
  The combination of a classic extended-resource GPU request with a DRA NIC
  claim is fully supported by the scheduler.
- `NCCL_IB_HCA=mlx5` tells RCCL to use all `mlx5_*` HCAs visible in the pod;
  because dranet only exposes the DRA-allocated ones, RCCL automatically
  restricts itself to those.
- For placement-group-aware scheduling across multi-node MPI jobs, add a CEL
  predicate on `azure.dra.net/placementGroupId` — see
  `examples/aks-gb300-placement-group/` for the pattern.
