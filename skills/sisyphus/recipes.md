# Recipes

Worked shapes for common jobs on a Sisyphus pool. Each uses the tools; the command line takes the same parameters as `--params`.

## The same program over many inputs

Put the inputs in one file, store it, and have each task take its share by its index.

1. Write the inputs one to a line, and `store_file` the file. Keep the content ID.
2. `run_job` with `container`:

```json
{
  "image": "python:3-slim",
  "input": "<content ID>",
  "tasks": 8,
  "command": ["python", "-c", "import os\ni=int(os.environ['SISYPHUS_TASK_INDEX']); n=int(os.environ['SISYPHUS_TASK_COUNT'])\nlines=open('/input/data').read().splitlines()\nopen('/output/part','w').write('\\n'.join(l.upper() for l in lines[i::n]))"]
}
```

3. `fetch_outputs` with the job's ID and a directory brings every task's files back, named `tasks-0-files-part`, `tasks-1-files-part` and so on, with what each printed as `tasks-0-stdout`. Put them together in task order.

Task *i* of *n* takes lines *i*, *i+n*, *i+2n* and so on, so every line is done once whatever *n* is. Try it with `"tasks": 2` on a few lines before running the whole file.

## A parameter sweep

No input file: each task works out its own parameters from its index.

```json
{
  "image": "python:3-slim",
  "tasks": 20,
  "command": ["python", "-c", "import os, json\ni=int(os.environ['SISYPHUS_TASK_INDEX'])\nrate=0.01*(i+1)\nprint(json.dumps({'rate': rate, 'score': (1+rate)**30}))"]
}
```

What each task prints comes back as its `stdout`, by content ID, in task order.

## A program that needs packages

Tasks have no network unless asked. Either use an image that already has what the program needs, or allow the network and install first:

```json
{ "image": "python:3-slim", "network": true, "tasks": 4,
  "command": ["sh", "-c", "pip install -q numpy && python -c 'import numpy; print(numpy.__version__)'"] }
```

The command runs as an ordinary user whose home is `/tmp`, so `pip install` puts the package there and Python finds it. What needs root, such as `apt-get install`, fails: that calls for an image with the package in it.

Installing in every task is slow. For anything run more than once, ask the user for an image with the packages in it.

## Transcoding a video

1. `store_file` the video.
2. `run_job` with `transcode`: `{"input": "<content ID>", "height": 720}`. `crf` (quality, lower is better, 23 if left out), `preset` and `segments` are optional.
3. The result names the output's content ID. `fetch_file` it to a path ending `.ts`.
4. `ffmpeg -i out.ts -c copy out.mp4` puts it in an MP4 without encoding again.

The first transcode on a worker builds its image and takes about a minute longer.

## Many prompts to the pool's models

When the user wants the pool's machines to do the thinking: classify, summarise or extract from many texts.

1. `pool_status` shows which models the workers serve. Pick one that is served; say so if none is.
2. For one prompt, `ask_model`.
3. For many, one `prompts` job. The prompts are shared out among the workers that serve the model, and the answers come back in the order asked:

```json
{ "workload": "prompts",
  "params": { "model": "llama3.1:8b",
    "system": "Answer with one word: positive, negative or neutral.",
    "options": {"temperature": 0},
    "prompts": ["I love this.", "This is terrible.", "The meeting is at noon."] } }
```

For more prompts than fit comfortably in a request, write them to a file one to a line, `store_file` it, and give its content ID as `"input"` instead of `"prompts"`. Many long answers come back as a stored file named in the result as `output`, one answer on each line as a JSON string; `fetch_file` it to a path.

Try the system instruction on three or four prompts first. A model answering alone, many times over, repeats whatever it gets wrong.

Write `"model": "llama3.1:8b@<worker's name>"`, or `@<its peer_id>` where other people's workers are in the pool, to keep the prompts on one machine, such as the user's own, when what is in them should not go to another.

## Finding texts that mean alike

`compare_texts` does this in one call: give it the model and the texts, and a `query` to rank them against or none to rank every pair. Reach for `embed` itself only when the vectors are wanted, and then `save_result` them to a file rather than read them: a result of vectors is far too long to show.

`embed` turns texts into vectors with an embedding model the pool serves (look for `embed` in the names `pool_status` lists): `{"model": "nomic-embed-text:latest", "input": ["...", "..."]}`, at most 128 texts to a job. Texts whose vectors point the same way mean alike; compare them by cosine similarity.

## One job's output into the next

A `graph` job runs the steps in order for you, so you need not wait on each and pass its result along by hand.

```json
{ "workload": "graph",
  "params": { "steps": [
    { "name": "encode", "workload": "transcode", "params": {"input": "<content ID>", "height": 480} },
    { "name": "probe", "workload": "container",
      "params": { "image": "alpine:3.20", "input": "${encode.result.output}",
                  "command": ["sh", "-c", "wc -c < /input/data"] } }
  ] } }
```

`${encode.result.output}` is filled in with that field of the `encode` step's result once it has finished, and `probe` waits for it because it refers to it. Steps that refer to nothing run at the same time; list a step's name under another's `"after"` to make it wait without using anything from it. The graph's result lists each step with the ID of the job that carried it out, and each of those is an ordinary job: `get_job` and `job_logs` work on it. Look at one step's result by itself before building a chain on it, since a reference to a field that is not there fails the step.

## Private data

Pass `private: true` to `store_file` and to `run_job`. The file is sealed before it leaves the machine, and the workers that run a job are given the keys to the sealed files that job's parameters name, and to no others. Name every private input in the parameters: a private file a task would reach only through another file is one its worker cannot open. A private file can be read only by a private job. The key is in `private.key` in the node's data directory: if it is lost, so is everything sealed with it, so tell the user where it is the first time private data is used.

## A question about files, for the planner

When the user wants the node's own planner to work something out from files:

1. `store_file` each file, with `private: true` if it is sensitive, and keep the content IDs.
2. `ask_planner` with the question and `files` set to those IDs. The planner is told of the files and runs the jobs it needs; the answer comes back with the IDs of the jobs it ran, each an ordinary job for `get_job` and `job_logs`.
3. Ask again with the `chat_id` returned to carry on. The files stay attached: they need not be given twice.
4. `delete_chat` when the user is done lets the node stop keeping the files for that conversation. A file the user also stored for themselves is still kept.

The planner's model service reads the question, the files' names and IDs, and the results of the jobs it runs. If that service is not on a machine the user trusts, say so before asking about sensitive data; `get_model` shows which service it is.

## A long job

1. `run_job` with `detach` returns the job's ID at once.
2. `wait_for_job` follows it for as long as you say and returns it as it stands. Call it again if it has not finished.
3. `job_logs` with `after_seq` set to the last `last_seq` shows only what is new.
4. `cancel_job` stops it.

## Keeping a result

What a job stores is held for the job for some days after it ends (seven unless the node's owner said otherwise) and then deleted. To keep it longer:

1. `get_job` gives the job's `stored_outputs`. `pin_file` each one to keep, with `ttl_hours` for a time (`24` is a day) or without to keep it until `unpin_file`.
2. Its answer lists every pin on the file, as `list_pins` with the `cid` does: `user` is the pin you placed, `job:<id>` the job's own. The file is kept until the last of them lapses, so a pin makes a file last longer and never shorter: asked to keep a result for a day, say that the job's own pin keeps it for longer anyway.
3. `storage_status` with the `cid` says how many other nodes hold a copy. `kept_by` is `followers` or `cluster` in a pool that keeps copies, and `this node alone` in one that does not: then the file is on one disk, and the user should hear that. A copy takes a moment to arrive, so `copies` may be short just after pinning; ask again before calling it a problem.

A file has one pin of the user's, so pinning again with another `ttl_hours` replaces how long it is kept. `unpin_file` releases it, and the file goes once nothing else holds it.

## A name for a result that changes

A content ID names bytes that never change, so each new result has a new ID. The node's **name**, which is its ID, stands for whichever file it was last pointed at, so that whoever has the name finds the latest.

1. `pin_file` the result: a name does not keep the file it stands for.
2. `publish_name` with the `cid`. The name is not yours to choose: it is the node's ID. It returns the name and `good_until`: the record runs out then (48 hours on, or `lifetime_hours`), and the name stands for nothing until published again.
3. `resolve_name` with the name, or with nothing for this node's own, returns the `cid` it stands for now, checked against the name's node's signature.

A node has one name, and publishing replaces what it stood for. Ask before pointing it somewhere new.

## Proof of what ran

When a job is over, the coordinator writes its history as a record: what was asked, which worker ran each task, and the content IDs of its inputs and outputs, all named by one ID that changes if anything in the history does.

- `get_job_record` with the job's ID returns `record_cid` and the record's `nodes`. In them `{"/": "<content ID>"}` is a link and `{"/": {"bytes": "<base64>"}}` is bytes, such as the parameters and the result.
- With `verify: true` the node works the record out again from the job as it holds it now, and answers `verified: true`, or fails if that gives another ID. Quote `record_cid` when the user wants something to hold the pool to.
- A private job's record holds commitments in place of what was sealed. `check_commitments: true` verifies and also checks them with the node's own key; it says so if there are none to check.

The record outlives the job's data: its links name outputs the node may no longer hold unless they were pinned.
