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
- Reuse existing provider examples while preserving their documented usage.
- Identify common workload intent and DRA allocation patterns where possible.
- Document provider-specific configuration and why each difference is necessary.
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
- Requiring a shared Kustomize base or migrating existing example workflows.
- Requiring contributors to adopt `dranetctl` to perform the validation.

## Portability hypothesis

The initial hypothesis is:

> The workload intent and Kubernetes DRA API pattern are portable, while
> provider discovery, topology vocabulary, and some selectors remain
> provider-specific.

The validation must be capable of disproving or refining this hypothesis.

## Existing example compatibility

The website links to provider example directories and documents commands such
as `kubectl apply -f resource-claim-template.yaml` and
`kubectl apply -f mpi-job.yaml`. These examples remain the canonical runnable
manifests. The initial implementation must preserve their paths and documented
apply commands; it must not replace standalone manifests with patches that
require rendering before use.

The validation workflow should reuse or reference these examples instead of
duplicating them:

- `examples/azure_aks_examples/gb300/`
- `examples/coreweave_cks_examples/b200-infiniband/`
- `examples/distributed_training/`
- `examples/nixl-kv-transfer/`

Kustomize can remain an optional, additive convenience for the provider
examples. Existing Kustomize workflows, such as distributed training and NIXL
KV transfer, remain supported. Extracting a shared base is deferred until
validation establishes which workload content is actually common and the
effect on website examples can be reviewed separately.

The initial common boundary is the evidence and result contract, not a required
manifest layout. For example, the current AKS GB300 MPIJob uses a combined GPU
and NIC DRA claim, while the CKS B200 MPIJob uses a DRA NIC claim and
`nvidia.com/gpu` resource limits. Reports must capture this difference rather
than assume a common GPU allocation mechanism.

## Validation implementation and CLI

Substantive validation logic should live in a tested Go package: topology
checks, Kubernetes object relationships, allocated-to-visible device matching,
runtime evidence parsing, and structured result generation. Bash, if needed,
should be limited to simple command orchestration rather than implementing
these rules or assembling results through text processing.

The package should separate evidence collection from checks so that recorded
AKS and CKS fixtures can exercise the same validation logic in CI without
requiring GPU clusters. Reuse provider attribute definitions where appropriate,
without requiring the validator to initialize node-local device discovery.

An optional `dranetctl validate` command is a candidate interface to this
package. The current CLI exposes GKE management commands and shares the
repository's Go module, build targets, and Go test target. That provides a
starting point, but does not establish cross-cloud validation support or user
adoption. Before recommending the command, verify its build and test coverage,
document supported Kubernetes/DRA versions, and provide a reproducible way to
build or install it from the revision used for validation.

The initial CLI scope, if implemented, is to inspect workloads deployed by the
contributor and produce validation results. It should not require the GKE
provisioning workflow. The documented workflow must also explain how to collect
and evaluate the required evidence with `kubectl` and workload logs, and how to
record results against the same schema. CLI adoption is not an acceptance
criterion, and completing the initial reports must not depend on a broader
`dranetctl` modernization effort.

## Proposed repository structure

The first implementation can use the following structure, while leaving the
existing provider manifests in place:

```text
examples/cross-cloud-validation/
|-- README.md
|-- result-schema.json
`-- results/
    |-- aks-gb300.md
    `-- cks-b200.md

internal/validation/
|-- ... Go collection and validation code with tests
`-- testdata/
    |-- aks/
    `-- cks/
```

The exact directory layout can change during review. The important boundary is
between existing runnable examples, reusable validation logic, an optional CLI
interface, and captured results.

## Expected portability boundary

| Layer | Expected to be portable | Expected to remain provider-specific |
|---|---|---|
| Workload | MPIJob structure, NCCL workload intent, NIC claim pattern | GPU allocation mechanism, container images, and platform tuning |
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

Before the benchmark begins, the workflow verifies that the selected workers
meet the required provider topology constraints. The documented manual checks
and any Go implementation must use the same provider-specific requirements.

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
2. Existing example paths and documented apply commands remain compatible;
   reports identify common workload intent and provider-specific configuration.
3. Preflight reports whether the selected nodes satisfy fabric constraints.
4. Allocation evidence connects a ResourceClaim to the device visible in the
   workload.
5. Runtime evidence identifies the RDMA transport and detects socket fallback.
6. Results conform to a documented machine-readable schema.
7. At least one AKS and one CKS report document the environment, commands,
   observations, limitations, and portability differences.
8. Committed artifacts contain no credentials or sensitive cluster data.
9. Validation logic has tests using AKS and CKS fixtures, including topology
   mismatch, missing evidence, allocation mismatch, and socket fallback cases.
10. The workflow can be completed without installing `dranetctl` or converting
    existing provider examples to Kustomize.

## Proposed implementation sequence

To keep reviews focused, the work should be split into small pull requests:

1. Add the result schema, collection contract, and a manual workflow referencing
   the existing examples. Check that website links and apply commands remain
   valid, and validate sample results against the schema in CI.
2. Add reusable Go collection and validation logic with AKS and CKS fixtures.
   If useful to initial contributors, expose it through a small optional
   `dranetctl validate` command with build/test coverage and installation
   instructions; the command is not a prerequisite for the reports.
3. Add a repeatable CKS B200 result.
4. Add a repeatable AKS GB300 result and the cross-environment comparison.

Later work may add the NIXL KV-cache benchmark, additional clouds, richer
accelerator-to-NIC affinity checks, optional shared manifest composition, or
automated CI with simulated devices. Broader CLI investment should follow
maintainer ownership and demonstrated contributor use.

## Open questions

- Should the initial validation cover `nccl-tests`, the existing
  PyTorch MFU workload, or both?
- Should provider-specific attributes be stored in a free-form result section
  or normalized into an experimental topology vocabulary?
- Which evidence is safe and useful to commit from production-shaped clusters?
- Can the preflight reuse a library shared with DRANET device discovery?
- Who will own the optional `dranetctl validate` interface, its compatibility
  coverage, and distribution, and would the initial contributors use it?
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
