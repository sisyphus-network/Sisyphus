package runtime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/ipfs/go-cid"
)

// Transcode re-encodes a stored video as H.264 with AAC sound, optionally
// at a smaller size. The video is cut into as many stretches as the job
// has tasks; each task encodes one, in a container with ffmpeg, and the
// stretches are joined into one MPEG transport stream.
//
// It is the container workload with the image and the command fixed, so it
// runs only on workers that run containers. The image is built on a worker
// the first time it is needed, from the few lines below.
type Transcode struct {
	// Engine runs the container engine, as Container's does.
	Engine func(ctx context.Context, args []string, stdout, stderr io.Writer) (status int, err error)
}

// TranscodeParams are a transcode job's parameters.
type TranscodeParams struct {
	// Input is the CID of the stored video.
	Input string `json:"input"`
	// Height, if not zero, is the height in pixels to scale the picture
	// to, keeping its shape.
	Height int `json:"height,omitempty"`
	// CRF is the quality to encode at, from 1 (best, largest) to 51. Zero
	// means 23.
	CRF int `json:"crf,omitempty"`
	// Preset trades encoding time for size, from ultrafast to veryslow.
	// Empty means medium.
	Preset string `json:"preset,omitempty"`
	// Segments is how many stretches to cut the video into. Zero leaves it
	// to the pool.
	Segments int `json:"segments,omitempty"`
}

// TranscodeResult is a transcode job's result.
type TranscodeResult struct {
	// Output is the CID of the encoded video, an MPEG transport stream.
	Output   string `json:"output"`
	Segments int    `json:"segments"`
	Bytes    uint64 `json:"bytes"`
}

// transcodeDockerfile is the whole of the image transcoding runs in.
const transcodeDockerfile = "FROM alpine:3.20\nRUN apk add --no-cache ffmpeg\n"

// TranscodeImage names that image. The name carries what the image is made
// from, so that a change to it is a different image.
var TranscodeImage = func() string {
	sum := sha256.Sum256([]byte(transcodeDockerfile))
	return "sisyphus-transcode:" + hex.EncodeToString(sum[:6])
}()

// transcodeSegment is what each task runs. It finds how long the video
// is, takes the stretch that is its own, and encodes it with timestamps
// that follow on from the stretch before, so that the stretches joined end
// to end are one stream.
const transcodeSegment = `set -eu
duration=$(ffprobe -v error -show_entries format=duration -of csv=p=0 /input/data)
start=$(awk -v d="$duration" -v i="$SISYPHUS_TASK_INDEX" -v n="$SISYPHUS_TASK_COUNT" 'BEGIN { printf "%.6f", d * i / n }')
length=$(awk -v d="$duration" -v n="$SISYPHUS_TASK_COUNT" 'BEGIN { printf "%.6f", d / n }')
echo "encoding $length seconds from $start of $duration" >&2
set -- -c:v libx264 -preset "$PRESET" -crf "$CRF" -c:a aac
if [ "$HEIGHT" != 0 ]; then set -- -vf "scale=-2:$HEIGHT" "$@"; fi
ffmpeg -nostdin -loglevel error -ss "$start" -t "$length" -i /input/data "$@" -muxdelay 0 -muxpreload 0 -output_ts_offset "$start" -f mpegts /output/segment.ts
echo "encoded $(wc -c < /output/segment.ts) bytes" >&2
`

// transcodePresets are the presets of the encoder, fastest first.
var transcodePresets = []string{"ultrafast", "superfast", "veryfast", "faster", "fast", "medium", "slow", "slower", "veryslow"}

func (Transcode) Name() string { return "transcode" }

func (Transcode) Describe() string {
	return `Re-encodes a stored video as H.264 video with AAC sound, cut into stretches that are encoded on different workers and joined. Parameters: {"input": "<CID of the stored video>", "height": <optional height in pixels to scale to, an even number>, "crf": <optional quality from 1 (best) to 51, default 23>, "preset": "<optional, from ultrafast to veryslow, default medium>", "segments": <optional number of stretches>}. Result: {"output": "<CID of the encoded video, an MPEG transport stream>", "segments": <n>, "bytes": <its size>}.`
}

func (Transcode) Split(ctx context.Context, blobs Blobs, params []byte, parts int) ([][]byte, error) {
	var p TranscodeParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("transcode parameters: %w", err)
	}
	if _, err := cid.Decode(p.Input); err != nil {
		return nil, fmt.Errorf("transcode parameters: input is not the CID of a stored video: %w", err)
	}
	if p.CRF == 0 {
		p.CRF = 23
	}
	if p.Preset == "" {
		p.Preset = "medium"
	}
	switch {
	case p.CRF < 1 || p.CRF > 51:
		return nil, fmt.Errorf("transcode parameters: crf is %d, and must be from 1 to 51", p.CRF)
	case !slices.Contains(transcodePresets, p.Preset):
		return nil, fmt.Errorf("transcode parameters: preset is %q, and must be one of %s", p.Preset, strings.Join(transcodePresets, ", "))
	case p.Height < 0 || p.Height%2 != 0:
		return nil, fmt.Errorf("transcode parameters: height is %d, and must be an even number of pixels", p.Height)
	}
	return Container{}.Split(ctx, blobs, mustJSON(ContainerParams{
		Image: TranscodeImage, Command: []string{"sh", "-c", transcodeSegment},
		Input: p.Input, Tasks: p.Segments,
		Env: map[string]string{"CRF": strconv.Itoa(p.CRF), "PRESET": p.Preset, "HEIGHT": strconv.Itoa(p.Height)},
	}), parts)
}

func (t Transcode) Execute(ctx context.Context, blobs Blobs, payload []byte) ([]byte, error) {
	engine := t.Engine
	if engine == nil {
		engine = docker
	}
	if err := haveTranscodeImage(ctx, engine); err != nil {
		return nil, err
	}
	return Container{Engine: engine}.Execute(ctx, blobs, payload)
}

// buildingImage lets one task at a time see to the image, so that tasks
// starting together on a new worker build it once between them.
var buildingImage sync.Mutex

// haveTranscodeImage builds the image transcoding runs in, unless this
// machine has it already.
func haveTranscodeImage(ctx context.Context, engine func(context.Context, []string, io.Writer, io.Writer) (int, error)) error {
	buildingImage.Lock()
	defer buildingImage.Unlock()
	if status, err := engine(ctx, []string{"image", "inspect", TranscodeImage}, io.Discard, io.Discard); err == nil && status == 0 {
		return nil
	}
	dir, err := os.MkdirTemp("", "sisyphus-image-")
	if err != nil {
		return fmt.Errorf("make a directory to build the transcoding image in: %w", err)
	}
	defer os.RemoveAll(dir)
	os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(transcodeDockerfile), 0o644) // in a directory just made, this cannot fail
	Report(ctx).Log("building the image transcoding runs in, which this machine does once")
	var said bytes.Buffer
	status, err := engine(ctx, []string{"build", "--tag", TranscodeImage, dir}, io.Discard, &said)
	if err == nil && status != 0 {
		lines := strings.Split(strings.TrimSpace(said.String()), "\n")
		err = errors.New(lines[len(lines)-1])
	}
	if err != nil {
		return fmt.Errorf("build the image transcoding runs in: %w", err)
	}
	return nil
}

func (Transcode) Aggregate(ctx context.Context, blobs Blobs, outputs [][]byte) ([]byte, error) {
	stretches := make([]io.Reader, len(outputs))
	for i, output := range outputs {
		var out ContainerOutput
		if err := json.Unmarshal(output, &out); err != nil {
			return nil, fmt.Errorf("task %d output: %w", i, err)
		}
		c, err := cid.Decode(out.Files["segment.ts"])
		if err != nil {
			return nil, fmt.Errorf("task %d left no encoded video", i)
		}
		blob, err := blobs.Open(ctx, c)
		if err != nil {
			return nil, fmt.Errorf("task %d's encoded video: %w", i, err)
		}
		defer blob.Close()
		stretches[i] = blob
	}
	// A transport stream is made to be cut and joined anywhere, so the
	// whole is the stretches end to end.
	whole := &counted{r: io.MultiReader(stretches...)}
	stored, err := blobs.Put(ctx, whole)
	if err != nil {
		return nil, fmt.Errorf("store the encoded video: %w", err)
	}
	return mustJSON(TranscodeResult{Output: stored.String(), Segments: len(outputs), Bytes: whole.n}), nil
}

// counted counts what is read through it.
type counted struct {
	r io.Reader
	n uint64
}

func (c *counted) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += uint64(n)
	return n, err
}
