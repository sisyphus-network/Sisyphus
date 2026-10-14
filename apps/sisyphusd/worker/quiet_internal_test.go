package worker

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/credentials/insecure"
)

// lines is a log that keeps the messages written to it.
type lines struct {
	mu   sync.Mutex
	said []string
}

func (l *lines) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.said = append(l.said, string(p))
	return len(p), nil
}

func (l *lines) count(message string) (n int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, line := range l.said {
		if strings.Contains(line, message) {
			n++
		}
	}
	return n
}

// A coordinator that stays away is reported while the worker backs off,
// then once more when it is trying as seldom as it will, and after that
// only every so often, however many times it tries.
func TestAWorkerThatCannotReachItsCoordinatorSaysSoLessAndLess(t *testing.T) {
	oldMin, oldMax, oldStill := minBackoff, maxBackoff, stillTrying
	t.Cleanup(func() { minBackoff, maxBackoff, stillTrying = oldMin, oldMax, oldStill })
	minBackoff, maxBackoff, stillTrying = time.Millisecond, 4*time.Millisecond, 150*time.Millisecond

	log := new(lines)
	w := &Worker{Name: "away", Coordinator: "127.0.0.1:1", Credentials: insecure.NewCredentials(), Log: slog.New(slog.NewTextHandler(log, nil))}
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	if err := w.Run(ctx); err != nil {
		t.Fatal(err)
	}
	// Backing off from 1ms to 4ms: told at 1ms and 2ms.
	if got := log.count("lost coordinator, reconnecting"); got != 2 {
		t.Errorf("told of the coordinator being lost %d times:\n%s", got, log.said)
	}
	// Then every 4ms for the rest of the 400, told at once and every 150.
	if got := log.count("still cannot reach the coordinator"); got < 2 || got > 3 {
		t.Errorf("told it is still trying %d times:\n%s", got, log.said)
	}
}
