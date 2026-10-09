package main

import (
	"context"
	"errors"
	"log/slog"
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

// flakyPinQueue is a node's database whose queue of pin changes can be made
// to fail.
type flakyPinQueue struct {
	*nodedb.DB
	failPending, failComplete bool
}

func (q *flakyPinQueue) PendingFilePins() ([]nodedb.FilePinOperation, error) {
	if q.failPending {
		return nil, errors.New("the queue cannot be read")
	}
	return q.DB.PendingFilePins()
}

func (q *flakyPinQueue) CompleteFilePin(seq int64) error {
	if q.failComplete {
		return errors.New("the queue cannot be written")
	}
	return q.DB.CompleteFilePin(seq)
}

// chatWithFile returns a database holding a conversation that has been
// given a file under the given name, which need not be a CID.
func chatWithFile(t *testing.T, name string) *nodedb.DB {
	t.Helper()
	db, err := nodedb.Open(filepath.Join(t.TempDir(), "node.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.AddFile(nodedb.File{CID: name}); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateChat(nodedb.Chat{ID: "owner", Created: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := db.RetainChatFile("owner", name); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestAQueuedAttachmentPinStaysQueuedUntilItIsApplied(t *testing.T) {
	ctx := context.Background()
	store := storage.NewMemory()
	c, err := store.Put(ctx, strings.NewReader("an attachment"))
	if err != nil {
		t.Fatal(err)
	}
	queue := &flakyPinQueue{DB: chatWithFile(t, c.String())}
	pinned := func() bool {
		for _, pin := range store.Pins() {
			if pin.Owner == "chat:owner" && pin.CID.Equals(c) {
				return true
			}
		}
		return false
	}

	// With no store to pin in there is nothing to apply, and the change
	// waits for a node that has one.
	if err := (&assistant{store: queue}).replayFilePins(ctx); err != nil {
		t.Fatalf("with no store: %v", err)
	}
	if ops, _ := queue.PendingFilePins(); len(ops) != 1 || !ops[0].Keep {
		t.Fatalf("with no store the queue holds %v", ops)
	}

	a := &assistant{store: queue, filePins: store}
	queue.failPending = true
	if err := a.replayFilePins(ctx); err == nil || !strings.Contains(err.Error(), "the queue cannot be read") {
		t.Errorf("with a queue that cannot be read: %v", err)
	}
	queue.failPending = false
	if pinned() {
		t.Error("a change that could not be read was applied")
	}

	// A change that was applied and could not be taken off the queue is
	// applied again, which changes nothing.
	queue.failComplete = true
	if err := a.replayFilePins(ctx); err == nil || !strings.Contains(err.Error(), "the queue cannot be written") {
		t.Errorf("with a queue that cannot be written: %v", err)
	}
	if ops, _ := queue.PendingFilePins(); len(ops) != 1 || !pinned() {
		t.Errorf("the change is pinned: %v, and the queue holds %v", pinned(), ops)
	}
	queue.failComplete = false
	if err := a.replayFilePins(ctx); err != nil {
		t.Fatal(err)
	}
	if ops, _ := queue.PendingFilePins(); len(ops) != 0 || !pinned() {
		t.Errorf("once applied the change is pinned: %v, and the queue holds %v", pinned(), ops)
	}

	// A conversation that cannot be deleted releases nothing.
	queue.Close()
	if err := a.DeleteChat("owner"); err == nil {
		t.Error("a conversation was deleted from a closed database")
	}
	if !pinned() {
		t.Error("a deletion that failed released the attachment")
	}
}

func TestAQueuedPinOfSomethingThatIsNoCIDIsReported(t *testing.T) {
	a := &assistant{store: chatWithFile(t, "not a CID"), filePins: storage.NewMemory()}
	if err := a.replayFilePins(context.Background()); err == nil {
		t.Error("a pin of something that is no CID was applied")
	}
	if ops, _ := a.store.PendingFilePins(); len(ops) != 1 {
		t.Errorf("the queue holds %v", ops)
	}
}

func TestQueuedAttachmentPinsAreTriedAgainUntilTheyAreApplied(t *testing.T) {
	ctx := context.Background()
	store := &failingAttachmentPins{Store: storage.NewMemory()}
	c, err := store.Put(ctx, strings.NewReader("an attachment"))
	if err != nil {
		t.Fatal(err)
	}
	db := chatWithFile(t, c.String())
	a := &assistant{store: db, filePins: store}
	if err := a.replayFilePins(ctx); err != nil {
		t.Fatal(err)
	}
	// The conversation goes while the store cannot release what it held.
	store.failRelease = true
	if err := a.DeleteChat("owner"); err == nil {
		t.Fatal("release failure was hidden")
	}

	logs := new(syncBuffer)
	replaying, stop := context.WithCancel(ctx)
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		replayFilePinsPeriodically(replaying, a, 5*time.Millisecond, slog.New(slog.NewTextHandler(logs, nil)))
	}()
	const warning = `msg="attachment pins could not be applied and will be retried" error="temporary pin store failure"`
	waitFor(t, func() bool { return strings.Contains(logs.String(), warning) })
	// It is said once, however many rounds fail.
	time.Sleep(30 * time.Millisecond)
	if said := strings.Count(logs.String(), warning); said != 1 {
		t.Errorf("the failure was logged %d times:\n%s", said, logs.String())
	}
	if ops, _ := db.PendingFilePins(); len(ops) != 1 {
		t.Errorf("while the store fails the queue holds %v", ops)
	}

	a.filePinMu.Lock()
	store.failRelease = false
	a.filePinMu.Unlock()
	waitFor(t, func() bool { return strings.Contains(logs.String(), `msg="attachment pins applied"`) })
	if ops, _ := db.PendingFilePins(); len(ops) != 0 {
		t.Errorf("once the store works the queue holds %v", ops)
	}
	for _, pin := range store.Pins() {
		if pin.Owner == "chat:owner" {
			t.Errorf("the deleted conversation still pins %s", pin.CID)
		}
	}
	stop()
	<-stopped

	// Stopped before its first round is over, it says nothing.
	logs = new(syncBuffer)
	replayFilePinsPeriodically(replaying, &assistant{store: &flakyPinQueue{DB: db, failPending: true}, filePins: store}, time.Hour, slog.New(slog.NewTextHandler(logs, nil)))
	if logs.String() != "" {
		t.Errorf("a replay that was stopped logged:\n%s", logs.String())
	}
}
