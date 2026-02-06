# AKS RDMA Demo

Run a node pool with AKS VMs using InfiniBand RDMA (ND-series VMs)

## Prerequisites

- Azure subscription with quota for ND-series VMs
- Azure CLI (`az`) installed and authenticated
- `kubectl` configured to access your AKS cluster
- `dranetctl` binary built from this repository

## Setup AKS Cluster with RDMA Support

### 1. Create an AKS Cluster

Create a basic AKS cluster (if you don't have one):

```bash
az aks create \
  --resource-group myResourceGroup \
  --name myAKSCluster \
  --location eastus \
  --node-count 1 \
  --node-vm-size Standard_DS2_v2 \
  --enable-managed-identity \
  --generate-ssh-keys
```

Get credentials:

```bash
az aks get-credentials --resource-group myResourceGroup --name myAKSCluster
```

### 2. Create RDMA-Enabled Node Pool with dranetctl

Use `dranetctl` to create an InfiniBand-enabled node pool:

```bash
dranetctl aks acceleratorpod create rdma-pool \
  --subscription <your-subscription-id> \
  --resource-group myResourceGroup \
  --cluster myAKSCluster \
  --location eastus \
  --vm-size Standard_ND96asr_v4 \
  --node-count 2 \
  --enable-ppg
```

This creates:
- A node pool with 2 Standard_ND96asr_v4 VMs (8x A100 GPUs, 200 Gb/s HDR InfiniBand per VM)
- A proximity placement group for optimal InfiniBand connectivity
- Node pool tagged with `dra.net/acceleratorpod: "true"`

### 3. List Accelerator Node Pools

```bash
dranetctl aks acceleratorpod list \
  --subscription <your-subscription-id> \
  --resource-group myResourceGroup \
  --cluster myAKSCluster
```

### 4. Get Node Pool Details

```bash
dranetctl aks acceleratorpod get rdma-pool \
  --subscription <your-subscription-id> \
  --resource-group myResourceGroup \
  --cluster myAKSCluster
```

## Install DRANET

After the node pool is created, install the DRANET DaemonSet to expose RDMA NICs:

```bash
kubectl apply -f https://raw.githubusercontent.com/kubernetes-sigs/dranet/main/manifests/dranet.yaml
```

The DRANET DaemonSet will:
- Run only on nodes labeled with `dra.net/acceleratorpod: "true"`
- Detect InfiniBand interfaces on ND-series VMs
- Create ResourceSlices with RDMA device information

Verify DRANET is running:

```bash
kubectl get pods -n kube-system -l app=dranet
```

Check that RDMA devices are exposed:

```bash
kubectl get resourceslices -o yaml | grep -A 10 "dra.net/rdma"
```

You should see attributes like:
- `dra.net/rdma: true`
- `azure.dra.net/vmSize: Standard_ND96asr_v4`
- `azure.dra.net/infinibandSupport: HDR`

## Deploy RDMA Test Workload

### 1. Apply the DeviceClass and Test Pods

```bash
kubectl apply -f examples/demo_aks_rdma/deviceclass.yaml
kubectl apply -f examples/demo_aks_rdma/rdma-perftest.yaml
```

This creates:
- A `DeviceClass` that selects RDMA-capable network interfaces
- A StatefulSet with 2 pods, each requesting one RDMA interface
- Pod anti-affinity to ensure pods run on different nodes

### 2. Verify Pods are Running

```bash
kubectl get pods -o wide
```

Expected output:
```
NAME              READY   STATUS    RESTARTS   AGE   IP            NODE
rdma-perftest-0   1/1     Running   0          2m    10.244.1.5    aks-rdma-pool-12345
rdma-perftest-1   1/1     Running   0          2m    10.244.2.3    aks-rdma-pool-67890
```

### 3. Check ResourceClaims

```bash
kubectl get resourceclaims
```

Expected output:
```
NAME                                        STATE                AGE
rdma-perftest-0-rdma-net-interface-xxxxx   allocated,reserved   3m
rdma-perftest-1-rdma-net-interface-xxxxx   allocated,reserved   3m
```

## Test RDMA Connectivity

### 1. Verify InfiniBand Interface in Pod

```bash
kubectl exec -it rdma-perftest-0 -- bash
```

Inside the pod:

```bash
# Check network interfaces
ip a

# Check InfiniBand devices
ls /dev/infiniband/

# Check RDMA links
rdma link
```

Expected output from `rdma link`:
```
link mlx5_0/1 state ACTIVE physical_state LINK_UP netdev ib0
```

### 2. Test RDMA Connectivity with rping

In pod 0 (server):
```bash
kubectl exec -it rdma-perftest-0 -- rping -s -v
```

Get the InfiniBand IP address from pod 0:
```bash
kubectl exec rdma-perftest-0 -- ip a show ib0 | grep "inet "
```

In pod 1 (client), use the IP from pod 0:
```bash
kubectl exec -it rdma-perftest-1 -- rping -c -a <pod-0-ib-ip> -C 3 -v -V
```

Successful output shows:
```
ping data: rdma-ping-0: ...
ping data: rdma-ping-1: ...
ping data: rdma-ping-2: ...
client DISCONNECT EVENT...
```

### 3. Benchmark with ib_write_bw

In pod 0 (server):
```bash
kubectl exec -it rdma-perftest-0 -- ib_write_bw
```

In pod 1 (client):
```bash
kubectl exec -it rdma-perftest-1 -- ib_write_bw <pod-0-ib-ip> -a --report_gbits
```

For ND96asr_v4 VMs with HDR InfiniBand (200 Gb/s), you should see bandwidth around 190-200 Gb/s.

## Azure-Specific RDMA Notes

### Supported VM Sizes

| VM Size | GPUs | InfiniBand | Bandwidth |
|---------|------|------------|-----------|
| Standard_ND96isr_H100_v5 | 8x H100 | NDR | 400 Gb/s |
| Standard_ND96asr_v4 | 8x A100 40GB | HDR | 200 Gb/s |
| Standard_ND128isr_NDR_GB200_v6 | 8x GB200 | NDR | 400 Gb/s |
| Standard_ND128isr_GB300_v6 | 8x GB300 | NDR | 400 Gb/s |

### InfiniBand Drivers

Azure ND-series VMs come with InfiniBand drivers pre-installed:
- OFED (OpenFabrics Enterprise Distribution)
- InfiniBand verbs libraries
- RDMA core utilities

No additional driver installation is required.

### Proximity Placement Groups

For optimal InfiniBand performance, use proximity placement groups (PPGs):
- PPGs ensure VMs are placed close together in the same datacenter
- This minimizes latency and maximizes bandwidth for InfiniBand traffic
- Use `--enable-ppg` flag when creating the node pool with `dranetctl`

## Cleanup

Delete the test workload:
```bash
kubectl delete -f examples/demo_aks_rdma/rdma-perftest.yaml
kubectl delete -f examples/demo_aks_rdma/deviceclass.yaml
```

Delete the RDMA node pool:
```bash
dranetctl aks acceleratorpod delete rdma-pool \
  --subscription <your-subscription-id> \
  --resource-group myResourceGroup \
  --cluster myAKSCluster
```

## Troubleshooting

### Pods not scheduling
- Check if nodes have the `dra.net/acceleratorpod: "true"` label
- Verify DRANET DaemonSet is running on RDMA nodes
- Check ResourceSlices are created: `kubectl get resourceslices`

### No InfiniBand devices in pod
- Verify the VM size supports InfiniBand (ND-series)
- Check DRANET detected the InfiniBand interface in ResourceSlices
- Ensure the DeviceClass selector matches the RDMA interfaces

### Low RDMA performance
- Verify proximity placement group is used
- Check all VMs are in the same availability zone
- Run `ibstat` to verify InfiniBand link is active
- Check for network congestion or other workloads

## References

- [Azure ND-series VMs](https://learn.microsoft.com/en-us/azure/virtual-machines/sizes-gpu)
- [Azure InfiniBand](https://learn.microsoft.com/en-us/azure/virtual-machines/workloads/hpc/enable-infiniband)
- [Kubernetes DRA](https://kubernetes.io/docs/concepts/scheduling-eviction/dynamic-resource-allocation/)
