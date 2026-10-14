package replication

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/ipfs/go-cid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
	"github.com/sisyphus-network/Sisyphus/packages/storage"
)

// scripted is a coordinator that tells its follower whatever a test has
// it say, and serves blobs from a store.
type scripted struct {
	pb.UnimplementedBlobServiceServer
	store *storage.Store

	mu sync.Mutex
	// hold and name are what it answers with, and err, if set, what it
	// fails with instead.
	hold []string
	name string
	err  error
	// list, if set, is the signed list it answers with, and asks is whether
	// it asks a follower that shows no list for the one it has.
	list *pb.KeepList
	asks bool
	// questions, if set, are put to a follower that asks without answers.
	questions []*pb.Challenge
	// asked holds every request it has had.
	asked []*pb.ReplicateRequest
}

func (s *scripted) Replicate(_ context.Context, req *pb.ReplicateRequest) (*pb.ReplicateResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.asked = append(s.asked, req)
	if s.err != nil {
		return nil, s.err
	}
	var questions []*pb.Challenge
	if len(req.GetAnswers()) == 0 {
		questions = s.questions
	}
	return &pb.ReplicateResponse{Hold: s.hold, Store: s.name, List: s.list, ShowList: s.asks && req.GetList() == nil, Challenges: questions}, nil
}

// ask has the coordinator put these questions from now on.
func (s *scripted) ask(questions ...*pb.Challenge) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.questions = questions
}

func (s *scripted) Get(req *pb.GetBlobRequest, stream grpc.ServerStreamingServer[pb.GetBlobResponse]) error {
	blob, err := s.store.Open(stream.Context(), cid.MustParse(req.GetCid()))
	if err != nil {
		return status.Errorf(codes.NotFound, "%v", err)
	}
	defer blob.Close()
	data, err := io.ReadAll(blob)
	if err != nil {
		return err
	}
	return stream.Send(&pb.GetBlobResponse{Data: data})
}

// say sets what the coordinator answers from now on.
func (s *scripted) say(name string, err error, hold ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.name, s.err, s.hold, s.list = name, err, hold, nil
}

// sign has the coordinator answer from now on with a list of the given
// blobs, as one that signs what it says does.
func (s *scripted) sign(name string, hold ...string) *pb.KeepList {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.name, s.err, s.hold, s.list = name, nil, nil, &pb.KeepList{Store: name, Sequence: uint64(len(s.asked)) + 1, Signature: []byte("signed")}
	for _, id := range hold {
		s.list.Blobs = append(s.list.Blobs, &pb.KeptBlob{Cid: id})
	}
	return s.list
}

// requests returns how many times the coordinator has been asked, and the
// last request.
func (s *scripted) requests() (int, *pb.ReplicateRequest) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.asked) == 0 {
		return 0, nil
	}
	return len(s.asked), s.asked[len(s.asked)-1]
}

// following returns a scripted coordinator and a follower of it with an
// empty store.
func following(t *testing.T) (*scripted, *Follower, *storage.Store, *logged) {
	t.Helper()
	coordinator := &scripted{store: storage.NewMemory(), name: "node/first"}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	pb.RegisterBlobServiceServer(srv, coordinator)
	go srv.Serve(lis)
	t.Cleanup(srv.Stop)
	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	copies, logs := storage.NewMemory(), new(logged)
	return coordinator, &Follower{Store: copies, ListFile: filepath.Join(t.TempDir(), "kept.list"), Coordinator: pb.NewBlobServiceClient(conn), Log: logs.logger()}, copies, logs
}

// run runs a follower, asking every interval, until the test ends or the
// returned function is called.
func run(t *testing.T, f *Follower, interval time.Duration) (stop func()) {
	t.Helper()
	usual := Interval
	Interval = interval
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.Run(ctx)
	}()
	stop = sync.OnceFunc(func() {
		cancel()
		<-done
		Interval = usual
	})
	t.Cleanup(stop)
	return stop
}

func eventually(t *testing.T, what string, condition func() bool) {
	t.Helper()
	for deadline := time.Now().Add(30 * time.Second); !condition(); time.Sleep(2 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("never happened: %s", what)
		}
	}
}

// pinsOf lists a store's pins as "owner cid", leaving out the short pin
// every new blob gets.
func pinsOf(store *storage.Store) []string {
	var pins []string
	for _, pin := range store.Pins() {
		if pin.Owner != storage.GraceOwner {
			pins = append(pins, pin.Owner+" "+pin.CID.String())
		}
	}
	slices.Sort(pins)
	return pins
}

func holds(t *testing.T, store *storage.Store, id string) bool {
	t.Helper()
	has, err := store.Has(ctx, cid.MustParse(id))
	if err != nil {
		t.Fatal(err)
	}
	return has
}

func TestAFollowerFetchesWhatItIsToldToHoldAndDropsTheRest(t *testing.T) {
	coordinator, follower, copies, _ := following(t)
	first, second := put(t, coordinator.store, "the first blob"), put(t, coordinator.store, "the second blob")
	// Something else in the store, which is none of the pool's.
	other := keep(t, copies, "kept here for another reason")
	coordinator.say("node/first", nil, first, second)

	// It asks once a day, and again at once when what it holds has changed.
	run(t, follower, 24*time.Hour)
	eventually(t, "the follower says it holds both blobs", func() bool {
		_, last := coordinator.requests()
		return len(last.GetHolding()) == 2
	})
	asked, last := coordinator.requests()
	want := []string{first, second}
	slices.Sort(want)
	if asked != 2 || !slices.Equal(last.GetHolding(), want) || last.GetStore() != "node/first" {
		t.Errorf("after %d requests the follower says it holds %v for %q, want both blobs for the store named, at the second", asked, last.GetHolding(), last.GetStore())
	}
	wantPins := []string{"pool:node/first " + first, "pool:node/first " + second, "user " + other}
	slices.Sort(wantPins)
	if pins := pinsOf(copies); !slices.Equal(pins, wantPins) {
		t.Errorf("the follower's pins are %v, want %v", pins, wantPins)
	}
	time.Sleep(50 * time.Millisecond)
	if asked, _ := coordinator.requests(); asked != 2 {
		t.Errorf("with nothing changing the follower asked %d times in all, want 2", asked)
	}
}

func TestAFollowerDeletesWhatItIsNoLongerToHold(t *testing.T) {
	coordinator, follower, copies, _ := following(t)
	stays, goes := put(t, coordinator.store, "stays"), put(t, coordinator.store, "goes")
	coordinator.say("node/first", nil, stays, goes)
	if changed, err := follower.sync(ctx); err != nil || !changed {
		t.Fatalf("fetching two blobs: changed %v, error %v", changed, err)
	}
	if changed, err := follower.sync(ctx); err != nil || changed {
		t.Fatalf("asking again with nothing to do: changed %v, error %v", changed, err)
	}

	coordinator.say("node/first", nil, stays)
	if changed, err := follower.sync(ctx); err != nil || !changed {
		t.Fatalf("dropping a blob: changed %v, error %v", changed, err)
	}
	if holds(t, copies, goes) || !holds(t, copies, stays) {
		t.Errorf("after one blob was released the follower holds the released one: %v, the other: %v", holds(t, copies, goes), holds(t, copies, stays))
	}
	if pins := pinsOf(copies); !slices.Equal(pins, []string{"pool:node/first " + stays}) {
		t.Errorf("the follower's pins are %v", pins)
	}
}

func TestAFollowerKeepsWhatItHasWhileItsCoordinatorCannotBeAsked(t *testing.T) {
	coordinator, follower, copies, logs := following(t)
	held := put(t, coordinator.store, "held through an outage")
	coordinator.say("node/first", nil, held)
	if _, err := follower.sync(ctx); err != nil {
		t.Fatal(err)
	}

	coordinator.say("", status.Error(codes.Unavailable, "the coordinator is away"))
	stop := run(t, follower, 5*time.Millisecond)
	eventually(t, "the follower asks several times", func() bool {
		asked, _ := coordinator.requests()
		return asked >= 5
	})
	if !holds(t, copies, held) || len(pinsOf(copies)) != 1 {
		t.Errorf("with its coordinator away the follower's pins are %v", pinsOf(copies))
	}
	if n := logs.count("could not bring this node's copies"); n != 1 {
		t.Errorf("an outage was logged %d times, want once for the lot", n)
	}

	// When the coordinator is back the follower carries on, and a second
	// outage is news again.
	coordinator.say("node/first", nil, held)
	before, _ := coordinator.requests()
	eventually(t, "the follower asks again", func() bool {
		asked, _ := coordinator.requests()
		return asked >= before+2
	})
	coordinator.say("", status.Error(codes.Unavailable, "the coordinator is away again"))
	eventually(t, "the second outage is logged", func() bool { return logs.count("could not bring this node's copies") == 2 })
	stop()
	if !holds(t, copies, held) {
		t.Error("the follower dropped a blob while its coordinator was away")
	}
}

func TestAFollowerHoldsWhatItCanWhenSomeOfItCannotBeFetched(t *testing.T) {
	coordinator, follower, copies, logs := following(t)
	there := put(t, coordinator.store, "the coordinator has this one")
	missing := put(t, storage.NewMemory(), "and has lost this one")
	coordinator.say("node/first", nil, missing, "not-a-cid", there)

	changed, err := follower.sync(ctx)
	if err != nil || !changed {
		t.Fatalf("fetching one blob of three: changed %v, error %v", changed, err)
	}
	if pins := pinsOf(copies); !slices.Equal(pins, []string{"pool:node/first " + there}) {
		t.Errorf("the follower's pins are %v, want only the blob it could fetch", pins)
	}
	if n := logs.count("could not fetch a blob this node is to hold"); n != 2 {
		t.Errorf("%d failed fetches were logged, want 2", n)
	}
}

func TestAFollowerHoldsItsCopiesForTheStoreItsCoordinatorNames(t *testing.T) {
	coordinator, follower, copies, _ := following(t)
	blob, other := put(t, coordinator.store, "held for one store, then the next"), put(t, coordinator.store, "another")
	coordinator.say("node/first", nil, blob, other)
	if _, err := follower.sync(ctx); err != nil {
		t.Fatal(err)
	}
	// A change of name that was cut short left one blob held under both.
	if err := copies.Pin(ctx, "pool:node/second", time.Time{}, cid.MustParse(blob)); err != nil {
		t.Fatal(err)
	}

	coordinator.say("node/second", nil, blob, other)
	changed, err := follower.sync(ctx)
	if err != nil || changed {
		t.Fatalf("taking up a new name for the store: changed %v, error %v", changed, err)
	}
	want := []string{"pool:node/second " + blob, "pool:node/second " + other}
	slices.Sort(want)
	if pins := pinsOf(copies); !slices.Equal(pins, want) {
		t.Errorf("the follower's pins are %v, want both blobs held for the second store only", pins)
	}
	if !holds(t, copies, blob) || !holds(t, copies, other) {
		t.Error("a blob was deleted in changing the name it is held under")
	}
	if _, err := follower.sync(ctx); err != nil {
		t.Fatal(err)
	}
	if _, last := coordinator.requests(); last.GetStore() != "node/second" || len(last.GetHolding()) != 2 {
		t.Errorf("the follower now says it holds %v for %q", last.GetHolding(), last.GetStore())
	}
}

// readOnly is a store that cannot record a pin.
type readOnly struct{ *storage.Store }

var errReadOnly = errors.New("the pin file is read-only")

func (readOnly) Pin(context.Context, string, time.Time, ...cid.Cid) error { return errReadOnly }

func TestAFollowerWhoseStoreFailsSaysSoAndDoesNotAskInARush(t *testing.T) {
	coordinator, follower, copies, logs := following(t)
	follower.Store = readOnly{copies}
	blob := put(t, coordinator.store, "fetched, and then not pinned")
	coordinator.say("node/first", nil, blob)

	if _, err := follower.sync(ctx); !errors.Is(err, errReadOnly) {
		t.Fatalf("with a store that cannot pin: %v", err)
	}
	run(t, follower, 24*time.Hour)
	eventually(t, "the failure is logged", func() bool { return logs.count("the pin file is read-only") == 1 })
	time.Sleep(50 * time.Millisecond)
	if asked, _ := coordinator.requests(); asked != 2 {
		t.Errorf("the follower has asked %d times, want once by hand and once more: a fetch that could not be kept is no reason to ask again at once", asked)
	}
}

func TestAFollowerKeepsTheLastListItsCoordinatorSigned(t *testing.T) {
	coordinator, follower, copies, _ := following(t)
	first, second := put(t, coordinator.store, "on the first list"), put(t, coordinator.store, "on both lists")
	if list, err := loadList(follower.ListFile); list != nil || err != nil {
		t.Fatalf("before it has asked the follower keeps %v, error %v", list, err)
	}

	signed := coordinator.sign("node/first", first, second)
	if changed, err := follower.sync(ctx); err != nil || !changed {
		t.Fatalf("fetching what a list names: changed %v, error %v", changed, err)
	}
	want := []string{"pool:node/first " + first, "pool:node/first " + second}
	slices.Sort(want)
	if pins := pinsOf(copies); !slices.Equal(pins, want) {
		t.Errorf("the follower's pins are %v, want the two blobs the list names", pins)
	}
	if kept, err := loadList(follower.ListFile); err != nil || !proto.Equal(kept, signed) {
		t.Errorf("the follower keeps %v, error %v, want the list it was given, signature and all", kept, err)
	}
	if _, last := coordinator.requests(); !last.GetKeepsLists() || last.GetList() != nil {
		t.Errorf("the follower asked with %v, want it to say it keeps lists and to show none unasked", last)
	}

	// A later list takes the place of the first, and what it leaves out goes.
	signed = coordinator.sign("node/first", second)
	if changed, err := follower.sync(ctx); err != nil || !changed {
		t.Fatalf("dropping what a list no longer names: changed %v, error %v", changed, err)
	}
	if kept, err := loadList(follower.ListFile); err != nil || !proto.Equal(kept, signed) {
		t.Errorf("the follower keeps %v, error %v, want the later list", kept, err)
	}
	if holds(t, copies, first) || !holds(t, copies, second) {
		t.Error("the follower does not hold exactly what the later list names")
	}
	// An answer with no list leaves the one it has alone.
	coordinator.say("node/first", nil, second)
	if _, err := follower.sync(ctx); err != nil {
		t.Fatal(err)
	}
	if kept, err := loadList(follower.ListFile); err != nil || !proto.Equal(kept, signed) {
		t.Errorf("after an answer without a list the follower keeps %v, error %v", kept, err)
	}
}

func TestAFollowerShowsItsListWhenItsCoordinatorAsks(t *testing.T) {
	coordinator, follower, copies, logs := following(t)
	blob := put(t, coordinator.store, "held for a store the coordinator has lost")
	signed := coordinator.sign("node/first", blob)
	if _, err := follower.sync(ctx); err != nil {
		t.Fatal(err)
	}

	// The coordinator comes back with another store, and asks.
	coordinator.say("node/first", nil, blob)
	coordinator.mu.Lock()
	coordinator.asks = true
	coordinator.mu.Unlock()
	before, _ := coordinator.requests()
	if changed, err := follower.sync(ctx); err != nil || changed {
		t.Fatalf("showing the list: changed %v, error %v", changed, err)
	}
	asked, last := coordinator.requests()
	if asked != before+2 || !proto.Equal(last.GetList(), signed) || !slices.Equal(last.GetHolding(), []string{blob}) || last.GetStore() != "node/first" {
		t.Errorf("asked for its list, the follower made %d requests, the last %v, want a second that shows the list it kept", asked-before, last)
	}

	// A follower whose list cannot be read says so and carries on with what
	// it was told; one that has none just carries on.
	if err := os.WriteFile(follower.ListFile, []byte("not a list"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, unreadable := range []int{1, 1} {
		before, _ = coordinator.requests()
		if _, err := follower.sync(ctx); err != nil {
			t.Fatal(err)
		}
		if asked, _ := coordinator.requests(); asked != before+1 {
			t.Errorf("with no list to show the follower made %d requests, want one", asked-before)
		}
		if n := logs.count("could not read the list this node's coordinator signed"); n != unreadable {
			t.Errorf("a list that cannot be read was logged %d times, want %d", n, unreadable)
		}
		if err := os.Remove(follower.ListFile); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
	}
	if !holds(t, copies, blob) {
		t.Error("the follower dropped its copy while its coordinator was asking for a list")
	}
}

func TestAFollowerThatCannotKeepAListDoesNotActOnIt(t *testing.T) {
	coordinator, follower, copies, _ := following(t)
	held, fresh := put(t, coordinator.store, "held already"), put(t, coordinator.store, "named by a list that cannot be kept")
	coordinator.say("node/first", nil, held)
	if _, err := follower.sync(ctx); err != nil {
		t.Fatal(err)
	}

	follower.ListFile = filepath.Join(t.TempDir(), "absent", "kept.list")
	coordinator.sign("node/first", fresh)
	if changed, err := follower.sync(ctx); err == nil || changed {
		t.Fatalf("with nowhere to keep the list: changed %v, error %v", changed, err)
	}
	if !holds(t, copies, held) || holds(t, copies, fresh) {
		t.Error("the follower acted on a list it could not keep")
	}
}
