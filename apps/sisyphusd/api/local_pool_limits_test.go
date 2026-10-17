package api

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/sisyphus-network/Sisyphus/packages/nodedb"
	nodepb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/node/v1"
)

type memoryPoolLimits struct {
	limits nodedb.PoolLimits
	set    bool
	err    error
}

func (m *memoryPoolLimits) PoolLimits() (nodedb.PoolLimits, bool, error) {
	return m.limits, m.set, m.err
}

func (m *memoryPoolLimits) SetPoolLimits(limits nodedb.PoolLimits) error {
	if m.err != nil {
		return m.err
	}
	m.limits, m.set = limits, true
	return nil
}

func TestPoolLimitsAreAuthorizedAndPersisted(t *testing.T) {
	store := &memoryPoolLimits{}
	running := nodedb.PoolLimits{MaxJobsPerClient: 4, MaxTaskTimeout: time.Hour}
	client := serveLocal(t, LocalConfig{PoolLimits: store, PoolLimitsRunning: running, RunsCoordinator: true})
	if _, err := client.GetPoolLimits(context.Background(), &nodepb.GetPoolLimitsRequest{}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("Get without token = %v", err)
	}
	if _, err := client.SetPoolLimits(context.Background(), &nodepb.SetPoolLimitsRequest{}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("Set without token = %v", err)
	}
	ctx := withToken("the-token")
	state, err := client.GetPoolLimits(ctx, &nodepb.GetPoolLimitsRequest{})
	if err != nil || state.GetSaved().GetMaxJobsPerClient() != 4 || state.GetSaved().GetMaxTaskTimeoutSeconds() != 3600 || state.GetConfigured() || state.GetRestartNeeded() || !state.GetCoordinator() {
		t.Fatalf("initial limits = %v, %v", state, err)
	}
	want := &nodepb.PoolLimits{
		MaxJobsPerClient: 2, MaxTasksPerClient: 30, MaxTaskTimeoutSeconds: 600,
		MaxStoreBytesPerClient: 1 << 30, MaxUploadBytesPerAttempt: math.MaxInt64,
	}
	state, err = client.SetPoolLimits(ctx, &nodepb.SetPoolLimitsRequest{Limits: want})
	if err != nil || !state.GetConfigured() || !state.GetRestartNeeded() || state.GetSaved().String() != want.String() || state.GetRunning().GetMaxJobsPerClient() != 4 {
		t.Fatalf("saved limits = %v, %v", state, err)
	}
	if store.limits != (nodedb.PoolLimits{MaxJobsPerClient: 2, MaxTasksPerClient: 30, MaxTaskTimeout: 10 * time.Minute, MaxStoreBytesPerClient: 1 << 30, MaxUploadBytesPerAttempt: math.MaxInt64}) {
		t.Fatalf("stored limits = %+v", store.limits)
	}
	state, err = client.GetPoolLimits(ctx, &nodepb.GetPoolLimitsRequest{})
	if err != nil || !state.GetConfigured() || state.GetSaved().String() != want.String() {
		t.Fatalf("read saved limits = %v, %v", state, err)
	}
	// Saving what the node runs with leaves nothing to restart for.
	state, err = client.SetPoolLimits(ctx, &nodepb.SetPoolLimitsRequest{Limits: &nodepb.PoolLimits{MaxJobsPerClient: 4, MaxTaskTimeoutSeconds: 3600}})
	if err != nil || state.GetRestartNeeded() {
		t.Fatalf("saved running limits = %v, %v", state, err)
	}
}

// A node without the coordinator role says so, and still keeps limits for
// when it has one.
func TestPoolLimitsSayWhenTheNodeIsNoCoordinator(t *testing.T) {
	client := serveLocal(t, LocalConfig{PoolLimits: &memoryPoolLimits{}})
	state, err := client.SetPoolLimits(withToken("the-token"), &nodepb.SetPoolLimitsRequest{Limits: &nodepb.PoolLimits{MaxJobsPerClient: 1}})
	if err != nil || state.GetCoordinator() || !state.GetRestartNeeded() {
		t.Fatalf("limits on a worker-only node = %v, %v", state, err)
	}
}

func TestPoolLimitsValidateAndReportUnavailableStores(t *testing.T) {
	ctx := withToken("the-token")
	without := serveLocal(t, LocalConfig{})
	if _, err := without.GetPoolLimits(ctx, &nodepb.GetPoolLimitsRequest{}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("Get without store = %v", err)
	}
	if _, err := without.SetPoolLimits(ctx, &nodepb.SetPoolLimitsRequest{}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("Set without store = %v", err)
	}
	for _, test := range []struct {
		name   string
		limits *nodepb.PoolLimits
		want   string
	}{
		{"missing limits", nil, "limits are required"},
		{"too many jobs", &nodepb.PoolLimits{MaxJobsPerClient: math.MaxInt32 + 1}, "max_jobs_per_client may be at most 2147483647"},
		{"too many tasks", &nodepb.PoolLimits{MaxTasksPerClient: math.MaxUint32}, "max_tasks_per_client may be at most 2147483647"},
		{"timeout past a duration", &nodepb.PoolLimits{MaxTaskTimeoutSeconds: 9223372037}, "max_task_timeout_seconds may be at most 9223372036"},
		{"store bytes past SQLite", &nodepb.PoolLimits{MaxStoreBytesPerClient: math.MaxInt64 + 1}, "max_store_bytes_per_client may be at most 9223372036854775807"},
		{"upload bytes past SQLite", &nodepb.PoolLimits{MaxUploadBytesPerAttempt: math.MaxUint64}, "max_upload_bytes_per_attempt may be at most 9223372036854775807"},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := &memoryPoolLimits{}
			client := serveLocal(t, LocalConfig{PoolLimits: store})
			_, err := client.SetPoolLimits(ctx, &nodepb.SetPoolLimitsRequest{Limits: test.limits})
			if status.Code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Set invalid limits = %v", err)
			}
			if store.set {
				t.Errorf("invalid limits were saved: %+v", store.limits)
			}
		})
	}
	// The largest of each that fits is taken.
	largest := &nodepb.PoolLimits{
		MaxJobsPerClient: math.MaxInt32, MaxTasksPerClient: math.MaxInt32, MaxTaskTimeoutSeconds: 9223372036,
		MaxStoreBytesPerClient: math.MaxInt64, MaxUploadBytesPerAttempt: math.MaxInt64,
	}
	if state, err := serveLocal(t, LocalConfig{PoolLimits: &memoryPoolLimits{}}).SetPoolLimits(ctx, &nodepb.SetPoolLimitsRequest{Limits: largest}); err != nil || state.GetSaved().String() != largest.String() {
		t.Errorf("Set largest limits = %v, %v", state, err)
	}
	broken := serveLocal(t, LocalConfig{PoolLimits: &memoryPoolLimits{err: errors.New("disk gone")}})
	if _, err := broken.GetPoolLimits(ctx, &nodepb.GetPoolLimitsRequest{}); status.Code(err) != codes.Internal {
		t.Errorf("Get from broken store = %v", err)
	}
	if _, err := broken.SetPoolLimits(ctx, &nodepb.SetPoolLimitsRequest{Limits: &nodepb.PoolLimits{MaxJobsPerClient: 1}}); status.Code(err) != codes.Internal {
		t.Errorf("Set to broken store = %v", err)
	}
}
