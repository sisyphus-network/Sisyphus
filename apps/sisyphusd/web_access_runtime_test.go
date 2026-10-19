package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"math/big"
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

type webAccessStoreStub struct {
	config nodedb.WebAccessConfig
	getErr error
	setErr error
}

func (s *webAccessStoreStub) WebAccess() (nodedb.WebAccessConfig, error) {
	return s.config, s.getErr
}

func (s *webAccessStoreStub) SetWebAccess(config nodedb.WebAccessConfig) error {
	if s.setErr != nil {
		return s.setErr
	}
	s.config = config
	return nil
}

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
	if _, _, _, err := runtime.Set(context.Background(), config); err != nil {
		t.Fatalf("saving an unchanged live configuration: %v", err)
	}
	runtime.Close()
	if _, _, _, err := runtime.Set(context.Background(), config); err != nil {
		t.Fatalf("retrying an unchanged saved configuration: %v", err)
	}

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
	deadline := time.Now().Add(eventualWait)
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

func TestWebAccessRuntimeDatabaseAndFingerprintFailures(t *testing.T) {
	dbErr := errors.New("database unavailable")
	store := &webAccessStoreStub{getErr: dbErr}
	runtime := &webAccessRuntime{db: store, dataDir: t.TempDir(), local: &api.LocalConfig{Token: "test-token"}}
	if _, _, _, err := runtime.Get(); !errors.Is(err, dbErr) {
		t.Fatalf("Get error = %v, want database error", err)
	}
	if _, _, _, err := runtime.Set(context.Background(), nodedb.WebAccessConfig{}); !errors.Is(err, dbErr) {
		t.Fatalf("Set read error = %v, want database error", err)
	}
	if err := runtime.startSaved(); !errors.Is(err, dbErr) {
		t.Fatalf("startSaved error = %v, want database error", err)
	}

	store.getErr = nil
	store.config.Enabled = true
	config, fingerprint, restart, err := runtime.Get()
	if err != nil || config.Enabled || fingerprint != "" || !restart {
		t.Fatalf("saved-but-not-live access = %+v, fingerprint=%q restart=%v err=%v", config, fingerprint, restart, err)
	}
	runtime.fingerprint = func(string) (string, error) { return "", dbErr }
	if _, _, _, err := runtime.Get(); !errors.Is(err, dbErr) {
		t.Fatalf("fingerprint read error = %v, want injected error", err)
	}
}

func TestWebAccessRuntimeSetTransitionsAndRollback(t *testing.T) {
	dbErr := errors.New("cannot save")
	store := &webAccessStoreStub{}
	runtime := &webAccessRuntime{db: store, dataDir: t.TempDir(), local: &api.LocalConfig{Token: "test-token"}}
	addr := "192.0.2.10:50052"
	config, _, _, err := runtime.Set(context.Background(), nodedb.WebAccessConfig{ListenAddress: addr})
	if err != nil || config.ListenAddress != addr || !slices.Equal(config.ListenAddresses, []string{addr}) {
		t.Fatalf("saving disabled configuration = %+v, %v", config, err)
	}
	store.setErr = dbErr
	if _, _, _, err := runtime.Set(context.Background(), nodedb.WebAccessConfig{ListenAddress: addr}); !errors.Is(err, dbErr) {
		t.Fatalf("disabled save error = %v", err)
	}
	store.setErr = nil

	store.config = nodedb.WebAccessConfig{Enabled: true, ListenAddress: addr, ListenAddresses: []string{addr}, PublicURL: "https://node.example.test:50052"}
	if _, _, _, err := runtime.Set(context.Background(), nodedb.WebAccessConfig{Enabled: true, ListenAddress: "192.0.2.11:50052", ListenAddresses: []string{"192.0.2.11:50052"}, PublicURL: "https://node.example.test:50052"}); err == nil {
		t.Fatal("changed listener configuration was accepted while access remained enabled")
	}
	store.setErr = dbErr
	if _, _, _, err := runtime.Set(context.Background(), nodedb.WebAccessConfig{ListenAddress: addr}); !errors.Is(err, dbErr) {
		t.Fatalf("disable save error = %v", err)
	}
	store.setErr = nil
	if config, _, _, err := runtime.Set(context.Background(), nodedb.WebAccessConfig{ListenAddress: addr}); err != nil || config.Enabled {
		t.Fatalf("disable without a live server = %+v, %v", config, err)
	}

	ips := nonLoopbackInterfaceIPs(t)
	port := unusedTCPPort(t, ips[0])
	listenAddress := net.JoinHostPort(ips[0].String(), strconv.Itoa(port))
	publicURL := "https://" + listenAddress
	occupied, err := net.Listen("tcp", listenAddress)
	if err != nil {
		t.Skipf("could not reserve interface port for listener rollback test: %v", err)
	}
	store.config = nodedb.WebAccessConfig{}
	runtime.dataDir = t.TempDir()
	failed, fingerprint, _, err := runtime.Set(context.Background(), nodedb.WebAccessConfig{Enabled: true, ListenAddress: listenAddress, PublicURL: publicURL})
	occupied.Close()
	if err == nil || failed.Enabled || fingerprint == "" {
		t.Fatalf("occupied listener rollback = %+v fingerprint=%q err=%v", failed, fingerprint, err)
	}

	port = unusedTCPPort(t, ips[0])
	listenAddress = net.JoinHostPort(ips[0].String(), strconv.Itoa(port))
	store.config = nodedb.WebAccessConfig{}
	store.setErr = dbErr
	runtime.dataDir = t.TempDir()
	if _, _, _, err := runtime.Set(context.Background(), nodedb.WebAccessConfig{Enabled: true, ListenAddress: listenAddress, PublicURL: "https://" + listenAddress}); !errors.Is(err, dbErr) {
		t.Fatalf("save-after-listen error = %v", err)
	}
	if runtime.server != nil || len(runtime.listeners) != 0 {
		t.Fatal("listener remained open after settings could not be persisted")
	}
	store.setErr = nil

	store.config = nodedb.WebAccessConfig{}
	runtime.dataDir = t.TempDir()
	runtime.fingerprint = func(string) (string, error) { return "", dbErr }
	listenAddress = net.JoinHostPort(ips[0].String(), strconv.Itoa(unusedTCPPort(t, ips[0])))
	if config, _, _, err := runtime.Set(context.Background(), nodedb.WebAccessConfig{Enabled: true, ListenAddress: listenAddress, PublicURL: "https://" + listenAddress}); !config.Enabled || !errors.Is(err, dbErr) {
		t.Fatalf("fingerprint failure after successful start = %+v, %v", config, err)
	}
	runtime.Close()
}

func TestWebAccessRuntimeSetRejectsChangesAndRetriesSavedListener(t *testing.T) {
	ips := nonLoopbackInterfaceIPs(t)
	port := unusedTCPPort(t, ips[0])
	address := net.JoinHostPort(ips[0].String(), strconv.Itoa(port))
	config := nodedb.WebAccessConfig{Enabled: true, ListenAddress: address, ListenAddresses: []string{address}, PublicURL: "https://" + address}
	store := &webAccessStoreStub{config: config}
	occupier, err := net.Listen("tcp", address)
	if err != nil {
		t.Skipf("could not reserve listener address: %v", err)
	}
	runtime := &webAccessRuntime{db: store, dataDir: t.TempDir(), local: &api.LocalConfig{Token: "token"}}
	if _, _, _, err := runtime.Set(context.Background(), config); err == nil || !strings.Contains(err.Error(), "listen for remote browser access") {
		t.Fatalf("retry with occupied listener error = %v", err)
	}
	occupier.Close()
	if _, _, _, err := runtime.Set(context.Background(), config); err != nil {
		t.Fatalf("retry saved listener after address became available: %v", err)
	}
	defer runtime.Close()
	changed := config
	changed.PublicURL = "https://other.example.test"
	if _, _, _, err := runtime.Set(context.Background(), changed); err == nil || !strings.Contains(err.Error(), "turn remote access off") {
		t.Fatalf("changing a live listener configuration error = %v", err)
	}
}

func TestWebAccessStartReportsConfigurationAndCertificateErrors(t *testing.T) {
	runtime := &webAccessRuntime{dataDir: t.TempDir(), local: &api.LocalConfig{Token: "token"}}
	if err := runtime.start(nodedb.WebAccessConfig{}); err == nil || !strings.Contains(err.Error(), "choose at least one") {
		t.Fatalf("empty listener error = %v", err)
	}
	ips := nonLoopbackInterfaceIPs(t)
	address := net.JoinHostPort(ips[0].String(), strconv.Itoa(unusedTCPPort(t, ips[0])))
	base := nodedb.WebAccessConfig{ListenAddress: address, ListenAddresses: []string{address}, PublicURL: "http://node.example.test"}
	if err := runtime.start(base); err == nil || !strings.Contains(err.Error(), "HTTPS origin") {
		t.Fatalf("invalid public URL error = %v", err)
	}
	base.PublicURL = "https://node.example.test"
	if err := os.WriteFile(filepath.Join(runtime.dataDir, "web-access.crt"), []byte("invalid"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ensureWebAccessCertificate(runtime.dataDir, address, "node.example.test"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runtime.dataDir, "web-access.key"), []byte("invalid"), 0o600); err != nil {
		t.Fatal(err)
	}
	oldLoad := loadWebAccessKeyPair
	t.Cleanup(func() { loadWebAccessKeyPair = oldLoad })
	loadWebAccessKeyPair = func(string, string) (tls.Certificate, error) {
		return tls.Certificate{}, errors.New("key pair unavailable")
	}
	if err := runtime.start(base); err == nil || !strings.Contains(err.Error(), "load browser-access TLS certificate") {
		t.Fatalf("invalid saved certificate error = %v", err)
	}
	runtime.dataDir = filepath.Join(t.TempDir(), "missing")
	if err := runtime.start(base); err == nil {
		t.Fatal("certificate creation failure under missing data directory was ignored")
	}
}

func TestPrimaryWebListenAddressWithoutSelection(t *testing.T) {
	if address, addresses := primaryWebListenAddress(nodedb.WebAccessConfig{}); address != "" || addresses != nil {
		t.Fatalf("empty listener fallback = %q, %v", address, addresses)
	}
}

func TestWebAccessStartClosesEarlierListenersOnLaterFailure(t *testing.T) {
	ips := nonLoopbackInterfaceIPs(t)
	firstAddress := net.JoinHostPort(ips[0].String(), "1")
	secondAddress := net.JoinHostPort(ips[0].String(), "2")
	var first *closeTrackingListener
	listenCalls := 0
	runtime := &webAccessRuntime{
		dataDir: t.TempDir(), local: &api.LocalConfig{Token: "token"},
		listen: func(network, address string) (net.Listener, error) {
			listenCalls++
			if listenCalls == 2 {
				return nil, errors.New("address already in use")
			}
			listener, err := net.Listen(network, net.JoinHostPort(ips[0].String(), "0"))
			if err != nil {
				return nil, err
			}
			first = &closeTrackingListener{Listener: listener}
			return first, nil
		},
	}
	config := nodedb.WebAccessConfig{
		ListenAddress: firstAddress, ListenAddresses: []string{firstAddress, secondAddress},
		PublicURL: "https://" + ips[0].String(),
	}
	err := runtime.start(config)
	if err == nil || !strings.Contains(err.Error(), "listen for remote browser access") {
		t.Fatalf("secondary listen error = %v", err)
	}
	if first == nil || first.closeCalls != 1 {
		t.Fatalf("first listener close calls = %v, want 1", first)
	}
}

type closeTrackingListener struct {
	net.Listener
	closeCalls int
}

func (l *closeTrackingListener) Close() error {
	l.closeCalls++
	return l.Listener.Close()
}

func TestWebAccessStartClosesEarlierListenersOnValidationFailure(t *testing.T) {
	ips := nonLoopbackInterfaceIPs(t)
	firstAddress := net.JoinHostPort(ips[0].String(), strconv.Itoa(unusedTCPPort(t, ips[0])))
	secondAddress := "127.0.0.1:50052"
	runtime := &webAccessRuntime{dataDir: t.TempDir(), local: &api.LocalConfig{Token: "token"}}
	err := runtime.start(nodedb.WebAccessConfig{
		ListenAddress: firstAddress, ListenAddresses: []string{firstAddress, secondAddress},
		PublicURL: "https://" + ips[0].String(),
	})
	if err == nil || !strings.Contains(err.Error(), "non-loopback") {
		t.Fatalf("secondary validation error = %v", err)
	}
	probe, err := net.Listen("tcp", firstAddress)
	if err != nil {
		t.Fatalf("first listener leaked after secondary validation failed: %v", err)
	}
	probe.Close()
}

func TestWebAccessStartLogsSuccessfulListener(t *testing.T) {
	ips := nonLoopbackInterfaceIPs(t)
	address := net.JoinHostPort(ips[0].String(), strconv.Itoa(unusedTCPPort(t, ips[0])))
	var logs bytes.Buffer
	runtime := &webAccessRuntime{
		dataDir: t.TempDir(), local: &api.LocalConfig{Token: "token"},
		ui:  fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("ok")}},
		log: slog.New(slog.NewTextHandler(&logs, nil)),
	}
	if err := runtime.start(nodedb.WebAccessConfig{
		ListenAddress: address, ListenAddresses: []string{address}, PublicURL: "https://" + ips[0].String(),
	}); err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	if !strings.Contains(logs.String(), "remote browser access listening over HTTPS") || !strings.Contains(logs.String(), address) {
		t.Fatalf("listener was not logged: %s", logs.String())
	}
}

func TestStartSavedWebAccessHandlesSavedStateAndTokenCreation(t *testing.T) {
	store := &webAccessStoreStub{}
	runtime := &webAccessRuntime{db: store, dataDir: t.TempDir()}
	local := &api.LocalConfig{}
	if err := startSavedWebAccess(local, runtime); err != nil {
		t.Fatalf("disabled access startup: %v", err)
	}
	store.getErr = errors.New("read config")
	if err := startSavedWebAccess(local, runtime); err == nil {
		t.Fatal("database error starting saved web access was ignored")
	}
	store.getErr = nil
	store.config.Enabled = true
	store.config.ListenAddress = "invalid"
	local.Token = ""
	runtime.dataDir = filepath.Join(t.TempDir(), "missing")
	if err := startSavedWebAccess(local, runtime); err == nil {
		t.Fatal("token creation under a missing data directory unexpectedly succeeded")
	}
	runtime.dataDir = t.TempDir()
	if err := startSavedWebAccess(local, runtime); err == nil || local.Token == "" {
		t.Fatalf("saved settings failure/token creation = err %v token-empty=%v", err, local.Token == "")
	}
	if _, err := os.Stat(filepath.Join(runtime.dataDir, "api.token")); err != nil {
		t.Fatalf("API token was not persisted before starting the configured listener: %v", err)
	}
}

func TestWaitForDaemonStopPropagatesStartupAndShutdown(t *testing.T) {
	startupErr := errors.New("saved access failed")
	failed := &webAccessRuntime{db: &webAccessStoreStub{getErr: startupErr}}
	if err := waitForDaemonStop(context.Background(), &api.LocalConfig{}, failed, make(chan error), slog.Default()); !errors.Is(err, startupErr) {
		t.Fatalf("saved access startup error = %v", err)
	}

	stopErr := errors.New("node stopped")
	stopped := make(chan error, 1)
	stopped <- stopErr
	ready := &webAccessRuntime{db: &webAccessStoreStub{}}
	if err := waitForDaemonStop(context.Background(), &api.LocalConfig{}, ready, stopped, slog.Default()); !errors.Is(err, stopErr) {
		t.Fatalf("daemon stop error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := waitForDaemonStop(ctx, &api.LocalConfig{}, ready, make(chan error), slog.Default()); err != nil {
		t.Fatalf("context shutdown error = %v", err)
	}
}

func TestNormalizeWebAccessConfigValidationAndCanonicalization(t *testing.T) {
	ips := nonLoopbackInterfaceIPs(t)
	ip := ips[0].String()
	port := strconv.Itoa(unusedTCPPort(t, ips[0]))
	validAddress := net.JoinHostPort(ip, port)
	differentPort := strconv.Itoa((mustPort(t, port) % 65534) + 1)

	tests := []struct {
		name    string
		config  nodedb.WebAccessConfig
		wantErr bool
	}{
		{name: "no address", config: nodedb.WebAccessConfig{}, wantErr: true},
		{name: "malformed listener", config: nodedb.WebAccessConfig{ListenAddress: "not-an-address"}, wantErr: true},
		{name: "invalid port", config: nodedb.WebAccessConfig{ListenAddress: net.JoinHostPort(ip, "65536")}, wantErr: true},
		{name: "port zero", config: nodedb.WebAccessConfig{ListenAddress: net.JoinHostPort(ip, "0")}, wantErr: true},
		{name: "different interface ports", config: nodedb.WebAccessConfig{ListenAddresses: []string{validAddress, net.JoinHostPort(ip, differentPort)}}, wantErr: true},
		{name: "loopback", config: nodedb.WebAccessConfig{ListenAddress: "127.0.0.1:50052"}, wantErr: true},
		{name: "wildcard", config: nodedb.WebAccessConfig{ListenAddress: "0.0.0.0:50052"}, wantErr: true},
		{name: "multicast", config: nodedb.WebAccessConfig{ListenAddress: "224.0.0.1:50052"}, wantErr: true},
		{name: "link local", config: nodedb.WebAccessConfig{ListenAddress: "169.254.1.2:50052"}, wantErr: true},
		{name: "public URL required to be origin", config: nodedb.WebAccessConfig{ListenAddress: validAddress, PublicURL: "http://node.example.test"}, wantErr: true},
		{name: "deduplicates and defaults URL", config: nodedb.WebAccessConfig{ListenAddresses: []string{validAddress, validAddress}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			test.config.Enabled = true
			got, err := normalizeWebAccessConfig(test.config)
			if (err != nil) != test.wantErr {
				t.Fatalf("normalize error = %v, wantErr=%v", err, test.wantErr)
			}
			if test.wantErr {
				return
			}
			if !slices.Equal(got.ListenAddresses, []string{validAddress}) || got.ListenAddress != validAddress {
				t.Fatalf("canonical listener addresses = %+v", got)
			}
			if got.PublicURL != "https://"+validAddress {
				t.Fatalf("default public URL = %q", got.PublicURL)
			}
		})
	}
}

func mustPort(t *testing.T, value string) int {
	t.Helper()
	port, err := strconv.Atoi(value)
	if err != nil {
		t.Fatal(err)
	}
	return port
}

func TestValidateWebListenTLSRejectsInvalidAndUnavailableAddresses(t *testing.T) {
	for _, address := range []string{"bad", "192.0.2.1:0", "127.0.0.1:50052", "0.0.0.0:50052", "224.0.0.1:50052", "169.254.1.2:50052"} {
		if err := validateWebListenTLS(address, false, false); err == nil {
			t.Errorf("validateWebListenTLS(%q) unexpectedly succeeded", address)
		}
	}
	if err := validateWebListenTLS("203.0.113.10:50052", false, false); err != nil {
		t.Fatalf("valid unassigned address is allowed when assignment is not required: %v", err)
	}

	oldInterfaces, oldAddresses := webAccessInterfaces, webAccessInterfaceAddresses
	t.Cleanup(func() { webAccessInterfaces, webAccessInterfaceAddresses = oldInterfaces, oldAddresses })
	webAccessInterfaces = func() ([]net.Interface, error) { return nil, errors.New("interface enumeration failed") }
	if err := validateWebListenTLS("203.0.113.10:50052", true, true); err == nil || !strings.Contains(err.Error(), "inspect local network interfaces") {
		t.Fatalf("interface enumeration error = %v", err)
	}
	webAccessInterfaces = func() ([]net.Interface, error) {
		return []net.Interface{{Name: "test0", Flags: net.FlagUp | net.FlagRunning}}, nil
	}
	webAccessInterfaceAddresses = func(net.Interface) ([]net.Addr, error) { return nil, errors.New("addresses unavailable") }
	if err := validateWebListenTLS("203.0.113.10:50052", true, true); err == nil {
		t.Fatal("unassigned address unexpectedly passed validation")
	}
	webAccessInterfaceAddresses = func(net.Interface) ([]net.Addr, error) {
		return []net.Addr{testNetAddr("203.0.113.10/24")}, nil
	}
	if err := validateWebListenTLS("203.0.113.10:50052", true, true); err != nil {
		t.Fatalf("assigned usable address rejected: %v", err)
	}
}

type testNetAddr string

func (a testNetAddr) Network() string { return "test" }
func (a testNetAddr) String() string  { return string(a) }

func TestWebAccessURLParsing(t *testing.T) {
	for _, raw := range []string{"", "http://node.example.test", "https://", "https://user:pass@node.example.test", "https://node.example.test/path", "https://node.example.test?query", "https://node.example.test#fragment", "https://bad host", "https://" + strings.Repeat("a", 254)} {
		if _, err := parseWebAccessURL(raw); err == nil {
			t.Errorf("parseWebAccessURL(%q) unexpectedly succeeded", raw)
		}
	}
	parsed, err := parseWebAccessURL("https://node.example.test:50052/")
	if err != nil || parsed.String() != "https://node.example.test:50052" {
		t.Fatalf("normalized HTTPS origin = %v, %v", parsed, err)
	}
}

func TestWebAccessCertificateFailureAndReusePaths(t *testing.T) {
	if _, _, err := ensureWebAccessCertificateForAddresses(t.TempDir(), []string{"bad"}, "node.example.test"); err == nil {
		t.Fatal("malformed listener address unexpectedly generated a certificate")
	}
	if _, _, err := ensureWebAccessCertificateForAddresses(t.TempDir(), []string{"localhost:50052"}, "node.example.test"); err == nil {
		t.Fatal("non-IP listener unexpectedly generated a certificate")
	}
	if _, _, err := ensureWebAccessCertificateForAddresses(t.TempDir(), nil, "node.example.test"); err == nil {
		t.Fatal("empty listener list unexpectedly generated a certificate")
	}

	ips := nonLoopbackInterfaceIPs(t)
	address := net.JoinHostPort(ips[0].String(), "50052")
	dir := t.TempDir()
	cert, key, err := ensureWebAccessCertificate(dir, address, ips[0].String())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := ensureWebAccessCertificate(dir, address, ips[0].String()); err != nil {
		t.Fatalf("reuse an existing matching certificate: %v", err)
	}
	if !certificateCoversAll(cert, []netip.Addr{ips[0]}, ips[0].String()) {
		t.Fatal("fresh certificate does not cover the requested host and listener")
	}
	if certificateCoversAll(cert, []netip.Addr{ips[0]}, "other.example.test") || certificateCoversAll(cert, []netip.Addr{netip.MustParseAddr("192.0.2.44")}, ips[0].String()) {
		t.Fatal("certificate was reported to cover an unlisted host or address")
	}
	if _, err := os.Stat(key); err != nil {
		t.Fatal(err)
	}
	dnsDir := t.TempDir()
	dnsCert, _, err := ensureWebAccessCertificate(dnsDir, address, "node.example.test")
	if err != nil {
		t.Fatal(err)
	}
	dnsPEM, err := os.ReadFile(dnsCert)
	if err != nil {
		t.Fatal(err)
	}
	dnsBlock, _ := pem.Decode(dnsPEM)
	dnsParsed, err := x509.ParseCertificate(dnsBlock.Bytes)
	if err != nil || !slices.Contains(dnsParsed.DNSNames, "node.example.test") {
		t.Fatalf("DNS SAN missing from generated certificate: %#v, %v", dnsParsed, err)
	}

	oldKey, oldSerial := generateWebAccessKey, generateWebAccessSerial
	oldCreate, oldMarshal := createWebAccessCertificate, marshalWebAccessKey
	t.Cleanup(func() {
		generateWebAccessKey, generateWebAccessSerial = oldKey, oldSerial
		createWebAccessCertificate, marshalWebAccessKey = oldCreate, oldMarshal
	})
	failure := errors.New("injected crypto failure")
	generateWebAccessKey = func() (*ecdsa.PrivateKey, error) { return nil, failure }
	if _, _, err := ensureWebAccessCertificate(t.TempDir(), address, ips[0].String()); !errors.Is(err, failure) {
		t.Fatalf("private-key generation error = %v", err)
	}
	generateWebAccessKey = func() (*ecdsa.PrivateKey, error) { return ecdsa.GenerateKey(elliptic.P256(), rand.Reader) }
	generateWebAccessSerial = func() (*big.Int, error) { return nil, failure }
	if _, _, err := ensureWebAccessCertificate(t.TempDir(), address, ips[0].String()); !errors.Is(err, failure) {
		t.Fatalf("serial generation error = %v", err)
	}
	generateWebAccessSerial = func() (*big.Int, error) { return big.NewInt(1), nil }
	createWebAccessCertificate = func(io.Reader, *x509.Certificate, *x509.Certificate, any, any) ([]byte, error) { return nil, failure }
	if _, _, err := ensureWebAccessCertificate(t.TempDir(), address, ips[0].String()); !errors.Is(err, failure) {
		t.Fatalf("certificate creation error = %v", err)
	}
	createWebAccessCertificate = x509.CreateCertificate
	marshalWebAccessKey = func(*ecdsa.PrivateKey) ([]byte, error) { return nil, failure }
	if _, _, err := ensureWebAccessCertificate(t.TempDir(), address, ips[0].String()); !errors.Is(err, failure) {
		t.Fatalf("private-key encoding error = %v", err)
	}
	marshalWebAccessKey = x509.MarshalECPrivateKey
	if _, _, err := ensureWebAccessCertificate(filepath.Join(t.TempDir(), "missing"), address, ips[0].String()); err == nil {
		t.Fatal("unwritable key path unexpectedly succeeded")
	}
	certDirectory := t.TempDir()
	if err := os.Mkdir(filepath.Join(certDirectory, "web-access.crt"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ensureWebAccessCertificate(certDirectory, address, ips[0].String()); err == nil {
		t.Fatal("certificate destination directory unexpectedly succeeded")
	}
}

func TestWebAccessCertificateParsingAndPrivateFileErrors(t *testing.T) {
	dir := t.TempDir()
	if fingerprint, err := webAccessFingerprint(dir); err != nil || fingerprint != "" {
		t.Fatalf("missing certificate fingerprint=%q err=%v", fingerprint, err)
	}
	certPath := filepath.Join(dir, "web-access.crt")
	if err := os.WriteFile(certPath, []byte("not pem"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := webAccessFingerprint(dir); err == nil {
		t.Fatal("malformed PEM unexpectedly produced a fingerprint")
	}
	if certificateCoversAll(certPath, []netip.Addr{netip.MustParseAddr("192.0.2.1")}, "node.example.test") {
		t.Fatal("non-PEM certificate unexpectedly matched")
	}
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("invalid DER")}), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := webAccessFingerprint(dir); err == nil {
		t.Fatal("malformed certificate unexpectedly produced a fingerprint")
	}
	if certificateCoversAll(certPath, []netip.Addr{netip.MustParseAddr("192.0.2.1")}, "node.example.test") {
		t.Fatal("invalid certificate unexpectedly matched")
	}
	if err := writePrivateFile(filepath.Join(dir, "missing", "key"), []byte("key")); err == nil {
		t.Fatal("missing parent directory unexpectedly accepted the private key")
	}
	dataDirFile := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(dataDirFile, []byte("file"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := webAccessFingerprint(dataDirFile); err == nil {
		t.Fatal("fingerprint read error from non-directory data path was ignored")
	}

	oldOpen := openWebAccessKeyFile
	t.Cleanup(func() { openWebAccessKeyFile = oldOpen })
	openWebAccessKeyFile = func(string, int, os.FileMode) (privateKeyFile, error) { return testPrivateFile{}, nil }
	if err := writePrivateFile(filepath.Join(dir, "key"), []byte("key")); err == nil || err.Error() != "chmod failed" {
		t.Fatalf("chmod failure = %v", err)
	}
	openWebAccessKeyFile = func(string, int, os.FileMode) (privateKeyFile, error) {
		return testPrivateFile{writeErr: errors.New("write failed")}, nil
	}
	if err := writePrivateFile(filepath.Join(dir, "key"), []byte("key")); err == nil || err.Error() != "write failed" {
		t.Fatalf("write failure = %v", err)
	}
	openWebAccessKeyFile = func(string, int, os.FileMode) (privateKeyFile, error) {
		return testPrivateFile{closeErr: errors.New("close failed")}, nil
	}
	if err := writePrivateFile(filepath.Join(dir, "key"), []byte("key")); err == nil || err.Error() != "close failed" {
		t.Fatalf("close failure = %v", err)
	}
}

type testPrivateFile struct {
	writeErr error
	closeErr error
}

func (f testPrivateFile) Chmod(os.FileMode) error {
	if f.writeErr != nil {
		return nil
	}
	if f.closeErr != nil {
		return nil
	}
	return errors.New("chmod failed")
}
func (f testPrivateFile) Write([]byte) (int, error) {
	if f.writeErr != nil {
		return 0, f.writeErr
	}
	return 1, nil
}
func (f testPrivateFile) Close() error { return f.closeErr }

func TestWebAccessListenerFailureStopsAllInterfaces(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	server := &http.Server{}
	runtime := &webAccessRuntime{server: server, listeners: []net.Listener{listener}, addr: listener.Addr().String(), log: slog.New(slog.NewTextHandler(&logs, nil))}
	runtime.stopAfterListenerFailure(server, listener, errors.New("serve failed"))
	if runtime.server != nil || len(runtime.listeners) != 0 || runtime.addr != "" || !strings.Contains(logs.String(), "remote browser access stopped") {
		t.Fatalf("listener failure state server=%v listeners=%v addr=%q logs=%q", runtime.server, runtime.listeners, runtime.addr, logs.String())
	}

	staleListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer staleListener.Close()
	current := &http.Server{}
	runtime.server, runtime.listeners = current, []net.Listener{staleListener}
	runtime.stopAfterListenerFailure(server, staleListener, errors.New("stale failure"))
	if runtime.server != current || len(runtime.listeners) != 1 {
		t.Fatal("stale listener failure stopped its replacement")
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
