package worker_test

import (
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/worker"
	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
	"github.com/sisyphus-network/Sisyphus/packages/runtime"
	"github.com/sisyphus-network/Sisyphus/packages/storage"
)

// ownerPlace is where an owner shares that the node is, changed as the
// owner changes it.
type ownerPlace struct {
	mu                  sync.Mutex
	latitude, longitude float64
	shared              bool
}

func (o *ownerPlace) set(latitude, longitude float64, shared bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.latitude, o.longitude, o.shared = latitude, longitude, shared
}

func (o *ownerPlace) location() (float64, float64, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.latitude, o.longitude, o.shared
}

// startPlaced runs a worker that says where it is, as location has it,
// against a listening coordinator, and returns the coordinator once it has
// heard the worker's hello.
func startPlaced(t *testing.T, location func() (float64, float64, bool)) (*listeningCoordinator, *worker.Worker, *pb.Hello) {
	t.Helper()
	coordinator := &listeningCoordinator{assign: make(chan *pb.TaskAssignment, 1), heard: make(chan *pb.WorkerMessage, 16)}
	t.Cleanup(func() { close(coordinator.assign) })
	srv := newServer()
	pb.RegisterCoordinatorServiceServer(srv, coordinator)
	addr, _ := serve(t, srv)
	w := &worker.Worker{
		Name: "w", Coordinator: addr, Credentials: workerCreds, Slots: 1,
		Workloads: runtime.Builtin(), Blobs: storage.NewMemory(), Log: quiet(), HeartbeatInterval: time.Hour,
		Location: location,
	}
	runWorker(t, w)
	return coordinator, w, (<-coordinator.heard).GetHello()
}

func quietFor(t *testing.T, coordinator *listeningCoordinator, why string) {
	t.Helper()
	select {
	case msg := <-coordinator.heard:
		t.Fatalf("the worker sent %v %s", msg, why)
	case <-time.After(100 * time.Millisecond):
	}
}

// A worker says in its hello as much of where it is as its owner shares,
// and tells its coordinator at once when that changes, without
// reconnecting.
func TestAWorkerSaysWhereItsOwnerSharesItIs(t *testing.T) {
	place := &ownerPlace{latitude: 52.37, longitude: 4.9, shared: true}
	coordinator, w, hello := startPlaced(t, place.location)
	if want := (&pb.SharedLocation{Latitude: 52.37, Longitude: 4.9}); !proto.Equal(hello.GetSharedLocation(), want) {
		t.Fatalf("hello shared location = %v, want %v", hello.GetSharedLocation(), want)
	}

	// Word that changed nothing sends nothing.
	w.LocationChanged()
	quietFor(t, coordinator, "for a location that did not change")

	// The owner moves the pin.
	place.set(48.86, 2.35, true)
	w.LocationChanged()
	w.LocationChanged()
	moved := (<-coordinator.heard).GetHeartbeat().GetLocation()
	if want := (&pb.SharedLocation{Latitude: 48.86, Longitude: 2.35}); !proto.Equal(moved.GetSharedLocation(), want) {
		t.Fatalf("after the pin moved the worker said %v, want %v", moved, want)
	}

	// The owner stops sharing coordinates: the worker says so, with an
	// update that carries none.
	place.set(48.86, 2.35, false)
	w.LocationChanged()
	stopped := (<-coordinator.heard).GetHeartbeat()
	if stopped.GetLocation() == nil || stopped.GetLocation().GetSharedLocation() != nil {
		t.Fatalf("after sharing stopped the worker said %v, want an update with no location", stopped)
	}
	w.LocationChanged()
	quietFor(t, coordinator, "when there was still nothing to share")
}

// A worker whose owner shares no coordinates, or that has no way to ask,
// says nothing of where it is.
func TestAWorkerThatSharesNoPlaceSaysNone(t *testing.T) {
	_, _, hello := startPlaced(t, (&ownerPlace{latitude: 52.37, longitude: 4.9}).location)
	if hello.GetSharedLocation() != nil {
		t.Errorf("a worker that shares nothing said it is at %v", hello.GetSharedLocation())
	}
	coordinator, w, hello := startPlaced(t, nil)
	if hello.GetSharedLocation() != nil {
		t.Errorf("a worker with no location said it is at %v", hello.GetSharedLocation())
	}
	w.LocationChanged()
	quietFor(t, coordinator, "with no location to share")
}

// Word that comes while earlier word is still waiting to be acted on is
// acted on with it, and telling a worker never waits, even one that is
// not running.
func TestWordOfAMoveNeverWaits(t *testing.T) {
	idle := &worker.Worker{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		idle.LocationChanged()
		idle.LocationChanged()
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("telling a worker that is not running of a move waited")
	}
}
