# Disaggregated Prefill/Decode on Gemma 4B With NUMA-Aware DRA

A self-contained demo for validating **joint GPU + RDMA NIC allocation with
NUMA awareness** for vLLM disaggregated prefill/decode. The stack runs vLLM
in 1-prefill-1-decode (1P1D) mode across two GPU nodes, with prefill-to-decode
KV cache transfer over RDMA through NIXL/UCX.

The only thing that changes between runs is whether DRA hands each pod 4 GPUs
and 4 NICs on the same NUMA node, or 4 GPUs on one NUMA node and 4 NICs on the
other NUMA node so every rail is cross-socket.

This example now uses `google/gemma-3-4b-it` by default. Gemma 4B reduces
prefill latency enough to make the handoff path easier to study, while still
using long prompts to generate substantial KV transfers.

## Topology

| Component | Where it runs | Devices it claims via DRA |
|---|---|---|
| `vllm-prefill` | Node A | 4 x H100 (TP=4) + 4 RDMA NICs |
| `vllm-decode` | Node B | 4 x H100 (TP=4) + 4 RDMA NICs |
| `vllm-router` | CPU pod | none; forwards prefill to decode and streams response |
| `model-warmer` | DaemonSet on every GPU node | downloads model weights once into hostPath, then idles |

Both vLLM pods load a full BF16 copy of `google/gemma-3-4b-it`. The prefill
pod runs each prompt to first token, NIXL exports KV blocks over UCX/RDMA to
the decode pod, and the decode pod streams the rest. Per-token decode work
stays node-local; the only cross-node traffic is the KV cache.

The benchmark uses 28 k input + 64 output tokens (`--random-input-len 28672`,
`--random-output-len 64`) at concurrency 32. The long input stresses the KV
handoff path; the short output keeps decode work from dominating the result.

## What This Tests

Two `ResourceClaimTemplate`s keep compute and aggregate RDMA bandwidth fixed.
Only the GPU-to-NIC NUMA relationship differs:

| Template | Claims per pod | Effect |
|---|---|---|
| `pd-4gpu-aligned` | 4 GPUs on NUMA 0 + 4 NICs on NUMA 0 | every GPU has a same-NUMA NIC |
| `pd-4gpu-unaligned` | 4 GPUs on NUMA 1 + 4 NICs on NUMA 0 | every GPU reaches NICs across the socket boundary |

A scheduler that picks any free GPUs and any free NICs without joint NUMA
awareness can land anywhere on this spectrum. dranet lets you express the
device relationship declaratively in one ResourceClaim.

## Why This Can't Be Expressed With `matchAttribute`

The cleanest way to express "GPU and NIC on the same NUMA node" would be a
single DRA `matchAttribute` constraint joining GPU and NIC requests on a shared
`numaNode` attribute. That does not work today:

- The NVIDIA k8s-dra-driver-gpu publishes `pciBusID` per GPU but not `numaNode`.
- On Azure ND H100 v5, each GPU sits behind a Hyper-V VMBus path that prevents
  deriving a useful `pcieRoot` attribute.
- dranet publishes `dra.net/numaNode` for NICs by reading sysfs directly.

Until GPU drivers publish a standard `numaNode` attribute, the GPU side has to
be pinned by `pciBusID`, while the NIC side carries an explicit
`dra.net/numaNode == 0/1` selector. That is what both templates in
`resource-claim-template.yaml` do.

## Layout

| File | Purpose |
|---|---|
| `resource-claim-template.yaml` | Two RCTs: `pd-4gpu-aligned`, `pd-4gpu-unaligned` |
| `vllm-prefill-decode.yaml` | model-warmer DaemonSet, prefill pod, decode pod, router pod, services, proxy ConfigMap |
| `benchmark-job.yaml` | Job that runs `vllm bench serve` and prints TTFT / TPOT / ITL / throughput |

## Cluster Prereqs

- Two GPU nodes that each expose 8 GPUs via `gpu.nvidia.com` and 8 RDMA NICs
  via `dra.net`, with 4 NICs per NUMA node.
- The `dra.net` DeviceClass. If your dranet install did not create it, apply
  the one shipped with the repo:

```bash
kubectl apply -f tests/manifests/deviceclass.yaml
```

Verify NIC resource publication:

```bash
kubectl get resourceslice -o yaml | yq '.items[].spec.devices[]
  | select(.attributes."dra.net/rdma".bool == true)
  | {name, rdmaDevice: .attributes."dra.net/rdmaDevice".string,
     numa: .attributes."dra.net/numaNode".int}'
```

- A HuggingFace token with access to `google/gemma-3-4b-it`:

```bash
kubectl create secret generic hf-token --from-literal=token="$HF_TOKEN"
```

- A node label that identifies GPU nodes. The model-warmer DaemonSet uses
  `nvidia.com/gpu.present: "true"` from NVIDIA NFD. Adjust the selector in
  `vllm-prefill-decode.yaml` if your cluster uses a different label.

- Local disk on each GPU node under `/var/lib/models`. Gemma 4B is much smaller
  than 70B-class models, but the hostPath still needs enough space for the HF
  snapshot and tokenizer files.

## Adapting The Templates To Your Hardware

`resource-claim-template.yaml` pins GPUs by `pciBusID`. The default values
match Azure ND H100 v5:

| NUMA node | GPU pciBusIDs | NIC selector |
|---|---|---|
| NUMA 0 | `0001/0002/0003/0008:00:00.0` | `dra.net/numaNode == 0` |
| NUMA 1 | `0009/000a/000b/000c:00:00.0` | `dra.net/numaNode == 1` |

On a different SKU, list GPUs and their NUMA mapping first, then update the
two CEL expressions in `resource-claim-template.yaml`:

```bash
# Get GPU pciBusIDs.
kubectl get resourceslice -o yaml | yq '.items[].spec.devices[]
  | select(.attributes."resource.kubernetes.io/pciBusID")
  | {name, pciBusID: .attributes."resource.kubernetes.io/pciBusID".string}'

# Map GPU pciBusID to NUMA node by reading sysfs on the node.
kubectl debug node/<gpu-node> -it --image=busybox:1.36 -- \
  sh -c 'for d in 0001 0002 0003 0008 0009 000a 000b 000c; do \
    echo $d numa=$(cat /host/sys/bus/pci/devices/$d:00:00.0/numa_node); \
  done'
```

## Run

```bash
# 1. One-time: apply both ResourceClaimTemplates.
kubectl apply -f resource-claim-template.yaml

# 2. For each test case, bring up the stack, benchmark, capture, tear down.
for tpl in pd-4gpu-aligned pd-4gpu-unaligned; do
  echo "=== $tpl ==="

  sed "s/resourceClaimTemplateName:.*/resourceClaimTemplateName: ${tpl}/" \
    vllm-prefill-decode.yaml | kubectl apply -f -

  kubectl rollout status daemonset/model-warmer --timeout=30m
  kubectl wait --for=condition=ready pod/vllm-prefill pod/vllm-decode pod/vllm-router --timeout=15m

  # Pod readiness can be true before vLLM has finished model load/compile.
  until kubectl exec vllm-router -- python -c "import urllib.request as u; u.urlopen('http://127.0.0.1:8000/v1/models', timeout=5)"; do
    echo "waiting for router HTTP readiness..."
    sleep 10
  done

  kubectl apply -f benchmark-job.yaml
  kubectl wait --for=condition=complete job/vllm-bench --timeout=45m
  kubectl logs job/vllm-bench > "results-${tpl}.txt"
  kubectl delete -f benchmark-job.yaml --wait=true

  # Tear down vLLM pods. The model-warmer DS keeps cached weights on hostPath.
  sed "s/resourceClaimTemplateName:.*/resourceClaimTemplateName: ${tpl}/" \
    vllm-prefill-decode.yaml | kubectl delete -f - --ignore-not-found
done
```

## Verify DRA Allocation

Inspect resolved ResourceClaims:

```bash
kubectl get resourceclaim -o yaml | yq '.items[]
  | select(.metadata.name | test("vllm-(prefill|decode)"))
  | {pod: .metadata.name,
     devices: [.status.allocation.devices.results[].device]}'
```

Confirm the RDMA devices are visible inside a vLLM pod:

```bash
kubectl exec vllm-prefill -- ls /dev/infiniband
```

Each pod's allocation should list 4 GPUs and 4 NIC `pci-*` names from the same
node:

- `pd-4gpu-aligned`: `gpu-0..3` + `pci-0101..0104` on NUMA 0.
- `pd-4gpu-unaligned`: `gpu-4..7` on NUMA 1 + `pci-0101..0104` on NUMA 0.

## Verify KV Transport

```bash
kubectl logs vllm-prefill | grep -E "NIXL|UCX|GDR|IB"
kubectl logs vllm-decode  | grep -E "NIXL|KV Transfer metrics"
```

The decode pod emits `KV Transfer metrics` lines with transfer count, average
transfer time, transfer size, throughput, and descriptor count. For this Gemma
load shape, the transfer size observed on Azure ND H100 v5 was about 952 MB per
request.

## Interpreting Results

- **TTFT** includes prompt prefill, prefill-to-decode KV transfer, and any
  queueing in either path.
- **TPOT** is steady-state per-token decode latency and is mostly node-local.
- **Output throughput** is end-to-end request throughput multiplied by output
  tokens per request.
- A smaller model can reduce prefill latency, but it also reduces KV bytes per
  request. If KV transfer is still not the bottleneck, aligned and unaligned
  benchmark-level TTFT can remain close even when transport logs show transfer
  differences.

## Observed Numbers

Azure ND H100 v5, 2 GPU nodes, TP=4, `google/gemma-3-4b-it`, 28 k input + 64
output tokens, OpenAI `/v1/completions` endpoint.

Concurrency 32, 300 prompts:

| Metric | `pd-4gpu-aligned` | `pd-4gpu-unaligned` |
|---|---:|---:|
| Successful / Failed | 300 / 0 | 300 / 0 |
| Mean TTFT | 7673.76 ms | 7500.86 ms |
| Median TTFT | 7426.04 ms | 7320.67 ms |
| P99 TTFT | 13863.42 ms | 14698.24 ms |
| Mean TPOT | 6.04 ms | 7.30 ms |
| Mean ITL | 7.19 ms | 9.01 ms |
| Request throughput | 3.78 req/s | 3.83 req/s |
| Output throughput | 242.23 tok/s | 245.14 tok/s |
| Benchmark wall clock | 79.26 s | 78.32 s |

Concurrency 64, 600 prompts:

| Metric | `pd-4gpu-aligned` | `pd-4gpu-unaligned` |
|---|---:|---:|
| Successful / Failed | 600 / 0 | 600 / 0 |
| Mean TTFT | 14541.39 ms | 14479.99 ms |
| Median TTFT | 15023.59 ms | 14969.59 ms |
| P99 TTFT | 16748.04 ms | 19907.17 ms |
| Mean TPOT | 5.79 ms | 6.49 ms |
| Request throughput | 4.07 req/s | 4.08 req/s |
| Output throughput | 260.69 tok/s | 261.00 tok/s |

Higher concurrency runs:

| Metric | Concurrency 96 aligned | Concurrency 96 unaligned | Concurrency 128 aligned | Concurrency 128 unaligned |
|---|---:|---:|---:|---:|
| Successful / Failed | 900 / 0 | 900 / 0 | 1198 / 2 | 1200 / 0 |
| Mean TTFT | 21845.42 ms | 21705.32 ms | 29518.23 ms | 29111.85 ms |
| Median TTFT | 22700.17 ms | 22612.50 ms | 30767.16 ms | 30387.72 ms |
| P99 TTFT | 23928.53 ms | 24171.57 ms | 31542.06 ms | 31023.78 ms |
| Mean TPOT | 5.73 ms | 6.42 ms | 5.71 ms | 6.17 ms |
| Request throughput | 4.10 req/s | 4.12 req/s | 4.06 req/s | 4.11 req/s |
| Output throughput | 262.25 tok/s | 263.40 tok/s | 259.77 tok/s | 263.11 tok/s |

In these runs, switching from Llama-70B-scale weights to Gemma 4B reduced TTFT
substantially, but the end-to-end aligned-vs-unaligned gap did not show up in
benchmark throughput or mean TTFT even up to concurrency 128. The decode logs
still showed transport-level signal: aligned steady-state transfers settled
around 90-92 ms for about 952 MB, while unaligned settled around 160-170 ms.
Both placements had early queueing windows with second-scale transfer times,
and concurrency 128 started to hit reliability limits in the toy proxy path.

If you need the NUMA gap to dominate end-to-end metrics, increase KV bytes per
request or queueing pressure without letting prefill dominate everything:

- Increase input length while staying within `--max-model-len`.
- Increase concurrency beyond 32 if the model and KV cache capacity allow it.
- Keep output length short so decode work does not hide the handoff path.
- Compare decode pod `KV Transfer metrics` in addition to benchmark TTFT.

## Notes

- **1P1D is the simplest topology.** Production deployments often use multiple
  decode pods per prefill pod. The same RCTs work; scale the pods and update
  the router's `--prefiller-hosts` and `--decoder-hosts` lists.
- **Why TP=4 and not TP=8?** TP=4 is the largest tensor-parallelism size that
  lets this Azure ND H100 v5 layout pin all ranks to one NUMA node. TP=8 would
  span both NUMA nodes and confound the comparison.
- **Gemma uses hybrid attention.** vLLM logs that the hybrid KV cache manager
  is disabled when `--kv-transfer-config` is set. This is expected for this
  example and keeps NIXL enabled for disaggregated serving.
- **`kv_role` is currently a placeholder for `NixlConnector`.** The actual
  prefill/decode split is decided by the proxy, not by the connector config.
  Both vLLM pods pass `kv_role: kv_both`.
- **The router pod is a toy proxy** adapted from vLLM's NIXL integration tests.
  Use NVIDIA Dynamo or a hardened router for production.
- **HF Xet flakiness.** The model-warmer sets `HF_HUB_DISABLE_XET=1` to avoid
  intermittent Xet endpoint failures observed during Gemma weight downloads.
