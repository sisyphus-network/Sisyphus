package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/access"
	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/p2p"
	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/worker"
	nodepb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/node/v1"
	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
	"github.com/sisyphus-network/Sisyphus/packages/runtime"
)

// neighbourhood is the world as a node's reciprocity sees it: whom the node
// trusts, who trusts it, and whom it is working for.
type neighbourhood struct {
	mu        sync.Mutex
	trusted   []string
	trusting  map[string]bool
	addresses map[string][]string
	working   map[string]string // node ID to the address worked at
	turnAway  map[string]chan struct{}
	asked     map[string]int
	// every is how often the node looks without being told to; nudge tells
	// it to, and employed is its own account of whom it works for.
	every    time.Duration
	nudge    func()
	employed func(id string) bool
}

func newNeighbourhood() *neighbourhood {
	return &neighbourhood{
		trusting: make(map[string]bool), addresses: make(map[string][]string),
		working: make(map[string]string), turnAway: make(map[string]chan struct{}), asked: make(map[string]int),
		every: 5 * time.Millisecond,
	}
}

func (n *neighbourhood) set(change func()) {
	n.mu.Lock()
	defer n.mu.Unlock()
	change()
}

func (n *neighbourhood) workingFor() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	var ids []string
	for id := range n.working {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

// start runs a node's reciprocity in the neighbourhood until the test ends
// or stop is called, and returns what it logs.
func (n *neighbourhood) start(t *testing.T, most int) (logs *syncBuffer, stop func()) {
	t.Helper()
	logs = new(syncBuffer)
	r := newReciprocity(&reciprocity{
		trusted: func() []string {
			n.mu.Lock()
			defer n.mu.Unlock()
			return slices.Clone(n.trusted)
		},
		addresses: func(id string) []string {
			n.mu.Lock()
			defer n.mu.Unlock()
			return n.addresses[id]
		},
		ask: func(_ context.Context, id, addr string) bool {
			n.mu.Lock()
			defer n.mu.Unlock()
			n.asked[id]++
			// Only at its right address does a node answer at all.
			return n.trusting[id] && strings.HasPrefix(addr, "right")
		},
		work: func(ctx context.Context, id, addr string) error {
			n.mu.Lock()
			n.working[id] = addr
			away := make(chan struct{})
			n.turnAway[id] = away
			n.mu.Unlock()
			defer n.set(func() { delete(n.working, id) })
			select {
			case <-ctx.Done():
				return nil
			case <-away:
				return errors.New("turned away")
			}
		},
		every: n.every, most: most, log: slog.New(slog.NewTextHandler(logs, nil)),
	})
	n.nudge, n.employed = r.nudge, r.workingFor
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.run(ctx)
	}()
	stop = sync.OnceFunc(func() {
		cancel()
		<-done
	})
	t.Cleanup(stop)
	return logs, stop
}

func TestANodeWorksForThoseItTrustsThatTrustItBack(t *testing.T) {
	n := newNeighbourhood()
	n.set(func() {
		n.trusted = []string{"mutual", "one-sided", "unreachable"}
		n.trusting["mutual"], n.trusting["unreachable"] = true, true
		n.addresses["mutual"] = []string{"wrong:1", "right:2", "right:3"}
		n.addresses["one-sided"] = []string{"right:4"}
		// "unreachable" trusts this node too, but is known at no address.
	})
	logs, stop := n.start(t, 16)

	waitFor(t, func() bool { return slices.Equal(n.workingFor(), []string{"mutual"}) })
	n.set(func() {
		if n.working["mutual"] != "right:2" {
			t.Errorf("working for the node at %q, want the first address at which it answered", n.working["mutual"])
		}
	})
	// The one that does not trust this node is asked again and again, in
	// case it comes to.
	waitFor(t, func() bool {
		n.mu.Lock()
		defer n.mu.Unlock()
		return n.asked["one-sided"] >= 3
	})
	n.set(func() { n.trusting["one-sided"] = true })
	waitFor(t, func() bool { return slices.Equal(n.workingFor(), []string{"mutual", "one-sided"}) })

	// Turned away, by the other node ending its trust, it stops, and asks
	// again later like anyone else.
	n.set(func() {
		n.trusting["mutual"] = false
		close(n.turnAway["mutual"])
	})
	waitFor(t, func() bool { return slices.Equal(n.workingFor(), []string{"one-sided"}) })
	waitFor(t, func() bool {
		return strings.Contains(logs.String(), `msg="stopped working for a node" node=mutual reason="turned away"`)
	})

	// Ending its own trust in a node, it stops working for it.
	n.set(func() { n.trusted = []string{"mutual", "unreachable"} })
	waitFor(t, func() bool { return len(n.workingFor()) == 0 })
	waitFor(t, func() bool {
		return strings.Contains(logs.String(), "no longer working for a node this one no longer trusts")
	})

	// And stopping the node stops whatever is left.
	n.set(func() { n.trusting["mutual"] = true })
	waitFor(t, func() bool { return slices.Equal(n.workingFor(), []string{"mutual"}) })
	stop()
	if left := n.workingFor(); len(left) != 0 {
		t.Errorf("still working for %v after stopping", left)
	}
}

func TestWordOfAChangeIsActedOnAtOnce(t *testing.T) {
	n := newNeighbourhood()
	// Left to itself this node would not look again for an hour.
	n.every = time.Hour
	n.set(func() {
		n.trusted = []string{"rig"}
		n.addresses["rig"] = []string{"right:1"}
	})
	n.start(t, 16)
	waitFor(t, func() bool {
		n.mu.Lock()
		defer n.mu.Unlock()
		return n.asked["rig"] == 1
	})
	if n.employed("rig") {
		t.Fatal("working for a node that does not trust this one")
	}

	// The rig comes to trust this node, and sends word.
	n.set(func() { n.trusting["rig"] = true })
	n.nudge()
	waitFor(t, func() bool { return n.employed("rig") })

	// This node ends its own trust, which is news too.
	n.set(func() { n.trusted = nil })
	n.nudge()
	waitFor(t, func() bool { return !n.employed("rig") && len(n.workingFor()) == 0 })
}

func TestANodeWorksForOnlySoManyAtOnce(t *testing.T) {
	n := newNeighbourhood()
	n.set(func() {
		for _, id := range []string{"a", "b", "c"} {
			n.trusted = append(n.trusted, id)
			n.trusting[id] = true
			n.addresses[id] = []string{"right:1"}
		}
	})
	n.start(t, 2)
	waitFor(t, func() bool { return len(n.workingFor()) == 2 })
	time.Sleep(50 * time.Millisecond)
	if got := n.workingFor(); !slices.Equal(got, []string{"a", "b"}) {
		t.Errorf("working for %v, want the first two and no more", got)
	}
	// When one goes, the next has its turn.
	n.set(func() { n.trusted = []string{"b", "c"} })
	waitFor(t, func() bool { return slices.Equal(n.workingFor(), []string{"b", "c"}) })
}

func TestWhereANodeAsksAnotherForWork(t *testing.T) {
	found := []p2p.Peer{
		{ID: "other", Addrs: []string{"/ip4/10.0.0.9/tcp/7700"}},
		{ID: "rig", Addrs: []string{"/ip4/10.0.0.5/tcp/7700", "/ip4/10.0.0.5/udp/7700/quic-v1", "/ip6/2001:db8::5/tcp/7700", "/ip4/10.0.0.5/tcp/7700"}},
	}
	if got := whereToAsk(found, "rig"); !slices.Equal(got, []string{"10.0.0.5:7700", "[2001:db8::5]:7700"}) {
		t.Errorf("where to ask the rig: %v", got)
	}
	if got := whereToAsk(found, "nobody"); len(got) != 0 {
		t.Errorf("where to ask a node never heard of: %v", got)
	}
}

func TestAskingANodeWhetherItTrustsThisOne(t *testing.T) {
	p := startPool(t, runtime.Builtin())
	worker, _ := p.admit(access.Worker)
	client, _ := p.admit(access.Client)
	stranger := newIdentity(t)

	if !trustsThisNode(p.ctx, worker, p.ident.ID(), p.addr) {
		t.Error("a node admitted as a worker is not told it is trusted")
	}
	// Being let use the pool is not being trusted to work for it.
	if trustsThisNode(p.ctx, client, p.ident.ID(), p.addr) {
		t.Error("a client was told it is trusted for compute")
	}
	if trustsThisNode(p.ctx, stranger, p.ident.ID(), p.addr) {
		t.Error("a stranger was told it is trusted")
	}
	// Somebody else answering there, or nothing that is an address.
	if trustsThisNode(p.ctx, worker, stranger.ID(), p.addr) {
		t.Error("the wrong node at the address was taken for the right one")
	}
	if trustsThisNode(p.ctx, worker, p.ident.ID(), "bad\x00address") {
		t.Error("something that is no address answered")
	}
}

func TestWorkingForAnotherNodeBesideOnesOwnPool(t *testing.T) {
	p := startPool(t, runtime.Builtin())
	ident, _ := p.admit(access.Worker)
	dataDir := t.TempDir()
	guests := &guestWork{ident: ident, dataDir: dataDir, template: &worker.Worker{Name: "guest", Slots: 2, Workloads: p.workloads, Log: quiet}}

	ctx, stop := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- guests.work(ctx, p.ident.ID(), p.addr) }()
	p.waitForWorkers(1)
	// What its tasks fetch is kept apart, under the other node's name.
	input := p.upload(sampleText())
	p.wordcountOn(input, 2)
	if blocks, _ := filepath.Glob(filepath.Join(dataDir, "guest", p.ident.ID(), "cache", "blocks", "*", "*.data")); len(blocks) == 0 {
		t.Error("nothing was cached for the node worked for")
	}
	stop()
	if err := <-done; err != nil {
		t.Errorf("stopping work for another node: %v", err)
	}

	// Turned away, it says why.
	if _, err := p.access.Remove(ident.ID()); err != nil {
		t.Fatal(err)
	}
	if err := guests.work(context.Background(), p.ident.ID(), p.addr); err == nil || !strings.Contains(err.Error(), "rejected") {
		t.Errorf("working for a node that does not trust this one: %v", err)
	}
	if err := guests.work(context.Background(), p.ident.ID(), "bad\x00address"); err == nil {
		t.Error("working for a node at no address succeeded")
	}
	// Nowhere to keep what is fetched.
	blocked := t.TempDir()
	if err := os.WriteFile(filepath.Join(blocked, "guest"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	guests.dataDir = blocked
	if err := guests.work(context.Background(), p.ident.ID(), p.addr); err == nil {
		t.Error("working with nowhere to cache succeeded")
	}
}

func TestANodesWorkersShareItsSlots(t *testing.T) {
	g := gated{started: make(chan struct{}, 16), release: make(chan struct{})}
	p := startPool(t, runtime.NewRegistry(g))
	// Two workers, as one node working for two pools has, with room for
	// one task between them.
	shared := worker.NewSlots(1)
	p.tune = func(w *worker.Worker) { w.Limit = shared }
	p.startWorker("first", 1)
	stopSecond := p.startWorker("second", 1)
	p.waitForWorkers(2)
	job := p.submit(&pb.JobSpec{Workload: "gated", MaxTasks: 2})

	<-g.started
	select {
	case <-g.started:
		t.Fatal("two tasks ran at once on a node with room for one")
	case <-time.After(200 * time.Millisecond):
	}
	stopSecond()
	close(g.release)
	if done := p.wait(job.GetJobId()); done.GetState() != pb.JobState_JOB_STATE_SUCCEEDED {
		t.Errorf("job %v: %s", done.GetState(), done.GetError())
	}
}

func TestAWorkerWaitingForASlotCanBeStopped(t *testing.T) {
	g := gated{started: make(chan struct{}, 16), release: make(chan struct{})}
	p := startPool(t, runtime.NewRegistry(g))
	// Every slot the node has is taken by work for someone else.
	shared := worker.NewSlots(1)
	shared.Acquire(context.Background(), "someone else")
	p.tune = func(w *worker.Worker) { w.Limit = shared }
	stop := p.startWorker("busy", 1)
	p.waitForWorkers(1)
	job := p.submit(&pb.JobSpec{Workload: "gated", MaxTasks: 1})
	waitFor(t, func() bool { return p.job(job.GetJobId()).GetTasks()[0].GetState() == pb.TaskState_TASK_STATE_RUNNING })
	select {
	case <-g.started:
		t.Fatal("a task ran on a node with no slot free")
	case <-time.After(100 * time.Millisecond):
	}
	// Stopped while its task still waits, the worker goes without a fuss.
	stop()
	p.waitForWorkers(0)

	// With a slot free again, another worker runs the task.
	shared.Release("someone else")
	p.startWorker("free", 1)
	<-g.started
	close(g.release)
	if done := p.wait(job.GetJobId()); done.GetState() != pb.JobState_JOB_STATE_SUCCEEDED {
		t.Errorf("job %v: %s", done.GetState(), done.GetError())
	}
}

func TestNodesThatTrustEachOtherWorkForEachOther(t *testing.T) {
	// Left to themselves the nodes would look once a minute. Here they act
	// on being told.
	logs := captureLogs(t)

	rigDir, laptopDir := t.TempDir(), t.TempDir()
	rigListen, rig := onEveryInterface(t)
	laptopListen, laptop := onEveryInterface(t)
	rigAPI, laptopAPI := freeAddr(t), freeAddr(t)
	startDaemon(t, "--data-dir", rigDir, "--listen", rigListen, "--name", "rig", "--slots", "2", "--discovery", "on", "--api-listen", rigAPI)
	startDaemon(t, "--data-dir", laptopDir, "--listen", laptopListen, "--name", "laptop", "--slots", "2", "--discovery", "on", "--api-listen", laptopAPI)
	dataDirs.Store(rig, rigDir)
	dataDirs.Store(laptop, laptopDir)
	onRig, onLaptop := desktop(t, rigAPI), desktop(t, laptopAPI)
	rigID, laptopID := nodeID(t, rigDir), nodeID(t, laptopDir)
	waitFor(t, func() bool {
		return peerSeenBy(t, onRig, laptopID).GetConnectionState() == connected && peerSeenBy(t, onLaptop, rigID).GetConnectionState() == connected
	})
	workers := func(addr string) string { return mustCLI(t, "nodes", "--addr", addr) }

	// One trusting the other changes nothing by itself: the laptop has not
	// said it will work for the rig.
	trust := func(client nodepb.NodeServiceClient, dir, peer string, trusted bool) {
		t.Helper()
		if _, err := client.SetPeerComputeTrust(tokenOf(t, dir), &nodepb.SetPeerComputeTrustRequest{PeerId: peer, Trusted: trusted}); err != nil {
			t.Fatal(err)
		}
	}
	trust(onRig, rigDir, laptopID, true)
	time.Sleep(300 * time.Millisecond)
	if out := workers(rig); strings.Contains(out, "laptop") {
		t.Fatalf("the laptop is working for a rig it has not said it trusts:\n%s", out)
	}

	if seen := peerSeenBy(t, onRig, laptopID); seen.GetWorksForThisNode() || seen.GetThisNodeWorksFor() {
		t.Errorf("with trust one way only, the rig lists the laptop as %v", seen)
	}

	// The laptop's owner trusts the rig in turn, and with nothing restarted
	// each starts working for the other. Each got word of the other's
	// trust: neither has waited the minute it would take to ask unprompted.
	started := time.Now()
	trust(onLaptop, laptopDir, rigID, true)
	waitForOutput(t, "laptop", "nodes", "--addr", rig)
	waitForOutput(t, "rig", "nodes", "--addr", laptop)
	if took := time.Since(started); took > 20*time.Second {
		t.Errorf("mutual trust took %v to take effect", took)
	}
	// And the desktop is told which way work is flowing: both ways.
	waitFor(t, func() bool {
		rigsView, laptopsView := peerSeenBy(t, onRig, laptopID), peerSeenBy(t, onLaptop, rigID)
		return rigsView.GetWorksForThisNode() && rigsView.GetThisNodeWorksFor() && laptopsView.GetWorksForThisNode() && laptopsView.GetThisNodeWorksFor()
	})
	out := mustCLI(t, "job", "submit", "--addr", rig, "--tasks", "4", "--params", `{"from":0,"to":2000000}`)
	if !strings.Contains(out, "on laptop") || !strings.Contains(out, "on rig") || !strings.Contains(out, primesBelowTwoMillion) {
		t.Errorf("a job on the rig, with the laptop working for it:\n%s", out)
	}

	// The laptop's owner thinks better of it. The laptop stops working for
	// the rig, and the rig is turned away from the laptop.
	trust(onLaptop, laptopDir, rigID, false)
	waitFor(t, func() bool {
		return !strings.Contains(workers(rig), "laptop") && !strings.Contains(workers(laptop), "rig")
	})
	waitFor(t, func() bool {
		return strings.Contains(logs.String(), "no longer working for a node this one no longer trusts") &&
			strings.Contains(logs.String(), `msg="stopped working for a node" node=`+laptopID)
	})
	waitFor(t, func() bool {
		rigsView := peerSeenBy(t, onRig, laptopID)
		return !rigsView.GetWorksForThisNode() && !rigsView.GetThisNodeWorksFor()
	})
	// Each still has its own worker, and its own pool works as before.
	if out := mustCLI(t, "job", "submit", "--addr", laptop, "--params", `{"from":0,"to":100}`); !strings.Contains(out, `{"count":25}`) {
		t.Errorf("a job on the laptop afterwards:\n%s", out)
	}
}

func TestWorkForAnotherNodeGoesOverItsPrivateNetwork(t *testing.T) {
	requireKubo(t)
	logs := captureLogs(t)
	rigDir, laptopDir, plainDir := t.TempDir(), t.TempDir(), t.TempDir()
	rigListen, rig := onEveryInterface(t)
	laptopListen, laptop := onEveryInterface(t)
	plainListen, plain := onEveryInterface(t)
	rigAPI, laptopAPI, plainAPI := freeAddr(t), freeAddr(t), freeAddr(t)
	// Two nodes that each keep their pool's data in Kubo, on a private
	// network of that pool's own, and one that uses no Kubo at all.
	startDaemon(t, "--data-dir", rigDir, "--listen", rigListen, "--name", "rig", "--slots", "1", "--kubo", "--discovery", "on", "--api-listen", rigAPI)
	startDaemon(t, "--data-dir", laptopDir, "--listen", laptopListen, "--name", "laptop", "--slots", "4", "--kubo", "--discovery", "on", "--api-listen", laptopAPI)
	startDaemon(t, "--data-dir", plainDir, "--listen", plainListen, "--name", "plain", "--slots", "1", "--discovery", "on", "--api-listen", plainAPI)
	for addr, dir := range map[string]string{rig: rigDir, laptop: laptopDir, plain: plainDir} {
		dataDirs.Store(addr, dir)
	}
	onRig, onLaptop, onPlain := desktop(t, rigAPI), desktop(t, laptopAPI), desktop(t, plainAPI)
	rigID, laptopID, plainID := nodeID(t, rigDir), nodeID(t, laptopDir), nodeID(t, plainDir)
	waitFor(t, func() bool {
		return peerSeenBy(t, onRig, laptopID).GetConnectionState() == connected && peerSeenBy(t, onLaptop, rigID).GetConnectionState() == connected &&
			peerSeenBy(t, onPlain, laptopID).GetConnectionState() == connected && peerSeenBy(t, onLaptop, plainID).GetConnectionState() == connected
	})
	trust := func(client nodepb.NodeServiceClient, dir, peer string) {
		t.Helper()
		if _, err := client.SetPeerComputeTrust(tokenOf(t, dir), &nodepb.SetPeerComputeTrustRequest{PeerId: peer, Trusted: true}); err != nil {
			t.Fatal(err)
		}
	}
	trust(onRig, rigDir, laptopID)
	trust(onLaptop, laptopDir, rigID)
	trust(onPlain, plainDir, laptopID)
	trust(onLaptop, laptopDir, plainID)
	waitForOutput(t, "laptop", "nodes", "--addr", rig)
	waitForOutput(t, "laptop", "nodes", "--addr", plain)

	// A job on the rig, most of it done by the laptop.
	text := strings.Repeat(boulderText, 20_000)
	input := strings.TrimSpace(mustCLI(t, "blob", "put", "--addr", rig, writeFile(t, text)))
	out := mustCLI(t, "job", "submit", "--addr", rig, "--workload", "wordcount", "--tasks", "5", "--params", `{"input":"`+input+`"}`)
	if !strings.Contains(out, "on laptop") || !strings.Contains(out, `"words":180000`) {
		t.Fatalf("job output:\n%s", out)
	}
	// The laptop has a Kubo daemon for the rig's pool, beside its own, and
	// that is where the rig's data came to it.
	guest := filepath.Join(laptopDir, "guest", rigID)
	if got := ipfsIn(t, guest, "cat", "--offline", input); got != text {
		t.Errorf("the laptop's Kubo for the rig's pool holds %d bytes of the input, want all %d", len(got), len(text))
	}
	rigKey, _ := os.ReadFile(filepath.Join(rigDir, "ipfs", "swarm.key"))
	guestKey, _ := os.ReadFile(filepath.Join(guest, "ipfs", "swarm.key"))
	ownKey, _ := os.ReadFile(filepath.Join(laptopDir, "ipfs", "swarm.key"))
	if len(rigKey) == 0 || string(guestKey) != string(rigKey) || string(ownKey) == string(rigKey) {
		t.Error("the laptop's Kubo for the rig's pool is not on the rig's network, or its own Kubo is")
	}
	if strings.Contains(logs.String(), "did not supply a blob") {
		t.Error("the rig's private network did not supply the input; the laptop fell back to asking the rig")
	}

	// For the node with no such network the laptop works without one.
	out = mustCLI(t, "job", "submit", "--addr", plain, "--tasks", "4", "--params", `{"from":0,"to":2000000}`)
	if !strings.Contains(out, "on laptop") || !strings.Contains(out, primesBelowTwoMillion) {
		t.Errorf("a job on the node without Kubo:\n%s", out)
	}
	if !strings.Contains(logs.String(), `msg="working for a node without its private IPFS network" node=`+plainID) {
		t.Error("nothing was said about working for a node without its private network")
	}
}

// shelf keeps a list of nodes worked for as a database would, and can be
// made to fail.
type shelf struct {
	ids []string
	err error
}

func (s *shelf) WorksFor() ([]string, error) { return s.ids, s.err }

func (s *shelf) SetWorksFor(id string, willing bool, _ time.Time) error {
	if s.err != nil {
		return s.err
	}
	s.ids = slices.DeleteFunc(s.ids, func(other string) bool { return other == id })
	if willing {
		s.ids = append(s.ids, id)
	}
	return nil
}

func TestTheListOfNodesWorkedFor(t *testing.T) {
	store := &shelf{ids: []string{"from-before"}}
	takes, err := openWilling(store)
	if err != nil {
		t.Fatal(err)
	}
	changes := 0
	takes.onChange(func() { changes++ })
	for _, id := range []string{"b", "a", "b"} {
		if err := takes.Set(id, true); err != nil {
			t.Fatal(err)
		}
	}
	if err := takes.Set("from-before", false); err != nil {
		t.Fatal(err)
	}
	if got := takes.List(); !slices.Equal(got, []string{"a", "b"}) {
		t.Errorf("worked for: %v, want a and b, each once and in order", got)
	}
	if changes != 4 {
		t.Errorf("told of %d changes, want 4", changes)
	}
	// What cannot be saved does not take effect, and nobody is told.
	store.err = errors.New("the disk is full")
	if err := takes.Set("c", true); err == nil {
		t.Error("a change that could not be saved succeeded")
	}
	if got := takes.List(); slices.Contains(got, "c") || changes != 4 {
		t.Errorf("after a failed change: %v, %d changes told", got, changes)
	}
	if _, err := openWilling(store); err == nil {
		t.Error("a list that cannot be read was opened")
	}
	// With nobody to tell, a change is just a change.
	store.err = nil
	takes.onChange(nil)
	if err := takes.Set("d", true); err != nil {
		t.Error(err)
	}
}

func TestBothSidesOfTrustAreListedForEachNode(t *testing.T) {
	peers := []*nodepb.Peer{
		{PeerId: "gives-only", GivesWork: true, WorksForThisNode: true},
		{PeerId: "both", GivesWork: true},
		{PeerId: "found"},
		{PeerId: "coordinator", TakesWork: true, ThisNodeWorksFor: true},
	}
	got := withTrust(peers, []string{"both", "takes-only"}, func(id string) bool { return id == "both" })
	type row struct {
		id                                      string
		gives, takes, trusted, forUs, weWorkFor bool
	}
	var rows []row
	for _, p := range got {
		rows = append(rows, row{p.GetPeerId(), p.GetGivesWork(), p.GetTakesWork(), p.GetTrustedForCompute(), p.GetWorksForThisNode(), p.GetThisNodeWorksFor()})
	}
	want := []row{
		{"both", true, true, true, false, true},
		{"coordinator", false, true, true, false, true},
		{"found", false, false, false, false, false},
		{"gives-only", true, false, true, true, false},
		// Known only as a node to work for.
		{"takes-only", false, true, true, false, false},
	}
	if !slices.Equal(rows, want) {
		t.Errorf("listed\n%v\nwant\n%v", rows, want)
	}
}

func TestTrustInOneDirectionOnly(t *testing.T) {
	logs := captureLogs(t)
	rigDir, laptopDir := t.TempDir(), t.TempDir()
	rigListen, rig := onEveryInterface(t)
	laptopListen, laptop := onEveryInterface(t)
	rigAPI, laptopAPI := freeAddr(t), freeAddr(t)
	startDaemon(t, "--data-dir", rigDir, "--listen", rigListen, "--name", "rig", "--slots", "1", "--discovery", "on", "--api-listen", rigAPI)
	startDaemon(t, "--data-dir", laptopDir, "--listen", laptopListen, "--name", "laptop", "--slots", "1", "--discovery", "on", "--api-listen", laptopAPI)
	dataDirs.Store(rig, rigDir)
	dataDirs.Store(laptop, laptopDir)
	onRig, onLaptop := desktop(t, rigAPI), desktop(t, laptopAPI)
	rigID, laptopID := nodeID(t, rigDir), nodeID(t, laptopDir)
	waitFor(t, func() bool {
		return peerSeenBy(t, onRig, laptopID).GetConnectionState() == connected && peerSeenBy(t, onLaptop, rigID).GetConnectionState() == connected
	})

	// The rig gives the laptop work, and will take none from it. The
	// laptop takes the rig's work, and has none for the rig: set from the
	// command line there, as on a machine with no desktop.
	if _, err := onRig.SetPeerComputePermissions(tokenOf(t, rigDir), &nodepb.SetPeerComputePermissionsRequest{PeerId: laptopID, GivesWork: true}); err != nil {
		t.Fatal(err)
	}
	if out := mustCLI(t, "pool", "work-for", "--addr", laptop, rigID); !strings.Contains(out, "will work for node "+rigID) {
		t.Errorf("pool work-for printed %q", out)
	}
	waitForOutput(t, "laptop", "nodes", "--addr", rig)
	waitFor(t, func() bool {
		rigsView, laptopsView := peerSeenBy(t, onRig, laptopID), peerSeenBy(t, onLaptop, rigID)
		return rigsView.GetGivesWork() && !rigsView.GetTakesWork() && rigsView.GetWorksForThisNode() && !rigsView.GetThisNodeWorksFor() &&
			laptopsView.GetTakesWork() && !laptopsView.GetGivesWork() && laptopsView.GetThisNodeWorksFor() && !laptopsView.GetWorksForThisNode()
	})
	// Work flows one way. The rig is no worker of the laptop's, and is not
	// even asking to be.
	if out := mustCLI(t, "nodes", "--addr", laptop); strings.Contains(out, "rig") {
		t.Errorf("the rig is working for the laptop:\n%s", out)
	}
	if members := mustCLI(t, "pool", "members", "--addr", laptop); !strings.Contains(members, "no nodes have been admitted") || !strings.Contains(members, "this node will work for:\n"+rigID) {
		t.Errorf("the laptop's pool members:\n%s", members)
	}
	if members := mustCLI(t, "pool", "members", "--addr", rig); !strings.Contains(members, laptopID) || strings.Contains(members, "will work for") {
		t.Errorf("the rig's pool members:\n%s", members)
	}

	// The laptop's owner stops it.
	if out := mustCLI(t, "pool", "work-for", "--addr", laptop, "--stop", rigID); !strings.Contains(out, "no longer working for node") {
		t.Errorf("pool work-for --stop printed %q", out)
	}
	waitFor(t, func() bool { return !strings.Contains(mustCLI(t, "nodes", "--addr", rig), "laptop") })
	waitForLogged(t, logs, "no longer working for a node this one no longer trusts", 1)

	for _, bad := range [][]string{
		{"pool", "work-for", "--addr", laptop},
		{"pool", "work-for", "--addr", laptop, "not-a-node-id"},
		{"pool", "work-for", "--no-such-flag"},
		{"pool", "work-for", "--data-dir", filepath.Join(laptopDir, "missing", "\x00"), rigID},
	} {
		if _, err := cli(t, bad...); err == nil {
			t.Errorf("%v succeeded", bad)
		}
	}
}

func TestAWorkerOnlyNodeCanWorkForASecondNode(t *testing.T) {
	// A worker started for one coordinator, with nothing of its own to
	// give, takes work from another as well.
	firstDir, secondDir, workerDir := t.TempDir(), t.TempDir(), t.TempDir()
	firstListen, first := onEveryInterface(t)
	secondListen, second := onEveryInterface(t)
	apiAddr, secondAPI := freeAddr(t), freeAddr(t)
	startDaemon(t, "--data-dir", firstDir, "--role", "coordinator", "--listen", firstListen, "--discovery", "on")
	startDaemon(t, "--data-dir", secondDir, "--role", "coordinator", "--listen", secondListen, "--discovery", "on", "--api-listen", secondAPI)
	dataDirs.Store(first, firstDir)
	dataDirs.Store(second, secondDir)
	startDaemon(t, "--data-dir", workerDir, "--role", "worker", "--coordinator", first, "--name", "hand", "--slots", "1", "--discovery", "on", "--api-listen", apiAddr)
	waitForOutput(t, "hand", "nodes", "--addr", first)
	onWorker, onSecond := desktop(t, apiAddr), desktop(t, secondAPI)
	workerID, secondID := nodeID(t, workerDir), nodeID(t, secondDir)
	waitFor(t, func() bool { return peerSeenBy(t, onWorker, secondID).GetConnectionState() == connected })

	if _, err := onSecond.SetPeerComputePermissions(tokenOf(t, secondDir), &nodepb.SetPeerComputePermissionsRequest{PeerId: workerID, GivesWork: true}); err != nil {
		t.Fatal(err)
	}
	// The one switch, on a node with no pool, is the taking side alone.
	if _, err := onWorker.SetPeerComputeTrust(tokenOf(t, workerDir), &nodepb.SetPeerComputeTrustRequest{PeerId: secondID, Trusted: true}); err != nil {
		t.Fatal(err)
	}
	waitForOutput(t, "hand", "nodes", "--addr", second)
	for _, addr := range []string{first, second} {
		if out := mustCLI(t, "job", "submit", "--addr", addr, "--params", `{"from":0,"to":100}`); !strings.Contains(out, "on hand") {
			t.Errorf("a job on %s:\n%s", addr, out)
		}
	}
	// It lists both as nodes it works for, and neither as one it gives
	// work to.
	for _, id := range []string{nodeID(t, firstDir), secondID} {
		if seen := peerSeenBy(t, onWorker, id); !seen.GetTakesWork() || !seen.GetThisNodeWorksFor() || seen.GetGivesWork() {
			t.Errorf("the worker lists %s as %v", id, seen)
		}
	}
}

func TestNewsThatArrivesWhileANodeIsLookingIsNotMissed(t *testing.T) {
	r := newReciprocity(&reciprocity{every: time.Hour})
	ctx := context.Background()
	// The node looks, and while it is looking something changes.
	heard := r.heardSoFar()
	r.nudge()
	done := make(chan bool, 1)
	go func() { done <- r.pause(ctx, &heard) }()
	select {
	case ended := <-done:
		if ended {
			t.Error("the pause reported the node stopped")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("news that arrived before the node paused was slept through")
	}
	// Having caught up, it waits for the next.
	go func() { done <- r.pause(ctx, &heard) }()
	select {
	case <-done:
		t.Fatal("the node did not wait though there was nothing new")
	case <-time.After(100 * time.Millisecond):
	}
	r.nudge()
	<-done
	// And a node that is stopping says so, news or no news.
	stopped, cancel := context.WithCancel(ctx)
	cancel()
	r.nudge()
	// Asked twice, since a pause that reports a stop once must go on doing so.
	if once, again := r.pause(stopped, &heard), r.pause(stopped, &heard); !once || !again {
		t.Error("a pause on a stopped node did not report it")
	}
}
