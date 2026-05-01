# AKS MI300X RCCL + dranet Example

End-to-end example of RDMA NIC allocation using
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

This example assumes the following are already installed on the AKS cluster:

- dranet DaemonSet running on the GPU nodes (see `install-containerd-1.7yaml`
  if the nodes do not have NRI enabled in containerd by default; pass
  `--move-ib-interfaces=false` if the ConnectX VFs are in IB mode so dranet
  publishes `rdmaDevice` attributes)
- `amdgpu` kernel driver installed on the nodes
- AMD GPU Operator (`rocm/gpu-operator-charts`) with `DeviceConfig` applied,
  advertising `amd.com/gpu` as an extended resource
- MPI Operator v0.7.0

Verify with:

```bash
kubectl get resourceslices                             # dranet NIC slices
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

The dranet NRI plugin injects only the `/dev/infiniband/uverbsN` and
`/dev/infiniband/rdma_cm` char devices that correspond to the DRA-allocated
NIC(s) — without `privileged: true`, other `uverbs*` devices are not visible
inside the pod.

## Files

| File | Description |
|---|---|
| `resource-claim-template.yaml` | Three `ResourceClaimTemplate` objects for the test cases |
| `mpi-job.yaml` | `MPIJob` for the 8 GPU × 8 NIC case with DMABUF GDR (`maja/rccl-tests:rocm-7.0.2-gfx942`, RCCL 2.27.7 / ROCm 7.0.2) |
| `resourceslice-dranet.yaml` | Live NIC `ResourceSlice` from an MI300X node (reference) |

## ResourceClaimTemplates

| Template | NIC(s) selected | Selector |
|---|---|---|
| `4nic-same-numa` | 4 × NUMA-0 NICs (`mlx5_0`..`mlx5_3`) | `rdma == true && numaNode == 0` |
| `4nic-cross-numa` | 4 × NUMA-1 NICs (`mlx5_4`..`mlx5_7`) | `rdma == true && numaNode == 1` |
| `8nic-all` | all 8 RDMA NICs per worker | `rdma == true` |

## Usage

```bash
# Install MPI Operator (if not already installed)
kubectl apply --server-side -k "https://github.com/kubeflow/mpi-operator/manifests/overlays/standalone?ref=v0.7.0"

# Apply templates
kubectl apply -f resource-claim-template.yaml

# 8 GPU × 8 NIC with DMABUF GDR. Switch templates by editing
# `resourceClaimTemplateName:` in mpi-job.yaml.
kubectl apply -f mpi-job.yaml

# Wait for workers, then stream launcher logs
kubectl wait --for=condition=ready \
  pod -l training.kubeflow.org/job-name=nccl-test-dra,training.kubeflow.org/job-role=worker \
  --timeout=600s
launcher=$(kubectl get pods \
  -l training.kubeflow.org/job-name=nccl-test-dra,training.kubeflow.org/job-role=launcher \
  -o jsonpath='{.items[0].metadata.name}')
kubectl logs -f "${launcher}"

# Verify device isolation in a worker
kubectl exec nccl-test-dra-worker-0 -- ls /dev/infiniband/
```

## Verifying DRA allocation

`ResourceClaims` created from the template will show the allocated devices:

```bash
kubectl get resourceclaims
kubectl get resourceclaims -o yaml | grep -E 'name:|device:' | head -40
```

## Benchmark Results

2-node `all_reduce_perf` across MI300X nodes, NIC set controlled by the DRA
template. Each row corresponds to a different `MPIJob` + `ResourceClaimTemplate`
combination. The progression shows how topology-aware NIC selection — expressed
as a single CEL expression in the claim template — moves throughput across more
than an order of magnitude on the same hardware.

| Scenario | Template | GPUs × NICs | Avg busbw | Peak busbw |
|---|---|---|---|---|
| Cross-NUMA NIC selection, host-staged | `4nic-cross-numa` | 4 × 4 | 6.10 GB/s | 6.19 GB/s |
| Same-NUMA NIC selection, host-staged | `4nic-same-numa` | 4 × 4 | 42.53 GB/s | 49.28 GB/s |
| All NICs, DMABUF GDR | `8nic-all` | 8 × 8 | 67.73 GB/s | 78.81 GB/s |

### Key observations

**Same-NUMA NIC selection is ~7× faster than cross-NUMA at 4 GPU × 4 NIC.**
Without GDR, every GPU↔NIC byte is staged through host memory. When multiple
local GPUs share a cross-NUMA host path, throughput collapses far below the
per-NIC line rate; confining the claim to same-NUMA NICs keeps the aggregate
near the single-HCA ceiling.

**GDR (DMABUF) breaks past the host-staging ceiling.** With RCCL 2.27.7 / ROCm
7.0.2 and `NCCL_DMABUF_ENABLE=1`, 8 GPUs each DMA straight to their allocated
HCA. The 8 × 8 aggregate climbs past 78 GB/s — an order of magnitude above the
cross-NUMA case on the same hardware, with no change except the claim template.

**Isolation confirmed.** Each template injects exactly the allocated
`/dev/infiniband/uverbsN` char devices into the pod:
`4nic-same-numa` → `uverbs0..uverbs3`,
`4nic-cross-numa` → `uverbs4..uverbs7`,
`8nic-all` → `uverbs0..uverbs7`. dranet's NRI plugin does this without
`privileged: true`.

### Host-side prerequisites for best performance

- **Give each worker a proper `/dev/shm`.** ROCm 6.3 / 7.0+ needs >64 MiB of
  shared memory; mount an `emptyDir: {medium: Memory, sizeLimit: 8Gi}` at
  `/dev/shm` as shown in the MPIJob. Without this, RCCL init fails with
  `No space left on device (28)` while creating `/dev/shm/nccl-*`.
- **Allow enough warmup time for OpenMPI.** The launcher `sleep 60` at the top
  of `mpirun` gives workers time to start `sshd` before the launcher connects;
  otherwise mpirun hits `ORTE does not know how to route a message` and the
  MPIJob backs off.

## Notes

- `amd.com/gpu: N` in the worker spec reserves MI300X GPUs via the AMD device
  plugin — GPUs are *not* (yet) managed as DRA devices in this demo. The
  combination of a classic extended-resource GPU request with a DRA NIC claim
  is fully supported by the scheduler.
- `NCCL_IB_HCA=mlx5` tells RCCL to use all `mlx5_*` HCAs visible in the pod;
  because dranet only exposes the DRA-allocated ones, RCCL automatically
  restricts itself to those.
- For placement-group-aware scheduling across multi-node MPI jobs, add a CEL
  predicate on `azure.dra.net/placementGroupId` — see
  `examples/aks-gb300-placement-group/` for the pattern.
