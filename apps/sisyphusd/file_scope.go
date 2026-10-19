package main

import "github.com/sisyphus-network/Sisyphus/packages/nodedb"

// fileCatalogue is where a node keeps the names of every file it has
// stored, in whichever store. A nodedb.DB is one.
type fileCatalogue interface {
	Files() ([]nodedb.File, error)
	AddFile(nodedb.File) error
	RemoveFile(cid string) error
}

// poolFileList is a node's list of files as one store sees it: those kept
// in its own store, or in that of the pool it has joined now. A file kept in
// another pool's store is left out, and left alone, so that rejoining that
// pool shows it again (#509).
type poolFileList struct {
	files fileCatalogue
	// pool is the ID of the coordinator whose store the list is of, or
	// empty for the node's own.
	pool func() string
}

func (l poolFileList) Files() ([]nodedb.File, error) {
	all, err := l.files.Files()
	if err != nil {
		return nil, err
	}
	pool := l.pool()
	var here []nodedb.File
	for _, file := range all {
		if file.Pool == pool {
			here = append(here, file)
		}
	}
	return here, nil
}

func (l poolFileList) AddFile(file nodedb.File) error {
	file.Pool = l.pool()
	return l.files.AddFile(file)
}

// RemoveFile forgets a file kept in this store. One kept in another is not
// this store's to forget: what keeps it there is that pool's.
func (l poolFileList) RemoveFile(cid string) error {
	all, err := l.files.Files()
	if err != nil {
		return err
	}
	for _, file := range all {
		if file.CID == cid && file.Pool != l.pool() {
			return nil
		}
	}
	return l.files.RemoveFile(cid)
}
