package pinning

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// serve runs a pinning service a test has written, and returns a client of
// it.
func serve(t *testing.T, service http.HandlerFunc) *Client {
	t.Helper()
	server := httptest.NewServer(service)
	t.Cleanup(server.Close)
	client, err := NewClient(server.URL, key)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestAClientHasANodeKeepDataAndFollowsTheRequest(t *testing.T) {
	ctx := context.Background()
	n := newNode(t, false)
	server := httptest.NewServer(n.service)
	defer server.Close()
	held := put(t, n.store.Store, "the boulder rolls")
	absent := put(t, n.store.peers[nearby], "held elsewhere")

	// The address may be given as some tools show it, with the path of the
	// requests on the end.
	client, err := NewClient(server.URL+"/pins/", key)
	if err != nil {
		t.Fatal(err)
	}
	asked, err := client.Add(ctx, Pin{CID: held, Name: "boulder.txt", Meta: map[string]string{"app": "tests"}})
	if err != nil || asked.RequestID == "" || asked.Status != Pinned || asked.Pin.Name != "boulder.txt" {
		t.Fatalf("Add: %+v, %v", asked, err)
	}
	if kept := n.keptFor(asked.RequestID); !slices.Equal(kept, []string{held}) {
		t.Errorf("the node keeps %v for the request", kept)
	}
	lost, err := client.Add(ctx, Pin{CID: absent, Origins: []string{nearby}})
	if err != nil || lost.Status != Failed || !strings.Contains(lost.Info[Details], "no way to fetch it") {
		t.Errorf("Add of what the node cannot have: %+v, %v", lost, err)
	}
	if shown, err := client.Status(ctx, asked.RequestID); err != nil || shown.Status != Pinned || shown.Pin.Meta["app"] != "tests" || !shown.Created.Equal(asked.Created) {
		t.Errorf("Status: %+v, %v", shown, err)
	}
	// A listing is of everything asked, whatever became of it.
	listed, err := client.List(ctx)
	if err != nil || len(listed) != 2 || listed[0].RequestID != lost.RequestID || listed[1].RequestID != asked.RequestID {
		t.Errorf("List: %+v, %v", listed, err)
	}
	if done, err := client.Wait(ctx, asked.RequestID, time.Hour); err != nil || done.Status != Pinned {
		t.Errorf("Wait for what is pinned already: %+v, %v", done, err)
	}

	if err := client.Remove(ctx, asked.RequestID); err != nil {
		t.Fatal(err)
	}
	if kept := n.keptFor(asked.RequestID); len(kept) != 0 {
		t.Errorf("the node still keeps %v for the request removed", kept)
	}
	for what, err := range map[string]error{
		"Remove": client.Remove(ctx, asked.RequestID),
		"Status": func() error { _, err := client.Status(ctx, asked.RequestID); return err }(),
		"Wait":   func() error { _, err := client.Wait(ctx, asked.RequestID, time.Hour); return err }(),
	} {
		if err == nil || !strings.Contains(err.Error(), "the pinning service refused (HTTP 404): NOT_FOUND there is no request "+asked.RequestID) {
			t.Errorf("%s of a request removed: %v", what, err)
		}
	}

	stranger, _ := NewClient(server.URL, "another-key")
	if _, err := stranger.Add(ctx, Pin{CID: held}); err == nil || !strings.Contains(err.Error(), "(HTTP 401): UNAUTHORIZED") {
		t.Errorf("Add with another key: %v", err)
	}
	if _, err := stranger.List(ctx); err == nil || !strings.Contains(err.Error(), "(HTTP 401): UNAUTHORIZED") {
		t.Errorf("List with another key: %v", err)
	}
}

func TestAListingIsReadPageByPage(t *testing.T) {
	ctx := context.Background()
	start := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	all := make([]Status, 5)
	for i := range all {
		// Newest first, each a little older than the one before.
		all[i] = Status{RequestID: fmt.Sprint("r", i), Status: Pinned, Created: start.Add(-time.Duration(i) * time.Nanosecond)}
	}
	var mu sync.Mutex
	var queries []string
	client := serve(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		queries = append(queries, r.URL.Query().Get("status")+" "+r.URL.Query().Get("limit")+" "+r.URL.Query().Get("before"))
		mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer "+key || r.URL.Path != "/pins" {
			t.Errorf("asked %s with %q", r.URL.Path, r.Header.Get("Authorization"))
		}
		// Two to a page, of those made before the time asked.
		left := all
		if before := r.URL.Query().Get("before"); before != "" {
			at, _ := time.Parse(time.RFC3339Nano, before)
			left = slices.DeleteFunc(slices.Clone(all), func(s Status) bool { return !s.Created.Before(at) })
		}
		json.NewEncoder(w).Encode(results{Count: len(left), Results: left[:min(len(left), 2)]})
	})
	listed, err := client.List(ctx)
	if err != nil || !slices.EqualFunc(listed, all, func(a, b Status) bool { return a.RequestID == b.RequestID }) {
		t.Fatalf("List: %+v, %v", listed, err)
	}
	if want := []string{
		"queued,pinning,pinned,failed 1000 ",
		"queued,pinning,pinned,failed 1000 2026-10-09T11:59:59.999999999Z",
		"queued,pinning,pinned,failed 1000 2026-10-09T11:59:59.999999997Z",
	}; !slices.Equal(queries, want) {
		t.Errorf("asked for %q, want %q", queries, want)
	}

	// A service that says there is more and gives nothing new is taken to
	// have given everything.
	stuck := serve(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(results{Count: 5, Results: all[:2]})
	})
	if listed, err := stuck.List(ctx); err != nil || len(listed) != 2 {
		t.Errorf("List from a service that repeats itself: %+v, %v", listed, err)
	}
	empty := serve(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"count":0,"results":[]}`)
	})
	if listed, err := empty.List(ctx); err != nil || len(listed) != 0 {
		t.Errorf("List from a service with nothing: %+v, %v", listed, err)
	}
}

func TestWaitingAsksUntilTheRequestIsSettled(t *testing.T) {
	// following returns a client of a service that gives the request these
	// states, one each time it is asked, and how often it was asked.
	following := func(states ...string) (*Client, func() int) {
		var mu sync.Mutex
		asks := 0
		client := serve(t, func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			defer mu.Unlock()
			if r.Method != http.MethodGet || r.URL.Path != "/pins/r1" {
				t.Errorf("asked %s %s", r.Method, r.URL.Path)
			}
			state := states[min(asks, len(states)-1)]
			asks++
			if state == "broken" {
				http.Error(w, "the service is down for maintenance", http.StatusServiceUnavailable)
				return
			}
			json.NewEncoder(w).Encode(Status{RequestID: "r1", Status: state, Info: map[string]string{Details: "as of ask " + fmt.Sprint(asks)}})
		})
		return client, func() int {
			mu.Lock()
			defer mu.Unlock()
			return asks
		}
	}
	ctx := context.Background()

	client, asks := following(Queued, Pinning, Pinning, Pinned)
	if done, err := client.Wait(ctx, "r1", time.Millisecond); err != nil || done.Status != Pinned || asks() != 4 {
		t.Errorf("a request that is fetched: %+v, %v, after %d asks", done, err, asks())
	}
	client, asks = following(Queued, Failed)
	if done, err := client.Wait(ctx, "r1", time.Millisecond); err != nil || done.Status != Failed || done.Info[Details] != "as of ask 2" {
		t.Errorf("a request that fails: %+v, %v", done, err)
	}
	client, _ = following(Queued, "sleeping")
	if _, err := client.Wait(ctx, "r1", time.Millisecond); err == nil || !strings.Contains(err.Error(), "a status that is not one: sleeping") {
		t.Errorf("a status the API does not have: %v", err)
	}
	client, _ = following(Pinning, "broken")
	if _, err := client.Wait(ctx, "r1", time.Millisecond); err == nil || !strings.Contains(err.Error(), "(HTTP 503): the service is down for maintenance") {
		t.Errorf("a service that stops answering properly: %v", err)
	}
	// Whoever waits can give up.
	client, asks = following(Queued)
	giveUp, cancel := context.WithCancel(ctx)
	go func() {
		for asks() == 0 {
			time.Sleep(time.Millisecond)
		}
		cancel()
	}()
	if last, err := client.Wait(giveUp, "r1", time.Hour); err != context.Canceled || last.Status != Queued {
		t.Errorf("a wait given up: %+v, %v", last, err)
	}
}

func TestWhatAServiceGetsWrongIsReported(t *testing.T) {
	for _, address := range []string{"", "pins.example.com", "pins.example.com:8080", "ftp://pins.example.com", "http://", "http://bad host/"} {
		if _, err := NewClient(address, key); err == nil || !strings.Contains(err.Error(), "looks like https://host/path") {
			t.Errorf("a service at %q: %v", address, err)
		}
	}

	ctx := context.Background()
	for answer, want := range map[string]string{
		`{"error":{"reason":"INSUFFICIENT_FUNDS","details":"top up your account"}}`: "the pinning service refused (HTTP 409): INSUFFICIENT_FUNDS top up your account",
		`{"error":{"reason":"INSUFFICIENT_FUNDS"}}`:                                 "the pinning service refused (HTTP 409): INSUFFICIENT_FUNDS",
		`{"message":"no"}`: `the pinning service refused (HTTP 409): {"message":"no"}`,
		"<html>" + strings.Repeat("no ", 100) + "</html>": "the pinning service refused (HTTP 409): <html>no no",
		"": "the pinning service refused (HTTP 409): ",
	} {
		client := serve(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusConflict)
			io.WriteString(w, answer)
		})
		if _, err := client.Add(ctx, Pin{CID: "bafyexample"}); err == nil || !strings.HasPrefix(err.Error(), want) || len(err.Error()) > 300 {
			t.Errorf("refused with %q: %v, want %q", answer, err, want)
		}
	}

	garbled := serve(t, func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "pinned!") })
	if _, err := garbled.Add(ctx, Pin{CID: "bafyexample"}); err == nil || !strings.Contains(err.Error(), "answer could not be read") {
		t.Errorf("an answer that is not JSON: %v", err)
	}
	if _, err := garbled.List(ctx); err == nil || !strings.Contains(err.Error(), "answer could not be read") {
		t.Errorf("a listing that is not JSON: %v", err)
	}

	gone := httptest.NewServer(http.NotFoundHandler())
	gone.Close()
	nobody, _ := NewClient(gone.URL, key)
	if _, err := nobody.Status(ctx, "r1"); err == nil || !strings.Contains(err.Error(), "ask the pinning service") {
		t.Errorf("a service that is not there: %v", err)
	}
}
