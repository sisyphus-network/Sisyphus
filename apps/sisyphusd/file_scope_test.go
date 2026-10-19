package main

import (
	"errors"
	"path/filepath"
	"slices"
	"testing"

	"github.com/sisyphus-network/Sisyphus/packages/nodedb"
)

// brokenCatalogue is a list of files that cannot be read.
type brokenCatalogue struct{ fileCatalogue }

func (brokenCatalogue) Files() ([]nodedb.File, error) { return nil, errors.New("disk on fire") }

// Each store sees only the files kept in it, under one list: the node's own
// store, and each pool it has joined in turn (#509).
func TestEachStoreSeesOnlyItsOwnFiles(t *testing.T) {
	db, err := nodedb.Open(filepath.Join(t.TempDir(), "node.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	pool := "A"
	joined := poolFileList{files: db, pool: func() string { return pool }}
	own := poolFileList{files: db, pool: func() string { return "" }}
	names := func(list poolFileList) []string {
		t.Helper()
		files, err := list.Files()
		if err != nil {
			t.Fatal(err)
		}
		var named []string
		for _, file := range files {
			named = append(named, file.Name)
		}
		slices.Sort(named)
		return named
	}
	for _, add := range []struct {
		list poolFileList
		file nodedb.File
	}{
		{own, nodedb.File{CID: "mine", Name: "mine.txt"}},
		{joined, nodedb.File{CID: "a", Name: "a.txt", Pool: "ignored"}},
	} {
		if err := add.list.AddFile(add.file); err != nil {
			t.Fatal(err)
		}
	}
	pool = "B"
	if err := joined.AddFile(nodedb.File{CID: "b", Name: "b.txt"}); err != nil {
		t.Fatal(err)
	}

	if got := names(joined); !slices.Equal(got, []string{"b.txt"}) {
		t.Errorf("in pool B the list is %q", got)
	}
	if got := names(own); !slices.Equal(got, []string{"mine.txt"}) {
		t.Errorf("in the node's own store the list is %q", got)
	}
	// Another store's file is not this one's to forget.
	if err := joined.RemoveFile("a"); err != nil {
		t.Fatal(err)
	}
	pool = "A"
	if got := names(joined); !slices.Equal(got, []string{"a.txt"}) {
		t.Errorf("back in pool A the list is %q", got)
	}
	if err := joined.RemoveFile("a"); err != nil || len(names(joined)) != 0 {
		t.Errorf("removing a file in its own pool: %v, list %q", err, names(joined))
	}

	broken := poolFileList{files: brokenCatalogue{}, pool: func() string { return "" }}
	if _, err := broken.Files(); err == nil {
		t.Error("listing from a broken catalogue")
	}
	if err := broken.RemoveFile("a"); err == nil {
		t.Error("removing from a broken catalogue")
	}
}
