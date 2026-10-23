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
	"math"
	"slices"
	"strings"

	"github.com/ipfs/go-cid"

	"github.com/sisyphus-network/Sisyphus/packages/storage"
)

// Chronos-Bolt is a family of pretrained forecasting models (Amazon
// Science, Apache 2.0). As a forecast candidate it is backtested on the
// same windows as the models written in Go, and joins the forecast only if
// it scores among the best kinds there. It runs in an image of its own,
// with the weights in it, on workers that run container work, and never
// reaches the network while a task runs.
//
// Its forecasts are not the same to the bit on every processor: torch's
// arithmetic differs with the processor, and with how many forecasts are
// made in one batch. Two workers given the same task make the same
// batches, on one thread, and on one machine they agree to the bit; across
// machines they agree to about a ten-thousandth, and Compare holds them to
// chronosTolerance.

// chronosPin is one Chronos-Bolt model, at a fixed revision, and what its
// files hash to.
type chronosPin struct {
	repo, revision, config, weights string
}

// chronosVariants are the Chronos-Bolt models a job may ask for, by the
// name it asks for each by.
var chronosVariants = map[string]chronosPin{
	"tiny":  {"amazon/chronos-bolt-tiny", "a0e552de83495b5c28c14c71c374f3e33280b340", "278f0086733031635fb1c861cb01c1bad6477420c7fcb19381a2993e335785e0", "75068728d376d2bec670379eeef4bfb4d24c0cfe24d957451f8d19b447030a32"},
	"mini":  {"amazon/chronos-bolt-mini", "251268337516a88e253628c43e1d26ec577b376b", "4299e93f30bb8d99d8f7dbaa39a1dfc6bf827ae67d4ef4c0b6ba4ca3df514980", "1a1a4297f132b808c5c7da24e3cce549d519c01ff5c2661fcad404baba018f24"},
	"small": {"amazon/chronos-bolt-small", "772f3d25d38aec6d914c8949dab4462e2d46f5d8", "9ca0ebbe049ccebacb0546361e8aa728f0cf104cd18d1838d78d296aa317bd21", "06a6a19bbe74bc10a9cd193bd4bf2bf638ae07f7e0d51653ae7ab8ea968a21dd"},
	"base":  {"amazon/chronos-bolt-base", "5d9f166d69f47aef3401367a7b842e78fe97b121", "46f7e7e695569f67b06178faa4f236c281392945aa2d1bdf844442e57155530b", "31f875483a3215bc6880a0837ea608a13ce55f88ad90538c3cdd0b29aeb60b36"},
}

// chronosKinds are the kinds of model a job names the Chronos-Bolt models
// by, smallest first. "all" does not include them: they are asked for.
var chronosKinds = []string{"chronos-tiny", "chronos-mini", "chronos-small", "chronos-base"}

const (
	// chronosFewest is how many points Chronos-Bolt is given at least to
	// forecast from.
	chronosFewest = 1
	// chronosBatch is how many forecasts the image makes at once. It is
	// part of the task, not the worker, since it moves the last digits.
	chronosBatch = 64
	// chronosMostOutput bounds what the image leaves: a forecast for each
	// window of each candidate a task carries.
	chronosMostOutput = 64 << 20
	// chronosMemoryMB is the most memory the image is given: the largest
	// model and a batch of long series fit in it.
	chronosMemoryMB = 4096
	// chronosTolerance is how far apart, relative to the size of what is
	// forecast, two workers' Chronos numbers may be and still agree. Run on
	// one thread and four, and in batches of 64, 7 and 1, the same 102 M4
	// series differed by at most 1.0e-4 relative; this is ten times that.
	chronosTolerance = 1e-3
)

// wantsChronos reports whether any of the kinds is a Chronos-Bolt model.
func wantsChronos(kinds []string) bool {
	return slices.ContainsFunc(kinds, func(k string) bool { return slices.Contains(chronosKinds, k) })
}

// chronosRequirements are the Python packages the image is built from, each
// held to its hashes, for x86-64 and 64-bit ARM.
const chronosRequirements = `
accelerate==1.15.0 --hash=sha256:97eacca0b73e45cb867dbf8c5d5d4dc32219544300e0c8992c7334dc2ef33cec
annotated-doc==0.0.5 --hash=sha256:117bac03a25ede5df5440e855b32d556049ca169ead221505badf432fed4b101
anyio==4.15.1 --hash=sha256:6152fdbbf9a77fdec97731721bebf7c4c44f7c29b424b0065826173efc7ed101
chronos-forecasting==2.3.2 --hash=sha256:0f0d9a1972f252d6cf584b9fa749bd1b389b3c1cf05a889c4130cb10652c2117
click==8.5.0 --hash=sha256:255bc9599cf7748b4b1a446ccc735421bd08a2ae529a8b88597d3de5664ee360
einops==0.8.2 --hash=sha256:54058201ac7087911181bfec4af6091bb59380360f069276601256a76af08193
filelock==4.2.4 --hash=sha256:301241438c57936ef93f4243a94f2559022268bfcf9b375a9a72eb0f8db8358e
fsspec==2026.9.0 --hash=sha256:8dd6e646e99ea382bd85f97a45e6b526a442d79423a7dc673f1e2756d05fcb5f
h11==0.16.0 --hash=sha256:63cf8bbe7522de3bf65932fda1d9c2772064ffb3dae62d55932da54b31cb6c86
hf-xet==1.7.0 --hash=sha256:2814a6e999d13464c4d679b788cc5d784eb5a4edfc638a31f10e9a11ab531ef8 --hash=sha256:fcfd6c22418e57dd5b3aea649e813b2e2cfb2aebf317b210d90f1fe4b3018b52
httpcore2==2.16.0 --hash=sha256:5c4358319f2602d984ad93182c9fee5160f22e9da7babdda870a5e7c4e2dc34f
httpx2==2.16.0 --hash=sha256:fd2c5d7909e8d86fe92d53020c310ca0e4e431b838c207c5754b9bbf0308d49a
huggingface-hub==2.2.0 --hash=sha256:1667f145dc56dc210d60966069397df9ecfca9607a5d43db88b308c89dae56b3
idna==3.20 --hash=sha256:ab7ae7122974553370f0bdb919e1a960b2cd1bc1ef0276416d896db81c14582c
jinja2==3.1.6 --hash=sha256:85ece4451f492d0c13c5dd7c13a64681a86afae63a5f347908daf103ce6d2f67
markdown-it-py==4.2.0 --hash=sha256:9f7ebbcd14fe59494226453aed97c1070d83f8d24b6fc3a3bcf9a38092641c4a
markupsafe==3.0.4 --hash=sha256:6a45c3d514f2436064db00d7fc8778d888f0236ebfed649b53d13a59e69ad51b --hash=sha256:8e124f974786f831d6043728e38296969d3579db8896fe004682f5758e613581
mdurl==0.1.2 --hash=sha256:84008a41e51615a49fc9966191ff91509e3c40b939176e643fd50a5c2196b8f8
mpmath==1.3.0 --hash=sha256:a0b2b9fe80bbcd81a6647ff13108738cfb482d481d826cc0e02f5b35e5c88d2c
networkx==3.7 --hash=sha256:e3fd2c13a7814cee3746340d8d7f8598a67f16a58bf47fb7f8793fab6efca1b0
numpy==2.5.4 --hash=sha256:fbde6962867ee75b48b0ee29b2b9372ec5d617799dbaf38e82dc0596f2f7738a --hash=sha256:fe4d21ab149f15e4e6043dfb0de87e6e5f34ac176cde83060e9802981fca2ac2
packaging==26.3 --hash=sha256:d7193f7c8e4e93f444fde0262bf90af30e16fa0ad0ad44cb553c87339b23cd1c
pandas==3.1.0 --hash=sha256:9433b5dc2f26ef9d9b56daaf4d41d078bf4372c5d88c42ed5b397a6eea80de16 --hash=sha256:9b4e696ff70ed06e986feeec09356c491ce878d0df69a36c8e591b6a38afa29b
psutil==7.2.2 --hash=sha256:076a2d2f923fd4821644f5ba89f059523da90dc9014e85f8e45a5774ca5bc6f9 --hash=sha256:b0726cecd84f9474419d67252add4ac0cd9811b04d61123054b9fb6f57df6e9e
pygments==2.21.0 --hash=sha256:2363c69b61c4a97c838da3b130dcd6468f4848992b21a82f2a63ec34377137d9
python-dateutil==3.0.0 --hash=sha256:3c79630202e4152c6add678446984236efe4f57e84d8e892aab0637bab82e9fd
pyyaml==6.0.3 --hash=sha256:9149cad251584d5fb4981be1ecde53a1ca46c891a79788c0df828d2f166bda28 --hash=sha256:ba1cc08a7ccde2d2ec775841541641e4548226580ab850948cbfda66a1befcdc
regex==2026.9.29 --hash=sha256:39ab5894d971f9ac68baa6eca5c50387db579cfcacf36ae8df3feceb1815e6d0 --hash=sha256:f93bc1c3486ef3747e07c9d7c1d0a147b8fbaab975f80e348aed6f71309dfaca
rich==15.0.0 --hash=sha256:33bd4ef74232fb73fe9279a257718407f169c09b78a87ad3d296f548e27de0bb
safetensors==0.8.0 --hash=sha256:7a46e5ff292c356d6991e60942ba7f79817682d3a2cef0702136448cb9c4d235 --hash=sha256:fd6f3f93c9a0a7cc2788ee63fb763353d4bd2e89b0751bc78fcf7dda00bea774
setuptools==84.2.1 --hash=sha256:ca140b45d5325a9478f880e351484f62fef15f336462e449ca0c03c873d589c2
shellingham==1.5.4 --hash=sha256:7ecfff8f2fd72616f7481040475a65b2bf8af90a56c89140852d1120324e8686
six==1.17.0 --hash=sha256:4721f391ed90541fddacab5acf947aa0d3dc7d27b2e1e8eda2be8970586c3274
sympy==1.14.0 --hash=sha256:e091cc3e99d2141a0ba2847328f5479b05d94a6635cb96148ccb3f34671bd8f5
tokenizers==0.23.3 --hash=sha256:376851d22bcf9d650a5c3090bb83e6cf9e895fbf0595369fa4cd43c1f69b5f87 --hash=sha256:ec82e80e65a862275b97c3d90b7a523df8d9519ee48aeb4e9625b2cc909274e0
torch==2.8.0+cpu --hash=sha256:610f600c102386e581327d5efc18c0d6edecb9820b4140d26163354a99cd800d --hash=sha256:cb9a8ba8137ab24e36bf1742cb79a1294bd374db570f09fc15a5e1318160db4e
tqdm==4.70.1 --hash=sha256:c293e525e6fef9c20e8728fd4612df02a0aa31bb5fe91ecd93e123b1b7bffa73
transformers==5.20.2 --hash=sha256:7ae75dbc1fb3922aff1b6c97b9e5479d6cf26fd86a8866bfb71324190479f807
truststore==0.10.5 --hash=sha256:9aaaedaefaf06d8b206278cf8b5012bc897f485a874503501e12d776df78951c
typer==0.27.3 --hash=sha256:e50022f28b82a86313e54501317a1db64bf8f8d036ff8cfe5ca7e47675454aff
typing-extensions==4.16.0 --hash=sha256:481caa481374e813c1b176ada14e97f1f67a4539ce9cfeb3f350d78d6370c2e8
tzdata==2026.5 --hash=sha256:b683bd1b6659ddcd810ff02ad09ba821d4bf1065072805063eb35c49617905ac
`

// chronosFetch downloads each model at its revision into /models and
// checks its files against their hashes, so that the image is made of
// what is pinned here or not made at all.
var chronosFetch = func() string {
	var b strings.Builder
	b.WriteString("import hashlib,sys\nfrom huggingface_hub import snapshot_download\n")
	for _, name := range []string{"tiny", "mini", "small", "base"} {
		p := chronosVariants[name]
		fmt.Fprintf(&b, "d=snapshot_download(%q,revision=%q,local_dir='/models/%s',allow_patterns=['config.json','model.safetensors'])\n", p.repo, p.revision, name)
		fmt.Fprintf(&b, "for f,s in (('config.json',%q),('model.safetensors',%q)):\n", p.config, p.weights)
		b.WriteString(" h=hashlib.sha256(open(d+'/'+f,'rb').read()).hexdigest()\n")
		fmt.Fprintf(&b, " h==s or sys.exit('%s/'+f+' hashes to '+h+', not '+s)\n", name)
	}
	return b.String()
}()

// chronosDockerfile makes the image: Python, CPU torch and Chronos from
// the hashes above, and the four models' weights from their pins.
var chronosDockerfile = func() string {
	var lines []string
	for _, line := range strings.Split(strings.TrimSpace(chronosRequirements), "\n") {
		lines = append(lines, "'"+line+"'")
	}
	return `FROM python:3.12-slim@sha256:a0f57064cbdcd6285fa45f51237ab0b35ea8ebaed06a1f5973e89e19dfb86704
RUN printf '%s\n' ` + strings.Join(lines, " ") + ` > /requirements.txt && pip install --no-cache-dir --no-deps --require-hashes --index-url https://download.pytorch.org/whl/cpu --extra-index-url https://pypi.org/simple -r /requirements.txt
RUN printf '%s' ` + base64.StdEncoding.EncodeToString([]byte(chronosFetch)) + ` | base64 -d > /fetch.py && python -I /fetch.py && rm /fetch.py
ENV HF_HUB_OFFLINE=1 TRANSFORMERS_OFFLINE=1 OMP_NUM_THREADS=1 MKL_NUM_THREADS=1
`
}()

// ChronosImage names the image by what it is made from, as QchemImage
// does, so that a change to it is a different image.
var ChronosImage = func() string {
	sum := sha256.Sum256([]byte(chronosDockerfile + chronosScript))
	return "sisyphus-chronos:" + hex.EncodeToString(sum[:6])
}()

// ChronosLabel marks a worker that runs Chronos-Bolt in this image. A job
// that asks for Chronos-Bolt needs it, so it goes only to workers that run
// the same image as its coordinator, not merely any that run one.
var ChronosLabel = "forecast-chronos:" + strings.TrimPrefix(ChronosImage, "sisyphus-chronos:")

// chronosScript forecasts what /input/data asks for and leaves the
// forecasts in /output/answers.json: for each request, a forecast from
// each prefix of its series, the median of what Chronos-Bolt forecasts.
// The forecasts are made one model at a time, then one horizon at a time,
// in batches of the size the task says, in the order they were asked for,
// so that the same task makes the same batches wherever it runs.
const chronosScript = `import json, torch
from chronos import BaseChronosPipeline
torch.set_num_threads(1)
asked = json.load(open('/input/data'))
batch = asked['batch']
answers = []
work = []
for i, r in enumerate(asked['requests']):
    answers.append({'forecasts': [None] * len(r['ends'])})
    for j, end in enumerate(r['ends']):
        work.append((i, j, r['variant'], r['series'][:end], r['horizon']))
for variant in sorted({w[2] for w in work}):
    model = BaseChronosPipeline.from_pretrained('/models/' + variant, device_map='cpu', torch_dtype=torch.float32)
    mine = [w for w in work if w[2] == variant]
    for horizon in sorted({w[4] for w in mine}):
        group = [w for w in mine if w[4] == horizon]
        for k in range(0, len(group), batch):
            chunk = group[k:k + batch]
            q, _ = model.predict_quantiles([torch.tensor(w[3], dtype=torch.float32) for w in chunk], prediction_length=horizon, quantile_levels=[0.5])
            for w, row in zip(chunk, q[:, :, 0].tolist()):
                answers[w[0]]['forecasts'][w[1]] = row
json.dump(answers, open('/output/answers.json', 'w'))
`

// chronosAsked is what the image is given.
type chronosAsked struct {
	Batch    int              `json:"batch"`
	Requests []chronosRequest `json:"requests"`
}

// chronosRequest is one candidate's fits: its series once, and where each
// prefix it is forecast from ends.
type chronosRequest struct {
	Variant string    `json:"variant"`
	Horizon int       `json:"horizon"`
	Series  []float64 `json:"series"`
	Ends    []int     `json:"ends"`
}

// chronosRunner fits Chronos-Bolt candidates in the image, all of a task's
// at once.
type chronosRunner struct{ f Forecast }

func (chronosRunner) name() string { return "chronos" }

func (r chronosRunner) fit(ctx context.Context, requests []fitRequest, done func(int)) ([]fitAnswer, error) {
	asked := chronosAsked{Batch: chronosBatch}
	for _, q := range requests {
		series := q.Prefixes[0]
		req := chronosRequest{Variant: q.Model.Variant, Horizon: q.Horizon, Series: series}
		for _, p := range q.Prefixes {
			// Every prefix is the start of the whole series, as
			// backtestRequest makes them.
			if len(p) > len(series) || (len(p) > 0 && &p[0] != &series[0]) {
				return nil, errors.New("chronos: a prefix is not the start of its series")
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
	// The image failing, as when it runs out of memory, is its candidates'
	// failure, not the task's: the candidates written in Go are still
	// scored. Only a task given up as a whole fails.
	failed := func(err error) ([]fitAnswer, error) {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		answers := make([]fitAnswer, len(requests))
		for i := range answers {
			answers[i] = fitAnswer{Err: err.Error()}
		}
		done(len(requests))
		return answers, nil
	}
	if err := haveImage(ctx, engine, ChronosImage, chronosDockerfile, "Chronos-Bolt forecasting"); err != nil {
		return failed(err)
	}
	task := containerTask{
		ContainerParams: ContainerParams{Image: ChronosImage, Command: []string{"python", "-I", "-c", chronosScript}, Input: in.String(), MemoryMB: chronosMemoryMB, CPUs: 1},
		Job:             "chronos-" + in.String(),
		Count:           1,
	}
	raw, err := Container{Engine: engine, Confined: r.f.Confined, MaxOutput: chronosMostOutput}.Execute(ctx, held, mustJSON(task))
	if err != nil {
		return failed(fmt.Errorf("chronos: %w", err))
	}
	var out ContainerOutput
	json.Unmarshal(raw, &out) // made just now
	c, err := cid.Decode(out.Files["answers.json"])
	if err != nil {
		return failed(errors.New("chronos: the image left no answers"))
	}
	blob, _ := held.Open(ctx, c) // left in the store just now
	defer blob.Close()
	var answered []struct {
		Forecasts [][]float64 `json:"forecasts"`
	}
	if err := json.NewDecoder(io.LimitReader(blob, chronosMostOutput)).Decode(&answered); err != nil {
		return failed(fmt.Errorf("chronos: the answers: %w", err))
	}
	if len(answered) != len(requests) {
		return failed(fmt.Errorf("chronos: the image answered %d of %d candidates", len(answered), len(requests)))
	}
	answers := make([]fitAnswer, len(requests))
	for i, a := range answered {
		answers[i] = fitAnswer{Forecasts: a.Forecasts}
	}
	done(len(requests))
	return answers, nil
}

// runners is how a forecast task fits its candidates: Chronos-Bolt in its
// image, everything else in process.
func (f Forecast) runners(models []forecastModel) (runnerFor, error) {
	if !slices.ContainsFunc(models, func(m forecastModel) bool { return m.Kind == "chronos" }) {
		return inProcess, nil
	}
	if why := f.cannotRunChronos(); why != "" {
		return nil, errors.New("forecast payload: the task has Chronos-Bolt candidates, and " + why)
	}
	return func(m forecastModel) forecastRunner {
		if m.Kind == "chronos" {
			return chronosRunner{f}
		}
		return goRunner{}
	}, nil
}

// Needs says that a job that asks for Chronos-Bolt needs a worker that
// runs it in the coordinator's image.
func (Forecast) Needs(params []byte) []string {
	var p ForecastParams
	if json.Unmarshal(params, &p) != nil {
		return nil
	}
	if kinds, err := p.kinds(); err != nil || !wantsChronos(kinds) {
		return nil
	}
	return []string{ChronosLabel}
}

// cannotRunChronos says why this node cannot run the Chronos-Bolt image,
// or nothing if it can: it must run container work, and give a container
// the memory the image asks for, if its owner holds containers to less.
func (f Forecast) cannotRunChronos() string {
	switch {
	case !f.Containers:
		return "this node does not run container work"
	case f.Confined.MemoryMB > 0 && f.Confined.MemoryMB < chronosMemoryMB:
		return fmt.Sprintf("this node holds a container to %d MB, and Chronos-Bolt needs %d", f.Confined.MemoryMB, chronosMemoryMB)
	}
	return ""
}

// Offers is Chronos-Bolt, where this node can run it.
func (f Forecast) Offers(context.Context) []string {
	if f.cannotRunChronos() != "" {
		return nil
	}
	return []string{ChronosLabel}
}

// modelChoices are the kinds a form offers: Chronos-Bolt's only where the
// node can run it.
func (f Forecast) modelChoices() []string {
	choices := append([]string{"all"}, forecastKinds...)
	if f.cannotRunChronos() == "" {
		choices = append(choices, chronosKinds...)
	}
	return choices
}

func (f Forecast) modelsHelp() string {
	if f.cannotRunChronos() != "" {
		return "Which kind of model to try, or all of them."
	}
	return "Which kind of model to try, or all of them. The Chronos-Bolt models are pretrained, run in an image on workers that run containers, and are tried only when asked for."
}

// Varies says that, with a Chronos-Bolt candidate, two honest workers'
// outputs can differ in their last digits. Compare says when they still
// agree.
func (Forecast) Varies() bool { return true }

// Compare says whether two outputs of one forecast task are the same
// result. Outputs without a Chronos-Bolt candidate are the same only if
// their bytes are, as before there was one. With one, every other
// candidate must still be the same to the bit, and each Chronos-Bolt
// candidate's forecasts, residuals and scores must agree to within
// chronosTolerance of the size of the series or of what it forecast,
// whichever is larger. The series' size is given by both outputs and must
// be the same, since it is read from the task's input. The choice of
// forecast is not compared: Aggregate makes it once, from the outputs
// taken.
func (Forecast) Compare(a, b []byte) (bool, string) {
	if bytes.Equal(a, b) {
		return true, ""
	}
	if bytes.Compare(a, b) > 0 {
		a, b = b, a
	}
	var x, y forecastPart
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false, "an output is not a forecast task's"
	}
	if x.Points != y.Points || x.Horizon != y.Horizon || x.Folds != y.Folds || x.Last != y.Last || x.Scale != y.Scale || len(x.Candidates) != len(y.Candidates) {
		return false, fmt.Sprintf("one backtested %d candidates %d steps ahead on %d windows of %d points of size %g ending at %q, the other %d %d steps ahead on %d windows of %d points of size %g ending at %q", len(x.Candidates), x.Horizon, x.Folds, x.Points, x.Scale, x.Last, len(y.Candidates), y.Horizon, y.Folds, y.Points, y.Scale, y.Last)
	}
	chronos := false
	for i := range x.Candidates {
		p, q := x.Candidates[i], y.Candidates[i]
		switch {
		case p.Model != q.Model:
			return false, fmt.Sprintf("candidate %d is %s in one and %s in the other", i+1, p.Model.Name(), q.Model.Name())
		case p.Model.Kind != "chronos":
			if !bytes.Equal(mustJSON(p), mustJSON(q)) {
				return false, fmt.Sprintf("candidate %s, which is the same on every processor, differs", p.Model.Name())
			}
		default:
			chronos = true
			if why := compareChronos(p, q, x.Scale); why != "" {
				return false, fmt.Sprintf("candidate %s: %s", p.Model.Name(), why)
			}
		}
	}
	if !chronos {
		return false, "the outputs differ, and neither has a candidate that may"
	}
	return true, ""
}

// compareChronos says how two runs of one Chronos-Bolt candidate differ by
// more than chronosTolerance of the size of what was forecast, or nothing
// where they agree. The size is the series' own, its largest value by
// magnitude, or the largest forecast either made if that is larger. A
// residual is what the series did less a forecast of it from a window's
// past, so it moves as much as that forecast does; and a window's forecast
// is the size of the series, though the forecast from its end may be near
// zero, as for a series that swings about it.
func compareChronos(p, q forecastOutcome, seriesSize float64) string {
	switch {
	case p.Error != q.Error:
		return fmt.Sprintf("one failed with %q and the other with %q", p.Error, q.Error)
	case p.Error != "":
		return ""
	case len(p.Forecast) != len(q.Forecast) || len(p.StepRMSE) != len(q.StepRMSE) || len(p.Residuals) != len(q.Residuals):
		return "the two forecast, scored or kept residuals for different numbers of steps"
	}
	scale := seriesSize
	for _, v := range slices.Concat(p.Forecast, q.Forecast) {
		scale = max(scale, math.Abs(v))
	}
	limit := chronosTolerance * max(scale, 1e-12)
	within := func(what string, xs, ys []float64) string {
		for i := range xs {
			if gap := math.Abs(xs[i] - ys[i]); !(gap <= limit) {
				return fmt.Sprintf("its %s %d is %g in one and %g in the other, %.3g apart, more than %g of the size %g", what, i+1, xs[i], ys[i], gap, chronosTolerance, scale)
			}
		}
		return ""
	}
	if why := within("forecast step", p.Forecast, q.Forecast); why != "" {
		return why
	}
	if why := within("score", []float64{p.MAE, p.RMSE}, []float64{q.MAE, q.RMSE}); why != "" {
		return why
	}
	if why := within("error at step", p.StepRMSE, q.StepRMSE); why != "" {
		return why
	}
	for k := range p.Residuals {
		if len(p.Residuals[k]) != len(q.Residuals[k]) {
			return fmt.Sprintf("window %d kept residuals for different numbers of steps", k+1)
		}
		if why := within(fmt.Sprintf("window %d residual", k+1), p.Residuals[k], q.Residuals[k]); why != "" {
			return why
		}
	}
	return ""
}
