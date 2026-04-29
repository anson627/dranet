# AKS MI300X RCCL + dranet Exmaple

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
| `resource-claim-template.yaml` | Four `ResourceClaimTemplate` objects for the test cases |
| `mpi-job.yaml` | `MPIJob` that runs `rccl-tests/all_reduce_perf` across 2 workers × 8 GPUs |
| `resourceslice-dranet.yaml` | Live NIC `ResourceSlice` from an MI300X node (reference) |
| `results-*.log` | Raw launcher logs from each benchmark variant |

The RCCL test container is `ghcr.io/anson627/rccl-tests:rocm7` — ROCm 7.2.2 /
RCCL 2.27.7 built with `GPU_TARGETS=gfx942` for MI300X. The Dockerfile lives at
`demo/aks/gpu/rccl/Dockerfile` (set to base `rocm/dev-ubuntu-24.04:7.2.2` for
GDR via dma-buf support).

## ResourceClaimTemplates

Four templates are defined. Change `mpi-job.yaml` `resourceClaimTemplateName:`
to switch between them.

### `1nic-aligned` — 1 NIC on NUMA 0 (`mlx5_0`)

```yaml
- name: nic
  exactly:
    deviceClassName: dranet.net
    count: 1
    selectors:
    - cel:
        expression: 'device.attributes["dra.net"]["rdmaDevice"] == "mlx5_0"'
```

One NIC on the same NUMA node as GPU 0. Useful as a baseline for per-NIC
throughput and for verifying NRI-based device isolation: only
`/dev/infiniband/uverbs0` should appear inside the pod.

### `1nic-unaligned` — 1 NIC on NUMA 1 (`mlx5_4`)

```yaml
- name: nic
  exactly:
    deviceClassName: dranet.net
    count: 1
    selectors:
    - cel:
        expression: 'device.attributes["dra.net"]["rdmaDevice"] == "mlx5_4"'
```

One NIC on the opposite NUMA node. On systems with PCIe-only GPU↔NIC paths
this costs bandwidth (cross-NUMA); on MI300X, GPUs are fully mesh-connected
via XGMI so alignment matters much less — see *Benchmark Results* below.

### `2nic-aligned` — 2 NICs on NUMA 0

```yaml
- name: nic
  exactly:
    deviceClassName: dranet.net
    count: 2
    selectors:
    - cel:
        expression: >-
          device.attributes["dra.net"]["rdma"] == true &&
          device.attributes["dra.net"]["numaNode"] == 0
```

DRA picks 2 distinct NUMA-0 devices (`mlx5_0` + `mlx5_1`). Demonstrates
multi-device allocation from a homogeneous pool.

### `8nic-all` — all 8 RDMA NICs per worker

```yaml
- name: nic
  exactly:
    deviceClassName: dranet.net
    count: 8
    selectors:
    - cel:
        expression: 'device.attributes["dra.net"]["rdma"] == true'
```

Full-fabric shape: 8 GPUs paired with 8 NICs per worker.

## Usage

```bash
# Install MPI Operator (if not already installed)
kubectl apply --server-side -k "https://github.com/kubeflow/mpi-operator/manifests/overlays/standalone?ref=v0.7.0"

# Apply templates
kubectl apply -f resource-claim-template.yaml

# (Optional) switch templates by editing mpi-job.yaml:
#   resourceClaimTemplateName: 1nic-aligned | 1nic-unaligned | 2nic-aligned | 8nic-all

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

## Benchmark Results

2-node `all_reduce_perf`, 16 ranks × MI300X (8 GPUs per node), RCCL 2.27.7 /
ROCm 7.2.2, `NCCL_DMABUF_ENABLE=1` (dma-buf GDR). The NIC set visible to each
worker is controlled by the DRA template. Raw logs saved as
`results-<variant>.log`.

| Template | NICs visible | NUMA | Avg busbw | Peak busbw (8 GiB) |
|---|---|---|---|---|
| `1nic-aligned` | `mlx5_0` | NUMA 0 | 15.68 GB/s | 15.78 GB/s |
| `1nic-unaligned` | `mlx5_4` | NUMA 1 | 15.76 GB/s | 15.77 GB/s |
| `2nic-aligned` | `mlx5_0`+`mlx5_1` | NUMA 0 | 16.34 GB/s | 16.34 GB/s |

### Key observations

**NUMA alignment does NOT matter on MI300X.** Unlike GB300 (NVLink mesh + per-GPU
PCIe affinity to a NIC), MI300X has XGMI all-to-all between all 8 GPUs and the
GPU↔NIC PCIe paths go through the host PCIe complex. The aligned and unaligned
1-NIC runs are within noise (~15.7 GB/s).

**NIC count has a small sub-linear effect at this rank count.** Going from 1→2
NICs gains ~4%. At 16 ranks routed through 1–2 HCAs, the bottleneck is the
inter-node fabric link(s), not the NIC count itself. Bigger gains would show up
with higher rank counts / larger messages where the per-HCA queue pairs saturate.

**GDR is enabled via dma-buf.** RCCL 2.27.7 + ROCm 7.2.2 + kernel 6.8 provides
GDR through the dma-buf path (no `ib_peer_mem`/`amdp2p` kernel modules required):

```
Connected all rings, use ring PXN 0 GDR 1
NCCL_DMABUF_ENABLE set by environment to 1
```

Older RCCL 2.22 (ROCm 6.3.4) **segfaults** with `NCCL_DMABUF_ENABLE=1` — the
image base was upgraded to `rocm/dev-ubuntu-24.04:7.2.2` to fix this. Without
`NCCL_DMABUF_ENABLE=1`, RCCL falls back to host-memory staging and prints
`GDRDMA not enabled. Could not find memory_peers directory or peer_memory symbol`.

**Isolation confirmed.** With `1nic-aligned` the worker only sees `uverbs0`;
with `1nic-unaligned` only `uverbs4`. dranet's NRI plugin injects exactly the
allocated `/dev/infiniband/uverbsN` char devices without `privileged: true`.

### Host-side prerequisites for best performance

- **Disable NUMA auto-balancing on the GPU nodes** (`sysctl kernel.numa_balancing=0`).
  RCCL prints a `WARN NUMA auto balancing enabled` when this is on.
- **Give each worker a proper `/dev/shm`.** ROCm 7.2 needs >64 MiB of shared
  memory; mount an `emptyDir: {medium: Memory, sizeLimit: 8Gi}` at `/dev/shm`
  as shown in `mpi-job.yaml`. Without this, RCCL init fails with
  `No space left on device (28)` while creating `/dev/shm/nccl-*`.
- **Allow enough warmup time for OpenMPI.** The launcher `sleep 60` at the top
  of `mpirun` gives workers time to start `sshd` before the launcher connects;
  otherwise mpirun hits `ORTE does not know how to route a message` and the
  MPIJob backs off.

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
