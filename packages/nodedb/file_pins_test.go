package nodedb

import (
	"testing"
	"time"
)

func TestChatPinOperationsAreDurableAndOrdered(t *testing.T) {
	db, path := newDB(t)
	if err := db.CreateChat(Chat{ID: "chat", Created: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := db.AddFile(File{CID: "shared"}); err != nil {
		t.Fatal(err)
	}
	if err := db.RetainChatFiles("chat", []string{"shared", "absent"}); err == nil {
		t.Fatal("invalid retain succeeded")
	}
	if ops, err := db.PendingFilePins(); err != nil || len(ops) != 0 {
		t.Fatalf("failed retain queued changes: %v, %v", ops, err)
	}
	if err := db.RetainChatFile("chat", "shared"); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteChat("chat"); err != nil {
		t.Fatal(err)
	}
	db = reopen(t, db, path)
	ops, err := db.PendingFilePins()
	if err != nil || len(ops) != 2 {
		t.Fatalf("pending changes: %v, %v", ops, err)
	}
	if !ops[0].Keep || ops[1].Keep || ops[0].Seq >= ops[1].Seq || ops[0].Owner != "chat:chat" || ops[1].CID != "shared" {
		t.Fatalf("changes out of order: %v", ops)
	}
	for i := 0; i < 2; i++ {
		if err := db.CompleteFilePin(ops[0].Seq); err != nil {
			t.Fatal(err)
		}
	}
	left, err := db.PendingFilePins()
	if err != nil || len(left) != 1 || left[0].Seq != ops[1].Seq {
		t.Fatalf("acknowledgement discarded other changes: %v, %v", left, err)
	}
	db.Close()
	if _, err := db.PendingFilePins(); err == nil {
		t.Fatal("closed database returned operations")
	}
	if err := db.CompleteFilePin(ops[1].Seq); err == nil {
		t.Fatal("closed database accepted acknowledgement")
	}
}
