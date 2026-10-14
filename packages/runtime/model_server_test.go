package runtime

import "testing"

func TestModelServerNilReceiverIsSafe(t *testing.T) {
	var server *ModelServer
	if url, owner := server.Settings(); url != "" || len(owner.Only) != 0 || len(owner.Options) != 0 {
		t.Fatalf("nil settings = %q, %+v", url, owner)
	}
	server.Update(Owner{Only: []string{"model"}})

	if url, owner := (&ModelServer{}).Settings(); url != "" || len(owner.Only) != 0 || len(owner.Options) != 0 {
		t.Fatalf("empty settings = %q, %+v", url, owner)
	}
}
