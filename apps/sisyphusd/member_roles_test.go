package main

import (
	"strings"
	"testing"

	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
)

// pool members names every role a member holds, and takes a coordinator
// that says only one at its word (#474).
func TestPoolMembersNamesEveryRole(t *testing.T) {
	for _, tt := range []struct {
		member *pb.Member
		want   string
	}{
		{&pb.Member{Role: pb.Role_ROLE_WORKER, Roles: []pb.Role{pb.Role_ROLE_WORKER, pb.Role_ROLE_CLIENT}}, "worker,client"},
		{&pb.Member{Role: pb.Role_ROLE_CLIENT, Roles: []pb.Role{pb.Role_ROLE_CLIENT}}, "client"},
		{&pb.Member{Role: pb.Role_ROLE_CLIENT}, "client"},
	} {
		if got := memberRoles(tt.member); got != tt.want {
			t.Errorf("memberRoles(%v) = %q, want %q", tt.member, got, tt.want)
		}
	}
}

// A worker that redeems a client invitation goes on working, and may now
// submit jobs to the pool it works for too (#474).
func TestAWorkerThatRedeemsAClientInvitationIsBoth(t *testing.T) {
	addr, _ := startNode(t, "--name", "rig")
	workerDir := t.TempDir()
	startDaemon(t, "--role", "worker", "--coordinator", addr, "--join", invite(t, addr, "worker"), "--data-dir", workerDir, "--name", "both", "--slots", "1")
	waitForOutput(t, "both", "nodes", "--addr", addr)

	// As a worker alone it may not submit work.
	if _, err := as(t, workerDir, "job", "submit", "--addr", addr, "--params", `{"from":0,"to":1000}`); err == nil || !strings.Contains(err.Error(), "admitted as a worker") {
		t.Fatalf("a worker submitting a job: %v, want it refused", err)
	}
	if _, err := as(t, workerDir, "pool", "join", "--addr", addr, invite(t, addr, "client")); err != nil {
		t.Fatal(err)
	}
	if members := mustCLI(t, "pool", "members", "--addr", addr); !strings.Contains(members, "worker,client") {
		t.Errorf("the members after the worker redeemed a client invitation:\n%s", members)
	}
	if out, err := as(t, workerDir, "job", "submit", "--addr", addr, "--params", `{"from":0,"to":1000}`); err != nil || !strings.Contains(out, `"count":168`) {
		t.Errorf("the worker, now a client too, submitting a job: %q, %v", out, err)
	}
	// And it is still the pool's worker.
	waitForOutput(t, "both", "nodes", "--addr", addr)
}
