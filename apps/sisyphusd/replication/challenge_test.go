package replication

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ipfs/go-cid"

	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
	"github.com/sisyphus-network/Sisyphus/packages/storage"
)

// answers returns what a follower that holds, in holds, the blobs it is
// asked about would answer; about any other it says it cannot.
func answers(t *testing.T, holds *storage.Store, asked []*pb.Challenge) []*pb.ChallengeAnswer {
	t.Helper()
	var given []*pb.ChallengeAnswer
	for _, c := range asked {
		answer := &pb.ChallengeAnswer{Cid: c.GetCid(), Nonce: c.GetNonce()}
		if named, err := read(ctx, holds, c.GetCid(), c.GetOffset(), c.GetLength()); err == nil {
			answer.Digest = Answer(c.GetNonce(), named)
		}
		given = append(given, answer)
	}
	return given
}

// of returns what a status says of one blob: who is counted as holding it
// and who has shown that they do.
func (r *rig) of(id string) (holders, shown string) {
	for _, blob := range r.Status(id).GetBlobs() {
		return strings.Join(blob.GetHolders(), ","), strings.Join(blob.GetShown(), ",")
	}
	return "", ""
}

// cids lists the blobs some challenges are about, in order.
func cids(asked []*pb.Challenge) []string {
	var ids []string
	for _, c := range asked {
		ids = append(ids, c.GetCid())
	}
	return ids
}

// A coordinator does not take a follower's word for what it holds: it asks
// for a digest only a holder of the bytes could make, counts a copy as
// shown when it gets one, and does not count a copy at all when it does not.
func TestAFollowerIsAskedToShowThatItHoldsWhatItSaysItHolds(t *testing.T) {
	r := newRig(t, 1, "one blob", "another")
	one, another := put(t, r.store, "one blob"), put(t, r.store, "another")
	holding := []string{one, another}
	// The follower really holds the first, and says it holds both.
	honest := storage.NewMemory()
	put(t, honest, "one blob")

	asked := r.Replicate(ctx, "f1", &pb.ReplicateRequest{Holding: holding, AnswersChallenges: true})
	if got := cids(asked.GetChallenges()); len(got) != 2 || !slices.Contains(got, one) || !slices.Contains(got, another) {
		t.Fatalf("a follower that says it holds two blobs was asked about %v", got)
	}
	for _, c := range asked.GetChallenges() {
		if len(c.GetNonce()) != 16 || c.GetOffset() != 0 || (c.GetCid() == one && c.GetLength() != uint32(len("one blob"))) {
			t.Errorf("a question about a short blob: %v", c)
		}
	}
	// Until it answers it is taken at its word, as a follower always was.
	if holders, shown := r.of(another); holders != "f1" || shown != "" {
		t.Errorf("before it answers, the blob is held by %q and shown by %q", holders, shown)
	}

	again := r.Replicate(ctx, "f1", &pb.ReplicateRequest{Holding: holding, AnswersChallenges: true, Answers: answers(t, honest, asked.GetChallenges())})
	if len(again.GetChallenges()) != 0 {
		t.Errorf("a follower that answered was at once asked %d more questions", len(again.GetChallenges()))
	}
	if holders, shown := r.of(one); holders != "f1" || shown != "f1" {
		t.Errorf("the blob it holds is held by %q and shown by %q", holders, shown)
	}
	if holders, shown := r.of(another); holders != "" || shown != "" {
		t.Errorf("the blob it does not hold is held by %q and shown by %q", holders, shown)
	}
	if failed := r.Status("").GetChallengesFailed(); failed != 1 || r.logs.count("did not show that it holds a blob it says it holds") != 1 {
		t.Errorf("%d failures counted, %d logged", failed, r.logs.count("did not show that it holds"))
	}
	// Asked next time, what it failed to show comes first; having fetched
	// it, it shows it, and is counted again.
	put(t, honest, "another")
	asked = r.Replicate(ctx, "f1", &pb.ReplicateRequest{Holding: holding, AnswersChallenges: true})
	if got := cids(asked.GetChallenges()); len(got) != 2 || got[0] != another {
		t.Fatalf("the blob it failed to show was not asked about first: %v", got)
	}
	r.Replicate(ctx, "f1", &pb.ReplicateRequest{Holding: holding, AnswersChallenges: true, Answers: answers(t, honest, asked.GetChallenges())})
	if holders, shown := r.of(another); holders != "f1" || shown != "f1" {
		t.Errorf("once shown, the blob is held by %q and shown by %q", holders, shown)
	}
	if failed := r.Status("").GetChallengesFailed(); failed != 1 {
		t.Errorf("%d failures counted after it made good", failed)
	}

	// A follower from before there were questions is asked none, and is
	// taken at its word.
	if old := r.Replicate(ctx, "f2", &pb.ReplicateRequest{Holding: holding}); len(old.GetChallenges()) != 0 {
		t.Errorf("a follower that does not answer questions was asked %d", len(old.GetChallenges()))
	}
	if holders, shown := r.of(one); holders != "f1,f2" || shown != "f1" {
		t.Errorf("with such a follower too, the blob is held by %q and shown by %q", holders, shown)
	}
}

// A question stands until it is answered or its time is up. Asked again
// meanwhile, a follower is put the same question; out of time, it has
// failed; and an answer that comes late is no answer.
func TestAQuestionIsAnsweredInItsTimeOrNotAtAll(t *testing.T) {
	r := newRig(t, 1, "one blob")
	one := put(t, r.store, "one blob")
	req := func(given ...*pb.ChallengeAnswer) *pb.ReplicateRequest {
		return &pb.ReplicateRequest{Holding: []string{one}, AnswersChallenges: true, Answers: given}
	}
	late := func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		for i := range r.Manager.followers["f1"].pending {
			r.Manager.followers["f1"].pending[i].issued = time.Now().Add(-2 * challengeWindow)
		}
	}

	first := r.Replicate(ctx, "f1", req()).GetChallenges()
	second := r.Replicate(ctx, "f1", req()).GetChallenges()
	if len(first) != 1 || len(second) != 1 || string(first[0].GetNonce()) != string(second[0].GetNonce()) {
		t.Fatalf("asked again within its time, a follower was put %v and then %v", first, second)
	}
	if failed := r.Status("").GetChallengesFailed(); failed != 0 {
		t.Errorf("%d failures counted while the question stood", failed)
	}

	// Its time runs out with no answer.
	late()
	third := r.Replicate(ctx, "f1", req()).GetChallenges()
	if holders, _ := r.of(one); holders != "" || r.Status("").GetChallengesFailed() != 1 {
		t.Errorf("with a question unanswered, the blob is held by %q after %d failures", holders, r.Status("").GetChallengesFailed())
	}
	if len(third) != 1 || string(third[0].GetNonce()) == string(first[0].GetNonce()) {
		t.Fatalf("after a question ran out, a follower was put %v", third)
	}

	// The right answer, too late.
	late()
	r.Replicate(ctx, "f1", req(answers(t, r.store, third)...))
	if holders, shown := r.of(one); holders != "" || shown != "" || r.Status("").GetChallengesFailed() != 2 {
		t.Errorf("after a late answer, the blob is held by %q and shown by %q", holders, shown)
	}
	// The right answer to another question, or under another number.
	fourth := r.Replicate(ctx, "f1", req()).GetChallenges()
	stale := answers(t, r.store, fourth)
	stale[0].Nonce = third[0].GetNonce()
	r.Replicate(ctx, "f1", req(stale...))
	if holders, _ := r.of(one); holders != "" {
		t.Errorf("after an answer under another question's number, the blob is held by %q", holders)
	}
}

// unreadable is a store some of whose blobs cannot be opened.
type unreadable struct {
	*storage.Store
	lost map[string]bool
}

func (u unreadable) Open(ctx context.Context, c cid.Cid) (storage.Blob, error) {
	if u.lost[c.String()] {
		return nil, errors.New("the disk gave an error")
	}
	return u.Store.Open(ctx, c)
}

// A follower is asked about a few blobs at a time: those it has never shown
// before those it has, those shown longest ago before the rest, and never
// about what this node has not pinned or cannot read to check an answer.
func TestWhichBlobsAFollowerIsAskedAbout(t *testing.T) {
	texts := []string{"one", "two", "three", "four", "five", "six"}
	r := newRig(t, 1, texts...)
	lost := map[string]bool{}
	cfg := r.config(1)
	cfg.Store = unreadable{r.store, lost}
	r.Manager = New(cfg)
	var holding []string
	for _, text := range texts {
		holding = append(holding, put(t, r.store, text))
	}
	stray := put(t, r.store, "held by the follower, pinned by nobody")
	// A long blob is asked about a part at a time.
	long := keep(t, r.store, strings.Repeat("0123456789abcdef", 8192))
	req := func(given ...*pb.ChallengeAnswer) *pb.ReplicateRequest {
		return &pb.ReplicateRequest{Holding: append(slices.Clone(holding), stray, "not a CID at all"), AnswersChallenges: true, Answers: given}
	}

	seen := map[string]bool{}
	first := r.Replicate(ctx, "f1", req()).GetChallenges()
	if len(first) != challengesEach {
		t.Fatalf("a follower holding six blobs was asked about %d", len(first))
	}
	for _, c := range first {
		seen[c.GetCid()] = true
		if c.GetCid() == stray || !slices.Contains(holding, c.GetCid()) {
			t.Errorf("a follower was asked about %s, which is not pinned", c.GetCid())
		}
	}
	r.Replicate(ctx, "f1", req(answers(t, r.store, first)...))
	// The two it was not asked about come first the next time.
	second := r.Replicate(ctx, "f1", req()).GetChallenges()
	if len(second) != challengesEach || seen[second[0].GetCid()] || seen[second[1].GetCid()] || !seen[second[2].GetCid()] {
		t.Errorf("having shown four, a follower was next asked about %v", cids(second))
	}
	r.Replicate(ctx, "f1", req(answers(t, r.store, second)...))
	// With all six shown, the two shown longest ago come first.
	longest := map[string]bool{}
	for _, c := range first {
		longest[c.GetCid()] = !slices.Contains(cids(second), c.GetCid())
	}
	time.Sleep(2 * time.Millisecond)
	if round := r.Replicate(ctx, "f1", req()).GetChallenges(); len(round) != challengesEach || !longest[round[0].GetCid()] || !longest[round[1].GetCid()] {
		t.Errorf("with all shown, a follower was asked about %v, and not first about the two shown longest ago", cids(round))
	} else {
		r.Replicate(ctx, "f1", req(answers(t, r.store, round)...))
	}

	// A blob this node cannot read is not asked about, and an answer about
	// one it could read when it asked and cannot now is neither right nor
	// wrong.
	holding = []string{long, holding[0]}
	third := r.Replicate(ctx, "f1", req()).GetChallenges()
	if len(third) != 2 || third[0].GetCid() != long || third[0].GetLength() != challengeSpan || third[0].GetOffset() > uint64(16*8192-challengeSpan) {
		t.Fatalf("a follower with a long blob it has never shown was asked %v", third)
	}
	lost[long] = true
	r.Replicate(ctx, "f1", req(answers(t, r.store, third)...))
	if holders, shown := r.of(long); holders != "f1" || shown != "" || r.Status("").GetChallengesFailed() != 0 {
		t.Errorf("a blob this node cannot read is held by %q and shown by %q, after %d failures", holders, shown, r.Status("").GetChallengesFailed())
	}
	if fourth := r.Replicate(ctx, "f1", req()).GetChallenges(); len(fourth) != 1 || fourth[0].GetCid() == long {
		t.Errorf("a follower was asked about a blob this node cannot read: %v", cids(fourth))
	}
}

func TestReadingTheBytesAQuestionNames(t *testing.T) {
	store := storage.NewMemory()
	id := put(t, store, "0123456789")
	if named, err := read(ctx, store, id, 3, 4); err != nil || string(named) != "3456" {
		t.Errorf("four bytes from the fourth: %q, %v", named, err)
	}
	for name, tt := range map[string]struct {
		id     string
		offset uint64
		length uint32
	}{
		"what is no CID":          {"not a CID", 0, 1},
		"a blob that is not held": {put(t, storage.NewMemory(), "elsewhere"), 0, 1},
		"more than there is":      {id, 8, 4},
		"an offset past any blob": {id, 1 << 63, 1},
	} {
		if _, err := read(ctx, store, tt.id, tt.offset, tt.length); err == nil {
			t.Errorf("reading %s gave no error", name)
		}
	}
	if string(Answer([]byte("n"), []byte("bytes"))) == string(Answer([]byte("m"), []byte("bytes"))) {
		t.Error("two numbers gave one answer")
	}
}

// unopenable is a follower's store that cannot read some of what it has.
type unopenable struct {
	*storage.Store
	lost map[string]bool
}

func (u unopenable) Open(ctx context.Context, c cid.Cid) (storage.Blob, error) {
	if u.lost[c.String()] {
		return nil, errors.New("the disk gave an error")
	}
	return u.Store.Open(ctx, c)
}

// Asked to show that it holds a blob, a follower answers at once with what
// only a holder could make. One it finds it cannot read, it says so of, lets
// go of, and fetches again.
func TestAFollowerAnswersWhatItIsAskedAndFetchesAgainWhatItCannotRead(t *testing.T) {
	coordinator, f, copies, logs := following(t)
	kept, spoiled := put(t, coordinator.store, "a blob the follower keeps"), put(t, coordinator.store, "a blob its disk loses")
	coordinator.say("node/first", nil, kept, spoiled)
	if _, err := f.sync(ctx); err != nil {
		t.Fatal(err)
	}
	if !holds(t, copies, kept) || !holds(t, copies, spoiled) {
		t.Fatal("the follower did not fetch what it was told to hold")
	}

	lost := map[string]bool{spoiled: true}
	f.Store = unopenable{copies, lost}
	coordinator.ask(
		&pb.Challenge{Cid: kept, Offset: 2, Length: 4, Nonce: []byte("a number used once")},
		&pb.Challenge{Cid: spoiled, Offset: 0, Length: 1, Nonce: []byte("another")},
		&pb.Challenge{Cid: "not a CID at all", Nonce: []byte("a third")},
	)
	before, _ := coordinator.requests()
	changed, err := f.sync(ctx)
	if err != nil || !changed {
		t.Fatalf("a follower that let go of a blob and fetched it again: changed %v, %v", changed, err)
	}
	// It asked, was put the questions, and asked again at once with its answers.
	after, last := coordinator.requests()
	if after != before+2 || !last.GetAnswersChallenges() || len(last.GetAnswers()) != 3 {
		t.Fatalf("the follower asked %d times, the last with %d answers", after-before, len(last.GetAnswers()))
	}
	given := map[string]*pb.ChallengeAnswer{}
	for _, a := range last.GetAnswers() {
		given[a.GetCid()] = a
	}
	if want := Answer([]byte("a number used once"), []byte("blob")); string(given[kept].GetDigest()) != string(want) || string(given[kept].GetNonce()) != "a number used once" {
		t.Errorf("of the blob it holds it answered %x", given[kept].GetDigest())
	}
	if len(given[spoiled].GetDigest()) != 0 || len(given["not a CID at all"].GetDigest()) != 0 {
		t.Error("it gave an answer about what it could not read")
	}
	if logs.count("cannot read a blob it holds for its pool, and lets go of it to fetch it again") != 2 {
		t.Errorf("what it could not read was logged %d times", logs.count("lets go of it to fetch it again"))
	}
	// What it let go of it was still to hold, so it fetched it again.
	lost[spoiled] = false
	if !holds(t, copies, spoiled) || !slices.Contains(pinsOf(copies), "pool:node/first "+spoiled) {
		t.Error("the blob it could not read was not fetched again and held")
	}
}
