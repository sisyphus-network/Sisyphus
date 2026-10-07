package runtime

import "context"

// Reporter is how a task says, while it runs, how far along it is and what
// it is doing. Both are for people watching the job: nothing depends on
// either, and a workload that reports nothing still works.
type Reporter interface {
	// Progress says how much of the task is done, from 0 to 1.
	Progress(done float64)
	// Log records a line about what the task is doing.
	Log(line string)
}

type reporterKey struct{}

// WithReporter returns a context whose tasks report to r.
func WithReporter(ctx context.Context, r Reporter) context.Context {
	return context.WithValue(ctx, reporterKey{}, r)
}

// Report returns the reporter of the task ctx belongs to. Where nobody is
// listening it returns one that takes reports and does nothing with them.
func Report(ctx context.Context) Reporter {
	if r, ok := ctx.Value(reporterKey{}).(Reporter); ok {
		return r
	}
	return unheard{}
}

type unheard struct{}

func (unheard) Progress(float64) {}
func (unheard) Log(string)       {}
