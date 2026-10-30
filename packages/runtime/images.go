package runtime

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	goruntime "runtime"
	"slices"
	"strconv"
	"strings"

	"github.com/ipfs/go-cid"
	"golang.org/x/sys/cpu"
)

// Images edits a batch of stored pictures with libvips: each is turned
// upright by its EXIF orientation, made to fit within a width, a height or
// both, shrinking only unless asked to enlarge, saved as JPEG, PNG, WebP,
// TIFF or GIF, or in its own format, and, unless asked to keep it, has its
// metadata taken out, which in a photo's EXIF can say where it was taken.
// The pictures are shared out across the pool, each task editing a run of
// them one after another.
//
// A job may also ask for each picture to be upscaled 2, 3 or 4 times with
// Real-ESRGAN before it is fitted and saved: on the worker's graphics card
// if the job asks for one, and otherwise on its processor, through Mesa's
// software Vulkan, lavapipe.
//
// libvips is run with the loaders it does not trust with hostile files
// blocked, so only the formats taken here are read; SVG, PDF and the like
// are refused as unknown, as they are by the coordinator, which reads each
// file's first bytes before the job is shared out.
//
// Like Transcode, it is the container workload with the image and the
// command fixed, so it runs only on workers that run containers. The image
// is built on a worker the first time it is needed, from Alpine's own
// libvips; a job that upscales runs in a second image, built only where
// one is asked for, from Debian's libvips and lavapipe and Real-ESRGAN's
// release.
type Images struct {
	// Engine runs the container engine, as Container's does.
	Engine func(ctx context.Context, args []string, stdout, stderr io.Writer) (status int, err error)
	// Confined holds its containers in as Container's does.
	Confined Confinement
}

// ImagesParams are an images job's parameters.
type ImagesParams struct {
	// Inputs are the CIDs of the stored pictures, each a PNG, JPEG, WebP,
	// TIFF or GIF; Input, in their place, that of one.
	Input  string   `json:"input,omitempty"`
	Inputs []string `json:"inputs,omitempty"`
	// Width and Height are the box, in pixels, each picture is made to
	// fit within, keeping its shape: zero leaves that side free, and both
	// zero leaves the size as it is.
	Width  int `json:"width,omitempty"`
	Height int `json:"height,omitempty"`
	// Enlarge makes a picture smaller than the box as big as fits in it.
	// Without it, only those bigger than the box are made smaller.
	Enlarge bool `json:"enlarge,omitempty"`
	// Format is what to save each as, one of imagesFormats. Empty means
	// same, each in its own.
	Format string `json:"format,omitempty"`
	// Quality is how faithfully to save a JPEG or WebP, from 1 to 100.
	// Zero means 85.
	Quality int `json:"quality,omitempty"`
	// Keep keeps each picture's metadata. Without it, it is taken out.
	Keep bool `json:"keep_metadata,omitempty"`
	// Upscale, if not zero, is how many times bigger, 2, 3 or 4, Real-
	// ESRGAN makes each picture before it is fitted to the box and saved.
	// Model is the network it does it with, one of imagesModels, the
	// first unless said; it is left out where there is no upscaling. GPU
	// upscales on a graphics card of the worker's.
	Upscale int    `json:"upscale,omitempty"`
	Model   string `json:"model,omitempty"`
	GPU     bool   `json:"gpu,omitempty"`
	// Tasks is how many runs of pictures to share them out as. Zero leaves
	// it to the pool; there are never more runs than pictures.
	Tasks int `json:"tasks,omitempty"`
}

// ImagesPicture is what one picture came to.
type ImagesPicture struct {
	// Index is which of the job's pictures it is, from 0.
	Index int `json:"index"`
	// Picture is the CID of the edited picture, Name what it is called in
	// Files and the archive, Format what it was saved as, and Width,
	// Height and Bytes its size.
	Picture string `json:"picture,omitempty"`
	Name    string `json:"name,omitempty"`
	Format  string `json:"format,omitempty"`
	Width   int    `json:"width,omitempty"`
	Height  int    `json:"height,omitempty"`
	Bytes   uint64 `json:"bytes,omitempty"`
	// Error says why a picture was not made, and Log is the CID of the
	// lines libvips wrote that say more.
	Error string `json:"error,omitempty"`
	Log   string `json:"log,omitempty"`
}

// ImagesResult is an images job's result.
type ImagesResult struct {
	Width   int    `json:"width"`
	Height  int    `json:"height"`
	Enlarge bool   `json:"enlarge"`
	Format  string `json:"format"`
	Quality int    `json:"quality"`
	Keep    bool   `json:"keep_metadata"`
	Upscale int    `json:"upscale"`
	Model   string `json:"model,omitempty"`
	GPU     bool   `json:"gpu"`
	// Pictures are what each picture came to, in the order of the files.
	// Made and Failed count them.
	Pictures []ImagesPicture `json:"pictures"`
	Made     int             `json:"made"`
	Failed   int             `json:"failed"`
	// Files are the edited pictures by name, image-000.webp and so on by
	// their index, and Archive the CID of a zip file of them.
	Files   map[string]string `json:"files"`
	Archive string            `json:"archive"`
	// Tasks is how many tasks the job was shared out as.
	Tasks int `json:"tasks"`
}

// imagesFormats are what a job may save its pictures as, and imagesSuffix
// the file name's ending each is saved with: same saves each in the format
// it is in.
var (
	imagesFormats = []string{"same", "jpeg", "png", "webp", "tiff", "gif"}
	imagesSuffix  = map[string]string{"jpeg": "jpg", "png": "png", "webp": "webp", "tiff": "tif", "gif": "gif"}
)

// imagesModels are the Real-ESRGAN networks a job may upscale with, the
// first for photos and the like, the others for drawn pictures, and
// imagesNative the factors each upscales by itself. realesrgan-x4plus and
// realesrgan-x4plus-anime upscale by 4 only, so for 2 or 3 their picture
// is made smaller again with libvips, as Real-ESRGAN's own --outscale
// does; realesr-animevideov3, a smaller network made for frames of drawn
// video, has one for each factor.
var (
	imagesModels = []string{"realesrgan-x4plus", "realesrgan-x4plus-anime", "realesr-animevideov3"}
	imagesNative = map[string][]int{"realesrgan-x4plus": {4}, "realesrgan-x4plus-anime": {4}, "realesr-animevideov3": {2, 3, 4}}
)

// imagesUpscales are the factors a job may upscale by.
var imagesUpscales = []int{2, 3, 4}

// Bounds of an images job: how many pictures it may have; the most pixels
// a side of the box may have; the most bytes a stored picture, and an
// edited one, may have; and the most bytes of a log of one that failed.
const (
	imagesMostPictures = 256
	imagesMostSide     = 16384
	imagesMostInput    = 256 << 20
	imagesMostFile     = 1 << 30
	imagesMostLog      = 64 << 10
)

// imagesSettings are what a job asks of each task.
type imagesSettings struct {
	Format                 string
	Width, Height, Quality int
	Enlarge, Keep          bool
	// Upscale is 0 for none; Model is empty where it is.
	Upscale int
	Model   string
	GPU     bool
}

// check refuses settings off the menu or out of bounds.
func (s imagesSettings) check() error {
	switch {
	case !slices.Contains(imagesFormats, s.Format):
		return fmt.Errorf("format is %q, and must be one of %s", s.Format, strings.Join(imagesFormats, ", "))
	case s.Width < 0 || s.Width > imagesMostSide:
		return fmt.Errorf("width is %d, and must be from 1 to %d, or 0 to leave it free", s.Width, imagesMostSide)
	case s.Height < 0 || s.Height > imagesMostSide:
		return fmt.Errorf("height is %d, and must be from 1 to %d, or 0 to leave it free", s.Height, imagesMostSide)
	case s.Quality < 1 || s.Quality > 100:
		return fmt.Errorf("quality is %d, and must be from 1 to 100", s.Quality)
	case s.Enlarge && s.Width == 0 && s.Height == 0:
		return errors.New("enlarge is asked for with no width or height to enlarge to")
	case s.Upscale == 0 && (s.Model != "" || s.GPU):
		return errors.New("a model or a graphics card is asked for with no upscale to use it for")
	case s.Upscale != 0 && !slices.Contains(imagesUpscales, s.Upscale):
		return fmt.Errorf("upscale is %d, and must be 2, 3 or 4, or 0 to leave the pictures as big as they are", s.Upscale)
	case s.Upscale != 0 && !slices.Contains(imagesModels, s.Model):
		return imagesModelRefused(s.Model)
	case s.Upscale != 0 && s.Keep:
		return errors.New("keep_metadata cannot be had with upscale: Real-ESRGAN keeps none of a picture's metadata")
	}
	return nil
}

// imagesModelRefused says a model is not one a job may upscale with.
func imagesModelRefused(model string) error {
	return fmt.Errorf("model is %q, and must be one of %s", model, strings.Join(imagesModels, ", "))
}

// native is the factor Real-ESRGAN upscales by itself, as near the one
// asked for as its model has.
func (s imagesSettings) native() int {
	native := imagesNative[s.Model]
	if slices.Contains(native, s.Upscale) {
		return s.Upscale
	}
	return slices.Max(native)
}

// size is the size libvips' vipsthumbnail is given: the box, with > to
// shrink only, or, with no box, one no picture is bigger than.
func (s imagesSettings) size() string {
	box := "10000000x10000000"
	switch {
	case s.Width > 0 && s.Height > 0:
		box = fmt.Sprintf("%dx%d", s.Width, s.Height)
	case s.Width > 0:
		box = fmt.Sprintf("%dx", s.Width)
	case s.Height > 0:
		box = fmt.Sprintf("x%d", s.Height)
	}
	if !s.Enlarge {
		box += ">"
	}
	return box
}

// env is the settings as a task's environment. A task that upscales is
// also told the factor Real-ESRGAN upscales by itself, NATIVE, what to
// make its picture smaller by after, SHRINK, and the most pixels a side
// of the picture it makes may have, MOST_SIDE; and, on a graphics card,
// that the card's Vulkan driver is wanted in the container.
func (s imagesSettings) env() map[string]string {
	env := map[string]string{
		"FORMAT": s.Format, "WIDTH": strconv.Itoa(s.Width), "HEIGHT": strconv.Itoa(s.Height),
		"ENLARGE": strconv.FormatBool(s.Enlarge), "QUALITY": strconv.Itoa(s.Quality), "KEEP": strconv.FormatBool(s.Keep),
		"SIZE": s.size(), "UPSCALE": strconv.Itoa(s.Upscale), "MODEL": s.Model, "GPU": strconv.FormatBool(s.GPU),
	}
	if s.Upscale > 0 {
		env["NATIVE"] = strconv.Itoa(s.native())
		env["SHRINK"] = strconv.FormatFloat(float64(s.Upscale)/float64(s.native()), 'f', -1, 64)
		env["MOST_SIDE"] = strconv.Itoa(imagesMostSide)
	}
	if s.GPU {
		env["NVIDIA_DRIVER_CAPABILITIES"] = "compute,utility,graphics"
	}
	return env
}

// image is the image a task with these settings runs in, the Dockerfile
// it is built from, and what it is for.
func (s imagesSettings) image() (tag, dockerfile, what string) {
	if s.Upscale > 0 {
		return ImagesUpscaleImage, imagesUpscaleDockerfile, "upscaling pictures"
	}
	return ImagesImage, imagesDockerfile, "editing pictures"
}

// imagesSettingsOf reads and checks the settings a task's environment, or
// a task's summary, carries, by the names of the environment. A task or a
// summary from before upscaling was offered says nothing of it, and asks
// for none.
func imagesSettingsOf(values map[string]string) (imagesSettings, error) {
	s := imagesSettings{Format: values["FORMAT"], Model: values["MODEL"]}
	var errs [7]error
	s.Width, errs[0] = strconv.Atoi(values["WIDTH"])
	s.Height, errs[1] = strconv.Atoi(values["HEIGHT"])
	s.Quality, errs[2] = strconv.Atoi(values["QUALITY"])
	s.Enlarge, errs[3] = strconv.ParseBool(values["ENLARGE"])
	s.Keep, errs[4] = strconv.ParseBool(values["KEEP"])
	s.Upscale, errs[5] = strconv.Atoi(cmp.Or(values["UPSCALE"], "0"))
	s.GPU, errs[6] = strconv.ParseBool(cmp.Or(values["GPU"], "false"))
	if err := errors.Join(errs[:]...); err != nil {
		return imagesSettings{}, fmt.Errorf("settings are not numbers and yes or no: %w", err)
	}
	return s, s.check()
}

// pictureKind says what the start of a stored file says it is: one of the
// formats a picture is taken in, or none.
func pictureKind(head []byte) string {
	switch {
	case bytes.HasPrefix(head, []byte("\x89PNG\r\n\x1a\n")):
		return "png"
	case bytes.HasPrefix(head, []byte("\xff\xd8\xff")):
		return "jpeg"
	case len(head) >= 12 && bytes.HasPrefix(head, []byte("RIFF")) && string(head[8:12]) == "WEBP":
		return "webp"
	case bytes.HasPrefix(head, []byte("II*\x00")), bytes.HasPrefix(head, []byte("MM\x00*")):
		return "tiff"
	case bytes.HasPrefix(head, []byte("GIF87a")), bytes.HasPrefix(head, []byte("GIF89a")):
		return "gif"
	}
	return ""
}

// imagesDockerfile is the whole of the image pictures are edited in:
// Alpine's libvips and its command-line tools.
const imagesDockerfile = `FROM alpine:3.20
RUN apk add --no-cache vips-tools
`

// ImagesImage names that image. The name carries what the image is made
// from, as TranscodeImage's does.
var ImagesImage = func() string {
	sum := sha256.Sum256([]byte(imagesDockerfile))
	return "sisyphus-images:" + hex.EncodeToString(sum[:6])
}()

// imagesUpscaleDockerfile is the image pictures are upscaled in. Real-
// ESRGAN's release of realesrgan-ncnn-vulkan is built for glibc, and needs
// its libmvec, which Alpine's musl has no stand-in for, so the image is
// Debian's: its libvips, the Vulkan loader, and Mesa's Vulkan drivers, of
// which lavapipe runs Vulkan on the processor where no graphics card is
// given. Debian 13's, for its newer Mesa and LLVM. The release is fetched
// at a pinned version and checked against its SHA-256, and so is each
// file taken from it: the program and the three networks' weights. The
// release is for x86-64 alone, and a build elsewhere stops with a line
// that says so.
const imagesUpscaleDockerfile = `FROM debian:trixie-slim AS fetch
RUN [ "$(uname -m)" = x86_64 ] || { echo "Real-ESRGAN's release runs on x86-64 only, and this is $(uname -m)" >&2; exit 1; }
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl unzip && rm -rf /var/lib/apt/lists/*
RUN curl -fsSLo /realesrgan.zip https://github.com/xinntao/Real-ESRGAN/releases/download/v0.2.5.0/realesrgan-ncnn-vulkan-20220424-ubuntu.zip \
 && echo "e5aa6eb131234b87c0c51f82b89390f5e3e642b7b70f2b9bbe95b6a285a40c96  /realesrgan.zip" | sha256sum -c - \
 && mkdir /unpacked && unzip -q /realesrgan.zip -d /unpacked && cd /unpacked \
 && printf '%s\n' \
  "22c2296a39b40cc43ac92615c827ab6d4b60b7142e8cee59f718e3f8e2cdbda9  realesrgan-ncnn-vulkan" \
  "713ee713b0353afaa27976f0563a64a5043bd70b9bd8936c2e26e25ebcdbcddf  models/realesrgan-x4plus.bin" \
  "35330ececcea33b6c397a72548e788d5d53becee4734c50b7fada36e89f10a86  models/realesrgan-x4plus.param" \
  "fe01c269cfd10cdef8e018ab66ebe750cf79c7af4d1f9c16c737e1295229bacc  models/realesrgan-x4plus-anime.bin" \
  "2b8fb6e0ae4d2d85704ca08c119a2f5ea40add4f2ecd512eb7f4cd44b6127ed4  models/realesrgan-x4plus-anime.param" \
  "548a36f9c3f4ab8da56cd3b13badf23968bee207b396dad14d04b830e5f2ab2d  models/realesr-animevideov3-x2.bin" \
  "b88ff4f00ebf019a7fdac17fdd45a7fd3665d37509efc5baf2e4da2e24420a04  models/realesr-animevideov3-x2.param" \
  "548a36f9c3f4ab8da56cd3b13badf23968bee207b396dad14d04b830e5f2ab2d  models/realesr-animevideov3-x3.bin" \
  "d1a5755008791d09b57e3425fc9dd0bd26b00fdf79c606210bc0e693f8230881  models/realesr-animevideov3-x3.param" \
  "548a36f9c3f4ab8da56cd3b13badf23968bee207b396dad14d04b830e5f2ab2d  models/realesr-animevideov3-x4.bin" \
  "850a248e7c14c27e5bd8cf7265113a9441036a7db63963bb8aa5169d788a435e  models/realesr-animevideov3-x4.param" \
  > /sums && sha256sum -c /sums \
 && mkdir -p /opt/realesrgan/models && cut -c67- /sums | while read -r f; do install -m 0644 "$f" "/opt/realesrgan/$f"; done \
 && chmod 0755 /opt/realesrgan/realesrgan-ncnn-vulkan
FROM debian:trixie-slim
RUN apt-get update && apt-get install -y --no-install-recommends libvips-tools libvulkan1 mesa-vulkan-drivers libgomp1 && rm -rf /var/lib/apt/lists/*
COPY --from=fetch /opt/realesrgan /opt/realesrgan
RUN ! ldd /opt/realesrgan/realesrgan-ncnn-vulkan | grep 'not found'
ENV PATH=/opt/realesrgan:$PATH
`

// ImagesUpscaleImage names that image, by what it is made from.
var ImagesUpscaleImage = func() string {
	sum := sha256.Sum256([]byte(imagesUpscaleDockerfile))
	return "sisyphus-images-upscale:" + hex.EncodeToString(sum[:6])
}()

// ImagesUpscaleLabel is what a worker whose processor can upscale pictures
// without a graphics card is labelled. lavapipe compiles Real-ESRGAN's
// shaders, which work in 16-bit floating point, to the processor's own
// code, and LLVM cannot do it for one without F16C: Real-ESRGAN stops with
// "LLVM ERROR: Cannot select". F16C came with AVX2, or a little before it,
// so an x86-64 processor with AVX2 has it; a job that upscales on the
// processor is given only to workers that say so.
const ImagesUpscaleLabel = "images-upscale-cpu"

// ImagesUpscaleGPULabel is what a worker that can upscale pictures on a
// graphics card is labelled. Real-ESRGAN's release is built for x86-64
// only, and its image stops building anywhere else, so an arm64 worker
// with a card (a Jetson, say) must not be given the task; the card itself
// is asked for through GPUsNeeded.
const ImagesUpscaleGPULabel = "images-upscale-gpu"

// upscalesOnProcessor says whether this machine's processor can upscale
// pictures through lavapipe.
var upscalesOnProcessor = func() bool { return cpu.X86.HasAVX2 }

// upscalesOnCard says whether Real-ESRGAN's release runs on this machine
// at all, which a job that upscales on a graphics card needs.
var upscalesOnCard = func() bool { return goruntime.GOARCH == "amd64" }

// imagesScript is what each task runs. The pictures are
// /input/image-<i>.<kind>, numbered from 000, of which the task takes the
// contiguous run that is its share, and it is given only those. For each,
// vipsthumbnail turns it upright, fits it to SIZE and saves it to
// /output/image-<i>.<suffix>, with libvips on one thread and its untrusted
// loaders blocked; a picture it cannot make is left with the last lines
// libvips wrote, as /output/image-<i>.log. The task says what each came
// to in summary.json, with the size it reads back from the saved picture.
//
// A job that upscales has each picture made upright and sRGB by libvips
// first, as a PNG without its metadata, since Real-ESRGAN knows nothing of
// EXIF orientation and reads only PNG, JPEG and WebP; then upscaled by
// NATIVE with Real-ESRGAN, and made smaller by SHRINK with libvips where
// the model upscales by more than was asked; and only then fitted to SIZE
// and saved as any other. A picture that would come to more than
// MOST_SIDE pixels a side from Real-ESRGAN is not upscaled, and is listed
// as not made, with why. The task's log, which the script keeps as
// descriptor 3 while each picture's lines go to its own file, says once
// which Vulkan devices Real-ESRGAN found, which shows whether a graphics
// card was used.
const imagesScript = `set -eu
export VIPS_BLOCK_UNTRUSTED=1 VIPS_CONCURRENCY=1 MESA_SHADER_CACHE_DISABLE=true
exec 3>&2
work=/output/.work
mkdir -p "$work"
found=
upscale() {
	flat=$work/$name.flat.png
	vipsthumbnail "$in" --size '10000000x10000000>' -o "$flat[strip]" || return 1
	width=$(vipsheader -f width "$flat") || return 1
	height=$(vipsheader -f height "$flat") || return 1
	if [ $((width * NATIVE)) -gt "$MOST_SIDE" ] || [ $((height * NATIVE)) -gt "$MOST_SIDE" ]; then
		echo "$width by $height pixels is too big to upscale by $NATIVE: a side may come to at most $MOST_SIDE pixels" >&2
		return 1
	fi
	big=$work/$name.big.png
	realesrgan-ncnn-vulkan -i "$flat" -o "$big" -n "$MODEL" -s "$NATIVE" -m /opt/realesrgan/models -f png 2> "$work/$name.esrgan" || status=$?
	cat "$work/$name.esrgan" >&2
	if [ -z "$found" ]; then
		found=$(sed -n 's/^\[[0-9]* \(.*\)\]  queueC=.*/\1/p' "$work/$name.esrgan" | paste -sd ';' -)
		echo "Real-ESRGAN found the Vulkan devices: ${found:-none}" >&3
		found=${found:-none}
	fi
	if [ -n "${status:-}" ] || ! [ -s "$big" ]; then
		echo "Real-ESRGAN made no picture${status:+, and stopped with status $status}" >&2
		unset status
		return 1
	fi
	in=$big
	if [ "$UPSCALE" != "$NATIVE" ]; then
		in=$work/$name.up.png
		vips resize "$big" "$in" "$SHRINK" || return 1
	fi
	echo "picture $j: upscaled by $UPSCALE with $MODEL" >&3
}
edit() {
	j=$1
	name=$(printf 'image-%03d' "$j")
	set -- /input/"$name".*
	in=$1
	kind=${in##*.}
	case "$FORMAT" in
	same) suffix=$kind ;;
	jpeg) suffix=jpg ;;
	tiff) suffix=tif ;;
	*) suffix=$FORMAT ;;
	esac
	case "$suffix" in
	jpg) format=jpeg ;;
	tif) format=tiff ;;
	*) format=$suffix ;;
	esac
	options=
	case "$format" in jpeg | webp) options=Q=$QUALITY ;; esac
	if [ "$KEEP" = false ]; then options=${options:+$options,}strip; fi
	out=/output/$name.$suffix
	: > "$work/$name.err"
	if { [ "$UPSCALE" = 0 ] || upscale 2>> "$work/$name.err"; } && vipsthumbnail "$in" --size "$SIZE" -o "$out${options:+[$options]}" 2>> "$work/$name.err"; then
		width=$(vipsheader -f width "$out")
		height=$(vipsheader -f height "$out")
		entry=$(printf '{"index":%d,"format":"%s","width":%d,"height":%d}' "$j" "$format" "$width" "$height")
		echo "picture $j: $format, $width by $height" >&2
	else
		rm -f "$out"
		tail -n 20 "$work/$name.err" > "/output/$name.log"
		grep -v '^[0-9.]*%$' "$work/$name.err" >&2 || true
		entry=$(printf '{"index":%d,"failed":true}' "$j")
		echo "picture $j could not be made" >&2
	fi
}
i=$SISYPHUS_TASK_INDEX
k=$SISYPHUS_TASK_COUNT
first=$(( PICTURES * i / k ))
last=$(( PICTURES * (i + 1) / k - 1 ))
echo "editing pictures $first to $last of $PICTURES to $SIZE as $FORMAT, upscaled by $UPSCALE" >&2
made=
for j in $(seq "$first" "$last"); do
	edit "$j"
	made=${made:+$made,}$entry
done
rm -rf "$work"
printf '{"pictures":%d,"first":%d,"last":%d,"format":"%s","width":"%s","height":"%s","enlarge":"%s","quality":"%s","keep":"%s","upscale":"%s","model":"%s","gpu":"%s","made":[%s]}\n' \
	"$PICTURES" "$first" "$last" "$FORMAT" "$WIDTH" "$HEIGHT" "$ENLARGE" "$QUALITY" "$KEEP" "$UPSCALE" "$MODEL" "$GPU" "$made" > /output/summary.json
echo "edited pictures $first to $last" >&2
`

func (Images) Name() string { return "images" }

func (Images) Describe() string {
	return `Edits a batch of stored pictures with libvips: each is turned upright by its EXIF orientation, made to fit within a width, a height or both (shrinking only, unless asked to enlarge), saved as JPEG, PNG, WebP, TIFF or GIF or in its own format, and has its metadata, such as where a photo was taken, taken out unless asked to keep it. The pictures are shared out across the pool, a run of them to each task. Each may first be upscaled 2, 3 or 4 times with Real-ESRGAN, on a graphics card or on the processor. Pictures are taken as PNG, JPEG, WebP, TIFF or GIF; of a GIF or TIFF of several frames or pages, the first. Parameters: {"inputs": ["<CIDs of the stored pictures>", ...], "input": "<CID of one, in place of inputs>", "width": <pixels to fit within, 1 to 16384, or 0 (the default) to leave it free>, "height": <the same for the height>, "enlarge": <true to make smaller pictures as big as fits; default false>, "format": "<same (the default), jpeg, png, webp, tiff or gif>", "quality": <for JPEG and WebP, 1 to 100, default 85>, "keep_metadata": <true to keep EXIF and the like; default false, and not with upscale>, "upscale": <2, 3 or 4 to make each picture that many times bigger with Real-ESRGAN first, then fit and save it as above; default 0, none>, "model": "<with upscale, realesrgan-x4plus (the default, for photos), realesrgan-x4plus-anime (for drawings) or realesr-animevideov3 (small and fast, for frames of drawn video)>", "gpu": <with upscale, true to upscale on a worker's graphics card; default false, on its processor>, "tasks": <optional number of runs of pictures>}. Result: {"width", "height", "enlarge", "format", "quality", "keep_metadata", "upscale", "model", "gpu", "pictures": [{"index": <n>, "picture": "<CID of the edited picture>", "name": "<image-000.webp and so on>", "format": "<what it was saved as>", "width": <pixels>, "height": <pixels>, "bytes": <n>, "error": "<why, if it was not made>", "log": "<CID of the lines libvips and Real-ESRGAN wrote that say more, if not>"}], "made": <n>, "failed": <n>, "files": {"<name>": "<CID>"}, "archive": "<CID of a zip of every edited picture>", "tasks": <n>}.`
}

func (Images) Form() []Field {
	return []Field{
		{Name: "inputs", Kind: KindFiles, Label: "Pictures", Help: "Stored PNG, JPEG, WebP, TIFF or GIF pictures to edit. Several are shared out across the pool.", Required: true},
		{Name: "width", Kind: KindInteger, Label: "Width", Help: "The most pixels wide each picture may be, keeping its shape, or 0 to leave the width free.", Default: 0, Minimum: bound(0), Maximum: bound(imagesMostSide)},
		{Name: "height", Kind: KindInteger, Label: "Height", Help: "The most pixels high each picture may be, keeping its shape, or 0 to leave the height free.", Default: 0, Minimum: bound(0), Maximum: bound(imagesMostSide)},
		{Name: "enlarge", Kind: KindToggle, Label: "Enlarge smaller pictures", Help: "Make pictures smaller than the width and height as big as fits. Off, only bigger ones are made smaller.", Default: false},
		{Name: "format", Kind: KindChoice, Label: "Save as", Help: "The format to save each picture as, or same to keep each in its own.", Default: "same", Choices: slices.Clone(imagesFormats)},
		{Name: "quality", Kind: KindInteger, Label: "Quality", Help: "How faithfully to save a JPEG or WebP: higher is truer to the picture and bigger.", Default: 85, Minimum: bound(1), Maximum: bound(100)},
		{Name: "keep_metadata", Kind: KindToggle, Label: "Keep metadata", Help: "Keep each picture's EXIF and other metadata, which can say where and when a photo was taken and with what. Off, it is taken out. Upscaled pictures keep none.", Default: false},
		{Name: "upscale", Kind: KindInteger, Label: "Upscale", Help: "Make each picture 2, 3 or 4 times bigger with Real-ESRGAN, which draws in detail, before it is fitted to the width and height and saved; 0 leaves it as big as it is. Slow without a graphics card.", Default: 0, Minimum: bound(0), Maximum: bound(4)},
		{Name: "model", Kind: KindChoice, Label: "Upscaling model", Help: "With upscale: realesrgan-x4plus for photos, realesrgan-x4plus-anime for drawings and anime, realesr-animevideov3, smaller and faster, for frames of anime video.", Default: imagesModels[0], Choices: slices.Clone(imagesModels)},
		{Name: "gpu", Kind: KindToggle, Label: "Upscale on a graphics card", Help: "With upscale: run Real-ESRGAN on a worker's graphics card, many times faster than on its processor.", Default: false},
		{Name: "tasks", Kind: KindInteger, Label: "Tasks", Help: "How many runs of pictures to share them out as, or 0 to leave it to the pool.", Default: 0, Minimum: bound(0), Maximum: bound(MaxTasks)},
	}
}

// checkPicture says what a stored file is, refusing one that is too big or
// is not a picture of a format taken here.
func checkPicture(ctx context.Context, blobs Blobs, id cid.Cid) (string, error) {
	blob, err := blobs.Open(ctx, id)
	if err != nil {
		return "", err
	}
	defer blob.Close()
	if blob.Size() > imagesMostInput {
		return "", fmt.Errorf("it has %d bytes, and a picture given to images may have at most %d", blob.Size(), imagesMostInput)
	}
	head := make([]byte, 12)
	n, _ := io.ReadFull(blob, head) // a short file has a short head
	kind := pictureKind(head[:n])
	if kind == "" {
		return "", errors.New("it is not a PNG, JPEG, WebP, TIFF or GIF picture")
	}
	return kind, nil
}

func (Images) Split(ctx context.Context, blobs Blobs, params []byte, parts int) ([][]byte, error) {
	var p ImagesParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("images parameters: %w", err)
	}
	inputs := p.Inputs
	switch {
	case p.Input != "" && len(inputs) > 0:
		return nil, errors.New("images parameters: inputs are given in place of input, not with it")
	case p.Input != "":
		inputs = []string{p.Input}
	case len(inputs) == 0:
		return nil, errors.New("images parameters: no picture is given")
	case len(inputs) > imagesMostPictures:
		return nil, fmt.Errorf("images parameters: %d pictures are given; a job can have at most %d", len(inputs), imagesMostPictures)
	}
	settings := imagesSettings{
		Format: cmp.Or(p.Format, "same"), Width: p.Width, Height: p.Height,
		Quality: cmp.Or(p.Quality, 85), Enlarge: p.Enlarge, Keep: p.Keep,
		Upscale: p.Upscale, GPU: p.GPU,
	}
	// A model is checked whether or not it is used, and used only to
	// upscale: a form sends one either way.
	if p.Model != "" && !slices.Contains(imagesModels, p.Model) {
		return nil, fmt.Errorf("images parameters: %w", imagesModelRefused(p.Model))
	}
	if p.Upscale != 0 {
		settings.Model = cmp.Or(p.Model, imagesModels[0])
	}
	if err := settings.check(); err != nil {
		return nil, fmt.Errorf("images parameters: %w", err)
	}
	if p.Tasks < 0 || p.Tasks > MaxTasks {
		return nil, fmt.Errorf("images parameters: tasks is %d, and must be from 1 to %d, or 0 to leave it to the pool", p.Tasks, MaxTasks)
	}
	// Each file's first bytes are read here, which is also what tells the
	// coordinator the job reads it, so that it keeps it while the job runs.
	kinds := make([]string, len(inputs))
	for i, input := range inputs {
		id, err := cid.Decode(input)
		if err != nil {
			return nil, fmt.Errorf("images parameters: input %d is not the CID of a stored picture: %w", i, err)
		}
		if kinds[i], err = checkPicture(ctx, blobs, id); err != nil {
			return nil, fmt.Errorf("images input %d, %s: %w", i, id, err)
		}
	}
	// No more tasks than pictures, and enough that none is given more than
	// a task may have.
	n := len(inputs)
	tasks := max(min(cmp.Or(p.Tasks, parts, 1), n), (n+maxInputs-1)/maxInputs)
	env := settings.env()
	env["PICTURES"] = strconv.Itoa(n)
	image, _, _ := settings.image()
	// The image, the count of tasks and the lack of inputs are made here,
	// and are what Container takes: this cannot fail.
	payloads, _ := Container{}.Split(ctx, blobs, mustJSON(ContainerParams{
		Image: image, Command: []string{"sh", "-c", imagesScript}, Tasks: tasks, Env: env, GPUs: settings.gpus(),
	}), parts)
	// Each task is given only its own run of pictures, as the script
	// reckons its share, by their checked CIDs and names made here from
	// what each is, which the worker checks again.
	for i := range payloads {
		var task containerTask
		json.Unmarshal(payloads[i], &task) // made just now
		task.Inputs = map[string]string{}
		for j := n * i / tasks; j < n*(i+1)/tasks; j++ {
			task.Inputs[imagesName(j)+"."+imagesSuffix[kinds[j]]] = inputs[j]
		}
		payloads[i] = mustJSON(task)
	}
	return payloads, nil
}

// gpus is the graphics cards a task with these settings holds.
func (s imagesSettings) gpus() int {
	if s.GPU {
		return 1
	}
	return 0
}

// GPUsNeeded is one graphics card for each task of a job that asks to
// upscale on one.
func (Images) GPUsNeeded(params []byte) int {
	var p ImagesParams
	if json.Unmarshal(params, &p) != nil || p.Upscale == 0 || !p.GPU {
		return 0
	}
	return 1
}

// Needs says that a job that upscales needs a worker that can: on the
// processor, one whose processor can; on a card, one Real-ESRGAN runs on.
func (Images) Needs(params []byte) []string {
	var p ImagesParams
	if json.Unmarshal(params, &p) != nil || p.Upscale == 0 {
		return nil
	}
	if p.GPU {
		return []string{ImagesUpscaleGPULabel}
	}
	return []string{ImagesUpscaleLabel}
}

// Offers is upscaling on the processor, where this machine's can, and on
// a card, where Real-ESRGAN runs at all.
func (Images) Offers(context.Context) []string {
	var offers []string
	if upscalesOnProcessor() {
		offers = append(offers, ImagesUpscaleLabel)
	}
	if upscalesOnCard() {
		offers = append(offers, ImagesUpscaleGPULabel)
	}
	return offers
}

// imagesName is what picture i's files are called, before their ending.
func imagesName(i int) string { return fmt.Sprintf("image-%03d", i) }

// imagesInput reads the index from the name a picture is given to a task
// by, as one of a job of pictures pictures, or says it is no such name.
func imagesInput(name string, pictures int) (int, bool) {
	base, suffix, _ := strings.Cut(name, ".")
	index, err := strconv.Atoi(strings.TrimPrefix(base, "image-"))
	known := slices.Contains(slices.Collect(maps.Values(imagesSuffix)), suffix)
	return index, err == nil && known && index >= 0 && index < pictures && base == imagesName(index)
}

func (im Images) Execute(ctx context.Context, blobs Blobs, payload []byte) ([]byte, error) {
	var task containerTask
	if err := json.Unmarshal(payload, &task); err != nil {
		return nil, fmt.Errorf("images payload: %w", err)
	}
	// The worker runs only the image of its own making for the settings,
	// and its own script, with settings it checks and a size it makes for
	// itself: whatever command the payload holds is not run. It takes no
	// input but the job's pictures, by the names the script reads them by,
	// and a graphics card only to upscale on.
	settings, err := imagesSettingsOf(task.Env)
	if err != nil {
		return nil, fmt.Errorf("images payload: %w", err)
	}
	image, dockerfile, what := settings.image()
	if task.Image != image {
		return nil, fmt.Errorf("images payload: %q is not the image pictures are edited in", task.Image)
	}
	pictures, err := strconv.Atoi(task.Env["PICTURES"])
	if err != nil || pictures < 1 || pictures > imagesMostPictures {
		return nil, fmt.Errorf("images payload: %q pictures", task.Env["PICTURES"])
	}
	for name := range task.Inputs {
		if _, ok := imagesInput(name, pictures); !ok {
			return nil, fmt.Errorf("images payload: %q is not one of the job's %d pictures", name, pictures)
		}
	}
	task.Command, task.Input, task.Network, task.GPUs = []string{"sh", "-c", imagesScript}, "", false, settings.gpus()
	task.Env = settings.env()
	task.Env["PICTURES"] = strconv.Itoa(pictures)
	engine := im.Engine
	if engine == nil {
		engine = docker
	}
	if err := haveImage(ctx, engine, image, dockerfile, what); err != nil {
		return nil, err
	}
	return Container{Engine: engine, Confined: im.Confined}.Execute(ctx, blobs, mustJSON(task))
}

// Varies says that two honest workers may give different bytes for a
// picture. On one machine libvips saves the same picture the same way
// every time. But each worker builds the image from Alpine's packages as
// they are when it does, so two may have different releases of libvips
// and its encoders, and libvips picks its vector code by the processor it
// finds, which can move a pixel's value by one. Real-ESRGAN reckons in
// 16-bit floating point on whatever device it is given, a graphics card or
// lavapipe on the processor, which round differently. Comparing two runs'
// bytes, as verification does, would take honest workers for wrong ones.
func (Images) Varies() bool { return true }

// imagesSummary is what a task says it edited: pictures First to Last of
// Pictures, with the settings it ran with, and what each came to.
type imagesSummary struct {
	Pictures int            `json:"pictures"`
	First    int            `json:"first"`
	Last     int            `json:"last"`
	Format   string         `json:"format"`
	Width    string         `json:"width"`
	Height   string         `json:"height"`
	Enlarge  string         `json:"enlarge"`
	Quality  string         `json:"quality"`
	Keep     string         `json:"keep"`
	Upscale  string         `json:"upscale"`
	Model    string         `json:"model"`
	GPU      string         `json:"gpu"`
	Made     []imagesReport `json:"made"`
}

// settings are the settings a summary says, by the names of a task's
// environment.
func (s imagesSummary) settings() map[string]string {
	return map[string]string{
		"FORMAT": s.Format, "WIDTH": s.Width, "HEIGHT": s.Height, "ENLARGE": s.Enlarge, "QUALITY": s.Quality, "KEEP": s.Keep,
		"UPSCALE": s.Upscale, "MODEL": s.Model, "GPU": s.GPU,
	}
}

// imagesReport is what a task says one picture came to.
type imagesReport struct {
	Index  int    `json:"index"`
	Failed bool   `json:"failed"`
	Format string `json:"format"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
}

// imagesMostEdited is the most pixels a side of an edited picture may
// have where the box leaves it free: the most any of the formats saves.
const imagesMostEdited = 65535

// Aggregate takes from each task only its summary and the files it is meant
// to leave, by the fixed names of the pictures it says it edited, and
// checks that the tasks edited every picture once, in order, with the same
// settings, and that each picture is what was asked for, before anything
// they said is believed.
func (Images) Aggregate(ctx context.Context, blobs Blobs, outputs [][]byte) ([]byte, error) {
	result := ImagesResult{Tasks: len(outputs), Pictures: []ImagesPicture{}, Files: map[string]string{}}
	var first imagesSummary
	var settings imagesSettings
	read := 0
	for i, raw := range outputs {
		var out ContainerOutput
		if err := json.Unmarshal(raw, &out); err != nil {
			return nil, fmt.Errorf("task %d output: %w", i, err)
		}
		var summary imagesSummary
		if err := decodeImagesSummary(ctx, blobs, out.Files["summary.json"], &summary); err != nil {
			return nil, fmt.Errorf("task %d's summary: %w", i, err)
		}
		said, err := imagesSettingsOf(summary.settings())
		if i == 0 {
			first, settings = summary, said
			if err != nil || summary.Pictures < 1 || summary.Pictures > imagesMostPictures {
				return nil, fmt.Errorf("task %d says it edited %d pictures, with settings no job asks for: %v", i, summary.Pictures, err)
			}
			result.Width, result.Height, result.Enlarge = settings.Width, settings.Height, settings.Enlarge
			result.Format, result.Quality, result.Keep = settings.Format, settings.Quality, settings.Keep
			result.Upscale, result.Model, result.GPU = settings.Upscale, settings.Model, settings.GPU
		}
		if summary.Pictures != first.Pictures || err != nil || said != settings {
			return nil, fmt.Errorf("task %d edited %d pictures as %s to %sx%s (enlarge %s, quality %s, keep %s, upscale %s, model %q, gpu %s), and task 0 %d as %s to %sx%s (enlarge %s, quality %s, keep %s, upscale %s, model %q, gpu %s)", i,
				summary.Pictures, summary.Format, summary.Width, summary.Height, summary.Enlarge, summary.Quality, summary.Keep, summary.Upscale, summary.Model, summary.GPU,
				first.Pictures, first.Format, first.Width, first.Height, first.Enlarge, first.Quality, first.Keep, first.Upscale, first.Model, first.GPU)
		}
		if summary.First != read || summary.Last < summary.First-1 || summary.Last >= summary.Pictures || len(summary.Made) != summary.Last-summary.First+1 {
			return nil, fmt.Errorf("task %d says it edited pictures %d to %d of %d, after pictures up to %d, and reports %d", i, summary.First, summary.Last, summary.Pictures, read-1, len(summary.Made))
		}
		for _, report := range summary.Made {
			if report.Index != read {
				return nil, fmt.Errorf("task %d reports picture %d where picture %d was to be", i, report.Index, read)
			}
			picture, err := checkImagesReport(ctx, blobs, settings, report, out.Files)
			if err != nil {
				return nil, fmt.Errorf("task %d's picture %d: %w", i, read, err)
			}
			if picture.Error == "" {
				result.Made++
				result.Files[picture.Name] = picture.Picture
			} else {
				result.Failed++
			}
			result.Pictures = append(result.Pictures, picture)
			read++
		}
	}
	if read != first.Pictures {
		return nil, fmt.Errorf("the tasks edited %d of the %d pictures", read, first.Pictures)
	}
	if result.Made == 0 {
		return nil, fmt.Errorf("no picture was made: picture 0: %s", result.Pictures[0].Error)
	}
	// Each is named by its index and its format: names the coordinator
	// makes from what it has checked, and none a worker gave.
	var names, ids []string
	for _, picture := range result.Pictures {
		if picture.Error == "" {
			names, ids = append(names, picture.Name), append(ids, picture.Picture)
		}
	}
	reading, writing := io.Pipe()
	go func() { writing.CloseWithError(zipFrames(ctx, blobs, writing, names, ids)) }()
	archive, err := blobs.Put(ctx, reading)
	reading.CloseWithError(errors.New("the archive is no longer being stored"))
	if err != nil {
		return nil, fmt.Errorf("store the pictures as one zip file: %w", err)
	}
	result.Archive = archive.String()
	return mustJSON(result), nil
}

// checkImagesReport checks what a task says of one picture, and the file it
// left for it, and makes what the result says of it.
func checkImagesReport(ctx context.Context, blobs Blobs, settings imagesSettings, report imagesReport, files map[string]string) (ImagesPicture, error) {
	name := imagesName(report.Index)
	picture := ImagesPicture{Index: report.Index}
	if report.Failed {
		var err error
		if picture.Log, _, err = prepFile(ctx, blobs, files, name+".log", imagesMostLog); err != nil {
			return ImagesPicture{}, err
		}
		picture.Error = "libvips could not read or save the picture; its log says why"
		if settings.Upscale > 0 {
			picture.Error = "the picture could not be upscaled, read or saved; its log says why"
		}
		return picture, nil
	}
	suffix, known := imagesSuffix[report.Format]
	widest, highest := cmp.Or(settings.Width, imagesMostEdited), cmp.Or(settings.Height, imagesMostEdited)
	switch {
	case !known || settings.Format != "same" && report.Format != settings.Format:
		return ImagesPicture{}, fmt.Errorf("it says it was saved as %q, and the job asks for %s", report.Format, settings.Format)
	case report.Width < 1 || report.Width > widest || report.Height < 1 || report.Height > highest:
		return ImagesPicture{}, fmt.Errorf("it says it is %d by %d pixels, which does not fit within %d by %d", report.Width, report.Height, widest, highest)
	}
	picture.Name, picture.Format, picture.Width, picture.Height = name+"."+suffix, report.Format, report.Width, report.Height
	id, err := cid.Decode(files[picture.Name])
	if err != nil {
		return ImagesPicture{}, fmt.Errorf("the task left no %s", picture.Name)
	}
	blob, err := blobs.Open(ctx, id)
	if err != nil {
		return ImagesPicture{}, fmt.Errorf("%s: %w", picture.Name, err)
	}
	defer blob.Close()
	if blob.Size() == 0 || blob.Size() > imagesMostFile {
		return ImagesPicture{}, fmt.Errorf("%s has %d bytes, and must have from 1 to %d", picture.Name, blob.Size(), uint64(imagesMostFile))
	}
	head := make([]byte, 12)
	n, _ := io.ReadFull(blob, head) // a short file has a short head
	if pictureKind(head[:n]) != report.Format {
		return ImagesPicture{}, fmt.Errorf("%s is not a %s picture", picture.Name, report.Format)
	}
	picture.Picture, picture.Bytes = id.String(), blob.Size()
	return picture, nil
}

// decodeImagesSummary reads a task's summary.
func decodeImagesSummary(ctx context.Context, blobs Blobs, id string, summary *imagesSummary) error {
	c, err := cid.Decode(id)
	if err != nil {
		return errors.New("the task left none")
	}
	blob, err := blobs.Open(ctx, c)
	if err != nil {
		return err
	}
	defer blob.Close()
	return json.NewDecoder(io.LimitReader(blob, 1<<20)).Decode(summary)
}

// ResultBlobs are the pictures and logs the result refers to, which the
// tasks stored. The archive is stored by Aggregate, and kept with the
// result as everything it stores is.
func (Images) ResultBlobs(raw []byte) []string {
	var result ImagesResult
	json.Unmarshal(raw, &result)
	var ids []string
	for _, picture := range result.Pictures {
		for _, id := range []string{picture.Picture, picture.Log} {
			if id != "" {
				ids = append(ids, id)
			}
		}
	}
	return ids
}
