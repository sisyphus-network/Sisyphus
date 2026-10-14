package main

import (
	"testing"

	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
	"github.com/sisyphus-network/Sisyphus/packages/runtime"
)

// serving connects a hand-driven worker with so many slots that serves the
// model m, with what it says of m in memory, if anything.
func (p *pool) serving(name string, slots uint32, loaded ...string) *rawWorker {
	p.t.Helper()
	msg := hello(name, slots, "chat")
	caps := msg.GetHello().GetCapabilities()
	caps.Labels = append(caps.Labels, append([]string{"model:m"}, loaded...)...)
	w := p.connectRaw(msg)
	w.welcome()
	return w
}

func (p *pool) chat() string {
	p.t.Helper()
	return p.submit(&pb.JobSpec{Workload: "chat", Params: []byte(`{"model":"m","messages":[{"role":"user","content":"Hi"}]}`), Mode: pb.ScheduleMode_SCHEDULE_MODE_DISTRIBUTED}).GetJobId()
}

// A model server answers about one request at a time, however many slots
// its worker has, so two replies asked for at once go to two workers that
// serve the model, not both to the one with more slots.
func TestRepliesGoToTheWorkerWithTheFewestOnTheModel(t *testing.T) {
	p := startPool(t, runtime.Builtin().With(runtime.Chat{}))
	big, small := p.serving("big", 12), p.serving("small", 4)
	p.chat()
	first := big.assignment()
	p.chat()
	second := small.assignment()
	if first.GetJobId() == second.GetJobId() {
		t.Fatalf("both workers were handed the same reply")
	}
}

// Among workers with as much on the model, the one with it in memory and
// most of it on a graphics card answers soonest.
func TestAReplyGoesWhereTheModelIsMostOnACard(t *testing.T) {
	p := startPool(t, runtime.Builtin().With(runtime.Chat{}))
	p.serving("cold", 32)
	p.serving("partly", 16, "loaded:m=25")
	card := p.serving("card", 4, "loaded:m=100")
	id := p.chat()
	if got := card.assignment(); got.GetJobId() != id {
		t.Errorf("the worker with the model on its card was handed %v", got)
	}
}
