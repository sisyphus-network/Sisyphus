package pinning

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ipfs/go-cid"

	"github.com/sisyphus-network/Sisyphus/packages/nodedb"
	"github.com/sisyphus-network/Sisyphus/packages/storage"
)

const key = "the-key"

// The addresses of two peers that may have what a node is asked for.
const (
	nearby  = "/ip4/10.0.0.7/tcp/4001/p2p/12D3KooWnearby"
	faraway = "/ip4/203.0.113.9/tcp/4001/p2p/12D3KooWfaraway"
)

// faults are the failures a test has arranged, by the name of what fails.
type faults struct {
	mu      sync.Mutex
	failing map[string]error
}

func (f *faults) fail(what string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failing == nil {
		f.failing = make(map[string]error)
	}
	f.failing[what] = err
}

func (f *faults) failure(what string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.failing[what]
}

// shelf is a node's store. Reading a blob through fetches it, as a store
// over Kubo does, from whichever of the peers the node has connected to
// holds it.
type shelf struct {
	*storage.Store
	faults
	wire  *wire
	peers map[string]*storage.Store
	// used is how much disk the store says it occupies.
	used uint64
	// onVerify, if set, is called as a blob begins to be read through.
	onVerify func(ctx context.Context, c cid.Cid) error
}

func (s *shelf) Has(ctx context.Context, c cid.Cid) (bool, error) {
	if err := s.failure("has"); err != nil {
		return false, err
	}
	return s.Store.Has(ctx, c)
}

func (s *shelf) Pin(ctx context.Context, owner string, expires time.Time, cids ...cid.Cid) error {
	if err := s.failure("pin"); err != nil {
		return err
	}
	return s.Store.Pin(ctx, owner, expires, cids...)
}

func (s *shelf) Unpin(owner string, cids ...cid.Cid) error {
	if err := s.failure("unpin"); err != nil {
		return err
	}
	return s.Store.Unpin(owner, cids...)
}

func (s *shelf) Size(context.Context) (uint64, error) {
	return s.used, s.failure("size")
}

func (s *shelf) watch(onVerify func(ctx context.Context, c cid.Cid) error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onVerify = onVerify
}

func (s *shelf) Verify(ctx context.Context, c cid.Cid) error {
	s.mu.Lock()
	onVerify := s.onVerify
	s.mu.Unlock()
	if onVerify != nil {
		if err := onVerify(ctx, c); err != nil {
			return err
		}
	}
	if held, _ := s.Store.Has(ctx, c); held {
		return nil
	}
	for _, address := range s.wire.reached() {
		if peer := s.peers[address]; peer != nil {
			if blob, err := peer.Open(ctx, c); err == nil {
				defer blob.Close()
				_, err = s.Store.Put(ctx, blob)
				return err
			}
		}
	}
	return fmt.Errorf("could not find %s", c)
}

// wire is a node's Kubo as a service uses it.
type wire struct {
	faults
	connected []string
}

func (w *wire) Connect(_ context.Context, address string) error {
	if err := w.failure("connect"); err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.connected = append(w.connected, address)
	return nil
}

func (w *wire) reached() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.connected)
}

func (w *wire) Addresses(context.Context) ([]string, error) {
	if err := w.failure("addresses"); err != nil {
		return nil, err
	}
	return []string{"/ip4/10.0.0.1/tcp/4001/p2p/12D3KooWnode"}, nil
}

// ledger is a node's database, which can be made to fail.
type ledger struct {
	*nodedb.DB
	faults
}

func (l *ledger) PinRequests() ([]nodedb.PinRequest, error) {
	if err := l.failure("load"); err != nil {
		return nil, err
	}
	return l.DB.PinRequests()
}

func (l *ledger) SavePinRequest(r nodedb.PinRequest) error {
	if err := l.failure("save"); err != nil {
		return err
	}
	return l.DB.SavePinRequest(r)
}

func (l *ledger) RemovePinRequest(id string) error {
	if err := l.failure("remove"); err != nil {
		return err
	}
	return l.DB.RemovePinRequest(id)
}

// syncBuffer is a buffer several goroutines may write to.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// node is a node serving the API, and what it is made of.
type node struct {
	t       *testing.T
	store   *shelf
	wire    *wire
	records *ledger
	logs    *syncBuffer
	config  Config
	service *Service
}

// newNode starts a service on a node that keeps its data in Kubo, or one
// that does not.
func newNode(t *testing.T, withKubo bool) *node {
	t.Helper()
	db, err := nodedb.Open(filepath.Join(t.TempDir(), "node.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	n := &node{t: t, wire: &wire{}, records: &ledger{DB: db}, logs: &syncBuffer{}}
	n.store = &shelf{Store: storage.NewMemory(), wire: n.wire, peers: map[string]*storage.Store{nearby: storage.NewMemory(), faraway: storage.NewMemory()}}
	n.config = Config{Store: n.store, Records: n.records, Token: key, Log: slog.New(slog.NewTextHandler(n.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))}
	if withKubo {
		n.config.Network = n.wire
	}
	n.start()
	t.Cleanup(func() { n.service.Close() })
	return n
}

// start starts the node's service, as at the start of a run.
func (n *node) start() {
	n.t.Helper()
	service, err := NewService(n.config)
	if err != nil {
		n.t.Fatal(err)
	}
	n.service = service
}

// ask makes a request of the node's service, with the key.
func (n *node) ask(method, path, body string) (int, string) {
	return askAs(n.service, key, method, path, body)
}

func askAs(service http.Handler, key, method, path, body string) (int, string) {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	rec := httptest.NewRecorder()
	service.ServeHTTP(rec, req)
	answer, _ := io.ReadAll(rec.Result().Body)
	return rec.Code, string(answer)
}

// status asks for something that should be answered with a request's
// status, and returns it.
func (n *node) status(method, path, body string, want int) Status {
	n.t.Helper()
	code, answer := n.ask(method, path, body)
	var status Status
	if err := json.Unmarshal([]byte(answer), &status); code != want || err != nil {
		n.t.Fatalf("%s %s: %d %s, want %d and a status", method, path, code, answer, want)
	}
	return status
}

// pin asks the node to keep something.
func (n *node) pin(pin Pin) Status {
	n.t.Helper()
	body, _ := json.Marshal(pin)
	return n.status(http.MethodPost, "/pins", string(body), http.StatusAccepted)
}

func (n *node) show(id string) Status {
	n.t.Helper()
	return n.status(http.MethodGet, "/pins/"+id, "", http.StatusOK)
}

// settled waits for a request to be in a state, and returns it as it is.
func (n *node) settled(id, state string) Status {
	n.t.Helper()
	eventually(n.t, func() bool { return n.show(id).Status == state })
	return n.show(id)
}

// listed returns the IDs a listing gives, in its order, and its count.
func (n *node) listed(query string) (ids []string, count int) {
	n.t.Helper()
	code, answer := n.ask(http.MethodGet, "/pins"+query, "")
	var page results
	if err := json.Unmarshal([]byte(answer), &page); code != http.StatusOK || err != nil {
		n.t.Fatalf("GET /pins%s: %d %s", query, code, answer)
	}
	ids = []string{}
	for _, status := range page.Results {
		ids = append(ids, status.RequestID)
	}
	return ids, page.Count
}

// keptFor lists what the node's store keeps for a request.
func (n *node) keptFor(id string) []string {
	kept := []string{}
	for _, pin := range n.store.Pins() {
		if pin.Owner == owner(id) {
			kept = append(kept, pin.CID.String())
		}
	}
	return kept
}

// put stores something in a store and returns its CID.
func put(t *testing.T, store *storage.Store, content string) string {
	t.Helper()
	c, err := store.Put(context.Background(), strings.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	return c.String()
}

func eventually(t *testing.T, condition func() bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); !condition(); time.Sleep(2 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting")
		}
	}
}

// gate holds each read-through of a node's store until the test lets it
// go on, and tells the test of each as it begins.
type gate struct {
	began chan string
	open  chan struct{}
}

func gateOn(store *shelf) *gate {
	g := &gate{began: make(chan string), open: make(chan struct{})}
	store.watch(func(ctx context.Context, c cid.Cid) error {
		select {
		case g.began <- c.String():
		case <-ctx.Done():
			return ctx.Err()
		}
		select {
		case <-g.open:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	return g
}

// pass lets one whole fetch of blob through: it is read through twice.
func (g *gate) pass(t *testing.T, blob string) {
	t.Helper()
	g.open <- struct{}{}
	if began := <-g.began; began != blob {
		t.Fatalf("%s began to be read, not %s", began, blob)
	}
	g.open <- struct{}{}
}

// only sets how many fetches go on at once, for one test.
func only(t *testing.T, fetches int) {
	before := fetchesAtOnce
	fetchesAtOnce = fetches
	t.Cleanup(func() { fetchesAtOnce = before })
}

func TestDataTheNodeHoldsIsPinnedAtOnceAndKeptUntilTheRequestIsRemoved(t *testing.T) {
	n := newNode(t, false)
	held := put(t, n.store.Store, "the boulder rolls")

	asked := n.pin(Pin{CID: held, Name: "boulder.txt", Meta: map[string]string{"app": "tests"}})
	if asked.RequestID == "" || asked.Status != Pinned || asked.Pin.CID != held || asked.Pin.Name != "boulder.txt" || asked.Pin.Meta["app"] != "tests" ||
		asked.Created.IsZero() || asked.Delegates == nil || len(asked.Delegates) != 0 || asked.Info != nil {
		t.Fatalf("the request as taken: %+v", asked)
	}
	if kept := n.keptFor(asked.RequestID); !slices.Equal(kept, []string{held}) {
		t.Errorf("kept for the request: %v", kept)
	}
	if shown := n.show(asked.RequestID); shown.Status != Pinned || shown.Pin.Name != "boulder.txt" || !shown.Created.Equal(asked.Created) {
		t.Errorf("the request as shown: %+v", shown)
	}
	// Asked again, the same data is a second request, kept until both go.
	// It is the later of the two even by a clock that has not moved on,
	// since a listing is paged by when requests were made.
	now = func() time.Time { return asked.Created }
	defer func() { now = time.Now }()
	again := n.pin(Pin{CID: held})
	if again.RequestID == asked.RequestID || !again.Created.After(asked.Created) {
		t.Errorf("the second request %+v beside the first %+v", again, asked)
	}
	if ids, count := n.listed(""); count != 2 || !slices.Equal(ids, []string{again.RequestID, asked.RequestID}) {
		t.Errorf("listed: %v of %d", ids, count)
	}

	if code, answer := n.ask(http.MethodDelete, "/pins/"+asked.RequestID, ""); code != http.StatusAccepted || answer != "" {
		t.Fatalf("removing: %d %s", code, answer)
	}
	if kept := n.keptFor(asked.RequestID); len(kept) != 0 {
		t.Errorf("still kept for the request removed: %v", kept)
	}
	if kept := n.keptFor(again.RequestID); !slices.Equal(kept, []string{held}) {
		t.Errorf("kept for the request that remains: %v", kept)
	}
	if code, answer := n.ask(http.MethodGet, "/pins/"+asked.RequestID, ""); code != http.StatusNotFound || !strings.Contains(answer, `"reason":"NOT_FOUND"`) {
		t.Errorf("the request once removed: %d %s", code, answer)
	}
	// With the last request gone, garbage collection takes the data.
	n.ask(http.MethodDelete, "/pins/"+again.RequestID, "")
	if _, err := n.store.GC(context.Background(), time.Now().Add(2*storage.GracePeriod)); err != nil {
		t.Fatal(err)
	}
	c, _ := cid.Decode(held)
	if has, _ := n.store.Store.Has(context.Background(), c); has {
		t.Error("the data outlived its requests and a garbage collection")
	}
}

func TestWithoutKuboWhatTheNodeDoesNotHoldFailsAndSaysWhy(t *testing.T) {
	n := newNode(t, false)
	absent := put(t, n.store.peers[nearby], "held elsewhere")

	asked := n.pin(Pin{CID: absent, Origins: []string{nearby}})
	if asked.Status != Failed || !strings.Contains(asked.Info[Details], "no way to fetch it") || !strings.Contains(asked.Info[Details], "--kubo") {
		t.Fatalf("a request for what is not held: %+v", asked)
	}
	if kept := n.keptFor(asked.RequestID); len(kept) != 0 {
		t.Errorf("kept for a failed request: %v", kept)
	}
	// It stays, to be asked after, until removed.
	if ids, count := n.listed("?status=failed"); count != 1 || !slices.Equal(ids, []string{asked.RequestID}) {
		t.Errorf("failed requests listed: %v of %d", ids, count)
	}
	if code, _ := n.ask(http.MethodDelete, "/pins/"+asked.RequestID, ""); code != http.StatusAccepted {
		t.Errorf("removing a failed request: %d", code)
	}
}

func TestWithKuboARequestIsFetchedFromItsOriginsAndGoesFromQueuedToPinned(t *testing.T) {
	only(t, 1)
	n := newNode(t, true)
	first := put(t, n.store.peers[nearby], "the first thing asked for")
	second := put(t, n.store.peers[nearby], "the second")
	third := put(t, n.store.peers[nearby], "the third")
	nowhere := put(t, storage.NewMemory(), "nobody has this")
	reads := gateOn(n.store)

	one := n.pin(Pin{CID: first, Name: "first", Origins: []string{nearby}})
	if one.Status != Queued && one.Status != Pinning {
		t.Fatalf("a request for what must be fetched: %+v", one)
	}
	if !slices.Equal(one.Delegates, []string{"/ip4/10.0.0.1/tcp/4001/p2p/12D3KooWnode"}) {
		t.Errorf("the node's Kubo is not given as where to bring the data: %v", one.Delegates)
	}
	if began := <-reads.began; began != first {
		t.Fatalf("%s is being fetched, not the first", began)
	}
	if shown := n.show(one.RequestID); shown.Status != Pinning {
		t.Errorf("while being fetched: %+v", shown)
	}
	if !slices.Equal(n.wire.reached(), []string{nearby}) {
		t.Errorf("connected to %v before fetching, not to the origin", n.wire.reached())
	}
	// One is fetched at a time here, so the rest wait.
	two := n.pin(Pin{CID: second, Origins: []string{nearby}})
	three := n.pin(Pin{CID: third})
	if two.Status != Queued || three.Status != Queued || n.show(two.RequestID).Status != Queued {
		t.Errorf("requests made while another is fetched: %+v, %+v", two, three)
	}
	if ids, count := n.listed("?status=queued,pinning"); count != 3 || len(ids) != 3 {
		t.Errorf("unfinished requests listed: %v of %d", ids, count)
	}
	// One that waits can be removed, and never is fetched.
	if code, _ := n.ask(http.MethodDelete, "/pins/"+three.RequestID, ""); code != http.StatusAccepted {
		t.Errorf("removing a queued request: %d", code)
	}

	reads.pass(t, first)
	if done := n.settled(one.RequestID, Pinned); done.Info != nil {
		t.Errorf("once fetched: %+v", done)
	}
	if kept := n.keptFor(one.RequestID); !slices.Equal(kept, []string{first}) {
		t.Errorf("kept for the fetched request: %v", kept)
	}

	// One being fetched can be removed too: the fetch stops and nothing is
	// kept for it.
	if began := <-reads.began; began != second {
		t.Fatalf("%s is being fetched, not the second", began)
	}
	if code, _ := n.ask(http.MethodDelete, "/pins/"+two.RequestID, ""); code != http.StatusAccepted {
		t.Errorf("removing a request being fetched: %d", code)
	}
	if kept := n.keptFor(two.RequestID); len(kept) != 0 {
		t.Errorf("kept for a request removed while fetched: %v", kept)
	}

	// What no peer the node can reach has, fails, and says what kind of
	// peer could have supplied it.
	n.store.watch(nil)
	n.wire.fail("connect", errors.New("no route to host"))
	lost := n.pin(Pin{CID: nowhere, Origins: []string{faraway}})
	failed := n.settled(lost.RequestID, Failed)
	if why := failed.Info[Details]; !strings.Contains(why, "could not find "+nowhere) || !strings.Contains(why, "swarm key") {
		t.Errorf("a request for what nobody has: %+v", failed)
	}
	if kept := n.keptFor(lost.RequestID); len(kept) != 0 {
		t.Errorf("kept for a failed request: %v", kept)
	}
	if logged := n.logs.String(); !strings.Contains(logged, "could not connect to a pin request's origin") || !strings.Contains(logged, "no route to host") {
		t.Errorf("an origin that could not be connected to is not logged:\n%s", logged)
	}
}

func TestWhatKuboAlreadyHoldsIsAnsweredAsPinnedIfItIsReadThroughInTime(t *testing.T) {
	n := newNode(t, true)
	held := put(t, n.store.Store, "already here")
	if asked := n.pin(Pin{CID: held}); asked.Status != Pinned || !slices.Equal(n.keptFor(asked.RequestID), []string{held}) {
		t.Fatalf("a request for what Kubo holds: %+v", asked)
	}

	// An asker that has gone is not waited for.
	before := promptness
	defer func() { promptness = before }()
	promptness = time.Hour
	reads := gateOn(n.store)
	left, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(http.MethodPost, "/pins", strings.NewReader(`{"cid":"`+held+`"}`)).WithContext(left)
	req.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()
	n.service.ServeHTTP(rec, req)
	var unheard Status
	if json.Unmarshal(rec.Body.Bytes(), &unheard); rec.Code != http.StatusAccepted || unheard.Status == Pinned {
		t.Fatalf("answered to an asker that has gone: %d %s", rec.Code, rec.Body)
	}
	<-reads.began
	reads.pass(t, held)
	n.settled(unheard.RequestID, Pinned)

	// Something large takes longer to read than an answer should wait.
	promptness = time.Millisecond
	slow := n.pin(Pin{CID: held})
	if slow.Status == Pinned {
		t.Fatalf("answered as pinned before it was read through: %+v", slow)
	}
	<-reads.began
	reads.pass(t, held)
	n.settled(slow.RequestID, Pinned)

	// A block that has gone by the time the data is pinned fails the
	// request, which then keeps nothing.
	checks := 0
	n.store.watch(func(context.Context, cid.Cid) error {
		if checks++; checks == 2 {
			return errors.New("a block has gone")
		}
		return nil
	})
	gone := n.pin(Pin{CID: held})
	if failed := n.settled(gone.RequestID, Failed); !strings.Contains(failed.Info[Details], "a block has gone") || len(n.keptFor(gone.RequestID)) != 0 {
		t.Errorf("a request whose data went missing: %+v, keeping %v", failed, n.keptFor(gone.RequestID))
	}
}

func TestAReplacedRequestKeepsItsDataUntilTheNewOneIsSettled(t *testing.T) {
	replace := func(n *node, id string, pin Pin) Status {
		t.Helper()
		body, _ := json.Marshal(pin)
		return n.status(http.MethodPost, "/pins/"+id, string(body), http.StatusAccepted)
	}

	// On a node without Kubo everything is settled at once.
	plain := newNode(t, false)
	a, b := put(t, plain.store.Store, "version one"), put(t, plain.store.Store, "version two")
	absent := put(t, storage.NewMemory(), "held nowhere")
	old := plain.pin(Pin{CID: a, Name: "v1"})
	next := replace(plain, old.RequestID, Pin{CID: b, Name: "v2"})
	if next.RequestID == old.RequestID || next.Status != Pinned || next.Pin.CID != b || next.Pin.Name != "v2" {
		t.Fatalf("the replacement: %+v", next)
	}
	if code, _ := plain.ask(http.MethodGet, "/pins/"+old.RequestID, ""); code != http.StatusNotFound {
		t.Errorf("the replaced request is still there: %d", code)
	}
	if len(plain.keptFor(old.RequestID)) != 0 || !slices.Equal(plain.keptFor(next.RequestID), []string{b}) {
		t.Errorf("kept for the old request %v, and for the new %v", plain.keptFor(old.RequestID), plain.keptFor(next.RequestID))
	}
	// Replacing with what cannot be had is still a replacement.
	if lost := replace(plain, next.RequestID, Pin{CID: absent}); lost.Status != Failed || len(plain.keptFor(next.RequestID)) != 0 || len(plain.keptFor(lost.RequestID)) != 0 {
		t.Errorf("replaced with what cannot be had: %+v", lost)
	}
	if code, answer := plain.ask(http.MethodPost, "/pins/nonesuch", `{"cid":"`+a+`"}`); code != http.StatusNotFound || !strings.Contains(answer, "there is no request nonesuch") {
		t.Errorf("replacing a request that is not there: %d %s", code, answer)
	}

	// With Kubo the new data takes time to fetch, and until it is here the
	// old data is kept for the new request.
	n := newNode(t, true)
	a = put(t, n.store.Store, "version one")
	b, c := put(t, n.store.peers[nearby], "version two"), put(t, n.store.peers[nearby], "version three")
	old = n.pin(Pin{CID: a})
	reads := gateOn(n.store)
	next = replace(n, old.RequestID, Pin{CID: b, Origins: []string{nearby}})
	<-reads.began
	if kept := n.keptFor(next.RequestID); !slices.Equal(kept, []string{a}) || len(n.keptFor(old.RequestID)) != 0 {
		t.Errorf("while the replacement is fetched, kept for it: %v, and for the old one: %v", kept, n.keptFor(old.RequestID))
	}
	// Replaced again before it is settled, it hands on what it kept.
	last := replace(n, next.RequestID, Pin{CID: c, Origins: []string{nearby}})
	<-reads.began
	if kept := n.keptFor(last.RequestID); !slices.Equal(kept, []string{a}) || len(n.keptFor(next.RequestID)) != 0 {
		t.Errorf("replaced a second time, kept for the newest: %v, and for the one between: %v", kept, n.keptFor(next.RequestID))
	}
	reads.pass(t, c)
	n.settled(last.RequestID, Pinned)
	if kept := n.keptFor(last.RequestID); !slices.Equal(kept, []string{c}) {
		t.Errorf("once the replacement is fetched, kept for it: %v", kept)
	}

	// A request replaced by one for the same data keeps that data.
	same := replace(n, last.RequestID, Pin{CID: c, Name: "renamed"})
	<-reads.began
	reads.pass(t, c)
	if n.settled(same.RequestID, Pinned); !slices.Equal(n.keptFor(same.RequestID), []string{c}) {
		t.Errorf("replaced by a request for the same data, kept: %v", n.keptFor(same.RequestID))
	}
	// A replacement that fails lets go of what it kept for the old one.
	n.store.watch(nil)
	lost := replace(n, same.RequestID, Pin{CID: absent})
	if n.settled(lost.RequestID, Failed); len(n.keptFor(lost.RequestID)) != 0 {
		t.Errorf("a failed replacement keeps: %v", n.keptFor(lost.RequestID))
	}

	// A replacement the node cannot take in leaves the old request as it
	// was: still being fetched, if it was.
	reads = gateOn(n.store)
	unfinished := n.pin(Pin{CID: b, Origins: []string{nearby}})
	<-reads.began
	n.store.fail("has", errors.New("the disk has failed"))
	if code, answer := n.ask(http.MethodPost, "/pins/"+unfinished.RequestID, `{"cid":"`+c+`"}`); code != http.StatusInternalServerError || !strings.Contains(answer, "the disk has failed") {
		t.Fatalf("a replacement that cannot be taken in: %d %s", code, answer)
	}
	n.store.fail("has", nil)
	<-reads.began
	reads.pass(t, b)
	n.settled(unfinished.RequestID, Pinned)
	// And one that is settled is left settled.
	n.store.fail("has", errors.New("the disk has failed"))
	if code, _ := n.ask(http.MethodPost, "/pins/"+unfinished.RequestID, `{"cid":"`+c+`"}`); code != http.StatusInternalServerError || n.show(unfinished.RequestID).Status != Pinned {
		t.Errorf("a settled request after a replacement that could not be taken in: %d, %+v", code, n.show(unfinished.RequestID))
	}
}

func TestAListingIsFilteredAndPaged(t *testing.T) {
	n := newNode(t, false)
	absent := put(t, storage.NewMemory(), "held nowhere")
	var made []Status
	for i, pin := range []Pin{
		{Name: "Results.tar", Meta: map[string]string{"app": "render", "run": "1"}},
		{Name: "results.tar", Meta: map[string]string{"app": "render", "run": "2"}},
		{Name: "notes"},
		{Name: "lost", CID: absent},
	} {
		if pin.CID == "" {
			pin.CID = put(t, n.store.Store, fmt.Sprint("file ", i))
		}
		made = append(made, n.pin(pin))
	}
	id := func(i int) string { return made[i].RequestID }
	at := func(i int) string { return url.QueryEscape(made[i].Created.Format(time.RFC3339Nano)) }

	for query, want := range map[string]struct {
		ids   []string
		count int
	}{
		// Unasked, a listing is of what is pinned, newest first.
		"":                      {[]string{id(2), id(1), id(0)}, 3},
		"?status=failed":        {[]string{id(3)}, 1},
		"?status=queued":        {[]string{}, 0},
		"?status=pinned,failed": {[]string{id(3), id(2), id(1), id(0)}, 4},
		// The count is of everything that matches, not of the page.
		"?limit=2":                 {[]string{id(2), id(1)}, 3},
		"?limit=2&before=" + at(1): {[]string{id(0)}, 1},
		"?after=" + at(0):          {[]string{id(2), id(1)}, 2},
		"?cid=" + made[2].Pin.CID:  {[]string{id(2)}, 1},
		"?cid=" + made[0].Pin.CID + "," + made[1].Pin.CID: {[]string{id(1), id(0)}, 2},
		"?cid=" + absent:                                         {[]string{}, 0},
		"?name=results.tar":                                      {[]string{id(1)}, 1},
		"?name=results.tar&match=exact":                          {[]string{id(1)}, 1},
		"?name=RESULTS.TAR&match=iexact":                         {[]string{id(1), id(0)}, 2},
		"?name=esults&match=partial":                             {[]string{id(1), id(0)}, 2},
		"?name=RES&match=ipartial":                               {[]string{id(1), id(0)}, 2},
		"?name=res&match=partial":                                {[]string{id(1)}, 1},
		"?meta=" + url.QueryEscape(`{"app":"render"}`):           {[]string{id(1), id(0)}, 2},
		"?meta=" + url.QueryEscape(`{"app":"render","run":"2"}`): {[]string{id(1)}, 1},
		"?meta=" + url.QueryEscape(`{"owner":"nobody"}`):         {[]string{}, 0},
	} {
		if ids, count := n.listed(query); count != want.count || !slices.Equal(ids, want.ids) {
			t.Errorf("GET /pins%s: %v of %d, want %v of %d", query, ids, count, want.ids, want.count)
		}
	}

	for query, want := range map[string]string{
		"?cid=not-a-cid": "is not a CID",
		"?cid=" + strings.Repeat(absent+",", 10) + made[0].Pin.CID: "at most 10 CIDs",
		"?name=x&match=fuzzy":         "match is exact, iexact, partial or ipartial",
		"?status=pinned,forgotten":    `"forgotten\" is not a status`,
		"?before=yesterday":           "before is a time",
		"?after=2026":                 "after is a time",
		"?limit=0":                    "limit is a number from 1 to 1000",
		"?limit=1001":                 "limit is a number from 1 to 1000",
		"?limit=many":                 "limit is a number from 1 to 1000",
		"?meta=map%5Bapp%3Arender%5D": "meta is a JSON object",
	} {
		if code, answer := n.ask(http.MethodGet, "/pins"+query, ""); code != http.StatusBadRequest || !strings.Contains(answer, `"reason":"BAD_REQUEST"`) || !strings.Contains(answer, want) {
			t.Errorf("GET /pins%s: %d %s, want it refused with %q", query, code, answer, want)
		}
	}
}

func TestWhatCannotBeDoneIsRefusedInTheAPIsOwnWords(t *testing.T) {
	n := newNode(t, false)
	held := put(t, n.store.Store, "the boulder rolls")
	asked := n.pin(Pin{CID: held})
	body := `{"cid":"` + held + `"}`

	// Every refusal is the API's error object, with a reason and details.
	refused := func(what string, code int, answer string, wantCode int, reason, details string) {
		t.Helper()
		var f failure
		if err := json.Unmarshal([]byte(answer), &f); err != nil || code != wantCode || f.Error.Reason != reason || !strings.Contains(f.Error.Details, details) {
			t.Errorf("%s: %d %s, want %d %s with %q", what, code, answer, wantCode, reason, details)
		}
	}
	for _, shown := range []string{"", "another-key"} {
		for _, tt := range []struct{ method, path string }{{http.MethodGet, "/pins"}, {http.MethodPost, "/pins"}, {http.MethodGet, "/pins/" + asked.RequestID}, {http.MethodDelete, "/pins/" + asked.RequestID}} {
			code, answer := askAs(n.service, shown, tt.method, tt.path, body)
			refused(tt.method+" "+tt.path+" with the key "+shown, code, answer, http.StatusUnauthorized, "UNAUTHORIZED", "pinning.token")
		}
	}
	if n.show(asked.RequestID).Status != Pinned {
		t.Error("a request was removed without the key")
	}

	for _, tt := range []struct {
		method, path, body string
		code               int
		reason, details    string
	}{
		{http.MethodGet, "/", "", http.StatusNotFound, "NOT_FOUND", "/pins and /pins/<request ID>"},
		{http.MethodGet, "/pins/", "", http.StatusNotFound, "NOT_FOUND", "/pins and /pins/<request ID>"},
		{http.MethodGet, "/pins/" + asked.RequestID + "/more", "", http.StatusNotFound, "NOT_FOUND", "/pins and /pins/<request ID>"},
		{http.MethodGet, "/pins/nonesuch", "", http.StatusNotFound, "NOT_FOUND", "there is no request nonesuch"},
		{http.MethodDelete, "/pins/nonesuch", "", http.StatusNotFound, "NOT_FOUND", "there is no request nonesuch"},
		{http.MethodPut, "/pins", body, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "listed with GET and made with POST"},
		{http.MethodPut, "/pins/" + asked.RequestID, body, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "removed with DELETE"},
		{http.MethodPost, "/pins", "cid=" + held, http.StatusBadRequest, "BAD_REQUEST", "not a pin object"},
		{http.MethodPost, "/pins", `{"cid":"` + held + `","meta":{"size":5}}`, http.StatusBadRequest, "BAD_REQUEST", "not a pin object"},
		{http.MethodPost, "/pins", `{"name":"nothing"}`, http.StatusBadRequest, "BAD_REQUEST", "is not a CID"},
		{http.MethodPost, "/pins", `{"cid":"QmNotACID"}`, http.StatusBadRequest, "BAD_REQUEST", "is not a CID"},
		// A dag-cbor object, which this store has no way to walk.
		{http.MethodPost, "/pins", `{"cid":"bafyreigh2akiscaildcqabsyg3dfr6chu3fgpregiymsck7e7aqa4s52zy"}`, http.StatusBadRequest, "BAD_REQUEST", "raw and dag-pb"},
		{http.MethodPost, "/pins", `{"cid":"` + held + `","name":"` + strings.Repeat("n", 256) + `"}`, http.StatusBadRequest, "BAD_REQUEST", "at most 255 bytes"},
		{http.MethodPost, "/pins", `{"cid":"` + held + `","origins":[` + strings.Repeat(`"`+nearby+`",`, 20) + `"` + nearby + `"]}`, http.StatusBadRequest, "BAD_REQUEST", "at most 20 origins"},
		{http.MethodPost, "/pins/" + asked.RequestID, `{"cid":"nothing"}`, http.StatusBadRequest, "BAD_REQUEST", "is not a CID"},
	} {
		code, answer := n.ask(tt.method, tt.path, tt.body)
		refused(tt.method+" "+tt.path+" "+tt.body, code, answer, tt.code, tt.reason, tt.details)
	}
	if n.show(asked.RequestID).Status != Pinned {
		t.Error("a replacement that was refused removed what it would have replaced")
	}

	// What goes wrong on the node is said to be the node's.
	failed := errors.New("the disk has failed")
	n.store.fail("has", failed)
	code, answer := n.ask(http.MethodPost, "/pins", body)
	refused("when the store cannot be searched", code, answer, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "look for the data: the disk has failed")
	n.store.fail("has", nil)
	n.store.fail("pin", failed)
	code, answer = n.ask(http.MethodPost, "/pins", body)
	refused("when the data cannot be pinned", code, answer, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "pin the data: the disk has failed")
	n.store.fail("pin", nil)
	n.records.fail("save", failed)
	code, answer = n.ask(http.MethodPost, "/pins", body)
	refused("when the request cannot be recorded", code, answer, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "record the request: the disk has failed")
	n.records.fail("save", nil)
	// None of which leaves a request or a pin behind.
	if ids, count := n.listed("?status=queued,pinning,pinned,failed"); count != 1 || !slices.Equal(ids, []string{asked.RequestID}) {
		t.Errorf("after requests that could not be taken in: %v", ids)
	}
	if pins := n.store.Pins(); len(pins) != 2 { // the request's, and the one every new blob has for an hour
		t.Errorf("after requests that could not be taken in, the store keeps: %v", pins)
	}

	n.store.fail("unpin", failed)
	code, answer = n.ask(http.MethodDelete, "/pins/"+asked.RequestID, "")
	refused("when the pin cannot be released", code, answer, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "remove the request: the disk has failed")
	n.store.fail("unpin", nil)
	n.records.fail("remove", failed)
	code, answer = n.ask(http.MethodDelete, "/pins/"+asked.RequestID, "")
	refused("when the request cannot be forgotten", code, answer, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "remove the request: the disk has failed")
	n.records.fail("remove", nil)
	if code, _ := n.ask(http.MethodDelete, "/pins/"+asked.RequestID, ""); code != http.StatusAccepted {
		t.Errorf("removing, once the node has recovered: %d", code)
	}

	// A node with Kubo fetches no more once its store is as full as its
	// owner allows, though it still pins what it holds.
	full := newNode(t, true)
	full.service.cfg.MaxStoreBytes, full.store.used = 1000, 1000
	here, elsewhere := put(t, full.store.Store, "already here"), put(t, full.store.peers[nearby], "to be fetched")
	code, answer = full.ask(http.MethodPost, "/pins", `{"cid":"`+elsewhere+`"}`)
	refused("when the store is full", code, answer, http.StatusInsufficientStorage, "INSUFFICIENT_STORAGE", "1000 of 1000 bytes used")
	if kept := full.pin(Pin{CID: here}); kept.Status != Pinned {
		t.Errorf("a request for what a full node holds: %+v", kept)
	}
	full.store.used = 999
	if fetched := full.pin(Pin{CID: elsewhere, Origins: []string{nearby}}); full.settled(fetched.RequestID, Pinned).Status != Pinned {
		t.Errorf("a request made of a node with room: %+v", fetched)
	}
	full.store.fail("size", failed)
	code, answer = full.ask(http.MethodPost, "/pins", `{"cid":"`+put(t, full.store.peers[nearby], "more")+`"}`)
	refused("when the store cannot be measured", code, answer, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "measure the store: the disk has failed")
}

func TestUnfinishedRequestsAreTakenUpAgainAfterARestart(t *testing.T) {
	only(t, 1)
	n := newNode(t, true)
	first, second := put(t, n.store.peers[nearby], "the first"), put(t, n.store.peers[nearby], "the second")
	held := put(t, n.store.Store, "already here")
	done := n.pin(Pin{CID: held, Name: "kept"})
	reads := gateOn(n.store)
	one := n.pin(Pin{CID: first, Origins: []string{nearby}})
	<-reads.began
	two := n.pin(Pin{CID: second, Origins: []string{nearby}})

	// The node stops with one request being fetched and one waiting.
	n.service.Close()
	n.store.watch(nil)
	n.start()
	n.settled(one.RequestID, Pinned)
	n.settled(two.RequestID, Pinned)
	if kept := n.show(done.RequestID); kept.Status != Pinned || kept.Pin.Name != "kept" || !kept.Created.Equal(done.Created) {
		t.Errorf("a settled request after the restart: %+v, was %+v", kept, done)
	}
	// Requests made after it are still newer than those made before.
	if later := n.pin(Pin{CID: held}); !later.Created.After(two.Created) {
		t.Errorf("a request made after the restart is dated %s, the last before it %s", later.Created, two.Created)
	}

	// Started again without Kubo, the node can fetch nothing, and says so
	// of what it had still to fetch.
	third := put(t, n.store.peers[nearby], "the third")
	reads = gateOn(n.store)
	three := n.pin(Pin{CID: third, Origins: []string{nearby}})
	<-reads.began
	n.service.Close()
	n.config.Network = nil
	n.start()
	if failed := n.settled(three.RequestID, Failed); !strings.Contains(failed.Info[Details], "no way to fetch it") {
		t.Errorf("an unfinished request on a node restarted without Kubo: %+v", failed)
	}

	n.service.Close()
	n.records.fail("load", errors.New("the disk has failed"))
	if _, err := NewService(n.config); err == nil || !strings.Contains(err.Error(), "the disk has failed") {
		t.Errorf("starting with requests that cannot be read: %v", err)
	}
}

func TestFailuresBesideTheWorkAreLoggedAndTheServiceCarriesOn(t *testing.T) {
	n := newNode(t, true)
	failed := errors.New("the disk has failed")
	logged := func(want string) {
		t.Helper()
		eventually(t, func() bool { return strings.Contains(n.logs.String(), want) })
	}

	// A request that cannot be recorded as it changes still changes.
	fetched := put(t, n.store.peers[nearby], "to be fetched")
	reads := gateOn(n.store)
	asked := n.pin(Pin{CID: fetched, Origins: []string{nearby}})
	<-reads.began
	n.records.fail("save", failed)
	reads.pass(t, fetched)
	n.settled(asked.RequestID, Pinned)
	logged("a pin request could not be recorded")
	n.records.fail("save", nil)
	n.store.watch(nil)

	// A replacement goes ahead though the old request cannot be removed,
	// and though what it kept for the old one cannot be released.
	next := put(t, n.store.peers[nearby], "the next version")
	n.store.fail("unpin", failed)
	body, _ := json.Marshal(Pin{CID: next, Origins: []string{nearby}})
	replaced := n.status(http.MethodPost, "/pins/"+asked.RequestID, string(body), http.StatusAccepted)
	n.settled(replaced.RequestID, Pinned)
	logged("a replaced pin request could not be removed")
	logged("a pin request's pins could not be released")
	n.store.fail("unpin", nil)

	// A Kubo that will not give its addresses leaves a request without
	// them, not unanswered.
	n.wire.fail("addresses", failed)
	if shown := n.show(replaced.RequestID); shown.Delegates == nil || len(shown.Delegates) != 0 {
		t.Errorf("shown when Kubo gives no addresses: %+v", shown)
	}
	logged("the node's Kubo did not give its addresses")
}
