package runtime

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sisyphus-network/Sisyphus/packages/storage"
)

// Heads of each format a picture is taken in, enough to be told by.
var pictureHeads = map[string]string{
	"png":  "\x89PNG\r\n\x1a\nrest",
	"jpeg": "\xff\xd8\xff\xe0rest",
	"webp": "RIFF\x10\x00\x00\x00WEBPVP8 ",
	"tiff": "II*\x00rest",
	"gif":  "GIF89arest",
}

func TestAnImagesJobIsAContainerJobWithTheImageAndScriptFixed(t *testing.T) {
	store := storage.NewMemory()
	ctx := context.Background()
	var pictures []string
	for _, kind := range []string{"png", "jpeg", "webp", "tiff", "gif"} {
		pictures = append(pictures, storedCID(t, store, pictureHeads[kind]))
	}
	inputs := `"inputs":["` + strings.Join(pictures, `","`) + `"]`

	payloads, err := Images{}.Split(ctx, store, []byte(`{`+inputs+`}`), 2)
	if err != nil || len(payloads) != 2 {
		t.Fatalf("split: %d payloads, %v", len(payloads), err)
	}
	first, second := trajTaskOf(payloads[0]), trajTaskOf(payloads[1])
	if first.Image != ImagesImage || !strings.HasPrefix(ImagesImage, "sisyphus-images:") || first.Input != "" || first.Network || first.Count != 2 || second.Index != 1 {
		t.Errorf("the first task: %+v", first)
	}
	if len(first.Command) != 3 || first.Command[0] != "sh" || first.Command[2] != imagesScript || !strings.Contains(imagesScript, "vipsthumbnail") || !strings.Contains(imagesScript, "VIPS_BLOCK_UNTRUSTED=1") {
		t.Errorf("its command: %q", first.Command)
	}
	// Unless said otherwise: each in its own format and size, at quality
	// 85, with its metadata taken out.
	want := map[string]string{"FORMAT": "same", "WIDTH": "0", "HEIGHT": "0", "ENLARGE": "false", "QUALITY": "85", "KEEP": "false", "SIZE": "10000000x10000000>", "PICTURES": "5"}
	if !equalEnv(first.Env, want) {
		t.Errorf("its settings: %v", first.Env)
	}
	// Each task is given only its own run of pictures, named by what each
	// is.
	if want := map[string]string{"image-000.png": pictures[0], "image-001.jpg": pictures[1]}; !equalEnv(first.Inputs, want) {
		t.Errorf("the first task's pictures: %v", first.Inputs)
	}
	if want := map[string]string{"image-002.webp": pictures[2], "image-003.tif": pictures[3], "image-004.gif": pictures[4]}; !equalEnv(second.Inputs, want) {
		t.Errorf("the second task's pictures: %v", second.Inputs)
	}

	for params, want := range map[string]map[string]string{
		`{` + inputs + `,"width":800,"height":600,"format":"webp","quality":70,"keep_metadata":true,"tasks":5}`: {"SIZE": "800x600>", "FORMAT": "webp", "QUALITY": "70", "KEEP": "true"},
		`{` + inputs + `,"width":1024}`:                {"SIZE": "1024x>"},
		`{` + inputs + `,"height":300,"enlarge":true}`: {"SIZE": "x300", "ENLARGE": "true"},
	} {
		payloads, err := Images{}.Split(ctx, store, []byte(params), 8)
		if err != nil {
			t.Fatalf("%s: %v", params, err)
		}
		env := trajTaskOf(payloads[0]).Env
		for k, v := range want {
			if env[k] != v {
				t.Errorf("%s: %s is %q, want %q", params, k, env[k], v)
			}
		}
	}
	// Never more tasks than pictures, and one picture alone is one task.
	if payloads, _ := (Images{}).Split(ctx, store, []byte(`{`+inputs+`,"tasks":9}`), 9); len(payloads) != 5 {
		t.Errorf("five pictures in %d tasks", len(payloads))
	}
	if payloads, _ := (Images{}).Split(ctx, store, []byte(`{"input":"`+pictures[1]+`"}`), 4); len(payloads) != 1 || !equalEnv(trajTaskOf(payloads[0]).Inputs, map[string]string{"image-000.jpg": pictures[1]}) {
		t.Errorf("one picture: %d tasks", len(payloads))
	}
	// Enough tasks that none is given more pictures than a task may have.
	many := `"inputs":["` + strings.Repeat(pictures[0]+`","`, 40) + pictures[0] + `"]`
	if payloads, _ := (Images{}).Split(ctx, store, []byte(`{`+many+`,"tasks":1}`), 1); len(payloads) != 3 {
		t.Errorf("41 pictures in %d tasks", len(payloads))
	}

	svg := storedCID(t, store, `<svg xmlns="http://www.w3.org/2000/svg"/>`)
	pdf := storedCID(t, store, "%PDF-1.7")
	empty := storedCID(t, store, "")
	missing := "bafkreigh2akiscaildcqabsyg3dfr6chu3fgpregiymsck7e7aqa4s52zy"
	tooMany := `"inputs":["` + strings.Repeat(pictures[0]+`","`, imagesMostPictures) + pictures[0] + `"]`
	for params, want := range map[string]string{
		`not json`: "images parameters",
		`{}`:       "images parameters: no picture is given",
		`{"input":"` + pictures[0] + `",` + inputs + `}`: "inputs are given in place of input, not with it",
		`{` + tooMany + `}`:                                        "257 pictures are given; a job can have at most 256",
		`{"inputs":["somewhere"]}`:                                 "input 0 is not the CID of a stored picture",
		`{"inputs":["` + svg + `"]}`:                               "it is not a PNG, JPEG, WebP, TIFF or GIF picture",
		`{"inputs":["` + pdf + `"]}`:                               "it is not a PNG, JPEG, WebP, TIFF or GIF picture",
		`{"inputs":["` + empty + `"]}`:                             "it is not a PNG",
		`{"inputs":["` + missing + `"]}`:                           "images input 0, " + missing,
		`{` + inputs + `,"format":"bmp"}`:                          `format is "bmp", and must be one of same, jpeg, png, webp, tiff, gif`,
		`{` + inputs + `,"format":"svg"}`:                          `format is "svg"`,
		`{` + inputs + `,"width":-1}`:                              "width is -1, and must be from 1 to 16384, or 0 to leave it free",
		`{` + inputs + `,"width":16385}`:                           "width is 16385",
		`{` + inputs + `,"height":-5}`:                             "height is -5, and must be from 1 to 16384",
		`{` + inputs + `,"height":20000}`:                          "height is 20000",
		`{` + inputs + `,"quality":101}`:                           "quality is 101, and must be from 1 to 100",
		`{` + inputs + `,"quality":-3}`:                            "quality is -3",
		`{` + inputs + `,"enlarge":true}`:                          "enlarge is asked for with no width or height to enlarge to",
		`{` + inputs + `,"tasks":-1}`:                              "tasks is -1, and must be from 1 to 10000",
		`{` + inputs + `,"tasks":10001}`:                           "tasks is 10001",
		`{` + inputs + `,"width":16384,"height":1,"quality":1}`:    "",
		`{` + inputs + `,"format":"gif","enlarge":true,"width":1}`: "",
	} {
		_, err := (Images{}).Split(ctx, store, []byte(params), 2)
		if want == "" && err != nil || want != "" && (err == nil || !strings.Contains(err.Error(), want)) {
			t.Errorf("%s: %v, want an error containing %q", params, err, want)
		}
	}
	if _, err := (Images{}).Split(ctx, sized{store, imagesMostInput + 1}, []byte(`{"input":"`+pictures[0]+`"}`), 1); err == nil || !strings.Contains(err.Error(), "it has 268435457 bytes, and a picture given to images may have at most 268435456") {
		t.Errorf("a picture too big: %v", err)
	}
	if w, err := WithContainers().Get("images"); err != nil || w.Name() != "images" || !strings.Contains(WithContainers().Describe("images"), "libvips") {
		t.Errorf("the workload as registered: %v, %v", w, err)
	}
	if !WithContainers().InContainers("images") {
		t.Error("images is not said to run in containers")
	}
	if _, err := Builtin().Get("images"); err == nil {
		t.Error("a node that runs no containers offers to edit pictures")
	}
	if !(Images{}).Varies() {
		t.Error("images says two honest workers give the same bytes, and libvips picks its code by processor")
	}
}

func TestOnlyPicturesOfTheFormatsTakenAreEdited(t *testing.T) {
	for head, want := range map[string]string{
		"\x89PNG\r\n\x1a\n": "png", "\x89PNG": "", "\xff\xd8\xff": "jpeg", "\xff\xd8": "",
		"RIFF\x00\x00\x00\x00WEBP": "webp", "RIFF\x00\x00\x00\x00WAVE": "", "RIFF": "",
		"II*\x00": "tiff", "MM\x00*": "tiff", "GIF87a": "gif", "GIF89a": "gif", "GIF90a": "",
		"BM": "", "<svg": "", "%PDF-": "", "": "",
	} {
		if got := pictureKind([]byte(head)); got != want {
			t.Errorf("%q is %q, want %q", head, got, want)
		}
	}
	for name, want := range map[string]bool{
		"image-000.png": true, "image-004.jpg": true, "image-001.webp": true, "image-002.tif": true, "image-003.gif": true,
		"image-005.png": false, "image-000.jpeg": false, "image-000.svg": false, "image-000": false, "image-0.png": false,
		"image--01.png": false, "image-000.png.sh": false, "summary.json": false, "../image-000.png": false,
	} {
		if _, got := imagesInput(name, 5); got != want {
			t.Errorf("%q is one of 5 pictures: %v", name, got)
		}
	}
	for settings, want := range map[imagesSettings]string{
		{Format: "same", Quality: 85}:                            "10000000x10000000>",
		{Format: "same", Quality: 85, Width: 10, Height: 20}:     "10x20>",
		{Format: "same", Quality: 85, Width: 10, Enlarge: true}:  "10x",
		{Format: "same", Quality: 85, Height: 20, Enlarge: true}: "x20",
	} {
		if got := settings.size(); got != want {
			t.Errorf("%+v is %q, want %q", settings, got, want)
		}
	}
	if _, err := imagesSettingsOf("same", "ten", "0", "maybe", "85", "false"); err == nil || !strings.Contains(err.Error(), "settings are not numbers and yes or no") {
		t.Errorf("settings that are not numbers: %v", err)
	}
}

func TestTheImagesImageIsBuiltOnlyWhereItIsMissing(t *testing.T) {
	store := storage.NewMemory()
	ctx := context.Background()
	picture := storedCID(t, store, pictureHeads["png"])
	payloads, _ := Images{}.Split(ctx, store, []byte(`{"inputs":["`+picture+`","`+picture+`"],"width":64,"tasks":2}`), 2)
	var ran []string
	edits := func(args []string, _, _ io.Writer) (int, error) {
		ran = args
		os.WriteFile(filepath.Join(volume(args, "/output"), "summary.json"), []byte(`{}`), 0o644)
		return 0, nil
	}
	has := &pretend{do: func(args []string, stdout, stderr io.Writer) (int, error) {
		if args[0] == "image" {
			return 0, nil
		}
		return edits(args, stdout, stderr)
	}}
	if _, err := (Images{Engine: has.engine}).Execute(ctx, store, payloads[1]); err != nil {
		t.Fatal(err)
	}
	if len(has.asked) != 2 || strings.Join(has.asked[0], " ") != "image inspect "+ImagesImage || has.asked[1][0] != "run" {
		t.Errorf("with the image there, the engine was asked %v", has.asked)
	}
	if !slices.Contains(ran, "SIZE=64x>") || !slices.Contains(ran, "PICTURES=2") || ran[len(ran)-1] != imagesScript || ran[len(ran)-4] != ImagesImage {
		t.Errorf("the task ran %q", ran)
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
		return edits(args, stdout, stderr)
	}}
	said := new(heard)
	if _, err := (Images{Engine: lacks.engine}).Execute(WithReporter(ctx, said), store, payloads[0]); err != nil {
		t.Fatal(err)
	}
	if len(lacks.asked) != 3 || strings.Join(lacks.asked[1][:3], " ") != "build --tag "+ImagesImage || !strings.Contains(recipe, "FROM alpine:3.20") || !strings.Contains(recipe, "vips-tools") {
		t.Errorf("with the image missing, the engine was asked %v with the recipe %q", lacks.asked, recipe)
	}
	if log := strings.Join(said.lines, "\n"); !strings.Contains(log, "building the image editing pictures runs in") {
		t.Errorf("the task's log: %q", said.lines)
	}

	broken := &pretend{do: func(args []string, _, stderr io.Writer) (int, error) {
		if args[0] == "image" {
			return 1, nil
		}
		io.WriteString(stderr, "ERROR: unable to select packages\n")
		return 1, nil
	}}
	if _, err := (Images{Engine: broken.engine}).Execute(ctx, store, payloads[0]); err == nil || !strings.Contains(err.Error(), "build the image editing pictures runs in: ERROR: unable to select packages") {
		t.Errorf("a build that fails: %v", err)
	}

	// The worker makes the size itself, from settings it checks for
	// itself, runs only its own image and script, and takes no input but
	// the job's pictures: what a payload says of them is not run.
	task := trajTaskOf(payloads[1])
	tampered := func(change func(*containerTask)) []byte {
		copied := task
		copied.Env, copied.Inputs = map[string]string{}, map[string]string{}
		for k, v := range task.Env {
			copied.Env[k] = v
		}
		for k, v := range task.Inputs {
			copied.Inputs[k] = v
		}
		change(&copied)
		return mustJSON(copied)
	}
	ran = nil
	if _, err := (Images{Engine: has.engine}).Execute(ctx, store, tampered(func(c *containerTask) {
		c.Env["SIZE"] = "64x\" ; rm -rf /output ; \""
		c.Env["LD_PRELOAD"] = "/input/evil.so"
		c.Command = []string{"sh", "-c", "anything"}
		c.Input, c.Network, c.GPUs = picture, true, 1
	})); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(ran, "SIZE=64x>") || slices.Contains(ran, "LD_PRELOAD=/input/evil.so") || ran[len(ran)-1] != imagesScript ||
		slices.Contains(ran, "anything") || slices.Contains(ran, "--gpus") || !slices.Contains(ran, "none") {
		t.Errorf("a payload with commands of its own ran %q", ran)
	}
	for name, tt := range map[string]struct {
		change func(*containerTask)
		want   string
	}{
		"another image":              {func(c *containerTask) { c.Image = "alpine:3.20" }, `images payload: "alpine:3.20" is not the image pictures are edited in`},
		"a format not on it":         {func(c *containerTask) { c.Env["FORMAT"] = "svg" }, `images payload: format is "svg"`},
		"a width with a command":     {func(c *containerTask) { c.Env["WIDTH"] = "64; ls" }, `images payload: settings are not numbers and yes or no`},
		"a quality out of bounds":    {func(c *containerTask) { c.Env["QUALITY"] = "0" }, `images payload: quality is 0`},
		"keeping that is not a flag": {func(c *containerTask) { c.Env["KEEP"] = "$(id)" }, `images payload: settings are not numbers and yes or no`},
		"no settings":                {func(c *containerTask) { c.Env = nil }, `images payload: settings are not numbers`},
		"no pictures":                {func(c *containerTask) { c.Env["PICTURES"] = "0" }, `images payload: "0" pictures`},
		"too many pictures":          {func(c *containerTask) { c.Env["PICTURES"] = "257" }, `images payload: "257" pictures`},
		"a script among its files":   {func(c *containerTask) { c.Inputs["run.sh"] = picture }, `images payload: "run.sh" is not one of the job's 2 pictures`},
		"a picture past the last":    {func(c *containerTask) { c.Inputs["image-002.png"] = picture }, `"image-002.png" is not one of the job's 2 pictures`},
		"a picture of an SVG":        {func(c *containerTask) { c.Inputs["image-000.svg"] = picture }, `"image-000.svg" is not one`},
	} {
		if _, err := (Images{Engine: has.engine}).Execute(ctx, store, tampered(tt.change)); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: %v, want an error containing %q", name, err, tt.want)
		}
	}
	if _, err := (Images{Engine: has.engine}).Execute(ctx, store, []byte("not json")); err == nil || !strings.Contains(err.Error(), "images payload") {
		t.Errorf("a payload that is not one: %v", err)
	}
}

// imagesReportOf is what a task says of a picture it saved.
func imagesReportOf(index int, format string, width, height int) imagesReport {
	return imagesReport{Index: index, Format: format, Width: width, Height: height}
}

// imagesSummaryOf is what a task that edited pictures first on of pictures
// says, made to fit 640 by 480 as webp.
func imagesSummaryOf(first, pictures int, reports ...imagesReport) imagesSummary {
	return imagesSummary{Pictures: pictures, First: first, Last: first + len(reports) - 1, Format: "webp", Width: "640", Height: "480", Enlarge: "false", Quality: "85", Keep: "false", Made: reports}
}

// imagesTask is the output of a task that said summary and left a picture
// of the format it says for each it made, and a log for each it did not.
func imagesTask(t *testing.T, store *storage.Store, index int, summary imagesSummary) []byte {
	t.Helper()
	files := map[string]string{}
	for _, report := range summary.Made {
		if report.Failed {
			files[imagesName(report.Index)+".log"] = "VipsJpeg: Premature end of JPEG file\n"
			continue
		}
		files[imagesName(report.Index)+"."+imagesSuffix[report.Format]] = pictureHeads[report.Format] + strings.Repeat("x", report.Index)
	}
	return ocrTask(t, store, index, mustJSONString(summary), files)
}

func TestEditedPicturesArePutTogetherInOrder(t *testing.T) {
	store := storage.NewMemory()
	ctx := context.Background()
	// Three pictures among three tasks, the first of which was left none,
	// as when there are more tasks than pictures; the last could not be
	// read.
	none := imagesSummaryOf(0, 3)
	outputs := [][]byte{
		imagesTask(t, store, 0, none),
		imagesTask(t, store, 1, imagesSummaryOf(0, 3, imagesReportOf(0, "webp", 640, 427), imagesReportOf(1, "webp", 320, 480))),
		imagesTask(t, store, 2, imagesSummaryOf(2, 3, imagesReport{Index: 2, Failed: true})),
	}
	raw, err := Images{}.Aggregate(ctx, store, outputs)
	if err != nil {
		t.Fatal(err)
	}
	var result ImagesResult
	json.Unmarshal(raw, &result)
	if result.Width != 640 || result.Height != 480 || result.Format != "webp" || result.Quality != 85 || result.Keep || result.Enlarge || result.Tasks != 3 || result.Made != 2 || result.Failed != 1 {
		t.Errorf("result: %+v", result)
	}
	if len(result.Pictures) != 3 {
		t.Fatalf("pictures: %+v", result.Pictures)
	}
	made, tall, broken := result.Pictures[0], result.Pictures[1], result.Pictures[2]
	if made.Name != "image-000.webp" || made.Format != "webp" || made.Width != 640 || made.Height != 427 || made.Bytes != uint64(len(pictureHeads["webp"])) ||
		readBlob(t, store, made.Picture) != pictureHeads["webp"] || made.Error != "" {
		t.Errorf("picture 0: %+v", made)
	}
	if tall.Name != "image-001.webp" || tall.Width != 320 || tall.Height != 480 || tall.Index != 1 {
		t.Errorf("picture 1: %+v", tall)
	}
	if broken.Index != 2 || broken.Picture != "" || !strings.Contains(broken.Error, "libvips could not read or save the picture") || !strings.Contains(readBlob(t, store, broken.Log), "Premature end of JPEG") {
		t.Errorf("picture 2: %+v", broken)
	}
	if want := map[string]string{"image-000.webp": made.Picture, "image-001.webp": tall.Picture}; !equalEnv(result.Files, want) {
		t.Errorf("files: %v", result.Files)
	}
	archive, err := zip.NewReader(bytes.NewReader([]byte(readBlob(t, store, result.Archive))), int64(len(readBlob(t, store, result.Archive))))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range archive.File {
		names = append(names, f.Name)
	}
	if !slices.Equal(names, []string{"image-000.webp", "image-001.webp"}) {
		t.Errorf("the archive has %v", names)
	}
	if got := (Images{}).ResultBlobs(raw); !slices.Equal(got, []string{made.Picture, tall.Picture, broken.Log}) {
		t.Errorf("ResultBlobs = %v", got)
	}
	if got := (Images{}).ResultBlobs([]byte("not json")); len(got) != 0 {
		t.Errorf("ResultBlobs of no result = %v", got)
	}

	// Each in its own format, and no box: whatever size, up to what the
	// formats save.
	same := imagesSummary{Pictures: 2, First: 0, Last: 1, Format: "same", Width: "0", Height: "0", Enlarge: "false", Quality: "85", Keep: "true",
		Made: []imagesReport{imagesReportOf(0, "jpeg", 6000, 4000), imagesReportOf(1, "gif", 1, 65535)}}
	raw, err = Images{}.Aggregate(ctx, store, [][]byte{imagesTask(t, store, 0, same)})
	json.Unmarshal(raw, &result)
	if err != nil || result.Files["image-000.jpg"] == "" || result.Files["image-001.gif"] == "" || !result.Keep || result.Pictures[1].Format != "gif" {
		t.Errorf("each in its own format: %+v, %v", result, err)
	}
}

func TestWhatImagesTasksSayIsChecked(t *testing.T) {
	store := storage.NewMemory()
	ctx := context.Background()
	missing := "bafkreigh2akiscaildcqabsyg3dfr6chu3fgpregiymsck7e7aqa4s52zy"
	garbled := storedCID(t, store, "not json")
	first := imagesTask(t, store, 0, imagesSummaryOf(0, 2, imagesReportOf(0, "webp", 640, 480)))
	second := func(summary imagesSummary) [][]byte {
		return [][]byte{first, imagesTask(t, store, 1, summary)}
	}
	alone := func(summary imagesSummary) [][]byte {
		return [][]byte{imagesTask(t, store, 0, summary)}
	}
	with := func(change func(*imagesSummary)) imagesSummary {
		s := imagesSummaryOf(0, 1, imagesReportOf(0, "webp", 640, 480))
		change(&s)
		return s
	}
	report := func(change func(*imagesReport)) [][]byte {
		r := imagesReportOf(0, "webp", 640, 480)
		change(&r)
		return alone(imagesSummaryOf(0, 1, r))
	}
	// The file a task left for picture 0, as the summary says, in place
	// of the one it would leave.
	file := func(name, content string) [][]byte {
		stored := map[string]string{"summary.json": storedCID(t, store, mustJSONString(imagesSummaryOf(0, 1, imagesReportOf(0, "webp", 640, 480))))}
		if name != "" {
			stored[name] = content
		}
		return [][]byte{mustJSON(ContainerOutput{Files: stored})}
	}
	failed := func(log string) [][]byte {
		stored := map[string]string{"summary.json": storedCID(t, store, mustJSONString(imagesSummaryOf(0, 1, imagesReport{Index: 0, Failed: true})))}
		if log != "" {
			stored["image-000.log"] = log
		}
		return [][]byte{mustJSON(ContainerOutput{Files: stored})}
	}
	for name, tt := range map[string]struct {
		outputs [][]byte
		blobs   Blobs
		want    string
	}{
		"an output that is not one":        {[][]byte{first, []byte("not json")}, store, "task 1 output"},
		"a task with no summary":           {[][]byte{ocrTask(t, store, 0, "", nil)}, store, "task 0's summary: the task left none"},
		"a summary that is not there":      {[][]byte{mustJSON(ContainerOutput{Files: map[string]string{"summary.json": missing}})}, store, "task 0's summary: "},
		"a summary that is not one":        {[][]byte{mustJSON(ContainerOutput{Files: map[string]string{"summary.json": garbled}})}, store, "task 0's summary: invalid character"},
		"settings no job asks for":         {alone(with(func(s *imagesSummary) { s.Format = "bmp" })), store, `task 0 says it edited 1 pictures, with settings no job asks for: format is "bmp"`},
		"a width that is not a number":     {alone(with(func(s *imagesSummary) { s.Width = "wide" })), store, "settings are not numbers"},
		"no pictures":                      {alone(with(func(s *imagesSummary) { s.Pictures = 0 })), store, "task 0 says it edited 0 pictures"},
		"too many pictures":                {alone(with(func(s *imagesSummary) { s.Pictures = 257 })), store, "task 0 says it edited 257 pictures"},
		"tasks of other settings":          {second(imagesSummary{Pictures: 2, First: 1, Last: 1, Format: "png", Width: "640", Height: "480", Enlarge: "false", Quality: "85", Keep: "false", Made: []imagesReport{imagesReportOf(1, "png", 1, 1)}}), store, "task 1 edited 2 pictures as png"},
		"tasks of other counts":            {second(imagesSummaryOf(1, 3, imagesReportOf(1, "webp", 1, 1))), store, "task 1 edited 3 pictures"},
		"a task that skips one":            {second(imagesSummaryOf(2, 2)), store, "task 1 says it edited pictures 2 to 1 of 2, after pictures up to 0"},
		"a task past the last":             {alone(imagesSummary{Pictures: 1, First: 0, Last: 1, Format: "webp", Width: "640", Height: "480", Enlarge: "false", Quality: "85", Keep: "false"}), store, "says it edited pictures 0 to 1 of 1"},
		"a task that reports too few":      {alone(with(func(s *imagesSummary) { s.Made = nil })), store, "and reports 0"},
		"a report out of its place":        {report(func(r *imagesReport) { r.Index = 1 }), store, "task 0 reports picture 1 where picture 0 was to be"},
		"too few pictures":                 {[][]byte{first}, store, "the tasks edited 1 of the 2 pictures"},
		"a format not asked for":           {report(func(r *imagesReport) { r.Format = "png" }), store, `task 0's picture 0: it says it was saved as "png", and the job asks for webp`},
		"a format no job asks for":         {alone(imagesSummary{Pictures: 1, Last: 0, Format: "same", Width: "0", Height: "0", Enlarge: "false", Quality: "85", Keep: "false", Made: []imagesReport{imagesReportOf(0, "bmp", 1, 1)}}), store, `it says it was saved as "bmp"`},
		"a picture too wide":               {report(func(r *imagesReport) { r.Width = 641 }), store, "it says it is 641 by 480 pixels, which does not fit within 640 by 480"},
		"a picture too high":               {report(func(r *imagesReport) { r.Height = 481 }), store, "it says it is 640 by 481 pixels"},
		"a picture of no size":             {report(func(r *imagesReport) { r.Width = 0 }), store, "it says it is 0 by 480 pixels"},
		"a picture past any format":        {alone(imagesSummary{Pictures: 1, Last: 0, Format: "same", Width: "0", Height: "0", Enlarge: "false", Quality: "85", Keep: "false", Made: []imagesReport{imagesReportOf(0, "png", 65536, 1)}}), store, "which does not fit within 65535 by 65535"},
		"no picture left":                  {file("", ""), store, "the task left no image-000.webp"},
		"a picture that is not there":      {file("image-000.webp", missing), store, "image-000.webp: "},
		"an empty picture":                 {file("image-000.webp", storedCID(t, store, "")), store, "image-000.webp has 0 bytes, and must have from 1 to 1073741824"},
		"a picture of another format":      {file("image-000.webp", storedCID(t, store, pictureHeads["png"])), store, "image-000.webp is not a webp picture"},
		"a failure with no log":            {failed(""), store, "the task left no image-000.log"},
		"every picture failed":             {failed(storedCID(t, store, "unable to thumbnail")), store, "no picture was made: picture 0: libvips could not read or save the picture"},
		"an archive that cannot be stored": {alone(imagesSummaryOf(0, 1, imagesReportOf(0, "webp", 640, 480))), &awkward{Blobs: store, failPut: 1}, "store the pictures as one zip file: the store is full"},
		"files that cannot be read":        {alone(imagesSummaryOf(0, 1, imagesReportOf(0, "webp", 640, 480))), &awkward{Blobs: store, badReads: true}, "task 0's summary: the store is full"},
	} {
		if _, err := (Images{}).Aggregate(ctx, tt.blobs, tt.outputs); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: %v, want an error containing %q", name, err, tt.want)
		}
	}
	if _, err := (Images{}).Aggregate(ctx, sized{store, imagesMostFile + 1}, alone(imagesSummaryOf(0, 1, imagesReportOf(0, "webp", 640, 480)))); err == nil || !strings.Contains(err.Error(), "image-000.webp has 1073741825 bytes") {
		t.Errorf("a picture too big: %v", err)
	}
}

// photo is a JPEG of width by height pixels, lighter to the right, with an
// EXIF block that says it is to be turned a quarter clockwise to be seen
// upright, as a phone held on its side saves one, and names the camera.
func photo(t *testing.T, width, height int) []byte {
	t.Helper()
	picture := image.NewRGBA(image.Rect(0, 0, width, height))
	for x := range width {
		for y := range height {
			picture.Set(x, y, color.RGBA{uint8(255 * x / width), 80, 160, 255})
		}
	}
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, picture, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	// TIFF's header, little-endian, then one directory of two entries: the
	// orientation, 6, and the make of camera, a string at offset 38.
	var exif bytes.Buffer
	exif.WriteString("Exif\x00\x00II*\x00")
	for _, v := range []any{uint32(8), uint16(2),
		uint16(0x0112), uint16(3), uint32(1), uint32(6),
		uint16(0x010f), uint16(2), uint32(14), uint32(38),
		uint32(0)} {
		binary.Write(&exif, binary.LittleEndian, v)
	}
	exif.WriteString("SisyphusPhone\x00")
	segment := append([]byte{0xff, 0xe1, 0, 0}, exif.Bytes()...)
	binary.BigEndian.PutUint16(segment[2:], uint16(len(segment)-2))
	jpg := encoded.Bytes()
	return append(append(append([]byte{}, jpg[:2]...), segment...), jpg[2:]...)
}

// Real pictures, made here, edited in real containers and put together:
// a photo taken on its side is turned upright and shrunk to fit, a PNG is
// left as small as it is unless asked to be enlarged, each is saved as
// asked, and a photo's metadata is taken out unless asked to be kept.
func TestRealPicturesAreEdited(t *testing.T) {
	requireRealImages(t)
	requireDocker(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	// The image is built here if this machine has not got it, which takes
	// the network and a minute.
	if err := haveImage(ctx, docker, ImagesImage, imagesDockerfile, "editing pictures"); err != nil {
		t.Fatal(err)
	}
	store := storage.NewMemory()
	put := func(content []byte) string {
		t.Helper()
		c, err := store.Put(ctx, bytes.NewReader(content))
		if err != nil {
			t.Fatal(err)
		}
		return c.String()
	}
	small := image.NewNRGBA(image.Rect(0, 0, 40, 30))
	for i := range small.Pix {
		small.Pix[i] = 200
	}
	var encoded bytes.Buffer
	png.Encode(&encoded, small)
	taken := photo(t, 800, 600)
	if !bytes.Contains(taken, []byte("SisyphusPhone")) {
		t.Fatal("the photo has no EXIF to take out")
	}
	inputs := []string{put(taken), put(encoded.Bytes()), put([]byte("\xff\xd8\xff\xe0 the rest is not a JPEG"))}

	w := Images{Confined: Confinement{ReadOnly: true}}
	run := func(params string, parts int) (ImagesResult, string) {
		t.Helper()
		payloads, err := w.Split(ctx, store, []byte(params), parts)
		if err != nil {
			t.Fatal(err)
		}
		outputs := make([][]byte, len(payloads))
		said := new(heard)
		started := time.Now()
		for i, payload := range payloads {
			if outputs[i], err = w.Execute(WithReporter(ctx, said), store, payload); err != nil {
				t.Fatalf("task %d: %v\n%s", i, err, strings.Join(said.lines, "\n"))
			}
		}
		log := strings.Join(said.lines, "\n")
		t.Logf("%s edited in %s:\n%s", params, time.Since(started).Round(time.Millisecond), log)
		raw, err := w.Aggregate(ctx, store, outputs)
		if err != nil {
			t.Fatal(err)
		}
		var result ImagesResult
		json.Unmarshal(raw, &result)
		t.Logf("result: %+v", result)
		return result, log
	}

	all := `"inputs":["` + strings.Join(inputs, `","`) + `"]`
	result, log := run(`{`+all+`,"width":300,"height":300,"format":"webp","quality":80,"tasks":2}`, 4)
	if result.Tasks != 2 || result.Made != 2 || result.Failed != 1 || len(result.Pictures) != 3 {
		t.Fatalf("result: %+v", result)
	}
	upright, left, broken := result.Pictures[0], result.Pictures[1], result.Pictures[2]
	// Turned a quarter, the 800 by 600 photo stands 600 by 800, and fits
	// 300 by 300 as 225 by 300.
	if upright.Name != "image-000.webp" || upright.Width != 225 || upright.Height != 300 || upright.Format != "webp" {
		t.Errorf("the photo: %+v", upright)
	}
	edited := []byte(readBlob(t, store, upright.Picture))
	if pictureKind(edited) != "webp" || bytes.Contains(edited, []byte("SisyphusPhone")) || bytes.Contains(edited, []byte("EXIF")) {
		t.Errorf("the photo's metadata was not taken out, or it is no WebP: %q", edited[:min(len(edited), 64)])
	}
	if left.Width != 40 || left.Height != 30 || left.Name != "image-001.webp" {
		t.Errorf("the small picture was enlarged: %+v", left)
	}
	if broken.Error == "" || broken.Picture != "" || !strings.Contains(readBlob(t, store, broken.Log), "VipsJpeg") || !strings.Contains(log, "picture 2 could not be made") {
		t.Errorf("the broken picture: %+v\n%s", broken, readBlob(t, store, broken.Log))
	}
	archive := readBlob(t, store, result.Archive)
	if zipped, err := zip.NewReader(strings.NewReader(archive), int64(len(archive))); err != nil || len(zipped.File) != 2 || zipped.File[0].Name != "image-000.webp" || zipped.File[1].Name != "image-001.webp" {
		t.Errorf("the archive: %v", err)
	}

	// The same pictures, kept in their own formats with their metadata,
	// the small one enlarged to the box; and on one machine, the same
	// bytes however the job is shared out.
	both := `"inputs":["` + inputs[0] + `","` + inputs[1] + `"]`
	kept, _ := run(`{`+both+`,"width":120,"height":120,"enlarge":true,"keep_metadata":true}`, 1)
	if kept.Made != 2 || kept.Pictures[0].Name != "image-000.jpg" || kept.Pictures[0].Width != 90 || kept.Pictures[0].Height != 120 ||
		kept.Pictures[1].Name != "image-001.png" || kept.Pictures[1].Width != 120 || kept.Pictures[1].Height != 90 {
		t.Errorf("kept as they are: %+v", kept.Pictures)
	}
	if photo := []byte(readBlob(t, store, kept.Pictures[0].Picture)); !bytes.Contains(photo, []byte("SisyphusPhone")) {
		t.Error("the photo's metadata was taken out when it was to be kept")
	}
	split, _ := run(`{`+both+`,"width":120,"height":120,"enlarge":true,"keep_metadata":true,"tasks":2}`, 2)
	if split.Pictures[0].Picture != kept.Pictures[0].Picture || split.Pictures[1].Picture != kept.Pictures[1].Picture {
		t.Errorf("shared out in two, the pictures differ: %+v and %+v", split.Pictures, kept.Pictures)
	}
}
