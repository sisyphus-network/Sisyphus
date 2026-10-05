package runtime

import (
	"bytes"
	"encoding/json"
	"io"
	"math/rand/v2"
	"reflect"
	"strings"
	"testing"

	"github.com/ipfs/go-cid"

	"github.com/excho0/Sisyphus/packages/storage"
)

// referenceCounts counts words in one pass using the standard library, as an
// independent statement of what countWords should find.
func referenceCounts(text string) map[string]uint64 {
	counts := make(map[string]uint64)
	isSeparator := func(r rune) bool {
		return r < 0x80 && !(r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z')
	}
	for _, word := range strings.FieldsFunc(text, isSeparator) {
		lowered := []byte(word)
		for i, b := range lowered {
			lowered[i] = lower(b)
		}
		counts[string(lowered)]++
	}
	return counts
}

// countInRanges cuts text into parts ranges at arbitrary bytes, counts each
// separately and merges.
func countInRanges(t *testing.T, text string, parts int) map[string]uint64 {
	t.Helper()
	merged := make(map[string]uint64)
	size := uint64(len(text))
	n := uint64(parts)
	for i := uint64(0); i < n; i++ {
		start, end := size*i/n, size*(i+1)/n
		counts, err := countWords(ctx, strings.NewReader(text), start, end-start)
		if err != nil {
			t.Fatal(err)
		}
		for word, c := range counts {
			merged[word] += c
		}
	}
	return merged
}

var wordTexts = map[string]string{
	"empty":             "",
	"one word":          "boulder",
	"only separators":   " \n\t .,;",
	"plain":             "The boulder rolls. The hill waits; the boulder ROLLS again!",
	"leading/trailing":  "  push  the  rock  ",
	"no final newline":  "up\nthe\nhill",
	"digits and joins":  "node42 gpu-rig 3.14 don't",
	"non-ASCII":         "Σίσυφος pushes — naïve café Σίσυφος",
	"one very long run": strings.Repeat("a", 5000),
	"long run in text":  "x " + strings.Repeat("ab", 3000) + " y " + strings.Repeat("ab", 3000),
	"single letters":    "a b c a b a",
}

func TestCountWordsOverTheWholeText(t *testing.T) {
	for name, text := range wordTexts {
		got, err := countWords(ctx, strings.NewReader(text), 0, uint64(len(text)))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if want := referenceCounts(text); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: got %v, want %v", name, got, want)
		}
	}
}

// However a text is cut, every word must be counted exactly once. Cutting
// into as many ranges as bytes puts a boundary at every position.
func TestCountWordsIsUnaffectedByWhereRangesAreCut(t *testing.T) {
	for name, text := range wordTexts {
		want := referenceCounts(text)
		for _, parts := range []int{1, 2, 3, 7, 64, len(text)} {
			if parts == 0 {
				continue
			}
			if got := countInRanges(t, text, parts); !reflect.DeepEqual(got, want) {
				t.Errorf("%s in %d ranges: got %v, want %v", name, parts, got, want)
			}
		}
	}
}

func TestCountWordsOnRandomTextCutAtRandom(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	const alphabet = "aAbB9 \n.é"
	for trial := 0; trial < 300; trial++ {
		var text strings.Builder
		for n := rng.IntN(200); n > 0; n-- {
			text.WriteByte(alphabet[rng.IntN(len(alphabet))])
		}
		want := countInRanges(t, text.String(), 1)
		parts := 1 + rng.IntN(20)
		if got := countInRanges(t, text.String(), parts); !reflect.DeepEqual(got, want) {
			t.Fatalf("%q in %d ranges: got %v, want %v", text.String(), parts, got, want)
		}
	}
}

func TestCountWordsStopsWhenCancelled(t *testing.T) {
	cancelled, cancel := contextWithCancel()
	cancel()
	if _, err := countWords(cancelled, strings.NewReader("some words here"), 0, 15); err == nil {
		t.Error("countWords with a cancelled context succeeded")
	}
}

// runWordCount runs the workload start to finish against one store, as the
// coordinator and workers would between them.
func runWordCount(t *testing.T, store *storage.Store, text string, parts int) WordCountResult {
	t.Helper()
	input, err := store.Put(ctx, strings.NewReader(text))
	if err != nil {
		t.Fatal(err)
	}
	params, _ := json.Marshal(WordCountParams{Input: input.String()})
	w := WordCount{}
	payloads, err := w.Split(ctx, store, params, parts)
	if err != nil {
		t.Fatal(err)
	}
	var outputs [][]byte
	for _, payload := range payloads {
		output, err := w.Execute(ctx, store, payload)
		if err != nil {
			t.Fatal(err)
		}
		outputs = append(outputs, output)
	}
	encoded, err := w.Aggregate(ctx, store, outputs)
	if err != nil {
		t.Fatal(err)
	}
	var result WordCountResult
	if err := json.Unmarshal(encoded, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func readBlob(t *testing.T, store *storage.Store, id string) string {
	t.Helper()
	c, err := cid.Decode(id)
	if err != nil {
		t.Fatal(err)
	}
	blob, err := store.Open(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	defer blob.Close()
	data, err := io.ReadAll(blob)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestWordCountProducesASortedTable(t *testing.T) {
	store := storage.NewMemory()
	result := runWordCount(t, store, "the hill, the boulder, the hill. Up!", 3)

	if result.Words != 7 || result.Distinct != 4 {
		t.Errorf("words %d, distinct %d; want 7 and 4", result.Words, result.Distinct)
	}
	want := "3\tthe\n2\thill\n1\tboulder\n1\tup\n"
	if got := readBlob(t, store, result.Output); got != want {
		t.Errorf("table:\n%s\nwant:\n%s", got, want)
	}
}

// The result's CID must not depend on how the job was split, so that two
// independent runs of a job can be compared by CID alone.
func TestWordCountResultCIDIsTheSameHoweverTheJobIsSplit(t *testing.T) {
	text := strings.Repeat("one must imagine Sisyphus happy; the struggle itself is enough. ", 2000)
	store := storage.NewMemory()
	whole := runWordCount(t, store, text, 1)
	for _, parts := range []int{2, 5, 13, 100} {
		if got := runWordCount(t, store, text, parts); got != whole {
			t.Errorf("split into %d: %+v, but as one task: %+v", parts, got, whole)
		}
	}
}

func TestWordCountSplitCoversTheInputExactly(t *testing.T) {
	store := storage.NewMemory()
	for _, tt := range []struct{ size, parts, wantParts int }{
		{1000, 7, 7},
		{3, 8, 3},
		{0, 4, 1},
		{1000, 1, 1},
		{1000, 0, 1},
	} {
		input, err := store.Put(ctx, bytes.NewReader(make([]byte, tt.size)))
		if err != nil {
			t.Fatal(err)
		}
		params, _ := json.Marshal(WordCountParams{Input: input.String()})
		payloads, err := WordCount{}.Split(ctx, store, params, tt.parts)
		if err != nil {
			t.Fatal(err)
		}
		if len(payloads) != tt.wantParts {
			t.Errorf("%d bytes into %d: got %d parts, want %d", tt.size, tt.parts, len(payloads), tt.wantParts)
		}
		next := uint64(0)
		for _, payload := range payloads {
			var r wordCountRange
			if err := json.Unmarshal(payload, &r); err != nil {
				t.Fatal(err)
			}
			if r.Offset != next || r.Input != input.String() {
				t.Fatalf("%d bytes into %d: range %+v does not continue from %d", tt.size, tt.parts, r, next)
			}
			next = r.Offset + r.Length
		}
		if next != uint64(tt.size) {
			t.Errorf("%d bytes into %d: ranges end at %d", tt.size, tt.parts, next)
		}
	}
}

func TestWordCountRejectsBadInput(t *testing.T) {
	store := storage.NewMemory()
	absent, err := storage.CID(ctx, strings.NewReader("never stored"))
	if err != nil {
		t.Fatal(err)
	}
	for _, params := range []string{
		`not json`,
		`{}`,
		`{"input":"not-a-cid"}`,
		`{"input":"` + absent.String() + `"}`,
	} {
		if _, err := (WordCount{}).Split(ctx, store, []byte(params), 2); err == nil {
			t.Errorf("Split(%s) succeeded, want an error", params)
		}
	}
	for _, payload := range []string{`not json`, `{"input":"not-a-cid"}`, `{"input":"` + absent.String() + `","length":5}`} {
		if _, err := (WordCount{}).Execute(ctx, store, []byte(payload)); err == nil {
			t.Errorf("Execute(%s) succeeded, want an error", payload)
		}
	}
	for _, output := range []string{`not json`, `{"counts":"not-a-cid"}`, `{"counts":"` + absent.String() + `"}`} {
		if _, err := (WordCount{}).Aggregate(ctx, store, [][]byte{[]byte(output)}); err == nil {
			t.Errorf("Aggregate(%s) succeeded, want an error", output)
		}
	}
}
