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

// ShortenSharingPoll has a worker that takes tasks only while its owner is
// away ask whether they are more often, for the length of a test.
func ShortenSharingPoll(t *testing.T, d time.Duration) {
	t.Helper()
	was := sharingPoll
	sharingPoll = d
	t.Cleanup(func() { sharingPoll = was })
}
