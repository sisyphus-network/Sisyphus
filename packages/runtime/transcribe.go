package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"regexp"
	"strings"

	"github.com/ipfs/go-cid"
)

// Transcribe turns the speech in a stored recording, sound or video, into
// text with whisper.cpp. The recording is cut into as many stretches as
// the job has tasks, each cut in a quiet moment near where an even share
// would end; each task transcribes one, in a container, and the stretches'
// words are put together in order as a plain transcript and as subtitles.
//
// Like Transcode, it is the container workload with the image and the
// command fixed, so it runs only on workers that run containers. There is
// an image for each model, built on a worker the first time it is needed.
type Transcribe struct {
	// Engine runs the container engine, as Container's does.
	Engine func(ctx context.Context, args []string, stdout, stderr io.Writer) (status int, err error)
	// Confined holds its containers in as Container's does.
	Confined Confinement
}

// TranscribeParams are a transcribe job's parameters.
type TranscribeParams struct {
	// Input is the CID of the stored recording, sound or video.
	Input string `json:"input"`
	// Language is the spoken language, as a code such as en or fr. Empty
	// or auto has whisper tell, for each stretch.
	Language string `json:"language,omitempty"`
	// Model is the whisper model to use, one of transcribeModelNames.
	// Empty means transcribeDefault.
	Model string `json:"model,omitempty"`
	// Segments is how many stretches to cut the recording into. Zero
	// leaves it to the pool.
	Segments int `json:"segments,omitempty"`
}

// TranscribeResult is a transcribe job's result.
type TranscribeResult struct {
	// Text is the CID of the transcript, a line of plain text for each
	// thing said; Subtitles is the CID of the same as an SRT file.
	Text      string `json:"text"`
	Subtitles string `json:"subtitles"`
	// Segments is how many stretches the recording was cut into.
	Segments int `json:"segments"`
	// Seconds is how long the recording is, and Words how many words were
	// heard in it.
	Seconds float64 `json:"seconds"`
	Words   int     `json:"words"`
}

// TranscribeSegment is one thing said, with when it was said in seconds
// from the start of the recording.
type TranscribeSegment struct {
	Start float64 `json:"start"`
	End   float64 `json:"end"`
	Text  string  `json:"text"`
}

// transcribeStretch is what a task leaves in segments.json: the stretch it
// transcribed, in seconds, and what was said in it.
type transcribeStretch struct {
	Start    float64             `json:"start"`
	End      float64             `json:"end"`
	Duration float64             `json:"duration"`
	Language string              `json:"language"`
	Segments []TranscribeSegment `json:"segments"`
}

// whisperRelease is the release of whisper.cpp the images are built from,
// and the commit it must be.
const whisperRelease, whisperCommit = "v1.9.5", "d1be6fde11ac6e0407606b4e42fe72d34add8037"

// whisperModels is the revision of the whisper.cpp model repository the
// models are fetched from.
const whisperModels = "5359861c739e955e79d9a303bcbc70fb988958b1"

// transcribeModels are the models a job may choose, by name, with the
// SHA-256 of each one's file as the model repository publishes it. The
// names ending in .en understand only English; the q5_0 ones are
// quantised, a third the size of the model they are made from.
var transcribeModels = map[string]string{
	"tiny":                "be07e048e1e599ad46341c8d2a135645097a538221678b7acdd1b1919c6e1b21",
	"tiny.en":             "921e4cf8686fdd993dcd081a5da5b6c365bfde1162e72b08d75ac75289920b1f",
	"base":                "60ed5bc3dd14eea856493d334349b405782ddcaf0028d4b5df4088345fba2efe",
	"base.en":             "a03779c86df3323075f5e796cb2ce5029f00ec8869eee3fdfb897afe36c6d002",
	"small":               "1be3a9b2063867b937e64e2ec7483364a79917e157fa98c5d94b5c1fffea987b",
	"small.en":            "c6138d6d58ecc8322097e0f987c32f1be8bb0a18532a3f88f734d1bbf9c41e5d",
	"medium":              "6c14d5adee5f86394037b4e4e8b59f1673b6cee10e3cf0b11bbdbee79c156208",
	"medium.en":           "cc37e93478338ec7700281a7ac30a10128929eb8f427dda2e865faa8f6da4356",
	"large-v1":            "7d99f41a10525d0206bddadd86760181fa920438b6b33237e3118ff6c83bb53d",
	"large-v2":            "9a423fe4d40c82774b6af34115b8b935f34152246eb19e80e376071d3f999487",
	"large-v3":            "64d182b440b98d5203c4f9bd541544d84c605196c4f7b845dfa11fb23594d1e2",
	"large-v3-q5_0":       "d75795ecff3f83b5faa89d1900604ad8c780abd5739fae406de19f23ecd98ad1",
	"large-v3-turbo":      "1fc70f774d38eb169993ac391eea357ef47c88757ef72ee5943879b7e8e2bc69",
	"large-v3-turbo-q5_0": "394221709cd5ad1f40c46e6031ca61bce88931e6e088c188294c6d5a55ffa7e2",
}

// transcribeModelNames are those models, smallest first, each before its
// English-only form.
var transcribeModelNames = []string{"tiny", "tiny.en", "base", "base.en", "small", "small.en", "large-v3-turbo-q5_0", "large-v3-q5_0", "medium", "medium.en", "large-v3-turbo", "large-v1", "large-v2", "large-v3"}

// transcribeDefault is the model a job that names none gets: the large
// model's turbo form, quantised, which was quicker than small on a
// processor and as accurate as the largest.
const transcribeDefault = "large-v3-turbo-q5_0"

// englishOnly says whether a model understands only English.
func englishOnly(model string) bool { return strings.HasSuffix(model, ".en") }

// transcribeDockerfile is the whole of the image transcribing with model
// runs in. whisper.cpp is built from source, for the processor of the
// worker that builds it, at the lowest priority so that the worker's other
// tasks go on meanwhile; the model is fetched and checked against its
// digest; and the rest is the transcoding image, so that a worker that has
// both keeps ffmpeg once. Only the model's lines differ from one model to
// another, and they come last in each stage, so the images of every model
// share the compiled whisper.cpp and ffmpeg, and differ by the model alone.
func transcribeDockerfile(model string) string {
	return `FROM alpine:3.20 AS build
RUN apk add --no-cache build-base cmake git
RUN git clone --quiet --depth 1 --branch ` + whisperRelease + ` https://github.com/ggml-org/whisper.cpp /src && test "$(git -C /src rev-parse HEAD)" = ` + whisperCommit + `
RUN cmake -S /src -B /build -DCMAKE_BUILD_TYPE=Release -DBUILD_SHARED_LIBS=OFF -DWHISPER_BUILD_TESTS=OFF -DWHISPER_BUILD_SERVER=OFF && nice -n 19 cmake --build /build --target whisper-cli -j "$(nproc)"
FROM alpine:3.20 AS model
RUN apk add --no-cache curl && curl -fsSL -o /model.bin https://huggingface.co/ggerganov/whisper.cpp/resolve/` + whisperModels + `/ggml-` + model + `.bin && echo "` + transcribeModels[model] + `  /model.bin" | sha256sum -c -
` + transcodeDockerfile + `RUN apk add --no-cache jq libstdc++ libgomp
COPY --from=build /build/bin/whisper-cli /usr/local/bin/whisper-cli
COPY --from=model /model.bin /models/ggml-` + model + `.bin
`
}

// TranscribeImages name those images, by model. Each name carries what the
// image is made from, as TranscodeImage's does.
var TranscribeImages = func() map[string]string {
	images := map[string]string{}
	for model := range transcribeModels {
		sum := sha256.Sum256([]byte(transcribeDockerfile(model)))
		images[model] = "sisyphus-transcribe-" + model + ":" + hex.EncodeToString(sum[:6])
	}
	return images
}()

// transcribeSegment is what each task runs. It finds how long the
// recording is and where its stretch begins and ends: where an even share
// would, moved to the middle of the nearest quiet moment within a quarter
// of a share or five seconds, so that a cut falls between words where it
// can. The task before and the task after find the same cut, from the same
// recording. It takes its stretch as 16 kHz mono sound, has whisper
// transcribe it, and leaves what was said, timed from the recording's
// start, in segments.json.
const transcribeSegment = `set -eu
duration=$(ffprobe -v error -show_entries format=duration -of csv=p=0 /input/data)
case "$duration" in ''|N/A) echo "the input has no duration: it is not sound or video" >&2; exit 1 ;; esac
n=$SISYPHUS_TASK_COUNT
reach=$(awk -v d="$duration" -v n="$n" 'BEGIN { r = d / n / 4; if (r > 5) r = 5; printf "%.3f", r }')
cut() {
	if [ "$1" = 0 ]; then echo 0; return; fi
	if [ "$1" = "$n" ]; then echo "$duration"; return; fi
	even=$(awk -v d="$duration" -v k="$1" -v n="$n" 'BEGIN { printf "%.3f", d * k / n }')
	from=$(awk -v b="$even" -v r="$reach" 'BEGIN { printf "%.3f", b - r }')
	ffmpeg -nostdin -hide_banner -nostats -ss "$from" -t "$(awk -v r="$reach" 'BEGIN { print 2 * r }')" -i /input/data -vn -af silencedetect=noise=-35dB:duration=0.3 -f null - 2>&1 |
		awk -v b="$even" -v from="$from" '
			/silence_start:/ { s = $NF }
			/silence_end:/ { for (f = 1; f < NF; f++) if ($f == "silence_end:") e = $(f + 1)
				m = from + (s + e) / 2; if (!found || (m - b) * (m - b) < (best - b) * (best - b)) { best = m; found = 1 }; s = 0 }
			END { printf "%.3f\n", found ? best : b }'
}
start=$(cut "$SISYPHUS_TASK_INDEX")
end=$(cut $((SISYPHUS_TASK_INDEX + 1)))
length=$(awk -v s="$start" -v e="$end" 'BEGIN { printf "%.3f", e - s }')
echo "transcribing $length seconds from $start of $duration with the $MODEL model" >&2
keep() { jq "$@" --argjson start "$start" --argjson end "$end" --argjson duration "$duration" > /output/segments.json; }
if awk -v l="$length" 'BEGIN { exit !(l < 0.1) }'; then
	keep -n '{start: $start, end: $end, duration: $duration, language: "", segments: []}'
	exit 0
fi
ffmpeg -nostdin -loglevel error -ss "$start" -t "$length" -i /input/data -vn -ac 1 -ar 16000 -c:a pcm_s16le /output/.stretch.wav
threads=$(nproc)
if [ "$threads" -gt 8 ]; then threads=8; fi
whisper-cli --model "/models/ggml-$MODEL.bin" --language "$LANGUAGE" --threads "$threads" --no-prints --output-json --output-file /output/.whisper --file /output/.stretch.wav
keep '{start: $start, end: $end, duration: $duration, language: (.result.language // ""),
	segments: [.transcription[] | {start: ($start + .offsets.from / 1000), end: ($start + .offsets.to / 1000), text: .text}]}' /output/.whisper.json
rm -f /output/.stretch.wav /output/.whisper.json
echo "heard $(jq '.segments | length' /output/segments.json) segments" >&2
`

// transcribeLanguage is what a language may be: a code such as en, or
// haw for Hawaiian.
var transcribeLanguage = regexp.MustCompile(`^[a-z]{2,3}$`)

func (Transcribe) Name() string { return "transcribe" }

func (Transcribe) Describe() string {
	return `Turns the speech in a stored recording, sound or video, into text with whisper.cpp, cut into stretches that are transcribed on different workers and put together. Parameters: {"input": "<CID of the stored recording>", "language": "<optional spoken language as a code such as en, or auto to have whisper tell, the default>", "model": "<optional, one of tiny, base, small, medium, large-v1, large-v2, large-v3, large-v3-turbo, large-v3-q5_0 and large-v3-turbo-q5_0 (the default), larger being slower and more accurate; tiny, base, small and medium also as English-only tiny.en and so on>", "segments": <optional number of stretches>}. Result: {"text": "<CID of the transcript, plain text>", "subtitles": "<CID of the transcript as SRT subtitles>", "segments": <n>, "seconds": <how long the recording is>, "words": <words heard>}.`
}

func (Transcribe) Form() []Field {
	return []Field{
		{Name: "input", Kind: KindFile, Label: "Recording", Help: "The stored sound or video to transcribe.", Required: true},
		{Name: "language", Kind: KindText, Label: "Language", Help: "The spoken language as a code such as en or fr, or auto to have it told.", Default: "auto"},
		{Name: "model", Kind: KindChoice, Label: "Model", Help: "Larger models are slower and more accurate; those ending in .en understand only English.", Default: transcribeDefault, Choices: append([]string(nil), transcribeModelNames...)},
		{Name: "segments", Kind: KindInteger, Label: "Stretches", Help: "How many stretches to cut the recording into, or 0 to leave it to the pool.", Default: 0, Minimum: bound(0), Maximum: bound(MaxTasks)},
	}
}

func (Transcribe) Split(ctx context.Context, blobs Blobs, params []byte, parts int) ([][]byte, error) {
	var p TranscribeParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("transcribe parameters: %w", err)
	}
	if _, err := cid.Decode(p.Input); err != nil {
		return nil, fmt.Errorf("transcribe parameters: input is not the CID of a stored recording: %w", err)
	}
	if p.Language == "" {
		p.Language = "auto"
	}
	if p.Model == "" {
		p.Model = transcribeDefault
	}
	if englishOnly(p.Model) && p.Language == "auto" {
		p.Language = "en"
	}
	switch {
	case p.Language != "auto" && !transcribeLanguage.MatchString(p.Language):
		return nil, fmt.Errorf("transcribe parameters: language is %q, and must be a code such as en, or auto", p.Language)
	case TranscribeImages[p.Model] == "":
		return nil, fmt.Errorf("transcribe parameters: model is %q, and must be one of %s", p.Model, strings.Join(transcribeModelNames, ", "))
	case englishOnly(p.Model) && p.Language != "en":
		return nil, fmt.Errorf("transcribe parameters: the model %s understands only English, and language is %q; choose en or auto, or a model without .en", p.Model, p.Language)
	case p.Segments < 0:
		return nil, fmt.Errorf("transcribe parameters: segments is %d, and must be 0 or more", p.Segments)
	}
	return Container{}.Split(ctx, blobs, mustJSON(ContainerParams{
		Image: TranscribeImages[p.Model], Command: []string{"sh", "-c", transcribeSegment},
		Input: p.Input, Tasks: p.Segments,
		Env: map[string]string{"LANGUAGE": p.Language, "MODEL": p.Model},
	}), parts)
}

func (t Transcribe) Execute(ctx context.Context, blobs Blobs, payload []byte) ([]byte, error) {
	var task containerTask
	if err := json.Unmarshal(payload, &task); err != nil {
		return nil, fmt.Errorf("transcribe payload: %w", err)
	}
	// The worker builds only an image of its own making: the one for the
	// model the task names.
	model := ""
	for m, image := range TranscribeImages {
		if image == task.Image {
			model = m
		}
	}
	if model == "" {
		return nil, fmt.Errorf("transcribe payload: %q is not an image transcription runs in", task.Image)
	}
	engine := t.Engine
	if engine == nil {
		engine = docker
	}
	if err := haveImage(ctx, engine, task.Image, transcribeDockerfile(model), "transcription with the "+model+" model"); err != nil {
		return nil, err
	}
	return Container{Engine: engine, Confined: t.Confined}.Execute(ctx, blobs, payload)
}

// Varies says that transcripts can differ between runs: whisper's sums
// come out a little differently on different processors and thread counts.
func (Transcribe) Varies() bool { return true }

func (Transcribe) Aggregate(ctx context.Context, blobs Blobs, outputs [][]byte) ([]byte, error) {
	var said []TranscribeSegment
	var result TranscribeResult
	for i, output := range outputs {
		var out ContainerOutput
		if err := json.Unmarshal(output, &out); err != nil {
			return nil, fmt.Errorf("task %d output: %w", i, err)
		}
		c, err := cid.Decode(out.Files["segments.json"])
		if err != nil {
			return nil, fmt.Errorf("task %d left no transcript", i)
		}
		blob, err := blobs.Open(ctx, c)
		if err != nil {
			return nil, fmt.Errorf("task %d's transcript: %w", i, err)
		}
		var stretch transcribeStretch
		err = json.NewDecoder(io.LimitReader(blob, 64<<20)).Decode(&stretch)
		blob.Close()
		if err != nil {
			return nil, fmt.Errorf("task %d's transcript: %w", i, err)
		}
		result.Seconds = math.Max(result.Seconds, stretch.Duration)
		for _, s := range stretch.Segments {
			s.Text = strings.TrimSpace(s.Text)
			// whisper marks what is not speech in brackets, such as
			// [BLANK_AUDIO] or [MUSIC], which is no part of a transcript.
			if s.Text == "" || strings.HasPrefix(s.Text, "[") && strings.HasSuffix(s.Text, "]") {
				continue
			}
			// It can time the last words past the stretch's end, into
			// the next one's.
			s.Start = min(max(s.Start, stretch.Start), stretch.End)
			s.End = min(max(s.End, s.Start), stretch.End)
			said = append(said, s)
		}
	}
	var text, subtitles strings.Builder
	for i, s := range said {
		result.Words += len(strings.Fields(s.Text))
		fmt.Fprintf(&text, "%s\n", s.Text)
		fmt.Fprintf(&subtitles, "%d\n%s --> %s\n%s\n\n", i+1, srtTime(s.Start), srtTime(s.End), s.Text)
	}
	stored, err := blobs.Put(ctx, strings.NewReader(text.String()))
	if err != nil {
		return nil, fmt.Errorf("store the transcript: %w", err)
	}
	result.Text = stored.String()
	if stored, err = blobs.Put(ctx, strings.NewReader(subtitles.String())); err != nil {
		return nil, fmt.Errorf("store the subtitles: %w", err)
	}
	result.Subtitles = stored.String()
	result.Segments = len(outputs)
	return mustJSON(result), nil
}

// srtTime writes a time in seconds as SRT has it: hours, minutes, seconds
// and milliseconds.
func srtTime(seconds float64) string {
	ms := int64(math.Round(seconds * 1000))
	return fmt.Sprintf("%02d:%02d:%02d,%03d", ms/3600000, ms/60000%60, ms/1000%60, ms%1000)
}
