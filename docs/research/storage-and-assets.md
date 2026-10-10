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

Who challenges: the issue suggests masternode quorums. That works if a quorum's members can each challenge and the quorum signs what they found, as in [the chain note](chain-design.md). Until there is a chain, the coordinator can challenge its followers itself. That is worth building now: today a coordinator believes a follower's report of what it holds.

### Deals

A deal is: this CID, for this long, at this price, with this much collateral, checked this often. On chain it is a special transaction, and the pin a node makes for it is a pin like any other, owned by the deal and expiring with it. The daemon's pins already have owners and expiry, so the local half is small.

Akash's escrow is the model for payment that was looked at: the buyer's deposit is paid out block by block and the lease ends when it runs out ([Akash payments](https://akash.network/docs/getting-started/intro-to-akash/payments)). For storage, paying only for periods in which a challenge was answered ties the payment to the proof.

### Retrieval, and data held hostage

Proof of holding is not proof of serving. The plain defences: keep more than one copy with unrelated holders, which the cluster already does inside a pool; pay for retrieval separately and only on delivery of bytes that check against the CID; and count a refusal as a failed challenge. None stops a holder who would sooner lose the deal than serve. Erasure coding across holders, so that any k of n suffice, is the structural answer and is in the issue's list.

### The first deployable step

The issue's fifth item stands out as buildable early: masternodes must pin recent job records for a fixed window. Records are small, they are what a reward is paid against, and a quorum can challenge its own members for them. It gives the network a memory before it has a market.

### Not looked into

Storj, Sia and Arweave were not surveyed for this note, and Filecoin only as far as the one page cited. Dash Platform's storage on its larger masternodes is described in the chain note; how much data it is meant to hold was not checked. The survey in the issue's last item is still to do.

## Assets that point at data (#36)

### What Ravencoin's assets carry

One 32-byte hash, written as an IPFS identifier of the oldest form (a SHA-256 multihash, base58, 46 characters beginning `Qm`), or a transaction ID ([assets code](https://raw.githubusercontent.com/RavenProject/Ravencoin/master/src/assets/assets.cpp)). Evrmore's mainnet is the same. A test release of Evrmore adds a hash that cannot be changed once set and a call to update metadata ([releases](https://github.com/EvrmoreOrg/Evrmore/releases)).

So an asset can name a file added to IPFS the old way, and nothing else: not a job record, which is linked data with another codec, and not anything hashed with another function.

### A full CID

The issue's first item is the fix: carry the CID whole, with its version, codec and hash. A CID is a short prefix and a hash, about 36 bytes for the common case, so the cost in space is a few bytes over what is there.

- **Consensus should check its form and nothing else**: that it parses as a CID, within a length limit. Not that the data exists, which no node can know.
- **A limit** on length, so that an identity hash, which carries data inline, cannot be used to put arbitrary bytes on chain. Refuse the identity hash outright.

### Hints are not identifiers

The issue has this right and it should stay in the design's first sentence: the CID says what the content is; a hint says where a copy may be; a reader checks what it fetched against the CID. A hint that lies costs a reader a failed fetch and nothing else.

That makes hints safe to leave out of consensus rules beyond a size limit and a fee. A gateway URL, an S3 endpoint and bucket, a pinning service: each is a string a wallet may show and try.

### A name in place of a CID

For content meant to change, an asset would carry a name and not a CID. The daemon already has names: a node's ID stands for whichever CID the node last signed, with a record that says until when. An asset pointing at a name is then as trustworthy as the key behind the name, which should be said to whoever holds the asset. Reissuing the asset with a new CID is the on-chain alternative and leaves a history; the name does not.

### Job receipts

The chain note argues that a receipt is better as the reward transaction, which carries the record's CID, than as a named asset. An asset pointing at a record remains useful where someone wants to hold and transfer the right to a result: a rendered film, a trained model. The record already holds, by CID, who ran what and the results' CIDs, and a private job's record holds commitments in their place.

## What can be built now

1. **Challenges from a coordinator to its followers**: a random range of a random blob, answered with a keyed hash, counted in what `blob replicas` reports. No chain needed, and it replaces belief with a check.
2. **A written format for a deal** and for a challenge, as messages, so that the chain work has something exact to carry.
3. **A CID field's rules** written as a short specification: accepted versions and codecs, the length limit, the refusal of inline data.
