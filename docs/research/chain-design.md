# The chain: what Dash and Evrmore each bring

A design note for [#20](https://github.com/sisyphus-network/Sisyphus/issues/20). It is a proposal to argue with. No fork has been made and no chain node has been run for it. Core design facts were gathered on 10 October 2026 from the chains' own documents and source, and each says where it is from; upstream release cadence below was measured on 19 October 2026. A fact marked *unconfirmed* was recalled and not found in a source.

## What each chain is

### Dash

- **Blocks.** Proof of work with X11, a 2.6 minute target and difficulty set every block. The subsidy falls by a fourteenth about every 383 days ([features](https://docs.dash.org/en/stable/docs/user/introduction/features.html)).
- **Who is paid.** Since the fork in v20 (late 2023) the subsidy goes 20 per cent to miners, 20 per cent to the governance budget and 60 per cent to masternodes; fees go a quarter to miners and three quarters to masternodes ([block reward](https://dash-user-docs.readthedocs.io/en/sitemap/docs/core/reference/block-chain-serialized-blocks.html)). Miners secure the order of blocks for a fifth of the reward. The rest of the security comes from the next two items.
- **Masternodes.** 1,000 DASH of collateral, registered on chain by a special transaction with separate owner, operator and voting keys ([DIP3](https://raw.githubusercontent.com/dashpay/dips/master/dip-0003.md)). A masternode that fails its duties gathers a penalty score and is banned from payment until its operator revives it ([proof of service](https://docs.dash.org/en/stable/docs/core/guide/dash-features-proof-of-service.html)). A larger kind, 4,000 DASH, also runs Dash Platform ([DIP28](https://raw.githubusercontent.com/dashpay/dips/master/dip-0028.md)).
- **Quorums.** Subsets of masternodes, chosen deterministically, generate a shared key among themselves and sign with a threshold of members ([DIP6](https://raw.githubusercontent.com/dashpay/dips/master/dip-0006.md)). Sizes run from 50 members with 30 to sign, to 400 with 340.
- **ChainLocks.** A quorum of 400 signs the first block seen at each height, and nodes then refuse any other block at or below it. A block is final after one confirmation, and a miner with most of the hash power cannot reorganise the chain. It assumes an attacker holds under about 30 per cent of masternodes ([DIP8](https://raw.githubusercontent.com/dashpay/dips/master/dip-0008.md)).
- **InstantSend.** Quorums lock a transaction's inputs within a second or two, so it cannot be spent twice before it is in a block ([DIP10](https://raw.githubusercontent.com/dashpay/dips/master/dip-0010.md)).
- **Governance.** Masternodes vote on proposals; one passes with a net tenth of votes in favour, and about monthly a block pays the winners from the budget. Budget not allocated is never created ([governance](https://docs.dash.org/en/stable/docs/user/governance/understanding.html)).
- **Special transactions.** A typed payload on an ordinary transaction, which is how masternode registration and the rest are carried ([DIP2](https://raw.githubusercontent.com/dashpay/dips/master/dip-0002.md)). This is the extension point a reward transaction would use.
- **Platform.** A second chain run by the larger masternodes, for data contracts, identities, names and tokens, with no user code on chain ([what is Dash Platform](https://docs.dash.org/projects/platform/en/stable/docs/intro/what-is-dash-platform.html)). Live on mainnet since 2024.
- **Licence.** MIT.

### Ravencoin, and Evrmore after it

- **Ravencoin** is a fork of Bitcoin's code with one-minute blocks, KAWPOW mining for graphics cards, and assets in the protocol ([repository](https://github.com/RavenProject/Ravencoin)).
- **Assets.** A name is bought by burning coin: 500 for a main asset, 100 for a sub-asset, 5 for a unique one, 1,500 for a restricted one, 1,000 for a qualifier tag ([chain parameters](https://raw.githubusercontent.com/RavenProject/Ravencoin/master/src/chainparams.cpp)). An asset has a quantity, up to eight decimals, and a flag for whether more can be issued. An owner token controls it. A unique asset has quantity one.
- **Restricted assets and tags.** An issuer names the tags an address must hold to receive a restricted asset, and can freeze an address or the whole asset. A qualifier tag is itself an asset, given to addresses.
- **Metadata.** One 32-byte hash: an IPFS identifier of the oldest form, or a transaction ID ([assets code](https://raw.githubusercontent.com/RavenProject/Ravencoin/master/src/assets/assets.cpp)). Nothing on chain keeps the data it names.
- **Limits.** No contracts and no rules on transfer beyond tags and freezing. One global namespace, first come first served. The burned fees pay nobody, so asset activity does not fund security.
- **Evrmore** is a fork of Ravencoin's code and coin balances with a new first block in late October 2022; assets were not carried over ([repository](https://github.com/EvrmoreOrg/Evrmore)). It mines with EvrProgPow and, unlike Ravencoin, requires a tenth of every block reward to go to a development fund ([miner fund](https://raw.githubusercontent.com/EvrmoreOrg/Evrmore/master/src/minerdevfund.cpp)). Its asset layer on mainnet is Ravencoin's. Tolls on transfers, burning and reminting, and metadata that cannot be changed are in a test release and not on mainnet ([releases](https://github.com/EvrmoreOrg/Evrmore/releases)). Its maximum supply and the terms of its airdrop are *unconfirmed*.
- **Licence.** MIT.

### Upstream release and maintenance cadence

I counted published GitHub releases dated **19 October 2024 through 19 October 2026** (inclusive; queried 19 October 2026), rather than counting every git tag:

| Upstream | Published stable releases | Published pre-releases | Release range in the window |
| --- | ---: | ---: | --- |
| [Dash Core](https://github.com/dashpay/dash/releases) | 14 | 11 | `v21.1.1` (23 Oct 2024) to `v23.1.8` (3 Aug 2026); `v24.0.0-rc.3` was the latest pre-release (7 Oct 2026) |
| [Evrmore Core](https://github.com/EvrmoreOrg/Evrmore/releases) | 2 | 1 | `v1.0.5.2` (10 Jun 2026) to `v1.1` (13 Aug 2026); `v2.0.0test` is testnet-only |

This is a count of published releases, not a claim that every release needs to be merged. The release notes show why a fork cannot simply ignore the stream: Dash [`v22.0.0`](https://github.com/dashpay/dash/releases/tag/v22.0.0) was a required major upgrade, [`v23.1.7`](https://github.com/dashpay/dash/releases/tag/v23.1.7) was mandatory, and [`v23.1.8`](https://github.com/dashpay/dash/releases/tag/v23.1.8) fixes three remotely triggerable crashes. Evrmore [`v1.1`](https://github.com/EvrmoreOrg/Evrmore/releases/tag/v1.1) addresses a mining vulnerability and an asset-database/reindex bug. Those are upstream maintenance obligations even if Sisyphus never adopts their product features.

The raw commit-graph distance is also uneven. GitHub's comparison reports **5,833 commits** from Dash `v21.1.1` to `v23.1.8`, versus **4 commits** from Evrmore `v1.0.5.2` to `v1.1` ([Dash comparison](https://github.com/dashpay/dash/compare/v21.1.1...v23.1.8), [Evrmore comparison](https://github.com/EvrmoreOrg/Evrmore/compare/v1.0.5.2...v1.1)). This measures ancestry distance between release tags, not the number of commits a fork must cherry-pick: it includes unrelated features, merges, and development history. It does show that "track upstream" means very different workloads for the two candidates.

**Implication (inference):** a Dash-based fork needs a maintained upstream review lane, not occasional rebasing as an afterthought. At minimum, each stable release should be triaged for security fixes, consensus/serialization changes, RPC/API changes and build/dependency changes; mandatory or security releases need an explicit adopt, backport or reject decision, with regression/regtest evidence. Evrmore's lower release and tag distance is cheaper to inspect, but the release count alone cannot establish its security or maintenance quality. Neither count answers whether the two codebases build together or how much an asset-layer port costs; those still require the regtest/port spike in #20.

### What a network already on Evrmore found

[Satori](verification-prior-art.md#satori-network-on-evrmore) pays its nodes in an Evrmore asset, and its experience bears on three of the proposals below.

- An asset carries payment and no rules. Satori's supply, stakes and rewards are all enforced by its own server, because the asset layer has no lock, no penalty and no cap on a reissuable asset. Anything Sisyphus wants enforced, collateral that can be taken above all, has to be in consensus.
- Every holder of an asset also needs the chain's coin for fees.
- A custodian's bridge to another chain was where it was exploited.

## The proposals in the issue, taken one at a time

### What secures the order of blocks

The issue's hard question is right, and the survey for #19 agrees: no network surveyed orders blocks by useful work except Primecoin, whose work checks itself. Useful jobs have no difficulty to adjust and are chosen by users.

Dash already separates the two things. Ordering is proof of work, made final by a masternode quorum's signature one block later. That is the model to keep: **a conventional mechanism orders blocks, and useful work is paid by transactions that the chain verifies.** "Proof of useful work" then names what earns the reward, not what makes blocks.

Whether the conventional mechanism should be proof of work at all is open. Dash keeps miners at a fifth of the reward and lets ChainLocks carry finality. A new chain with little hash power is easy to attack by mining alone, and ChainLocks is what would protect it; but ChainLocks needs hundreds of honest masternodes, which a new chain does not have either. A start with fewer, larger quorums run by known parties, and a stated plan to widen them, is the honest shape of the first year.

### Masternodes as coordinators and verifiers

The fit is real. A coordinator is trusted with everything in its pool, and today nothing checks it. A masternode has collateral, an on-chain identity, and a score that bans it for failing its duties. Two cautions:

- Dash's penalty, as its proof of service page describes it, is a ban from payment, not a loss of collateral. Verification with stakes ([the #19 note](verification-next.md)) needs collateral that can be taken. That is a consensus change to Dash, not a setting.
- A masternode's duty in Dash is cheap and uniform. Coordinating jobs is neither. If every masternode must coordinate, small operators are priced out; if only some do, the reward has to follow the work. The second is the Dash Platform arrangement, where the larger nodes do more and are paid from a separate pool.

### A quorum's signature on a job's result

A quorum can sign any message. The job's record already has a content identifier that changes if anything in it does (the IPLD record of a finished job). So the attestation can be small: the record's CID, the workers paid and the amounts, signed by a quorum. The reward transaction is a special transaction carrying it, valid only with a good signature from the quorum of that height.

What the quorum is attesting to has to be decided before anything else. It cannot rerun the job. It can check that the record is well formed, that N distinct staked workers returned the same result, and that no challenge was raised in a window. That is replication plus a dispute window, signed, which is as much as the survey found anyone doing without trusted hardware.

### Fast payment for short jobs

InstantSend suits it as it stands: a payment locked in a second or two. No change proposed.

### Tags for vetted providers, assets for receipts

Qualifier tags do what is wanted for vetting: a tag is given to an address by whoever owns the tag, and can be required. Akash's signed provider attributes are the same idea.

Job receipts as assets fit less well. A Ravencoin asset costs a burn to name and lives in one global namespace; a receipt is one of millions and nobody wants to name it. A receipt is better as the reward transaction itself, which already carries the record's CID. Assets remain right for things people do name and trade: compute credits, a dataset, a model.

### Porting the asset layer onto Dash

Both descend from Bitcoin Core at different versions, and both have changed transaction handling: Dash with typed special transactions, Ravencoin with asset data in output scripts. Dash Platform has since added tokens of its own, with minting, burning and freezing, on its second chain. So there are three ways to have assets, and porting Ravencoin's is only one:

1. Port Ravencoin's script-level assets into Dash Core. The largest change to consensus code, and the one that brings the namespace and burn model with it.
2. Carry assets as a new type of Dash special transaction, taking Ravencoin's model (names, units, reissue, tags) and not its encoding.
3. Use Dash Platform's tokens and data contracts, and write no asset code.

The effort of each has not been measured. That needs both codebases built and run, which is the issue's fifth task and has not been done.

### How `sisyphusd` could talk to a chain node

This is an interface sketch for a Dash-based prototype, not a decision to fork Dash or an implementation contract. Evrmore compatibility has not been checked.

Dash Core exposes two different local interfaces:

- **JSON-RPC for queries and commands.** `dashd` accepts authenticated HTTP JSON-RPC requests. The default bind is loopback; the RPC protocol is not encrypted, and Dash warns against exposing it to the public Internet. RPC access can include wallet and administrative operations, so the Sisyphus adapter should use a dedicated local credential, keep it out of job input and logs, and call only the methods the feature needs. See the [RPC guide](https://docs.dash.org/projects/core/en/develop/docs/api/remote-procedure-calls.html) and [RPC security notes](https://github.com/dashpay/dash/blob/develop/doc/JSON-RPC-interface.md#security).
- **ZeroMQ for event notifications.** Dash Core can publish block, transaction, ChainLock and other notifications. This channel is read-only but unauthenticated; Dash says to expose it only to trusted local clients and notes that notifications can be lost. A block-tip change may report only the new tip, leaving the subscriber to retrieve the missing chain range. See the [Dash ZMQ reference](https://docs.dash.org/en/develop/docs/core/api/zmq.html).

**Proposed split:** use RPC as the authority for reading chain state and submitting an explicitly constructed transaction. Treat ZMQ as a low-latency wake-up signal only: after startup, reconnect, or a sequence gap, reconcile from the last persisted block hash through RPC, including reorganisations. Never finalize a reward from a notification alone. This is an inference from the documented interfaces; it still needs a regtest prototype against a pinned Dash release.

Before implementation, decide whether Sisyphus connects only to an already-running local node or may manage its lifecycle; whether that node's wallet is enabled; which RPC methods and credential scope are necessary; and which specific ZMQ topics and recovery behavior the chosen Dash release supports. Keep the RPC and ZMQ listeners loopback/IPC-only unless a separately secured private transport is designed. Do not forward either listener through the pool's public port.

## Open, and what would close it

| Question | What would answer it |
| --- | --- |
| Is proof of work kept for ordering? | A decision; then a look at what hash power a small X11 or ProgPoW chain attracts and what it costs to attack. |
| Can collateral be taken, not only banned? | Reading Dash's masternode payment and collateral code, and a design for a slashing transaction. |
| What exactly does a quorum attest? | The dispute window and stake design from #19, written as a message format. |
| Which of the three ways to have assets? | Both chains built and run in regtest; a spike of option 2. |
| How does `sisyphusd` talk to the chain? | This interface sketch separates authenticated RPC from local, unauthenticated ZMQ notifications. A regtest prototype against a pinned Dash release should verify reconnect recovery, reorganisation handling and the wallet/credential boundary. |
| How much of upstream can be tracked? | The two-year release counts and tag-to-tag commit distances are measured above. Still needed: classify each relevant release's security, consensus, RPC/API and dependency changes, then test a sustainable review/backport lane on a pinned Dash release. |

None of this costs money. All of it costs time with two large C++ codebases, and belongs to whoever knows them best.
