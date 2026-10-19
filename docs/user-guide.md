# Using Sisyphus

A walk through Sisyphus as someone using it sees it: start a node, ask it a question, run work on a pool, keep files, and add a second machine. The pictures are of the desktop app; each section ends with the command that does the same from a terminal, because the app and the command line talk to the same node.

The pictures were taken on 10 October 2026 from a development build, against the three-node example backend (`examples/desktop-backend.sh`) planning with a small local model. The app is under active development, so details will have moved.

## What you are looking at

A **node** is one running copy of `sisyphusd`. A **pool** is a group of nodes that work together: one coordinates, and the others, the **workers**, run tasks for it. The **planner** is a language model that a node can ask to turn a question into work for the pool. The desktop app is a window onto one node on your own machine.

Nothing here costs anything or leaves your machines. There is no token and no public network yet; see the [project status](../README.md#project-status).

## Starting

You need the node and, if you want the window, the app.

```sh
make build                                        # builds bin/sisyphusd; needs Go
bin/sisyphusd run --api-listen 127.0.0.1:50051    # a node that is its own coordinator and worker
```

```sh
cd apps/sisyphus
npm install
npm run dev                                       # the desktop app, connected to that node
```

If no node is answering on `127.0.0.1:50051`, the app starts one itself and stops it when you quit. A node it finds already running is used and left alone.

### Installing a packaged build

Without building anything, take a package of the app with the node inside. A release carries them beside the `sisyphusd` binaries, and every run of the `Desktop packaging` workflow on GitHub attaches the same files as development builds.

| System | File | What to do with it |
|---|---|---|
| Windows | `Sisyphus-<version>-win-x64-setup.exe`, or `-win-arm64-setup.exe` on an Arm laptop | Run it. It installs for you alone, asks where, and puts Sisyphus in the Start menu and on the desktop. |
| macOS | `Sisyphus-<version>-mac-arm64.dmg` on Apple silicon, `-mac-x64.dmg` on an Intel Mac | Open it and drag Sisyphus onto Applications. |
| Linux | `Sisyphus-<version>-linux-x86_64.AppImage`, or `-linux-arm64.AppImage` on an Arm machine | `chmod +x` it and run it. |

Each also comes as a plain `.zip` or `.tar.gz` to unpack and run in place.

### The warnings on first run

These packages are not signed by a publisher that Windows or macOS knows, because that takes a paid certificate the project does not have yet. Both systems say so the first time, in words that sound worse than the fact. Check what you downloaded against the release's `SHA256SUMS` first (`Get-FileHash <file>` in PowerShell, `shasum -a 256 <file>` on macOS, `sha256sum <file>` on Linux); a file that matches is the one the project built.

- **Windows, "Windows protected your PC".** Choose **More info**, then **Run anyway**. The publisher is shown as unknown: Windows takes that name only from a signature. Once installed, the app's details (its Properties, and Apps and Features) name it as made by Sisyphus Network, rvnminers-A-and-N and excho0.
- **Windows, the firewall asking about "Sisyphus node".** That is the node inside the app asking to accept connections. Allow it on private networks if other machines are to join yours; a node used alone works without.
- **macOS, "Sisyphus cannot be opened" or "the developer cannot be verified".** Close the message, open **System Settings**, **Privacy & Security**, scroll to the line about Sisyphus and choose **Open Anyway**. On older versions of macOS, right-click the app and choose **Open** instead.
- **macOS, "Sisyphus is damaged and can't be opened".** It is not damaged: macOS says this of an unsigned app that came from a browser. Take the download mark off it with `xattr -dr com.apple.quarantine /Applications/Sisyphus.app` and open it again.
- **Linux** says nothing.

These steps are what each system does with any unsigned program. Nobody on the project has yet walked through them with a released package, so tell us where they are wrong.

For a pool with something in it straight away, `examples/desktop-backend.sh` starts three nodes on one machine and prints how to point the app at them.

## The workspace

![The workspace: the planner's chat on the left, the globe of known nodes on the right](images/workspace-start.png)

The app opens on the workspace. On the left is the conversation with your node's planner. On the right is a globe showing this node and the nodes it knows of, with a count of those it is connected to. The handles between the panes resize them; the clock at the top left opens earlier conversations.

On a narrow window the two become one view with a switch at the bottom. On a machine with no WebGL the globe is left out and the chat takes the room.

## Giving the planner a model

The planner needs a language model before it can answer. Any of these will do:

- a model served by [Ollama](https://ollama.com) on your machine, which costs nothing;
- any service that speaks as OpenAI does, including a local one;
- Claude, with an API key.

Set it in **Settings → Model provider**, or from a terminal:

```sh
bin/sisyphusd model set --model llama3.1:8b       # a model your Ollama already has
```

A small local model works for simple questions and makes mistakes on harder ones; the pool's arithmetic is exact either way, and the steps the planner took are there to check.

## Asking a question

![A question answered: the planner ran a job on the pool and answered from its result](images/workspace-steps.png)

Type a question and press Enter. If answering needs computing, the planner writes a job, the pool runs it, and the planner answers from the result. **Finished planning** opens to show what it did: here, one job, with its ID and the result the pool returned. The answer above it comes from that result, not from the model's memory.

The **+** beside the box attaches a file to the question. An attached file is stored sealed, and the job that reads it is private.

From a terminal:

```sh
bin/sisyphusd ask "How many primes are there below 100000?"
bin/sisyphusd ask --chat <id> "And below a million?"      # carry the conversation on
```

## The node

![Node overview: identity, connections, and each worker with its hardware and what it can run](images/node-overview.png)

**Node overview** is the machine's own view of the network: its identity, how many nodes it is connected to, and every worker of its pool with its processor, memory, free task slots and the kinds of work it runs. Further down are the node's addresses and the nodes it has found, each of which you can trust for compute or not. Trusting one admits it to your pool as a worker.

```sh
bin/sisyphusd id
bin/sisyphusd nodes
```

## Running a job yourself

![Operations, Jobs: the form a job is submitted with](images/operations-jobs.png)

**Operations → Jobs** submits work without the planner. Choose the workload, give its parameters as JSON, and say how the work is to be spread: **Distributed** splits it across the pool's workers. `Tasks` at 0 lets the node decide how many pieces to cut it into.

![A finished job, with its result opened](images/operations-job-done.png)

Jobs appear below the form as they run, newest first, each with its state, its progress, its log and its result.

**Private job** seals what the job reads and writes. Workers are given the keys for that job's data and nothing more. The key itself, `private.key` in the node's data directory, is the only way back to sealed data: if it is lost, so is the data.

```sh
bin/sisyphusd job submit --params '{"from":0,"to":100000}'
bin/sisyphusd job get <job-id>
bin/sisyphusd job logs <job-id>
```

## Files

![Operations, Files: two stored files, one of them private](images/operations-files.png)

**Operations → Files** stores files in the pool for jobs to read. Each is named by a content identifier, the long string under its name, which is a fingerprint of its bytes: the same file always has the same one, and a file cannot be swapped for another under it. Tick **Encrypt as a private file** to seal a file before it is stored; only private jobs can read it.

```sh
bin/sisyphusd blob put notes.txt
bin/sisyphusd job submit --workload wordcount --params '{"input":"<cid>"}'

bin/sisyphusd key new job.key                                 # a key of your own, for sealed files
bin/sisyphusd blob put --key-file job.key notes.txt
bin/sisyphusd job submit --workload wordcount --key-file job.key --params '{"input":"<sealed-cid>"}'
```

## Adding another machine

![Operations, Pool: issuing an invitation and the pool's members](images/operations-pool.png)

A second machine joins your pool by invitation. **Operations → Pool → Invite worker** prints one; it can be used once, within the time you choose. On the other machine:

```sh
sisyphusd run --role worker --coordinator <your-address>:7700 --join <invitation> --name <a-name>
```

Two things to know:

- **Your node must be listening where the other machine can reach it.** A node the app started for itself listens only on your own machine. Start the node yourself with `--listen 0.0.0.0:7700`, and open TCP port 7700 to the other machine.
- **Invite client** is for a machine that submits work and stores files but runs none.
- **A machine with Ollama can lend its models.** Add `--models-from http://127.0.0.1:11434`. Its owner decides what the pool gets: `--models-only llama3.1,qwen3:8b` offers only those, `--models-gpu-layers 0` keeps the pool's work off the graphics card, and `--models-keep-alive 0` frees the model's memory as soon as it has answered. `sisyphusd nodes` then shows each model and how much of it is on the card. Some limits only Ollama itself can be told; the [daemon guide](../apps/sisyphusd/README.md) lists them.
- **On Windows**, the first run of `sisyphusd.exe`, or of the app, may bring up "Windows protected your PC" and then the firewall asking about "Sisyphus node"; see [the warnings on first run](#the-warnings-on-first-run).

Members are listed underneath, and **Remove** takes one out. A member can also take itself out, from its own machine, with `sisyphusd pool leave --addr <coordinator>`: the coordinator then lets go of the files it kept there for that machine, as **Remove** does. A machine whose node was started to work for another's pool (`--role worker --coordinator …`) has **Leave pool** under **Joined pool** on **Node overview**. It asks first; once the pool has let go, the node forgets that pool and stops. Start it with another pool's `--coordinator` and an invitation to join that one. [Testing a pool across machines](multi-machine-test.md) goes through all of it step by step, including what to do when a machine will not join.

```sh
bin/sisyphusd pool invite
bin/sisyphusd pool members
```

## Settings

![Settings: the node, the interface language, and the planner's model](images/settings.png)

**Settings** holds what belongs to the window (language, theme and accent, which are kept on this device) and the planner's model, which is kept on the node. The app is in seven languages, two of them written right to left.

**Lend this machine's models** offers the models of an Ollama on this machine to the pool, without the command line:

1. Turn lending on and give Ollama's address (`http://127.0.0.1:11434` for one on this machine). **Check connection** shows the models it has and which are loaded now.
2. Tick the models to offer. Then set the limits:
   - **GPU layers:** how much of a model goes on the graphics card. `0` keeps the pool's work off it, `-1` puts on as much as fits.
   - **CPU threads**, **context length**, and **keep loaded**, which is how long a model stays in memory after it answers.
3. Save.

Changes to the models and the limits apply at once. Turning lending on or off, or changing the address, needs the node restarted. The page says when. A few limits belong to Ollama itself rather than to Sisyphus, such as how many models it keeps loaded and which graphics cards it may use; the section lists them and how to set them on Windows, macOS and Linux. If the node was started with `--models-from`, those flags decide, and the saved settings wait for a start without them.

**Access** lets browsers on other machines use this node, which is off until you turn it on. Pick which of the machine's network addresses to serve on. Browsers then reach the node over HTTPS, with a certificate the node makes for itself, so each one warns about it the first time: compare the fingerprint shown here before accepting. **Devices** then pairs a browser with a one-time code, and lists the paired ones so you can revoke them.

## Checking results

A pool of your own machines can be taken at its word. One with other people's machines in it cannot, and a job can ask to be checked:

```sh
bin/sisyphusd job submit --verify 2 --params '{"from":0,"to":100000}'          # two workers must agree
bin/sisyphusd job submit --audit-share 0.2 --params '{"from":0,"to":100000}'   # this node reruns a fifth itself
```

What each of these catches and what it does not is in the [node's README](../apps/sisyphusd/README.md#verifying-results) and the [security model](security-model.md).

## From an AI agent

The node is also an MCP server, so an agent such as Claude Code can use the pool as a tool:

```sh
claude mcp add sisyphus -- sisyphusd mcp
```

The skill in `skills/sisyphus` tells an agent how to use it well. Both are described in the [node's README](../apps/sisyphusd/README.md).

## Where to go next

- [The node's README](../apps/sisyphusd/README.md): every command and option.
- [Testing a pool across machines](multi-machine-test.md): a real pool on real machines.
- [The security model](security-model.md): what is protected, and what is not yet.
- [The desktop app's README](../apps/sisyphus/README.md): building and packaging the app.
