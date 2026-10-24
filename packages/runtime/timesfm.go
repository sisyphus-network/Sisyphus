package runtime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/ipfs/go-cid"

	"github.com/sisyphus-network/Sisyphus/packages/storage"
)

// TimesFM is a pretrained forecasting model from Google Research. Its 2.5
// release, of 200M parameters, is published under Apache-2.0; its 3.0
// release is not, and is not used. As a forecast candidate it is backtested
// on the same windows as the models written in Go and joins the forecast
// only if it scores among the best kinds there, as Chronos-Bolt does. It
// runs in an image of its own, with the weights in it, on workers that run
// container work, and never reaches the network while a task runs.
//
// On one machine its forecasts are the same to the bit on a rerun, on one
// thread or four, and in batches of 1 or 64: 102 M4 series so run agreed
// exactly (#683). Across processors they have not been compared yet, so
// Compare holds them to the same tolerance as Chronos-Bolt's.

// timesfmPin is the TimesFM model, at a fixed revision, and what its files
// hash to.
var timesfmPin = struct{ repo, revision, config, weights string }{
	"google/timesfm-2.5-200m-pytorch", "1d952420fba87f3c6dee4f240de0f1a0fbc790e3",
	"cd3315b760d5cc7e278d7afdf41b897031ced888fc6c115bd9b3ac0ea2c47408",
	"2f776efe6245e42b24bc4153ffdf61810140210e4bd3b01fb21f7aa779ab6ce8",
}

// timesfmFamily is TimesFM as a forecast family: a job names its model
// timesfm-200m, and it runs in TimesFMImage.
var timesfmFamily = forecastFamily{
	kind:      "timesfm",
	name:      "TimesFM",
	variants:  []string{"200m"},
	fewest:    timesfmFewest,
	memoryMB:  timesfmMemoryMB,
	tolerance: timesfmTolerance,
	label:     TimesFMLabel,
	runner:    func(f Forecast) forecastRunner { return timesfmRunner{f} },
}

const (
	// timesfmFewest is how many points TimesFM is given at least to
	// forecast from.
	timesfmFewest = 1
	// timesfmBatch is the most forecasts the image makes at once. Each
	// batch is padded to its size, so a horizon with fewer forecasts than
	// this is made in one batch of just those.
	timesfmBatch = 64
	// timesfmContext is the most of a series TimesFM forecasts from: the
	// last 1024 points.
	timesfmContext = 1024
	// timesfmMostOutput bounds what the image leaves.
	timesfmMostOutput = 64 << 20
	// timesfmMemoryMB is the most memory the image is given. A batch of 64
	// long M4 series peaked at 2.2 GB.
	timesfmMemoryMB = 3072
	// timesfmTolerance is how far apart, relative to the size of what is
	// forecast, two workers' TimesFM numbers may be and still agree.
	timesfmTolerance = 1e-3
)

// timesfmNeeds are the packages of chronosRequirements that TimesFM's torch
// model needs; the rest are Chronos-Bolt's own.
var timesfmNeeds = []string{"anyio", "click", "filelock", "fsspec", "h11", "hf-xet", "httpcore2", "httpx2", "huggingface-hub", "idna", "jinja2", "markupsafe", "mpmath", "networkx", "numpy", "packaging", "pyyaml", "safetensors", "setuptools", "sympy", "torch", "tqdm", "truststore", "typing-extensions"}

// timesfmRequirements are the Python packages the image is built from, each
// held to its hashes: TimesFM's own, and the ones it shares with
// Chronos-Bolt at the same pins.
var timesfmRequirements = func() []string {
	lines := []string{"timesfm==3.0.2 --hash=sha256:cfa3d22ddc3f019f0d9d95f19055428c4a4713526dafd7e5d1a7769782e7613f"}
	for _, line := range strings.Split(strings.TrimSpace(chronosRequirements), "\n") {
		if name, _, _ := strings.Cut(line, "=="); slices.Contains(timesfmNeeds, name) {
			lines = append(lines, line)
		}
	}
	return lines
}()

// timesfmFetch downloads the model at its revision into /models/200m and
// checks its files against their hashes.
var timesfmFetch = fmt.Sprintf(`import hashlib,sys
from huggingface_hub import snapshot_download
d=snapshot_download(%q,revision=%q,local_dir='/models/200m',allow_patterns=['config.json','model.safetensors'])
for f,s in (('config.json',%q),('model.safetensors',%q)):
 h=hashlib.sha256(open(d+'/'+f,'rb').read()).hexdigest()
 h==s or sys.exit('200m/'+f+' hashes to '+h+', not '+s)
`, timesfmPin.repo, timesfmPin.revision, timesfmPin.config, timesfmPin.weights)

// timesfmDockerfile makes the image: Python, CPU torch and TimesFM from the
// hashes above, and the model's weights from its pin.
var timesfmDockerfile = func() string {
	var lines []string
	for _, line := range timesfmRequirements {
		lines = append(lines, "'"+line+"'")
	}
	return `FROM python:3.12-slim@sha256:a0f57064cbdcd6285fa45f51237ab0b35ea8ebaed06a1f5973e89e19dfb86704
RUN printf '%s\n' ` + strings.Join(lines, " ") + ` > /requirements.txt && pip install --no-cache-dir --no-deps --require-hashes --index-url https://download.pytorch.org/whl/cpu --extra-index-url https://pypi.org/simple -r /requirements.txt
RUN printf '%s' ` + base64.StdEncoding.EncodeToString([]byte(timesfmFetch)) + ` | base64 -d > /fetch.py && python -I /fetch.py && rm /fetch.py
ENV HF_HUB_OFFLINE=1 OMP_NUM_THREADS=1 MKL_NUM_THREADS=1
`
}()

// TimesFMImage names the image by what it is made from.
var TimesFMImage = func() string {
	sum := sha256.Sum256([]byte(timesfmDockerfile + timesfmScript))
	return "sisyphus-timesfm:" + hex.EncodeToString(sum[:6])
}()

// TimesFMLabel marks a worker that runs TimesFM in this image.
var TimesFMLabel = "forecast-timesfm:" + strings.TrimPrefix(TimesFMImage, "sisyphus-timesfm:")

// timesfmScript forecasts what /input/data asks for and leaves the
// forecasts in /output/answers.json: for each request, TimesFM's point
// forecast from each prefix of its series. The forecasts are made one
// horizon at a time, in batches of at most the size the task says, in the
// order they were asked for, so that the same task makes the same batches
// wherever it runs.
var timesfmScript = fmt.Sprintf(`import json, torch, numpy as np, timesfm
torch.set_num_threads(1)
asked = json.load(open('/input/data'))
answers = []
work = []
for i, r in enumerate(asked['requests']):
    answers.append({'forecasts': [None] * len(r['ends'])})
    for j, end in enumerate(r['ends']):
        work.append((i, j, r['series'][:end], r['horizon']))
model = timesfm.TimesFM_2p5_200M_torch(torch_compile=False)
model.load_checkpoint('/models/200m', torch_compile=False)
for horizon in sorted({w[3] for w in work}):
    group = [w for w in work if w[3] == horizon]
    batch = min(asked['batch'], len(group))
    model.compile(timesfm.ForecastConfig(max_context=%d, max_horizon=horizon, per_core_batch_size=batch, normalize_inputs=True, force_flip_invariance=True, infer_is_positive=True, fix_quantile_crossing=True))
    for k in range(0, len(group), batch):
        chunk = group[k:k + batch]
        point, _ = model.forecast(horizon=horizon, inputs=[np.array(w[2], dtype=np.float64) for w in chunk])
        for w, row in zip(chunk, point.tolist()):
            answers[w[0]]['forecasts'][w[1]] = row
json.dump(answers, open('/output/answers.json', 'w'))
`, timesfmContext)

// timesfmAsked is what the image is given.
type timesfmAsked struct {
	Batch    int              `json:"batch"`
	Requests []timesfmRequest `json:"requests"`
}

// timesfmRequest is one candidate's fits: its series once, and where each
// prefix it is forecast from ends.
type timesfmRequest struct {
	Horizon int       `json:"horizon"`
	Series  []float64 `json:"series"`
	Ends    []int     `json:"ends"`
}

// timesfmFailed is a TimesFM candidate's error when its image fails.
const timesfmFailed = "timesfm: the image failed on this worker"

// timesfmRunner fits TimesFM candidates in the image, all of a task's at
// once.
type timesfmRunner struct{ f Forecast }

func (timesfmRunner) name() string { return "timesfm" }

func (r timesfmRunner) fit(ctx context.Context, requests []fitRequest, done func(int)) ([]fitAnswer, error) {
	asked := timesfmAsked{Batch: timesfmBatch}
	for _, q := range requests {
		series := q.Prefixes[0]
		req := timesfmRequest{Horizon: q.Horizon, Series: series}
		for _, p := range q.Prefixes {
			if len(p) > len(series) || (len(p) > 0 && &p[0] != &series[0]) {
				return nil, errors.New("timesfm: a prefix is not the start of its series")
			}
			req.Ends = append(req.Ends, len(p))
		}
		asked.Requests = append(asked.Requests, req)
	}
	held := storage.NewMemory()
	in, _ := held.Put(ctx, bytes.NewReader(mustJSON(asked))) // in memory, which does not fail
	engine := r.f.Engine
	if engine == nil {
		engine = docker
	}
	// As with Chronos-Bolt, the image failing is its candidates' failure,
	// not the task's, and they are given one error whatever went wrong.
	failed := func(err error) ([]fitAnswer, error) {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		Report(ctx).Log(err.Error())
		answers := make([]fitAnswer, len(requests))
		for i := range answers {
			answers[i] = fitAnswer{Err: timesfmFailed}
		}
		done(len(requests))
		return answers, nil
	}
	if err := haveImage(ctx, engine, TimesFMImage, timesfmDockerfile, "TimesFM forecasting"); err != nil {
		return failed(err)
	}
	task := containerTask{
		ContainerParams: ContainerParams{Image: TimesFMImage, Command: []string{"python", "-I", "-c", timesfmScript}, Input: in.String(), MemoryMB: timesfmMemoryMB, CPUs: 1},
		Job:             "timesfm-" + in.String(),
		Count:           1,
	}
	raw, err := Container{Engine: engine, Confined: r.f.Confined, MaxOutput: timesfmMostOutput}.Execute(ctx, held, mustJSON(task))
	if err != nil {
		return failed(fmt.Errorf("timesfm: %w", err))
	}
	var out ContainerOutput
	json.Unmarshal(raw, &out) // made just now
	c, err := cid.Decode(out.Files["answers.json"])
	if err != nil {
		return failed(errors.New("timesfm: the image left no answers"))
	}
	blob, _ := held.Open(ctx, c) // left in the store just now
	defer blob.Close()
	var answered []struct {
		Forecasts [][]float64 `json:"forecasts"`
	}
	if err := json.NewDecoder(io.LimitReader(blob, timesfmMostOutput)).Decode(&answered); err != nil {
		return failed(fmt.Errorf("timesfm: the answers: %w", err))
	}
	if len(answered) != len(requests) {
		return failed(fmt.Errorf("timesfm: the image answered %d of %d candidates", len(answered), len(requests)))
	}
	answers := make([]fitAnswer, len(requests))
	for i, a := range answered {
		answers[i] = fitAnswer{Forecasts: a.Forecasts}
	}
	done(len(requests))
	return answers, nil
}
