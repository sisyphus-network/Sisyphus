# Paid storage, and assets that point at data

A note for [#35](https://github.com/sisyphus-network/Sisyphus/issues/35) and [#36](https://github.com/sisyphus-network/Sisyphus/issues/36). Both depend on the chain ([#20](chain-design.md)) and the token ([#21](token-economics.md)). This note says what the daemon already has that they would build on, what can be designed now, and what has not been looked into. It is a proposal to argue with.

## What exists today

Without a chain, a pool already has most of the machinery a storage market needs, with trust standing where payment and proof would be:

- **Content addressing.** Everything stored is named by its CID, and whatever is fetched is checked against it.
- **Pins with owners and expiry**, and garbage collection of what nothing pins.
- **Copies on other nodes.** Storage followers hold what the coordinator signs a list of, and the coordinator takes it back if it loses its own store. With IPFS Cluster, pins are held by several nodes.
- **A pinning service** that speaks the standard API, so other tools can ask a node to keep data.
- **A record of each finished job** as linked data, with a CID of its own.
- **Names** that point at a CID and can be moved.
- **Sealing**, so that a holder who is not meant to read data cannot.

What it lacks is the two things the issue names: a reason for a stranger to keep data, and a way to know they still do.

## Storage for pay (#35)

### Proof that data is still held

The issue's first step is the right one and its caution is right: a random challenge shows possession at that moment and nothing more.

A challenge that fits what is already here: the challenger names a CID, a random offset and a length, and a fresh random value. The holder returns the hash of that value with those bytes. The challenger, who holds the data or its Merkle tree, checks it. Sealed blobs can be challenged as they are, since the sealed bytes are what is stored.

What it does not show, to be said wherever it is offered:

- **That there is more than one copy.** Two "providers" can be one disk. Filecoin's sealing exists to make each copy cost its own work, and proves it on chain; a missed proof costs the provider collateral ([Filecoin proofs](https://docs.filecoin.io/basics/the-blockchain/proofs)).
- **That the data will be there tomorrow**, or that it will be handed over when asked.
- **Anything, if the challenger does not hold the data.** A challenger without it needs the Merkle tree, which for IPFS-style data it can fetch once and keep: the tree is small.

Who challenges: the issue suggests masternode quorums. That works if a quorum's members can each challenge and the quorum signs what they found, as in [the chain note](chain-design.md). Until there is a chain, the coordinator can challenge its followers itself, and now does: each time a follower says what it holds it is asked for the digest of a random stretch of up to four blobs, and a copy not shown in thirty seconds is not counted.

### Deals

A deal is: this CID, for this long, at this price, with this much collateral, checked this often. On chain it is a special transaction, and the pin a node makes for it is a pin like any other, owned by the deal and expiring with it. The daemon's pins already have owners and expiry, so the local half is small.

Akash's escrow is the model for payment that was looked at: the buyer's deposit is paid out block by block and the lease ends when it runs out ([Akash payments](https://akash.network/docs/getting-started/intro-to-akash/payments)). For storage, paying only for periods in which a challenge was answered ties the payment to the proof.

### Retrieval, and data held hostage

Proof of holding is not proof of serving. The plain defences: keep more than one copy with unrelated holders, which the cluster already does inside a pool; pay for retrieval separately and only on delivery of bytes that check against the CID; and count a refusal as a failed challenge. None stops a holder who would sooner lose the deal than serve. Erasure coding across holders, so that any k of n suffice, is the structural answer and is in the issue's list.

### The first deployable step

The issue's fifth item stands out as buildable early: masternodes must pin recent job records for a fixed window. Records are small, they are what a reward is paid against, and a quorum can challenge its own members for them. It gives the network a memory before it has a market.

### Survey: Storj, Sia, Filecoin, Arweave and Dash Platform

This is a comparison of the designs described in each project's own documentation, not an independent security or performance review. Product details and protocol versions change; current prices are deliberately omitted.

| Network | How storage is organized and checked | Payment and failure handling | What is useful here—and what is not |
|---|---|---|---|
| [Storj](https://storj.dev/learn) | A Satellite chooses storage nodes and coordinates metadata, reputation, audit and repair. The client encrypts data, erasure-codes it, and sends pieces directly to different nodes. Audits request a small byte range; uptime and audit results feed reputation, and repair regenerates pieces when availability nears a threshold. | Storage and bandwidth are accounted for and paid through the Satellite service. This is an operational marketplace with a managed control plane, not a public-chain storage contract with provider collateral. | Strong reference for the data path, sampling audits, diversity, and automatic repair. Do not inherit the assumption that one trusted Satellite may control placement, reputation and payment in an open Sisyphus network. [Audit and repair](https://storj.dev/learn/concepts/file-repair) |
| [Sia](https://docs.sia.tech/store-your-data/about-renting) | A renter forms a blockchain-enforced contract with a host. The current host guide describes a storage-proof window at contract end: the host submits a cryptographic proof to the chain that it still has the renter's data. The renter-side SDK handles encryption and erasure coding; an indexer manages contracts and health. | Hosts set storage and bandwidth prices and lock collateral. A valid proof releases earned revenue and collateral; missing the proof can forfeit both. Host availability, data integrity, and the ability to pay the chain transaction fee all matter. | Useful reference for explicit per-deal terms, proof windows, collateral and separating data transfer from contract/indexer services. The full chain-enforced contract lifecycle is more involved than a coordinator asking a follower for a random byte-range digest. [Host contracts and proofs](https://docs.sia.tech/provide-storage/about-hosting-on-sia) |
| [Filecoin](https://docs.filecoin.io/reference/general/glossary) | Storage is committed in sectors. Proof-of-Replication establishes a provider-specific replica; Window Proof-of-Spacetime repeatedly proves continued storage, with sector proofs recorded on-chain. | Storage deals lock client/provider balances and provider collateral; faults and missed proofs can result in slashing. Retrieval is also modeled as a provider service/deal. | The clearest reference for cryptographic replica uniqueness and public, recurring proofs, but its sealing, sector and proof machinery is not equivalent to cheap random sampling and should not be treated as a small feature to copy. [Storage-market actor](https://docs.filecoin.io/reference/built-in-actors) |
| [Arweave](https://docs.arweave.org/developers/mining/overview/mining) | SPoRA ties block production to proving access to pseudorandom historical data chunks; the protocol encourages miners to keep distinct packed replicas. | Upload fees contribute to a storage endowment that pays miners over time. This is a consensus-coupled permanent-archive model, not a market of time-bounded provider contracts with a separately priced retrieval service. | Useful for understanding the coupling between storage incentives and block production, and why proof-of-storage can change consensus. It is a poor direct template if Sisyphus keeps conventional block ordering and sells finite storage deals. [Protocol overview](https://github.com/ArweaveTeam/docs.arweave.org-info/blob/master/development/protocol/overview.md) |
| [Dash Platform Drive](https://docs.dash.org/projects/platform/en/latest/docs/explanations/drive.html) | Drive is the layer-2 storage/state component for identities, contracts, documents and other Platform data. Platform validators order state transitions; nodes apply committed transitions to their Drive state. Dash documentation says evonodes run Platform services, but the Drive overview describes storage as hosted by masternodes and does not specify a per-provider storage deal or public proof-of-retention protocol. | Platform fees/credits and evonode block rewards belong to the Platform service/consensus design; the cited Drive material does not describe a standalone paid storage market, retrieval pricing, or collateral slashing for an individual stored object. | Useful as a reference for separating an application-data layer from the payment chain and for state-machine-driven replication. Do not treat Platform consensus or evonode participation rewards as proof that a particular provider independently retains a particular blob. The public docs checked here do not establish Drive's exact replication/failure-domain guarantees. [Evonode glossary](https://docs.dash.org/projects/platform/en/stable/docs/reference/glossary.html) |

#### What this comparison suggests for Sisyphus

1. Keep the current random-range check accurately scoped: it can catch a follower that cannot serve sampled bytes when challenged. By itself it does not establish an independent physical copy, future availability, or successful retrieval by a client.
2. If storage is sold by contract, specify the CID, term, price, collateral, challenge schedule, proof deadline, retrieval terms and failure outcome together. A proof should be verifiable by an actor other than the provider, and challenge selection must not be predictable before the provider commits to the data.
3. Treat diversity and repair as separate from possession proofs. Distinct node IDs are not evidence of distinct operators or failure domains; the network needs a policy for placement, repair and operator identity before it can claim independent redundancy.
4. Consider a staged path: require a bounded retention window for recent job records first, then trial coordinator-issued audits with signed receipts, and only design trustless collateral/slashing after #20 and #21 settle the chain and payment model. This is a proposal, not a protocol decision.

Sources checked 14 October 2026. Official operator documentation describes each project's intended behavior; it does not independently prove the claims or establish their present network performance.

## Assets that point at data (#36)

### What Ravencoin's assets carry

One 32-byte hash, written as an IPFS identifier of the oldest form (a SHA-256 multihash, base58, 46 characters beginning `Qm`), or a transaction ID ([assets code](https://raw.githubusercontent.com/RavenProject/Ravencoin/master/src/assets/assets.cpp)). Evrmore's mainnet is the same. A test release of Evrmore adds a hash that cannot be changed once set and a call to update metadata ([releases](https://github.com/EvrmoreOrg/Evrmore/releases)).

So an asset can name a file added to IPFS the old way, and nothing else: not a job record, which is linked data with another codec, and not anything hashed with another function.

### A full CID

The issue's first item is the fix: carry the CID whole, with its version, codec and hash. A CID is a short prefix and a hash, about 36 bytes for the common case, so the cost in space is a few bytes over what is there.

- **Consensus should check its form and nothing else**: that it parses as a CID, within a length limit. Not that the data exists, which no node can know.
- **A limit** on length, so that an identity hash, which carries data inline, cannot be used to put arbitrary bytes on chain. Refuse the identity hash outright.

The wire field should be the **binary CID**, not its printable multibase string. The string base is a presentation choice; consensus must not give different encodings of the same CID different meanings. The CID specification also requires minimally encoded varints and exact consumption of the multihash digest, rejecting malformed/trailing bytes ([CID specification](https://github.com/ipfs/specs/blob/main/src/cid.md)).

#### Draft validation boundary (not a chain decision)

1. Parse the complete byte string as a CID; reject malformed versions, non-minimal varints, truncated fields and trailing bytes.
2. Reject identity multihashes, so a CID cannot be used to smuggle its payload into the transaction.
3. Treat the parsed CID as an identifier only. Do not fetch its content, resolve a name, or rely on a gateway during consensus validation.
4. Keep the accepted CID versions, codec/hash allowlists, maximum encoded size, legacy-v0 migration policy and fee schedule explicitly open for #20/#21. The Multiformats registries include codecs and hash functions with different properties; registry membership alone is not a security allowlist ([multihash notes](https://github.com/multiformats/multihash#insecure--obsolete-hash-functions)).

This gives implementers a deterministic parsing contract without choosing network-specific serialization limits or fees before the chain and token work is settled.

### Hints are not identifiers

The issue has this right and it should stay in the design's first sentence: the CID says what the content is; a hint says where a copy may be; a reader checks what it fetched against the CID. A hint that lies costs a reader a failed fetch and nothing else.

That makes hints safe to leave out of consensus rules beyond a size limit and a fee. A gateway URL, an S3 endpoint and bucket, a pinning service: each is a string a wallet may show and try.

### A name in place of a CID

For content meant to change, an asset can carry a typed mutable name rather than a CID. These are different identifier kinds and should not be represented by an untagged string. For IPNS, the name is tied to a public key and records are signed by its private-key holder; the record includes a path and validity information. That proves who signed a record, not that a resolver returned the latest record: publication and resolution have separate availability/consistency tradeoffs ([IPNS overview](https://docs.ipfs.tech/concepts/ipns/)).

The daemon also has its own signed names: a node's ID stands for whichever CID the node last signed, with a record that says until when. Either kind of name is an off-chain pointer whose resolution may change or lag. Consensus can preserve the name bytes and type, but must not treat its current resolution as the asset's immutable content commitment. Reissuing the asset with a new CID is the on-chain alternative and leaves a history; a mutable name does not.

### Job receipts

The chain note argues that a receipt is better as the reward transaction, which carries the record's CID, than as a named asset. An asset pointing at a record remains useful where someone wants to hold and transfer the right to a result: a rendered film, a trained model. The record already holds, by CID, who ran what and the results' CIDs, and a private job's record holds commitments in their place.

## What can be built now

1. **Challenges from a coordinator to its followers.** Built since this note was first written: a random range of a random blob, answered with a digest under a fresh number, counted in what `blob replicas` reports. What it leaves for a paid network is everything in the list above: more than one real copy, the future, and retrieval.
2. **A written format for a deal** and for a challenge, as messages, so that the chain work has something exact to carry.
3. **A CID field's rules** written as a short specification: strict binary parsing and refusal of inline data are clear now; accepted versions/codecs/hashes, the length limit and fee remain decisions for #20/#21.
