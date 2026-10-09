package main

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
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
	verify := fs.Uint("verify", 0, "have each task run by this many different workers, and take its result only once that many have returned the same one; for work that gives the same result every time, and not with --key-file. 0 or 1 runs each task once")
	share := fs.Float64("verify-share", 0, "with --verify, verify only this share of the tasks, from 0 to 1, picked at random, and run the rest once; a worker caught returning a different result has its other tasks in the job verified after all. 0 or 1 verifies every task")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}

	spec := &pb.JobSpec{Workload: *workload, Params: []byte(*params), MaxTasks: uint32(*tasks), TaskTimeoutSeconds: uint32(*timeout / time.Second),
		MinMemoryBytes: *minMemory << 20, MinGpus: uint32(*minGPUs), Verify: uint32(*verify), VerifyShare: *share}
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
	fmt.Fprintf(stdout, "job %s: %d task(s)%s\n", job.GetJobId(), len(job.GetTasks()), verified(job))
	return followJob(ctx, client, job)
}

// verified says, of a job that is verified, by how many workers its tasks
// are, and how many of them if not all.
func verified(job *pb.Job) string {
	by := job.GetSpec().GetVerify()
	if by < 2 {
		return ""
	}
	if job.GetSpec().GetVerifyShare() == 0 {
		return fmt.Sprintf(", each verified by %d workers", by)
	}
	checked := 0
	for _, task := range job.GetTasks() {
		if task.GetVerify() >= 2 {
			checked++
		}
	}
	if checked == 0 {
		// A job whose tasks are jobs, each of which picks its own.
		return fmt.Sprintf(", a share of %.3g of the tasks of each verified by %d workers", job.GetSpec().GetVerifyShare(), by)
	}
	return fmt.Sprintf(", %d of them verified by %d workers", checked, by)
}

// heldTo says, of a task of a job only some of whose tasks are verified,
// whether it is one of them. It says nothing of a task that is itself a
// job.
func heldTo(job *pb.Job, task *pb.Task) string {
	switch {
	case job.GetSpec().GetVerifyShare() == 0, task.GetVerify() == 0:
		return ""
	case task.GetVerify() >= 2:
		return fmt.Sprintf(", verified by %d workers", task.GetVerify())
	}
	return ", not verified"
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
	fmt.Fprintf(stdout, "job %s: %s, workload %s%s\n", job.GetJobId(), stateName(job.GetState().String()), job.GetSpec().GetWorkload(), verified(job))
	for _, task := range job.GetTasks() {
		fmt.Fprintln(stdout, "  "+describeTask(task)+heldTo(job, task))
	}
	if job.GetRecordCid() != "" {
		fmt.Fprintln(stdout, "  record "+job.GetRecordCid())
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

// jobRecord prints the record of a job that is over: the CID that names it,
// and its nodes as JSON.
func jobRecord(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("sisyphusd job record", flag.ContinueOnError)
	node := targetFlags(fs)
	verify := fs.Bool("verify", false, "work the record out again from the job, and fail if it is not the record the job names")
	keyFile := fs.String("key-file", "", "a private job's key: verify the record, and also check its commitments against the values the node holds")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("expected exactly one job ID")
	}
	request := &pb.GetJobRecordRequest{JobId: fs.Arg(0), Verify: *verify}
	if *keyFile != "" {
		// The commitments checked are the job's. Only a record that has
		// been checked against the job is known to hold the same ones, so
		// a key asks for that check too.
		*verify, request.Verify = true, true
		key, err := readKey(*keyFile)
		if err != nil {
			return err
		}
		request.Key = key[:]
	}
	client, closeConn, err := dial(node)
	if err != nil {
		return err
	}
	defer closeConn()
	record, err := client.GetJobRecord(ctx, request)
	if err != nil {
		return err
	}
	fmt.Fprintln(stdout, record.GetRootCid())
	// The nodes by CID, each as the coordinator rendered it. They are put
	// together by hand so that each stays exactly as it was sent.
	var doc strings.Builder
	doc.WriteString("{")
	for i, n := range record.GetNodes() {
		if i > 0 {
			doc.WriteString(",")
		}
		fmt.Fprintf(&doc, "%q:%s", n.GetCid(), n.GetJson())
	}
	doc.WriteString("}")
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, []byte(doc.String()), "", "  "); err != nil {
		return fmt.Errorf("the record came back as something that is not JSON: %w", err)
	}
	fmt.Fprintln(stdout, pretty.String())
	if *verify && !record.GetMatches() {
		return fmt.Errorf("job %s does not match its record: worked out again, the record would be %s", record.GetJobId(), record.GetRecomputedCid())
	}
	if *verify {
		fmt.Fprintln(stdout, "verified: the job as the node holds it gives this record")
	}
	if *keyFile != "" {
		return reportCommitments(record, *keyFile)
	}
	return nil
}

// reportCommitments prints, for each commitment in a private job's record,
// whether the key in keyFile and the value the node holds give it, and
// fails if any does not. That is no fault in the record: the key may simply
// not be the job's.
func reportCommitments(record *pb.JobRecord, keyFile string) error {
	checks := record.GetCommitments()
	if len(checks) == 0 {
		fmt.Fprintln(stdout, "nothing to check with a key: this record holds no commitments, as only the record of a private job does")
		return nil
	}
	missed := 0
	for _, check := range checks {
		outcome := "matches"
		if !check.GetMatches() {
			outcome = "does not match"
			missed++
		}
		fmt.Fprintf(stdout, "%s: %s\n", check.GetName(), outcome)
	}
	if missed > 0 {
		return fmt.Errorf("%d of the %d commitments in the record of job %s do not match: %s is not the job's key, or the node no longer holds the values the job ended with",
			missed, len(checks), record.GetJobId(), keyFile)
	}
	fmt.Fprintf(stdout, "checked: the key and the values the node holds give all %d commitments\n", len(checks))
	return nil
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
	// What verified tasks have shown of the nodes they have shown anything
	// of, one to a line below the table.
	var standings []string
	for _, node := range listed.GetNodes() {
		if standing := standingOf(node); standing != "" {
			standings = append(standings, standing)
		}
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
	for _, standing := range standings {
		// A line with no tabs in it leaves the columns as they are.
		fmt.Fprintln(tw, standing)
	}
	return tw.Flush()
}

// standingOf says how a node's results for verified tasks have come out, in
// every job so far, and whether it is on probation. It says nothing of a
// node that has returned none that were settled.
func standingOf(node *pb.NodeInfo) string {
	agreed, outvoted, owed := node.GetVerifiedAgreed(), node.GetVerifiedOutvoted(), node.GetProbation()
	if agreed+outvoted == 0 {
		return ""
	}
	said := fmt.Sprintf("%s: results for verified tasks: %d agreed, %d outvoted", cmp.Or(node.GetName(), node.GetNodeId()), agreed, outvoted)
	if owed > 0 {
		said += fmt.Sprintf("; on probation, %d more to agree", owed)
	}
	return said
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
