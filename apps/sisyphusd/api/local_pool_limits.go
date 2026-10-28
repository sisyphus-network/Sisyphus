package api

import (
	"context"
	"fmt"
	"math"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/sisyphus-network/Sisyphus/packages/nodedb"
	nodepb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/node/v1"
)

// PoolLimitsStore keeps the limits the node, as a coordinator, holds each
// member to.
type PoolLimitsStore interface {
	PoolLimits() (nodedb.PoolLimits, bool, error)
	SetPoolLimits(nodedb.PoolLimits) error
}

// maxTaskTimeoutSeconds is the longest task timeout a time.Duration holds,
// in whole seconds.
const maxTaskTimeoutSeconds = uint64(math.MaxInt64 / int64(time.Second))

// GetPoolLimits returns the limits the node saved and those it is running
// with. The token is required because the matching setter changes them.
func (s *localService) GetPoolLimits(ctx context.Context, _ *nodepb.GetPoolLimitsRequest) (*nodepb.PoolLimitsState, error) {
	if err := s.authorize(ctx); err != nil {
		return nil, err
	}
	return s.poolLimitsSnapshot()
}

func (s *localService) poolLimitsSnapshot() (*nodepb.PoolLimitsState, error) {
	if s.cfg.PoolLimits == nil {
		return nil, status.Error(codes.FailedPrecondition, "pool limits are unavailable")
	}
	saved, configured, err := s.cfg.PoolLimits.PoolLimits()
	if err != nil {
		return nil, asError(err)
	}
	if !configured {
		saved = s.cfg.PoolLimitsRunning
	}
	return poolLimitsState(saved, s.cfg.PoolLimitsRunning, configured, s.cfg.RunsCoordinator), nil
}

// SetPoolLimits saves the limits the node holds each member to. A separate
// daemon is not controlled by Electron's process lifecycle, so the owner
// restarts that daemon to apply the saved limits.
func (s *localService) SetPoolLimits(ctx context.Context, req *nodepb.SetPoolLimitsRequest) (*nodepb.PoolLimitsState, error) {
	if err := s.authorize(ctx); err != nil {
		return nil, err
	}
	if s.cfg.PoolLimits == nil {
		return nil, status.Error(codes.FailedPrecondition, "pool limits are unavailable")
	}
	limits, err := poolLimitsFrom(req.GetLimits())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if err := s.cfg.PoolLimits.SetPoolLimits(limits); err != nil {
		return nil, asError(err)
	}
	s.notifySharedStateChanged()
	return poolLimitsState(limits, s.cfg.PoolLimitsRunning, true, s.cfg.RunsCoordinator), nil
}

// poolLimitsFrom checks that limits fit where the node keeps them: counts
// in an int on any platform, the timeout in a time.Duration, and bytes in
// SQLite's signed integers.
func poolLimitsFrom(limits *nodepb.PoolLimits) (nodedb.PoolLimits, error) {
	switch {
	case limits == nil:
		return nodedb.PoolLimits{}, fmt.Errorf("pool limits are required")
	case limits.GetMaxJobsPerClient() > math.MaxInt32:
		return nodedb.PoolLimits{}, fmt.Errorf("max_jobs_per_client may be at most %d", math.MaxInt32)
	case limits.GetMaxTasksPerClient() > math.MaxInt32:
		return nodedb.PoolLimits{}, fmt.Errorf("max_tasks_per_client may be at most %d", math.MaxInt32)
	case limits.GetMaxTaskTimeoutSeconds() > maxTaskTimeoutSeconds:
		return nodedb.PoolLimits{}, fmt.Errorf("max_task_timeout_seconds may be at most %d", maxTaskTimeoutSeconds)
	case limits.GetMaxStoreBytesPerClient() > math.MaxInt64:
		return nodedb.PoolLimits{}, fmt.Errorf("max_store_bytes_per_client may be at most %d", int64(math.MaxInt64))
	case limits.GetMaxUploadBytesPerAttempt() > math.MaxInt64:
		return nodedb.PoolLimits{}, fmt.Errorf("max_upload_bytes_per_attempt may be at most %d", int64(math.MaxInt64))
	}
	return nodedb.PoolLimits{
		MaxJobsPerClient:         int(limits.GetMaxJobsPerClient()),
		MaxTasksPerClient:        int(limits.GetMaxTasksPerClient()),
		MaxTaskTimeout:           time.Duration(limits.GetMaxTaskTimeoutSeconds()) * time.Second,
		MaxStoreBytesPerClient:   limits.GetMaxStoreBytesPerClient(),
		MaxUploadBytesPerAttempt: limits.GetMaxUploadBytesPerAttempt(),
	}, nil
}

func poolLimitsState(saved, running nodedb.PoolLimits, configured, coordinator bool) *nodepb.PoolLimitsState {
	return &nodepb.PoolLimitsState{
		Saved:         poolLimitsMessage(saved),
		Running:       poolLimitsMessage(running),
		RestartNeeded: saved != running,
		Configured:    configured,
		Coordinator:   coordinator,
	}
}

// poolLimitsMessage carries limits as the API does. The daemon refuses
// negative limits at startup and the store keeps none, so the conversions
// do not wrap.
func poolLimitsMessage(limits nodedb.PoolLimits) *nodepb.PoolLimits {
	return &nodepb.PoolLimits{
		MaxJobsPerClient:         uint32(limits.MaxJobsPerClient),
		MaxTasksPerClient:        uint32(limits.MaxTasksPerClient),
		MaxTaskTimeoutSeconds:    uint64(limits.MaxTaskTimeout / time.Second),
		MaxStoreBytesPerClient:   limits.MaxStoreBytesPerClient,
		MaxUploadBytesPerAttempt: limits.MaxUploadBytesPerAttempt,
	}
}
