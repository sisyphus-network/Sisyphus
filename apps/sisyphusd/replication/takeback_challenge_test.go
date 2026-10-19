package replication

import (
	"testing"
	"time"

	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
	"github.com/sisyphus-network/Sisyphus/packages/storage"
)

func TestSlowTakeBackDoesNotDelayOtherFollowersChallengeExpiry(t *testing.T) {
	r := newRig(t, 1, "pinned blob")
	pinned := put(t, r.store, "pinned blob")
	for _, caller := range []string{"slow", "zz-fast"} {
		response := r.Replicate(ctx, caller, &pb.ReplicateRequest{
			Holding: []string{pinned}, AnswersChallenges: true,
		})
		if len(response.GetChallenges()) != 1 {
			t.Fatalf("%s received %d challenges, want one", caller, len(response.GetChallenges()))
		}
	}

	oldCopies := storage.NewMemory()
	oldBlob := put(t, oldCopies, "restore during a slow network fetch")
	r.followers["slow"] = oldCopies
	oldList := &pb.KeepList{Store: "old", Sequence: 1, Blobs: []*pb.KeptBlob{{Cid: oldBlob}}}
	oldList.Signature = r.ident.Sign(signedBytes(oldList))
	enteredFetch, releaseFetch := make(chan struct{}), make(chan struct{})
	r.fetching = func() {
		close(enteredFetch)
		<-releaseFetch
	}
	defer func() {
		select {
		case <-releaseFetch:
		default:
			close(releaseFetch)
		}
	}()

	slowDone := make(chan struct{})
	go func() {
		defer close(slowDone)
		r.Replicate(ctx, "slow", &pb.ReplicateRequest{
			Store: "old", Holding: []string{pinned}, List: oldList, AnswersChallenges: true,
		})
	}()
	select {
	case <-enteredFetch:
	case <-time.After(eventualWait):
		t.Fatal("signed-list recovery did not enter its remote fetch")
	}

	r.mu.Lock()
	for _, caller := range []string{"slow", "zz-fast"} {
		r.Manager.followers[caller].pending[0].issued = time.Now().Add(-challengeWindow - time.Second)
	}
	r.mu.Unlock()
	expiredDone := make(chan struct{})
	go func() {
		defer close(expiredDone)
		r.Manager.expireChallenges(ctx, time.Now())
	}()

	select {
	case <-expiredDone:
	case <-time.After(eventualWait):
		t.Fatal("challenge expiry waited for a follower's network restore")
	}
	if got := r.Status("").GetChallengesFailed(); got != 2 {
		t.Errorf("while restore was still blocked, expired %d challenges, want both followers", got)
	}
	close(releaseFetch)
	select {
	case <-slowDone:
	case <-time.After(eventualWait):
		t.Fatal("signed-list recovery did not finish after releasing its fetch")
	}
}
