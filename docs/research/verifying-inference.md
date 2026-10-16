# Verifying model inference across workers

A design note for [#303](https://github.com/sisyphus-network/Sisyphus/issues/303), part of [#19](https://github.com/sisyphus-network/Sisyphus/issues/19). It is a proposal to argue with, not a decision. Sources were gathered on 16 October 2026 and are marked as in [the survey](verification-prior-art.md): **read** (the page was fetched and says it), **summary** (seen only in a search summary), **unconfirmed** (recalled or inferred, with no source that states it). Nothing here was tried on our machines.

## Where things stand

`chat`, `prompts`, `embed` and `container` say their result varies, and the coordinator refuses `--verify` of 2 or more, or an `--audit-share`, on them (#301). That is safe, since no honest worker is flagged for an answer that was never going to match. But model work now has no check at all: a worker can return text from a smaller model, a cached answer, or nothing it computed, and be paid in standing the same as an honest one.

One detail of the code matters below. A worker's owner can set model options of their own, and those are applied after the request's (`packages/runtime/ollama.go`), so an owner's `seed` or `temperature` wins over the job's. Any scheme that depends on the job's sampling settings has to either stop that for verified jobs or record what was actually used.

## Why two honest runs differ

- **Sampling.** Above temperature zero, a run draws random numbers. A shared seed fixes them only if everything else is the same.
- **Batch size.** The main cause, per Thinking Machines Lab: common kernels for normalisation, matrix multiplication and attention are not batch-invariant, so the same request computed beside different other requests gives different numbers. In their test, 1,000 completions of one prompt at temperature zero gave 80 different outputs; with batch-invariant kernels, all 1,000 were the same (read: [Defeating nondeterminism in LLM inference](https://thinkingmachines.ai/blog/defeating-nondeterminism-in-llm-inference/)).
- **Hardware and precision.** 16-bit greedy decoding changes with batch size, GPU count and GPU type; computing in 32-bit largely removes it (summary: [Give me FP32 or give me death?](https://arxiv.org/abs/2506.09501)).
- **Software.** Model-server and kernel versions, quantisation, how layers are split between GPU and CPU, context length, and the prompt cache. llama.cpp's server says that with its prompt cache on, which is the default, results are not guaranteed to be identical across batch sizes (read: [llama.cpp server README](https://github.com/ggml-org/llama.cpp/blob/master/tools/server/README.md)).

## The three things #303 asks to keep apart

### 1. Byte-for-byte comparison

This is what `--verify` does today, and it is honest only when the runs can be made the same.

- **vLLM** has a batch-invariant mode, `VLLM_BATCH_INVARIANT=1`, marked beta, on recent NVIDIA, AMD and Intel accelerators. It promises the same result whatever the batch, and makes no promise across different hardware (read: [vLLM batch invariance](https://docs.vllm.ai/en/latest/features/batch_invariance/)).
- **SGLang** has `--enable-deterministic-inference`, on the same kernels, with seeded sampling above temperature zero, at about a third more time (summary: [SGLang docs](https://docs.sglang.io/advanced_features/deterministic_inference.html)).
- **Ollama** takes a `seed` (read: [Ollama API](https://docs.ollama.com/api/generate)). Temperature zero and a fixed seed is the usual recipe for the same answer on the same machine, and it breaks with concurrent requests, a different version, quantisation or layer split (summary). Nothing found from Ollama promises it across machines (unconfirmed).
- **Across different hardware**, the only scheme found that claims bit-for-bit results is Gensyn's RepOps, which fixes the order of every sum: about twice the time of native for an 8-billion-parameter model in 32-bit, on its own runtime (read: [Verde](https://arxiv.org/html/2502.19405); [REE](https://www.gensyn.ai/blog/ree)).

**For Sisyphus:** not for the Ollama workers we have. It could be offered later as an opt-in class of worker: vLLM or SGLang in deterministic mode, one model build, one GPU type, temperature and seed fixed by the job, owner overrides refused. That is BOINC's homogeneous redundancy, and it has BOINC's cost: jobs wait for enough workers of one class. It needs real hardware to measure before it is offered.

### 2. Checking an answer with a second worker

A second worker does not generate again. It feeds the prompt and the claimed answer through the same model in one pass, as a prompt, and checks that the model would plausibly have produced it. That pass is parallel where generation is one token at a time, so it is much cheaper: Prime Intellect reports a median of about 25 times (summary: [SYNTHETIC-2](https://primeintellect.ai/blog/synthetic-2-release)).

- **Token probabilities.** The checker reads the probability of each token of the answer. A low-probability run of tokens suggests a different model or edited text. The threshold has to absorb honest variation, and light quantisation hides inside that margin (summary: [Are you getting what you pay for?](https://arxiv.org/abs/2504.04715)).
- **TOPLOC** has the worker commit, every 32 tokens, to the largest values of the model's last hidden state, in 258 bytes. The checker reruns the one pass and compares within a tolerance. It catches a swapped model, lowered precision, altered weights and a changed system prompt, and is reported robust across A100 and RTX 4090 cards and different attention kernels. It does not by itself catch changed sampling; its second version adds that (read: [TOPLOC](https://arxiv.org/html/2501.16007v2); [SYNTHETIC-2](https://primeintellect.ai/blog/synthetic-2)). It was used in Prime Intellect's INTELLECT-2 training run (summary).
- **SVIP** has the worker return a vector derived from its hidden states, which the requester checks with a small trained head and a secret. It catches a smaller model standing in for a larger one in under 10 ms, but its security is measured, not proved, and needs a trusted party to hand out secrets and a head trained for each model (read: [SVIP](https://arxiv.org/html/2410.22307v2)).
- **A reference answer** is what Bittensor's prompting subnets do: a validator generates its own answer and scores the miner's against it (summary: [Bittensor docs](https://docs.learnbittensor.org/subnets/walkthrough-prompting)). That measures quality, not whether the work was done, and it costs a full generation.

**What these need from a model server.** Probabilities of the tokens in a prompt, or access to hidden states. vLLM gives prompt probabilities (`prompt_logprobs`, summary: [vLLM sampling parameters](https://docs.vllm.ai/en/v0.8.1/api/inference_params.html)). Ollama gives probabilities only for tokens it generates, since v0.12.11 (read: [Ollama API](https://docs.ollama.com/api/generate), [v0.12.11](https://github.com/ollama/ollama/releases/tag/v0.12.11)), and llama.cpp's server likewise (read). **So none of this can be done through Ollama, which is what our workers run.** A checking worker would need vLLM, or llama.cpp used below its server. Hosted APIs are no use for either side, since the checker needs the weights.

**What it can and cannot say.** A checker with the weights catches a different or much smaller model reliably (read: TOPLOC, SVIP). It catches 8-bit for 16-bit only by a small margin, and 4-bit was not found evaluated (read; unconfirmed for 4-bit). It is evidence weighed against a threshold, never proof, and its false-positive rate has to be measured on our own machines before a mismatch costs a worker anything.

### 3. Pinning work to trusted workers

A job can already be kept to a pool's own members and, for private jobs, to workers that hold the key. Choosing a worker you trust is placement and privacy. It is not a check that the answer is right, and should never be presented as one.

Trusted hardware is the version of this that does check. An H100 or newer GPU in confidential-computing mode signs what it runs, and if the attested image pins the model's hash, both substitution and reading the prompt are ruled out, at a reported overhead under a tenth (summary: [NVIDIA attestation](https://docs.nvidia.com/attestation/quick-start-guide/latest/attestation-examples/hopper_single_gpu.html), [Phala's measurements](https://arxiv.org/html/2409.03992v2)). It needs data-centre GPUs, so none of the graphics cards people would lend (unconfirmed, but widely stated), and it trusts NVIDIA and the processor's own enclave.

## What is out of reach

- **Zero-knowledge proofs.** About a quarter of an hour of proving for one 13-billion-parameter inference (summary: [zkLLM](https://arxiv.org/html/2404.16109v1)); SVIP cites 803 seconds a query (read). Orders of magnitude too slow to run on every task.
- **Optimistic fraud proofs** (opML) bisect a disputed run down to one step checked on a chain. They need a chain, a challenge period and deterministic execution (summary: [opML](https://arxiv.org/abs/2401.17555)).
- **Black-box detection** of a substituted model, from text alone, needs many queries, is confused by honest variation, and is beaten by a worker that spots the test and routes it to the real model. The authors of the main study recommend trusted hardware instead (summary: [Are you getting what you pay for?](https://arxiv.org/abs/2504.04715)).

## Privacy

Every scheme that recomputes hands the prompt and the answer to a second worker, which a private job may not want. Byte comparison, the second-worker check and reference answers all do it. Only trusted hardware keeps both from the operator, and zero-knowledge proofs only in part. A private model job checked by a second worker should go only to workers that already hold the job's key, and the job should say so.

## Who is at fault

A mismatch from a tolerance check is not the same as an outvoted exact result. Until honest variation is measured, it should be recorded on the job and in its events, and should not put a worker on probation the way `--verify` does. When the measured false-positive rate is low enough, a repeated pattern, not one mismatch, could count against standing. A worker that declares its model build, quantisation and GPU when it takes a task gives a checker something to check against, and gives an owner override somewhere to show.

## Proposed order

1. **Say what was used.** Record each model task's model, quantisation, model-server version and the sampling options actually applied, owner overrides included, in the job record. Cheap, needs no new backend, and every later step depends on it.
2. **Measure.** On the hardware we have, run the same prompts on Ollama at temperature zero with a seed, and embeddings, and see how often honest runs differ. That decides whether `embed` can be compared within a tolerance (as [verification-next](verification-next.md) proposes) and whether same-machine replay means anything.
3. **A checking worker.** A worker type that runs vLLM or llama.cpp directly and scores a claimed answer in one pass, first as evidence on the job only. TOPLOC is the candidate to evaluate, since it is published, has a package, and was built for workers on mixed GPUs.
4. **A deterministic class**, opt-in, once there are machines of one kind to fill it.
5. **Trusted hardware**, if someone lends a GPU that has it.

Steps 1 and 2 can be done now. Step 3 needs a GPU worker running something other than Ollama.
