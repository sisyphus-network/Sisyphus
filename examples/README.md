# Examples

Things to run against a pool, and a pool to run them against.

| File | What it is |
| --- | --- |
| `local-pool.sh` | Starts a coordinator and two workers on this machine and leaves them running. `--kubo` gives each node Kubo and the pool a private IPFS network. |
| `tmux-pool.sh` | The same in one terminal: the pool in one tmux pane, a shell ready to use it in another. |
| `wordcount.sh` | Stores a file on the pool, counts its words across the workers, and prints the most frequent. |
| `private-wordcount.sh` | The same, sealed: the file and the result are encrypted with a key that only you and the job's workers hold. |
| `primes.sh` | Counts primes below a number, split across the pool and then as one task, to compare. |
| `big-file.sh` | Makes a large text file to time jobs with. |
| `boulder.txt` | A short sample text, the default input for `wordcount.sh`. |
| `common.sh` | Shared by the scripts; not run directly. |

## On one machine

In one terminal, with tmux:

```sh
examples/tmux-pool.sh
```

The left pane runs the pool; the right is a shell already pointed at it. Leave with `Ctrl-b d`, which keeps it running, or end it and the pool with `tmux kill-session -t sisyphus`.

Or in two terminals:

```sh
examples/local-pool.sh            # terminal 1; Ctrl-C stops it
```

It prints two variables to set in the other terminal. Either way, then:

```sh
examples/wordcount.sh                               # the sample text
examples/wordcount.sh path/to/any/file.txt 20       # your own file, top 20 words
examples/private-wordcount.sh                       # sealed with a key; shows what the pool holds
examples/primes.sh                                  # split versus single, primes below 2 billion
examples/wordcount.sh "$(examples/big-file.sh 50)"  # a 50 MB file
```

The pool's keys, joined workers and stored data are kept in `./sisyphus-pool`, so a second `local-pool.sh` picks up where the first left off. Delete that directory to start afresh.

## Against a pool on other machines

The scripts talk to whatever node `ADDR` names, as the key in `DATA_DIR`:

```sh
export ADDR=192.168.1.10:7700     # the coordinator
export DATA_DIR=~/.sisyphus       # your key, and the record of having joined
examples/wordcount.sh
```

On the coordinator's own machine the defaults already work. From anywhere else, join first, as a client:

```sh
sisyphusd pool invite --role client                    # on the coordinator; prints an invitation
sisyphusd pool join --addr 192.168.1.10:7700 <invitation>   # on your machine, once
```

[`docs/multi-machine-test.md`](../docs/multi-machine-test.md) walks through setting up such a pool and what to try on it.

## What the workloads are

Both are stand-ins that exercise the network; neither computes anything of value.

- **`primes`** has tiny inputs and outputs, so it shows scheduling and nothing else. Its time should fall as worker slots are added.
- **`wordcount`** moves real data: the input is stored once and fetched by each worker, tasks store their partial counts, and the result is a stored file. Its first run on a pool includes fetching the input; later runs use each worker's cache.

A job's result is the same whichever way it was split, down to the CID of a stored result. If two runs of the same job print different results, something is wrong.
