package kubo

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestBlockGetReadsPeerTimeoutWithTheRekeyLock(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v0/block/get" {
			t.Errorf("request path = %q, want block/get", r.URL.Path)
		}
		if _, err := time.ParseDuration(r.URL.Query().Get("timeout")); err != nil {
			t.Errorf("invalid peer timeout %q", r.URL.Query().Get("timeout"))
		}
		_, _ = io.WriteString(w, "block")
	}))
	defer server.Close()

	client := NewClient(strings.TrimPrefix(server.URL, "http://"))
	client.PeerTimeout = time.Second
	var calls sync.WaitGroup
	calls.Add(1)
	go func() {
		defer calls.Done()
		for i := range 200 {
			client.mu.Lock()
			client.PeerTimeout = time.Duration(i+1) * time.Millisecond
			client.mu.Unlock()
		}
	}()
	for range 8 {
		calls.Add(1)
		go func() {
			defer calls.Done()
			for range 40 {
				data, err := client.BlockGet(context.Background(), "bafy-test")
				if err != nil || string(data) != "block" {
					t.Errorf("BlockGet = %q, error %v", data, err)
					return
				}
			}
		}()
	}
	calls.Wait()
}
