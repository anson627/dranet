# Cross-Cloud RDMA Validation Proposal

Status: Proposal

## Summary

This proposal defines a reproducible workflow for validating DRANET-backed AI
workloads across Kubernetes environments, starting with Azure Kubernetes
Service (AKS) and CoreWeave Kubernetes Service (CKS).

The workflow is intended to answer one narrow question:

> Which parts of an RDMA AI workload are portable across environments, and
> where is provider-specific configuration still required?

The validation does not compare cloud performance. AKS GB300 and CKS B200
nodes have different accelerators, network designs, and fabric topologies, so
their bandwidth numbers are not directly comparable. Instead, the workflow
validates the portability of the workload structure, Kubernetes DRA API
patterns, device allocation evidence, and RDMA data-path verification.

## Motivation

A successfully allocated RDMA NIC does not prove that a distributed workload
has end-to-end connectivity. For example:

- AKS workers may receive valid HCAs but belong to incompatible InfiniBand
  placement groups.
- CKS workers may receive valid HCAs but belong to different fabrics,
  superpods, or leafgroups.
- A GPU and NIC may both be allocated but use an inefficient PCIe or NUMA
  path.
- NCCL may complete by falling back to sockets instead of using the claimed
  RDMA device.

The repository contains provider-specific examples and benchmark workloads,
but it does not yet provide a single procedure that separates the portable
contract from provider-specific topology discovery and proves which data path
the workload actually used.

## Goals

- Define a common validation contract for AKS and CKS.
- Reuse the same workload structure and DRA allocation pattern where possible.
- Document the exact provider-specific overlays and why each is necessary.
- Collect evidence from scheduling, device allocation, and workload runtime.
- Detect invalid fabric placement before starting an expensive benchmark.
- Emit machine-readable results alongside human-readable reports.
- Provide a procedure that another contributor can repeat on a fresh cluster.

## Non-goals

- Ranking cloud providers or accelerator platforms.
- Treating results from different hardware as an apples-to-apples benchmark.
- Claiming byte-for-byte YAML portability.
- Establishing a general performance guarantee from a small number of runs.
- Hiding provider-specific topology behind a premature common vocabulary.
- Replacing provider qualification, burn-in, or large-scale performance tests.

## Portability hypothesis

The initial hypothesis is:

> The workload intent and Kubernetes DRA API pattern are portable, while
> provider discovery, topology vocabulary, and some selectors remain
> provider-specific.

The validation must be capable of disproving or refining this hypothesis.

## Proposed repository structure

The first implementation can use the following structure:

```text
examples/cross-cloud-validation/
|-- README.md
|-- base/
|   |-- mpi-job.yaml
|   `-- kustomization.yaml
|-- overlays/
|   |-- aks/
|   |   |-- device-class.yaml
|   |   |-- resource-claim-template.yaml
|   |   `-- kustomization.yaml
|   `-- cks/
|       |-- device-class.yaml
|       |-- resource-claim-template.yaml
|       `-- kustomization.yaml
|-- scripts/
|   |-- preflight.sh
|   |-- validate-allocation.sh
|   `-- collect-results.sh
|-- result-schema.json
`-- results/
    |-- aks-gb300.md
    `-- cks-b200.md
```

The implementation should reuse or reference the existing examples instead of
duplicating them:

- `examples/azure_aks_examples/gb300/`
- `examples/coreweave_cks_examples/b200-infiniband/`
- `examples/distributed_training/`
- `examples/nixl-kv-transfer/`

The exact directory layout can change during review. The important boundary is
between a provider-neutral workload, provider overlays, common validation
scripts, and captured results.

## Expected portability boundary

| Layer | Expected to be portable | Expected to remain provider-specific |
|---|---|---|
| Workload | MPIJob shape, NCCL command, GPU and NIC claim pattern | Container images and platform tuning |
| Kubernetes API | DeviceClass, ResourceClaimTemplate, ResourceSlice, CEL selector mechanisms | Driver names, attribute domains, and selector values |
| Device allocation | DRA claim lifecycle and allocation evidence | GPU driver and NIC discovery implementation |
| Local topology | PCIe and NUMA relationship pattern | Available attributes and platform topology |
| Fabric topology | Requirement that workers share a reachable fabric domain | AKS placement groups and CKS fabric, superpod, leafgroup, and leaf-switch data |
| Device injection | NRI-based injection and RDMA device visibility checks | IB-only behavior and network-interface movement |
| Runtime validation | Claimed HCA identity, selected transport, GPUDirect RDMA, and benchmark output | Hardware-specific expectations and tuning |

The validation report must describe observed behavior. It must not mark a field
as portable solely because both providers have fields with similar names.

## Validation workflow

### 1. Record the environment

The collector records at least:

- Kubernetes version and enabled DRA API version.
- DRANET image, version, and relevant command-line options.
- containerd and NRI versions.
- Node instance type and architecture.
- GPU and HCA models.
- Relevant feature gates.
- Provider topology labels used by the test.

Credentials, internal addresses, unique cluster identifiers, and other
sensitive values must be removed before results are committed.

### 2. Run topology preflight checks

Before the benchmark begins, `preflight.sh` verifies that the selected workers
meet the required provider topology constraints.

Example failure output:

```text
FAIL: workers belong to different fabric domains
worker-0: fabric=FAB66, superpod=2
worker-1: fabric=FAB71, superpod=4

Device allocation may succeed, but end-to-end RDMA reachability is not
established.
```

The preflight should fail fast rather than deliberately leave an expensive GPU
job hanging.

### 3. Validate scheduling and allocation

The workflow captures:

- The node selected for each worker.
- ResourceClaim allocation status.
- The exact DRA device names allocated to each pod.
- GPU and NIC PCI addresses when available.
- GPU and NIC NUMA nodes when available.
- Relevant placement-group or fabric-domain attributes.
- The DeviceClass and selectors that affected allocation.

### 4. Validate device injection

The workflow verifies:

- The expected `/dev/infiniband` character devices are present.
- The allocated HCA is visible to the workload.
- Unallocated devices are not exposed when isolation is expected.
- Any required Linux network interface is present.
- The observed device identity matches the ResourceClaim allocation.

### 5. Validate the runtime data path

The NCCL validation must establish that successful completion was not caused
by a socket fallback. Evidence includes:

- NCCL transport selection from debug output.
- The selected HCA or interface.
- GPUDirect RDMA use when the platform supports it.
- Benchmark validation error count.
- Algorithmic and bus bandwidth at the tested message sizes.

The report should preserve only the minimum log excerpts necessary to support
these claims.

### 6. Repeat and summarize

Each documented performance observation should include at least three runs,
unless cluster availability prevents this. Reports include:

- All observed run values.
- Mean and variation.
- Message sizes and benchmark arguments.
- Any discarded run and the reason it was discarded.
- An explicit statement that results are platform-specific observations, not a
  provider performance comparison or guarantee.

## Result format

Each run should produce a machine-readable record. For example:

```json
{
  "schemaVersion": "v1alpha1",
  "environment": "cks-b200",
  "kubernetesVersion": "1.36",
  "gpu": "NVIDIA B200",
  "claimedDevice": "pci-0000-1a-00-0",
  "rdmaDevice": "ibp0",
  "transport": "NET/IBext_v11/0/GDRDMA",
  "fabricConstraintSatisfied": true,
  "messageSizeBytes": 1073741824,
  "busBandwidthGBps": 46.35,
  "validationErrors": 0
}
```

The final schema should distinguish collected facts, derived values, and
optional provider-specific attributes. Missing information must be represented
as unavailable rather than invented or inferred.

## Initial test matrix

| Environment | Accelerator | Fabric | Positive validation | Negative or preflight validation |
|---|---|---|---|---|
| AKS | NVIDIA GB300 | InfiniBand | Workers in a compatible placement group use the claimed HCA | Incompatible placement groups are rejected before the benchmark |
| CKS | NVIDIA B200 | InfiniBand | Workers in the required fabric, superpod, and leafgroup use the claimed HCA | Fabric-domain mismatch is rejected before the benchmark |

Additional platforms should be added only after the common contract works for
the initial two environments.

## Acceptance criteria

The initial work is complete when:

1. A contributor can follow one documented workflow on either AKS or CKS.
2. Provider-neutral workload content is clearly separated from overlays.
3. Preflight reports whether the selected nodes satisfy fabric constraints.
4. Allocation evidence connects a ResourceClaim to the device visible in the
   workload.
5. Runtime evidence identifies the RDMA transport and detects socket fallback.
6. Results conform to a documented machine-readable schema.
7. At least one AKS and one CKS report document the environment, commands,
   observations, limitations, and portability differences.
8. Committed artifacts contain no credentials or sensitive cluster data.

## Proposed implementation sequence

To keep reviews focused, the work should be split into small pull requests:

1. Add the result schema, collection contract, and workflow documentation.
2. Add the preflight and allocation-validation scripts.
3. Add a repeatable CKS B200 result.
4. Add a repeatable AKS GB300 result and the cross-environment comparison.

Later work may add the NIXL KV-cache benchmark, additional clouds, richer
accelerator-to-NIC affinity checks, or automated CI with simulated devices.

## Open questions

- Should the common workload be based first on `nccl-tests`, the existing
  PyTorch MFU workload, or both?
- Should provider-specific attributes be stored in a free-form result section
  or normalized into an experimental topology vocabulary?
- Which evidence is safe and useful to commit from production-shaped clusters?
- Can the preflight reuse a library shared with DRANET device discovery?
- Should the result schema be validated in CI before adding execution
  automation?
- How should a test represent an allocation that is valid locally but lacks a
  schedulable end-to-end fabric constraint?

## Expected outcome

The intended output is not a claim that the same manifest works unchanged on
every cloud. It is a reproducible description of:

- what Kubernetes and DRA make portable;
- what each provider must discover or configure;
- how to prove that allocation led to the intended RDMA path; and
- which upstream gaps still prevent end-to-end fabric-aware scheduling.

That evidence can guide DRANET development, provider integrations, and future
Kubernetes DRA discussions without turning the validation into a vendor
comparison.
