package replication

import (
	"testing"
	"time"

	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
)

// A follower can be sent the same questions twice: pushed on the stream and
// again in the reply to a report it sent meanwhile. It answers both. If the
// second answer lands after the first was judged and new questions were put,
// it must not count as a wrong answer to the new questions.
func TestARepeatedAnswerDoesNotFailQuestionsPutSince(t *testing.T) {
	r := newRig(t, 1, "one blob")
	one := put(t, r.store, "one blob")
	leaseID := r.BeginLease("follower")
	first := r.Replicate(ctx, "follower", &pb.ReplicateRequest{Holding: []string{one}, AnswersChallenges: true})
	r.RenewLease("follower", leaseID)
	stale := answers(t, r.store, first.GetChallenges())
	// The first answer is judged: shown.
	r.Replicate(ctx, "follower", &pb.ReplicateRequest{Holding: []string{one}, AnswersChallenges: true, Answers: stale})
	// The next proof deadline puts new questions.
	r.mu.Lock()
	r.Manager.followers["follower"].challengeAt = time.Now().Add(-time.Second)
	r.mu.Unlock()
	r.Manager.issueChallenges(ctx, time.Now())
	// The duplicate answer to the old questions arrives.
	r.Replicate(ctx, "follower", &pb.ReplicateRequest{Holding: []string{one}, AnswersChallenges: true, Answers: stale})
	r.mu.Lock()
	f := r.Manager.followers["follower"]
	_, failed := f.failed[one]
	_, held := f.held[one]
	pending := len(f.pending)
	r.mu.Unlock()
	if failed || !held {
		t.Errorf("a stale answer to old questions failed the new ones: failed=%v held=%v pending=%d", failed, held, pending)
	}
}
