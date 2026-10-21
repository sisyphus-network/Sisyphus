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
	"slices"
	"strconv"
	"strings"
	"unicode"

	"github.com/ipfs/go-cid"
)

// OCR reads the text in a stored scan with Tesseract: a PDF, whose pages
// are shared out across the pool, each task reading a run of them, or a
// single picture, which one task reads. The pages' text is put together in
// order as one text file, and, if asked, each task makes its pages into a
// searchable PDF too: the scan as it was, with the text it read laid
// invisibly over it, to be searched and copied from.
//
// Like Transcode, it is the container workload with the image and the
// command fixed, so it runs only on workers that run containers. The image
// is built on a worker the first time it is needed, from Alpine's own
// Tesseract and Poppler and the languages below.
type OCR struct {
	// Engine runs the container engine, as Container's does.
	Engine func(ctx context.Context, args []string, stdout, stderr io.Writer) (status int, err error)
	// Confined holds its containers in as Container's does.
	Confined Confinement
}

// OCRParams are an ocr job's parameters.
type OCRParams struct {
	// Input is the CID of the stored scan: a PDF, or a PNG, JPEG or TIFF
	// picture. A TIFF may have several pages.
	Input string `json:"input"`
	// Language is the language of the text, as one of ocrLanguages, or
	// several joined with +, as Tesseract takes them. Empty means eng.
	Language string `json:"language,omitempty"`
	// DPI is how many dots per inch to turn a PDF's pages into pictures
	// at, from ocrLeastDPI to ocrMostDPI. Zero means 300. A picture is
	// read at its own.
	DPI int `json:"dpi,omitempty"`
	// Output is text, pdf or text+pdf: what to make of what was read.
	// Empty means text.
	Output string `json:"output,omitempty"`
	// Tasks is how many runs of pages to share a PDF out as. Zero leaves
	// it to the pool. A picture is one task.
	Tasks int `json:"tasks,omitempty"`
}

// OCRResult is an ocr job's result.
type OCRResult struct {
	// Pages is how many pages were read.
	Pages int `json:"pages"`
	// Text is the CID of what was read, as plain text, the pages in order
	// with a form feed between each and the next, when text was asked for.
	Text string `json:"text"`
	// PDF is the CID of the searchable PDF, when it was asked for and one
	// task read every page. Where several did, each made its own pages
	// into one, and PDFs are those, in order, and Archive a zip file of
	// them, named by their pages: the pool's coordinator runs no
	// containers, so it puts no PDFs together.
	PDF     string   `json:"pdf"`
	PDFs    []string `json:"pdfs,omitempty"`
	Archive string   `json:"archive,omitempty"`
	// Characters is how many characters were read, not counting spaces
	// and line breaks.
	Characters int `json:"characters"`
	// Language is what the text was read as.
	Language string `json:"language"`
	// Tasks is how many tasks the job was shared out as.
	Tasks int `json:"tasks"`
}

// ocrLanguages are the languages the image has Tesseract's data for: those
// of the desktop app, English, Hebrew, Arabic, Russian, Polish, Spanish and
// French, and German.
var ocrLanguages = []string{"eng", "spa", "fra", "deu", "heb", "ara", "rus", "pol"}

// ocrOutputs are what a job may ask to have made.
var ocrOutputs = []string{"text", "pdf", "text+pdf"}

// Bounds of an ocr job: the fewest and most dots per inch a PDF's pages are
// turned into pictures at, and the most text a job may have, which the
// coordinator holds in memory as it puts the pages together: some 20,000
// pages of dense print.
const (
	ocrLeastDPI = 72
	ocrMostDPI  = 600
	ocrMostText = 64 << 20
)

// ocrDockerfile is the whole of the image reading text runs in: Alpine's
// Tesseract, the data for each language, and Poppler's utilities, which
// count a PDF's pages, turn them into pictures and put PDFs together.
//
// Alpine builds Tesseract with OpenCL, which, wherever it finds no record
// of having done so, times the machine's devices before reading anything,
// and keeps the record in the directory it runs in. The image keeps one,
// made as it is built, in the directory a task runs in, so that no task
// spends time on it or needs to write it.
var ocrDockerfile = func() string {
	data := make([]string, len(ocrLanguages))
	for i, language := range ocrLanguages {
		data[i] = "tesseract-ocr-data-" + language
	}
	return `FROM alpine:3.20
RUN apk add --no-cache poppler-utils tesseract-ocr ` + strings.Join(data, " ") + `
WORKDIR /opt/ocr
RUN tesseract /dev/null - >/dev/null 2>&1; test -s tesseract_opencl_profile_devices.dat
`
}()

// OCRImage names that image. The name carries what the image is made from,
// as TranscodeImage's does.
var OCRImage = func() string {
	sum := sha256.Sum256([]byte(ocrDockerfile))
	return "sisyphus-ocr:" + hex.EncodeToString(sum[:6])
}()

// ocrScript is what each task runs. A picture is read whole, all of its
// pages. A PDF's pages are counted, and the task takes the run of them
// that is its share; it turns each into a picture at the dots per inch
// asked for and reads it, one page at a time, with one thread, since the
// pool runs as many tasks on a worker as it has processors. It leaves the
// text of its pages in text.txt, with a form feed between each and the
// next, its searchable PDF in text.pdf if one was asked for, and which
// pages it read in summary.json.
const ocrScript = `set -eu -o pipefail
export OMP_THREAD_LIMIT=1
work=/output/.work
mkdir -p "$work"
formats=txt
if [ "$OUTPUT" != text ]; then formats="txt pdf"; fi
ocr() {
	tesseract "$@" -l "$LANGUAGE" $formats 2>&1 | { grep -v '^\[DS\]' >&2 || true; }
}
summary() {
	printf '{"first":%d,"last":%d,"pages":%d,"output":"%s","language":"%s"}\n' "$1" "$2" "$3" "$OUTPUT" "$LANGUAGE" > /output/summary.json
}
if [ "$KIND" = image ]; then
	echo "reading a picture in $LANGUAGE" >&2
	ocr /input/data "$work/all"
	mv "$work/all.txt" /output/text.txt
	if [ "$OUTPUT" != text ]; then mv "$work/all.pdf" /output/text.pdf; fi
	pages=$(( $(tr -cd '\f' < /output/text.txt | wc -c) + 1 ))
	summary 1 "$pages" "$pages"
	echo "read $pages pages" >&2
	exit 0
fi
info=$(pdfinfo /input/data) || { echo "the input is not a PDF that can be read" >&2; exit 1; }
pages=$(echo "$info" | awk '/^Pages:/ { print $2 }')
first=$(( pages * SISYPHUS_TASK_INDEX / SISYPHUS_TASK_COUNT + 1 ))
last=$(( pages * (SISYPHUS_TASK_INDEX + 1) / SISYPHUS_TASK_COUNT ))
summary "$first" "$last" "$pages"
if [ "$first" -gt "$last" ]; then
	echo "no pages are left to this task: the PDF has $pages among $SISYPHUS_TASK_COUNT tasks" >&2
	exit 0
fi
echo "reading pages $first to $last of $pages at $DPI dots per inch in $LANGUAGE" >&2
if [ "$OUTPUT" = text ]; then set -- -gray -png; picture=png; else set -- -jpeg -jpegopt quality=90; picture=jpg; fi
: > /output/text.txt
page=$first
while [ "$page" -le "$last" ]; do
	pdftoppm -r "$DPI" -f "$page" -l "$page" -singlefile "$@" /input/data "$work/page"
	ocr "$work/page.$picture" "$work/$page" --dpi "$DPI"
	rm "$work/page.$picture"
	if [ "$page" -gt "$first" ]; then printf '\f' >> /output/text.txt; fi
	tr -d '\f' < "$work/$page.txt" >> /output/text.txt
	page=$((page + 1))
done
if [ "$OUTPUT" != text ]; then
	set --
	page=$first
	while [ "$page" -le "$last" ]; do set -- "$@" "$work/$page.pdf"; page=$((page + 1)); done
	if [ $# = 1 ]; then mv "$1" /output/text.pdf; else pdfunite "$@" /output/text.pdf; fi
fi
echo "read $((last - first + 1)) pages" >&2
`

func (OCR) Name() string { return "ocr" }

func (OCR) Describe() string {
	return `Reads the text in a stored scan with Tesseract: a PDF, whose pages are shared out across the pool, each task reading a run of them, or a single PNG, JPEG or TIFF picture, which one task reads. Parameters: {"input": "<CID of the stored PDF or picture>", "language": "<eng (the default), spa, fra, deu, heb, ara, rus or pol, or several joined with +, such as eng+heb>", "dpi": <dots per inch to turn a PDF's pages into pictures at, from 72 to 600, default 300>, "output": "<text (the default), pdf or text+pdf: plain text, a searchable PDF, or both>", "tasks": <optional number of runs of pages>}. Result: {"pages": <n>, "text": "<CID of the text, the pages in order with a form feed between them, if asked for>", "pdf": "<CID of the searchable PDF, if asked for and one task read every page>", "pdfs": ["<CID of each task's searchable PDF of its pages, in order, where several did>", ...], "archive": "<CID of a zip of those>", "characters": <characters read, not counting spaces>, "language": "<language>", "tasks": <n>}.`
}

func (OCR) Form() []Field {
	return []Field{
		{Name: "input", Kind: KindFile, Label: "Scan", Help: "The stored PDF, or PNG, JPEG or TIFF picture, to read the text in.", Required: true},
		{Name: "language", Kind: KindText, Label: "Language", Help: "The text's language: eng, spa, fra, deu, heb, ara, rus or pol, or several joined with +, such as eng+heb.", Default: "eng"},
		{Name: "output", Kind: KindChoice, Label: "Make", Help: "Plain text, a searchable PDF of the scan with the text laid over it, or both.", Default: "text", Choices: slices.Clone(ocrOutputs)},
		{Name: "dpi", Kind: KindInteger, Label: "Dots per inch", Help: "How finely to turn a PDF's pages into pictures to read: 300 suits most print, more suits small print, and is slower.", Default: 300, Minimum: bound(ocrLeastDPI), Maximum: bound(ocrMostDPI)},
		{Name: "tasks", Kind: KindInteger, Label: "Tasks", Help: "How many runs of pages to share a PDF out as, or 0 to leave it to the pool. A picture is read in one.", Default: 0, Minimum: bound(0), Maximum: bound(MaxTasks)},
	}
}

// checkOCRLanguage refuses a language that is not one of ocrLanguages, or
// several of them joined with +, each once.
func checkOCRLanguage(language string) error {
	parts := strings.Split(language, "+")
	for i, part := range parts {
		if !slices.Contains(ocrLanguages, part) || slices.Contains(parts[:i], part) {
			return fmt.Errorf("language is %q, and must be one of %s, or several of them joined with +, each once", language, strings.Join(ocrLanguages, ", "))
		}
	}
	return nil
}

// scanKind says what the start of a stored file says it is: a PDF, a
// picture Tesseract reads, or neither.
func scanKind(head []byte) string {
	switch {
	case bytes.HasPrefix(head, []byte("%PDF-")):
		return "pdf"
	case bytes.HasPrefix(head, []byte("\x89PNG\r\n\x1a\n")), bytes.HasPrefix(head, []byte("\xff\xd8\xff")),
		bytes.HasPrefix(head, []byte("II*\x00")), bytes.HasPrefix(head, []byte("MM\x00*")):
		return "image"
	}
	return ""
}

func (OCR) Split(ctx context.Context, blobs Blobs, params []byte, parts int) ([][]byte, error) {
	var p OCRParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("ocr parameters: %w", err)
	}
	input, err := cid.Decode(p.Input)
	if err != nil {
		return nil, fmt.Errorf("ocr parameters: input is not the CID of a stored scan: %w", err)
	}
	p.Language = cmp.Or(p.Language, "eng")
	p.Output = cmp.Or(p.Output, "text")
	p.DPI = cmp.Or(p.DPI, 300)
	if err := checkOCRLanguage(p.Language); err != nil {
		return nil, fmt.Errorf("ocr parameters: %w", err)
	}
	switch {
	case !slices.Contains(ocrOutputs, p.Output):
		return nil, fmt.Errorf("ocr parameters: output is %q, and must be one of %s", p.Output, strings.Join(ocrOutputs, ", "))
	case p.DPI < ocrLeastDPI || p.DPI > ocrMostDPI:
		return nil, fmt.Errorf("ocr parameters: dpi is %d, and must be from %d to %d", p.DPI, ocrLeastDPI, ocrMostDPI)
	case p.Tasks < 0 || p.Tasks > MaxTasks:
		return nil, fmt.Errorf("ocr parameters: tasks is %d, and must be from 1 to %d, or 0 to leave it to the pool", p.Tasks, MaxTasks)
	}
	// What the scan is decides how it is shared out, so its first bytes
	// are read here; the pages of a PDF are counted by its tasks.
	blob, err := blobs.Open(ctx, input)
	if err != nil {
		return nil, fmt.Errorf("ocr input %s: %w", input, err)
	}
	head := make([]byte, 8)
	n, _ := io.ReadFull(blob, head) // a short file has a short head
	blob.Close()
	kind := scanKind(head[:n])
	switch kind {
	case "":
		return nil, errors.New("ocr parameters: input is neither a PDF nor a PNG, JPEG or TIFF picture")
	case "image":
		p.Tasks = 1
	}
	return Container{}.Split(ctx, blobs, mustJSON(ContainerParams{
		Image: OCRImage, Command: []string{"sh", "-c", ocrScript},
		Input: p.Input, Tasks: p.Tasks,
		Env: map[string]string{"KIND": kind, "LANGUAGE": p.Language, "DPI": strconv.Itoa(p.DPI), "OUTPUT": p.Output},
	}), parts)
}

func (o OCR) Execute(ctx context.Context, blobs Blobs, payload []byte) ([]byte, error) {
	engine := o.Engine
	if engine == nil {
		engine = docker
	}
	if err := haveImage(ctx, engine, OCRImage, ocrDockerfile, "reading text"); err != nil {
		return nil, err
	}
	return Container{Engine: engine, Confined: o.Confined}.Execute(ctx, blobs, payload)
}

// Varies says that what is read can differ between runs, though Tesseract
// reads the same picture the same way on one machine: the searchable PDF
// it makes carries the time it was made, so no two are the same bytes;
// each worker builds the image from Alpine's packages as they are when it
// does, so two workers may have different releases of Tesseract and
// Poppler; and both reckon with floating point, which differs a little
// from one kind of processor to another. Comparing two runs' bytes, as
// verification does, would take honest workers for wrong ones.
func (OCR) Varies() bool { return true }

// ocrSummary is what a task says it read: pages First to Last of a scan of
// Pages pages, none if Last is before First.
type ocrSummary struct {
	First    int    `json:"first"`
	Last     int    `json:"last"`
	Pages    int    `json:"pages"`
	Output   string `json:"output"`
	Language string `json:"language"`
}

// ocrPart is a run of pages one task made into a searchable PDF.
type ocrPart struct {
	first, last int
	pdf         string
}

// Aggregate takes from each task only the files it is meant to leave, by
// their fixed names, and checks that the tasks read every page once, in
// order, before anything they said is believed.
func (OCR) Aggregate(ctx context.Context, blobs Blobs, outputs [][]byte) ([]byte, error) {
	result := OCRResult{Tasks: len(outputs)}
	var text strings.Builder
	var parts []ocrPart
	var output string
	read := 0
	for i, raw := range outputs {
		var out ContainerOutput
		if err := json.Unmarshal(raw, &out); err != nil {
			return nil, fmt.Errorf("task %d output: %w", i, err)
		}
		var summary ocrSummary
		if err := decodeOCRSummary(ctx, blobs, out.Files["summary.json"], &summary); err != nil {
			return nil, fmt.Errorf("task %d's summary: %w", i, err)
		}
		if i == 0 {
			result.Pages, result.Language, output = summary.Pages, summary.Language, summary.Output
			if !slices.Contains(ocrOutputs, output) || checkOCRLanguage(result.Language) != nil || result.Pages < 0 {
				return nil, fmt.Errorf("task %d says it read %d pages in %q for %q, which no job asks for", i, result.Pages, result.Language, output)
			}
		}
		switch {
		case summary.Pages != result.Pages || summary.Output != output || summary.Language != result.Language:
			return nil, fmt.Errorf("task %d read a scan of %d pages in %q for %q, and task 0 one of %d in %q for %q", i, summary.Pages, summary.Language, summary.Output, result.Pages, result.Language, output)
		case summary.First != read+1 || summary.Last < read || summary.Last > result.Pages:
			return nil, fmt.Errorf("task %d says it read pages %d to %d of %d, after pages up to %d", i, summary.First, summary.Last, result.Pages, read)
		case summary.Last == read:
			continue // more tasks than pages: none was left to this one
		}
		if read > 0 {
			text.WriteByte('\f') // between the last page before and this task's first
		}
		pages, err := readOCRText(ctx, blobs, out.Files["text.txt"], ocrMostText-text.Len())
		if err != nil {
			return nil, fmt.Errorf("task %d's text: %w", i, err)
		}
		if got, want := strings.Count(pages, "\f")+1, summary.Last-summary.First+1; got != want {
			return nil, fmt.Errorf("task %d left the text of %d pages, and says it read %d", i, got, want)
		}
		text.WriteString(pages)
		if output != "text" {
			pdf := out.Files["text.pdf"]
			if _, err := cid.Decode(pdf); err != nil {
				return nil, fmt.Errorf("task %d left no searchable PDF", i)
			}
			parts = append(parts, ocrPart{summary.First, summary.Last, pdf})
		}
		read = summary.Last
	}
	if read != result.Pages {
		return nil, fmt.Errorf("the tasks read %d of the scan's %d pages", read, result.Pages)
	}
	for _, r := range text.String() {
		if !unicode.IsSpace(r) {
			result.Characters++
		}
	}
	if output != "pdf" {
		stored, err := blobs.Put(ctx, strings.NewReader(text.String()))
		if err != nil {
			return nil, fmt.Errorf("store the text: %w", err)
		}
		result.Text = stored.String()
	}
	switch {
	case len(parts) == 1:
		result.PDF = parts[0].pdf
	case len(parts) > 1:
		// Each is named by its pages, padded so that in order by name is
		// in order by page: names the coordinator makes from numbers it
		// has checked, and none a worker gave.
		width := len(strconv.Itoa(result.Pages))
		names := make([]string, len(parts))
		for i, part := range parts {
			names[i] = fmt.Sprintf("pages-%0*d-%0*d.pdf", width, part.first, width, part.last)
			result.PDFs = append(result.PDFs, part.pdf)
		}
		reading, writing := io.Pipe()
		go func() { writing.CloseWithError(zipFrames(ctx, blobs, writing, names, result.PDFs)) }()
		archive, err := blobs.Put(ctx, reading)
		reading.CloseWithError(errors.New("the archive is no longer being stored"))
		if err != nil {
			return nil, fmt.Errorf("store the searchable PDFs as one zip file: %w", err)
		}
		result.Archive = archive.String()
	}
	return mustJSON(result), nil
}

// decodeOCRSummary reads a task's summary.
func decodeOCRSummary(ctx context.Context, blobs Blobs, id string, summary *ocrSummary) error {
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

// readOCRText reads the text a task left, refusing more than room bytes.
func readOCRText(ctx context.Context, blobs Blobs, id string, room int) (string, error) {
	c, err := cid.Decode(id)
	if err != nil {
		return "", errors.New("the task left none")
	}
	blob, err := blobs.Open(ctx, c)
	if err != nil {
		return "", err
	}
	defer blob.Close()
	text, err := io.ReadAll(io.LimitReader(blob, int64(room)+1))
	if err == nil && len(text) > room {
		err = fmt.Errorf("it takes the job's text past the %d bytes a job may have", ocrMostText)
	}
	return string(text), err
}

// ResultBlobs are the searchable PDFs the result refers to, which the tasks
// stored. The text and the archive are stored by Aggregate, and kept with
// the result as everything it stores is.
func (OCR) ResultBlobs(raw []byte) []string {
	var result OCRResult
	json.Unmarshal(raw, &result)
	if result.PDF != "" {
		return []string{result.PDF}
	}
	return result.PDFs
}
