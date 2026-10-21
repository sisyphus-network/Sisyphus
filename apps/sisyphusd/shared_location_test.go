package main

import (
	"testing"

	"google.golang.org/protobuf/proto"

	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
	"github.com/sisyphus-network/Sisyphus/packages/runtime"
)

// A worker from before workers said where they are is listed as it was,
// at no shared place; one that gives coordinates that are no place on the
// Earth is placed nowhere; and a heartbeat that says nothing of the place
// leaves it as it was.
func TestTheCoordinatorKeepsWhereAWorkerIsSharedToBe(t *testing.T) {
	p := startPool(t, runtime.Builtin())
	placeOf := func(name string) *pb.SharedLocation {
		t.Helper()
		listed, err := p.client.ListNodes(p.ctx, &pb.ListNodesRequest{})
		if err != nil {
			t.Fatal(err)
		}
		for _, node := range listed.GetNodes() {
			if node.GetName() == name {
				return node.GetSharedLocation()
			}
		}
		t.Fatalf("%s is not listed", name)
		return nil
	}
	placed := func(name string, at *pb.SharedLocation) *pb.WorkerMessage {
		msg := hello(name, 1, "primes")
		msg.GetHello().SharedLocation = at
		return msg
	}

	old := p.connectRaw(hello("old", 1, "primes"))
	old.welcome()
	lost := p.connectRaw(placed("lost", &pb.SharedLocation{Latitude: 91, Longitude: 0}))
	lost.welcome()
	city := p.connectRaw(placed("city", &pb.SharedLocation{Latitude: 52.4, Longitude: 4.9}))
	city.welcome()
	p.waitForWorkers(3)
	if at := placeOf("old"); at != nil {
		t.Errorf("a worker that said nothing is at %v", at)
	}
	if at := placeOf("lost"); at != nil {
		t.Errorf("a worker at latitude 91 is taken to be at %v", at)
	}
	if at := placeOf("city"); !proto.Equal(at, &pb.SharedLocation{Latitude: 52.4, Longitude: 4.9}) {
		t.Errorf("a worker that said where it is is at %v", at)
	}

	beat := func(w *rawWorker, location *pb.LocationUpdate) {
		w.send(&pb.WorkerMessage{Kind: &pb.WorkerMessage_Heartbeat{Heartbeat: &pb.Heartbeat{Location: location}}})
	}
	// A heartbeat as one from before sends it, with nothing of the place,
	// changes nothing. Heartbeats on one stream are handled in turn, so
	// once a later one has moved the worker, the one before has been seen.
	beat(city, nil)
	beat(city, &pb.LocationUpdate{SharedLocation: &pb.SharedLocation{Latitude: 48.9, Longitude: 2.4}})
	waitFor(t, func() bool { return proto.Equal(placeOf("city"), &pb.SharedLocation{Latitude: 48.9, Longitude: 2.4}) })
	// An update with no place in it means the place is no longer shared.
	beat(city, &pb.LocationUpdate{})
	waitFor(t, func() bool { return placeOf("city") == nil })
	// One that is no place on the Earth is taken as none.
	beat(old, &pb.LocationUpdate{SharedLocation: &pb.SharedLocation{Latitude: -1, Longitude: -1}})
	waitFor(t, func() bool { return placeOf("old") != nil })
	beat(old, &pb.LocationUpdate{SharedLocation: &pb.SharedLocation{Latitude: 0, Longitude: 181}})
	waitFor(t, func() bool { return placeOf("old") == nil })
}
