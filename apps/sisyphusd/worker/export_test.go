package worker

import (
	"testing"
	"time"
)

// ShortenOffersWait has a worker give up sooner on a workload slow to say
// what it offers, for the length of a test.
func ShortenOffersWait(t *testing.T, d time.Duration) {
	t.Helper()
	was := offersWait
	offersWait = d
	t.Cleanup(func() { offersWait = was })
}
