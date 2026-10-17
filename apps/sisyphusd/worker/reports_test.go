package worker

import (
	"testing"

	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
)

func TestReportsWakeOnceForCoalescedProgressAndLogs(t *testing.T) {
	changed := make(chan struct{}, 1)
	r := &reports{changed: changed}
	r.Progress(0.25)
	r.Log("first")
	r.Progress(0.75)
	r.Log("second")
	select {
	case <-changed:
	default:
		t.Fatal("task report did not wake the sender")
	}
	select {
	case <-changed:
		t.Fatal("coalesced task reports scheduled redundant sends")
	default:
	}

	update := r.take(&pb.TaskAssignment{TaskId: "job/0", Attempt: 3})
	if update == nil || update.GetTaskId() != "job/0" || update.GetAttempt() != 3 || update.GetProgress() != 0.75 {
		t.Fatalf("task update = %v", update)
	}
	if got := update.GetLog(); len(got) != 2 || got[0] != "first" || got[1] != "second" {
		t.Fatalf("coalesced logs = %v", got)
	}
	if update := r.take(&pb.TaskAssignment{}); update != nil {
		t.Fatalf("an already-sent report was repeated: %v", update)
	}
}

func TestPrivateTaskLogsWakeOnlyForTheWithheldMarker(t *testing.T) {
	changed := make(chan struct{}, 1)
	r := &reports{private: true, changed: changed}
	r.Log("secret one")
	r.Log("secret two")
	select {
	case <-changed:
	default:
		t.Fatal("withheld marker did not wake the sender")
	}
	select {
	case <-changed:
		t.Fatal("private log contents caused repeated notifications")
	default:
	}
	update := r.take(&pb.TaskAssignment{TaskId: "private/0"})
	if update == nil || len(update.GetLog()) != 1 || update.GetLog()[0] != withheldLine {
		t.Fatalf("private task update = %v", update)
	}
}
