package nodedb

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestStoredFilesAreKeptAcrossReopening(t *testing.T) {
	db, file := newDB(t)
	if files, err := db.Files(); err != nil || len(files) != 0 {
		t.Fatalf("a new list of files: %v, %v", files, err)
	}
	// Times come back in the machine's own zone.
	at := submitted.Local()
	old := File{CID: "bafyold", Name: "old.txt", Size: 10, Stored: at}
	new := File{CID: "bafynew", Name: "new.txt", Size: 1 << 40, Stored: at.Add(time.Minute)}
	for _, f := range []File{old, new} {
		if err := db.AddFile(f); err != nil {
			t.Fatal(err)
		}
	}
	db = reopen(t, db, file)
	got, err := db.Files()
	if err != nil || !reflect.DeepEqual(got, []File{new, old}) {
		t.Fatalf("the files after reopening: %v, %v", got, err)
	}

	// The same content stored again takes the new name and moves to the top.
	renamed := File{CID: "bafyold", Name: "renamed.txt", Size: 10, Stored: at.Add(time.Hour)}
	if err := db.AddFile(renamed); err != nil {
		t.Fatal(err)
	}
	if got, _ := db.Files(); !reflect.DeepEqual(got, []File{renamed, new}) {
		t.Errorf("after storing one again: %v", got)
	}

	for _, cid := range []string{"bafynew", "bafynew"} {
		if err := db.RemoveFile(cid); err != nil {
			t.Fatal(err)
		}
	}
	if got, _ := db.Files(); !reflect.DeepEqual(got, []File{renamed}) {
		t.Errorf("after removing one: %v", got)
	}
}

func TestFileListFailuresAreReported(t *testing.T) {
	db, _ := newDB(t)
	loosen(t, db, "files", "cid, name, size_bytes, stored_at_ns")
	if _, err := db.Files(); err == nil || !strings.Contains(err.Error(), "load files") {
		t.Errorf("with a damaged table: %v", err)
	}
	db.Close()
	if err := db.AddFile(File{CID: "a"}); err == nil || !strings.Contains(err.Error(), "save file") {
		t.Errorf("AddFile on a closed database: %v", err)
	}
	if err := db.RemoveFile("a"); err == nil || !strings.Contains(err.Error(), "remove file") {
		t.Errorf("RemoveFile on a closed database: %v", err)
	}
}
