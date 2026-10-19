package coordinator

import (
	"slices"
	"sync"
	"testing"
	"time"

	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
)

// Two role changes that race reach the worker in the order their roles were
// read, so the last it is told is the newest, never an older set read first
// but queued after.
func TestTheLastRolesAWorkerIsToldAreTheNewest(t *testing.T) {
	var mu sync.Mutex
	held := []pb.Role{pb.Role_ROLE_WORKER}
	read, release := make(chan struct{}), make(chan struct{})
	first := true
	c := &Coordinator{rolesOf: func(string) []pb.Role {
		mu.Lock()
		roles, blocking := slices.Clone(held), first
		first = false
		mu.Unlock()
		if blocking {
			// The first change has read the old roles and is held here, as
			// if it were slow to queue them.
			close(read)
			<-release
		}
		return roles
	}}
	w := &worker{id: "w", send: newOutbox()}
	c.workers = map[string]*worker{"w": w}

	var both sync.WaitGroup
	both.Add(2)
	go func() { defer both.Done(); c.RolesChanged("w") }()
	<-read
	mu.Lock()
	held = []pb.Role{pb.Role_ROLE_WORKER, pb.Role_ROLE_CLIENT}
	mu.Unlock()
	go func() { defer both.Done(); c.RolesChanged("w") }()
	// Give the second change time to queue its roles first, if it can.
	time.Sleep(100 * time.Millisecond)
	close(release)
	both.Wait()

	var last []pb.Role
	for range 2 {
		msg, _ := w.send.next()
		last = msg.GetRolesChanged().GetRoles()
	}
	if want := []pb.Role{pb.Role_ROLE_WORKER, pb.Role_ROLE_CLIENT}; !slices.Equal(last, want) {
		t.Errorf("the last roles the worker is told are %v, want %v", last, want)
	}
}
