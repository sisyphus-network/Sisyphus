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

			TaskTimeoutSeconds: uint32(j.TaskTimeout / time.Second),
			MinMemoryBytes:     j.MinMemory,
			MinGpus:            uint32(j.MinGPUs),
		},
		State:       pb.JobState(j.State + 1),
		Result:      append([]byte(nil), j.Result...),
		Error:       j.Err,
		CreatedAt:   timestamp(j.CreatedAt),
		FinishedAt:  timestamp(j.FinishedAt),
		InputBlobs:  j.InputBlobs(),
		Private:     len(j.Key) > 0,
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
			Progress: t.Progress,
		})
		// A job is as far along as its tasks are, taken together.
		out.Progress += t.Progress / float64(len(j.Tasks))
	}
	return out
}

// ToProto returns the event in the pool's protocol.
func (e Event) ToProto() *pb.JobEvent {
	return &pb.JobEvent{Seq: e.Seq, At: timestamp(e.At), Kind: e.Kind, TaskIndex: int32(e.Task), NodeName: e.Node, Text: e.Text}
}

func timestamp(t time.Time) *timestamppb.Timestamp {
	if t.IsZero() {
		return nil
	}
	return timestamppb.New(t)
}
