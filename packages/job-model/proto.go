package jobmodel

import (
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
)

// ToProto returns a snapshot of the job that shares no memory with it.
func (j *Job) ToProto() *pb.Job {
	out := &pb.Job{
		JobId: j.ID,
		Spec: &pb.JobSpec{
			Workload: j.Workload,
			Params:   append([]byte(nil), j.Params...),
			Mode:     pb.ScheduleMode_SCHEDULE_MODE_DISTRIBUTED,
			MaxTasks: uint32(j.MaxTasks),
		},
		State:       pb.JobState(j.State + 1),
		Result:      append([]byte(nil), j.Result...),
		Error:       j.Err,
		CreatedAt:   timestamp(j.CreatedAt),
		FinishedAt:  timestamp(j.FinishedAt),
		InputBlobs:  j.InputBlobs(),
		OutputBlobs: j.OutputBlobs(),
	}
	if j.Mode == FullWorker {
		out.Spec.Mode = pb.ScheduleMode_SCHEDULE_MODE_FULL_WORKER
	}
	for _, t := range j.Tasks {
		out.Tasks = append(out.Tasks, &pb.Task{
			TaskId:   t.ID,
			Index:    uint32(t.Index),
			State:    pb.TaskState(t.State + 1),
			Attempt:  uint32(t.Attempt),
			NodeId:   t.NodeID,
			NodeName: t.NodeName,
			Error:    t.Err,
		})
	}
	return out
}

func timestamp(t time.Time) *timestamppb.Timestamp {
	if t.IsZero() {
		return nil
	}
	return timestamppb.New(t)
}
