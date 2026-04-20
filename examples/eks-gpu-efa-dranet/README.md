# EKS p4d.24xlarge GPU + EFA dranet Demo

End-to-end demo of topology-aware GPU + EFA allocation using
[Dynamic Resource Allocation (DRA)](https://kubernetes.io/docs/concepts/scheduling-eviction/dynamic-resource-allocation/)
on Amazon EKS with [p4d.24xlarge](https://aws.amazon.com/ec2/instance-types/p4/) instances.

## Context

### VM: p4d.24xlarge

Each node has:

| Resource | Count | Detail |
|---|---|---|
| GPU | 8 x NVIDIA A100-SXM4-40GB | NVSwitch all-to-all |
| EFA | 4 x Elastic Fabric Adapter | 400 Gb/s each |
| ENA | 4 x Elastic Network Adapter | Standard networking |

PCIe root topology:

| PCIe Root | GPUs | EFA | ENA |
|---|---|---|---|
| `pci0000:10` | gpu-0, gpu-1 | rdmap16s27 | ens33 |
| `pci0000:20` | gpu-2, gpu-3 | rdmap32s27 | ens65 |
| `pci0000:90` | gpu-4, gpu-5 | rdmap144s27 | ens129 |
| `pci0000:a0` | gpu-6, gpu-7 | rdmap160s27 | ens161 |

### DRA device attributes

**GPU** (driver: `gpu.nvidia.com`):

| Device | pciBusID | pcieRoot |
|---|---|---|
| gpu-0 | 0000:10:1c.0 | pci0000:10 |
| gpu-1 | 0000:10:1d.0 | pci0000:10 |
| gpu-2 | 0000:20:1c.0 | pci0000:20 |
| gpu-3 | 0000:20:1d.0 | pci0000:20 |
| gpu-4 | 0000:90:1c.0 | pci0000:90 |
| gpu-5 | 0000:90:1d.0 | pci0000:90 |
| gpu-6 | 0000:a0:1c.0 | pci0000:a0 |
| gpu-7 | 0000:a0:1d.0 | pci0000:a0 |

**EFA** (driver: `dra.net`):

| Device | pciAddress | rdmaDevice | pcieRoot |
|---|---|---|---|
| pci-0000-10-1b-0 | 0000:10:1b.0 | rdmap16s27 | pci0000:10 |
| pci-0000-20-1b-0 | 0000:20:1b.0 | rdmap32s27 | pci0000:20 |
| pci-0000-90-1b-0 | 0000:90:1b.0 | rdmap144s27 | pci0000:90 |
| pci-0000-a0-1b-0 | 0000:a0:1b.0 | rdmap160s27 | pci0000:a0 |

Both drivers publish `resource.kubernetes.io/pcieRoot`, enabling cross-driver
topology-aware co-selection via CEL selectors in ResourceClaimTemplates.

See `resourceslice-gpu.yaml` and `resourceslice-dranet.yaml` for the full
ResourceSlice objects from a live node.

## Files

| File | Description |
|---|---|
| `resource-claim-template.yaml` | Default `ResourceClaimTemplate` requesting 1 GPU + 1 EFA |
| `mpi-job.yaml` | `MPIJob` that runs `all_reduce_perf` across 2 workers via EFA |
| `resourceslice-gpu.yaml` | Live GPU `ResourceSlice` from a p4d.24xlarge node (reference) |
| `resourceslice-dranet.yaml` | Live NIC `ResourceSlice` from a p4d.24xlarge node (reference) |
| `resourceclaim.yaml` | Example allocated `ResourceClaim` showing GPU + EFA binding (reference) |

## ResourceClaimTemplates

### `gpu-efa-aligned` -- GPU + EFA on the same PCIe root

```yaml
requests:
- name: gpu
  exactly:
    deviceClassName: gpu.nvidia.com
    count: 1
    selectors:
    - cel:
        expression: 'device.attributes["resource.kubernetes.io"].pcieRoot == "pci0000:10"'
- name: efa
  exactly:
    deviceClassName: efa-rdma
    count: 1
    selectors:
    - cel:
        expression: 'device.attributes["resource.kubernetes.io"].pcieRoot == "pci0000:10"'
```

GPU and EFA share `pci0000:10` -- direct PCIe path enables GDRDMA.

### `gpu-efa-unaligned` -- GPU + EFA on different PCIe roots

```yaml
requests:
- name: gpu
  exactly:
    deviceClassName: gpu.nvidia.com
    count: 1
    selectors:
    - cel:
        expression: 'device.attributes["resource.kubernetes.io"].pcieRoot == "pci0000:10"'
- name: efa
  exactly:
    deviceClassName: efa-rdma
    count: 1
    selectors:
    - cel:
        expression: 'device.attributes["resource.kubernetes.io"].pcieRoot == "pci0000:a0"'
```

GPU on `pci0000:10`, EFA on `pci0000:a0` -- cross-PCIe, no direct path for GDR.

## Prerequisites

```bash
# Install MPI Operator
kubectl apply --server-side -k "https://github.com/kubeflow/mpi-operator/manifests/overlays/standalone?ref=v0.7.0"

# Install NVIDIA DRA driver
helm install nvidia-dra-driver-gpu nvidia/nvidia-dra-driver-gpu \
  --namespace nvidia-dra-driver --create-namespace \
  --set nvidiaDriverRoot=/ \
  --set gpuResourcesEnabledOverride=true \
  --set controller.affinity=null \
  --set 'controller.tolerations[0].key=nvidia.com/gpu' \
  --set 'controller.tolerations[0].operator=Exists' \
  --set 'controller.tolerations[0].effect=NoSchedule' \
  --set 'kubeletPlugin.tolerations[0].key=nvidia.com/gpu' \
  --set 'kubeletPlugin.tolerations[0].operator=Exists' \
  --set 'kubeletPlugin.tolerations[0].effect=NoSchedule'

# Label GPU nodes (if NFD is not installed)
kubectl label node <gpu-node> nvidia.com/gpu.present=true

# Create EFA device class (if not already present)
kubectl apply -f - <<EOF
apiVersion: resource.k8s.io/v1
kind: DeviceClass
metadata:
  name: efa-rdma
spec:
  selectors:
  - cel:
      expression: >-
        device.driver == "dra.net" &&
        device.attributes["dra.net"].rdma == true &&
        device.attributes["dra.net"].pciVendor == "Amazon.com, Inc."
EOF
```

## Usage

```bash
# Apply templates and run test
kubectl apply -f resource-claim-template.yaml
kubectl apply -f mpi-job.yaml

# Wait for workers then stream launcher logs
kubectl wait --for=condition=ready pod \
  -l training.kubeflow.org/job-name=nccl-test-dra,training.kubeflow.org/job-role=worker \
  --timeout=300s
launcher=$(kubectl get pods \
  -l training.kubeflow.org/job-name=nccl-test-dra,training.kubeflow.org/job-role=launcher \
  -o jsonpath='{.items[0].metadata.name}')
kubectl logs -f "${launcher}"
```

## Benchmark Results

2-node `all_reduce_perf`, 1 GPU per worker, EFA transport via `aws-ofi-nccl 1.16.3`.

| Template | GPU (PCIe Root) | EFA (PCIe Root) | Relation | Transport | GDR | Avg busbw |
|---|---|---|---|---|---|---|
| `gpu-efa-aligned` | gpu-0 (`pci0000:10`) | rdmap16s27 (`pci0000:10`) | Same | NET/Libfabric/GDRDMA | Yes | **~11.35 GB/s** |
| `gpu-efa-unaligned` | gpu-0 (`pci0000:10`) | rdmap160s27 (`pci0000:a0`) | Cross | NET/Libfabric | No | **~6.04 GB/s** |

### Key observations

**PCIe topology alignment matters ~1.9x:**
Cross-PCIe-root placement degrades performance from ~11.35 GB/s to ~6.04 GB/s
with the same GPU and EFA count. Two compounding penalties:

1. **GDR disabled** -- NCCL cannot use GPU Direct RDMA when the EFA adapter has
   no direct PCIe path to the GPU (`GDR 0` vs `GDR 1` in NCCL logs).
2. **Cross-root PCIe traffic** -- data must traverse the CPU/PCIe switch fabric
   between separate PCIe root complexes on every transfer.

**DRA enables topology-aware placement:**
The `resource.kubernetes.io/pcieRoot` attribute is published by both the NVIDIA
GPU DRA driver and dranet, enabling CEL selectors to co-locate GPU and EFA on
the same PCIe root without hardcoding device names.
