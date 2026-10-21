package runtime

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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

func TestAnOCRJobIsAContainerJobWithTheImageAndScriptFixed(t *testing.T) {
	store := storage.NewMemory()
	ctx := context.Background()
	scan, _ := store.Put(ctx, strings.NewReader("%PDF-1.7\nnot much of one"))
	input := `"input":"` + scan.String() + `"`

	payloads, err := OCR{}.Split(ctx, store, []byte(`{`+input+`}`), 4)
	if err != nil || len(payloads) != 4 {
		t.Fatalf("split: %d payloads, %v", len(payloads), err)
	}
	var task containerTask
	json.Unmarshal(payloads[3], &task)
	if task.Image != OCRImage || !strings.HasPrefix(OCRImage, "sisyphus-ocr:") || task.Input != scan.String() || task.Index != 3 || task.Count != 4 || task.Network {
		t.Errorf("the last task: %+v", task)
	}
	if len(task.Command) != 3 || task.Command[0] != "sh" || !strings.Contains(task.Command[2], "pdftoppm") || !strings.Contains(task.Command[2], "tesseract") || !strings.Contains(task.Command[2], "pdfunite") {
		t.Errorf("its command: %q", task.Command)
	}
	// Unless said otherwise: English, as text, at 300 dots per inch.
	if want := map[string]string{"KIND": "pdf", "LANGUAGE": "eng", "DPI": "300", "OUTPUT": "text"}; !equalEnv(task.Env, want) {
		t.Errorf("its settings: %v", task.Env)
	}

	payloads, err = OCR{}.Split(ctx, store, []byte(`{`+input+`,"language":"eng+heb+ara","dpi":600,"output":"text+pdf","tasks":3}`), 8)
	if err != nil || len(payloads) != 3 {
		t.Fatalf("split as asked: %d payloads, %v", len(payloads), err)
	}
	json.Unmarshal(payloads[0], &task)
	if want := map[string]string{"KIND": "pdf", "LANGUAGE": "eng+heb+ara", "DPI": "600", "OUTPUT": "text+pdf"}; !equalEnv(task.Env, want) || task.Count != 3 {
		t.Errorf("settings as asked: %v, %d tasks", task.Env, task.Count)
	}

	// A picture is one task, however many are asked for.
	for name, head := range map[string]string{"PNG": "\x89PNG\r\n\x1a\nrest", "JPEG": "\xff\xd8\xff\xe0rest", "TIFF": "II*\x00rest", "big-endian TIFF": "MM\x00*rest"} {
		picture, _ := store.Put(ctx, strings.NewReader(head))
		payloads, err := OCR{}.Split(ctx, store, []byte(`{"input":"`+picture.String()+`","tasks":5,"output":"pdf"}`), 8)
		if err != nil || len(payloads) != 1 {
			t.Fatalf("%s: %d payloads, %v", name, len(payloads), err)
		}
		json.Unmarshal(payloads[0], &task)
		if task.Env["KIND"] != "image" || task.Env["OUTPUT"] != "pdf" || task.Count != 1 {
			t.Errorf("%s: %v, %d tasks", name, task.Env, task.Count)
		}
	}

	nonsense, _ := store.Put(ctx, strings.NewReader("plain words"))
	empty, _ := store.Put(ctx, strings.NewReader(""))
	missing := "bafkreigh2akiscaildcqabsyg3dfr6chu3fgpregiymsck7e7aqa4s52zy"
	for params, want := range map[string]string{
		`not json`:                                  "ocr parameters",
		`{"input":"somewhere"}`:                     "input is not the CID of a stored scan",
		`{` + input + `,"language":"klingon"}`:      `language is "klingon", and must be one of eng, spa, fra, deu, heb, ara, rus, pol`,
		`{` + input + `,"language":"eng+"}`:         `language is "eng+"`,
		`{` + input + `,"language":"eng+eng"}`:      `language is "eng+eng"`,
		`{` + input + `,"language":"ENG"}`:          `language is "ENG"`,
		`{` + input + `,"output":"docx"}`:           `output is "docx", and must be one of text, pdf, text+pdf`,
		`{` + input + `,"dpi":71}`:                  "dpi is 71, and must be from 72 to 600",
		`{` + input + `,"dpi":601}`:                 "dpi is 601",
		`{` + input + `,"tasks":-1}`:                "tasks is -1, and must be from 1 to 10000",
		`{` + input + `,"tasks":10001}`:             "tasks is 10001",
		`{"input":"` + nonsense.String() + `"}`:     "input is neither a PDF nor a PNG, JPEG or TIFF picture",
		`{"input":"` + empty.String() + `"}`:        "input is neither a PDF nor a PNG, JPEG or TIFF picture",
		`{"input":"` + missing + `"}`:               "ocr input " + missing,
		`{"input":"` + missing + `","dpi":300000}`:  "dpi is 300000",
		`{` + input + `,"language":"eng+spa+xyz"}`:  `language is "eng+spa+xyz"`,
		`{` + input + `,"language":"heb+ara+heb"}`:  `language is "heb+ara+heb"`,
		`{` + input + `,"output":"text+pdf+text"}`:  `output is "text+pdf+text"`,
		`{` + input + `,"output":"pdf","dpi":-30}`:  "dpi is -30",
		`{` + input + `,"language":"eng","dpi":1}`:  "dpi is 1",
		`{` + input + `,"language":"eng+pol+rus"}`:  "",
		`{` + input + `,"language":"spa+fra+deu"}`:  "",
		`{` + input + `,"output":"pdf","dpi":72}`:   "",
		`{` + input + `,"output":"text","dpi":600}`: "",
	} {
		_, err := (OCR{}).Split(ctx, store, []byte(params), 2)
		if want == "" && err != nil || want != "" && (err == nil || !strings.Contains(err.Error(), want)) {
			t.Errorf("%s: %v, want an error containing %q", params, err, want)
		}
	}
	if w, err := WithContainers().Get("ocr"); err != nil || w.Name() != "ocr" || !strings.Contains(WithContainers().Describe("ocr"), "Tesseract") {
		t.Errorf("the workload as registered: %v, %v", w, err)
	}
	if !WithContainers().InContainers("ocr") {
		t.Error("ocr is not said to run in containers")
	}
	if _, err := Builtin().Get("ocr"); err == nil {
		t.Error("a node that runs no containers offers to read scans")
	}
	if !(OCR{}).Varies() {
		t.Error("ocr says two honest runs give the same result, and its PDFs carry the time they were made")
	}
}

func TestTheOCRImageIsBuiltOnlyWhereItIsMissing(t *testing.T) {
	store := storage.NewMemory()
	ctx := context.Background()
	scan, _ := store.Put(ctx, strings.NewReader("%PDF-1.4"))
	payloads, _ := OCR{}.Split(ctx, store, []byte(`{"input":"`+scan.String()+`"}`), 1)
	reads := func(args []string, _, _ io.Writer) (int, error) {
		os.WriteFile(filepath.Join(volume(args, "/output"), "summary.json"), []byte(`{}`), 0o644)
		return 0, nil
	}
	has := &pretend{do: func(args []string, stdout, stderr io.Writer) (int, error) {
		if args[0] == "image" {
			return 0, nil
		}
		return reads(args, stdout, stderr)
	}}
	if _, err := (OCR{Engine: has.engine}).Execute(ctx, store, payloads[0]); err != nil {
		t.Fatal(err)
	}
	if len(has.asked) != 2 || strings.Join(has.asked[0], " ") != "image inspect "+OCRImage || has.asked[1][0] != "run" {
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
		return reads(args, stdout, stderr)
	}}
	said := new(heard)
	if _, err := (OCR{Engine: lacks.engine}).Execute(WithReporter(ctx, said), store, payloads[0]); err != nil {
		t.Fatal(err)
	}
	if len(lacks.asked) != 3 || strings.Join(lacks.asked[1][:3], " ") != "build --tag "+OCRImage || !strings.Contains(recipe, "poppler-utils tesseract-ocr ") {
		t.Errorf("with the image missing, the engine was asked %v with the recipe %q", lacks.asked, recipe)
	}
	for _, language := range ocrLanguages {
		if !strings.Contains(recipe, " tesseract-ocr-data-"+language) {
			t.Errorf("the image has no data for %s: %q", language, recipe)
		}
	}
	if log := strings.Join(said.lines, "\n"); !strings.Contains(log, "building the image reading text runs in") {
		t.Errorf("the task's log: %q", said.lines)
	}

	broken := &pretend{do: func(args []string, _, stderr io.Writer) (int, error) {
		if args[0] == "image" {
			return 1, nil
		}
		io.WriteString(stderr, "ERROR: unable to select packages\n")
		return 1, nil
	}}
	if _, err := (OCR{Engine: broken.engine}).Execute(ctx, store, payloads[0]); err == nil || !strings.Contains(err.Error(), "build the image reading text runs in: ERROR: unable to select packages") {
		t.Errorf("a build that fails: %v", err)
	}
	if _, err := (OCR{Engine: has.engine}).Execute(ctx, store, []byte("not json")); err == nil {
		t.Error("a payload that is not one was run")
	}
}

// ocrTask is the output of a task that left these files, by name, with a
// summary among them unless it is empty.
func ocrTask(t *testing.T, store *storage.Store, index int, summary string, files map[string]string) []byte {
	t.Helper()
	ctx := context.Background()
	stored := map[string]string{}
	if summary != "" {
		s, _ := store.Put(ctx, strings.NewReader(summary))
		stored["summary.json"] = s.String()
	}
	for name, content := range files {
		c, _ := store.Put(ctx, strings.NewReader(content))
		stored[name] = c.String()
	}
	return mustJSON(ContainerOutput{Index: index, Files: stored})
}

// ocrSummaryOf is what a task reading pages first to last of pages says.
func ocrSummaryOf(first, last, pages int, output string) string {
	return mustJSONString(ocrSummary{First: first, Last: last, Pages: pages, Output: output, Language: "eng+heb"})
}

func mustJSONString(v any) string { return string(mustJSON(v)) }

func TestReadPagesArePutTogetherInOrder(t *testing.T) {
	store := storage.NewMemory()
	ctx := context.Background()
	// Five pages among four tasks, the first of which was left none, as
	// the script shares them: none, 1 to 2, 3, 4 to 5.
	outputs := [][]byte{
		ocrTask(t, store, 0, ocrSummaryOf(1, 0, 5, "text+pdf"), nil),
		ocrTask(t, store, 1, ocrSummaryOf(1, 2, 5, "text+pdf"), map[string]string{"text.txt": "one\n\fthe second page\n", "text.pdf": "%PDF pages 1-2"}),
		ocrTask(t, store, 2, ocrSummaryOf(3, 3, 5, "text+pdf"), map[string]string{"text.txt": "", "text.pdf": "%PDF page 3"}),
		ocrTask(t, store, 3, ocrSummaryOf(4, 5, 5, "text+pdf"), map[string]string{"text.txt": "שלום\n\fend", "text.pdf": "%PDF pages 4-5"}),
	}
	raw, err := OCR{}.Aggregate(ctx, store, outputs)
	if err != nil {
		t.Fatal(err)
	}
	var result OCRResult
	json.Unmarshal(raw, &result)
	if result.Pages != 5 || result.Language != "eng+heb" || result.Tasks != 4 || result.Characters != 23 || result.PDF != "" || len(result.PDFs) != 3 {
		t.Fatalf("the result: %+v", result)
	}
	if got, want := readBlob(t, store, result.Text), "one\n\fthe second page\n\f\fשלום\n\fend"; got != want {
		t.Errorf("the text is %q, want %q", got, want)
	}
	for i, want := range []string{"%PDF pages 1-2", "%PDF page 3", "%PDF pages 4-5"} {
		if got := readBlob(t, store, result.PDFs[i]); got != want {
			t.Errorf("PDF %d is %q", i, got)
		}
	}
	if got := (OCR{}).ResultBlobs(raw); !slices.Equal(got, result.PDFs) {
		t.Errorf("the blobs kept with the result: %v", got)
	}
	zipped := []byte(readBlob(t, store, result.Archive))
	archive, err := zip.NewReader(bytes.NewReader(zipped), int64(len(zipped)))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range archive.File {
		names = append(names, f.Name)
	}
	if !slices.Equal(names, []string{"pages-1-2.pdf", "pages-3-3.pdf", "pages-4-5.pdf"}) {
		t.Errorf("the archive has %v", names)
	}

	// Page numbers are padded to the width of the last.
	padded, err := OCR{}.Aggregate(ctx, store, [][]byte{
		ocrTask(t, store, 0, ocrSummaryOf(1, 9, 12, "pdf"), map[string]string{"text.txt": strings.Repeat("\f", 8), "text.pdf": "a"}),
		ocrTask(t, store, 1, ocrSummaryOf(10, 12, 12, "pdf"), map[string]string{"text.txt": "\f\f", "text.pdf": "b"}),
	})
	result = OCRResult{}
	json.Unmarshal(padded, &result)
	zipped = []byte(readBlob(t, store, result.Archive))
	archive, _ = zip.NewReader(bytes.NewReader(zipped), int64(len(zipped)))
	// Only a PDF was asked for: there is no text to keep.
	if err != nil || len(archive.File) != 2 || archive.File[0].Name != "pages-01-09.pdf" || archive.File[1].Name != "pages-10-12.pdf" || result.Text != "" || result.Characters != 0 {
		t.Errorf("a PDF alone, of 12 pages: %+v, %v", result, err)
	}

	// One task that read every page made the whole PDF, and an archive of
	// one would be no use.
	whole, err := OCR{}.Aggregate(ctx, store, [][]byte{ocrTask(t, store, 0, ocrSummaryOf(1, 2, 2, "text+pdf"), map[string]string{"text.txt": "a\fb", "text.pdf": "%PDF whole"})})
	result = OCRResult{}
	json.Unmarshal(whole, &result)
	if err != nil || result.Archive != "" || result.PDFs != nil || readBlob(t, store, result.PDF) != "%PDF whole" || readBlob(t, store, result.Text) != "a\fb" || result.Characters != 2 {
		t.Errorf("one task: %+v, %v", result, err)
	}
	if got := (OCR{}).ResultBlobs(whole); !slices.Equal(got, []string{result.PDF}) {
		t.Errorf("the blobs kept with one PDF: %v", got)
	}
	// Text alone keeps no PDF.
	text, err := OCR{}.Aggregate(ctx, store, [][]byte{
		ocrTask(t, store, 0, ocrSummaryOf(1, 1, 2, "text"), map[string]string{"text.txt": "a"}),
		ocrTask(t, store, 1, ocrSummaryOf(2, 2, 2, "text"), map[string]string{"text.txt": "b", "text.pdf": "left anyway"}),
	})
	result = OCRResult{}
	json.Unmarshal(text, &result)
	if err != nil || result.PDF != "" || result.PDFs != nil || readBlob(t, store, result.Text) != "a\fb" || len((OCR{}).ResultBlobs(text)) != 0 || len((OCR{}).ResultBlobs([]byte("not json"))) != 0 {
		t.Errorf("text alone: %+v, %v", result, err)
	}
	// A PDF with no pages is read, as nothing.
	nothing, err := OCR{}.Aggregate(ctx, store, [][]byte{ocrTask(t, store, 0, ocrSummaryOf(1, 0, 0, "text"), nil)})
	result = OCRResult{}
	json.Unmarshal(nothing, &result)
	if err != nil || result.Pages != 0 || readBlob(t, store, result.Text) != "" {
		t.Errorf("no pages: %+v, %v", result, err)
	}
}

// Whatever a task says is checked: the pages are read once each, in order,
// all with the same settings, and only the files the script leaves are
// taken, by their names.
func TestWhatOCRTasksSayIsChecked(t *testing.T) {
	store := storage.NewMemory()
	ctx := context.Background()
	missing := "bafkreigh2akiscaildcqabsyg3dfr6chu3fgpregiymsck7e7aqa4s52zy"
	garbled, _ := store.Put(ctx, strings.NewReader("not json"))
	first := ocrTask(t, store, 0, ocrSummaryOf(1, 1, 2, "text+pdf"), map[string]string{"text.txt": "a", "text.pdf": "%PDF"})
	second := func(summary string, files map[string]string) [][]byte {
		return [][]byte{first, ocrTask(t, store, 1, summary, files)}
	}
	good := map[string]string{"text.txt": "b", "text.pdf": "%PDF"}
	for name, tt := range map[string]struct {
		outputs [][]byte
		blobs   Blobs
		want    string
	}{
		"an output that is not one":        {[][]byte{first, []byte("not json")}, store, "task 1 output"},
		"a task with no summary":           {[][]byte{ocrTask(t, store, 0, "", good)}, store, "task 0's summary: the task left none"},
		"a summary that is not there":      {[][]byte{mustJSON(ContainerOutput{Files: map[string]string{"summary.json": missing}})}, store, "task 0's summary: "},
		"a summary that is garbled":        {[][]byte{mustJSON(ContainerOutput{Files: map[string]string{"summary.json": garbled.String()}})}, store, "task 0's summary: invalid character"},
		"an output no job asks for":        {[][]byte{ocrTask(t, store, 0, `{"first":1,"last":1,"pages":1,"output":"docx","language":"eng"}`, good)}, store, `task 0 says it read 1 pages in "eng" for "docx", which no job asks for`},
		"a language that is not installed": {[][]byte{ocrTask(t, store, 0, `{"first":1,"last":1,"pages":1,"output":"text","language":"../../x"}`, good)}, store, "which no job asks for"},
		"fewer than no pages":              {[][]byte{ocrTask(t, store, 0, `{"first":1,"last":-1,"pages":-1,"output":"text","language":"eng"}`, good)}, store, "which no job asks for"},
		"another scan":                     {second(ocrSummaryOf(2, 3, 3, "text+pdf"), good), store, `task 1 read a scan of 3 pages in "eng+heb" for "text+pdf", and task 0 one of 2`},
		"other settings":                   {second(ocrSummaryOf(2, 2, 2, "text"), good), store, "task 1 read a scan of 2 pages"},
		"a page read twice":                {second(ocrSummaryOf(1, 2, 2, "text+pdf"), good), store, "task 1 says it read pages 1 to 2 of 2, after pages up to 1"},
		"a page skipped":                   {[][]byte{ocrTask(t, store, 0, ocrSummaryOf(2, 2, 2, "text"), good)}, store, "task 0 says it read pages 2 to 2 of 2, after pages up to 0"},
		"pages that run backwards":         {second(ocrSummaryOf(2, 0, 2, "text+pdf"), good), store, "task 1 says it read pages 2 to 0"},
		"pages past the last":              {second(ocrSummaryOf(2, 3, 2, "text+pdf"), good), store, "task 1 says it read pages 2 to 3 of 2"},
		"pages left unread":                {[][]byte{first}, store, "the tasks read 1 of the scan's 2 pages"},
		"no text":                          {second(ocrSummaryOf(2, 2, 2, "text+pdf"), map[string]string{"text.pdf": "%PDF"}), store, "task 1's text: the task left none"},
		"text that is not there":           {[][]byte{mustJSON(ContainerOutput{Files: map[string]string{"summary.json": storedCID(t, store, ocrSummaryOf(1, 1, 1, "text")), "text.txt": missing}})}, store, "task 0's text: "},
		"the text of too many pages":       {second(ocrSummaryOf(2, 2, 2, "text+pdf"), map[string]string{"text.txt": "b\fc", "text.pdf": "%PDF"}), store, "task 1 left the text of 2 pages, and says it read 1"},
		"the text of too few pages":        {[][]byte{ocrTask(t, store, 0, ocrSummaryOf(1, 3, 3, "text"), map[string]string{"text.txt": "a\fb"})}, store, "task 0 left the text of 2 pages, and says it read 3"},
		"no searchable PDF":                {second(ocrSummaryOf(2, 2, 2, "text+pdf"), map[string]string{"text.txt": "b"}), store, "task 1 left no searchable PDF"},
		"text that cannot be stored":       {second(ocrSummaryOf(2, 2, 2, "text+pdf"), good), &awkward{Blobs: store, failPut: 1}, "store the text: the store is full"},
		"an archive that cannot be stored": {second(ocrSummaryOf(2, 2, 2, "text+pdf"), good), &awkward{Blobs: store, failPut: 2}, "store the searchable PDFs as one zip file: the store is full"},
	} {
		if _, err := (OCR{}).Aggregate(ctx, tt.blobs, tt.outputs); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: %v, want an error containing %q", name, err, tt.want)
		}
	}

	// A PDF a task names that is not stored fails the archive, by the name
	// the coordinator gave it.
	lost := mustJSON(ContainerOutput{Files: map[string]string{"summary.json": storedCID(t, store, ocrSummaryOf(2, 2, 2, "pdf")), "text.txt": storedCID(t, store, "b"), "text.pdf": missing}})
	firstPDF := ocrTask(t, store, 0, ocrSummaryOf(1, 1, 2, "pdf"), map[string]string{"text.txt": "a", "text.pdf": "%PDF"})
	if _, err := (OCR{}).Aggregate(ctx, store, [][]byte{firstPDF, lost}); err == nil || !strings.Contains(err.Error(), "store the searchable PDFs as one zip file: pages-2-2.pdf") {
		t.Errorf("a searchable PDF that is not stored: %v", err)
	}

	// Files other than the script's are not looked at, whatever their names.
	odd := ocrTask(t, store, 0, ocrSummaryOf(1, 1, 1, "text+pdf"), map[string]string{"text.txt": "a", "text.pdf": "%PDF", "../../escape.pdf": "x", "pages-1-1.pdf": "y"})
	raw, err := OCR{}.Aggregate(ctx, store, [][]byte{odd})
	var result OCRResult
	json.Unmarshal(raw, &result)
	if err != nil || readBlob(t, store, result.PDF) != "%PDF" {
		t.Errorf("a task that left other files: %+v, %v", result, err)
	}

	// Text past the most a job may have is refused, from one task or added
	// up from several.
	huge := ocrTask(t, store, 0, ocrSummaryOf(1, 1, 1, "text"), map[string]string{"text.txt": strings.Repeat("a", ocrMostText+1)})
	if _, err := (OCR{}).Aggregate(ctx, store, [][]byte{huge}); err == nil || !strings.Contains(err.Error(), "task 0's text: it takes the job's text past the 67108864 bytes a job may have") {
		t.Errorf("too much text: %v", err)
	}
	half := strings.Repeat("a", ocrMostText/2)
	halves := [][]byte{
		ocrTask(t, store, 0, ocrSummaryOf(1, 1, 2, "text"), map[string]string{"text.txt": half}),
		ocrTask(t, store, 1, ocrSummaryOf(2, 2, 2, "text"), map[string]string{"text.txt": half}),
	}
	if _, err := (OCR{}).Aggregate(ctx, store, halves); err == nil || !strings.Contains(err.Error(), "task 1's text: it takes the job's text past") {
		t.Errorf("too much text in all: %v", err)
	}
}

// storedCID stores content and returns its CID.
func storedCID(t *testing.T, store *storage.Store, content string) string {
	t.Helper()
	c, err := store.Put(context.Background(), strings.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	return c.String()
}

func TestOnlyInstalledLanguagesAreTaken(t *testing.T) {
	for language, want := range map[string]bool{
		"eng": true, "heb": true, "eng+heb": true, "eng+spa+fra+deu+heb+ara+rus+pol": true,
		"": false, "+": false, "eng+": false, "+eng": false, "eng++heb": false, "eng+eng": false, "en": false, "Eng": false,
		"eng heb": false, "eng,heb": false, "osd": false, "../eng": false, "eng;rm": false,
	} {
		if got := checkOCRLanguage(language) == nil; got != want {
			t.Errorf("%q is taken: %v", language, got)
		}
	}
	for head, want := range map[string]string{"%PDF-1.4": "pdf", "%PDF": "", "\x89PNG\r\n\x1a\n": "image", "\xff\xd8\xff": "image", "II*\x00": "image", "MM\x00*": "image", "GIF89a": "", "": ""} {
		if got := scanKind([]byte(head)); got != want {
			t.Errorf("%q is %q, want %q", head, got, want)
		}
	}
}

// ocrIn runs a command in an image with a directory of this machine's at
// /work, as transcribeIn does.
func ocrIn(t *testing.T, image, dir string, command ...string) {
	t.Helper()
	args := append([]string{"run", "--rm", "--network", "none", "--user", strconv.Itoa(os.Getuid()) + ":" + strconv.Itoa(os.Getgid()), "--volume", dir + ":/work", image}, command...)
	if out, err := exec.Command("docker", args...).CombinedOutput(); err != nil {
		t.Fatalf("%v: %v\n%s", command, err, out)
	}
}

// makeScan is three pages of PostScript, a few words on each, which
// Ghostscript makes into a PDF with its fonts in it.
const makeScan = `%!PS
/Helvetica findfont 40 scalefont setfont
72 650 moveto (Alpha harbour lantern) show showpage
72 650 moveto (Bravo meadow quartz) show showpage
72 650 moveto (Charlie violet summit) show showpage
`

// A real PDF of three pages, made by Ghostscript, read by Tesseract in two
// real containers and put together: the words of every page are there, in
// order, a page apart, and the two tasks' searchable PDFs are kept.
func TestARealScanIsRead(t *testing.T) {
	requireRealImages(t)
	requireDocker(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	// The image is built here if this machine has not got it, which takes
	// the network and a minute.
	if err := haveImage(ctx, docker, OCRImage, ocrDockerfile, "reading text"); err != nil {
		t.Fatal(err)
	}
	const writing = "FROM alpine:3.20\nRUN apk add --no-cache ghostscript\n"
	sum := sha256.Sum256([]byte(writing))
	writer := "sisyphus-test-ghostscript:" + hex.EncodeToString(sum[:6])
	if err := haveImage(ctx, docker, writer, writing, "writing PDFs"); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	os.Chmod(dir, 0o777)
	os.WriteFile(filepath.Join(dir, "scan.ps"), []byte(makeScan), 0o644)
	ocrIn(t, writer, dir, "gs", "-q", "-dBATCH", "-dNOPAUSE", "-dSAFER", "-dPDFSETTINGS=/prepress", "-sDEVICE=pdfwrite", "-sOutputFile=/work/scan.pdf", "/work/scan.ps")
	// The second page alone, as a picture, and all three as one TIFF of
	// three pages, as a fax machine would send them, too.
	ocrIn(t, OCRImage, dir, "pdftoppm", "-r", "150", "-f", "2", "-l", "2", "-singlefile", "-png", "/work/scan.pdf", "/work/page")
	ocrIn(t, writer, dir, "gs", "-q", "-dBATCH", "-dNOPAUSE", "-dSAFER", "-r200", "-sDEVICE=tiffg4", "-sOutputFile=/work/fax.tif", "/work/scan.ps")
	store := storage.NewMemory()
	put := func(name string) string {
		t.Helper()
		file, err := os.Open(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		c, err := store.Put(ctx, file)
		if err != nil {
			t.Fatal(err)
		}
		return c.String()
	}
	scan, page, fax := put("scan.pdf"), put("page.png"), put("fax.tif")

	w := OCR{Confined: Confinement{ReadOnly: true}}
	run := func(params string, parts int) (OCRResult, [][]byte) {
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
		t.Logf("%s read in %s:\n%s", params, time.Since(started).Round(time.Millisecond), strings.Join(said.lines, "\n"))
		if log := strings.Join(said.lines, "\n"); strings.Contains(log, "[DS]") || !strings.Contains(log, "read ") {
			t.Errorf("the tasks' log:\n%s", log)
		}
		raw, err := w.Aggregate(ctx, store, outputs)
		if err != nil {
			t.Fatal(err)
		}
		var result OCRResult
		json.Unmarshal(raw, &result)
		t.Logf("result: %+v", result)
		return result, outputs
	}

	result, outputs := run(`{"input":"`+scan+`","output":"text+pdf","tasks":2}`, 8)
	text := strings.ToLower(readBlob(t, store, result.Text))
	t.Logf("read: %q", text)
	pages := strings.Split(text, "\f")
	if len(pages) != 3 {
		t.Fatalf("the text has %d pages: %q", len(pages), text)
	}
	for i, words := range [][]string{{"alpha", "harbour", "lantern"}, {"bravo", "meadow", "quartz"}, {"charlie", "violet", "summit"}} {
		for _, word := range words {
			if !strings.Contains(pages[i], word) {
				t.Errorf("page %d has no %q: %q", i+1, word, pages[i])
			}
		}
	}
	if result.Pages != 3 || result.Tasks != 2 || result.Language != "eng" || result.Characters < 45 || result.Characters > 70 || result.PDF != "" || len(result.PDFs) != 2 || result.Archive == "" {
		t.Errorf("the result: %+v", result)
	}
	// Three pages in two tasks: the first reads page 1, the second 2 and 3,
	// and each makes its own pages into a searchable PDF.
	for i, want := range []struct{ pages, word string }{{"Pages:           1", "harbour"}, {"Pages:           2", "violet"}} {
		var out ContainerOutput
		json.Unmarshal(outputs[i], &out)
		if len(out.Files) != 3 {
			t.Errorf("task %d left %v", i, out.Files)
		}
		os.WriteFile(filepath.Join(dir, "part.pdf"), []byte(readBlob(t, store, result.PDFs[i])), 0o644)
		info, err := exec.Command("docker", "run", "--rm", "--network", "none", "--volume", dir+":/work", OCRImage, "sh", "-c", "pdfinfo /work/part.pdf && pdftotext /work/part.pdf -").CombinedOutput()
		if err != nil || !strings.Contains(string(info), want.pages) || !strings.Contains(strings.ToLower(string(info)), want.word) {
			t.Errorf("task %d's searchable PDF: %v\n%s", i, err, info)
		}
	}

	// A picture is one task, read at its own dots per inch, in any of the
	// languages asked for.
	result, _ = run(`{"input":"`+page+`","language":"eng+deu","tasks":2}`, 4)
	if text := strings.ToLower(readBlob(t, store, result.Text)); result.Pages != 1 || result.Tasks != 1 || result.Language != "eng+deu" || !strings.Contains(text, "meadow") {
		t.Errorf("a picture: %+v, %q", result, text)
	}

	// A TIFF's pages are each read, and made into one searchable PDF.
	result, _ = run(`{"input":"`+fax+`","output":"pdf"}`, 4)
	if result.Pages != 3 || result.Tasks != 1 || result.Text != "" || result.PDF == "" || result.Characters < 45 {
		t.Errorf("a TIFF of three pages: %+v", result)
	}

	// As text alone, in more tasks than there are pages: the tasks left no
	// pages read none, and the rest read each page once.
	result, _ = run(`{"input":"`+scan+`","tasks":4}`, 4)
	if text := strings.ToLower(readBlob(t, store, result.Text)); result.Pages != 3 || result.Tasks != 4 || result.PDF != "" || result.PDFs != nil ||
		strings.Count(text, "\f") != 2 || !strings.Contains(text, "lantern") || strings.Index(text, "quartz") > strings.Index(text, "summit") {
		t.Errorf("text alone in four tasks: %+v, %q", result, text)
	}

	// Something that only looks like a PDF is the task's failure.
	fake, _ := store.Put(ctx, strings.NewReader("%PDF-1.7 and nothing else"))
	bad, _ := w.Split(ctx, store, []byte(`{"input":"`+fake.String()+`"}`), 1)
	if _, err := w.Execute(ctx, store, bad[0]); err == nil || !strings.Contains(err.Error(), "exited with status 1: the input is not a PDF that can be read") {
		t.Errorf("reading what is not a PDF: %v", err)
	}
}
