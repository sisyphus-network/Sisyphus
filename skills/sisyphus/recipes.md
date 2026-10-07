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
2. For a handful of prompts, call `ask_model` for each.
3. For many, submit each as a `chat` job with `run_job` and `detach`, a few at a time and no more at once than the workers serving that model have task slots, then collect each with `wait_for_job`.

```json
{ "workload": "chat", "detach": true,
  "params": { "model": "llama3.1:8b", "messages": [
    {"role": "system", "content": "Answer with one word: positive, negative or neutral."},
    {"role": "user", "content": "<the text>"} ] } }
```

Write `"model": "llama3.1:8b@<worker's name>"` to keep a conversation on one machine, such as the user's own, when what is in it should not go to another.

## Private data

Pass `private: true` to `store_file` and to `run_job`. The file is sealed before it leaves the machine, and only the workers that run the job are given the key. A private file can be read only by a private job. The key is in `private.key` in the node's data directory: if it is lost, so is everything sealed with it, so tell the user where it is the first time private data is used.

## A long job

1. `run_job` with `detach` returns the job's ID at once.
2. `wait_for_job` follows it for as long as you say and returns it as it stands. Call it again if it has not finished.
3. `job_logs` with `after_seq` set to the last `last_seq` shows only what is new.
4. `cancel_job` stops it.
