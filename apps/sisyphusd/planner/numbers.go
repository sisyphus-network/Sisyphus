package planner

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/sisyphus-network/Sisyphus/packages/ai"
)

// A small model asked to compare two of the pool's results may copy one of
// them wrong and work out the difference from its own copy: in the V0 run,
// 7,573,900,598 came back as 7,573,905,598, followed by a negative count of
// primes (#532). So the planner does the arithmetic itself, handing the model
// each comparison ready to quote, and checks the numbers in the answer
// against those the pool returned.

// figure is one whole number that a job returned, or one the planner worked
// out from two of them.
type figure struct {
	job, field string
	value      int64
}

// figuresIn is every whole number in the job results that tools returned in
// messages, with the differences the planner gave alongside them.
func figuresIn(messages []ai.Message) []figure {
	var figures []figure
	for _, m := range messages {
		if m.Role != ai.ToolRole {
			continue
		}
		var said struct {
			JobID    string          `json:"job_id"`
			Result   json.RawMessage `json:"result"`
			Compared []struct {
				Job        string `json:"job_id"`
				Field      string `json:"field"`
				Difference int64  `json:"difference"`
			} `json:"compared_with"`
		}
		if json.Unmarshal([]byte(m.Content), &said) != nil || said.JobID == "" {
			continue
		}
		for field, value := range wholeNumbers(said.Result) {
			figures = append(figures, figure{job: said.JobID, field: field, value: value})
		}
		for _, c := range said.Compared {
			figures = append(figures, figure{job: said.JobID, field: "difference from " + c.Job, value: abs(c.Difference)})
		}
	}
	return figures
}

// wholeNumbers is the top-level fields of a JSON object that are whole
// numbers.
func wholeNumbers(result json.RawMessage) map[string]int64 {
	decoder := json.NewDecoder(bytes.NewReader(result))
	decoder.UseNumber()
	var fields map[string]any
	if decoder.Decode(&fields) != nil {
		return nil
	}
	out := make(map[string]int64)
	for field, value := range fields {
		if number, is := value.(json.Number); is {
			if whole, err := number.Int64(); err == nil {
				out[field] = whole
			}
		}
	}
	return out
}

// withComparisons adds to a job's result, as a tool returned it, how each of
// its whole numbers compares with the same field of the other jobs already
// in the conversation: the difference, and words to say it in.
func withComparisons(result string, earlier []figure) string {
	var out map[string]any
	decoder := json.NewDecoder(strings.NewReader(result))
	decoder.UseNumber()
	if decoder.Decode(&out) != nil {
		return result
	}
	job, _ := out["job_id"].(string)
	raw, _ := json.Marshal(out["result"]) // a value decoded from JSON always encodes
	var compared []map[string]any
	fields := wholeNumbers(raw)
	for _, field := range sortedKeys(fields) {
		value := fields[field]
		for _, e := range earlier {
			if e.job == job || e.field != field {
				continue
			}
			difference, held := subtract(value, e.value)
			if !held {
				// A result may be any whole number; one whose
				// difference would overflow is left uncompared.
				continue
			}
			compared = append(compared, map[string]any{
				"job_id": e.job, "field": field, "this": value, "earlier": e.value, "difference": difference,
				"in_words": inWords(field, difference, e),
			})
		}
	}
	if len(compared) == 0 {
		return result
	}
	out["compared_with"] = compared
	encoded, _ := json.Marshal(out) // decoded JSON with plain additions always encodes
	return string(encoded)
}

// inWords says how a result compares with an earlier one, so that the model
// need not, and gets neither the sign nor the subtraction wrong.
func inWords(field string, difference int64, earlier figure) string {
	more, less := "more", "less"
	if field == "count" {
		more, less = "more", "fewer"
	}
	switch {
	case difference > 0:
		return fmt.Sprintf("%s %s than job %s's %s", grouped(difference), more, earlier.job, grouped(earlier.value))
	case difference < 0:
		return fmt.Sprintf("%s %s than job %s's %s", grouped(-difference), less, earlier.job, grouped(earlier.value))
	}
	return fmt.Sprintf("the same as job %s's %s", earlier.job, grouped(earlier.value))
}

// numberInText is a whole number as an answer writes one, with or without
// groups of three digits set apart, and any decimals after it.
var numberInText = regexp.MustCompile(`\d{1,3}(?:[,\x{202f}\x{a0} ]\d{3})+(?:\.\d+)?|\d+(?:\.\d+)?`)

// misquoted is a note on each number in an answer that is nearly, but not,
// one of the figures the pool returned in this run. A number of five digits
// or more that differs from one figure in one or two digits is taken to be a
// copy of it gone wrong; one near several is pointed out with all of them.
// Nothing in the answer is changed: the note says which figure is right.
func misquoted(answer string, figures []figure) string {
	var notes []string
	for _, written := range numberInText.FindAllString(answer, -1) {
		if strings.Contains(written, ".") {
			continue
		}
		digits := strings.Map(func(r rune) rune {
			if r >= '0' && r <= '9' {
				return r
			}
			return -1
		}, written)
		if len(digits) < 5 || slices.ContainsFunc(figures, func(f figure) bool { return strconv.FormatInt(f.value, 10) == digits }) {
			continue
		}
		var near []figure
		for _, f := range figures {
			if nearly(digits, strconv.FormatInt(f.value, 10)) {
				near = append(near, f)
			}
		}
		switch {
		case len(near) == 1:
			notes = append(notes, fmt.Sprintf("the answer says %s, but job %s returned %s", written, near[0].job, grouped(near[0].value)))
		case len(near) > 1:
			var each []string
			for _, f := range near {
				each = append(each, fmt.Sprintf("%s from job %s", grouped(f.value), f.job))
			}
			notes = append(notes, fmt.Sprintf("the answer says %s, which is none of the results (%s)", written, strings.Join(each, ", ")))
		}
	}
	if len(notes) == 0 {
		return ""
	}
	return "\n\nNote: " + strings.Join(notes, "; ") + "."
}

// nearly reports whether two numbers written out have as many digits and
// differ in one or two of them.
func nearly(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	differ := 0
	for i := range a {
		if a[i] != b[i] {
			differ++
		}
	}
	return differ > 0 && differ <= 2
}

// grouped writes a whole number with commas between groups of three digits.
func grouped(n int64) string {
	sign, digits := "", strconv.FormatInt(n, 10)
	if n < 0 {
		sign, digits = "-", digits[1:]
	}
	var out strings.Builder
	for i, d := range digits {
		if i > 0 && (len(digits)-i)%3 == 0 {
			out.WriteByte(',')
		}
		out.WriteRune(d)
	}
	return sign + out.String()
}

// subtract is a-b, and whether it is held without overflowing. MinInt64 is
// taken not to be, since it has no positive counterpart to be quoted as
// "less" or "fewer".
func subtract(a, b int64) (int64, bool) {
	d := a - b
	if (b > 0 && d > a) || (b < 0 && d < a) || d == math.MinInt64 {
		return 0, false
	}
	return d, true
}

func abs(n int64) int64 {
	if n < 0 {
		return -n
	}
	return n
}

func sortedKeys(m map[string]int64) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
