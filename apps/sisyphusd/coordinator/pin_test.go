package coordinator

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/ipfs/go-cid"

	jobmodel "github.com/sisyphus-network/Sisyphus/packages/job-model"
	"github.com/sisyphus-network/Sisyphus/packages/storage"
)

// refusing is a store that will not pin anything.
type refusing struct{ *storage.Store }

func (refusing) Pin(context.Context, string, time.Time, ...cid.Cid) error {
	return errors.New("pin file is read-only")
}

func TestAJobKeepsWhatThePoolHoldsThoughItNamesABlobItDoesNot(t *testing.T) {
	ctx := context.Background()
	store := storage.NewMemory()
	result, _ := store.Put(ctx, strings.NewReader("the result"))
	absent, _ := storage.NewMemory().Put(ctx, strings.NewReader("held elsewhere"))
	expires := time.Now().Add(time.Hour)
	job := &jobmodel.Job{ID: "j"}

	pinned := func(store *storage.Store) map[string]time.Time {
		kept := make(map[string]time.Time)
		for _, pin := range store.Pins() {
			if pin.Owner == owner(job) {
				kept[pin.CID.String()] = pin.Expires
			}
		}
		return kept
	}
	coordinator := func(s Store) (*Coordinator, *bytes.Buffer) {
		logs := new(bytes.Buffer)
		return &Coordinator{ctx: ctx, store: s, log: slog.New(slog.NewTextHandler(logs, nil))}, logs
	}

	// What is all held is pinned at once, and nothing is said.
	whole := storage.NewMemory()
	held, _ := whole.Put(ctx, strings.NewReader("the result"))
	c, logs := coordinator(whole)
	c.pin(job, expires, []string{held.String()})
	if kept := pinned(whole); len(kept) != 1 || logs.Len() != 0 {
		t.Errorf("a job naming only what is held keeps %v and logged:\n%s", kept, logs)
	}

	// One blob the pool does not hold costs the job none of the others.
	c, logs = coordinator(store)
	c.pin(job, expires, []string{absent.String(), result.String()})
	if kept := pinned(store); len(kept) != 1 || !kept[result.String()].Equal(expires) {
		t.Errorf("the job keeps %v, want only its result until %v", kept, expires)
	}
	if !strings.Contains(logs.String(), "a job names a blob the pool does not hold") || !strings.Contains(logs.String(), absent.String()) {
		t.Errorf("the blob not held was not logged:\n%s", logs)
	}

	// A store that cannot pin at all is logged, whether or not every blob
	// is held, and not asked twice for nothing.
	for name, cids := range map[string][]string{
		"every blob held": {result.String()},
		"one not held":    {absent.String(), result.String()},
	} {
		c, logs := coordinator(refusing{storage.NewMemory()})
		c.store.(refusing).Put(ctx, strings.NewReader("the result"))
		c.pin(job, expires, cids)
		if strings.Count(logs.String(), "could not pin a job's blobs") != 1 {
			t.Errorf("%s: the refusal was logged %d times, want once:\n%s", name, strings.Count(logs.String(), "could not pin a job's blobs"), logs)
		}
	}
}
