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
	new := File{CID: "bafynew", Name: "new.txt", Size: 1 << 40, Stored: at.Add(time.Minute), Private: true}
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

func TestStoredFilesKeepTheirPoolAndNameSeparate(t *testing.T) {
	db, _ := newDB(t)
	at := submitted.Local()
	for _, entry := range []struct {
		pool string
		file File
	}{
		{"coordinator-a", File{CID: "shared", Name: "a.txt", Stored: at}},
		{"coordinator-b", File{CID: "shared", Name: "b.txt", Stored: at.Add(time.Minute)}},
	} {
		if err := db.AddFileInPool(entry.pool, entry.file); err != nil {
			t.Fatal(err)
		}
	}
	for pool, want := range map[string]string{"coordinator-a": "a.txt", "coordinator-b": "b.txt"} {
		files, err := db.FilesInPool(pool)
		if err != nil || len(files) != 1 || files[0].Name != want {
			t.Errorf("files in %s: %v, %v; want name %q", pool, files, err, want)
		}
	}
	if err := db.RemoveFileInPool("coordinator-a", "shared"); err != nil {
		t.Fatal(err)
	}
	if files, err := db.FilesInPool("coordinator-a"); err != nil || len(files) != 0 {
		t.Errorf("files in removed pool scope: %v, %v", files, err)
	}
	if files, err := db.FilesInPool("coordinator-b"); err != nil || len(files) != 1 || files[0].Name != "b.txt" {
		t.Errorf("files in remaining pool scope: %v, %v", files, err)
	}
}

func TestFilePoolScopesMigrateFromJoinedPoolHolds(t *testing.T) {
	db, file := newDB(t)
	if _, err := db.sql.Exec(`DROP TABLE file_pool_scopes; DELETE FROM schema_migrations WHERE version = 33;
		INSERT INTO files (cid, name, size_bytes, stored_at_ns, private) VALUES
			('joined-file', 'joined.txt', 12, 1, 0), ('local-file', 'local.txt', 5, 2, 0);
		INSERT INTO pool_file_holds (coordinator_id, cid, owner, expires_ns) VALUES ('coordinator-a', 'joined-file', 'user', 0)`); err != nil {
		t.Fatal(err)
	}
	db = reopen(t, db, file)
	for pool, want := range map[string]string{"coordinator-a": "joined.txt", "": "local.txt"} {
		files, err := db.FilesInPool(pool)
		if err != nil || len(files) != 1 || files[0].Name != want {
			t.Errorf("migrated files in %q: %v, %v; want %q", pool, files, err, want)
		}
	}
}

func TestFileListFailuresAreReported(t *testing.T) {
	db, _ := newDB(t)
	loosen(t, db, "files", "cid, name, size_bytes, stored_at_ns, private")
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
