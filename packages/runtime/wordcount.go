package runtime

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/ipfs/go-cid"
)

// WordCount counts how often each word occurs in a stored file. Like Primes
// it is a stand-in, here to exercise the path real workloads will use: a
// large input fetched by CID, each task reading only its share, task outputs
// stored as blobs, and a result blob named by a CID that is the same however
// the job was split.
//
// A word is a run of ASCII letters and digits and non-ASCII bytes, compared
// without regard to ASCII case.
type WordCount struct{}

// WordCountParams are the job parameters.
type WordCountParams struct {
	// Input is the CID of the file to count words in.
	Input string `json:"input"`
}

// wordCountRange is a task payload: count the words that start within
// Length bytes of Offset.
type wordCountRange struct {
	Input  string `json:"input"`
	Offset uint64 `json:"offset"`
	Length uint64 `json:"length"`
}

// wordCountPart is a task output.
type wordCountPart struct {
	// Counts is the CID of a JSON object mapping each word to its count.
	Counts string `json:"counts"`
}

// WordCountResult is the job result.
type WordCountResult struct {
	// Output is the CID of a text file with one "count<TAB>word" line per
	// distinct word, most frequent first and ties in byte order.
	Output   string `json:"output"`
	Words    uint64 `json:"words"`
	Distinct uint64 `json:"distinct"`
}

func (WordCount) Name() string { return "wordcount" }

func (WordCount) Split(ctx context.Context, blobs Blobs, params []byte, parts int) ([][]byte, error) {
	var p WordCountParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("wordcount params: %w", err)
	}
	input, err := cid.Decode(p.Input)
	if err != nil {
		return nil, fmt.Errorf("wordcount params: input is not a CID: %w", err)
	}
	blob, err := blobs.Open(ctx, input)
	if err != nil {
		return nil, fmt.Errorf("wordcount input %s: %w", input, err)
	}
	size := blob.Size()
	blob.Close()

	// Ranges are cut at arbitrary bytes. Execute's rule that a word belongs
	// to the range it starts in makes that safe.
	n := uint64(max(parts, 1))
	if size < n {
		n = max(size, 1)
	}
	payloads := make([][]byte, 0, n)
	for i := uint64(0); i < n; i++ {
		start := size/n*i + size%n*i/n
		end := size/n*(i+1) + size%n*(i+1)/n
		payloads = append(payloads, mustJSON(wordCountRange{Input: p.Input, Offset: start, Length: end - start}))
	}
	return payloads, nil
}

func (WordCount) Execute(ctx context.Context, blobs Blobs, payload []byte) ([]byte, error) {
	var r wordCountRange
	if err := json.Unmarshal(payload, &r); err != nil {
		return nil, fmt.Errorf("wordcount payload: %w", err)
	}
	input, err := cid.Decode(r.Input)
	if err != nil {
		return nil, fmt.Errorf("wordcount payload: input is not a CID: %w", err)
	}
	blob, err := blobs.Open(ctx, input)
	if err != nil {
		return nil, fmt.Errorf("wordcount input %s: %w", input, err)
	}
	defer blob.Close()

	counts, err := countWords(ctx, blob, r.Offset, r.Length)
	if err != nil {
		return nil, err
	}
	stored, err := blobs.Put(ctx, bytes.NewReader(mustJSON(counts)))
	if err != nil {
		return nil, fmt.Errorf("store word counts: %w", err)
	}
	return mustJSON(wordCountPart{Counts: stored.String()}), nil
}

func (WordCount) Aggregate(ctx context.Context, blobs Blobs, outputs [][]byte) ([]byte, error) {
	total := make(map[string]uint64)
	for i, output := range outputs {
		var part wordCountPart
		if err := json.Unmarshal(output, &part); err != nil {
			return nil, fmt.Errorf("task %d output: %w", i, err)
		}
		counts, err := cid.Decode(part.Counts)
		if err != nil {
			return nil, fmt.Errorf("task %d output: counts is not a CID: %w", i, err)
		}
		blob, err := blobs.Open(ctx, counts)
		if err != nil {
			return nil, fmt.Errorf("task %d counts %s: %w", i, counts, err)
		}
		var partial map[string]uint64
		err = json.NewDecoder(blob).Decode(&partial)
		blob.Close()
		if err != nil {
			return nil, fmt.Errorf("task %d counts %s: %w", i, counts, err)
		}
		for word, n := range partial {
			total[word] += n
		}
	}

	words := make([]string, 0, len(total))
	var result WordCountResult
	for word, n := range total {
		words = append(words, word)
		result.Words += n
	}
	result.Distinct = uint64(len(words))
	sort.Slice(words, func(i, j int) bool {
		if total[words[i]] != total[words[j]] {
			return total[words[i]] > total[words[j]]
		}
		return words[i] < words[j]
	})
	var table bytes.Buffer
	for _, word := range words {
		fmt.Fprintf(&table, "%d\t%s\n", total[word], word)
	}
	stored, err := blobs.Put(ctx, &table)
	if err != nil {
		return nil, fmt.Errorf("store word table: %w", err)
	}
	result.Output = stored.String()
	return mustJSON(result), nil
}

// countWords counts the words in src that start at a byte position in
// [offset, offset+length). A word that starts in the range is counted whole
// even if it runs past the end, and one that started before the range is
// left to the range it started in, so adjacent ranges together count every
// word exactly once.
func countWords(ctx context.Context, src io.ReadSeeker, offset, length uint64) (map[string]uint64, error) {
	counts := make(map[string]uint64)
	end := offset + length

	// Start one byte early to learn whether the range begins mid-word.
	start := offset
	if offset > 0 {
		start--
	}
	if _, err := src.Seek(int64(start), io.SeekStart); err != nil {
		return nil, err
	}
	r := bufio.NewReaderSize(src, 256<<10)
	skipping := false
	if offset > 0 {
		before, err := r.ReadByte()
		if errors.Is(err, io.EOF) {
			return counts, nil
		}
		if err != nil {
			return nil, err
		}
		skipping = isWordByte(before)
	}

	report := Report(ctx)
	report.Log(fmt.Sprintf("counting words in %d bytes from byte %d", length, offset))
	var word []byte
	inWord := false
	for pos := offset; pos < end || inWord; pos++ {
		if pos&(1<<20-1) == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			// Past the end only while finishing the last word.
			report.Progress(min(float64(pos-offset)/float64(max(length, 1)), 1))
		}
		b, err := r.ReadByte()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		switch {
		case !isWordByte(b):
			skipping = false
			if inWord {
				counts[wordKey(word)]++
				inWord = false
			}
		case skipping:
		case inWord:
			word = append(word, lower(b))
		default:
			inWord = true
			word = append(word[:0], lower(b))
		}
	}
	if inWord {
		counts[wordKey(word)]++
	}
	return counts, nil
}

func isWordByte(b byte) bool {
	return b >= 0x80 || b >= '0' && b <= '9' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z'
}

func lower(b byte) byte {
	if b >= 'A' && b <= 'Z' {
		return b + 'a' - 'A'
	}
	return b
}

// wordKey makes a word safe to use as a JSON object key, which must be valid
// UTF-8. Input that is not text would otherwise be altered differently
// depending on how it was encoded along the way.
func wordKey(word []byte) string {
	return strings.ToValidUTF8(string(word), "�")
}
