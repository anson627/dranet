# EKS Neuron + EFA Topology-Aware Scheduling with DRA

This example demonstrates topology-aware co-scheduling of AWS Neuron devices
and EFA network interfaces on EKS using Kubernetes Dynamic Resource Allocation
(DRA). Two DRA drivers work together:

- **DraNet** discovers EFA interfaces and publishes them as DRA resources with
  RDMA, NUMA, PCIe, and Neuron device group attributes
- **Neuron DRA driver** publishes Neuron accelerator devices with matching
  device group attributes

Both drivers expose `resource.aws.com/devicegroup*_id` attributes that map the
Neuron-to-EFA hardware topology. A `ResourceClaimTemplate` with `matchAttribute`
constraints ensures the scheduler co-allocates Neuron devices and EFA interfaces
that share the same physical interconnect path.

## How it works

On a trn1.32xlarge instance (16 Neuron devices, 8 EFA interfaces, 2 NUMA nodes):

```
ResourceClaimTemplate (neuron-efa-16x8)
  requests:
    neuron-devices: 16x neuron.aws.com
    efa-devices:     8x efa-rdma
  constraints:
    matchAttribute: resource.aws.com/devicegroup16_id
         |
         v
    Scheduler matches devicegroup16_id across both drivers
         |
         +---> neuron-device-0..15  (neuron.aws.com)   --+-- same node,
         +---> pci-0000-*-*-0      (dra.net)           --+-- same devicegroup16_id
```

The `devicegroup` hierarchy enables scheduling at different granularities:

| Attribute | Granularity | Use case |
|---|---|---|
| `devicegroup16_id` | All 16 devices on the instance | Full-node jobs |
| `devicegroup8_id` | 8 devices sharing an interconnect | Half-node jobs |
| `devicegroup4_id` | 4 devices sharing a closer path | Quarter-node jobs |

## Files

| File | Description |
|---|---|
| `resource-claim-template.yaml` | ResourceClaimTemplate requesting 16 Neuron + 8 EFA with topology constraint |
| `mpi-job.yaml` | MPIJob running nccom-test all_reduce benchmark across 2 nodes |
| `resourceslice-dranet.yaml` | Captured DraNet ResourceSlice from a trn1.32xlarge node |
| `resourceslice-neuron.yaml` | Captured Neuron DRA driver ResourceSlice from the same node |

## Prerequisites

- EKS 1.34+ cluster with 2x trn1.32xlarge nodes (EFA enabled)
- [DraNet](https://github.com/kubernetes-sigs/dranet) installed
- [Neuron DRA driver](https://awsdocs-neuron.readthedocs-hosted.com/en/latest/containers/neuron-dra.html) installed
- [MPI Operator](https://github.com/kubeflow/mpi-operator) installed

## Quick start

```bash
# Install prerequisites
kubectl apply -f https://raw.githubusercontent.com/kubernetes-sigs/dranet/main/install.yaml
helm upgrade --install neuron-helm-chart oci://public.ecr.aws/neuron/neuron-helm-chart \
  --set "devicePlugin.enabled=false" --set "npd.enabled=false" --set "draDriver.enabled=true"
kubectl apply --server-side -f https://github.com/kubeflow/mpi-operator/raw/refs/heads/master/deploy/v2beta1/mpi-operator.yaml

# Deploy the benchmark
kubectl apply -f resource-claim-template.yaml
kubectl apply -f mpi-job.yaml

# Watch results
kubectl logs -f -l training.kubeflow.org/job-name=nccom-test,training.kubeflow.org/job-role=launcher
```

## Observed results

nccom-test all_reduce across 2x trn1.32xlarge over EFA RDMA:

```
   size(B)    count(elems)     type    time:avg(us)    algbw(GB/s)    busbw(GB/s)
      65536           65536    uint8           43.77           1.50           1.50
     524288          524288    uint8          126.55           4.14           4.14
    4194304         4194304    uint8          517.65           8.10           8.10
    8388608         8388608    uint8          949.92           8.83           8.83
  134217728       134217728    uint8        21125.05           6.35           6.35
 1073741824      1073741824    uint8       167629.75           6.41           6.41
 2147483648      2147483648    uint8       336085.22           6.39           6.39
```

Peak bus bandwidth: **8.83 GB/s (~70.6 Gbps)** at 8 MiB.
Sustained large-message bandwidth: **~6.40 GB/s (~51.2 Gbps)**.
