package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"testing/fstest"

	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/api"
	"github.com/sisyphus-network/Sisyphus/packages/nodedb"
)

func TestRemoteWebAccessIsOptInTLSAndCanBeDisabled(t *testing.T) {
	db, _ := nodedb.Open(filepath.Join(t.TempDir(), "node.db"))
	defer db.Close()
	listenIPs := nonLoopbackInterfaceIPs(t)
	listenIP := listenIPs[0]
	port := unusedTCPPort(t, listenIP)
	address := net.JoinHostPort(listenIP.String(), strconv.Itoa(port))
	listenAddresses := []string{address}
	if len(listenIPs) > 1 {
		listenAddresses = append(listenAddresses, net.JoinHostPort(listenIPs[1].String(), strconv.Itoa(port)))
	}
	publicURL := "https://" + address
	dataDir := t.TempDir()
	runtime := &webAccessRuntime{
		db: db, dataDir: dataDir, log: nil,
		local: &api.LocalConfig{NodeID: "node", Token: "local-token", BrowserAccess: db},
		ui:    fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("<main>served over tls</main>")}},
	}

	initial, fingerprint, restart, err := runtime.Get()
	if err != nil || initial.Enabled || fingerprint != "" || restart {
		t.Fatalf("new-node access config = %+v fingerprint=%q restart=%v err=%v", initial, fingerprint, restart, err)
	}

	config, fingerprint, restart, err := runtime.Set(context.Background(), nodedb.WebAccessConfig{
		Enabled: true, ListenAddress: address, ListenAddresses: listenAddresses, PublicURL: publicURL,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !config.Enabled || config.ListenAddress != address || !slices.Equal(config.ListenAddresses, listenAddresses) || config.PublicURL != publicURL || fingerprint == "" || restart {
		t.Fatalf("enabled access config = %+v fingerprint=%q restart=%v", config, fingerprint, restart)
	}
	runtime.Close()
	runtime = &webAccessRuntime{
		db: db, dataDir: dataDir,
		local: &api.LocalConfig{NodeID: "node", Token: "local-token", BrowserAccess: db},
		ui:    fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("<main>served over tls</main>")}},
	}
	if err := runtime.startSaved(); err != nil {
		t.Fatalf("restore saved HTTPS listener after daemon restart: %v", err)
	}
	defer runtime.Close()

	certFile, keyFile, err := ensureWebAccessCertificate(dataDir, address, listenIP.String())
	if err != nil {
		t.Fatal(err)
	}
	certPEM, err := os.ReadFile(certFile)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(certPEM)
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	for _, ip := range listenIPs[:len(listenAddresses)] {
		if err := cert.VerifyHostname(ip.String()); err != nil {
			t.Errorf("self-signed certificate does not cover listener IP %s: %v", ip, err)
		}
	}
	keyInfo, err := os.Stat(keyFile)
	if err != nil {
		t.Fatal(err)
	}
	if keyInfo.Mode().Perm() != 0o600 {
		t.Errorf("private key permissions = %o, want 600", keyInfo.Mode().Perm())
	}

	client := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}} // certificate is self-signed by design in this test
	for _, ip := range listenIPs[:len(listenAddresses)] {
		url := "https://" + net.JoinHostPort(ip.String(), strconv.Itoa(port)) + "/"
		response, err := client.Get(url)
		if err != nil {
			t.Fatalf("GET over TLS at %s: %v", ip, err)
		}
		body, readErr := io.ReadAll(response.Body)
		response.Body.Close()
		if readErr != nil || response.StatusCode != http.StatusOK || !strings.Contains(string(body), "served over tls") {
			t.Fatalf("served page at %s status=%d body=%q err=%v", ip, response.StatusCode, body, readErr)
		}
	}

	runtime.mu.Lock()
	failedListener := runtime.listeners[0]
	runtime.mu.Unlock()
	if err := failedListener.Close(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		live, _, retryRequired, err := runtime.Get()
		if err != nil {
			t.Fatal(err)
		}
		if !live.Enabled && retryRequired {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("listener failure did not change the reported live state: config=%+v retry=%v", live, retryRequired)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, _, _, err := runtime.Set(context.Background(), config); err != nil {
		t.Fatalf("retry saved listener configuration: %v", err)
	}
	if live, _, retryRequired, err := runtime.Get(); err != nil || !live.Enabled || retryRequired {
		t.Fatalf("listener state after retry = %+v, retry=%v err=%v", live, retryRequired, err)
	}

	if _, _, _, err := runtime.Set(context.Background(), nodedb.WebAccessConfig{ListenAddress: address, ListenAddresses: listenAddresses}); err != nil {
		t.Fatalf("disable access: %v", err)
	}
	if saved, err := db.WebAccess(); err != nil || saved.Enabled {
		t.Fatalf("saved access after disable = %+v, %v", saved, err)
	}
	time.Sleep(800 * time.Millisecond)
	response, err := client.Get(publicURL + "/")
	if err == nil {
		response.Body.Close()
		t.Fatal("listener still accepted requests after access was disabled")
	}
}

func nonLoopbackInterfaceIPs(t *testing.T) []netip.Addr {
	t.Helper()
	interfaces, err := net.Interfaces()
	if err != nil {
		t.Fatal(err)
	}
	seen := make(map[netip.Addr]struct{})
	var result []netip.Addr
	for _, network := range interfaces {
		if network.Flags&net.FlagUp == 0 || network.Flags&net.FlagRunning == 0 || network.Flags&net.FlagLoopback != 0 {
			continue
		}
		addresses, err := network.Addrs()
		if err != nil {
			continue
		}
		for _, address := range addresses {
			prefix, err := netip.ParsePrefix(address.String())
			if err == nil && prefix.Addr().IsValid() && !prefix.Addr().IsLoopback() && !prefix.Addr().IsUnspecified() && !prefix.Addr().IsMulticast() && !prefix.Addr().IsLinkLocalUnicast() {
				address := prefix.Addr().Unmap()
				if _, exists := seen[address]; !exists {
					seen[address] = struct{}{}
					result = append(result, address)
				}
			}
		}
	}
	if len(result) == 0 {
		t.Skip("host has no non-loopback interface address")
	}
	return result
}

func unusedTCPPort(t *testing.T, ip netip.Addr) int {
	t.Helper()
	listener, err := net.Listen("tcp", net.JoinHostPort(ip.String(), "0"))
	if err != nil {
		t.Skipf("cannot bind a temporary port on %s: %v", ip, err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}
