package main

import (
	"database/sql"
	"errors"
	"flag"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sisyphus-network/Sisyphus/packages/nodedb"
	nodepb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/node/v1"
)

type fakePoolLimitsStore struct {
	limits     nodedb.PoolLimits
	configured bool
	err        error
}

func (s fakePoolLimitsStore) PoolLimits() (nodedb.PoolLimits, bool, error) {
	return s.limits, s.configured, s.err
}

func TestStartupPoolLimits(t *testing.T) {
	defaults := nodedb.PoolLimits{MaxJobsPerClient: 1, MaxTasksPerClient: 2, MaxTaskTimeout: time.Minute, MaxStoreBytesPerClient: 3, MaxUploadBytesPerAttempt: 4}
	saved := nodedb.PoolLimits{MaxJobsPerClient: 10, MaxTasksPerClient: 20, MaxTaskTimeout: time.Hour, MaxStoreBytesPerClient: 30, MaxUploadBytesPerAttempt: 40}

	t.Run("uses defaults when not configured", func(t *testing.T) {
		got, err := startupPoolLimits(fakePoolLimitsStore{}, defaults, poolLimitsFlagOverrides{})
		if err != nil {
			t.Fatal(err)
		}
		if got != defaults {
			t.Fatalf("limits = %#v, want defaults %#v", got, defaults)
		}
	})

	t.Run("uses saved limits when configured", func(t *testing.T) {
		got, err := startupPoolLimits(fakePoolLimitsStore{limits: saved, configured: true}, defaults, poolLimitsFlagOverrides{})
		if err != nil {
			t.Fatal(err)
		}
		if got != saved {
			t.Fatalf("limits = %#v, want saved %#v", got, saved)
		}
	})

	t.Run("returns storage errors", func(t *testing.T) {
		wantErr := errors.New("database unavailable")
		if _, err := startupPoolLimits(fakePoolLimitsStore{err: wantErr}, defaults, poolLimitsFlagOverrides{}); !errors.Is(err, wantErr) {
			t.Fatalf("error = %v, want %v", err, wantErr)
		}
	})

	t.Run("explicit flags override saved limits", func(t *testing.T) {
		jobs, tasks, timeout, store, upload := 0, 5, 90*time.Second, uint64(0), uint64(1<<20)
		got, err := startupPoolLimits(fakePoolLimitsStore{limits: saved, configured: true}, defaults, poolLimitsFlagOverrides{
			MaxJobsPerClient:         &jobs,
			MaxTasksPerClient:        &tasks,
			MaxTaskTimeout:           &timeout,
			MaxStoreBytesPerClient:   &store,
			MaxUploadBytesPerAttempt: &upload,
		})
		if err != nil {
			t.Fatal(err)
		}
		want := nodedb.PoolLimits{MaxTasksPerClient: 5, MaxTaskTimeout: 90 * time.Second, MaxUploadBytesPerAttempt: 1 << 20}
		if got != want {
			t.Fatalf("limits = %#v, want explicit overrides %#v", got, want)
		}
	})

	t.Run("an omitted flag keeps the saved limit", func(t *testing.T) {
		jobs := 7
		got, err := startupPoolLimits(fakePoolLimitsStore{limits: saved, configured: true}, defaults, poolLimitsFlagOverrides{MaxJobsPerClient: &jobs})
		if err != nil {
			t.Fatal(err)
		}
		want := saved
		want.MaxJobsPerClient = 7
		if got != want {
			t.Fatalf("limits = %#v, want %#v", got, want)
		}
	})
}

func TestPoolLimitsFlagOverridesTrackOnlyExplicitFlags(t *testing.T) {
	flags := func(fs *flag.FlagSet) (*int, *int, *time.Duration, *uint64, *uint64) {
		return fs.Int("max-jobs-per-client", 0, ""), fs.Int("max-tasks-per-client", 0, ""), fs.Duration("max-task-timeout", 0, ""),
			fs.Uint64("max-store-bytes-per-client", 0, ""), fs.Uint64("max-upload-bytes-per-attempt", 0, "")
	}
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	jobs, tasks, timeout, store, upload := flags(fs)
	if err := fs.Parse([]string{"--max-jobs-per-client=0", "--max-tasks-per-client=3", "--max-task-timeout=1m", "--max-store-bytes-per-client=0", "--max-upload-bytes-per-attempt=5"}); err != nil {
		t.Fatal(err)
	}
	got := poolLimitsFlagOverridesFrom(fs, jobs, tasks, timeout, store, upload)
	if got.MaxJobsPerClient == nil || *got.MaxJobsPerClient != 0 {
		t.Errorf("explicit zero jobs flag = %v, want pointer to 0", got.MaxJobsPerClient)
	}
	if got.MaxTasksPerClient == nil || *got.MaxTasksPerClient != 3 {
		t.Errorf("explicit tasks flag = %v, want pointer to 3", got.MaxTasksPerClient)
	}
	if got.MaxTaskTimeout == nil || *got.MaxTaskTimeout != time.Minute {
		t.Errorf("explicit timeout flag = %v, want pointer to 1m", got.MaxTaskTimeout)
	}
	if got.MaxStoreBytesPerClient == nil || *got.MaxStoreBytesPerClient != 0 {
		t.Errorf("explicit zero store flag = %v, want pointer to 0", got.MaxStoreBytesPerClient)
	}
	if got.MaxUploadBytesPerAttempt == nil || *got.MaxUploadBytesPerAttempt != 5 {
		t.Errorf("explicit upload flag = %v, want pointer to 5", got.MaxUploadBytesPerAttempt)
	}

	omitted := flag.NewFlagSet("omitted", flag.ContinueOnError)
	jobs, tasks, timeout, store, upload = flags(omitted)
	omitted.Bool("v", false, "")
	if err := omitted.Parse([]string{"-v"}); err != nil {
		t.Fatal(err)
	}
	if got := poolLimitsFlagOverridesFrom(omitted, jobs, tasks, timeout, store, upload); got != (poolLimitsFlagOverrides{}) {
		t.Fatalf("omitted flags = %#v, want no overrides", got)
	}
}

func TestClientCountLimitsCannotBeNegative(t *testing.T) {
	for _, name := range []string{"--max-jobs-per-client", "--max-tasks-per-client"} {
		if _, err := cli(t, "run", "--data-dir", t.TempDir(), "--listen", freeAddr(t), name, "-1"); err == nil || !strings.Contains(err.Error(), "cannot be negative") {
			t.Errorf("%s -1: %v", name, err)
		}
	}
}

// A node whose saved pool limits cannot be read does not start with limits
// its owner did not choose.
func TestADaemonWhosePoolLimitsCannotBeReadDoesNotStart(t *testing.T) {
	damaged := t.TempDir()
	db, err := nodedb.Open(filepath.Join(damaged, "node.db"))
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	raw, err := sql.Open("sqlite", "file:"+filepath.Join(damaged, "node.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`DROP TABLE pool_limits`); err != nil {
		t.Fatal(err)
	}
	raw.Close()
	if _, err := cli(t, "run", "--discovery", "off", "--data-dir", damaged, "--listen", freeAddr(t)); err == nil || !strings.Contains(err.Error(), "load pool limits") {
		t.Errorf("starting with pool limits that cannot be read: %v", err)
	}
}

// Limits saved through the local API hold the pool's clients once the
// node restarts, and a flag given on the command line still wins.
func TestSavedPoolLimitsHoldClientsAfterARestart(t *testing.T) {
	dataDir, laptop := t.TempDir(), t.TempDir()
	addr, apiAddr := freeAddr(t), freeAddr(t)
	stop := startDaemon(t, "--data-dir", dataDir, "--role", "coordinator", "--listen", addr, "--api-listen", apiAddr)
	client := desktop(t, apiAddr)
	poolAsSeenBy(t, client)
	ctx := tokenOf(t, dataDir)
	state, err := client.SetPoolLimits(ctx, &nodepb.SetPoolLimitsRequest{Limits: &nodepb.PoolLimits{MaxJobsPerClient: 1}})
	if err != nil || !state.GetRestartNeeded() || state.GetRunning().GetMaxJobsPerClient() != 0 || !state.GetCoordinator() {
		t.Fatalf("saving a limit: %v, %v", state, err)
	}
	stop()

	// Started again, it runs with the saved limit: a client with a job
	// waiting for a worker is refused a second.
	stop = startDaemon(t, "--data-dir", dataDir, "--role", "coordinator", "--listen", addr, "--api-listen", apiAddr)
	poolAsSeenBy(t, client)
	if state, err := client.GetPoolLimits(ctx, &nodepb.GetPoolLimitsRequest{}); err != nil || state.GetRestartNeeded() || state.GetRunning().GetMaxJobsPerClient() != 1 || !state.GetConfigured() {
		t.Fatalf("limits after the restart: %v, %v", state, err)
	}
	if _, err := as(t, laptop, "pool", "join", "--addr", addr, invite(t, addr, "client")); err != nil {
		t.Fatal(err)
	}
	if _, err := as(t, laptop, "job", "submit", "--addr", addr, "--detach", "--params", `{"from":0,"to":100}`); err != nil {
		t.Fatalf("a client's first job: %v", err)
	}
	if _, err := as(t, laptop, "job", "submit", "--addr", addr, "--detach", "--params", `{"from":0,"to":100}`); err == nil || !strings.Contains(err.Error(), "as many as this coordinator lets one client have") {
		t.Fatalf("a client's second job: %v, want it refused", err)
	}
	stop()

	// A flag given explicitly overrides what was saved, and the API says
	// the node does not run with the saved limits. The node comes back at
	// the address the client joined it at.
	startDaemon(t, "--data-dir", dataDir, "--role", "coordinator", "--listen", addr, "--api-listen", apiAddr, "--max-jobs-per-client", "0")
	poolAsSeenBy(t, client)
	if state, err := client.GetPoolLimits(ctx, &nodepb.GetPoolLimitsRequest{}); err != nil || !state.GetRestartNeeded() || state.GetRunning().GetMaxJobsPerClient() != 0 || state.GetSaved().GetMaxJobsPerClient() != 1 {
		t.Fatalf("limits with the flag given: %v, %v", state, err)
	}
	if _, err := as(t, laptop, "job", "submit", "--addr", addr, "--detach", "--params", `{"from":0,"to":100}`); err != nil {
		t.Fatalf("a client's second job with no limit: %v", err)
	}
}
