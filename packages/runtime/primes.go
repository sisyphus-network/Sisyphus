package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
)

// Primes counts the prime numbers in a half-open range [from, to). It exists
// to exercise the network: it splits cleanly by sub-range, is deterministic,
// and any task can be checked by running it again.
type Primes struct{}

// PrimesRange is both the job parameters and a task payload.
type PrimesRange struct {
	From uint64 `json:"from"`
	To   uint64 `json:"to"`
}

// PrimesCount is both a task output and the job result.
type PrimesCount struct {
	Count uint64 `json:"count"`
}

// maxPrimesTo keeps the base sieve, which holds every prime up to sqrt(to),
// small enough to build in memory.
const maxPrimesTo = 1 << 40

func (Primes) Name() string { return "primes" }

func (Primes) Split(_ context.Context, _ Blobs, params []byte, parts int) ([][]byte, error) {
	r, err := parsePrimesRange(params)
	if err != nil {
		return nil, err
	}
	n := uint64(max(parts, 1))
	span := r.To - r.From
	if span < n {
		n = max(span, 1)
	}
	payloads := make([][]byte, 0, n)
	for i := uint64(0); i < n; i++ {
		// Computing each bound from the full span spreads the remainder
		// instead of leaving it all in the last task.
		sub := PrimesRange{From: r.From + span/n*i + span%n*i/n, To: r.From + span/n*(i+1) + span%n*(i+1)/n}
		payloads = append(payloads, mustJSON(sub))
	}
	return payloads, nil
}

func (Primes) Describe() string {
	return `Counts the prime numbers in a range. Parameters: {"from": <integer>, "to": <integer>}, counting from "from" up to but not including "to". Result: {"count": <integer>}.`
}

func (Primes) Execute(ctx context.Context, _ Blobs, payload []byte) ([]byte, error) {
	r, err := parsePrimesRange(payload)
	if err != nil {
		return nil, err
	}
	count, err := countPrimes(ctx, r.From, r.To)
	if err != nil {
		return nil, err
	}
	return mustJSON(PrimesCount{Count: count}), nil
}

func (Primes) Aggregate(_ context.Context, _ Blobs, outputs [][]byte) ([]byte, error) {
	var total PrimesCount
	for i, output := range outputs {
		var c PrimesCount
		if err := json.Unmarshal(output, &c); err != nil {
			return nil, fmt.Errorf("task %d output: %w", i, err)
		}
		total.Count += c.Count
	}
	return mustJSON(total), nil
}

func parsePrimesRange(b []byte) (PrimesRange, error) {
	var r PrimesRange
	if err := json.Unmarshal(b, &r); err != nil {
		return r, fmt.Errorf("primes range: %w", err)
	}
	if r.To < r.From {
		return r, errors.New("primes range: to is less than from")
	}
	if r.To > maxPrimesTo {
		return r, fmt.Errorf("primes range: to exceeds %d", uint64(maxPrimesTo))
	}
	return r, nil
}

// countPrimes counts primes in [from, to) with a segmented sieve.
func countPrimes(ctx context.Context, from, to uint64) (uint64, error) {
	from = max(from, 2)
	if to <= from {
		return 0, nil
	}
	base := primesUpTo(isqrt(to - 1))

	const block = 1 << 18
	composite := make([]bool, block)
	var count uint64
	report := Report(ctx)
	report.Log(fmt.Sprintf("counting primes from %d to %d", from, to))
	for lo := from; lo < to; lo += block {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		report.Progress(float64(lo-from) / float64(to-from))
		hi := min(lo+block, to)
		seg := composite[:hi-lo]
		clear(seg)
		for _, p := range base {
			if p*p >= hi {
				break
			}
			for m := max(p*p, (lo+p-1)/p*p); m < hi; m += p {
				seg[m-lo] = true
			}
		}
		for _, c := range seg {
			if !c {
				count++
			}
		}
	}
	return count, nil
}

// primesUpTo returns every prime <= n.
func primesUpTo(n uint64) []uint64 {
	composite := make([]bool, n+1)
	var primes []uint64
	for i := uint64(2); i <= n; i++ {
		if composite[i] {
			continue
		}
		primes = append(primes, i)
		for m := i * i; m <= n; m += i {
			composite[m] = true
		}
	}
	return primes
}

// isqrt returns the integer part of the square root of n. It is exact for
// every n up to maxPrimesTo, which float64 represents with room to spare;
// it must not be used for values near the top of the uint64 range.
func isqrt(n uint64) uint64 {
	return uint64(math.Sqrt(float64(n)))
}
