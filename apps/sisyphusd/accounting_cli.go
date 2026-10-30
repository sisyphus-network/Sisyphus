package main

import (
	"context"
	"encoding/csv"
	"errors"
	"flag"
	"fmt"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
)

// now is the time a command takes as the present; tests replace it.
var now = time.Now

// column is one column of a table of totals: its heading, and how a row
// gives its value. A table printed for reading and one written as CSV have
// the same columns, so a figure added to the totals, such as a machine's
// time spent mining (#776), is added here once for both.
type column[T any] struct {
	name  string
	value func(T) string
}

func decimal[N uint32 | uint64](n N) string { return strconv.FormatUint(uint64(n), 10) }

// basisColumns are the columns of accepted tasks, by basis.
func basisColumns[T any](accepted func(T) *pb.AcceptedTasks) []column[T] {
	return []column[T]{
		{"accepted", func(r T) string {
			a := accepted(r)
			return decimal(a.GetAgreement() + a.GetAudit() + a.GetUnchecked())
		}},
		{"agreement", func(r T) string { return decimal(accepted(r).GetAgreement()) }},
		{"audit", func(r T) string { return decimal(accepted(r).GetAudit()) }},
		{"unchecked", func(r T) string { return decimal(accepted(r).GetUnchecked()) }},
		{"caught", func(r T) string { return decimal(accepted(r).GetCaught()) }},
	}
}

var machineColumns = append([]column[*pb.MachineTotals]{
	{"machine", (*pb.MachineTotals).GetName},
	{"node_id", (*pb.MachineTotals).GetNodeId},
	{"jobs", func(m *pb.MachineTotals) string { return decimal(m.GetJobs()) }},
	{"attempts", func(m *pb.MachineTotals) string { return decimal(m.GetAttempts()) }},
	{"failed_attempts", func(m *pb.MachineTotals) string { return decimal(m.GetFailedAttempts()) }},
}, append(basisColumns((*pb.MachineTotals).GetAccepted),
	column[*pb.MachineTotals]{"outvoted", func(m *pb.MachineTotals) string { return decimal(m.GetOutvoted()) }},
)...)

var workloadColumns = append([]column[*pb.WorkloadTotals]{
	{"workload", (*pb.WorkloadTotals).GetWorkload},
	{"jobs", func(w *pb.WorkloadTotals) string { return decimal(w.GetJobs()) }},
	{"succeeded", func(w *pb.WorkloadTotals) string { return decimal(w.GetSucceeded()) }},
	{"failed", func(w *pb.WorkloadTotals) string { return decimal(w.GetFailed()) }},
	{"cancelled", func(w *pb.WorkloadTotals) string { return decimal(w.GetCancelled()) }},
	{"tasks", func(w *pb.WorkloadTotals) string { return decimal(w.GetTasks()) }},
	{"attempts", func(w *pb.WorkloadTotals) string { return decimal(w.GetAttempts()) }},
	{"failed_attempts", func(w *pb.WorkloadTotals) string { return decimal(w.GetFailedAttempts()) }},
}, append(basisColumns((*pb.WorkloadTotals).GetAccepted),
	column[*pb.WorkloadTotals]{"machines", func(w *pb.WorkloadTotals) string { return decimal(w.GetMachines()) }},
	column[*pb.WorkloadTotals]{"wall_seconds", func(w *pb.WorkloadTotals) string { return decimal(w.GetWallSeconds()) }},
)...)

// writeTable writes rows under their columns' names: as CSV, or lined up
// for reading.
func writeTable[T any](columns []column[T], rows []T, asCSV bool) error {
	header := make([]string, len(columns))
	for i, c := range columns {
		header[i] = c.name
	}
	lines := [][]string{header}
	for _, row := range rows {
		line := make([]string, len(columns))
		for i, c := range columns {
			line[i] = c.value(row)
		}
		lines = append(lines, line)
	}
	if asCSV {
		w := csv.NewWriter(stdout)
		w.WriteAll(lines) // flushes, and its error is the writer's
		return w.Error()
	}
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	lines[0] = strings.Split(strings.ToUpper(strings.Join(header, "\t")), "\t")
	for _, line := range lines {
		fmt.Fprintln(tw, strings.Join(line, "\t"))
	}
	return tw.Flush()
}

// parseMoment reads a time given as RFC 3339, or as a date, which is the
// start of that day here.
func parseMoment(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	if t, err := time.ParseInLocation(time.DateOnly, s, time.Local); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("%q is neither a date (2006-01-02) nor a time (2006-01-02T15:04:05Z07:00)", s)
}

// parseSpan reads a span of time as Go writes one (36h, 90m), or as whole
// days (7d).
func parseSpan(s string) (time.Duration, error) {
	if days, ok := strings.CutSuffix(s, "d"); ok {
		if n, err := strconv.ParseUint(days, 10, 16); err == nil && n > 0 {
			return time.Duration(n) * 24 * time.Hour, nil
		}
	} else if d, err := time.ParseDuration(s); err == nil && d > 0 {
		return d, nil
	}
	return 0, fmt.Errorf("--last %q is not a span of time such as 24h, 7d or 30d", s)
}

// accountingCommand prints what each machine did and what was done of each
// workload, over a span of time.
func accountingCommand(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("sisyphusd accounting", flag.ContinueOnError)
	node := targetFlags(fs)
	last := fs.String("last", "", "count the jobs that ended in this span before --to or now: 24h, 7d, 30d or any other")
	from := fs.String("from", "", "count the jobs that ended at or after this date (2006-01-02, the start of that day here) or time (RFC 3339); not with --last")
	to := fs.String("to", "", "count the jobs that ended before this date or time")
	by := fs.String("by", "", "print only the totals by machine or by workload; --csv prints machines unless told workload")
	asCSV := fs.Bool("csv", false, "write one table as CSV, for a spreadsheet")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if *by != "" && *by != "machine" && *by != "workload" {
		return fmt.Errorf("--by %q: expected machine or workload", *by)
	}
	if *asCSV && *by == "" {
		*by = "machine"
	}
	if *last != "" && *from != "" {
		return errors.New("--last and --from both say where the span starts; give one")
	}
	request := &pb.GetAccountingRequest{}
	end := now()
	if *to != "" {
		t, err := parseMoment(*to)
		if err != nil {
			return err
		}
		end, request.To = t, timestamppb.New(t)
	}
	if *from != "" {
		t, err := parseMoment(*from)
		if err != nil {
			return err
		}
		request.From = timestamppb.New(t)
	}
	if *last != "" {
		span, err := parseSpan(*last)
		if err != nil {
			return err
		}
		request.From = timestamppb.New(end.Add(-span))
	}

	client, closeConn, err := dial(node)
	if err != nil {
		return err
	}
	defer closeConn()
	totals, err := client.GetAccounting(ctx, request)
	if err != nil {
		return err
	}
	if !*asCSV {
		fmt.Fprintf(stdout, "%d jobs ended %s\n", totals.GetJobs(), describeSpan(totals))
	}
	if *by != "workload" {
		if !*asCSV {
			fmt.Fprintln(stdout)
		}
		if err := writeTable(machineColumns, totals.GetMachines(), *asCSV); err != nil {
			return err
		}
	}
	if *by != "machine" {
		if !*asCSV {
			fmt.Fprintln(stdout)
		}
		return writeTable(workloadColumns, totals.GetWorkloads(), *asCSV)
	}
	return nil
}

// describeSpan says what span totals are over.
func describeSpan(totals *pb.Accounting) string {
	at := func(t *timestamppb.Timestamp) string { return t.AsTime().Local().Format(time.DateTime) }
	switch from, to := totals.GetFrom(), totals.GetTo(); {
	case from != nil && to != nil:
		return "from " + at(from) + " to " + at(to)
	case from != nil:
		return "since " + at(from)
	case to != nil:
		return "before " + at(to)
	}
	return "in all the coordinator holds"
}
