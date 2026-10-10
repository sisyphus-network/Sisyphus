# Verification: what to build next

A design note for [#19](https://github.com/sisyphus-network/Sisyphus/issues/19). It is a proposal to argue with, not a decision. The survey it draws on is [how other networks check untrusted workers](verification-prior-art.md).

## Where things stand

A job can have each task run by N workers and take the result N agree on, can verify a random share of its tasks, and puts a worker that was outvoted on probation. Three gaps remain, which the issue names: workers that agree with each other on a wrong answer, work that does not give the same bytes twice, and who bears the cost. A fourth is under them all: a worker has nothing to lose.

## What can be built now, with no chain

These are in order of value for effort. None needs a token, and each is useful in a private pool.

### 1. Results committed before they are compared

Today a worker returns its result and the coordinator compares. The coordinator is trusted, so one worker cannot see another's answer through it. That stops being true the moment workers can see each other's results by any route: a shared store, a log, a colluding operator. iExec and Bittensor both have results committed, as a hash, before any is revealed.

In Sisyphus the coordinator already holds results apart, so the cheap form of this is a rule and a test, not a protocol: a task's result and the blobs it stored are never visible to another worker of the same task until the task is settled. The blobs are the leak today, since any member can fetch a blob by CID and a private job's workers share its key. Worth checking and writing down before building anything else.

### 2. Workers chosen so that agreement means something

Two workers run by one person agree with each other by construction. The coordinator picks a task's workers by free slots. It could also refuse to count as independent any two workers that share an owner, an address, or a `/24`, and say so in the job's events when a pool is too small to find N that differ. BOINC has an option for one result per user for each job. This is what makes replication worth its cost in a pool with strangers in it, and it is a scheduling change.

It does not stop two different people who have agreed to cheat. Only cost does that; see stakes below.

### 3. Tasks whose answer is known

*Built, in a simpler form than proposed below: `--audit-share` has the coordinating node run a random share of tasks again itself and take its own result where a worker's differs. It does not yet reuse answers from earlier runs.*

Truebit makes one task in a thousand wrong on purpose so that checkers have something to find. The mirror of that fits Sisyphus better: the coordinator slips in a task it already knows the answer to, because it or a trusted worker ran it before, and a worker that returns anything else is caught without a second worker being paid. For `primes` and `wordcount` the coordinator can compute a small task itself. For containers, a task seen before with the same image, command and input has a known answer if the work is deterministic.

This catches a worker cheating alone or with others, since no one knows which tasks are known. It costs the tasks themselves and nothing more.

### 4. Fewer checks for a good record

The standing added in #159 only ever adds checks. BOINC's adaptive replication does the other half: a worker with a long unbroken record is sent work unchecked with a probability that grows with the record, and one fault resets it. That brings the cost of verifying from N times down towards a few per cent. The known hole is a worker that builds a record and then cheats. Known-answer tasks close most of it, since the chance of being caught never falls to nothing.

### 5. Work that is not the same twice

Three kinds of work are in the daemon that verification cannot touch today: models (`chat`, `prompts`, `embed`), `transcode`, and containers that use the time or random numbers.

- **Embeddings** are vectors. Two honest runs of one model on different hardware give vectors that differ in the last places. Comparing by cosine distance within a small tolerance is a validator in BOINC's sense, and is the easy one.
- **Prompts with a fixed seed and temperature zero** are often, not always, the same on the same hardware and build. BOINC's answer is to verify within a class of like machines. Workers already report their hardware; a job could ask that its N workers be of one class. Where even that fails, an honest job is failed for no fault of a worker, so this needs measuring on real machines before it is offered. It waits for hardware.
- **Transcoding** gives different bytes from different encoders. Livepeer gave up exact comparison and compares perceptual hashes. A cheaper first step that catches the lazy cheat: check that the output is a valid stream of the asked length, size and frame count. That is a check of form, not of quality, and should be called that.
- **Chat that a person reads** has no ground truth. Render's answer is that the person judges. For Sisyphus that is the planner's user already.

The honest summary: approximate comparison is buildable for embeddings now, structural checks for transcoding now, and the rest wants either real hardware to measure on or trusted hardware.

## What needs a chain, or at least money

### Stakes

Every scheme above makes cheating likelier to be caught. None makes it cost anything. Proof of Sampling gives the relation: a worker is honest when the chance of a check, times what it would lose, exceeds what it would gain. With nothing to lose, no chance of a check is enough for a worker that does not mind being removed.

In a private pool removal is the stake: the owner knows the worker's owner. In an open network a stake has to be money, held where the worker cannot withdraw it faster than a fault can be proved. That is the first real job for a chain, and the reason this note stops here on it; see [the chain note](chain-design.md).

### Who pays for redundancy

In a private pool, nobody: the capacity is the owner's. In a market the survey found two answers. The requester pays for each extra run, which is honest and makes verified work N times the price. Or new coins pay, which hides the cost in inflation. A third follows from stakes: a cheat's forfeited stake pays the workers who caught it, which funds checking from the people it is aimed at, though only when there are cheats.

The proposal for V0 and after: the requester chooses and pays, as `--verify` and `--verify-share` already let them choose, and the price of a job is its tasks times the copies run. Nothing new to build until there is a price.

### Trusted hardware

iExec moved from replication to trusted hardware. It answers two questions at once, correctness and keeping the data from the worker, which nothing else here does. It ties the network to particular processors and their makers, and to attestation services that have been broken before. Worth a separate note when someone has such a machine; not a V0 concern.

## Proposed order

1. Write down and test that a task's results are not visible between its workers before it settles (1).
2. Count workers as independent only when they differ in owner and address (2).
3. Known-answer tasks for `primes`, `wordcount` and repeated deterministic containers (3).
4. Approximate comparison for `embed`, and a check of form for `transcode` (5).
5. Fewer checks for a good record, once 3 is in (4).
6. Stakes, with the chain.

Items 1 to 5 can be done on one machine with the tests the project already has.
