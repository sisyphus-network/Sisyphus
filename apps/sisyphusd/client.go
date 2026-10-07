package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
)

// stdout is where client commands print; tests replace it.
var stdout io.Writer = os.Stdout

// stdin is what "blob put -" reads; tests replace it.
var stdin io.Reader = os.Stdin

// stderr is where usage and errors are printed; tests replace it.
var stderr io.Writer = os.Stderr

func dial(node *target) (pb.NodeServiceClient, func(), error) {
	conn, err := node.connect()
	if err != nil {
		return nil, nil, err
	}
	return pb.NewNodeServiceClient(conn), func() { conn.Close() }, nil
}

func jobSubmit(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("sisyphusd job submit", flag.ContinueOnError)
	node := targetFlags(fs)
	workload := fs.String("workload", "primes", "workload to run")
	params := fs.String("params", "", `workload parameters, e.g. '{"from":0,"to":100000000}' for primes`)
	mode := fs.String("mode", "distributed", "distributed (split across workers) or full-worker (whole job on one worker)")
	tasks := fs.Uint("tasks", 0, "distributed mode: number of tasks to split into (default: one per connected worker slot)")
	detach := fs.Bool("detach", false, "print the job ID and return without waiting")
	minMemory := fs.Uint64("min-memory-mb", 0, "give the job's tasks only to workers with at least this much memory, in mebibytes")
	minGPUs := fs.Uint("gpus", 0, "give the job's tasks only to workers with at least this many graphics cards")
	timeout := fs.Duration("timeout", 0, "stop and retry any attempt at a task that runs longer than this; 0 means no limit")
	keyFile := fs.String("key-file", "", "make the job private: seal everything it stores with this key, and open sealed inputs with it")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}

	spec := &pb.JobSpec{Workload: *workload, Params: []byte(*params), MaxTasks: uint32(*tasks), TaskTimeoutSeconds: uint32(*timeout / time.Second),
		MinMemoryBytes: *minMemory << 20, MinGpus: uint32(*minGPUs)}
	switch *mode {
	case "distributed":
		spec.Mode = pb.ScheduleMode_SCHEDULE_MODE_DISTRIBUTED
	case "full-worker":
		spec.Mode = pb.ScheduleMode_SCHEDULE_MODE_FULL_WORKER
	default:
		return fmt.Errorf("unknown mode %q", *mode)
	}

	if *keyFile != "" {
		key, err := readKey(*keyFile)
		if err != nil {
			return err
		}
		spec.Key = key[:]
	}

	client, closeConn, err := dial(node)
	if err != nil {
		return err
	}
	defer closeConn()

	submitted, err := client.SubmitJob(ctx, &pb.SubmitJobRequest{Spec: spec})
	if err != nil {
		return err
	}
	job := submitted.GetJob()
	if *detach {
		fmt.Fprintln(stdout, job.GetJobId())
		return nil
	}
	fmt.Fprintf(stdout, "job %s: %d task(s)\n", job.GetJobId(), len(job.GetTasks()))
	return followJob(ctx, client, job)
}

// followJob prints a job's progress until it finishes, then its outcome.
func followJob(ctx context.Context, client pb.NodeServiceClient, job *pb.Job) error {
	stream, err := client.WatchJob(ctx, &pb.WatchJobRequest{JobId: job.GetJobId()})
	if err != nil {
		return err
	}
	// Each event is a full snapshot, so print only the tasks that changed.
	seen := make(map[string]string)
	for {
		event, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		job = event.GetJob()
		for _, task := range job.GetTasks() {
			line := describeTask(task)
			if task.GetState() != pb.TaskState_TASK_STATE_PENDING && seen[task.GetTaskId()] != line {
				fmt.Fprintln(stdout, "  "+line)
			}
			seen[task.GetTaskId()] = line
		}
	}
	return reportOutcome(job)
}

func jobGet(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("sisyphusd job get", flag.ContinueOnError)
	node := targetFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("expected exactly one job ID")
	}

	client, closeConn, err := dial(node)
	if err != nil {
		return err
	}
	defer closeConn()

	got, err := client.GetJob(ctx, &pb.GetJobRequest{JobId: fs.Arg(0)})
	if err != nil {
		return err
	}
	job := got.GetJob()
	fmt.Fprintf(stdout, "job %s: %s, workload %s\n", job.GetJobId(), stateName(job.GetState().String()), job.GetSpec().GetWorkload())
	for _, task := range job.GetTasks() {
		fmt.Fprintln(stdout, "  "+describeTask(task))
	}
	if job.GetFinishedAt() != nil {
		return reportOutcome(job)
	}
	return nil
}

// jobCancel stops a job that has not finished.
func jobCancel(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("sisyphusd job cancel", flag.ContinueOnError)
	node := targetFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("expected exactly one job ID")
	}
	client, closeConn, err := dial(node)
	if err != nil {
		return err
	}
	defer closeConn()
	if _, err := client.CancelJob(ctx, &pb.CancelJobRequest{JobId: fs.Arg(0)}); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "job %s cancelled\n", fs.Arg(0))
	return nil
}

// jobLogs prints what has happened to a job and, while it is still going,
// what happens next, until it is over.
func jobLogs(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("sisyphusd job logs", flag.ContinueOnError)
	node := targetFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("expected exactly one job ID")
	}
	client, closeConn, err := dial(node)
	if err != nil {
		return err
	}
	defer closeConn()
	stream, err := client.WatchJobEvents(ctx, &pb.WatchJobEventsRequest{JobId: fs.Arg(0)})
	if err != nil {
		return err
	}
	for {
		event, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		fmt.Fprintln(stdout, describeEvent(event))
	}
}

// describeEvent writes one thing that happened to a job as a line.
func describeEvent(e *pb.JobEvent) string {
	line := e.GetAt().AsTime().UTC().Format("15:04:05.000") + " "
	if e.GetTaskIndex() >= 0 {
		line += fmt.Sprintf("task %d ", e.GetTaskIndex())
	}
	if e.GetNodeName() != "" {
		line += "on " + e.GetNodeName() + " "
	}
	line += e.GetKind()
	if e.GetText() != "" {
		line += ": " + e.GetText()
	}
	return line
}

func listNodes(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("sisyphusd nodes", flag.ContinueOnError)
	node := targetFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}

	client, closeConn, err := dial(node)
	if err != nil {
		return err
	}
	defer closeConn()

	listed, err := client.ListNodes(ctx, &pb.ListNodesRequest{})
	if err != nil {
		return err
	}
	if len(listed.GetNodes()) == 0 {
		fmt.Fprintln(stdout, "no workers connected")
		return nil
	}
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tNODE\tHOST\tPLATFORM\tCORES\tMEMORY\tGPUS\tTASKS\tWORKLOADS\tRELAYED")
	for _, node := range listed.GetNodes() {
		c := node.GetCapabilities()
		// What a node that relays has carried between other members.
		relayed := "-"
		if len(node.GetRelayAddresses()) > 0 {
			relayed = fmt.Sprintf("%s in %d connections", byteCount(node.GetRelayedBytes()), node.GetRelayedConnections())
		}
		memory := "?"
		if c.GetMemoryBytes() > 0 {
			memory = byteCount(c.GetMemoryBytes())
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s/%s\t%d\t%s\t%d\t%d/%d\t%s\t%s\n",
			node.GetName(), node.GetNodeId(), c.GetHostname(), c.GetOs(), c.GetArch(), c.GetCpuCores(), memory, len(c.GetGpus()),
			node.GetRunningTasks(), c.GetTaskSlots(), strings.Join(c.GetWorkloads(), ","), relayed)
	}
	return tw.Flush()
}

// reportOutcome prints a finished job's result, or returns its failure.
func reportOutcome(job *pb.Job) error {
	took := job.GetFinishedAt().AsTime().Sub(job.GetCreatedAt().AsTime())
	if job.GetState() == pb.JobState_JOB_STATE_CANCELLED {
		return fmt.Errorf("job %s was cancelled after %s", job.GetJobId(), took)
	}
	if job.GetState() != pb.JobState_JOB_STATE_SUCCEEDED {
		return fmt.Errorf("job %s failed after %s: %s", job.GetJobId(), took, job.GetError())
	}
	fmt.Fprintf(stdout, "job %s succeeded in %s\n%s\n", job.GetJobId(), took, job.GetResult())
	return nil
}

func describeTask(task *pb.Task) string {
	line := fmt.Sprintf("task %d %s", task.GetIndex(), stateName(task.GetState().String()))
	if task.GetNodeId() != "" {
		line += fmt.Sprintf(" on %s (attempt %d)", task.GetNodeName(), task.GetAttempt())
	}
	if task.GetError() != "" {
		line += ": " + task.GetError()
	}
	return line
}

// stateName turns an enum name such as TASK_STATE_RUNNING into "running".
func stateName(enum string) string {
	return strings.ToLower(enum[strings.LastIndex(enum, "_")+1:])
}

// byteCount writes a number of bytes the way people read them.
func byteCount(n uint64) string {
	units := []string{"B", "KiB", "MiB", "GiB", "TiB", "PiB"}
	size, unit := float64(n), 0
	for size >= 1024 && unit < len(units)-1 {
		size /= 1024
		unit++
	}
	if unit == 0 {
		return fmt.Sprintf("%d B", n)
	}
	return fmt.Sprintf("%.1f %s", size, units[unit])
}
