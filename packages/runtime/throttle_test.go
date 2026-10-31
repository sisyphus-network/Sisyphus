package runtime

import (
	"context"
	"io"
	"slices"
	"sync"
	"testing"

	"github.com/sisyphus-network/Sisyphus/packages/storage"
)

// holding is an engine whose containers run until let go, and that keeps
// what it is asked.
type holding struct {
	mu      sync.Mutex
	asked   [][]string
	started chan string
	release chan struct{}
}

func (h *holding) engine(_ context.Context, args []string, _, _ io.Writer) (int, error) {
	h.mu.Lock()
	h.asked = append(h.asked, args)
	h.mu.Unlock()
	if args[0] == "run" {
		h.started <- args[slices.Index(args, "--name")+1]
		<-h.release
	}
	return 0, nil
}

func (h *holding) updates() (updates [][]string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, args := range h.asked {
		if args[0] == "update" {
			updates = append(updates, args)
		}
	}
	return updates
}

// While the machine's owner is using it, the containers running the pool's
// tasks there are given a low share of the processor, those started in the
// meantime start with it, and all are given their usual share back once
// the owner has gone.
func TestContainersAreThrottledWhileTheOwnerIsBack(t *testing.T) {
	ctx := context.Background()
	t.Cleanup(func() { Throttle(ctx, false) })
	h := &holding{started: make(chan string), release: make(chan struct{})}
	c := Container{Engine: h.engine}
	payloads, _ := c.Split(ctx, nil, []byte(`{"image":"alpine:3.20","command":["sleep","60"]}`), 2)
	var ran sync.WaitGroup
	run := func(payload []byte) string {
		ran.Go(func() {
			if _, err := c.Execute(ctx, storage.NewMemory(), payload); err != nil {
				t.Error(err)
			}
		})
		return <-h.started
	}

	// Nothing is changed while nothing runs.
	Throttle(ctx, true)
	Throttle(ctx, false)
	if got := h.updates(); len(got) != 0 {
		t.Errorf("with nothing running, the engine was asked %v", got)
	}

	first := run(payloads[0])
	if slices.Contains(h.asked[0], "--cpu-shares") {
		t.Errorf("a container started while the owner was away was given a share: %v", h.asked[0])
	}
	Throttle(ctx, true)
	// Said twice, it is done once.
	Throttle(ctx, true)
	if got := h.updates(); len(got) != 1 || !slices.Equal(got[0], []string{"update", "--cpu-shares", "2", first}) {
		t.Errorf("the owner back, the engine was asked %v", got)
	}

	second := run(payloads[1])
	started := h.asked[len(h.asked)-1]
	if at := slices.Index(started, "--cpu-shares"); at < 0 || started[at+1] != "2" || at > slices.Index(started, "alpine:3.20") {
		t.Errorf("a container started while the owner was back: %v", started)
	}

	Throttle(ctx, false)
	got := h.updates()[1:]
	slices.SortFunc(got, func(a, b []string) int { return slices.Compare(a, b) })
	want := [][]string{{"update", "--cpu-shares", "1024", first}, {"update", "--cpu-shares", "1024", second}}
	slices.SortFunc(want, func(a, b []string) int { return slices.Compare(a, b) })
	if !slices.EqualFunc(got, want, slices.Equal) {
		t.Errorf("the owner gone, the engine was asked %v, want %v", got, want)
	}

	close(h.release)
	ran.Wait()
	containers.mu.Lock()
	defer containers.mu.Unlock()
	if len(containers.engines) != 0 {
		t.Errorf("containers that have ended are still held: %v", containers.engines)
	}
}
