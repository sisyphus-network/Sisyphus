package nodedb

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestForgettingAPoolForgetsOnlyThatPool(t *testing.T) {
	db, file := newDB(t)
	now := time.Unix(1_800_000_000, 0)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	// "left" is the pool the node leaves; "other" is one it has used too,
	// and "" its own.
	must(db.AddFileInPool("left", File{CID: "only-there", Name: "a.txt"}))
	must(db.AddFileInPool("left", File{CID: "attached", Name: "b.txt"}))
	must(db.AddFileInPool("left", File{CID: "both", Name: "c.txt"}))
	must(db.AddFileInPool("other", File{CID: "both", Name: "c-elsewhere.txt"}))
	must(db.AddFileInPool("", File{CID: "own", Name: "d.txt"}))
	must(db.AddFileReference("attached", "chat:1"))
	must(db.HoldPoolFile("left", "only-there", "user", time.Time{}, now))
	must(db.HoldPoolFile("other", "both", "user", time.Time{}, now))
	must(db.AddPoolJob("left", "job-there", now))
	must(db.AddPoolJob("other", "job-elsewhere", now))
	must(db.AddBootstrapPeer(BootstrapPeer{PeerID: "left", Address: "/ip4/192.0.2.1/tcp/4001/p2p/left"}))
	must(db.AddBootstrapPeer(BootstrapPeer{PeerID: "friend", Address: "/ip4/192.0.2.2/tcp/4001/p2p/friend"}))
	must(db.SetWorksFor("left", true, now))
	must(db.SetWorksFor("other", true, now))

	must(db.ForgetPool("left"))
	// Again, and for a pool never joined: neither is an error.
	must(db.ForgetPool("left"))
	must(db.ForgetPool("never"))
	db = reopen(t, db, file)

	if got, err := db.FilesInPool("left"); err != nil || len(got) != 0 {
		t.Errorf("files listed in the pool left: %v, %v", got, err)
	}
	if got, err := db.FilesInPool("other"); err != nil || len(got) != 1 || got[0].Name != "c-elsewhere.txt" {
		t.Errorf("files listed in the other pool: %v, %v", got, err)
	}
	if got, err := db.FilesInPool(""); err != nil || len(got) != 1 || got[0].CID != "own" {
		t.Errorf("files listed in the node's own pool: %v, %v", got, err)
	}
	// The file a chat attaches is still known, so the chat can say what it
	// was; the one only that pool had is gone.
	var cids []string
	files, err := db.Files()
	must(err)
	for _, f := range files {
		cids = append(cids, f.CID)
	}
	if want := []string{"attached", "both", "own"}; !sameSet(cids, want) {
		t.Errorf("files known after leaving: %v, want %v", cids, want)
	}
	if got, err := db.PoolFileHolds("left", "only-there", now); err != nil || len(got) != 0 {
		t.Errorf("holds in the pool left: %v, %v", got, err)
	}
	if got, err := db.PoolFileHolds("other", "both", now); err != nil || len(got) != 1 {
		t.Errorf("holds in the other pool: %v, %v", got, err)
	}
	if got, err := db.PoolJobs("left"); err != nil || len(got) != 0 {
		t.Errorf("jobs in the pool left: %v, %v", got, err)
	}
	if got, err := db.PoolJobs("other"); err != nil || !reflect.DeepEqual(got, []string{"job-elsewhere"}) {
		t.Errorf("jobs in the other pool: %v, %v", got, err)
	}
	peers, err := db.BootstrapPeers()
	if err != nil || len(peers) != 1 || peers[0].PeerID != "friend" {
		t.Errorf("bootstrap peers after leaving: %v, %v", peers, err)
	}
	if got, err := db.WorksFor(); err != nil || !reflect.DeepEqual(got, []string{"other"}) {
		t.Errorf("works for after leaving: %v, %v", got, err)
	}

	db.Close()
	if err := db.ForgetPool("other"); err == nil || !strings.Contains(err.Error(), "forget the pool") {
		t.Errorf("on a closed database: %v", err)
	}
}

func sameSet(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	seen := make(map[string]bool, len(got))
	for _, s := range got {
		seen[s] = true
	}
	for _, s := range want {
		if !seen[s] {
			return false
		}
	}
	return true
}
