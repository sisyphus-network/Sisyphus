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

Write `"model": "llama3.1:8b@<worker's name>"` to keep the prompts on one machine, such as the user's own, when what is in them should not go to another.

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

Pass `private: true` to `store_file` and to `run_job`. The file is sealed before it leaves the machine, and only the workers that run the job are given the key. A private file can be read only by a private job. The key is in `private.key` in the node's data directory: if it is lost, so is everything sealed with it, so tell the user where it is the first time private data is used.

## A long job

1. `run_job` with `detach` returns the job's ID at once.
2. `wait_for_job` follows it for as long as you say and returns it as it stands. Call it again if it has not finished.
3. `job_logs` with `after_seq` set to the last `last_seq` shows only what is new.
4. `cancel_job` stops it.
