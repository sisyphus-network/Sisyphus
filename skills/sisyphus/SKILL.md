---
name: sisyphus
description: Run computations on a Sisyphus pool, a group of machines that people pool to run jobs. Use when the user wants work split across their pool's machines (rendering, transcoding video, running a container image or a script over many inputs, a parameter sweep, counting or crunching that parallelises), asks what their pool or its workers are doing, or mentions Sisyphus, sisyphusd, a pool, workers or a coordinator.
---

# Sisyphus

Sisyphus is a network of computers that people pool to run computations. One node, the coordinator, takes jobs; the pool's workers run them. A **job** is one workload run over an input and split into **tasks**, which the workers run at the same time. You reach a pool through a node on this machine, `sisyphusd`.

## Two ways in

**The tools, if you have them.** If tools named `pool_status`, `list_workloads`, `run_job`, `get_job`, `list_jobs`, `cancel_job`, `job_logs`, `store_file`, `fetch_file`, `list_files` and `remove_file` are available, the node is connected as an MCP server: use them. `wait_for_job` follows a job you did not wait for, `fetch_outputs` brings back every file a job stored, and `save_result` writes a result too long to show to a file. `compare_texts` says which texts mean most alike without the vectors passing through you. Beside them are `ask_model` (a model on one of the pool's workers answers a prompt), `ask_planner`, `list_chats` and `get_chat` (the node's own planner), `list_peers` and `list_members` (who the node knows and who is in its pool), and `get_model` and `list_models` (what the planner plans with).

**Which tools you have is the user's choice.** The server may have been started read-only, in which case nothing can be run or stored. It keeps `store_file` and `fetch_file` to one directory, usually the one you are working in, and refuses paths outside it: copy a file in rather than ask for the limit to be lifted. It may run only certain container images. The tools that change the node itself (`create_invitation`, `remove_member`, `join_pool`, `connect_peer`, `set_peer_trust`, `set_model`, `pull_model`) are there only if the user started it with `--admin`. If a tool you need is missing, say which flag would add it and let the user decide. If they are not and the user wants them, the server is `sisyphusd mcp` (for Claude Code: `claude mcp add sisyphus -- sisyphusd mcp`), and it needs the node running with `--api-listen 127.0.0.1:50051`.

**The command line, otherwise.** The same things, as `sisyphusd` commands:

| To | Run |
| --- | --- |
| See the workers | `sisyphusd nodes` |
| Store a file, getting its content ID | `sisyphusd blob put <file>` |
| Run a job and wait for it | `sisyphusd job submit --workload <name> --params '<json>'` |
| Look at a job, or its log | `sisyphusd job get <id>`, `sisyphusd job logs <id>` |
| Stop a job | `sisyphusd job cancel <id>` |
| See who ran each task of a finished job, and check the job against its record | `sisyphusd job record --verify <id>` |
| Fetch a stored file | `sisyphusd blob get <cid> > <file>` |

`--addr <host:port>` names a coordinator other than the one on this machine. `sisyphusd <command> -h` lists a command's flags.

## How to work

[`recipes.md`](recipes.md), beside this file, has worked shapes for the common jobs: one program over many inputs, a parameter sweep, transcoding, many prompts to the pool's models, private data, a long job. Read it before building any of those from scratch.

1. **Look first.** `pool_status` says which workers are connected and how many tasks each runs at once. `list_workloads` says what can be run and gives each workload's parameters; it is the authority, and what follows here is a summary. A workload that no connected worker runs will wait for one, so say so rather than submit it.
2. **Files go in by content ID.** A workload that reads a file takes the ID `store_file` returned, not a path. Storing the same content twice gives the same ID.
3. **Run the job.** `run_job` waits for the result. For something long, pass `detach` and follow it with `wait_for_job` and `job_logs`. Leave `tasks` out to get one task for each free worker slot, which is usually right.
4. **Results come back as JSON**, and anything large as content IDs inside it or under `stored_outputs`. Fetch one with `fetch_file`, giving a `path` unless the file is short text, or all of them into a directory with `fetch_outputs`.
5. **Report what ran**: the job's ID, how long it took and on how many tasks. Do not present a result you did not get from a job as though the pool computed it.

## The workloads

- **`container`** runs any container image, one copy for each task. This is the one to reach for when the user's work is their own program. Parameters: `image`, `command` (a list), `tasks`, and optionally `input` (a content ID, given to every task at `/input/data`), `network` (off unless true) and `gpus`. Each task sees `SISYPHUS_TASK_INDEX` (from 0) and `SISYPHUS_TASK_COUNT` in its environment and uses them to pick its own share of the work. What a task prints, and any file it writes to `/output`, come back as content IDs, one set for each task.
- **`transcode`** re-encodes a stored video in stretches across the workers. Parameters: `input`, and optionally `height`, `crf`, `preset`, `segments`. The result is an MPEG transport stream; `ffmpeg -i out.ts -c copy out.mp4` rewraps it without re-encoding.
- **`chat`** has a language model that one of the pool's workers serves continue a conversation. Parameters are a chat request in OpenAI's form: `{"model": "<name>", "messages": [...]}`; the result is the reply in the same form. `pool_status` shows which models each worker serves: name the model exactly as it is shown there, and a request for one that no worker serves waits. Write `<model>@<worker's name>` to have that worker and no other run it, which is how to keep a conversation on a machine the user trusts. Use it for many prompts at once, a job for each with `detach`, when the user wants the pool's machines to do the thinking. The worker that runs it sees the conversation.
- **`prompts`** has a model the pool serves answer many prompts, shared out among the workers that serve it: `{"model": "<name>", "prompts": ["..."], "system": "<optional>", "options": {"temperature": 0}}`, or `"input"` with the content ID of a stored file that has a prompt on each line. The answers come back in the order asked. Use it, not many `chat` jobs, for classifying, summarising or extracting from many texts.
- **`embed`** turns up to 128 texts into vectors with an embedding model the pool serves: `{"model": "<name>", "input": ["..."]}`.
- **`graph`** runs several jobs as one, each a step that may wait for others and use what they produced: `{"steps": [{"name": "a", "workload": "...", "params": {...}}, {"name": "b", "workload": "...", "params": {"input": "${a.result.output}"}}]}`. A string in a step's parameters may refer to an earlier step as `${name.result}`, `${name.result.<field>}`, `${name.outputs.0}` or `${name.job_id}`; a step waits for every step it refers to, and for any it lists under `after`; steps that wait for nothing run side by side. Use it when one job's output is the next one's input, so that you need not wait on each in turn. If a step fails, the graph fails and says which.
- **`wordcount`** counts the words of a stored text file. Parameters: `input`.
- **`primes`** counts primes in a range: `{"from": 0, "to": 1000000}`. It computes nothing of use and is the quickest way to check that a pool works.

`container` and `transcode` need a worker started with `--containers`, which needs Docker on that machine.

### Splitting your own work with `container`

The shape that fits most work: write the program so that task *i* of *n* does the *i*-th share, and have each task write its share to `/output`.

```json
{
  "image": "python:3-slim",
  "command": ["python", "-c", "import os; i=int(os.environ['SISYPHUS_TASK_INDEX']); n=int(os.environ['SISYPHUS_TASK_COUNT']); open('/output/part','w').write(str(sum(x*x for x in range(i, 10**7, n))))"],
  "tasks": 8
}
```

The result lists each task's `stdout` and `files` by content ID, in task order; fetch them and put them together. Tasks have no network unless `network` is true, so an image must already hold what the program needs, or the job must ask for the network to install it.

## Things that go wrong

- **"no worker runs it" or a job that stays pending**: no connected worker offers that workload. Check `pool_status`; for container jobs a worker needs `--containers`.
- **A task fails and is retried**: `job_logs` shows each attempt and what the task printed. A container that exits with an error fails its task.
- **The first container or transcode job on a worker is slow**: the worker is pulling or building the image.
- **Private data.** With `private` on `store_file` and `run_job`, what is stored is sealed with the node's key, and only the workers that run the job are given it. Use it when the user says the data is sensitive; a private file can only be read by a private job.
- **Workers that may be wrong.** With `verify` on `run_job` (`--verify N` on the command line), each task is run by that many different workers and its result is taken only when they return the same one; a worker that returns something else is outvoted and named in `job_logs`. It costs that many times the work. Use it only for work that gives the same result every time it is run, such as `primes`, `wordcount` or a deterministic container: jobs on models, and anything that uses the time or random numbers, differ from run to run and fail with "could not be verified". It cannot be combined with `private`, and it is refused if fewer workers that could take the job are connected.
- **"is outside" from `store_file` or `fetch_file`**: the path is not in the directory the server keeps to. Use a path inside it.
- **"there is a file at ... already"**: `fetch_file` does not write over a file unless given `overwrite`. Fetch to a new path unless the user wants the old one replaced.
- **"the result is too long to show"**: `save_result` writes it to a file; read the file with a program, not into the conversation.
- **A job on a model is slow**: a large model on a machine with no graphics card answers a few words a second. Prefer the smallest model the pool serves that can do the work, and say how long it took.
- **Permission denied from the tools**: the node's token could not be read. The server must be started as the user who runs the node, with the same `--data-dir`.

## What not to do

- Do not start, stop or reconfigure the node, invite or remove members, or change which model it plans with unless the user asks for exactly that.
- Do not run a container image the user has not named or agreed to: it runs on other people's machines.
- Do not submit a large job to find out whether something works. Try it with one or two tasks first.
