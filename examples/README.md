# Examples

Things to run against a pool, and a pool to run them against.

| File | What it is |
| --- | --- |
| `local-pool.sh` | Starts a coordinator and two workers on this machine and leaves them running. `--kubo` gives each node Kubo and the pool a private IPFS network; `--containers` lets the workers run container jobs. |
| `tmux-pool.sh` | The same in one terminal: the pool in one tmux pane, a shell ready to use it in another. |
| `wordcount.sh` | Stores a file on the pool, counts its words across the workers, and prints the most frequent. |
| `private-wordcount.sh` | The same, sealed: the file and the result are encrypted with a key that only you and the job's workers hold. |
| `primes.sh` | Counts primes below a number, split across the pool and then as one task, to compare. |
| `container.sh` | Runs a container image on the pool, one copy for each task, and prints what each printed. |
| `transcode.sh` | Re-encodes a video on the pool: stored, encoded in stretches by the workers, fetched. |
| `render.sh` | Renders a picture in strips, a container task for each, and puts it back together. |
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
export DATA_DIR=$(sisyphusd data-dir)   # your key, and the record of having joined
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

## Running a container

`examples/container.sh` runs a container image on the pool, one copy for each task, and prints what each copy printed. At least one worker must have been started with `--containers`, which needs Docker.

```sh
examples/container.sh                                   # three copies of a small image
examples/container.sh 8 python:3-slim python -c 'import os; print(int(os.environ["SISYPHUS_TASK_INDEX"]) ** 2)'
```

## Transcoding a video

`examples/transcode.sh` is the first example that does a job people have. It stores a video, has the pool encode it in stretches, and fetches the result.

```sh
examples/local-pool.sh --containers          # in one terminal
examples/transcode.sh holiday.mov 720        # in another: to 720 pixels high
PRESET=slow QUALITY=20 SEGMENTS=4 examples/transcode.sh holiday.mov
```

The first run on a machine builds the image ffmpeg runs in, which takes about a minute. The result is a `.ts` file; the script prints the command that puts it in an MP4.

## Rendering a picture across the pool

`examples/render.sh` is a whole job of the kind the pool is for: one program run over parts of something. It renders the Mandelbrot set in strips, a container task for each, and puts the strips back together into `mandelbrot.pgm`.

```sh
examples/local-pool.sh --containers      # in one terminal: a pool whose workers run containers
examples/render.sh                       # in another: 8 strips of a 1200 by 800 picture
examples/render.sh 16 2400 1600 big.pgm  # more strips, a bigger picture
```

Each task works out which strip is its own from `SISYPHUS_TASK_INDEX` and `SISYPHUS_TASK_COUNT`, writes its rows to `/output/rows`, and logs how far it has got, which `job logs` shows live. The job's result names each strip's rows by CID, and the script fetches them in order. Swap the program and the image and the same script shape runs frames of a video, files of a dataset, or seeds of a simulation.

## A backend for the desktop app

`examples/desktop-backend.sh` starts three nodes for the desktop app to be built against: the one it talks to, a worker of its pool, and a node it finds by itself. See [`docs/desktop-backend.md`](../docs/desktop-backend.md).
