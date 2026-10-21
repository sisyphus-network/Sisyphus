package citytable

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// DefaultURL is where DB-IP publishes the table, with the month, 2026-10,
// in place of %s.
const DefaultURL = "https://download.db-ip.com/free/dbip-city-lite-%s.csv.gz"

// FileName is the table's file in the node's data directory.
const FileName = "dbip-city-lite.bin"

// maxDownload is more than DB-IP's gzipped table will be for years: it is
// some 90 MB.
const maxDownload = 1 << 30

// Store keeps the table in a directory, the node's data directory.
type Store struct {
	Dir string
	// URL is where the table is published, with the month in place of %s,
	// or DefaultURL if empty. Client, if set, is what asks for it, and Now,
	// if set, what says which month it is.
	URL    string
	Client *http.Client
	Now    func() time.Time
}

// Status says whether the table is there and, if so, which edition it is,
// how many bytes it takes on disk and when it was downloaded.
type Status struct {
	Installed  bool
	Edition    string
	Bytes      int64
	Downloaded time.Time
}

func (s Store) path() string { return filepath.Join(s.Dir, FileName) }

// Status says whether the table is there.
func (s Store) Status() (Status, error) {
	info, err := os.Stat(s.path())
	if errors.Is(err, fs.ErrNotExist) {
		return Status{}, nil
	}
	if err != nil {
		return Status{}, err
	}
	edition, err := editionOf(s.path())
	if err != nil {
		return Status{}, err
	}
	return Status{Installed: true, Edition: edition, Bytes: info.Size(), Downloaded: info.ModTime()}, nil
}

// editionOf reads which edition the table in a file is.
func editionOf(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	return readHeader(bufio.NewReader(f))
}

// Download fetches this month's table, or last month's if this month's is
// not published yet, and keeps it in place of any there was. It waits for
// the whole of it. DB-IP learns the machine's address from the request.
func (s Store) Download(ctx context.Context) (Status, error) {
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	year, month, _ := now().UTC().Date()
	this := time.Date(year, month, 1, 0, 0, 0, 0, time.UTC)
	editions := []string{this.Format("2006-01"), this.AddDate(0, -1, 0).Format("2006-01")}
	for _, edition := range editions {
		table, err := s.fetch(ctx, edition)
		if errors.Is(err, errUnpublished) {
			continue
		}
		if err != nil {
			return Status{}, err
		}
		if err := writeAtomically(s.Dir, FileName, func(w io.Writer) error { return table.Write(w, edition) }); err != nil {
			return Status{}, err
		}
		return s.Status()
	}
	return Status{}, fmt.Errorf("DB-IP has published no city table for %s or %s", editions[0], editions[1])
}

var errUnpublished = errors.New("not published")

// fetch downloads and reads one month's table.
func (s Store) fetch(ctx context.Context, edition string) (*Table, error) {
	template, client := s.URL, s.Client
	if template == "" {
		template = DefaultURL
	}
	if client == nil {
		client = http.DefaultClient
	}
	url := fmt.Sprintf(template, edition)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	res, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download the city table: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusNotFound {
		return nil, errUnpublished
	}
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download the city table from %s: %s", url, res.Status)
	}
	return Parse(io.LimitReader(res.Body, maxDownload))
}

// writeAtomically writes a file by write, so that it is there whole or not
// changed at all.
func writeAtomically(dir, name string, write func(io.Writer) error) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, name+".*")
	if err != nil {
		return err
	}
	err = write(f)
	if closed := f.Close(); err == nil {
		err = closed
	}
	if err == nil {
		err = os.Rename(f.Name(), filepath.Join(dir, name))
	}
	if err != nil {
		os.Remove(f.Name())
	}
	return err
}

// Remove deletes the table, if it is there.
func (s Store) Remove() error {
	if err := os.Remove(s.path()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// Load reads the table.
func (s Store) Load() (*Table, error) {
	f, err := os.Open(s.path())
	if err != nil {
		return nil, err
	}
	defer f.Close()
	table, _, err := Read(f)
	return table, err
}

// Finder looks addresses up in a store's table, keeping what it found.
// The table is read only to look up an address not looked up before, and
// let go of after: a node asks about a handful of its own addresses, and
// the table takes tens of megabytes. What it found is forgotten when the
// table's file changes or is removed, whoever changes it.
type Finder struct {
	Store Store

	mu    sync.Mutex
	stamp stamp
	found map[netip.Addr]*Place
}

type stamp struct {
	size     int64
	modified time.Time
}

// keptAnswers is how many addresses a Finder keeps what it found for.
const keptAnswers = 64

// Find returns where the table puts addr, and false if it puts it nowhere,
// or there is no table or it cannot be read.
func (f *Finder) Find(addr netip.Addr) (Place, bool) {
	info, err := os.Stat(f.Store.path())
	if err != nil {
		return Place{}, false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	now := stamp{info.Size(), info.ModTime()}
	if now != f.stamp || len(f.found) >= keptAnswers {
		f.stamp, f.found = now, map[netip.Addr]*Place{}
	}
	if place, asked := f.found[addr]; asked {
		return found(place)
	}
	var place *Place
	if table, err := f.Store.Load(); err == nil {
		if p, ok := table.Lookup(addr); ok {
			place = &p
		}
	}
	f.found[addr] = place
	return found(place)
}

func found(place *Place) (Place, bool) {
	if place == nil {
		return Place{}, false
	}
	return *place, true
}
