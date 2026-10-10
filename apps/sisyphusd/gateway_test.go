package main

import (
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestANodesStoredFilesAreFetchedThroughItsGateway(t *testing.T) {
	fetch := func(at, path string) (int, string) {
		res, err := http.Get("http://" + at + path)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		body, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(body)
	}
	input := filepath.Join(t.TempDir(), "words.txt")
	os.WriteFile(input, []byte("the boulder rolls"), 0o600)

	// Its owner's by default: the token is asked for.
	dataDir := t.TempDir()
	addr, gateway := freeAddr(t), freeAddr(t)
	startDaemon(t, "--data-dir", dataDir, "--listen", addr, "--name", "rig", "--gateway-listen", gateway)
	waitForOutput(t, "rig", "nodes", "--addr", addr)
	cid := strings.TrimSpace(mustCLI(t, "blob", "put", "--addr", addr, input))
	token, _ := os.ReadFile(filepath.Join(dataDir, "api.token"))
	if status, _ := fetch(gateway, "/ipfs/"+cid); status != http.StatusUnauthorized {
		t.Errorf("without the token: %d", status)
	}
	if status, body := fetch(gateway, "/ipfs/"+cid+"?token="+strings.TrimSpace(string(token))); status != http.StatusOK || body != "the boulder rolls" {
		t.Errorf("with the token: %d %q", status, body)
	}

	// Opened, it is anyone's who knows the ID.
	addr, gateway = freeAddr(t), freeAddr(t)
	startDaemon(t, "--data-dir", t.TempDir(), "--listen", addr, "--name", "open", "--gateway-listen", gateway, "--gateway-open")
	waitForOutput(t, "open", "nodes", "--addr", addr)
	cid = strings.TrimSpace(mustCLI(t, "blob", "put", "--addr", addr, input))
	if status, body := fetch(gateway, "/ipfs/"+cid); status != http.StatusOK || body != "the boulder rolls" {
		t.Errorf("from an open gateway: %d %q", status, body)
	}

	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()
	spoiled := t.TempDir()
	os.Mkdir(filepath.Join(spoiled, "api.token"), 0o700)
	for _, tt := range []struct{ name, dataDir, listen, want string }{
		{"an address in use", t.TempDir(), taken.Addr().String(), "address already in use"},
		{"a token that cannot be read", spoiled, freeAddr(t), "read API token"},
	} {
		if _, err := cli(t, "run", "--data-dir", tt.dataDir, "--listen", freeAddr(t), "--gateway-listen", tt.listen); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("--gateway-listen %s: %v, want %q", tt.name, err, tt.want)
		}
	}
}

// A service that takes a key over plain HTTP says so when other machines
// can reach it, and says nothing when only this one can.
func TestAServiceThatTakesItsKeyInTheClearFromOtherMachinesSaysSo(t *testing.T) {
	for addr, warned := range map[string]bool{"127.0.0.1:8080": false, "[::1]:8080": false, "0.0.0.0:8080": true, "192.0.2.7:8080": true, "[::]:8080": true} {
		logs := new(syncBuffer)
		at, err := net.ResolveTCPAddr("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		warnOfKeyInTheClear(slog.New(slog.NewTextHandler(logs, nil)), at, "the gateway", "the node's API token")
		said := strings.Contains(logs.String(), "the gateway can be reached from other machines, over plain HTTP: the node's API token crosses the network unencrypted with every request") && strings.Contains(logs.String(), "addr="+at.String())
		if said != warned || !warned && logs.String() != "" {
			t.Errorf("listening at %s: %q", addr, logs.String())
		}
	}
	// What is not a TCP address is not known to be this machine's alone.
	logs := new(syncBuffer)
	warnOfKeyInTheClear(slog.New(slog.NewTextHandler(logs, nil)), &net.UnixAddr{Name: "/run/gateway.sock", Net: "unix"}, "the pinning service", "its key")
	if !strings.Contains(logs.String(), "the pinning service can be reached from other machines") {
		t.Errorf("listening at a socket: %q", logs.String())
	}
}
