package runtime

import (
	"archive/zip"
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/draw"
	"image/png"
	"io"
	"path"
	"slices"
	"strconv"
	"strings"

	"github.com/ipfs/go-cid"
)

// Render renders a stored Blender scene with Blender itself, run from its
// command line with no window. A job is shared out across the pool in one
// of two ways: an animation by its frames, each task rendering a run of
// them, or one frame by its rows, each task rendering a strip of it, which
// are put back together into the whole picture.
//
// Like Transcode, it is the container workload with the image and the
// command fixed, so it runs only on workers that run containers. The image
// is built on a worker the first time it is needed, from Blender's own
// release for Linux, checked against the checksum Blender publishes for it.
//
// A .blend file can carry Python that Blender would run on opening it: a
// script registered to run, a driver's expression. Blender is told not to,
// whatever the file asks.
type Render struct {
	// Engine runs the container engine, as Container's does.
	Engine func(ctx context.Context, args []string, stdout, stderr io.Writer) (status int, err error)
	// Confined holds its containers in as Container's does.
	Confined Confinement
}

// RenderParams are a render job's parameters.
type RenderParams struct {
	// Input is the CID of the stored .blend file. Whatever it uses,
	// textures among it, is packed into it: a task has nothing else.
	Input string `json:"input"`
	// FrameStart and FrameEnd are the first and last frames to render.
	// Either left out is the file's own. Split by tiles, the one frame
	// rendered is FrameStart, or the file's current frame without it.
	FrameStart *int `json:"frame_start,omitempty"`
	FrameEnd   *int `json:"frame_end,omitempty"`
	// Engine is CYCLES or BLENDER_EEVEE_NEXT. Empty means CYCLES.
	Engine string `json:"engine,omitempty"`
	// Samples, if not zero, is how many samples to take of each pixel in
	// place of the file's.
	Samples int `json:"samples,omitempty"`
	// ResolutionPercent, if not zero, is the size to render at as a
	// percentage of the file's resolution, in place of the file's.
	ResolutionPercent int `json:"resolution_percent,omitempty"`
	// Format is PNG, 8-bit RGBA, or EXR, OpenEXR's floating point. Empty
	// means PNG. Only PNG strips can be put together.
	Format string `json:"format,omitempty"`
	// Split is frames or tiles. Empty means frames.
	Split string `json:"split,omitempty"`
	// Tasks is how many runs of frames, or strips of the frame, to share
	// the job out as. Zero leaves it to the pool.
	Tasks int `json:"tasks,omitempty"`
}

// RenderResult is a render job's result.
type RenderResult struct {
	// Split is how the job was shared out: frames or tiles.
	Split string `json:"split"`
	// Frames is how many frames were rendered.
	Frames int `json:"frames"`
	// Images are the CIDs of the frames, in order, and Archive that of a
	// zip file of them all, when split by frames.
	Images  []string `json:"images,omitempty"`
	Archive string   `json:"archive,omitempty"`
	// Image is the CID of the whole frame, put together from its Tiles
	// strips, when split by tiles.
	Image string `json:"image,omitempty"`
	Tiles int    `json:"tiles,omitempty"`
	// Width and Height are the size of each frame in pixels.
	Width  int `json:"width"`
	Height int `json:"height"`
	// Engine is what rendered it.
	Engine string `json:"engine"`
	// Seconds is how long the tasks spent rendering, added up.
	Seconds float64 `json:"seconds"`
}

// renderDockerfile is the whole of the image rendering runs in: Blender's
// release for Linux, as it comes, with the few libraries it loads that a
// slim Debian lacks, and Mesa's, which draw for EEVEE where there is no
// graphics card. The release is checked against the checksum blender.org
// publishes beside it.
const renderDockerfile = `FROM debian:bookworm-slim AS fetch
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl xz-utils && rm -rf /var/lib/apt/lists/*
RUN curl -fsSLo /blender.tar.xz https://download.blender.org/release/Blender4.5/blender-4.5.14-linux-x64.tar.xz \
 && echo "9ba871ff2ecd36526b77432745980b7e6664ecd0c7ca11c48849073dcfe06da3  /blender.tar.xz" | sha256sum -c - \
 && mkdir /opt/blender && tar -xJf /blender.tar.xz -C /opt/blender --strip-components=1 && rm /blender.tar.xz
FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends libx11-6 libxi6 libxxf86vm1 libxfixes3 libxrender1 libxkbcommon0 libsm6 libice6 libgl1 libegl1 libgl1-mesa-dri && rm -rf /var/lib/apt/lists/*
COPY --from=fetch /opt/blender /opt/blender
`

// RenderImage names that image, by what it is made from, as TranscodeImage
// does.
var RenderImage = func() string {
	sum := sha256.Sum256([]byte(renderDockerfile + renderScript))
	return "sisyphus-render:" + hex.EncodeToString(sum[:6])
}()

// renderScript is what each task has Blender run once the file is open: it
// sets the job's settings over the file's, takes its share of the frames
// or its strip of the frame, renders it to /output, and says what it did
// in summary.json.
const renderScript = `import bpy, json, os, sys, time
env = os.environ
index, count = int(env["SISYPHUS_TASK_INDEX"]), int(env["SISYPHUS_TASK_COUNT"])
scene = bpy.context.scene
render = scene.render
missing = [i.filepath for i in bpy.data.images if i.source == "FILE" and not i.packed_file and not os.path.exists(bpy.path.abspath(i.filepath))]
if missing:
    print(f"{len(missing)} images the file uses are not packed into it, and render as pink: {', '.join(missing[:5])}", flush=True)
render.engine = env["ENGINE"]
if env["ENGINE"] == "CYCLES":
    scene.cycles.device = "CPU"
    if env["SAMPLES"]:
        scene.cycles.samples = int(env["SAMPLES"])
elif env["SAMPLES"]:
    scene.eevee.taa_render_samples = int(env["SAMPLES"])
if env["PERCENT"]:
    render.resolution_percentage = int(env["PERCENT"])
render.use_file_extension = True
if env["FORMAT"] == "EXR":
    render.image_settings.file_format = "OPEN_EXR"
else:
    render.image_settings.file_format = "PNG"
    render.image_settings.color_mode = "RGBA"
    render.image_settings.color_depth = "8"
width = render.resolution_x * render.resolution_percentage // 100
height = render.resolution_y * render.resolution_percentage // 100
first = int(env["FRAME_START"]) if env["FRAME_START"] else None
last = int(env["FRAME_END"]) if env["FRAME_END"] else scene.frame_end
frames = []
started = time.time()
if env["SPLIT"] == "tiles":
    frame = scene.frame_current if first is None else first
    # Strip 0 is the top of the picture; Blender counts rows from the bottom.
    top, bottom = height - height * index // count, height - height * (index + 1) // count
    if top <= bottom:
        raise SystemExit(f"the picture is {height} pixels high, too few for {count} strips")
    edge = lambda row: 0.0 if row == 0 else 1.0 if row == height else (row + 0.25) / height
    render.use_border, render.use_crop_to_border = True, True
    render.border_min_x, render.border_max_x = 0.0, 1.0
    render.border_min_y, render.border_max_y = edge(bottom), edge(top)
    scene.frame_set(frame)
    print(f"strip {index + 1} of {count}: rows {bottom} to {top} of {height}, frame {frame}, with {render.engine}", flush=True)
    render.filepath = "/output/tile"
    bpy.ops.render.render(write_still=True)
    frames = [frame]
else:
    first = scene.frame_start if first is None else first
    if last < first:
        raise SystemExit(f"the last frame, {last}, comes before the first, {first}")
    every = list(range(first, last + 1, max(scene.frame_step, 1)))
    frames = every[len(every) * index // count:len(every) * (index + 1) // count]
    if len(frames) > 1000:
        raise SystemExit(f"this task's share is {len(frames)} frames, more than the 1000 a task may render: ask for more tasks")
    if frames:
        print(f"frames {frames[0]} to {frames[-1]} of {first} to {last}, {width}x{height}, with {render.engine}", flush=True)
        scene.frame_start, scene.frame_end = frames[0], frames[-1]
        render.filepath = "/output/frame_#######"
        bpy.ops.render.render(animation=True)
took = time.time() - started
with open("/output/summary.json", "w") as out:
    json.dump({"split": env["SPLIT"], "frames": frames, "seconds": took, "width": width, "height": height, "engine": render.engine}, out)
print(f"rendered {len(frames)} frames in {took:.1f} seconds", flush=True)
`

// renderEngines are the engines a job may ask for.
var renderEngines = []string{"CYCLES", "BLENDER_EEVEE_NEXT"}

// renderFormats and renderSplits are the formats and the ways of sharing
// out a job.
var (
	renderFormats = []string{"PNG", "EXR"}
	renderSplits  = []string{"frames", "tiles"}
)

// Bounds of a render job: the last frame Blender has, the most samples it
// takes, the largest percentage asked of a file's size, the most frames one
// task renders, and the most pixels a frame put together from strips may
// have, which it holds in memory.
const (
	renderLastFrame     = 1048574
	renderMostSamples   = 1 << 24
	renderMostPercent   = 1000
	renderFramesPerTask = 1000
	renderMostPixels    = 1 << 27
)

func (Render) Name() string { return "render" }

func (Render) Describe() string {
	return `Renders a stored Blender scene with Blender 4.5 on processors, shared across the pool by frames (each task renders a run of frames) or by tiles (each task renders a horizontal strip of one frame, and the strips are put together). Textures must be packed into the .blend file; Python in the file is not run. Parameters: {"input": "<CID of a stored .blend file>", "frame_start": <optional first frame, default the file's>, "frame_end": <optional last frame, default the file's>, "engine": "<CYCLES or BLENDER_EEVEE_NEXT, default CYCLES>", "samples": <optional samples per pixel, default the file's>, "resolution_percent": <optional percentage of the file's resolution, default the file's>, "format": "<PNG or EXR (by frames only), default PNG>", "split": "<frames or tiles, default frames>", "tasks": <optional number of runs of frames, or of strips>}. Result: {"split": "<frames or tiles>", "frames": <n>, "images": ["<CID of each frame>", ...], "archive": "<CID of a zip of the frames>", "image": "<CID of the whole frame, when split by tiles>", "tiles": <n>, "width": <pixels>, "height": <pixels>, "engine": "<engine>", "seconds": <rendering time, added up>}.`
}

func (Render) Form() []Field {
	return []Field{
		{Name: "input", Kind: KindFile, Label: "Scene", Help: "The stored .blend file to render, with its textures packed into it.", Required: true},
		{Name: "split", Kind: KindChoice, Label: "Share out by", Help: "Frames gives each task a run of frames; tiles gives each a strip of one frame and puts the strips together.", Default: "frames", Choices: slices.Clone(renderSplits)},
		{Name: "frame_start", Kind: KindInteger, Label: "First frame", Help: "The first frame to render, or the only one split by tiles; empty for the file's.", Minimum: bound(0), Maximum: bound(renderLastFrame)},
		{Name: "frame_end", Kind: KindInteger, Label: "Last frame", Help: "The last frame to render; empty for the file's.", Minimum: bound(0), Maximum: bound(renderLastFrame)},
		{Name: "engine", Kind: KindChoice, Label: "Engine", Help: "Cycles traces light on the processor; EEVEE needs OpenGL, which is drawn here in software and is slower.", Default: "CYCLES", Choices: slices.Clone(renderEngines)},
		{Name: "samples", Kind: KindInteger, Label: "Samples", Help: "How many samples to take of each pixel, more for less noise; empty for the file's.", Minimum: bound(1), Maximum: bound(renderMostSamples)},
		{Name: "resolution_percent", Kind: KindInteger, Label: "Size", Help: "The size to render at, as a percentage of the file's resolution; empty for the file's.", Minimum: bound(1), Maximum: bound(renderMostPercent)},
		{Name: "format", Kind: KindChoice, Label: "Format", Help: "PNG, or OpenEXR's floating point for frames to be graded later.", Default: "PNG", Choices: slices.Clone(renderFormats)},
		{Name: "tasks", Kind: KindInteger, Label: "Tasks", Help: "How many runs of frames or strips to share the job out as, or 0 to leave it to the pool.", Default: 0, Minimum: bound(0), Maximum: bound(MaxTasks)},
	}
}

func (Render) Split(ctx context.Context, blobs Blobs, params []byte, parts int) ([][]byte, error) {
	var p RenderParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("render parameters: %w", err)
	}
	if _, err := cid.Decode(p.Input); err != nil {
		return nil, fmt.Errorf("render parameters: input is not the CID of a stored .blend file: %w", err)
	}
	p.Engine = cmp.Or(p.Engine, "CYCLES")
	p.Format = cmp.Or(p.Format, "PNG")
	p.Split = cmp.Or(p.Split, "frames")
	for name, frame := range map[string]*int{"frame_start": p.FrameStart, "frame_end": p.FrameEnd} {
		if frame != nil && (*frame < 0 || *frame > renderLastFrame) {
			return nil, fmt.Errorf("render parameters: %s is %d, and must be from 0 to %d", name, *frame, renderLastFrame)
		}
	}
	switch {
	case !slices.Contains(renderEngines, p.Engine):
		return nil, fmt.Errorf("render parameters: engine is %q, and must be one of %s", p.Engine, strings.Join(renderEngines, ", "))
	case !slices.Contains(renderFormats, p.Format):
		return nil, fmt.Errorf("render parameters: format is %q, and must be one of %s", p.Format, strings.Join(renderFormats, ", "))
	case !slices.Contains(renderSplits, p.Split):
		return nil, fmt.Errorf("render parameters: split is %q, and must be one of %s", p.Split, strings.Join(renderSplits, ", "))
	case p.Split == "tiles" && p.Format != "PNG":
		return nil, fmt.Errorf("render parameters: split by tiles, the format must be PNG, which is what strips are put together from")
	case p.FrameStart != nil && p.FrameEnd != nil && *p.FrameEnd < *p.FrameStart:
		return nil, fmt.Errorf("render parameters: frame_end is %d, before frame_start, %d", *p.FrameEnd, *p.FrameStart)
	case p.Samples < 0 || p.Samples > renderMostSamples:
		return nil, fmt.Errorf("render parameters: samples is %d, and must be from 1 to %d", p.Samples, renderMostSamples)
	case p.ResolutionPercent < 0 || p.ResolutionPercent > renderMostPercent:
		return nil, fmt.Errorf("render parameters: resolution_percent is %d, and must be from 1 to %d", p.ResolutionPercent, renderMostPercent)
	case p.Tasks < 0 || p.Tasks > MaxTasks:
		return nil, fmt.Errorf("render parameters: tasks is %d, and must be from 1 to %d, or 0 to leave it to the pool", p.Tasks, MaxTasks)
	}
	count := parts
	if p.Tasks > 0 {
		count = p.Tasks
	}
	// Where the frames are known, there are no more tasks than frames, and
	// no fewer than it takes for none to render more than a task may.
	if p.Split == "frames" && p.FrameStart != nil && p.FrameEnd != nil {
		frames := *p.FrameEnd - *p.FrameStart + 1
		count = max(min(count, frames), (frames+renderFramesPerTask-1)/renderFramesPerTask)
	}
	frame := func(f *int) string {
		if f == nil {
			return ""
		}
		return strconv.Itoa(*f)
	}
	given := func(n int) string {
		if n == 0 {
			return ""
		}
		return strconv.Itoa(n)
	}
	return Container{}.Split(ctx, blobs, mustJSON(ContainerParams{
		Image:   RenderImage,
		Command: []string{"/opt/blender/blender", "--background", "--disable-autoexec", "--python-exit-code", "1", "/input/data", "--python-expr", renderScript},
		Input:   p.Input, Tasks: count,
		Env: map[string]string{
			"SPLIT": p.Split, "ENGINE": p.Engine, "FORMAT": p.Format,
			"FRAME_START": frame(p.FrameStart), "FRAME_END": frame(p.FrameEnd),
			"SAMPLES": given(p.Samples), "PERCENT": given(p.ResolutionPercent),
		},
	}), parts)
}

func (r Render) Execute(ctx context.Context, blobs Blobs, payload []byte) ([]byte, error) {
	engine := r.Engine
	if engine == nil {
		engine = docker
	}
	if err := haveImage(ctx, engine, RenderImage, renderDockerfile, "rendering"); err != nil {
		return nil, err
	}
	return Container{Engine: engine, Confined: r.Confined}.Execute(ctx, blobs, payload)
}

// Varies says that renders can differ between runs: Cycles' noise depends
// on the processor's arithmetic and how the work is shared among threads.
func (Render) Varies() bool { return true }

// renderSummary is what a task says it did.
type renderSummary struct {
	Split   string  `json:"split"`
	Frames  []int   `json:"frames"`
	Seconds float64 `json:"seconds"`
	Width   int     `json:"width"`
	Height  int     `json:"height"`
	Engine  string  `json:"engine"`
}

func (Render) Aggregate(ctx context.Context, blobs Blobs, outputs [][]byte) ([]byte, error) {
	tasks := make([]ContainerOutput, len(outputs))
	var result RenderResult
	for i, output := range outputs {
		if err := json.Unmarshal(output, &tasks[i]); err != nil {
			return nil, fmt.Errorf("task %d output: %w", i, err)
		}
		c, err := cid.Decode(tasks[i].Files["summary.json"])
		if err != nil {
			return nil, fmt.Errorf("task %d left no summary", i)
		}
		blob, err := blobs.Open(ctx, c)
		if err != nil {
			return nil, fmt.Errorf("task %d's summary: %w", i, err)
		}
		var summary renderSummary
		err = json.NewDecoder(io.LimitReader(blob, 1<<20)).Decode(&summary)
		blob.Close()
		if err != nil {
			return nil, fmt.Errorf("task %d's summary: %w", i, err)
		}
		result.Split, result.Width, result.Height, result.Engine = summary.Split, summary.Width, summary.Height, summary.Engine
		result.Seconds += summary.Seconds
	}
	if result.Split == "tiles" {
		return stitch(ctx, blobs, tasks, result)
	}
	return gatherFrames(ctx, blobs, tasks, result)
}

// gatherFrames lists the frames the tasks rendered, in order, and stores
// them as one zip file too.
func gatherFrames(ctx context.Context, blobs Blobs, tasks []ContainerOutput, result RenderResult) ([]byte, error) {
	var names []string
	for i, task := range tasks {
		// Frames are named by their number, padded to seven digits, the
		// most Blender's have: in order by name is in order by number.
		var own []string
		for name := range task.Files {
			if strings.HasPrefix(name, "frame_") {
				if !validRenderFrameName(name) {
					return nil, fmt.Errorf("task %d left a frame with an unsafe name %q", i, name)
				}
				own = append(own, name)
			}
		}
		slices.Sort(own)
		for _, name := range own {
			names = append(names, name)
			result.Images = append(result.Images, task.Files[name])
		}
	}
	result.Frames = len(names)
	// The archive is written as it is stored, so that however many frames
	// there are, no more than one is held at a time.
	reading, writing := io.Pipe()
	go func() { writing.CloseWithError(zipFrames(ctx, blobs, writing, names, result.Images)) }()
	archive, err := blobs.Put(ctx, reading)
	// Should the store stop reading early, the writing stops too.
	reading.CloseWithError(errors.New("the archive is no longer being stored"))
	if err != nil {
		return nil, fmt.Errorf("store the frames as one zip file: %w", err)
	}
	result.Archive = archive.String()
	return mustJSON(result), nil
}

// validRenderFrameName accepts only Blender's frame-number output names.
// Task outputs come from workers, so a prefix check alone is not enough to
// make a name safe to put into the user's ZIP archive.
func validRenderFrameName(name string) bool {
	if path.Base(name) != name || strings.Contains(name, `\`) || !strings.HasPrefix(name, "frame_") {
		return false
	}
	ext := path.Ext(name)
	if ext != ".png" && ext != ".exr" {
		return false
	}
	frame := strings.TrimSuffix(strings.TrimPrefix(name, "frame_"), ext)
	if len(frame) < 7 {
		return false
	}
	for _, digit := range frame {
		if digit < '0' || digit > '9' {
			return false
		}
	}
	return true
}

// zipFrames writes a zip file of the frames with the given names and CIDs.
// They are stored as they are, being compressed already.
func zipFrames(ctx context.Context, blobs Blobs, w io.Writer, names, cids []string) error {
	archive := zip.NewWriter(w)
	for i, name := range names {
		if err := zipFrame(ctx, blobs, archive, name, cids[i]); err != nil {
			return err
		}
	}
	return archive.Close()
}

func zipFrame(ctx context.Context, blobs Blobs, archive *zip.Writer, name, id string) error {
	c, err := cid.Decode(id)
	if err != nil {
		return fmt.Errorf("frame %s: %w", name, err)
	}
	blob, err := blobs.Open(ctx, c)
	if err != nil {
		return fmt.Errorf("frame %s: %w", name, err)
	}
	defer blob.Close()
	entry, err := archive.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Store})
	if err == nil {
		_, err = io.Copy(entry, blob)
	}
	return err
}

// stitch puts the strips the tasks rendered together, the first at the
// top, into the whole frame.
func stitch(ctx context.Context, blobs Blobs, tasks []ContainerOutput, result RenderResult) ([]byte, error) {
	strips := make([]image.Image, len(tasks))
	width, height := 0, 0
	for i, task := range tasks {
		c, err := cid.Decode(task.Files["tile.png"])
		if err != nil {
			return nil, fmt.Errorf("strip %d was not rendered", i)
		}
		blob, err := blobs.Open(ctx, c)
		if err != nil {
			return nil, fmt.Errorf("strip %d: %w", i, err)
		}
		strip, err := decodeStrip(blob, height)
		blob.Close()
		if err != nil {
			return nil, fmt.Errorf("strip %d: %w", i, err)
		}
		if i == 0 {
			width = strip.Bounds().Dx()
		}
		if strip.Bounds().Dx() != width {
			return nil, fmt.Errorf("strip %d is %d pixels wide, and strip 0 is %d", i, strip.Bounds().Dx(), width)
		}
		strips[i] = strip
		height += strip.Bounds().Dy()
	}
	whole := image.NewNRGBA(image.Rect(0, 0, width, height))
	top := 0
	for _, strip := range strips {
		b := strip.Bounds()
		draw.Draw(whole, image.Rect(0, top, width, top+b.Dy()), strip, b.Min, draw.Src)
		top += b.Dy()
	}
	var encoded bytes.Buffer
	png.Encode(&encoded, whole) // an image in memory to memory: this cannot fail
	stored, err := blobs.Put(ctx, &encoded)
	if err != nil {
		return nil, fmt.Errorf("store the whole frame: %w", err)
	}
	result.Image, result.Tiles, result.Frames = stored.String(), len(tasks), 1
	result.Width, result.Height = width, height
	return mustJSON(result), nil
}

// decodeStrip decodes a strip of PNG, refusing one that would take the
// frame, already above pixels high, past the most pixels a frame may have
// before it is decoded.
func decodeStrip(r io.Reader, above int) (image.Image, error) {
	var head bytes.Buffer
	config, err := png.DecodeConfig(io.TeeReader(r, &head))
	if err != nil {
		return nil, err
	}
	if config.Width*(above+config.Height) > renderMostPixels {
		return nil, fmt.Errorf("a frame of %dx%d pixels is more than the %d a frame may have", config.Width, above+config.Height, renderMostPixels)
	}
	return png.Decode(io.MultiReader(&head, r))
}

// ResultBlobs are the frames the result refers to, which the tasks stored.
// The archive and the whole frame are stored by Aggregate, and kept with
// the result as everything it stores is.
func (Render) ResultBlobs(raw []byte) []string {
	var result RenderResult
	json.Unmarshal(raw, &result)
	return result.Images
}
