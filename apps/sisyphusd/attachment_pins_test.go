package main

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ipfs/go-cid"
	"github.com/sisyphus-network/Sisyphus/packages/nodedb"
	"github.com/sisyphus-network/Sisyphus/packages/storage"
)

type failingAttachmentPins struct {
	*storage.Store
	failRelease bool
}

func (s *failingAttachmentPins) Unpin(owner string, cids ...cid.Cid) error {
	if s.failRelease {
		return errors.New("temporary pin store failure")
	}
	return s.Store.Unpin(owner, cids...)
}

func TestFailedAttachmentReleaseIsRetriedAfterChatDeletion(t *testing.T) {
	db, err := nodedb.Open(filepath.Join(t.TempDir(), "node.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	store := &failingAttachmentPins{Store: storage.NewMemory()}
	c, err := store.Put(context.Background(), strings.NewReader("shared content"))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AddFile(nodedb.File{CID: c.String()}); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateChat(nodedb.Chat{ID: "owner", Created: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := db.RetainChatFile("owner", c.String()); err != nil {
		t.Fatal(err)
	}
	a := &assistant{store: db, filePins: store}
	if err := a.replayFilePins(context.Background()); err != nil {
		t.Fatal(err)
	}
	// An independent user pin must not be released by chat cleanup.
	if err := store.Pin(context.Background(), "user", time.Time{}, c); err != nil {
		t.Fatal(err)
	}
	store.failRelease = true
	if err := a.DeleteChat("owner"); err == nil {
		t.Fatal("release failure was hidden")
	}
	if chats, err := db.Chats(); err != nil || len(chats) != 0 {
		t.Fatalf("chat deletion not committed: %v, %v", chats, err)
	}
	if ops, err := db.PendingFilePins(); err != nil || len(ops) != 1 || ops[0].Keep {
		t.Fatalf("release operation lost: %v, %v", ops, err)
	}
	store.failRelease = false
	// A replacement assistant models a restart; the queue is in SQLite.
	a = &assistant{store: db, filePins: store}
	if err := a.replayFilePins(context.Background()); err != nil {
		t.Fatal(err)
	}
	if ops, err := db.PendingFilePins(); err != nil || len(ops) != 0 {
		t.Fatalf("replayed release not acknowledged: %v, %v", ops, err)
	}
	userHeld := false
	for _, pin := range store.Pins() {
		if pin.Owner == "chat:owner" {
			t.Fatal("deleted chat still pins content")
		}
		userHeld = userHeld || pin.Owner == "user"
	}
	if !userHeld {
		t.Fatal("cleanup released independent user pin")
	}
}
