package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sisyphus-network/Sisyphus/packages/storage"
)

func TestATranscribeJobIsAContainerJobWithTheImageAndCommandFixed(t *testing.T) {
	store := storage.NewMemory()
	ctx := context.Background()
	recording, _ := store.Put(ctx, strings.NewReader("a recording"))

	payloads, err := Transcribe{}.Split(ctx, store, []byte(`{"input":"`+recording.String()+`"}`), 3)
	if err != nil || len(payloads) != 3 {
		t.Fatalf("split: %d payloads, %v", len(payloads), err)
	}
	var task containerTask
	json.Unmarshal(payloads[2], &task)
	if task.Image != TranscribeImages["large-v3-turbo-q5_0"] || !strings.HasPrefix(task.Image, "sisyphus-transcribe-large-v3-turbo-q5_0:") || task.Input != recording.String() || task.Index != 2 || task.Count != 3 || task.Network {
		t.Errorf("the last task: %+v", task)
	}
	if len(task.Command) != 3 || task.Command[0] != "sh" || !strings.Contains(task.Command[2], "whisper-cli") || !strings.Contains(task.Command[2], "silencedetect") {
		t.Errorf("its command: %q", task.Command)
	}
	// Unless said otherwise: the quantised turbo model, and whisper tells
	// the language.
	if task.Env["LANGUAGE"] != "auto" || task.Env["MODEL"] != "large-v3-turbo-q5_0" {
		t.Errorf("its settings: %v", task.Env)
	}

	payloads, err = Transcribe{}.Split(ctx, store, []byte(`{"input":"`+recording.String()+`","language":"fr","model":"small","segments":2}`), 8)
	if err != nil || len(payloads) != 2 {
		t.Fatalf("split as asked: %d payloads, %v", len(payloads), err)
	}
	json.Unmarshal(payloads[0], &task)
	if task.Image != TranscribeImages["small"] || !strings.HasPrefix(task.Image, "sisyphus-transcribe-small:") || task.Env["LANGUAGE"] != "fr" || task.Env["MODEL"] != "small" {
		t.Errorf("settings as asked: %s %v", task.Image, task.Env)
	}
	// An English-only model hears English, unless told otherwise.
	payloads, err = Transcribe{}.Split(ctx, store, []byte(`{"input":"`+recording.String()+`","model":"tiny.en"}`), 1)
	json.Unmarshal(payloads[0], &task)
	if err != nil || task.Image != TranscribeImages["tiny.en"] || task.Env["LANGUAGE"] != "en" || task.Env["MODEL"] != "tiny.en" {
		t.Errorf("an English-only model: %s %v, %v", task.Image, task.Env, err)
	}

	// Every model has an image of its own, under a name Docker takes.
	reference := regexp.MustCompile(`^[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*:[0-9a-f]{12}$`)
	seen := map[string]bool{}
	for _, model := range transcribeModelNames {
		image := TranscribeImages[model]
		if !reference.MatchString(image) || !strings.HasPrefix(image, "sisyphus-transcribe-"+model+":") || seen[image] || transcribeModels[model] == "" {
			t.Errorf("%s: the image %q", model, image)
		}
		seen[image] = true
	}
	if len(seen) != len(transcribeModels) || len(TranscribeImages) != len(transcribeModels) || len(transcribeModelNames) != 14 {
		t.Errorf("%d models, %d images, %d names", len(transcribeModels), len(TranscribeImages), len(transcribeModelNames))
	}

	for params, want := range map[string]string{
		`not json`:                                          "transcribe parameters",
		`{}`:                                                "input is not the CID of a stored recording",
		`{"input":"somewhere"}`:                             "input is not the CID of a stored recording",
		`{"input":"CID","language":"en;"}`:                  `language is "en;", and must be a code such as en, or auto`,
		`{"input":"CID","language":"EN"}`:                   `language is "EN"`,
		`{"input":"CID","language":"english"}`:              `language is "english"`,
		`{"input":"CID","model":"large"}`:                   `model is "large", and must be one of tiny, tiny.en, base, base.en, small, small.en, large-v3-turbo-q5_0, large-v3-q5_0, medium, medium.en, large-v3-turbo, large-v1, large-v2, large-v3`,
		`{"input":"CID","segments":-1}`:                     "segments is -1, and must be 0 or more",
		`{"input":"CID","model":"base.en","language":"fr"}`: `the model base.en understands only English, and language is "fr"`,
		`{"input":"CID","model":"Base"}`:                    `model is "Base"`,
	} {
		params = strings.Replace(params, "CID", recording.String(), 1)
		if _, err := (Transcribe{}).Split(ctx, store, []byte(params), 2); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want an error containing %q", params, err, want)
		}
	}
	if w, err := WithContainers().Get("transcribe"); err != nil || w.Name() != "transcribe" || !strings.Contains(WithContainers().Describe("transcribe"), "whisper.cpp") {
		t.Errorf("the workload as registered: %v, %v", w, err)
	}
	if !WithContainers().InContainers("transcribe") {
		t.Error("transcribe is not said to run in containers")
	}
	if _, err := Builtin().Get("transcribe"); err == nil {
		t.Error("a node that runs no containers offers to transcribe")
	}
	if !(Transcribe{}).Varies() {
		t.Error("transcribe says two honest runs give the same result")
	}
}

func TestEachTranscribingImageIsBuiltOnlyWhereItIsMissing(t *testing.T) {
	store := storage.NewMemory()
	ctx := context.Background()
	recording, _ := store.Put(ctx, strings.NewReader("a recording"))
	transcribes := func(args []string, _, _ io.Writer) (int, error) {
		os.WriteFile(filepath.Join(volume(args, "/output"), "segments.json"), []byte(`{"segments":[]}`), 0o644)
		return 0, nil
	}
	var shared string
	for _, model := range transcribeModelNames {
		payloads, _ := Transcribe{}.Split(ctx, store, []byte(`{"input":"`+recording.String()+`","model":"`+model+`"}`), 1)
		image := TranscribeImages[model]

		has := &pretend{do: func(args []string, stdout, stderr io.Writer) (int, error) {
			if args[0] == "image" {
				return 0, nil
			}
			return transcribes(args, stdout, stderr)
		}}
		raw, err := Transcribe{Engine: has.engine}.Execute(ctx, store, payloads[0])
		if err != nil {
			t.Fatal(err)
		}
		var out ContainerOutput
		json.Unmarshal(raw, &out)
		if got := readBlob(t, store, out.Files["segments.json"]); got != `{"segments":[]}` {
			t.Errorf("%s: the task's output: %q", model, got)
		}
		if len(has.asked) != 2 || strings.Join(has.asked[0], " ") != "image inspect "+image || has.asked[1][0] != "run" {
			t.Errorf("%s: with the image there, the engine was asked %v", model, has.asked)
		}

		var recipe string
		lacks := &pretend{do: func(args []string, stdout, stderr io.Writer) (int, error) {
			switch args[0] {
			case "image":
				return 1, nil
			case "build":
				text, _ := os.ReadFile(filepath.Join(args[len(args)-1], "Dockerfile"))
				recipe = string(text)
				return 0, nil
			}
			return transcribes(args, stdout, stderr)
		}}
		said := new(heard)
		if _, err := (Transcribe{Engine: lacks.engine}).Execute(WithReporter(ctx, said), store, payloads[0]); err != nil {
			t.Fatal(err)
		}
		if len(lacks.asked) != 3 || strings.Join(lacks.asked[1][:3], " ") != "build --tag "+image {
			t.Errorf("%s: with the image missing, the engine was asked %v", model, lacks.asked)
		}
		// whisper.cpp at its release, the model checked against its
		// digest, and ffmpeg as the transcoding image has it.
		for _, want := range []string{"--branch " + whisperRelease, whisperCommit, "/ggml-" + model + ".bin", transcribeModels[model] + "  /model.bin\" | sha256sum -c -", transcodeDockerfile, "/models/ggml-" + model + ".bin"} {
			if !strings.Contains(recipe, want) {
				t.Errorf("%s: the recipe lacks %q:\n%s", model, want, recipe)
			}
		}
		// Only the model's own lines differ, so every model's image
		// shares the compiled whisper.cpp and ffmpeg.
		var rest []string
		for _, line := range strings.Split(recipe, "\n") {
			if !strings.Contains(line, "ggml-"+model+".bin") {
				rest = append(rest, line)
			}
		}
		if shared == "" {
			shared = strings.Join(rest, "\n")
		}
		if strings.Join(rest, "\n") != shared || strings.Count(recipe, model+".bin") != 2 {
			t.Errorf("%s: the recipe differs from the others in more than its model:\n%s", model, recipe)
		}
		if sum := sha256.Sum256([]byte(recipe)); !strings.HasSuffix(image, ":"+hex.EncodeToString(sum[:6])) {
			t.Errorf("%s: the image %s is not named by its recipe", model, image)
		}
		if log := strings.Join(said.lines, "\n"); !strings.Contains(log, "building the image transcription with the "+model+" model runs in") {
			t.Errorf("%s: the task's log: %q", model, said.lines)
		}
	}

	payloads, _ := Transcribe{}.Split(ctx, store, []byte(`{"input":"`+recording.String()+`"}`), 1)
	broken := &pretend{do: func(args []string, _, stderr io.Writer) (int, error) {
		if args[0] == "image" {
			return 1, nil
		}
		io.WriteString(stderr, "fetching\nsha256sum: WARNING: 1 computed checksum did NOT match\n")
		return 1, nil
	}}
	if _, err := (Transcribe{Engine: broken.engine}).Execute(ctx, store, payloads[0]); err == nil || !strings.Contains(err.Error(), "build the image transcription with the large-v3-turbo-q5_0 model runs in: sha256sum: WARNING") {
		t.Errorf("a build that fails: %v", err)
	}

	// A worker builds and runs only an image of its own making.
	nothing := &pretend{do: func([]string, io.Writer, io.Writer) (int, error) { return 0, nil }}
	var task containerTask
	json.Unmarshal(payloads[0], &task)
	task.Image = "alpine:3.20"
	for payload, want := range map[string]string{
		"not json":             "transcribe payload",
		string(mustJSON(task)): `transcribe payload: "alpine:3.20" is not an image transcription runs in`,
	} {
		if _, err := (Transcribe{Engine: nothing.engine}).Execute(ctx, store, []byte(payload)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%.40s: %v, want an error containing %q", payload, err, want)
		}
	}
	if len(nothing.asked) != 0 {
		t.Errorf("the engine was asked %v", nothing.asked)
	}
}

func TestTheStretchesAreTranscribedInOrder(t *testing.T) {
	store := storage.NewMemory()
	ctx := context.Background()
	stretch := func(index int, s transcribeStretch) []byte {
		c, _ := store.Put(ctx, strings.NewReader(string(mustJSON(s))))
		return mustJSON(ContainerOutput{Index: index, Files: map[string]string{"segments.json": c.String()}})
	}
	outputs := [][]byte{
		stretch(0, transcribeStretch{Start: 0, End: 7.172, Duration: 3725.5, Language: "en", Segments: []TranscribeSegment{
			{Start: 0, End: 2.18, Text: " The boulder rolls up the hill."},
			{Start: 2.18, End: 2.18, Text: " "},
			{Start: 2.5, End: 9.0004, Text: "  Again, every morning. "},
		}}),
		stretch(1, transcribeStretch{Start: 7.172, End: 3725.5, Duration: 3725.5, Segments: []TranscribeSegment{
			{Start: 7.172, End: 8, Text: "[BLANK_AUDIO]"},
			{Start: 3661.0015, End: 3725.9, Text: " Then the stone falls."},
		}}),
		// A stretch too short to transcribe.
		stretch(2, transcribeStretch{Start: 3725.5, End: 3725.5, Duration: 3725.5}),
	}

	raw, err := Transcribe{}.Aggregate(ctx, store, outputs)
	if err != nil {
		t.Fatal(err)
	}
	var result TranscribeResult
	json.Unmarshal(raw, &result)
	if result.Segments != 3 || result.Seconds != 3725.5 || result.Words != 13 {
		t.Errorf("the result: %+v", result)
	}
	if got, want := readBlob(t, store, result.Text), "The boulder rolls up the hill.\nAgain, every morning.\nThen the stone falls.\n"; got != want {
		t.Errorf("the transcript:\n%s\nwant:\n%s", got, want)
	}
	// Times that run past their stretch are held to it.
	want := "1\n00:00:00,000 --> 00:00:02,180\nThe boulder rolls up the hill.\n\n" +
		"2\n00:00:02,500 --> 00:00:07,172\nAgain, every morning.\n\n" +
		"3\n01:01:01,002 --> 01:02:05,500\nThen the stone falls.\n\n"
	if got := readBlob(t, store, result.Subtitles); got != want {
		t.Errorf("the subtitles:\n%s\nwant:\n%s", got, want)
	}

	// Nothing said is an empty transcript, not a failure.
	raw, err = Transcribe{}.Aggregate(ctx, store, [][]byte{outputs[2]})
	json.Unmarshal(raw, &result)
	if err != nil || readBlob(t, store, result.Text) != "" || readBlob(t, store, result.Subtitles) != "" || result.Words != 0 || result.Segments != 1 {
		t.Errorf("nothing said: %+v, %v", result, err)
	}

	missing := "bafkreigh2akiscaildcqabsyg3dfr6chu3fgpregiymsck7e7aqa4s52zy"
	garbled, _ := store.Put(ctx, strings.NewReader("not json"))
	for name, tt := range map[string]struct {
		outputs [][]byte
		blobs   Blobs
		want    string
	}{
		"an output that is not one":          {[][]byte{outputs[0], []byte("not json")}, store, "task 1 output"},
		"a task that transcribed nothing":    {[][]byte{mustJSON(ContainerOutput{})}, store, "task 0 left no transcript"},
		"a transcript that is not stored":    {[][]byte{mustJSON(ContainerOutput{Files: map[string]string{"segments.json": missing}})}, store, "task 0's transcript"},
		"a transcript that is garbled":       {[][]byte{mustJSON(ContainerOutput{Files: map[string]string{"segments.json": garbled.String()}})}, store, "task 0's transcript: invalid character"},
		"a transcript that cannot be stored": {outputs, &awkward{Blobs: store, failPut: 1}, "store the transcript: the store is full"},
		"subtitles that cannot be stored":    {outputs, &awkward{Blobs: store, failPut: 2}, "store the subtitles: the store is full"},
	} {
		if _, err := (Transcribe{}).Aggregate(ctx, tt.blobs, tt.outputs); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: %v, want an error containing %q", name, err, tt.want)
		}
	}
}

// transcribeIn runs a command in an image with a directory of this
// machine's at /work, as ffmpegIn does.
func transcribeIn(t *testing.T, image, dir string, command ...string) {
	t.Helper()
	args := append([]string{"run", "--rm", "--network", "none", "--user", strconv.Itoa(os.Getuid()) + ":" + strconv.Itoa(os.Getgid()), "--volume", dir + ":/work", image}, command...)
	if out, err := exec.Command("docker", args...).CombinedOutput(); err != nil {
		t.Fatalf("%v: %v\n%s", command, err, out)
	}
}

// Two sentences, spoken by espeak-ng with a pause between them, cut in
// two, transcribed by whisper in real containers, and put together: the
// words are there, in order, and the cut fell in the pause.
func TestRealSpeechIsTranscribedInStretches(t *testing.T) {
	requireRealImages(t)
	requireDocker(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	image := TranscribeImages["base"]
	// The images are built here if this machine has not got them, which
	// takes the network and some minutes: whisper.cpp is compiled.
	if err := haveImage(ctx, docker, image, transcribeDockerfile("base"), "transcription with the base model"); err != nil {
		t.Fatal(err)
	}
	const speaking = "FROM alpine:3.20\nRUN apk add --no-cache espeak-ng\n"
	sum := sha256.Sum256([]byte(speaking))
	speaker := "sisyphus-test-espeak:" + hex.EncodeToString(sum[:6])
	if err := haveImage(ctx, docker, speaker, speaking, "speaking"); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	os.Chmod(dir, 0o777)
	transcribeIn(t, speaker, dir, "sh", "-c", `espeak-ng -s 130 -w /work/first.wav "The boulder rolls up the hill. He pushes it again, every morning." &&
		espeak-ng -s 130 -w /work/second.wav "Then the stone falls back down to the valley."`)
	// Joined with two seconds of quiet, as Opus in Ogg, which is not what
	// whisper reads, so that ffmpeg has to decode it.
	transcribeIn(t, image, dir, "ffmpeg", "-nostdin", "-loglevel", "error", "-i", "/work/first.wav", "-f", "lavfi", "-t", "2", "-i", "anullsrc=r=22050:cl=mono",
		"-i", "/work/second.wav", "-filter_complex", "[0:a][1:a][2:a]concat=n=3:v=0:a=1", "-c:a", "libopus", "/work/speech.ogg")
	store := storage.NewMemory()
	source, err := os.Open(filepath.Join(dir, "speech.ogg"))
	if err != nil {
		t.Fatal(err)
	}
	speech, err := store.Put(ctx, source)
	source.Close()
	if err != nil {
		t.Fatal(err)
	}

	// The base model, which is quick; the default is the same pipeline
	// with a larger model.
	w := Transcribe{Confined: Confinement{ReadOnly: true}}
	payloads, err := w.Split(ctx, store, []byte(`{"input":"`+speech.String()+`","language":"en","model":"base"}`), 2)
	if err != nil {
		t.Fatal(err)
	}
	outputs := make([][]byte, len(payloads))
	said := new(heard)
	started := time.Now()
	for i, payload := range payloads {
		if outputs[i], err = w.Execute(WithReporter(ctx, said), store, payload); err != nil {
			t.Fatalf("stretch %d: %v\n%s", i, err, strings.Join(said.lines, "\n"))
		}
	}
	t.Logf("transcribed in %s", time.Since(started).Round(time.Second))
	log := strings.Join(said.lines, "\n")
	if !strings.Contains(log, "with the base model") || !strings.Contains(log, "heard 1 segments") && !strings.Contains(log, "heard 2 segments") {
		t.Errorf("the tasks' log:\n%s", log)
	}
	raw, err := w.Aggregate(ctx, store, outputs)
	if err != nil {
		t.Fatal(err)
	}
	var result TranscribeResult
	json.Unmarshal(raw, &result)
	text := strings.ToLower(readBlob(t, store, result.Text))
	t.Logf("heard: %q", text)
	for _, want := range []string{"hill", "morning", "stone", "falls", "valley"} {
		if !strings.Contains(text, want) {
			t.Errorf("the transcript has no %q: %q", want, text)
		}
	}
	if strings.Index(text, "hill") > strings.Index(text, "valley") {
		t.Errorf("the transcript is out of order: %q", text)
	}
	if result.Segments != 2 || result.Words < 12 || result.Seconds < 8 || result.Seconds > 20 {
		t.Errorf("the result: %+v", result)
	}
	// The cut fell in the pause, so the second stretch began with the
	// second sentence, a second or more into the quiet.
	var second ContainerOutput
	json.Unmarshal(outputs[1], &second)
	var stretch transcribeStretch
	json.Unmarshal([]byte(readBlob(t, store, second.Files["segments.json"])), &stretch)
	if first := stretch.Segments; len(first) == 0 || !strings.Contains(strings.ToLower(first[0].Text), "stone") || stretch.Language != "en" {
		t.Errorf("the second stretch: %+v", stretch)
	}
	subtitles := readBlob(t, store, result.Subtitles)
	if !strings.HasPrefix(subtitles, "1\n00:00:0") || !strings.Contains(subtitles, " --> ") || !strings.Contains(strings.ToLower(subtitles), "valley") {
		t.Errorf("the subtitles:\n%s", subtitles)
	}
	if stretch.Start < 3 {
		t.Errorf("the second stretch starts at %v", stretch.Start)
	}

	// Something that is not a recording is the task's failure.
	nonsense, _ := store.Put(ctx, strings.NewReader("this is not a recording"))
	bad, _ := w.Split(ctx, store, []byte(`{"input":"`+nonsense.String()+`"}`), 1)
	if _, err := w.Execute(ctx, store, bad[0]); err == nil || !strings.Contains(err.Error(), "exited with status") {
		t.Errorf("transcribing what is not a recording: %v", err)
	}
}
