# How other networks check untrusted workers

A survey for [#19](https://github.com/sisyphus-network/Sisyphus/issues/19), gathered on 10 October 2026. It asks one question of each system: when a stranger's machine returns a result, how does anyone know it is right?

**How to read it.** Each claim was checked against a source at one of three levels, and says which:

- **read**: the page was fetched and the claim is in it;
- **summary**: the claim was seen only in a search summary of the source, which was not opened;
- **unconfirmed**: recalled, or inferred, with no source that states it.

Nothing here was tried. Where a system changed over time the version is named.

## What Sisyphus has already

Replication with a quorum (`--verify N`, #153), spot checks of a random share of tasks with rechecks for a worker caught out (`--verify-share`, #155), a standing for each worker that puts it on probation after being outvoted (#159), and the same for private jobs (#156). All of it compares results for exact equality, and none of it has stakes.

## The systems

### BOINC (2002 to now)

Volunteer scientific computing; each project runs its own server.

- **Replication with a quorum.** A job is sent to several hosts; once `min_quorum` successful results are in, a validator the application supplies decides whether they agree, and one becomes canonical (read: [JobReplication](https://github.com/BOINC/boinc/wiki/JobReplication)).
- **Adaptive replication.** The server counts consecutive valid results for each host and application version. Below 10 a host is never trusted. At N or more it is trusted with probability 1 − 1/N and sent unreplicated jobs; one invalid result resets it to zero. The stated aim is roughly 5 to 10 per cent overhead in place of 50 per cent or more (read: [Adaptive-Replication](https://github.com/BOINC/boinc/wiki/Adaptive-Replication)).
- **Non-determinism.** Homogeneous redundancy sorts hosts into "numerical equivalence classes" by operating system and processor and sends all copies of a job to one class, so that exact comparison works. Its costs are named: jobs stranded for want of hosts in a class, and classes tuned by hand (read: [Homogeneous-Redundancy](https://github.com/BOINC/boinc/wiki/Homogeneous-Redundancy)). The alternative is a validator that compares approximately (summary).
- **Collusion.** No stakes. The penalty is lost credit and a reset count. Nothing in the pages read addresses a host that builds trust and then cheats on its unreplicated share (unconfirmed, an inference).
- **Who pays.** Volunteers give the time; the project pays for redundancy in lost throughput.

### Golem

- **Clay and Brass (2016 to 2020).** Verification by redundancy, or by the requestor recomputing a small random part, such as some pixels of a render (read: [architecture](https://blog.golem.network/golem-architecture/)). For WebAssembly tasks, two providers were compared and a third broke ties, which works "because the WASM computations are deterministic"; the chance of a check fell with a provider's reputation (read: [gWASM verification](https://blog.golem.network/gwasm-verification/)).
- **Yagna (2021 on).** A marketplace with pay-as-you-go invoices the requestor accepts. Its documents describe no check of results by the network (read: [interaction](https://docs.golem.network/docs/creators/common/requestor-provider-interaction)), and its reputation system scores performance, not correctness (read: [reputation](https://blog.golem.network/introducing-golem-networks-reputation-system/)).
- **Who pays.** The requestor, for every redundant run.

### Truebit (2017)

Outsourced deterministic computation for contracts on Ethereum (read: [paper](https://arxiv.org/abs/1908.04756)).

- **A dispute game.** A solver posts an answer and a deposit. Anyone may challenge. The two then bisect the computation's trace, round by round, until one step is left, which the chain runs itself. One honest challenger is enough.
- **Forced errors.** Checkers who never find anything stop checking. So about one task in a thousand is made wrong on purpose, and whoever catches it wins a jackpot, paid for by a tax on every task that the paper puts at 500 to 5,000 per cent of the task's cost.
- **Collusion and false identities.** With k challengers the jackpot paid is divided as J/2^(k−1), so cloning oneself earns less, not more.
- **Non-determinism.** Not supported: "the computation steps for a given task must form a deterministic sequence".
- **Weakness.** The tax, and the paper's own word that the game degrades for large tasks.

### iExec

- **Earlier (about 2019).** The requester named a confidence level. Workers staked; a task was repeated until the stake and reputation behind one answer reached the level; those who agreed took the stake of those who did not (read: [audit summary](https://chainsecurity.com/security-audit/iexec-v3); the formula itself is unconfirmed). An audit finding is titled "worker score system can be gamed" (summary).
- **Now.** Trusted hardware (Intel TDX): "No replication is needed, trust comes from hardware attestation, not from multiple workers" (read: [Proof of Contribution](https://docs.iex.ec/protocol/proof-of-contribution)). Trust moves to the chip's maker.

### Render Network

GPU rendering. A person looks: the creator reviews watermarked previews and approves or rejects each frame, and frames are approved by themselves after 72 hours (read: [review and approve](https://know.rendernetwork.com/getting-started/how-to-get-started/review-and-approve-frames.md)). Reputation runs both ways, and nodes are ranked in tiers (read: [tiers](https://know.rendernetwork.com/getting-started/how-to-get-started/select-rndr-tier.md)). No stake or slashing was found for nodes. It works only for output a person can judge by eye.

### Akash Network

A market for leasing containers. No check of results was found in its documents (unconfirmed as a statement, since no page says so outright). What exists is escrow paid block by block and auditors who sign a provider's stated hardware (read: [provider audit](https://akash.network/docs/providers/operations/provider-audit)). The tenant carries the risk.

### Bittensor

No check of results by the protocol. Validators in each subnet score miners as that subnet chooses and submit weights; the chain takes the median weighted by stake and clips what is above it (read: [consensus](https://www.bittensor.com/llms.mdx/docs/internals/consensus/content.md)). Validators who copied others' scores are countered by having scores committed before they are revealed (read: [commit-reveal](https://www.bittensor.com/docs/hyperparameters/commit-reveal-weights-enabled)). It tolerates work that differs from run to run because nothing is compared for equality. Its own documents say a bad actor with 40 per cent of stake can gain a majority over time when scoring is subjective. Token emissions pay, not the requester.

### Primecoin (2013)

The work is finding chains of primes, which any node checks cheaply. Nothing to collude over, and no redundancy to pay for; but only problems that are cheap to check can be done this way (summary; the paper could not be read).

### Gridcoin

Pays for BOINC work in its own coin and leaves checking to the BOINC projects: "Gridcoin relies on the RAC values provided by the project" (read: [FAQ](https://gridcoin.us/wiki/faq)). Which projects count is decided by vote.

### Satori Network (on Evrmore)

Worth a longer look than its size suggests, because it is built on the chain Sisyphus is considering. Its nodes predict the next value of data streams, mostly prices.

- **How a result is checked.** It is not. Nothing verifies the computation. A prediction is scored by its distance from the real value once that arrives, and the best are paid (read: [user guide](https://github.com/SatoriNetwork/satori-lite/blob/main/docs/guides/USER_GUIDE.md), [scorer](https://github.com/SatoriNetwork/satori-lite/blob/main/neuron-lite/scoring/mae.py)). A prediction that arrives after 90 per cent of the interval is disqualified, against waiting until the answer is nearly known.
- **Who decides.** A central server run by the Satori Association registers nodes, assigns streams, checks stakes, scores once a day and pays out. Its code is not public: "For security reasons the Satori Server code is not publicly available" (read: [Neuron README](https://github.com/SatoriNetwork/Neuron)). The transport moved to Nostr relays in 2026; scoring and payment did not.
- **What Evrmore is used for.** Payment only. The token is an Evrmore asset, now `SATORIEVR`, and it is reissuable: the daily reward is made by issuing more, so the supply is whatever the holder of the owner token mints, not something the chain enforces (read: a live query of an Evrmore server on 10 October 2026; [tokenomics](https://satorinet.io/tokenomics)). Who holds that key is unconfirmed.
- **Stake.** A node must hold a minimum balance, 250 at the time of reading, raised several times since launch. It is a balance checked by the server, not a lock, and nothing is ever taken (read: [tokenomics](https://satorinet.io/tokenomics)). Pools lend "virtual" stake to workers by a signature with no funds moving, and their own documents say paying stakers "relies on pool operator honesty".
- **False identities.** The stake, and a server rule of one node per machine. No proof of work or of hardware.
- **Who pays.** New coins, split by a vote of token holders: at the time of reading about 85 per cent to predictors and 15 to development.
- **What went wrong.** A wrapped token on another chain was bridged by a custodian, and the roadmap speaks of a "rescue plan ... after the bridge exploit". No account of the exploit was found. The asset was later migrated from `SATORI` to `SATORIEVR`; that the two are connected is an inference. A repository for a chain of its own, a fork of Evrmore, exists and holds little; whether it runs is unconfirmed (read: [SatoriChain](https://github.com/SatoriNetwork/SatoriChain)).

What it teaches a compute network thinking of Evrmore:

- **An Evrmore asset pays cheaply and enforces nothing.** A supply cap, a lock on a stake or a penalty all need a server that is trusted, scripts the chain may not have, or a fork. Satori has the server and drifted towards the fork.
- **Holders of an asset need the chain's own coin to move it.** Satori sent a little to some 40,000 addresses so that they could (read: [AirdropEVR](https://github.com/SatoriNetwork/AirdropEVR)).
- **Evrmore's scripts are enough for payment channels.** Satori's library has two-of-two channels with timed refunds, for paying by the result (read: [scripts](https://github.com/SatoriNetwork/satorilib)). One transaction can pay about a thousand outputs.
- **"The truth arrives later" only suits forecasting.** A count of words or a rendered frame has no later truth to be scored against, so nothing of Satori's scoring carries over to general compute.

### Three more, for work that is not the same twice

- **Livepeer** (video transcoding) planned a Truebit-style game, found that "trying to force determinism with GPU video transcoding would be impractical", and turned slashing off. It moved to statistical checks: a detector of tampering, and a proposal to compare perceptual hashes of a segment from one trusted node and several untrusted ones (read: [forum post, 2021](https://forum.livepeer.org/t/transcoding-verification-improvements-fast-full-verification/1499)). Whether the proposals shipped is unconfirmed.
- **opML** (2024) plays Truebit's game over a model's inference, made deterministic by fixed-point arithmetic and floating point done in software (read: [paper](https://arxiv.org/abs/2401.17555)).
- **Proof of Sampling** (2024) has a second server recompute with probability p and slashes on a mismatch. It gives the condition under which honesty is the only stable choice: p > C / ((1 − r)(R + S)), for a cost of cheating C, a reward R, a stake S and a colluding share r; its example needs p of under one per cent at r of ten per cent. It needs deterministic execution too (read: [paper](https://arxiv.org/html/2405.00295v2)).

Proofs in zero knowledge of a model's inference exist and cost orders of magnitude more than the inference (summary).

## The families of technique

| Technique | Who uses it | Where Sisyphus stands |
| --- | --- | --- |
| Replication with a quorum | BOINC, Golem Clay, iExec earlier | Has it. |
| Fewer checks for the trusted | BOINC, Golem | Has a standing that adds checks after a fault; does not yet remove checks for a good record. |
| Random checks with a stake to lose | Proof of Sampling | Has the random checks; has no stake. |
| A dispute game, one honest checker enough | Truebit, opML | Not built. Needs exact determinism. |
| Tasks made wrong on purpose | Truebit | Not built. A coordinator could send tasks whose answer it knows. |
| Stake and a confidence level | iExec earlier | Not built; needs a chain. |
| Trusted hardware | iExec now, Phala | Not built. Would also keep data from the worker. |
| Work that checks itself | Primecoin | `primes` could be made so; most work cannot. |
| Scoring by a stake-weighted median | Bittensor | Not built. |
| A person, or an approximate comparison | Render, Livepeer, BOINC's validators | Not built. The only prior art for work that differs from run to run. |
| Equivalence classes of hardware | BOINC | Not built. Workers already report their hardware. |
| None; reputation and the market | Akash, Golem Yagna | The baseline a job without `--verify` has. |
| Scored against a truth that arrives later, by a trusted server | Satori | Does not apply: most work has no later truth. |

## What it says to the four open questions

1. **Collusion.** Five defences were found: assign at random and size the stake against the share assumed to collude (Proof of Sampling); a game one honest party can win (Truebit, opML); rewards that shrink when identities multiply (Truebit); results committed before they are revealed, so one worker cannot copy another (iExec, Bittensor); and reference nodes that are trusted outright (Livepeer). The first, fourth and fifth need no chain.
2. **Work that is not the same twice.** Three routes: force it to be the same (fixed-point arithmetic, WebAssembly); confine it to hardware that agrees (BOINC); or compare approximately (validators, perceptual hashes, a person). Livepeer gave up the first for GPU video.
3. **Who pays for the extra runs.** The requester (Golem, iExec, Truebit, Render), new coins (Bittensor, Gridcoin, Primecoin, Satori), or nobody, because the capacity is given (BOINC). A private pool is in the third case today.
4. **Stakes and reputation.** Stakes are slashed in iExec, Truebit, Proof of Sampling and Filecoin. Reputation alone is what BOINC, Render and Golem have. Satori's stake is a balance that is checked and never taken, which deters false identities and not wrong answers. Livepeer turned slashing off once its checks became matters of likelihood.

What Sisyphus should take from this is in [verification: what to build next](verification-next.md).
