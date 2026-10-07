package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sisyphus-network/Sisyphus/packages/storage"
)

func TestATranscodeJobIsAContainerJobWithTheImageAndCommandFixed(t *testing.T) {
	store := storage.NewMemory()
	ctx := context.Background()
	video, _ := store.Put(ctx, strings.NewReader("a video"))

	payloads, err := Transcode{}.Split(ctx, store, []byte(`{"input":"`+video.String()+`"}`), 4)
	if err != nil || len(payloads) != 4 {
		t.Fatalf("split: %d payloads, %v", len(payloads), err)
	}
	var task containerTask
	json.Unmarshal(payloads[3], &task)
	if task.Image != TranscodeImage || !strings.HasPrefix(TranscodeImage, "sisyphus-transcode:") || task.Input != video.String() || task.Index != 3 || task.Count != 4 {
		t.Errorf("the last task: %+v", task)
	}
	if len(task.Command) != 3 || task.Command[0] != "sh" || !strings.Contains(task.Command[2], "ffmpeg") {
		t.Errorf("its command: %q", task.Command)
	}
	// Unless said otherwise: the usual quality and speed, and the size left alone.
	if task.Env["CRF"] != "23" || task.Env["PRESET"] != "medium" || task.Env["HEIGHT"] != "0" || task.Network {
		t.Errorf("its settings: %v, network %v", task.Env, task.Network)
	}

	payloads, err = Transcode{}.Split(ctx, store, []byte(`{"input":"`+video.String()+`","height":480,"crf":30,"preset":"veryfast","segments":2}`), 4)
	if err != nil || len(payloads) != 2 {
		t.Fatalf("split as asked: %d payloads, %v", len(payloads), err)
	}
	json.Unmarshal(payloads[0], &task)
	if task.Env["CRF"] != "30" || task.Env["PRESET"] != "veryfast" || task.Env["HEIGHT"] != "480" {
		t.Errorf("settings as asked: %v", task.Env)
	}

	for params, want := range map[string]string{
		`not json`:                      "transcode parameters",
		`{}`:                            "input is not the CID of a stored video",
		`{"input":"somewhere"}`:         "input is not the CID of a stored video",
		`{"input":"CID","crf":52}`:      "crf is 52, and must be from 1 to 51",
		`{"input":"CID","crf":-1}`:      "crf is -1",
		`{"input":"CID","preset":"x"}`:  `preset is "x", and must be one of ultrafast`,
		`{"input":"CID","height":481}`:  "height is 481, and must be an even number",
		`{"input":"CID","height":-480}`: "height is -480",
	} {
		params = strings.Replace(params, "CID", video.String(), 1)
		if _, err := (Transcode{}).Split(ctx, store, []byte(params), 2); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want an error containing %q", params, err, want)
		}
	}
	if w, err := WithContainers().Get("transcode"); err != nil || w.Name() != "transcode" || !strings.Contains(WithContainers().Describe("transcode"), "H.264") {
		t.Errorf("the workload as registered: %v, %v", w, err)
	}
	if _, err := Builtin().Get("transcode"); err == nil {
		t.Error("a node that runs no containers offers to transcode")
	}
}

func TestTheTranscodingImageIsBuiltOnlyWhereItIsMissing(t *testing.T) {
	store := storage.NewMemory()
	ctx := context.Background()
	video, _ := store.Put(ctx, strings.NewReader("a video"))
	payloads, _ := Transcode{}.Split(ctx, store, []byte(`{"input":"`+video.String()+`"}`), 1)
	encodes := func(args []string, _, _ io.Writer) (int, error) {
		os.WriteFile(filepath.Join(volume(args, "/output"), "segment.ts"), []byte("encoded"), 0o644)
		return 0, nil
	}

	// A machine that has the image runs the task and builds nothing.
	has := &pretend{do: func(args []string, stdout, stderr io.Writer) (int, error) {
		if args[0] == "image" {
			return 0, nil
		}
		return encodes(args, stdout, stderr)
	}}
	raw, err := Transcode{Engine: has.engine}.Execute(ctx, store, payloads[0])
	if err != nil {
		t.Fatal(err)
	}
	var out ContainerOutput
	json.Unmarshal(raw, &out)
	if got := readBlob(t, store, out.Files["segment.ts"]); got != "encoded" {
		t.Errorf("the task's output: %q", got)
	}
	if len(has.asked) != 2 || strings.Join(has.asked[0], " ") != "image inspect "+TranscodeImage || has.asked[1][0] != "run" {
		t.Errorf("with the image there, the engine was asked %v", has.asked)
	}

	// One that has not builds it first, from a file that says how, and
	// tells whoever is following the task that it is doing so.
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
		return encodes(args, stdout, stderr)
	}}
	said := new(heard)
	if _, err := (Transcode{Engine: lacks.engine}).Execute(WithReporter(ctx, said), store, payloads[0]); err != nil {
		t.Fatal(err)
	}
	if len(lacks.asked) != 3 || strings.Join(lacks.asked[1][:3], " ") != "build --tag "+TranscodeImage || !strings.Contains(recipe, "apk add --no-cache ffmpeg") {
		t.Errorf("with the image missing, the engine was asked %v with the recipe %q", lacks.asked, recipe)
	}
	// The command is a whole script, of which the log has the first line.
	if log := strings.Join(said.lines, "\n"); !strings.Contains(log, "building the image") || !strings.Contains(log, "running "+TranscodeImage+" sh -c set -eu ...") || strings.Contains(log, "ffprobe") {
		t.Errorf("the task's log: %q", said.lines)
	}
	if left, _ := filepath.Glob(filepath.Join(os.TempDir(), "sisyphus-image-*")); len(left) != 0 {
		t.Errorf("build directories left behind: %v", left)
	}

	// A build that fails is the task's failure, in the build's own words.
	for name, tt := range map[string]struct {
		build func(stderr io.Writer) (int, error)
		want  string
	}{
		"a build that fails": {func(stderr io.Writer) (int, error) {
			io.WriteString(stderr, "fetching packages\nERROR: unable to select packages\n")
			return 1, nil
		}, "build the image transcoding runs in: ERROR: unable to select packages"},
		"no engine to build with": {func(io.Writer) (int, error) { return 0, errors.New("docker: not found") }, "build the image transcoding runs in: docker: not found"},
	} {
		broken := &pretend{do: func(args []string, _, stderr io.Writer) (int, error) {
			if args[0] == "image" {
				return 0, errors.New("docker: not found")
			}
			return tt.build(stderr)
		}}
		if _, err := (Transcode{Engine: broken.engine}).Execute(ctx, store, payloads[0]); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: %v, want an error containing %q", name, err, tt.want)
		}
	}
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "no-such-directory"))
	if _, err := (Transcode{Engine: lacks.engine}).Execute(ctx, store, payloads[0]); err == nil || !strings.Contains(err.Error(), "make a directory to build the transcoding image in") {
		t.Errorf("with no temporary directory: %v", err)
	}
}

func TestTheEncodedStretchesAreJoinedInOrder(t *testing.T) {
	store := storage.NewMemory()
	ctx := context.Background()
	stretch := func(index int, content string) []byte {
		c, _ := store.Put(ctx, strings.NewReader(content))
		return mustJSON(ContainerOutput{Index: index, Files: map[string]string{"segment.ts": c.String()}})
	}
	outputs := [][]byte{stretch(0, "one,"), stretch(1, "two,"), stretch(2, "three")}

	raw, err := Transcode{}.Aggregate(ctx, store, outputs)
	if err != nil {
		t.Fatal(err)
	}
	var result TranscodeResult
	json.Unmarshal(raw, &result)
	if got := readBlob(t, store, result.Output); got != "one,two,three" || result.Segments != 3 || result.Bytes != 13 {
		t.Errorf("the whole: %q, %+v", got, result)
	}

	missing := "bafkreigh2akiscaildcqabsyg3dfr6chu3fgpregiymsck7e7aqa4s52zy"
	for name, tt := range map[string]struct {
		outputs [][]byte
		blobs   Blobs
		want    string
	}{
		"an output that is not one":     {[][]byte{outputs[0], []byte("not json")}, store, "task 1 output"},
		"a task that encoded nothing":   {[][]byte{mustJSON(ContainerOutput{})}, store, "task 0 left no encoded video"},
		"a stretch that is not stored":  {[][]byte{mustJSON(ContainerOutput{Files: map[string]string{"segment.ts": missing}})}, store, "task 0's encoded video"},
		"a whole that cannot be stored": {outputs, &awkward{Blobs: store, failPut: 1}, "store the encoded video: the store is full"},
	} {
		if _, err := (Transcode{}).Aggregate(ctx, tt.blobs, tt.outputs); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: %v, want an error containing %q", name, err, tt.want)
		}
	}
}

// ffmpegIn runs a command in the transcoding image with a directory of this
// machine's at /work, and returns what it printed.
func ffmpegIn(t *testing.T, dir string, command ...string) string {
	t.Helper()
	args := append([]string{"run", "--rm", "--network", "none", "--user", strconv.Itoa(os.Getuid()) + ":" + strconv.Itoa(os.Getgid()), "--volume", dir + ":/work", TranscodeImage}, command...)
	out, err := exec.Command("docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %v\n%s", command, err, out)
	}
	return string(out)
}

// A real video, cut in three, encoded by ffmpeg in real containers, and
// joined: every frame is there, at the size asked for.
func TestARealVideoIsTranscodedInStretches(t *testing.T) {
	requireDocker(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	// The image is built here if this machine has not got it, which takes
	// the network and a minute.
	if err := haveTranscodeImage(ctx, docker); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	os.Chmod(dir, 0o777)
	// Six seconds of test card and tone, 144 frames.
	ffmpegIn(t, dir, "ffmpeg", "-nostdin", "-loglevel", "error", "-f", "lavfi", "-i", "testsrc=duration=6:size=640x360:rate=24",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=6", "-c:v", "libx264", "-preset", "ultrafast", "-c:a", "aac", "-shortest", "/work/source.mp4")
	store := storage.NewMemory()
	source, err := os.Open(filepath.Join(dir, "source.mp4"))
	if err != nil {
		t.Fatal(err)
	}
	video, err := store.Put(ctx, source)
	source.Close()
	if err != nil {
		t.Fatal(err)
	}

	w := Transcode{}
	payloads, err := w.Split(ctx, store, []byte(`{"input":"`+video.String()+`","height":180,"preset":"ultrafast","crf":30}`), 3)
	if err != nil {
		t.Fatal(err)
	}
	outputs := make([][]byte, len(payloads))
	said := new(heard)
	for i, payload := range payloads {
		if outputs[i], err = w.Execute(WithReporter(ctx, said), store, payload); err != nil {
			t.Fatalf("stretch %d: %v", i, err)
		}
	}
	if log := strings.Join(said.lines, "\n"); !strings.Contains(log, "encoding 2.0") || !strings.Contains(log, "from 4.0") {
		t.Errorf("the tasks' log:\n%s", log)
	}
	raw, err := w.Aggregate(ctx, store, outputs)
	if err != nil {
		t.Fatal(err)
	}
	var result TranscodeResult
	json.Unmarshal(raw, &result)
	if err := os.WriteFile(filepath.Join(dir, "whole.ts"), []byte(readBlob(t, store, result.Output)), 0o644); err != nil {
		t.Fatal(err)
	}
	if result.Segments != 3 || result.Bytes == 0 {
		t.Errorf("the result: %+v", result)
	}

	probe := ffmpegIn(t, dir, "ffprobe", "-v", "error", "-select_streams", "v:0", "-count_frames",
		"-show_entries", "stream=codec_name,height,nb_read_frames", "-of", "default=nw=1", "/work/whole.ts")
	for _, want := range []string{"codec_name=h264", "height=180", "nb_read_frames=144"} {
		if !strings.Contains(probe, want) {
			t.Errorf("the encoded video is not %s:\n%s", want, probe)
		}
	}
	if sound := ffmpegIn(t, dir, "ffprobe", "-v", "error", "-select_streams", "a:0", "-show_entries", "stream=codec_name", "-of", "default=nw=1", "/work/whole.ts"); !strings.Contains(sound, "codec_name=aac") {
		t.Errorf("the encoded sound: %s", sound)
	}
	// It plays from start to end as one video.
	ffmpegIn(t, dir, "ffmpeg", "-nostdin", "-v", "fatal", "-i", "/work/whole.ts", "-f", "null", "-")

	// Something that is not a video is the task's failure, in ffmpeg's words.
	nonsense, _ := store.Put(ctx, strings.NewReader("this is not a video"))
	bad, _ := w.Split(ctx, store, []byte(`{"input":"`+nonsense.String()+`"}`), 1)
	if _, err := w.Execute(ctx, store, bad[0]); err == nil || !strings.Contains(err.Error(), "exited with status") {
		t.Errorf("transcoding what is not a video: %v", err)
	}
}
