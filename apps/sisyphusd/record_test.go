package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ipfs/go-cid"

	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/api"
	"github.com/sisyphus-network/Sisyphus/packages/kubo"
	"github.com/sisyphus-network/Sisyphus/packages/names"
	"github.com/sisyphus-network/Sisyphus/packages/storage"
)

// quickRecords has nodes started after it look at their own record every
// few milliseconds, until the test and its nodes are over.
func quickRecords(t *testing.T) {
	t.Helper()
	old := recordCheck
	recordCheck = 20 * time.Millisecond
	t.Cleanup(func() { recordCheck = old })
}

// describedAt returns the description a node's name stands for, as the node
// at addr gives it.
func describedAt(t *testing.T, addr, id string) (nodeRecord, string) {
	t.Helper()
	at := strings.TrimSpace(waitForOutput(t, "baf", "name", "resolve", "--addr", addr, id))
	var described nodeRecord
	document := mustCLI(t, "blob", "get", "--addr", addr, at)
	if err := json.Unmarshal([]byte(document), &described); err != nil {
		t.Fatalf("the description %q: %v", document, err)
	}
	return described, at
}

func TestACoordinatorAskedToKeepsADescriptionOfItselfUnderItsName(t *testing.T) {
	quickRecords(t)
	addr, dataDir, laptop := freeAddr(t), t.TempDir(), t.TempDir()
	startDaemon(t, "--role", "coordinator", "--listen", addr, "--data-dir", dataDir, "--publish-record")
	id := nodeID(t, dataDir)

	described, first := describedAt(t, addr, id)
	if described.ID != id || described.Version != version || !slices.Contains(described.Workloads, "primes") || !slices.Contains(described.Workloads, "graph") || len(described.Members) != 0 {
		t.Errorf("the description of a node with no members: %+v", described)
	}
	// It is kept for as long as the record that points at it.
	if pins := mustCLI(t, "blob", "pins", "--addr", addr); !strings.Contains(pins, first) || !strings.Contains(pins, recordPins) {
		t.Errorf("the description is not pinned:\n%s", pins)
	}

	// A node that joins the pool appears in it, by ID and role.
	if _, err := as(t, laptop, "pool", "join", "--addr", addr, invite(t, addr, "client")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		described, _ = describedAt(t, addr, id)
		return len(described.Members) == 1
	})
	if member := described.Members[0]; member.ID != nodeID(t, laptop) || member.Role != "client" {
		t.Errorf("the member described: %+v", member)
	}

	// The node has the one name. Anything else its owner publishes under
	// it gives way to the description again.
	other := strings.TrimSpace(mustCLI(t, "blob", "put", "--addr", addr, writeFile(t, "something else")))
	mustCLI(t, "name", "publish", "--addr", addr, other)
	waitFor(t, func() bool { return strings.TrimSpace(mustCLI(t, "name", "resolve", "--addr", addr, id)) != other })
	if described, _ := describedAt(t, addr, id); described.ID != id {
		t.Errorf("the description after something else was published: %+v", described)
	}

	// Only a node with a pool has one to describe.
	if _, err := cli(t, "run", "--data-dir", t.TempDir(), "--role", "worker", "--coordinator", "127.0.0.1:1", "--publish-record"); err == nil || !strings.Contains(err.Error(), "is for a node that coordinates a pool") {
		t.Errorf("--publish-record on a node with no pool: %v", err)
	}
}

// flakyStore is a store that fails on request.
type flakyStore struct {
	*storage.Store
	failPut, failPin bool
}

func (f *flakyStore) Put(ctx context.Context, r io.Reader) (cid.Cid, error) {
	if f.failPut {
		return cid.Undef, errShelf
	}
	return f.Store.Put(ctx, r)
}

func (f *flakyStore) Pin(ctx context.Context, owner string, expires time.Time, cids ...cid.Cid) error {
	if f.failPin {
		return errShelf
	}
	return f.Store.Pin(ctx, owner, expires, cids...)
}

func TestANodesRecordIsRenewedBeforeItExpiresAndReplacedWhenTheNodeChanges(t *testing.T) {
	ctx := context.Background()
	ident := newIdentity(t)
	store := &flakyStore{Store: storage.NewMemory()}
	kept := &nameShelf{records: make(map[string][]byte)}
	held := api.NewNames(kept, func(string) bool { return true })
	description := "as it was"
	keeper := &recordKeeper{ident: ident, store: store, names: held, log: quiet, describe: func() []byte { return []byte(description) }}
	// after returns the record the name has once the keeper has looked at
	// it, some time after the start.
	start := time.Now()
	after := func(passed time.Duration) *names.Record {
		t.Helper()
		if err := keeper.publish(ctx, start.Add(passed)); err != nil {
			t.Fatal(err)
		}
		record, err := held.Held(ident.ID())
		if err != nil {
			t.Fatal(err)
		}
		return record
	}

	first := after(0)
	if first.Sequence != 0 || !first.Expires.Equal(start.Add(recordLifetime)) || string(stored(t, store.Store, first.Value)) != "as it was" {
		t.Fatalf("the first record: %+v", first)
	}
	// While it has most of its life left, and nothing has changed, it is
	// left alone.
	if again := after(recordLifetime / 4); again.Sequence != 0 || !again.Expires.Equal(first.Expires) {
		t.Errorf("a quarter of its life on: %+v", again)
	}
	// Half way through, a new one takes its place, pointing at the same.
	renewed := after(recordLifetime/2 + time.Minute)
	if renewed.Sequence != 1 || renewed.Value != first.Value || !renewed.Expires.After(first.Expires) {
		t.Errorf("past half its life: %+v", renewed)
	}
	// And at once when what it describes changes.
	description = "as it is now"
	changed := after(recordLifetime/2 + 2*time.Minute)
	if changed.Sequence != 2 || string(stored(t, store.Store, changed.Value)) != "as it is now" {
		t.Errorf("after a change: %+v", changed)
	}
	// Each description is pinned until its record would expire.
	pinned := make(map[string]time.Time)
	for _, pin := range store.Pins() {
		if pin.Owner == recordPins {
			pinned[pin.CID.String()] = pin.Expires
		}
	}
	if len(pinned) != 2 || !pinned[first.Value.String()].Equal(renewed.Expires) || !pinned[changed.Value.String()].Equal(changed.Expires) {
		t.Errorf("pins: %v", pinned)
	}

	// Whatever goes wrong is reported, and leaves the name as it was.
	for about, tt := range map[string]struct {
		breakIt func()
		want    string
	}{
		"the description cannot be stored": {func() { store.failPut = true }, "store the record"},
		"the description cannot be kept":   {func() { store.failPin = true }, "keep the record"},
		"the record cannot be saved":       {func() { kept.fail(false, true) }, errShelf.Error()},
		"the record held cannot be read":   {func() { kept.fail(true, false) }, errShelf.Error()},
	} {
		description = "after " + about
		tt.breakIt()
		if err := keeper.publish(ctx, start.Add(recordLifetime)); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("when %s: %v, want %q", about, err, tt.want)
		}
		store.failPut, store.failPin = false, false
		kept.fail(false, false)
		if record, err := held.Held(ident.ID()); err != nil || record.Sequence != 2 {
			t.Errorf("the name after a failure when %s: %+v, %v", about, record, err)
		}
	}
}

func TestANodeKeepsTryingToPublishItsRecord(t *testing.T) {
	quickRecords(t)
	ident := newIdentity(t)
	store := &flakyStore{Store: storage.NewMemory(), failPut: true}
	held := api.NewNames(&nameShelf{records: make(map[string][]byte)}, func(string) bool { return true })
	logs := new(syncBuffer)
	keeper := &recordKeeper{ident: ident, store: store, names: held, log: slog.New(slog.NewTextHandler(logs, nil)), describe: func() []byte { return []byte("this node") }}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		keeper.run(ctx)
	}()
	waitFor(t, func() bool { return strings.Count(logs.String(), "could not publish this node's record") >= 2 })
	if _, err := held.Held(ident.ID()); !errors.Is(err, api.ErrNoRecord) {
		t.Errorf("the name while its record could not be stored: %v", err)
	}
	cancel()
	<-done
}

// ipfsTry runs the ipfs command against the Kubo repository of the node
// whose data is in dataDir, and returns what it printed, or nothing if it
// failed.
func ipfsTry(dataDir string, args ...string) string {
	cmd := exec.Command("ipfs", args...)
	cmd.Env = append(cmd.Environ(), "IPFS_PATH="+filepath.Join(dataDir, "ipfs"))
	out, _ := cmd.Output()
	return strings.TrimSpace(string(out))
}

func TestAPoolWithKuboCarriesItsNamesOnItsPrivateNetwork(t *testing.T) {
	requireKubo(t)
	addr := freeAddr(t)
	coordinatorDir, workerDir := t.TempDir(), t.TempDir()
	startDaemon(t, "--role", "coordinator", "--kubo", "--listen", addr, "--data-dir", coordinatorDir)
	coordinator := nodeID(t, coordinatorDir)

	// With nobody else on the network yet, the node's Kubo keeps the
	// record itself.
	report := strings.TrimSpace(waitForOutput(t, "baf", "blob", "put", "--addr", addr, writeFile(t, "the pool's report")))
	mustCLI(t, "name", "publish", "--addr", addr, report)
	if got := ipfsTry(coordinatorDir, "name", "resolve", "/ipns/"+coordinator); got != "/ipfs/"+report {
		t.Errorf("the coordinator's Kubo resolves its name to %q, want /ipfs/%s", got, report)
	}

	startDaemon(t, "--role", "worker", "--kubo", "--coordinator", addr, "--data-dir", workerDir, "--name", "hand")
	waitForOutput(t, "hand", "nodes", "--addr", addr)
	waitFor(t, func() bool { return strings.Contains(peersOf(t, workerDir), coordinator) })
	hand := nodeID(t, workerDir)

	// A record published once the network has members reaches them: the
	// worker's Kubo resolves the coordinator's name, and reads what it
	// stands for, with IPFS's own tools.
	notes := strings.TrimSpace(mustCLI(t, "blob", "put", "--addr", addr, writeFile(t, "the pool's notes")))
	mustCLI(t, "name", "publish", "--addr", addr, notes)
	waitFor(t, func() bool { return ipfsTry(workerDir, "name", "resolve", "/ipns/"+coordinator) == "/ipfs/"+notes })
	if got := ipfsTry(workerDir, "cat", "/ipns/"+coordinator); got != "the pool's notes" {
		t.Errorf("ipfs cat of the coordinator's name on the worker: %q", got)
	}
	// And a record a worker hands its coordinator goes the same way.
	if _, err := as(t, workerDir, "name", "publish", "--addr", addr, report); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return ipfsTry(coordinatorDir, "name", "resolve", "/ipns/"+hand) == "/ipfs/"+report })
}

func TestANameIsHeldEvenIfKuboWillNotTakeIt(t *testing.T) {
	logs := new(syncBuffer)
	// Nothing is listening where this Kubo is supposed to be.
	daemon := &kubo.Daemon{Client: kubo.NewClient(freeAddr(t))}
	record := names.Make(newIdentity(t), mustCID(t, megabyteCID), 1, time.Hour, time.Minute, time.Now())
	carryName(context.Background(), daemon, record, slog.New(slog.NewTextHandler(logs, nil)))
	if !strings.Contains(logs.String(), "could not be given to Kubo") || !strings.Contains(logs.String(), record.Name) {
		t.Errorf("logged %q", logs.String())
	}
}
