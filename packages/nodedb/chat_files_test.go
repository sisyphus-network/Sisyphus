package nodedb

import (
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestChatFileOwnershipSurvivesRestartAndReleasesOnlyDeletedChat(t *testing.T) {
	db, path := newDB(t)
	for _, id := range []string{"first", "second"} {
		if err := db.CreateChat(Chat{ID: id, Created: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.AddFile(File{CID: "shared", Name: "attachment"}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"first", "second", "first"} {
		if err := db.RetainChatFile(id, "shared"); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.AddFileReference("shared", "user:explicit"); err != nil {
		t.Fatal(err)
	}
	db = reopen(t, db, path)
	if err := db.DeleteChat("first"); err != nil {
		t.Fatal(err)
	}
	owners, err := db.FileReferences("shared")
	if err != nil || !reflect.DeepEqual(owners, []string{"chat:second", "user:explicit"}) {
		t.Fatalf("remaining owners: %v, %v", owners, err)
	}
	if err := db.RetainChatFile("first", "shared"); err == nil {
		t.Fatal("retained a file for a deleted chat")
	}
	if err := db.RetainChatFile("second", "absent"); err == nil {
		t.Fatal("retained an unknown file")
	}
	if err := db.DeleteChat("second"); err != nil {
		t.Fatal(err)
	}
	if err := db.RemoveFile("shared"); !errors.Is(err, ErrFileReferenced) {
		t.Fatal("chat deletion discarded an independent user owner")
	}
	if err := db.RemoveFileReference("shared", "user:explicit"); err != nil {
		t.Fatal(err)
	}
	if err := db.RemoveFile("shared"); err != nil {
		t.Fatal(err)
	}
}

func TestFailedChatDeletionKeepsFileOwnership(t *testing.T) {
	db, _ := newDB(t)
	if err := db.CreateChat(Chat{ID: "kept", Created: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := db.AddFile(File{CID: "kept-file"}); err != nil {
		t.Fatal(err)
	}
	if err := db.RetainChatFile("kept", "kept-file"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.sql.Exec(`CREATE TRIGGER reject_chat_delete BEFORE DELETE ON chats BEGIN SELECT RAISE(ABORT, 'test failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteChat("kept"); err == nil {
		t.Fatal("expected deletion to fail")
	}
	owners, err := db.FileReferences("kept-file")
	if err != nil || !reflect.DeepEqual(owners, []string{"chat:kept"}) {
		t.Fatalf("failed deletion released ownership: %v, %v", owners, err)
	}
	db.Close()
	if err := db.RetainChatFile("kept", "kept-file"); err == nil {
		t.Fatal("retaining ownership on a closed database succeeded")
	}
}
