package main

import (
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTheLocalAPIIsServedToTheWebPagesItsOwnerLists(t *testing.T) {
	dataDir := t.TempDir()
	addr, webAddr := freeAddr(t), freeAddr(t)
	startDaemon(t, "--data-dir", dataDir, "--listen", addr, "--name", "rig", "--web-listen", webAddr, "--web-origin", " http://localhost:5173/ , ,https://app.example")
	waitForOutput(t, "rig", "nodes", "--addr", addr)
	token, err := os.ReadFile(filepath.Join(dataDir, "api.token"))
	if err != nil {
		t.Fatal(err)
	}
	ask := func(origin, key string) (int, string) {
		req, _ := http.NewRequest(http.MethodPost, "http://"+webAddr+"/sisyphus.node.v1.NodeService/ListWorkers", strings.NewReader(`{}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Connect-Protocol-Version", "1")
		req.Header.Set("Origin", origin)
		req.Header.Set("Authorization", "Bearer "+key)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		said, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(said)
	}
	key := strings.TrimSpace(string(token))
	for _, origin := range []string{"http://localhost:5173", "https://app.example"} {
		if status, body := ask(origin, key); status != http.StatusOK || !strings.Contains(body, `"name":"rig"`) {
			t.Errorf("a page from %s: %d %s", origin, status, body)
		}
	}
	if status, _ := ask("https://elsewhere.example", key); status != http.StatusForbidden {
		t.Errorf("a page from elsewhere: %d", status)
	}
	if status, _ := ask("http://localhost:5173", "not-the-token"); status != http.StatusUnauthorized {
		t.Errorf("a page without the token: %d", status)
	}

	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()
	spoiled := t.TempDir()
	if err := os.Mkdir(filepath.Join(spoiled, "api.token"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name, dataDir, listen, want string
	}{
		{"every interface", t.TempDir(), "0.0.0.0:50052", "must be a loopback address"},
		{"not an address", t.TempDir(), "nonsense", "must be a loopback address"},
		{"an address in use", t.TempDir(), taken.Addr().String(), "address already in use"},
		{"a token that cannot be read", spoiled, freeAddr(t), "read API token"},
	} {
		_, err := cli(t, "run", "--data-dir", tt.dataDir, "--listen", freeAddr(t), "--web-listen", tt.listen)
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("--web-listen %s: error %v, want one containing %q", tt.name, err, tt.want)
		}
	}
}
