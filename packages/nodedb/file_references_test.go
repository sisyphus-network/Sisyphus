package nodedb

import (
	"reflect"
	"testing"
)

func TestFileReferencesSurviveReopenAndRepeatedUploads(t *testing.T) {
	db, path := newDB(t)
	if err := db.AddFile(File{CID: "same", Name: "first"}); err != nil {
		t.Fatal(err)
	}
	for _, owner := range []string{"chat:b", "chat:a", "chat:a"} {
		if err := db.AddFileReference("same", owner); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.AddFile(File{CID: "same", Name: "renamed"}); err != nil {
		t.Fatal(err)
	}
	db = reopen(t, db, path)
	if got, err := db.FileReferences("same"); err != nil || !reflect.DeepEqual(got, []string{"chat:a", "chat:b"}) {
		t.Fatalf("owners: %v, %v", got, err)
	}
	for range 2 {
		if err := db.RemoveFileReference("same", "chat:a"); err != nil {
			t.Fatal(err)
		}
	}
	if got, _ := db.FileReferences("same"); !reflect.DeepEqual(got, []string{"chat:b"}) {
		t.Fatalf("other owner lost: %v", got)
	}
	if err := db.RemoveFile("same"); err == nil {
		t.Fatal("referenced catalogue entry was removed")
	}
	if err := db.RemoveFileReference("same", "chat:b"); err != nil {
		t.Fatal(err)
	}
	if err := db.RemoveFile("same"); err != nil {
		t.Fatal(err)
	}
}

func TestFileReferencesRejectMissingFilesAndEmptyOwners(t *testing.T) {
	db, _ := newDB(t)
	if err := db.AddFileReference("missing", "chat:a"); err == nil {
		t.Fatal("dangling reference allowed")
	}
	if err := db.AddFile(File{CID: "kept"}); err != nil {
		t.Fatal(err)
	}
	if err := db.AddFileReference("kept", ""); err == nil {
		t.Fatal("empty owner allowed")
	}
}

func TestClosedFileReferenceDatabaseReportsErrors(t *testing.T) {
	db, _ := newDB(t)
	db.Close()
	if err := db.AddFileReference("a", "b"); err == nil {
		t.Fatal("closed retain succeeded")
	}
	if err := db.RemoveFileReference("a", "b"); err == nil {
		t.Fatal("closed release succeeded")
	}
	if _, err := db.FileReferences("a"); err == nil {
		t.Fatal("closed lookup succeeded")
	}
}
