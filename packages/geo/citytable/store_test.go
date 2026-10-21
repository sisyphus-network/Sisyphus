package citytable

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// publisher serves the fixture, gzipped, as the editions given, and a 404
// for any other, and keeps the paths asked for.
func publisher(t *testing.T, editions ...string) (url string, asked func() []string) {
	t.Helper()
	body := gzipped(t, fixture)
	var mu sync.Mutex
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		for _, edition := range editions {
			if r.URL.Path == "/dbip-city-lite-"+edition+".csv.gz" {
				w.Write(body)
				return
			}
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(server.Close)
	return server.URL + "/dbip-city-lite-%s.csv.gz", func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), paths...)
	}
}

func at(date string) func() time.Time {
	return func() time.Time {
		day, _ := time.Parse(time.DateOnly, date)
		return day
	}
}

func TestLastMonthsTableIsDownloadedWhenThisMonthsIsNotOut(t *testing.T) {
	url, asked := publisher(t, "2026-09")
	store := Store{Dir: filepath.Join(t.TempDir(), "data"), URL: url, Now: at("2026-10-01")}
	if status, err := store.Status(); err != nil || status.Installed {
		t.Fatalf("status before downloading %+v, %v", status, err)
	}
	status, err := store.Download(context.Background())
	if err != nil || !status.Installed || status.Edition != "2026-09" || status.Bytes == 0 || time.Since(status.Downloaded) > time.Minute {
		t.Fatalf("downloaded %+v, %v", status, err)
	}
	if got := strings.Join(asked(), " "); got != "/dbip-city-lite-2026-10.csv.gz /dbip-city-lite-2026-09.csv.gz" {
		t.Errorf("asked for %s", got)
	}
	if info, err := os.Stat(filepath.Join(store.Dir, FileName)); err != nil || info.Mode().Perm()&0o077 != 0 {
		t.Errorf("the table's file %v, %v", info, err)
	}
	table, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	checkFixture(t, table)
	if err := store.Remove(); err != nil {
		t.Fatal(err)
	}
	if status, err := store.Status(); err != nil || status.Installed {
		t.Fatalf("status after removing %+v, %v", status, err)
	}
	if err := store.Remove(); err != nil {
		t.Errorf("removing what is not there: %v", err)
	}
	if _, err := store.Load(); err == nil {
		t.Error("loaded a table that is not there")
	}
}

func TestThisMonthsTableIsTakenAndJanuaryLooksBackToDecember(t *testing.T) {
	url, _ := publisher(t, "2027-01", "2026-12")
	store := Store{Dir: t.TempDir(), URL: url, Now: at("2027-01-31")}
	if status, err := store.Download(context.Background()); err != nil || status.Edition != "2027-01" {
		t.Fatalf("downloaded %+v, %v", status, err)
	}
	url, asked := publisher(t, "2026-12")
	store.URL = url
	if status, err := store.Download(context.Background()); err != nil || status.Edition != "2026-12" {
		t.Fatalf("downloaded %+v, %v", status, err)
	}
	if got := strings.Join(asked(), " "); got != "/dbip-city-lite-2027-01.csv.gz /dbip-city-lite-2026-12.csv.gz" {
		t.Errorf("asked for %s", got)
	}
}

func TestADownloadThatFailsLeavesTheTableAsItWas(t *testing.T) {
	url, _ := publisher(t, "2026-10")
	dir := t.TempDir()
	store := Store{Dir: dir, URL: url, Now: at("2026-10-21")}
	if _, err := store.Download(context.Background()); err != nil {
		t.Fatal(err)
	}
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/fails":
			http.Error(w, "no", http.StatusInternalServerError)
		case "/garbage":
			w.Write([]byte("not,a,table\n"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(broken.Close)
	gone := httptest.NewServer(http.NotFoundHandler())
	gone.Close()
	for template, want := range map[string]string{
		broken.URL + "/fails?%s":   "500 Internal Server Error",
		broken.URL + "/garbage?%s": "line 1",
		broken.URL + "/none/%s":    "no city table for 2026-10 or 2026-09",
		gone.URL + "/%s":           "download the city table",
		"http://\x7f/%s":           "invalid control character",
	} {
		store.URL = template
		if _, err := store.Download(context.Background()); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v; want %q", template, err, want)
		}
	}
	if status, err := store.Status(); err != nil || status.Edition != "2026-10" {
		t.Errorf("the table after failed downloads %+v, %v", status, err)
	}
}

// Without a URL, the table is asked of DB-IP.
func TestTheTableIsAskedOfDBIPUnlessToldOtherwise(t *testing.T) {
	var asked []string
	store := Store{Dir: t.TempDir(), Client: &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		asked = append(asked, r.URL.String())
		return &http.Response{StatusCode: http.StatusNotFound, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
	})}}
	store.Download(context.Background())
	year, month, _ := time.Now().UTC().Date()
	this := time.Date(year, month, 1, 0, 0, 0, 0, time.UTC).Format("2006-01")
	if len(asked) != 2 || asked[0] != "https://download.db-ip.com/free/dbip-city-lite-"+this+".csv.gz" {
		t.Errorf("asked %v", asked)
	}
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestATableThatCannotBeKeptIsRefused(t *testing.T) {
	url, _ := publisher(t, "2026-10")
	file := filepath.Join(t.TempDir(), "file")
	os.WriteFile(file, nil, 0o600)
	readOnly := t.TempDir()
	os.Chmod(readOnly, 0o500)
	t.Cleanup(func() { os.Chmod(readOnly, 0o700) })
	for name, dir := range map[string]string{"under a file": filepath.Join(file, "data"), "read-only": readOnly} {
		store := Store{Dir: dir, URL: url, Now: at("2026-10-21")}
		if _, err := store.Download(context.Background()); err == nil {
			t.Errorf("%s: kept", name)
		}
	}
	// The table's name taken by a directory with something in it.
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, FileName, "x"), 0o700)
	store := Store{Dir: dir, URL: url, Now: at("2026-10-21")}
	if _, err := store.Download(context.Background()); err == nil {
		t.Error("kept in place of a directory")
	}
	if err := store.Remove(); err == nil {
		t.Error("removed a directory")
	}
	if left, _ := filepath.Glob(filepath.Join(dir, FileName+".*")); len(left) != 0 {
		t.Errorf("left behind %v", left)
	}
	failed := errors.New("disk full")
	if err := writeAtomically(dir, "other", func(io.Writer) error { return failed }); err != failed {
		t.Errorf("a failed write gave %v", err)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "other*")); len(left) != 0 {
		t.Errorf("left behind %v", left)
	}
}

func TestStatusSaysWhenTheFileCannotBeRead(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	os.WriteFile(file, nil, 0o600)
	if _, err := (Store{Dir: file}).Status(); err == nil {
		t.Error("a directory that is a file")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, FileName)
	os.WriteFile(path, []byte("something else entirely"), 0o600)
	if _, err := (Store{Dir: dir}).Status(); err == nil {
		t.Error("a file that is not a table")
	}
	os.Chmod(path, 0)
	if _, err := (Store{Dir: dir}).Status(); err == nil {
		t.Error("a file that cannot be opened")
	}
}

func TestAFinderLooksUpOnlyWhatItHasNotAndForgetsWhenTheTableChanges(t *testing.T) {
	url, _ := publisher(t, "2026-10")
	store := Store{Dir: t.TempDir(), URL: url, Now: at("2026-10-21")}
	finder := &Finder{Store: store}
	brisbaneAddr := netip.MustParseAddr("1.0.0.1")
	if _, ok := finder.Find(brisbaneAddr); ok {
		t.Fatal("found with no table")
	}
	if _, err := store.Download(context.Background()); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if got, ok := finder.Find(brisbaneAddr); !ok || got != brisbane {
			t.Fatalf("found %+v, %v", got, ok)
		}
		if _, ok := finder.Find(netip.MustParseAddr("1.0.4.1")); ok {
			t.Fatal("found an address in no range")
		}
	}
	// More addresses than are kept.
	for i := range keptAnswers + 1 {
		finder.Find(netip.AddrFrom4([4]byte{1, 0, 1, byte(i)}))
	}
	if got, ok := finder.Find(brisbaneAddr); !ok || got != brisbane {
		t.Fatalf("found %+v, %v after many", got, ok)
	}
	// A damaged table is a different file, and finds nothing.
	os.WriteFile(filepath.Join(store.Dir, FileName), []byte(magic+"\x07"+"2026-10"), 0o600)
	if _, ok := finder.Find(brisbaneAddr); ok {
		t.Error("found in a damaged table")
	}
	store.Remove()
	if _, ok := finder.Find(brisbaneAddr); ok {
		t.Error("found after the table was removed")
	}
}
