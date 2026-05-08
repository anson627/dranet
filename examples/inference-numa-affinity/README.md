# Disaggregated prefill/decode on Gemma-4-31B-it: NIC selection vs. TTFT

A self-contained demo showing why **joint GPU + RDMA NIC allocation** matters
for production-scale LLM inference. We run vLLM in 1-prefill-1-decode (1P1D)
disaggregated mode across two GPU nodes, with the prefill→decode KV cache
transfer flowing over RDMA. The only thing we change between runs is the set
of NICs DRA hands the pods.

## Topology

| Component | Where it runs | Devices it claims via DRA |
|---|---|---|
| `vllm-prefill` | Node A | 8 × H100 (TP=8) + N RDMA NICs |
| `vllm-decode`  | Node B | 8 × H100 (TP=8) + N RDMA NICs |
| `vllm-router`  | CPU pod | none — forwards prefill→decode and streams response |

Both vLLM pods load a full BF16 copy of `google/gemma-4-31B-it` (~62 GB
weights). At TP=8 the per-GPU weight footprint is tiny (~8 GB) — that's
intentional: TP=8 ensures every rank owns a slice of the KV cache and ends
up using a dedicated RDMA rail, which is what surfaces the NIC-NUMA signal
in TTFT. The prefill pod runs each prompt to first token, NIXL exports the
KV blocks over UCX/RDMA to the decode pod, and the decode pod streams the
rest. Per-token decode work stays node-local; the only cross-node traffic
is the KV cache.

To make that KV transfer big enough to dominate TTFT on a 31B model, the
benchmark uses 32 k-token prompts (`--max-model-len 32768`,
`--random-input-len 32768`). Shorter prompts will shrink the gap between
the two test cases.

## What this demo proves

Two `ResourceClaimTemplate`s, identical workload, two sets of numbers:

| Template | Claims per pod | Effect on KV transfer |
|---|---|---|
| `pd-aligned` | 8 GPUs + 8 NICs (4 NUMA-0 + 4 NUMA-1, balanced) | every GPU has a same-NUMA NIC → GPUDirect RDMA on every rail, full fabric BW |
| `pd-half-bandwidth` | 8 GPUs + 4 NICs (NUMA-0 only) | half the rails; 4 GPUs on NUMA-1 must reach NICs on NUMA-0, cross-socket DMA |

A naïve scheduler that picks any free NICs without joint GPU/NIC NUMA
awareness behaves like `pd-half-bandwidth`. dranet lets you express
`pd-aligned` as a single declarative claim.

## Why this can't be done with `matchAttribute`

The NVIDIA GPU DRA driver publishes `pciBusID` per GPU but **not** `numaNode`
(and on Azure ND H100 v5 it can't even publish `pcieRoot` — the GPU sysfs path
goes through a Hyper-V VMBus root, not a PCI root complex, and the driver's
resolver bails). dranet publishes `dra.net/numaNode` for NICs. There is no
shared cross-driver attribute that expresses NUMA membership for both sides,
so the GPU side of the claim is unconstrained (`count: 8`, take all GPUs from
the node) while the NIC side carries explicit `dra.net/numaNode == 0/1`
selectors. Once GPU drivers publish a standard `numaNode`, this would collapse
to a single `matchAttribute` join.

## Layout

| File | Purpose |
|---|---|
| `resource-claim-template.yaml` | Two RCTs (`pd-aligned`, `pd-half-bandwidth`) |
| `vllm-prefill-decode.yaml` | Prefill + decode + router pods, services, PVCs, ConfigMap with proxy script |
| `benchmark-job.yaml` | Job that runs `vllm bench serve` and prints TTFT / TPOT / ITL summary |

## Cluster prereqs

- Two GPU nodes that each expose 8 GPUs via `gpu.nvidia.com` and 8 RDMA NICs
  (4 per NUMA node) via `dra.net`. The `dra.net` DeviceClass must exist; if
  your dranet install didn't create it, apply the one shipped with the repo:

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

- A HuggingFace token. `google/gemma-4-31B-it` is Apache 2.0, but HF still
  requires you to accept Google's usage notice on the model page once
  (instant approval, no waiting list).
- The model cache is an `emptyDir` per pod (re-downloads ~62 GB on each
  pod create). If your cluster has a CSI driver, swap the two `hf-cache`
  volumes in `vllm-prefill-decode.yaml` for PVCs to cache across runs.

  ```bash
  kubectl create secret generic hf-token --from-literal=token="$HF_TOKEN"
  ```

## Run

```bash
# 1. Apply the templates (both RCTs are created together)
kubectl apply -f resource-claim-template.yaml

# 2. For each test case: pd-aligned, then pd-half-bandwidth
for tpl in pd-aligned pd-half-bandwidth; do
  echo "=== $tpl ==="

  # Point both prefill and decode pods at the chosen template.
  sed "s/resourceClaimTemplateName:.*/resourceClaimTemplateName: ${tpl}/" \
    vllm-prefill-decode.yaml | kubectl apply -f -

  # Wait until both vLLM servers report ready (first run downloads ~62 GB
  # of weights, so the first iteration here usually takes 5-15 min depending
  # on your link; subsequent iterations boot from the PVC cache).
  kubectl wait --for=condition=ready pod/vllm-prefill pod/vllm-decode pod/vllm-router --timeout=60m
  until curl -sf http://$(kubectl get pod vllm-router -o jsonpath='{.status.podIP}'):8000/v1/models > /dev/null; do
    sleep 10
  done

  # Run the benchmark and capture results.
  kubectl apply -f benchmark-job.yaml
  kubectl wait --for=condition=complete job/vllm-bench --timeout=45m
  kubectl logs job/vllm-bench > "results-${tpl}.txt"
  kubectl delete -f benchmark-job.yaml --wait=true

  # Tear down the vLLM stack but keep the cached weights on the PVCs so the
  # next iteration boots in a few minutes instead of redownloading 405 GB.
  sed "s/resourceClaimTemplateName:.*/resourceClaimTemplateName: ${tpl}/" \
    vllm-prefill-decode.yaml | kubectl delete -f -
done
```

## Verifying that DRA actually allocated what you asked for

```bash
# Confirm the prefill pod sees N RDMA devices
kubectl exec vllm-prefill -- ibv_devices
kubectl exec vllm-prefill -- ls /dev/infiniband

# Inspect the resolved ResourceClaim — devices and which node they came from
kubectl get resourceclaim -o yaml | yq '.items[]
  | select(.metadata.name | test("vllm-(prefill|decode)"))
  | {pod: .metadata.name, node: .status.allocation.nodeSelector,
     devices: [.status.allocation.devices.results[].device]}'
```

Each pod's allocation should list 8 GPUs and (4 + 4 = 8) or (4) NIC device
names depending on the template, all from the same node.

## Verifying the KV transport

NIXL/UCX should pick RDMA over IB on every rail in the aligned case:

```bash
kubectl logs vllm-prefill | grep -E "NIXL|UCX|GDR|IB"
kubectl logs vllm-decode  | grep -E "NIXL|UCX|GDR|IB"
```

In `pd-aligned`, expect every UCX worker thread to bind a `mlx5_*` device on
the same NUMA as its GPU. In `pd-half-bandwidth`, expect 4 of the 8 workers
to bind cross-NUMA NICs and the per-rail saturation to drop accordingly.

## Interpreting the results

- **TTFT** is dominated by prompt prefill on the prefill pod **plus** the
  prefill→decode KV transfer over RDMA. Bigger prompts make the transfer
  fraction larger; the demo uses 32 k input tokens specifically because
  31B is a small enough model that shorter prompts would put the transfer
  cost in the noise.
- **TPOT** is steady-state per-token latency on the decode pod, all local —
  not very sensitive to NIC placement.
- **Output throughput** is gated by both, but at modest concurrency
  reflects how much of the fabric the prefill pod can keep saturated.

A measurable TTFT regression in `pd-half-bandwidth` (typically 30-80%
depending on prompt length and link speed) is the dranet value proposition
made visible to the application owner.

## Notes / caveats

- 1P1D is the simplest disaggregated topology. Production deployments use
  ratios closer to 1P:3D for chat workloads; the same RCTs work, just scale
  pods up.
- vLLM's `kv_role` is currently a placeholder for `NixlConnector` — the
  actual prefill/decode roles are determined by the proxy. We pass
  `kv_role: kv_both` per the upstream guide.
- The router pod is the toy proxy from
  `vllm/tests/v1/kv_connector/nixl_integration/toy_proxy_server.py`,
  inlined into the ConfigMap so the example is self-contained. It is
  fine for benchmarking; for production use NVIDIA Dynamo or a hardened
  router that handles failover and connection limits.
