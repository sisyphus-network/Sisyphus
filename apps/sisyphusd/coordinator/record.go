package coordinator

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/ipfs/go-cid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	jobmodel "github.com/sisyphus-network/Sisyphus/packages/job-model"
	"github.com/sisyphus-network/Sisyphus/packages/jobrecord"
	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
	"github.com/sisyphus-network/Sisyphus/packages/storage"
)

// A job that is over has a record: its history as linked data, named by one
// CID, which the job carries from the moment it ends. See
// packages/jobrecord for what a record holds.

// recordPrefix begins the name a job's record is pinned under. It is not a
// job's own name for its pins: those lapse when the retention period is up,
// and a record is kept for as long as the job is.
const recordPrefix = "record:"

func recordOwner(jobID string) string {
	return recordPrefix + jobID
}

// recordJobLocked works out the record of a job that has just ended, gives
// the job its CID, and sets about storing it. Working it out touches
// nothing but the job; storing it may take a while, a block at a time, so
// that is done without the lock.
func (c *Coordinator) recordJobLocked(job *jobmodel.Job) {
	record := c.buildRecordLocked(job)
	job.Record = record.Root.String()
	c.aggregating.Add(1)
	go func() {
		defer c.aggregating.Done()
		if err := c.keepRecord(c.ctx, job, record); err != nil {
			c.log.Warn("could not store a job's record; it is stored again when it is asked for", "job", job.ID, "error", err)
		}
	}()
}

// buildRecordLocked returns the record of a job that is over, as the job
// and the jobs that carried out its steps now stand.
func (c *Coordinator) buildRecordLocked(job *jobmodel.Job) *jobrecord.Record {
	var children []*jobmodel.Job
	for _, child := range c.jobs {
		if child.Parent == job.ID {
			children = append(children, child)
		}
	}
	// In the order they were submitted, by the clock on the wall and then
	// by ID: the order must come out the same after a restart.
	slices.SortFunc(children, func(a, b *jobmodel.Job) int {
		return cmp.Or(cmp.Compare(a.CreatedAt.UnixNano(), b.CreatedAt.UnixNano()), strings.Compare(a.ID, b.ID))
	})
	steps := make([]jobrecord.Step, 0, len(children))
	for _, child := range children {
		steps = append(steps, jobrecord.Step{Name: child.Step, Job: child.ID, Record: child.Record})
	}
	return jobrecord.Build(job, steps, blobName)
}

// blobName is the CID bytes would have as a stored blob.
func blobName(data []byte) cid.Cid {
	c, _ := storage.CID(context.Background(), bytes.NewReader(data)) // hashing bytes in memory cannot fail
	return c
}

// keepRecord stores a record and pins it. The record's nodes are kept for
// as long as the job is on record. The long values it links to instead of
// holding are the job's data like any other, kept for the retention period.
func (c *Coordinator) keepRecord(ctx context.Context, job *jobmodel.Job, record *jobrecord.Record) error {
	var err error
	var values []cid.Cid
	for _, value := range record.Blobs {
		_, failed := c.store.Put(ctx, bytes.NewReader(value.Data))
		err = errors.Join(err, failed)
		values = append(values, value.CID)
	}
	linked := make([][]byte, 0, len(record.Nodes)-1)
	for _, node := range record.Nodes[1:] {
		linked = append(linked, node.Data)
	}
	_, failed := c.store.PutNodes(ctx, record.Nodes[0].Data, linked...)
	return errors.Join(err, failed,
		c.store.Pin(ctx, owner(job), job.FinishedAt.Add(c.retain), values...),
		c.store.Pin(ctx, recordOwner(job.ID), time.Time{}, record.Root))
}

// Record returns the record of a job that is over, as the store holds it.
// With verify it also works the record out again from the job as it stands
// and says whether that is the record the job names.
func (c *Coordinator) Record(ctx context.Context, jobID string, verify bool) (*pb.JobRecord, error) {
	c.mu.Lock()
	job, ok := c.jobs[jobID]
	if !ok {
		c.mu.Unlock()
		return nil, status.Errorf(codes.NotFound, "job %q not found", jobID)
	}
	if !job.Terminal() {
		c.mu.Unlock()
		return nil, status.Errorf(codes.FailedPrecondition, "job %q is not over; its record is written when it ends", jobID)
	}
	root, err := cid.Decode(job.Record)
	if err != nil {
		c.mu.Unlock()
		return nil, status.Errorf(codes.FailedPrecondition, "job %q has no record: it ended before records were kept", jobID)
	}
	fresh := c.buildRecordLocked(job)
	c.mu.Unlock()

	nodes, err := jobrecord.Read(ctx, c.store, root)
	if errors.Is(err, storage.ErrNotFound) && fresh.Root.Equals(root) {
		// Not stored yet, or storing it failed. The job gives the same
		// record again, so store that.
		nodes, err = fresh.Nodes, c.keepRecord(ctx, job, fresh)
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "read the record of job %q: %v", jobID, err)
	}
	out := &pb.JobRecord{JobId: jobID, RootCid: root.String()}
	for _, node := range nodes {
		out.Nodes = append(out.Nodes, &pb.RecordNode{Cid: node.CID.String(), Data: node.Data, Json: node.JSON()})
	}
	if verify {
		out.RecomputedCid, out.Matches = fresh.Root.String(), fresh.Root.Equals(root)
	}
	return out, nil
}
