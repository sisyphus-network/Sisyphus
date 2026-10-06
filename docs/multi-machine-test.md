# Testing a pool across machines

Everything in the test suite runs on one machine. This is the guide for the test that cannot: a pool spread over two or three real machines, which is the exit test for V0 ([#18](https://github.com/sisyphus-network/Sisyphus/issues/18)). It takes about an hour. Work through it in order and write down what happens at each ✎.

## What you need

- **Two or three machines** that can reach each other over a network. One will be the coordinator; call the others workers. Mixed hardware is good; at least one with many cores makes the timings clearer.
- **`sisyphusd` on each.** Build it with `make build` on a machine with Go and copy `bin/sisyphusd` across, or cross-build with `scripts/build-release.sh v0.0.0-test dist` and take the binary for each machine's system from `dist/`. Check with `sisyphusd version`.
- **One port open on the coordinator**: TCP 7700, from the workers. Nothing needs opening on the workers, and nothing more for the Kubo part.
- For the Kubo part, **`ipfs` (Kubo) installed on every machine**, the same version if possible.
- This repository's `examples/` directory on the machine you will submit jobs from.

Throughout, `COORD` stands for the coordinator's address as the workers see it, such as `192.168.1.10`.

✎ Record each machine's system, processor, core count, and the `sisyphusd` and `ipfs` versions.

## Part 1: a plain pool

### 1. Start the coordinator

On the coordinator machine:

```sh
sisyphusd run --listen 0.0.0.0:7700 --name coordinator
```

It logs its node ID and `coordinator listening`. By default it is also a worker; add `--role coordinator` if you want that machine to run no tasks.

In another terminal on the same machine:

```sh
sisyphusd id          # the coordinator's ID
sisyphusd nodes       # itself, if it is also a worker
```

### 2. Add the workers

For **each** worker, on the coordinator:

```sh
sisyphusd pool invite
```

and on the worker machine, with the invitation that printed:

```sh
sisyphusd run --role worker --coordinator COORD:7700 --join <invitation> --name <a-name>
```

It logs `joined pool`. Back on the coordinator:

```sh
sisyphusd nodes           # every worker, with its cores and task slots
sisyphusd pool members    # every worker's ID, as a worker
```

✎ Did each worker join first time? If not, what was the message? See [Troubleshooting](#troubleshooting).

**Check that the invitation really was single-use:** try to start a second worker, with a different `--data-dir`, using an invitation you have already used. It must be refused.

### 3. Run jobs

On the coordinator machine, from this repository:

```sh
examples/primes.sh 4000000000
```

✎ Record both times and the count. The count must be the same on both lines. The split run should be faster by roughly the number of worker slots in use.

```sh
examples/wordcount.sh "$(examples/big-file.sh 200)"     # first run: workers fetch 200 MB
examples/wordcount.sh "$(examples/big-file.sh 200)"     # second run: workers have it cached
```

✎ Record both times. The first includes moving the file to each worker over your network; the second should not. Both must print the same `output` CID.

Watch a job as it runs, to see tasks land on different machines:

```sh
sisyphusd job submit --tasks 24 --params '{"from":0,"to":20000000000}'
```

✎ Did tasks run on every worker?

### 4. Workers fetching from each other

Workers do this with nothing opened and no flags: each can be reached through the coordinator, and two on the same network then connect directly.

Start one worker with `-v`. Run `wordcount` on a new big file with that worker stopped, start it, and run the job again.

✎ The late worker's log should have a line for the file like

```
msg="fetched a blob from a fellow worker" cid=<the file's CID> node=<another worker's ID> at=p2p
```

✎ If the workers are on one network, the second run should be about as fast as step 3's first. If they are on different private networks the data goes by way of the coordinator; note the time, and whether it improves on later runs, which would mean the two managed a direct connection.

A worker with a port open to the others can be made a full node with `--relay 0.0.0.0:7702`: it then relays between the others as the coordinator does, and `sisyphusd nodes` shows what it has carried under `RELAYED`.

✎ With a full node running, repeat the run above with a new file. Does its `RELAYED` count go up? If it stays at zero the two workers went through the coordinator or connected directly, both of which are fine.

It can also be given `--serve 0.0.0.0:7701 --advertise <this-worker's-address>:7701`. The others then fetch from it at that address, and the log line ends `at=<that address>`.

### 5. Break things

Each of these has a right answer. Note any that go otherwise.

| Do this | What should happen |
| --- | --- |
| Start the long job above, then press Ctrl-C on one worker while it runs. | Its tasks show as lost and run again elsewhere at `attempt 2`. The job finishes with the same count. |
| Start that worker again, with the same command. | It rejoins. The invitation, already used, is ignored. |
| Stop a worker, start it again, run `wordcount` on the same big file. | No slower than the cached run: the worker's cache survived. |
| Unplug a worker's network, or suspend the machine, mid-job. | Within about fifteen seconds the coordinator logs it disconnected and its tasks move. |
| On the coordinator: `sisyphusd pool remove <worker-id>`. | That worker's process exits saying it was removed. `sisyphusd nodes` no longer lists it. Restarting it is refused. |
| Stop the coordinator mid-job and start it again. | Workers reconnect by themselves within half a minute and the job carries on: tasks that had finished are not run again, and the ones that were running start over without it counting against them. `sisyphusd job get <job-id>` shows it, and finished jobs from before the restart too. Stored files and pins are still there (`sisyphusd blob pins`). |

### 6. Run a private job

```sh
examples/private-wordcount.sh "$(examples/big-file.sh 50)"
```

It seals the file with a key before storing it, runs the job with the key, and shows the first bytes of the stored result, which should be `SISYENC1` followed by noise, and then the table, unsealed.

✎ On a worker, look in its cache (`cache/blocks` in its data directory) for a word you know is in the file: `grep -rl boulder ~/.sisyphus/cache/blocks`. It must find nothing.

### 7. Use the pool from another machine

On the coordinator:

```sh
sisyphusd pool invite --role client
```

On a machine that is not part of the pool:

```sh
sisyphusd pool join --addr COORD:7700 <invitation>
ADDR=COORD:7700 examples/wordcount.sh
```

✎ Did it work? Then confirm what a client may not do: `sisyphusd pool invite --addr COORD:7700` from that machine must be refused.

**Check that strangers are kept out:** from a machine that has not joined, or with `--data-dir` pointing at an empty directory, `sisyphusd nodes --addr COORD:7700` must fail.

## Part 2: with Kubo and a private IPFS network

Stop every node. Either use fresh data directories (`--data-dir`) or accept that data stored in part 1 will not be visible: switching a node to Kubo does not move its data.

### 1. Start the pool

Coordinator:

```sh
sisyphusd run --kubo --listen 0.0.0.0:7700 --name coordinator
```

Each worker, with a fresh invitation if it uses a fresh data directory:

```sh
sisyphusd run --kubo --role worker --coordinator COORD:7700 --join <invitation> --name <a-name>
```

### 2. Check the private network

On any node, with `IPFS_PATH` set to `ipfs` inside that node's data directory (`$(sisyphusd data-dir)/ipfs` unless you gave `--data-dir`):

```sh
IPFS_PATH=$(sisyphusd data-dir)/ipfs ipfs id -f '<id>\n'      # equals: sisyphusd id
IPFS_PATH=$(sisyphusd data-dir)/ipfs ipfs swarm peers        # the other nodes of the pool, and nothing else
IPFS_PATH=$(sisyphusd data-dir)/ipfs ipfs bootstrap list     # the pool's own nodes only
```

✎ Does every node list every other? A worker that lists only the coordinator can reach it but not its fellow workers; note which.

Only run `ipfs` commands against a node's repository while the node is up. If its Kubo is down, the `ipfs` command takes the repository's lock itself and the node cannot start Kubo until the command ends.

### 3. Run jobs

```sh
examples/wordcount.sh "$(examples/big-file.sh 200)"
examples/wordcount.sh "$(examples/big-file.sh 200)"
```

✎ Record both times and compare with part 1.

Then confirm the data moved over the private network. On a worker:

```sh
IPFS_PATH=$(sisyphusd data-dir)/ipfs ipfs cat --offline <the input CID that wordcount.sh printed> | wc -c
```

It prints the file's size if the worker's Kubo holds it. And look in each worker's log for

```
the pool's private IPFS network did not supply a blob
```

✎ That warning must not appear. If it does, the worker's Kubo could not get the data from the others and fell back to the coordinator; the job still succeeds, but the private network is not working between those machines. Each worker's log says near the top how its Kubo reaches the coordinator's: "through the coordinator's own port", or "directly" if the coordinator was started with `--swarm-port`.

### 4. Remove a member

```sh
sisyphusd pool remove <worker-id>       # on the coordinator
```

The removed worker exits. Within a few seconds each remaining worker logs `moved to the new key of the pool's private IPFS network`.

✎ On a remaining worker, does `ipfs swarm peers` show the coordinator again after a few seconds? Does a job still run?

✎ On the removed machine, start Kubo by hand on its old repository (`IPFS_PATH=$(sisyphusd data-dir)/ipfs LIBP2P_FORCE_PNET=1 ipfs daemon`) and try `ipfs swarm connect` to the coordinator's address from `ipfs id` there. It must fail: that machine holds the old key.

## What to report

Add a comment to [#18](https://github.com/sisyphus-network/Sisyphus/issues/18) with:

- the machines, as recorded at the start;
- the times from part 1 step 3 and part 2 step 3;
- every row of the "break things" table that did not go as described, with the log lines from around it;
- every ✎ where the answer was not what the guide said to expect.

Logs go to each node's standard error. Start a node with `-v` to log each task. Kubo's own log is `daemon.log` in the node's `ipfs` directory.

## Troubleshooting

| Message or symptom | Cause |
| --- | --- |
| `connection refused`, or a command hangs and then fails | The coordinator is not listening on an address the other machine can reach (`--listen 0.0.0.0:7700`), or a firewall is blocking port 7700. |
| `authentication handshake failed: connected to node X, expected Y` | The machine running the command does not know the node at that address. On a worker, it needs `--join` with an invitation. For a command, it has not joined (`pool join`), or `--data-dir` points somewhere other than where it joined from. It also appears if the coordinator's data directory was replaced, giving it a new identity. |
| `has not been admitted to this node` | This machine knows the node but is not on its list: it was removed, or the coordinator's `node.db` was lost. Invite it again. |
| `invitation is unknown, already used or expired` | Invitations work once and last an hour. Issue another. |
| `this node has not joined a coordinator at ...` | A worker started without `--join` in a data directory that never joined that address. The address must match the one it joined with exactly. |
| `is admitted as a worker, which may not call ...` | A command is being run with a worker's key. Commands that manage the pool run on the coordinator's machine; to submit jobs from elsewhere, join as a client. |
| `in use by another process (nodes sharing a machine each need their own --data-dir)` | Two nodes on one machine are using the same data directory. |
| `kubo: ... executable file not found` | `ipfs` is not installed or not on the PATH of the user running `sisyphusd`. |
| `kubo: the daemon stopped while starting: ... someone else has the lock` | Another Kubo, or an `ipfs` command, has that repository open. A node that was killed outright leaves its Kubo running: find it with `pgrep -af 'ipfs daemon'` and stop it. |
| `does not run a private IPFS network` | A worker was started with `--kubo` but its coordinator was not. |
| Jobs work but every first run is slow, and workers log the "did not supply a blob" warning | The workers' Kubo daemons are not connected to the coordinator's. On a worker, `IPFS_PATH=<data-dir>/ipfs ipfs swarm peers` should list the coordinator's ID; if it does not, look for "a tunnel failed" in the worker's log. |
