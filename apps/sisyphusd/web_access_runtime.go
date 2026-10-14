package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/api"
	"github.com/sisyphus-network/Sisyphus/packages/nodedb"
)

// webAccessRuntime owns the optional, HTTPS-only browser listener. The local
// Electron API and the node-to-node P2P listener are deliberately independent.
type webAccessRuntime struct {
	mu          sync.Mutex
	db          webAccessStore
	dataDir     string
	local       *api.LocalConfig
	origins     []string
	ui          fs.FS
	log         *slog.Logger
	server      *http.Server
	addr        string
	listeners   []net.Listener
	fingerprint func(string) (string, error)
}

type webAccessStore interface {
	WebAccess() (nodedb.WebAccessConfig, error)
	SetWebAccess(nodedb.WebAccessConfig) error
}

var webAccessInterfaces = net.Interfaces
var webAccessInterfaceAddresses = func(network net.Interface) ([]net.Addr, error) { return network.Addrs() }
var generateWebAccessKey = func() (*ecdsa.PrivateKey, error) { return ecdsa.GenerateKey(elliptic.P256(), rand.Reader) }
var generateWebAccessSerial = func() (*big.Int, error) { return rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128)) }
var createWebAccessCertificate = x509.CreateCertificate
var marshalWebAccessKey = x509.MarshalECPrivateKey
var loadWebAccessKeyPair = tls.LoadX509KeyPair
var openWebAccessKeyFile = func(path string, flag int, mode os.FileMode) (privateKeyFile, error) {
	return os.OpenFile(path, flag, mode)
}

type privateKeyFile interface {
	Chmod(os.FileMode) error
	Write([]byte) (int, error)
	Close() error
}

func (w *webAccessRuntime) readFingerprint() (string, error) {
	if w.fingerprint != nil {
		return w.fingerprint(w.dataDir)
	}
	return webAccessFingerprint(w.dataDir)
}

func (w *webAccessRuntime) Get() (nodedb.WebAccessConfig, string, bool, error) {
	config, err := w.db.WebAccess()
	if err != nil {
		return nodedb.WebAccessConfig{}, "", false, err
	}
	w.mu.Lock()
	serving := w.server != nil && len(w.listeners) > 0
	w.mu.Unlock()
	restartRequired := config.Enabled && !serving
	if restartRequired {
		// Report the live listener state, not merely the persisted preference.
		// The saved configuration remains intact and can be retried by saving it.
		config.Enabled = false
	}
	fingerprint, err := w.readFingerprint()
	return config, fingerprint, restartRequired, err
}

func (w *webAccessRuntime) Set(ctx context.Context, config nodedb.WebAccessConfig) (nodedb.WebAccessConfig, string, bool, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	previous, err := w.db.WebAccess()
	if err != nil {
		return nodedb.WebAccessConfig{}, "", false, err
	}
	if config.Enabled {
		config, err = normalizeWebAccessConfig(config)
		if err != nil {
			return previous, "", false, err
		}
	}
	if previous.Enabled && config.Enabled {
		if sameWebAccessConfig(previous, config) {
			serving := w.server != nil && len(w.listeners) > 0
			if !serving {
				if err := w.start(previous); err != nil {
					return previous, "", false, err
				}
			}
			fingerprint, err := w.readFingerprint()
			return previous, fingerprint, false, err
		}
		return previous, "", false, errors.New("turn remote access off before changing its listener or public URL")
	}
	if !previous.Enabled && !config.Enabled {
		config.ListenAddress, config.ListenAddresses = primaryWebListenAddress(config)
		if err := w.db.SetWebAccess(config); err != nil {
			return previous, "", false, fmt.Errorf("save web access settings: %w", err)
		}
		return config, "", false, nil
	}
	if previous.Enabled && !config.Enabled {
		if err := w.db.SetWebAccess(config); err != nil {
			return previous, "", false, fmt.Errorf("save web access settings: %w", err)
		}
		server, listeners := w.server, w.listeners
		w.server, w.listeners, w.addr = nil, nil, ""
		if server != nil {
			// Let the current API call return its response before closing its
			// own HTTPS listener. The disabled state is already durable.
			go func() {
				time.Sleep(750 * time.Millisecond)
				_ = server.Close()
				for _, listener := range listeners {
					_ = listener.Close()
				}
			}()
		}
		return config, "", false, nil
	}
	if err := w.start(config); err != nil {
		fingerprint, _ := w.readFingerprint()
		return previous, fingerprint, false, err
	}
	if err := w.db.SetWebAccess(config); err != nil {
		if w.server != nil {
			_ = w.server.Close()
			for _, listener := range w.listeners {
				_ = listener.Close()
			}
			w.server, w.listeners, w.addr = nil, nil, ""
		}
		return previous, "", false, fmt.Errorf("save web access settings: %w", err)
	}
	fingerprint, err := w.readFingerprint()
	if err != nil {
		return config, "", false, err
	}
	return config, fingerprint, false, nil
}

func (w *webAccessRuntime) startSaved() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	config, err := w.db.WebAccess()
	if err != nil || !config.Enabled {
		return err
	}
	config, err = normalizeWebAccessConfig(config)
	if err != nil {
		return fmt.Errorf("saved web access settings: %w", err)
	}
	return w.start(config)
}

func (w *webAccessRuntime) Close() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.server != nil {
		_ = w.server.Close()
		for _, listener := range w.listeners {
			_ = listener.Close()
		}
		w.server, w.listeners, w.addr = nil, nil, ""
	}
}

func (w *webAccessRuntime) start(config nodedb.WebAccessConfig) error {
	addresses := webListenAddresses(config)
	if len(addresses) == 0 {
		return errors.New("choose at least one network interface address")
	}
	public, err := parseWebAccessURL(config.PublicURL)
	if err != nil {
		return err
	}
	certFile, keyFile, err := ensureWebAccessCertificateForAddresses(w.dataDir, addresses, public.Hostname())
	if err != nil {
		return err
	}
	if _, err := loadWebAccessKeyPair(certFile, keyFile); err != nil {
		return fmt.Errorf("load browser-access TLS certificate: %w", err)
	}
	listeners := make([]net.Listener, 0, len(addresses))
	for _, address := range addresses {
		if err := validateWebListenTLS(address, true, true); err != nil {
			for _, listener := range listeners {
				_ = listener.Close()
			}
			return err
		}
		listener, err := net.Listen("tcp", address)
		if err != nil {
			for _, opened := range listeners {
				_ = opened.Close()
			}
			return fmt.Errorf("listen for remote browser access on %s: %w", address, err)
		}
		listeners = append(listeners, listener)
	}
	local := *w.local
	local.AllowLAN = true
	local.PairingOrigin = strings.TrimRight(public.String(), "/")
	server := &http.Server{
		Handler:           api.NewWebAppHandler(local, w.origins, w.ui),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	w.server, w.listeners, w.addr = server, listeners, strings.Join(addresses, ", ")
	if w.log != nil {
		actualAddresses := make([]string, 0, len(listeners))
		for _, listener := range listeners {
			actualAddresses = append(actualAddresses, listener.Addr().String())
		}
		w.log.Info("remote browser access listening over HTTPS", "addresses", actualAddresses, "public_url", local.PairingOrigin)
	}
	for _, listener := range listeners {
		listener := listener
		go func() {
			if err := server.ServeTLS(listener, certFile, keyFile); err != nil && !errors.Is(err, http.ErrServerClosed) {
				w.stopAfterListenerFailure(server, listener, err)
			}
		}()
	}
	return nil
}

func (w *webAccessRuntime) stopAfterListenerFailure(server *http.Server, failed net.Listener, cause error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.server != server {
		return
	}
	// The selected addresses are an all-or-nothing configuration. Stop the
	// remaining listeners too, so the reported OFF state cannot leave one
	// interface unexpectedly exposed.
	_ = server.Close()
	for _, listener := range w.listeners {
		_ = listener.Close()
	}
	w.server, w.listeners, w.addr = nil, nil, ""
	if w.log != nil {
		w.log.Error("remote browser access stopped", "addr", failed.Addr().String(), "error", cause)
	}
}

func normalizeWebAccessConfig(config nodedb.WebAccessConfig) (nodedb.WebAccessConfig, error) {
	addresses := webListenAddresses(config)
	if len(addresses) == 0 {
		return config, errors.New("choose at least one network interface address")
	}
	canonical := make([]string, 0, len(addresses))
	seen := make(map[string]struct{}, len(addresses))
	portText := ""
	for _, address := range addresses {
		host, currentPort, err := net.SplitHostPort(address)
		if err != nil {
			return config, errors.New("listener addresses must be specific node IPs and ports, such as 192.168.1.20:50052")
		}
		port, err := strconv.Atoi(currentPort)
		if err != nil || port < 1 || port > 65535 {
			return config, errors.New("listener port must be between 1 and 65535")
		}
		if portText != "" && currentPort != portText {
			return config, errors.New("all selected network interfaces must use the same port")
		}
		portText = currentPort
		listenIP, err := netip.ParseAddr(strings.Trim(host, "[]"))
		if err != nil || listenIP.IsLoopback() || listenIP.IsUnspecified() || listenIP.IsMulticast() || listenIP.IsLinkLocalUnicast() {
			return config, errors.New("remote access must bind to specific non-loopback IP addresses assigned to this node")
		}
		if err := validateWebListenTLS(address, true, true); err != nil {
			return config, err
		}
		address = net.JoinHostPort(listenIP.String(), currentPort)
		if _, duplicate := seen[address]; duplicate {
			continue
		}
		seen[address] = struct{}{}
		canonical = append(canonical, address)
	}
	config.ListenAddresses = canonical
	config.ListenAddress = canonical[0]
	host, _, _ := net.SplitHostPort(config.ListenAddress)
	listenIP, _ := netip.ParseAddr(host)
	if config.PublicURL == "" {
		config.PublicURL = "https://" + net.JoinHostPort(listenIP.String(), portText)
	}
	public, err := parseWebAccessURL(config.PublicURL)
	if err != nil {
		return config, err
	}
	config.PublicURL = strings.TrimRight(public.String(), "/")
	return config, nil
}

func webListenAddresses(config nodedb.WebAccessConfig) []string {
	if len(config.ListenAddresses) > 0 {
		return slices.Clone(config.ListenAddresses)
	}
	if config.ListenAddress != "" {
		return []string{config.ListenAddress}
	}
	return nil
}

// validateWebListenTLS only permits a concrete IP currently assigned to an
// active, non-loopback interface. Unlike the development listener, the HTTPS
// listener must never bind a wildcard address.
func validateWebListenTLS(address string, requireAssigned, requireActive bool) error {
	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		return errors.New("remote access listener must be a specific IP and port, not a wildcard")
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return errors.New("remote access port must be between 1 and 65535")
	}
	parsed, err := netip.ParseAddr(strings.Trim(host, "[]"))
	if err != nil || parsed.IsLoopback() || parsed.IsUnspecified() || parsed.IsMulticast() || parsed.IsLinkLocalUnicast() {
		return errors.New("remote access must bind to a specific usable non-loopback IP address assigned to this node")
	}
	if !requireAssigned {
		return nil
	}
	interfaces, err := webAccessInterfaces()
	if err != nil {
		return fmt.Errorf("inspect local network interfaces: %w", err)
	}
	for _, network := range interfaces {
		if network.Flags&net.FlagLoopback != 0 || requireActive && (network.Flags&net.FlagUp == 0 || network.Flags&net.FlagRunning == 0) {
			continue
		}
		addresses, err := webAccessInterfaceAddresses(network)
		if err != nil {
			continue
		}
		for _, item := range addresses {
			prefix, err := netip.ParsePrefix(item.String())
			if err == nil && prefix.Addr().Unmap() == parsed.Unmap() {
				return nil
			}
		}
	}
	return errors.New("remote access address must belong to an active network interface on this node")
}

func primaryWebListenAddress(config nodedb.WebAccessConfig) (string, []string) {
	addresses := webListenAddresses(config)
	if len(addresses) > 0 {
		return addresses[0], addresses
	}
	return config.ListenAddress, nil
}

func sameWebAccessConfig(first, second nodedb.WebAccessConfig) bool {
	return first.Enabled == second.Enabled && first.PublicURL == second.PublicURL && slices.Equal(webListenAddresses(first), webListenAddresses(second))
}

func parseWebAccessURL(raw string) (*url.URL, error) {
	if raw == "" {
		return nil, errors.New("enter the HTTPS address browsers will use for this node")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Path != "" && u.Path != "/" || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("public address must be an HTTPS origin, such as https://node.example.net:50052")
	}
	if ip := net.ParseIP(u.Hostname()); ip == nil && (len(u.Hostname()) > 253 || strings.ContainsAny(u.Hostname(), " /\\")) {
		return nil, errors.New("public HTTPS address has an invalid host name")
	}
	u.Path = ""
	return u, nil
}

func ensureWebAccessCertificate(dataDir, listenAddress, publicHost string) (string, string, error) {
	return ensureWebAccessCertificateForAddresses(dataDir, []string{listenAddress}, publicHost)
}

func ensureWebAccessCertificateForAddresses(dataDir string, listenAddresses []string, publicHost string) (string, string, error) {
	listenIPs := make([]netip.Addr, 0, len(listenAddresses))
	for _, address := range listenAddresses {
		listenHost, _, err := net.SplitHostPort(address)
		if err != nil {
			return "", "", err
		}
		listenIP, err := netip.ParseAddr(strings.Trim(listenHost, "[]"))
		if err != nil {
			return "", "", errors.New("browser listener must bind to specific IP addresses on this node")
		}
		listenIPs = append(listenIPs, listenIP.Unmap())
	}
	if len(listenIPs) == 0 {
		return "", "", errors.New("browser listener needs at least one IP address")
	}
	certFile := filepath.Join(dataDir, "web-access.crt")
	keyFile := filepath.Join(dataDir, "web-access.key")
	if certificateCoversAll(certFile, listenIPs, publicHost) {
		if _, err := loadWebAccessKeyPair(certFile, keyFile); err == nil {
			return certFile, keyFile, nil
		}
	}
	key, err := generateWebAccessKey()
	if err != nil {
		return "", "", fmt.Errorf("generate browser-access key: %w", err)
	}
	serial, err := generateWebAccessSerial()
	if err != nil {
		return "", "", fmt.Errorf("generate browser-access certificate serial: %w", err)
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: publicHost},
		NotBefore:             now.Add(-5 * time.Minute),
		NotAfter:              now.Add(365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	if ip := net.ParseIP(publicHost); ip != nil {
		template.IPAddresses = append(template.IPAddresses, ip)
	} else {
		template.DNSNames = append(template.DNSNames, publicHost)
	}
	for _, listenIP := range listenIPs {
		found := false
		for _, ip := range template.IPAddresses {
			found = found || ip.Equal(net.IP(listenIP.AsSlice()))
		}
		if !found {
			template.IPAddresses = append(template.IPAddresses, net.IP(listenIP.AsSlice()))
		}
	}
	der, err := createWebAccessCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return "", "", fmt.Errorf("create browser-access certificate: %w", err)
	}
	keyDER, err := marshalWebAccessKey(key)
	if err != nil {
		return "", "", fmt.Errorf("encode browser-access key: %w", err)
	}
	if err := writePrivateFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})); err != nil {
		return "", "", fmt.Errorf("save browser-access key: %w", err)
	}
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		return "", "", fmt.Errorf("save browser-access certificate: %w", err)
	}
	return certFile, keyFile, nil
}

func writePrivateFile(path string, data []byte) error {
	file, err := openWebAccessKeyFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if err := file.Chmod(0o600); err != nil {
		file.Close()
		return err
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		return err
	}
	return file.Close()
}

func certificateCoversAll(path string, listenIPs []netip.Addr, publicHost string) bool {
	contents, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	block, _ := pem.Decode(contents)
	if block == nil {
		return false
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil || time.Until(cert.NotAfter) < 30*24*time.Hour {
		return false
	}
	if err := cert.VerifyHostname(publicHost); err != nil {
		return false
	}
	for _, listenIP := range listenIPs {
		if cert.VerifyHostname(listenIP.String()) != nil {
			return false
		}
	}
	return true
}

func webAccessFingerprint(dataDir string) (string, error) {
	contents, err := os.ReadFile(filepath.Join(dataDir, "web-access.crt"))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	block, _ := pem.Decode(contents)
	if block == nil {
		return "", errors.New("saved browser-access certificate is not PEM encoded")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return "", err
	}
	fingerprint := sha256.Sum256(cert.Raw)
	return strings.ToUpper(hex.EncodeToString(fingerprint[:])), nil
}
