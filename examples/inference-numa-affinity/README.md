# Disaggregated prefill/decode on Llama-3.1-70B: GPU↔NIC NUMA matters

A self-contained demo showing why **joint GPU + RDMA NIC allocation with
NUMA awareness** matters for production-scale LLM inference. We run vLLM
in 1-prefill-1-decode (1P1D) disaggregated mode across two GPU nodes, with
the prefill→decode KV cache transfer flowing over RDMA via NIXL/UCX. The
only thing that changes between runs is whether DRA hands the pod 4 GPUs
and 4 NICs **on the same NUMA node**, or 4 GPUs on one NUMA node and 4
NICs on the other so every rail is forced cross-socket.

**TL;DR** — at Llama-3.1-70B scale (~327 KB KV/token), forcing the 4 GPUs
across the inter-socket link from the 4 NICs collapses throughput by 88%
and inflates mean TTFT 100× under concurrency-16 load. Same compute, same
aggregate RDMA bandwidth — only the NUMA placement differs.

## Topology

| Component | Where it runs | Devices it claims via DRA |
|---|---|---|
| `vllm-prefill` | Node A | 4 × H100 (TP=4) + 4 RDMA NICs |
| `vllm-decode`  | Node B | 4 × H100 (TP=4) + 4 RDMA NICs |
| `vllm-router`  | CPU pod | none — forwards prefill→decode and streams response |
| `model-warmer` | DaemonSet on every GPU node | downloads model weights once into hostPath, then idles |

Both vLLM pods load a full BF16 copy of `meta-llama/Llama-3.1-70B-Instruct`
(~140 GB weights, ~35 GB per GPU at TP=4 — fits with plenty of headroom
for KV cache). The prefill pod runs each prompt to first token, NIXL
exports the KV blocks over UCX/RDMA to the decode pod, and the decode pod
streams the rest. Per-token decode work stays node-local; the only
cross-node traffic is the KV cache.

The benchmark uses 28 k input + 256 output tokens (`--max-model-len 32768`,
`--random-input-len 28672`) at concurrency 16, which makes the
prefill→decode KV transfer (~9 GB per request) the dominant TTFT
contributor.

## What this demo proves

Two `ResourceClaimTemplate`s — same compute, same aggregate RDMA bandwidth
(4 GPUs + 4 NICs each way) — only the GPU↔NIC NUMA relationship differs:

| Template | Claims per pod | Effect |
|---|---|---|
| `pd-4gpu-aligned`   | 4 GPUs on NUMA 0 + 4 NICs on NUMA 0 | every GPU has a same-NUMA NIC → GPUDirect RDMA on every rail |
| `pd-4gpu-unaligned` | 4 GPUs on NUMA 1 + 4 NICs on NUMA 0 | every GPU must reach a NIC on the other NUMA → cross-socket DMA on every rail |

A naïve scheduler that picks any free GPUs and any free NICs without joint
NUMA awareness lands you somewhere on the spectrum between these two.
dranet lets you express either configuration declaratively in one
ResourceClaim.

## Why this can't be expressed with `matchAttribute`

The cleanest way to express "GPU and NIC on the same NUMA node" would be a
single DRA `matchAttribute` constraint joining the GPU and NIC requests on
a shared `numaNode` attribute. That doesn't work today:

- The NVIDIA k8s-dra-driver-gpu publishes `pciBusID` per GPU but **not**
  `numaNode`. On Azure ND H100 v5 it can't even publish `pcieRoot` — each
  GPU sits behind a Hyper-V VMBus path that the driver's resolver
  rejects. So the GPU side of the join has no NUMA attribute to match on.
- dranet publishes `dra.net/numaNode` for NICs (it reads sysfs directly).

Until GPU drivers publish a standard `numaNode` attribute, the GPU side
has to be pinned by `pciBusID` (a list of GPUs you've separately
verified are on the desired NUMA node), while the NIC side carries an
explicit `dra.net/numaNode == 0/1` selector. That's what both RCTs in
`resource-claim-template.yaml` do.

## Layout

| File | Purpose |
|---|---|
| `resource-claim-template.yaml` | Two RCTs (`pd-4gpu-aligned`, `pd-4gpu-unaligned`) |
| `vllm-prefill-decode.yaml` | model-warmer DaemonSet + prefill + decode + router pods, services, ConfigMap with proxy script |
| `benchmark-job.yaml` | Job that runs `vllm bench serve` against the router and prints a TTFT / TPOT / ITL summary |

## Cluster prereqs

- Two GPU nodes that each expose 8 GPUs via `gpu.nvidia.com` and 8 RDMA
  NICs (4 per NUMA node) via `dra.net`. The `dra.net` DeviceClass must
  exist; if your dranet install didn't create it, apply the one shipped
  with the repo:

  ```bash
  kubectl apply -f tests/manifests/deviceclass.yaml
  ```

  Verify with:

  ```bash
  kubectl get resourceslice -o yaml | yq '.items[].spec.devices[]
    | select(.attributes."dra.net/rdma".bool == true)
    | {name, rdmaDevice: .attributes."dra.net/rdmaDevice".string,
       numa: .attributes."dra.net/numaNode".int}'
  ```

- A HuggingFace token with access to `meta-llama/Llama-3.1-70B-Instruct`
  (the model is gated; accept the license on the HF model page first):

  ```bash
  kubectl create secret generic hf-token --from-literal=token="$HF_TOKEN"
  ```

- A node label that identifies your GPU nodes. The model-warmer DaemonSet
  uses `nvidia.com/gpu.present: "true"` (set by NVIDIA NFD). Adjust the
  selector in `vllm-prefill-decode.yaml` if your cluster uses a different
  label (e.g. `agentpool=gpu` on AKS, `cloud.google.com/gke-accelerator`
  on GKE).

- The model is cached on each GPU node's local disk via a hostPath
  (`/var/lib/models`). The DaemonSet downloads ~140 GB once per node, so
  each node needs ~150 GB free under that path. No CSI driver required.

## Adapting the templates to your hardware

`resource-claim-template.yaml` pins GPUs by `pciBusID`. The default values
match Azure ND H100 v5 (NUMA 0 = `0001/0002/0003/0008:00:00.0`,
NUMA 1 = `0009/000a/000b/000c:00:00.0`). On a different SKU, list the
GPUs and their NUMA mapping first, then update the two CEL expressions:

```bash
# 1) Get GPU pciBusIDs
kubectl get resourceslice -o yaml | yq '.items[].spec.devices[]
  | select(.attributes."resource.kubernetes.io/pciBusID")
  | {name, pciBusID: .attributes."resource.kubernetes.io/pciBusID".string}'

# 2) Map GPU pciBusID -> NUMA node by reading /sys on the node
#    (the NVIDIA DRA driver doesn't expose numaNode):
kubectl debug node/<gpu-node> -it --image=busybox:1.36 -- \
  sh -c 'for d in 0001 0002 0003 0008 0009 000a 000b 000c; do \
    echo $d numa=$(cat /host/sys/bus/pci/devices/$d:00:00.0/numa_node); \
  done'
```

Pick four pciBusIDs on each NUMA and update the `device.attributes[
"resource.kubernetes.io"]["pciBusID"] in [...]` list in both templates.

## Run

```bash
# 1. One-time: apply both ResourceClaimTemplates.
kubectl apply -f resource-claim-template.yaml

# 2. For each test case, bring up the stack, benchmark, capture, tear down.
for tpl in pd-4gpu-aligned pd-4gpu-unaligned; do
  echo "=== $tpl ==="

  # Point both prefill and decode pods at the chosen template.
  sed "s/resourceClaimTemplateName:.*/resourceClaimTemplateName: ${tpl}/" \
    vllm-prefill-decode.yaml | kubectl apply -f -

  # Wait for everything to come up. The very first run downloads ~140 GB
  # of weights into the per-node hostPath via the model-warmer DaemonSet
  # (15-30 min depending on link). Subsequent iterations only restart the
  # vllm pods, which boot in 2-3 min from the cached weights.
  kubectl rollout status daemonset/model-warmer --timeout=60m
  kubectl wait --for=condition=ready pod/vllm-prefill pod/vllm-decode pod/vllm-router --timeout=15m

  # Run the benchmark and capture results.
  kubectl apply -f benchmark-job.yaml
  kubectl wait --for=condition=complete job/vllm-bench --timeout=45m
  kubectl logs job/vllm-bench > "results-${tpl}.txt"
  kubectl delete -f benchmark-job.yaml --wait=true

  # Tear down vllm pods. The model-warmer DS keeps the cached weights on
  # the hostPath so the next iteration boots in a couple of minutes.
  sed "s/resourceClaimTemplateName:.*/resourceClaimTemplateName: ${tpl}/" \
    vllm-prefill-decode.yaml | kubectl delete -f - --ignore-not-found
done
```

## Verifying DRA actually allocated what you asked for

```bash
# Inspect the resolved ResourceClaim — devices and which node they came from.
kubectl get resourceclaim -o yaml | yq '.items[]
  | select(.metadata.name | test("vllm-(prefill|decode)"))
  | {pod: .metadata.name,
     devices: [.status.allocation.devices.results[].device]}'

# Confirm 4 RDMA devices visible inside the pod.
kubectl exec vllm-prefill -- ls /dev/infiniband
```

Each pod's allocation should list **4 GPUs and 4 NIC `pci-*` names**, all
from the same node:

- `pd-4gpu-aligned`: `gpu-0..3` + `pci-0101..0104` (every device on NUMA 0)
- `pd-4gpu-unaligned`: `gpu-4..7` + `pci-0101..0104` (GPUs on NUMA 1, NICs on NUMA 0)

## Verifying the KV transport

NIXL/UCX should pick RDMA over IB on every rail in the aligned case:

```bash
kubectl logs vllm-prefill | grep -E "NIXL|UCX|GDR|IB"
kubectl logs vllm-decode  | grep -E "NIXL|KV Transfer metrics"
```

In `pd-4gpu-aligned`, expect every UCX worker thread to bind an `mlx5_*`
device on the same NUMA as its GPU and saturate the rail. In
`pd-4gpu-unaligned`, every worker drives a cross-NUMA NIC and the
per-transfer cost climbs from microseconds to hundreds of milliseconds —
visible in the decode pod's `KV Transfer metrics: ... Avg xfer time (ms)=`
log line.

## Interpreting the results

- **TTFT** = prompt prefill on the prefill pod **plus** the prefill→decode
  KV transfer over RDMA. The demo uses 28 k input tokens to push the
  per-request KV transfer to ~9 GB on Llama-3.1-70B, so the cross-NUMA
  cost (when present) dominates TTFT. At higher concurrency the per-NIC
  transfer queue grows and the gap blows up nonlinearly.
- **TPOT** = steady-state per-token decode latency, all node-local. Not
  sensitive to NIC placement.
- **Output throughput** = end-to-end requests per second × tokens per
  request. Tracks the prefill pod's ability to hand off KV to decode
  fast enough to keep its slots free. When that hand-off slows down,
  prefill blocks new requests and throughput collapses.

### Observed numbers (Azure ND H100 v5, 2 nodes, TP=4, concurrency 16, 200 reqs)

`meta-llama/Llama-3.1-70B-Instruct`, 28 k input + 256 output tokens, OpenAI
`/v1/completions` endpoint:

| Metric | `pd-4gpu-aligned` (NUMA 0 + NUMA 0) | `pd-4gpu-unaligned` (NUMA 1 + NUMA 0) | Δ |
|---|---:|---:|---:|
| Successful / Failed | 200 / 0 | 199 / 1 | — |
| **Mean TTFT** | **326 ms** | **32 605 ms** | **+10 000% (≈100×)** |
| Median TTFT | 247 ms | 33 525 ms | ≈135× |
| P99 TTFT | 1 030 ms | 36 094 ms | ≈35× |
| Mean TPOT | 16.64 ms | 17.48 ms | +5% |
| Output throughput | 858 tok/s | 106 tok/s | **−88%** |
| Total throughput | 96 934 tok/s | 11 976 tok/s | **−88%** |
| Benchmark wall clock | 60 s | 481 s | 8× longer |

The decode pod's NIXL metrics show why: in the unaligned config the
average prefill→decode KV transfer takes ~231 ms per request at
~9.7 GB/s. With 16 concurrent requests in flight on 4 NICs, those
hundreds-of-milliseconds transfers serialise behind each other and the
TTFT inflates from a few hundred ms to tens of seconds.

In the aligned config the transfers complete fast enough to never queue,
TPOT stays unchanged (decode work is local), and the headline TTFT and
throughput numbers stay flat as concurrency rises.

### When the signal will be smaller

The size of the gap depends on whether the KV transfer cost approaches or
exceeds the prefill compute cost:

- **Smaller models hide it.** We separately measured
  `meta-llama/Llama-3.1-8B-Instruct` at the same TP=4 / concurrency 16 /
  28 k-input setup. Mean TTFT differences were within run-to-run noise
  (Δ ≈ ±1%). Llama-8B has ~262 KB KV/token (~2.5× smaller than 70B), so
  per-request KV transfer (~7 GB) finishes well under the prefill compute
  time and the cross-NUMA penalty disappears in the noise.
- **Shorter prompts hide it.** At 4 k input tokens the per-request KV
  drops to ~1.3 GB on 70B and the relative cost shrinks. Move to longer
  contexts (32 k+) to surface the effect.
- **Lower concurrency hides it.** At concurrency 1-2 there's no queuing,
  so even a slow KV transfer just adds a constant offset rather than
  blowing up the tail.

## Notes / caveats

- **1P1D is the simplest disaggregated topology.** Production deployments
  use ratios closer to 1P:3D for chat workloads. The same RCTs work; just
  scale the prefill / decode pods and update the router's
  `--prefiller-host` / `--decoder-host` lists.
- **Why TP=4 and not TP=8?** TP=4 is the largest tensor-parallelism size
  that lets us pin all 4 ranks to a single NUMA node on a 4-NUMA-0,
  4-NUMA-1 GPU layout, which is the only way to make the cross-NUMA
  comparison clean. TP=8 would force each pod to span both NUMA nodes
  and confound the alignment effect with the natural 4+4 layout.
- **NixlConnector limitation: uniform attention only.** vLLM's
  `NixlConnector.register_kv_caches` asserts all KV cache tensors are the
  same size per rank. Models with mixed sliding-window + global attention
  (e.g. Gemma 4 with `head_dim=256` sliding-window and `head_dim=512`
  global layers) fail this assertion at TP=4. Llama-3.1-70B uses uniform
  GQA across all layers, so it works.
- **`kv_role` is currently a placeholder for `NixlConnector`.** Per the
  upstream guide, the actual prefill/decode split is decided by the
  proxy, not by the connector config. We pass `kv_role: kv_both` on both
  vllm pods.
- **The router pod is a toy proxy** adapted from
  `vllm/tests/v1/kv_connector/nixl_integration/toy_proxy_server.py`,
  inlined into the ConfigMap so the example is self-contained. Two minor
  fixes vs. the upstream version: (1) don't forward `null` for
  `min_tokens` / `min_completion_tokens` (vLLM rejects them with 400),
  and (2) forward the *original* request body (not the prefill-modified
  one with `max_tokens=1`) to decode. Use NVIDIA Dynamo or a hardened
  router for production.
- **Model download via DaemonSet.** `huggingface_hub.snapshot_download` is
  used with `ignore_patterns=["original/*"]` to skip the duplicate Meta
  PyTorch checkpoints that ship in the Llama-3.1 repo (`original/`
  contains a redundant ~140 GB of `.pth` files vLLM doesn't need).
- **HF Xet flakiness.** During testing HF's Xet chunked-storage endpoint
  intermittently returned 500s for Gemma 4 weights. If you hit it, set
  `HF_HUB_DISABLE_XET=1` (already set on the model-warmer DS) to fall
  back to the legacy CDN — slower but stable.
