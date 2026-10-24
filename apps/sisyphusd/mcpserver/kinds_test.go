package mcpserver

import (
	"slices"
	"testing"

	nodepb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/node/v1"
)

// Every call in the node's API has a kind, and the table names no call the
// API does not have, so a call added or renamed is decided on (#650).
func TestEveryNodeCallHasAKind(t *testing.T) {
	methods := nodepb.File_sisyphus_node_v1_node_proto.Services().ByName("NodeService").Methods()
	var calls []string
	for i := range methods.Len() {
		name := string(methods.Get(i).Name())
		calls = append(calls, name)
		if _, ok := rpcKinds[name]; !ok {
			t.Errorf("NodeService.%s has no kind in rpcKinds", name)
		}
	}
	for name := range rpcKinds {
		if !slices.Contains(calls, name) {
			t.Errorf("rpcKinds names %s, which NodeService does not have", name)
		}
	}
}

// A destructive tool is offered as an admin one is: only when the owner
// allowed admin tools, and never to a read-only server.
func TestDestructiveToolsAreOfferedAsAdminOnesAre(t *testing.T) {
	for _, cfg := range []Config{{}, {Admin: true}, {ReadOnly: true}, {ReadOnly: true, Admin: true}} {
		s := &server{cfg}
		if got, want := s.offers(destructive), s.offers(admin); got != want {
			t.Errorf("%+v: destructive offered %t, admin %t", cfg, got, want)
		}
	}
}
