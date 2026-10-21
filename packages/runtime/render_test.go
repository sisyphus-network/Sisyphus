package runtime

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"image/png"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sisyphus-network/Sisyphus/packages/storage"
)

func TestARenderJobIsAContainerJobWithTheImageAndScriptFixed(t *testing.T) {
	store := storage.NewMemory()
	ctx := context.Background()
	scene, _ := store.Put(ctx, strings.NewReader("BLENDER-v405"))
	input := `"input":"` + scene.String() + `"`

	payloads, err := Render{}.Split(ctx, store, []byte(`{`+input+`}`), 4)
	if err != nil || len(payloads) != 4 {
		t.Fatalf("split: %d payloads, %v", len(payloads), err)
	}
	var task containerTask
	json.Unmarshal(payloads[2], &task)
	if task.Image != RenderImage || !strings.HasPrefix(RenderImage, "sisyphus-render:") || task.Input != scene.String() || task.Index != 2 || task.Count != 4 || task.Network {
		t.Errorf("the third task: %+v", task)
	}
	// Python in the file is not run, whatever it asks, and the script's
	// failure is Blender's.
	if len(task.Command) != 8 || task.Command[0] != "/opt/blender/blender" || !slices.Contains(task.Command, "--disable-autoexec") || slices.Contains(task.Command, "-y") || slices.Contains(task.Command, "--enable-autoexec") ||
		!slices.Contains(task.Command, "--background") || task.Command[6] != "--python-expr" || !strings.Contains(task.Command[7], "import bpy") {
		t.Errorf("its command: %q", task.Command)
	}
	// Unless said otherwise: Cycles, PNG, by frames, and the file's own
	// frames, samples and size.
	want := map[string]string{"SPLIT": "frames", "ENGINE": "CYCLES", "FORMAT": "PNG", "FRAME_START": "", "FRAME_END": "", "SAMPLES": "", "PERCENT": ""}
	if !equalEnv(task.Env, want) {
		t.Errorf("its settings: %v", task.Env)
	}

	payloads, err = Render{}.Split(ctx, store, []byte(`{`+input+`,"frame_start":0,"frame_end":9,"engine":"BLENDER_EEVEE_NEXT","samples":16,"resolution_percent":50,"format":"EXR","tasks":3}`), 8)
	if err != nil || len(payloads) != 3 {
		t.Fatalf("split as asked: %d payloads, %v", len(payloads), err)
	}
	json.Unmarshal(payloads[0], &task)
	want = map[string]string{"SPLIT": "frames", "ENGINE": "BLENDER_EEVEE_NEXT", "FORMAT": "EXR", "FRAME_START": "0", "FRAME_END": "9", "SAMPLES": "16", "PERCENT": "50"}
	if !equalEnv(task.Env, want) || task.Count != 3 {
		t.Errorf("settings as asked: %v, %d tasks", task.Env, task.Count)
	}

	// Where the frames are known there are no more tasks than frames, and
	// no fewer than it takes for none to have more than a thousand.
	for params, tasks := range map[string]int{
		`,"frame_start":1,"frame_end":3`:                 3,
		`,"frame_start":1,"frame_end":3,"tasks":50`:      3,
		`,"frame_start":1,"frame_end":2500`:              8,
		`,"frame_start":1,"frame_end":2500,"tasks":1`:    3,
		`,"frame_start":1,"frame_end":2500,"tasks":10`:   10,
		`,"frame_start":5`:                               8,
		`,"split":"tiles","frame_start":1,"frame_end":3`: 8,
		`,"split":"tiles","tasks":2`:                     2,
	} {
		if payloads, err := (Render{}).Split(ctx, store, []byte(`{`+input+params+`}`), 8); err != nil || len(payloads) != tasks {
			t.Errorf("%s: %d tasks, %v; want %d", params, len(payloads), err, tasks)
		}
	}
	payloads, _ = Render{}.Split(ctx, store, []byte(`{`+input+`,"split":"tiles","frame_start":12}`), 1)
	json.Unmarshal(payloads[0], &task)
	if task.Env["SPLIT"] != "tiles" || task.Env["FRAME_START"] != "12" {
		t.Errorf("split by tiles: %v", task.Env)
	}

	for params, want := range map[string]string{
		`not json`:                                       "render parameters",
		`{"input":"somewhere"}`:                          "input is not the CID of a stored .blend file",
		`{` + input + `,"frame_start":-1}`:               "frame_start is -1, and must be from 0 to 1048574",
		`{` + input + `,"frame_end":1048575}`:            "frame_end is 1048575",
		`{` + input + `,"engine":"WORKBENCH"}`:           `engine is "WORKBENCH", and must be one of CYCLES, BLENDER_EEVEE_NEXT`,
		`{` + input + `,"format":"JPEG"}`:                `format is "JPEG", and must be one of PNG, EXR`,
		`{` + input + `,"split":"halves"}`:               `split is "halves", and must be one of frames, tiles`,
		`{` + input + `,"split":"tiles","format":"EXR"}`: "split by tiles, the format must be PNG",
		`{` + input + `,"frame_start":5,"frame_end":4}`:  "frame_end is 4, before frame_start, 5",
		`{` + input + `,"samples":-1}`:                   "samples is -1, and must be from 1 to 16777216",
		`{` + input + `,"samples":16777217}`:             "samples is 16777217",
		`{` + input + `,"resolution_percent":-1}`:        "resolution_percent is -1, and must be from 1 to 1000",
		`{` + input + `,"resolution_percent":1001}`:      "resolution_percent is 1001",
		`{` + input + `,"tasks":-1}`:                     "tasks is -1, and must be from 1 to 10000",
		`{` + input + `,"tasks":10001}`:                  "tasks is 10001",
	} {
		if _, err := (Render{}).Split(ctx, store, []byte(params), 2); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want an error containing %q", params, err, want)
		}
	}
	if w, err := WithContainers().Get("render"); err != nil || w.Name() != "render" || !strings.Contains(WithContainers().Describe("render"), "Blender") {
		t.Errorf("the workload as registered: %v, %v", w, err)
	}
	if !WithContainers().InContainers("render") {
		t.Error("render is not said to run in containers")
	}
	if _, err := Builtin().Get("render"); err == nil {
		t.Error("a node that runs no containers offers to render")
	}
	if !(Render{}).Varies() {
		t.Error("render says two honest runs give the same result")
	}
}

// equalEnv reports whether env is want, where an empty value in want is a
// variable set to nothing.
func equalEnv(env, want map[string]string) bool {
	if len(env) != len(want) {
		return false
	}
	for k, v := range want {
		if got, ok := env[k]; !ok || got != v {
			return false
		}
	}
	return true
}

func TestTheRenderImageIsBuiltOnlyWhereItIsMissing(t *testing.T) {
	store := storage.NewMemory()
	ctx := context.Background()
	scene, _ := store.Put(ctx, strings.NewReader("BLENDER-v405"))
	payloads, _ := Render{}.Split(ctx, store, []byte(`{"input":"`+scene.String()+`"}`), 1)
	renders := func(args []string, _, _ io.Writer) (int, error) {
		os.WriteFile(filepath.Join(volume(args, "/output"), "summary.json"), []byte(`{}`), 0o644)
		return 0, nil
	}
	has := &pretend{do: func(args []string, stdout, stderr io.Writer) (int, error) {
		if args[0] == "image" {
			return 0, nil
		}
		return renders(args, stdout, stderr)
	}}
	if _, err := (Render{Engine: has.engine}).Execute(ctx, store, payloads[0]); err != nil {
		t.Fatal(err)
	}
	if len(has.asked) != 2 || strings.Join(has.asked[0], " ") != "image inspect "+RenderImage || has.asked[1][0] != "run" {
		t.Errorf("with the image there, the engine was asked %v", has.asked)
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
		return renders(args, stdout, stderr)
	}}
	said := new(heard)
	if _, err := (Render{Engine: lacks.engine}).Execute(WithReporter(ctx, said), store, payloads[0]); err != nil {
		t.Fatal(err)
	}
	if len(lacks.asked) != 3 || strings.Join(lacks.asked[1][:3], " ") != "build --tag "+RenderImage || !strings.Contains(recipe, "blender-4.5.14-linux-x64.tar.xz") || !strings.Contains(recipe, "sha256sum -c") {
		t.Errorf("with the image missing, the engine was asked %v with the recipe %q", lacks.asked, recipe)
	}
	if log := strings.Join(said.lines, "\n"); !strings.Contains(log, "building the image rendering runs in") {
		t.Errorf("the task's log: %q", said.lines)
	}

	broken := &pretend{do: func(args []string, _, stderr io.Writer) (int, error) {
		if args[0] == "image" {
			return 1, nil
		}
		io.WriteString(stderr, "sha256sum: WARNING: 1 computed checksum did NOT match\n")
		return 1, nil
	}}
	if _, err := (Render{Engine: broken.engine}).Execute(ctx, store, payloads[0]); err == nil || !strings.Contains(err.Error(), "build the image rendering runs in: sha256sum: WARNING") {
		t.Errorf("a build that fails: %v", err)
	}
	if _, err := (Render{Engine: has.engine}).Execute(ctx, store, []byte("not json")); err == nil {
		t.Error("a payload that is not one was run")
	}
}

// pngOf encodes a picture of the given size, each row the colour its
// number gives, from the top.
func pngOf(width, height, from int) []byte {
	picture := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y := range height {
		for x := range width {
			picture.Set(x, y, color.NRGBA{uint8(from + y), uint8(x), 0, 255})
		}
	}
	var out bytes.Buffer
	png.Encode(&out, picture)
	return out.Bytes()
}

// renderTask is the output of a task that left these files, and a summary
// of them.
func renderTask(t *testing.T, store *storage.Store, index int, summary string, files map[string][]byte) []byte {
	t.Helper()
	ctx := context.Background()
	stored := map[string]string{}
	s, _ := store.Put(ctx, strings.NewReader(summary))
	stored["summary.json"] = s.String()
	for name, content := range files {
		c, _ := store.Put(ctx, bytes.NewReader(content))
		stored[name] = c.String()
	}
	return mustJSON(ContainerOutput{Index: index, Files: stored})
}

func TestRenderedFramesAreListedInOrderAndArchived(t *testing.T) {
	store := storage.NewMemory()
	ctx := context.Background()
	frame := func(n int) []byte { return pngOf(4, 2, n) }
	outputs := [][]byte{
		renderTask(t, store, 0, `{"split":"frames","frames":[9,10],"seconds":1.5,"width":4,"height":2,"engine":"CYCLES"}`,
			map[string][]byte{"frame_0000010.png": frame(10), "frame_0000009.png": frame(9)}),
		// A task with no frames left to it, where there were more tasks than frames.
		renderTask(t, store, 1, `{"split":"frames","frames":[],"seconds":0,"width":4,"height":2,"engine":"CYCLES"}`, nil),
		renderTask(t, store, 2, `{"split":"frames","frames":[11],"seconds":2,"width":4,"height":2,"engine":"CYCLES"}`,
			map[string][]byte{"frame_0000011.png": frame(11)}),
	}
	raw, err := Render{}.Aggregate(ctx, store, outputs)
	if err != nil {
		t.Fatal(err)
	}
	var result RenderResult
	json.Unmarshal(raw, &result)
	if result.Split != "frames" || result.Frames != 3 || len(result.Images) != 3 || result.Width != 4 || result.Height != 2 || result.Engine != "CYCLES" || result.Seconds != 3.5 || result.Image != "" {
		t.Fatalf("the result: %+v", result)
	}
	for i, n := range []int{9, 10, 11} {
		if got := readBlob(t, store, result.Images[i]); got != string(frame(n)) {
			t.Errorf("frame %d is not frame %d", i, n)
		}
	}
	if got := (Render{}).ResultBlobs(raw); !slices.Equal(got, result.Images) {
		t.Errorf("the blobs kept with the result: %v", got)
	}
	if got := (Render{}).ResultBlobs([]byte("not json")); len(got) != 0 {
		t.Errorf("the blobs of no result: %v", got)
	}
	zipped := []byte(readBlob(t, store, result.Archive))
	archive, err := zip.NewReader(bytes.NewReader(zipped), int64(len(zipped)))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range archive.File {
		names = append(names, f.Name)
		r, _ := f.Open()
		content, _ := io.ReadAll(r)
		if f.Method != zip.Store || !bytes.Equal(content, frame(map[string]int{"frame_0000009.png": 9, "frame_0000010.png": 10, "frame_0000011.png": 11}[f.Name])) {
			t.Errorf("%s in the archive is not the frame", f.Name)
		}
	}
	if !slices.Equal(names, []string{"frame_0000009.png", "frame_0000010.png", "frame_0000011.png"}) {
		t.Errorf("the archive has %v", names)
	}

	missing := "bafkreigh2akiscaildcqabsyg3dfr6chu3fgpregiymsck7e7aqa4s52zy"
	garbled, _ := store.Put(ctx, strings.NewReader("not json"))
	for name, tt := range map[string]struct {
		outputs [][]byte
		blobs   Blobs
		want    string
	}{
		"an output that is not one":        {[][]byte{outputs[0], []byte("not json")}, store, "task 1 output"},
		"a task with no summary":           {[][]byte{mustJSON(ContainerOutput{})}, store, "task 0 left no summary"},
		"a summary that is not there":      {[][]byte{mustJSON(ContainerOutput{Files: map[string]string{"summary.json": missing}})}, store, "task 0's summary"},
		"a summary that is garbled":        {[][]byte{mustJSON(ContainerOutput{Files: map[string]string{"summary.json": garbled.String()}})}, store, "task 0's summary: invalid character"},
		"an archive that cannot be stored": {outputs, &awkward{Blobs: store, failPut: 1}, "store the frames as one zip file: the store is full"},
	} {
		if _, err := (Render{}).Aggregate(ctx, tt.blobs, tt.outputs); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: %v, want an error containing %q", name, err, tt.want)
		}
	}
	summary, _ := store.Put(ctx, strings.NewReader(`{"split":"frames"}`))
	for name, frame := range map[string]string{"a frame that is not stored": missing, "a frame that is no CID": "nothing"} {
		lost := mustJSON(ContainerOutput{Files: map[string]string{"summary.json": summary.String(), "frame_0000001.png": frame}})
		if _, err := (Render{}).Aggregate(ctx, store, [][]byte{lost}); err == nil || !strings.Contains(err.Error(), "store the frames as one zip file: frame frame_0000001.png") {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestRenderedStripsArePutTogether(t *testing.T) {
	store := storage.NewMemory()
	ctx := context.Background()
	strip := func(index int, rows, from int) []byte {
		return renderTask(t, store, index, `{"split":"tiles","frames":[1],"seconds":1,"width":3,"height":5,"engine":"CYCLES"}`,
			map[string][]byte{"tile.png": pngOf(3, rows, from)})
	}
	outputs := [][]byte{strip(0, 2, 0), strip(1, 3, 2)}
	raw, err := Render{}.Aggregate(ctx, store, outputs)
	if err != nil {
		t.Fatal(err)
	}
	var result RenderResult
	json.Unmarshal(raw, &result)
	if result.Split != "tiles" || result.Frames != 1 || result.Tiles != 2 || result.Width != 3 || result.Height != 5 || result.Seconds != 2 || result.Images != nil || result.Archive != "" {
		t.Fatalf("the result: %+v", result)
	}
	whole, err := png.Decode(strings.NewReader(readBlob(t, store, result.Image)))
	if err != nil {
		t.Fatal(err)
	}
	if whole.Bounds() != image.Rect(0, 0, 3, 5) {
		t.Fatalf("the whole frame is %v", whole.Bounds())
	}
	// Each row is the colour of its number: the strips are in order.
	for y := range 5 {
		if r, g, _, _ := whole.At(2, y).RGBA(); r>>8 != uint32(y) || g>>8 != 2 {
			t.Errorf("row %d is %v", y, whole.At(2, y))
		}
	}

	missing := "bafkreigh2akiscaildcqabsyg3dfr6chu3fgpregiymsck7e7aqa4s52zy"
	summary := `{"split":"tiles"}`
	narrow := renderTask(t, store, 1, summary, map[string][]byte{"tile.png": pngOf(2, 3, 0)})
	notPNG := renderTask(t, store, 0, summary, map[string][]byte{"tile.png": []byte("not a picture")})
	s, _ := store.Put(ctx, strings.NewReader(summary))
	lost := mustJSON(ContainerOutput{Files: map[string]string{"summary.json": s.String(), "tile.png": missing}})
	huge := renderTask(t, store, 1, summary, map[string][]byte{"tile.png": pngHeader(1<<14, 1<<14)})
	for name, tt := range map[string]struct {
		outputs [][]byte
		blobs   Blobs
		want    string
	}{
		"a strip not rendered":          {[][]byte{outputs[0], renderTask(t, store, 1, summary, nil)}, store, "strip 1 was not rendered"},
		"a strip not stored":            {[][]byte{lost}, store, "strip 0: "},
		"a strip that is no picture":    {[][]byte{notPNG}, store, "strip 0: png: invalid format"},
		"a strip of another width":      {[][]byte{outputs[0], narrow}, store, "strip 1 is 2 pixels wide, and strip 0 is 3"},
		"a frame too large":             {[][]byte{outputs[0], huge}, store, "strip 1: a frame of 16384x16386 pixels is more than the 134217728"},
		"a whole that cannot be stored": {outputs, &awkward{Blobs: store, failPut: 1}, "store the whole frame: the store is full"},
	} {
		if _, err := (Render{}).Aggregate(ctx, tt.blobs, tt.outputs); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: %v, want an error containing %q", name, err, tt.want)
		}
	}
	if _, err := decodeStrip(errReader{}, 0); err == nil {
		t.Error("a strip that cannot be read was decoded")
	}
}

// pngHeader is the start of a PNG file of the given size: enough to say
// how large it is, and no more.
func pngHeader(width, height uint32) []byte {
	var ihdr [13]byte
	binary.BigEndian.PutUint32(ihdr[0:], width)
	binary.BigEndian.PutUint32(ihdr[4:], height)
	ihdr[8], ihdr[9] = 8, 6
	var out bytes.Buffer
	out.WriteString("\x89PNG\r\n\x1a\n")
	binary.Write(&out, binary.BigEndian, uint32(len(ihdr)))
	chunk := append([]byte("IHDR"), ihdr[:]...)
	out.Write(chunk)
	binary.Write(&out, binary.BigEndian, crc32.ChecksumIEEE(chunk))
	return out.Bytes()
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("the disk is gone") }

// makeScene is a Python script that has Blender make a tiny scene, a cube
// lit by the sun, four frames of 64 by 64 pixels at four samples, and
// save it in /work. It carries a script of its own that Blender would run
// on opening the file, were it let to.
const makeScene = `import bpy
bpy.ops.wm.read_factory_settings(use_empty=True)
scene = bpy.context.scene
bpy.ops.mesh.primitive_cube_add()
bpy.ops.object.light_add(type="SUN", location=(4, -4, 6))
bpy.ops.object.camera_add(location=(6, -6, 5), rotation=(1.0, 0, 0.785))
scene.camera = bpy.context.object
scene.render.resolution_x = scene.render.resolution_y = 64
scene.render.resolution_percentage = 100
scene.frame_start, scene.frame_end = 1, 4
scene.cycles.samples = 4
script = bpy.data.texts.new("autorun.py")
script.write("open('/output/ran', 'w').write('the file ran its own Python')\n")
script.use_module = True
bpy.ops.wm.save_as_mainfile(filepath="/work/scene.blend")
`

// A real scene, made by Blender and rendered by it in real containers,
// by frames and by strips put together, with the Python it carries left
// unrun.
func TestARealSceneIsRendered(t *testing.T) {
	requireDocker(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	// The image is built here if this machine has not got it, which takes
	// the network and a few minutes.
	if err := haveImage(ctx, docker, RenderImage, renderDockerfile, "rendering"); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	os.Chmod(dir, 0o777)
	os.WriteFile(filepath.Join(dir, "make.py"), []byte(makeScene), 0o644)
	made, err := exec.CommandContext(ctx, "docker", "run", "--rm", "--network", "none", "--user", strconv.Itoa(os.Getuid())+":"+strconv.Itoa(os.Getgid()), "--env", "HOME=/tmp",
		"--volume", dir+":/work", RenderImage, "/opt/blender/blender", "--background", "--factory-startup", "--python", "/work/make.py").CombinedOutput()
	if err != nil {
		t.Fatalf("make the scene: %v\n%s", err, made)
	}
	store := storage.NewMemory()
	file, err := os.Open(filepath.Join(dir, "scene.blend"))
	if err != nil {
		t.Fatal(err)
	}
	scene, err := store.Put(ctx, file)
	file.Close()
	if err != nil {
		t.Fatal(err)
	}
	w := Render{Confined: Confinement{ReadOnly: true}}
	run := func(params string, parts int) RenderResult {
		t.Helper()
		payloads, err := w.Split(ctx, store, []byte(`{"input":"`+scene.String()+`",`+params+`}`), parts)
		if err != nil {
			t.Fatal(err)
		}
		outputs := make([][]byte, len(payloads))
		said := new(heard)
		for i, payload := range payloads {
			if outputs[i], err = w.Execute(WithReporter(ctx, said), store, payload); err != nil {
				t.Fatalf("task %d: %v\n%s", i, err, strings.Join(said.lines, "\n"))
			}
			var out ContainerOutput
			json.Unmarshal(outputs[i], &out)
			if _, ran := out.Files["ran"]; ran {
				t.Errorf("task %d ran the Python the file carries", i)
			}
		}
		// Blender says so as it skips the script.
		if log := strings.Join(said.lines, "\n"); !strings.Contains(log, "skipping 'autorun.py'") {
			t.Errorf("the tasks' log:\n%s", log)
		}
		raw, err := w.Aggregate(ctx, store, outputs)
		if err != nil {
			t.Fatal(err)
		}
		var result RenderResult
		json.Unmarshal(raw, &result)
		if result.Engine != "CYCLES" || result.Width != 64 || result.Height != 64 || result.Seconds <= 0 {
			t.Errorf("%s: %+v", params, result)
		}
		return result
	}
	size := func(id string) image.Rectangle {
		t.Helper()
		picture, err := png.Decode(strings.NewReader(readBlob(t, store, id)))
		if err != nil {
			t.Fatal(err)
		}
		return picture.Bounds()
	}

	// By frames, the file's four, in two tasks of two.
	frames := run(`"split":"frames"`, 2)
	if frames.Frames != 4 || len(frames.Images) != 4 {
		t.Fatalf("by frames: %+v", frames)
	}
	for i, id := range frames.Images {
		if got := size(id); got != image.Rect(0, 0, 64, 64) {
			t.Errorf("frame %d is %v", i, got)
		}
	}
	zipped := readBlob(t, store, frames.Archive)
	archive, err := zip.NewReader(strings.NewReader(zipped), int64(len(zipped)))
	if err != nil || len(archive.File) != 4 || archive.File[3].Name != "frame_0000004.png" {
		t.Errorf("the archive: %v, %v", archive, err)
	}

	// A first frame past the file's last is the task's failure, in the
	// script's words.
	bad, _ := w.Split(ctx, store, []byte(`{"input":"`+scene.String()+`","frame_start":10}`), 1)
	if _, err := w.Execute(ctx, store, bad[0]); err == nil || !strings.Contains(err.Error(), "exited with status 1: the last frame, 4, comes before the first, 10") {
		t.Errorf("rendering from past the last frame: %v", err)
	}

	// By tiles, frame two, in two strips of 32 rows put back together.
	tiles := run(`"split":"tiles","frame_start":2,"samples":2`, 2)
	if tiles.Frames != 1 || tiles.Tiles != 2 || size(tiles.Image) != image.Rect(0, 0, 64, 64) {
		t.Errorf("by tiles: %+v", tiles)
	}
}
