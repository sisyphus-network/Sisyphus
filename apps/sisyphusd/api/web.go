package api

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math/big"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"connectrpc.com/vanguard/vanguardgrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/sisyphus-network/Sisyphus/packages/nodedb"
	nodepb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/node/v1"
)

// NewWebHandler serves the local API to web pages. A page cannot speak
// gRPC as the desktop app does, so the same calls are offered in the ways a
// browser can make them: gRPC-Web, and Connect, in which a call that
// returns one thing is a plain POST of JSON.
//
// A browser is a more dangerous caller than a program on the machine: any
// page the user visits can try the node's address. So here every call needs
// the node's token or an enrolled browser session; a page is answered only
// if it comes from one of origins; and requests must name this machine by
// loopback, or its exact private interface address when LAN access is enabled.
//
// Sending a file in pieces is the one thing a browser cannot do in either
// protocol, so a file is stored by PUT to /files/<name>, its body the
// file, with ?private=true to seal it; the answer is the File as JSON.
const webSessionCookie = "sisyphus_web_session"

type webSessions struct {
	mu     sync.Mutex
	active map[string]time.Time
}

const browserPairingLifetime = 5 * time.Minute
const pairedBrowserLifetime = 30 * 24 * time.Hour

type browserAccess struct {
	mu      sync.Mutex
	store   BrowserAccessStore
	pepper  []byte
	devices map[string]nodedb.BrowserDevice
	failed  map[string]int
	blocked map[string]time.Time
}

type volatileBrowserAccess struct {
	mu          sync.Mutex
	pairHash    []byte
	pairExpires time.Time
	devices     map[string]nodedb.BrowserDevice
}

func (s *volatileBrowserAccess) SaveBrowserPairing(hash []byte, expires time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pairHash, s.pairExpires = append([]byte(nil), hash...), expires
	return nil
}
func (s *volatileBrowserAccess) ConsumeBrowserPairing(hash []byte, now time.Time, device nodedb.BrowserDevice) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.pairExpires.After(now) || len(hash) != len(s.pairHash) || subtle.ConstantTimeCompare(hash, s.pairHash) != 1 {
		return false, nil
	}
	s.devices[device.ID] = device
	s.pairHash, s.pairExpires = nil, time.Time{}
	return true, nil
}
func (s *volatileBrowserAccess) BrowserDevices() ([]nodedb.BrowserDevice, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	devices := make([]nodedb.BrowserDevice, 0, len(s.devices))
	for _, device := range s.devices {
		devices = append(devices, device)
	}
	return devices, nil
}
func (s *volatileBrowserAccess) DeleteBrowserDevice(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.devices, id)
	return nil
}

func newBrowserAccess(store BrowserAccessStore, pepper string) *browserAccess {
	if store == nil {
		store = &volatileBrowserAccess{devices: make(map[string]nodedb.BrowserDevice)}
	}
	access := &browserAccess{store: store, pepper: []byte(pepper), devices: make(map[string]nodedb.BrowserDevice), failed: make(map[string]int), blocked: make(map[string]time.Time)}
	if devices, err := store.BrowserDevices(); err == nil {
		for _, device := range devices {
			access.devices[device.ID] = device
		}
	}
	return access
}

func (a *browserAccess) issuePairing(now time.Time) (string, time.Time, error) {
	value, err := rand.Int(rand.Reader, big.NewInt(100_000_000))
	if err != nil {
		return "", time.Time{}, err
	}
	code := fmt.Sprintf("%08d", value.Int64())
	expires := now.Add(browserPairingLifetime)
	hash := hmac.New(sha256.New, a.pepper)
	_, _ = hash.Write([]byte(code))
	if err := a.store.SaveBrowserPairing(hash.Sum(nil), expires); err != nil {
		return "", time.Time{}, err
	}
	return code, expires, nil
}

func (a *browserAccess) pair(code, name, remote string, now time.Time) (string, string, bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.failed)+len(a.blocked) > 512 {
		for address, until := range a.blocked {
			if !until.After(now) {
				delete(a.blocked, address)
			}
		}
	}
	if a.blocked[remote].After(now) {
		return "", "", false, nil
	}
	if _, tracked := a.failed[remote]; !tracked && len(a.failed)+len(a.blocked) >= 4096 {
		return "", "", false, nil
	}
	tokenBytes := make([]byte, 32)
	idBytes := make([]byte, 16)
	if _, err := rand.Read(tokenBytes); err != nil {
		return "", "", false, err
	}
	if _, err := rand.Read(idBytes); err != nil {
		return "", "", false, err
	}
	token := hex.EncodeToString(tokenBytes)
	id := hex.EncodeToString(idBytes)
	codeHash := hmac.New(sha256.New, a.pepper)
	_, _ = codeHash.Write([]byte(code))
	tokenHash := sha256.Sum256([]byte(token))
	expires := now.Add(pairedBrowserLifetime)
	device := nodedb.BrowserDevice{ID: id, Name: name, TokenHash: tokenHash[:], CreatedAt: now, ExpiresAt: expires}
	consumed, err := a.store.ConsumeBrowserPairing(codeHash.Sum(nil), now, device)
	if err != nil {
		return "", "", false, err
	}
	if !consumed {
		if a.failed[remote] < 8 {
			a.failed[remote]++
		}
		if a.failed[remote] >= 8 {
			a.blocked[remote] = now.Add(2 * time.Minute)
			delete(a.failed, remote)
		}
		return "", "", false, nil
	}
	delete(a.failed, remote)
	a.devices[id] = device
	return id, token, true, nil
}

func (a *browserAccess) valid(cookie string) bool {
	id, token, ok := strings.Cut(cookie, ".")
	if !ok || id == "" || token == "" {
		return false
	}
	tokenHash := sha256.Sum256([]byte(token))
	a.mu.Lock()
	device, exists := a.devices[id]
	a.mu.Unlock()
	return exists && device.ExpiresAt.After(time.Now()) && len(device.TokenHash) == len(tokenHash) && subtle.ConstantTimeCompare(device.TokenHash, tokenHash[:]) == 1
}

func (a *browserAccess) list() ([]nodedb.BrowserDevice, error) { return a.store.BrowserDevices() }
func (a *browserAccess) revoke(id string) error {
	if err := a.store.DeleteBrowserDevice(id); err != nil {
		return err
	}
	a.mu.Lock()
	delete(a.devices, id)
	a.mu.Unlock()
	return nil
}

func newWebSessions() *webSessions { return &webSessions{active: make(map[string]time.Time)} }

func (s *webSessions) create() (string, error) {
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	id := hex.EncodeToString(random[:])
	expires := time.Now().Add(12 * time.Hour)
	s.mu.Lock()
	defer s.mu.Unlock()
	for existing, until := range s.active {
		if !until.After(time.Now()) {
			delete(s.active, existing)
		}
	}
	s.active[id] = expires
	return id, nil
}

func (s *webSessions) valid(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	expires, ok := s.active[id]
	if !ok {
		return false
	}
	if !expires.After(time.Now()) {
		delete(s.active, id)
		return false
	}
	return true
}

func (s *webSessions) delete(id string) {
	s.mu.Lock()
	delete(s.active, id)
	s.mu.Unlock()
}

// NewWebHandler serves the local API without static UI assets.
func NewWebHandler(cfg LocalConfig, origins []string) http.Handler {
	return NewWebAppHandler(cfg, origins, nil)
}

// NewWebAppHandler serves the local API and, when ui is present, the static
// web build from the same loopback origin or explicitly enabled LAN origin.
// The UI itself is public on the listener; API calls still require a bearer
// token or an enrolled HttpOnly session cookie.
func NewWebAppHandler(cfg LocalConfig, origins []string, ui fs.FS) http.Handler {
	sessions := newWebSessions()
	devices := newBrowserAccess(cfg.BrowserAccess, cfg.Token)
	service := &localService{cfg: normal(cfg)}
	server := grpc.NewServer()
	nodepb.RegisterNodeServiceServer(server, service)
	calls, _ := vanguardgrpc.NewTranscoder(server) // fails only for a service with no description, and this one has it
	apiMux := http.NewServeMux()
	apiMux.Handle("/", calls)
	apiMux.HandleFunc("PUT /files/{name}", func(w http.ResponseWriter, r *http.Request) {
		if service.cfg.Store == nil {
			refuseWeb(w, http.StatusPreconditionFailed, status.Convert(errNoStore).Message())
			return
		}
		buf := make([]byte, 256<<10)
		file, err := service.keep(r.Context(), r.PathValue("name"), r.URL.Query().Get("private") == "true", false, func() ([]byte, error) {
			n, err := r.Body.Read(buf)
			if n > 0 {
				return buf[:n], nil
			}
			return nil, err
		})
		if err != nil {
			refuseWeb(w, http.StatusBadRequest, status.Convert(err).Message())
			return
		}
		encoded, _ := protojson.Marshal(file) // a message of strings and numbers always encodes
		w.Header().Set("Content-Type", "application/json")
		w.Write(encoded)
	})
	var appHandler http.Handler = apiMux
	if ui != nil {
		static := staticWebUI(ui)
		appHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/session" {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			if strings.HasPrefix(r.URL.Path, "/sisyphus.node.v1.NodeService/") || strings.HasPrefix(r.URL.Path, "/files/") {
				apiMux.ServeHTTP(w, r)
				return
			}
			static.ServeHTTP(w, r)
		})
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !webHostAllowed(r.Host, cfg.AllowLAN) {
			refuseWeb(w, http.StatusForbidden, "this is a node's local API, and answers only to its own machine's address")
			return
		}
		origin := r.Header.Get("Origin")
		if origin != "" {
			if !sameOrigin(r, origin) && !slices.Contains(origins, origin) {
				refuseWeb(w, http.StatusForbidden, "pages from "+origin+" may not use this node: its owner has not listed that origin with --web-origin")
				return
			}
			if !sameOrigin(r, origin) {
				h := w.Header()
				h.Set("Access-Control-Allow-Origin", origin)
				h.Add("Vary", "Origin")
				h.Set("Access-Control-Allow-Credentials", "true")
				h.Set("Access-Control-Expose-Headers", "Grpc-Status, Grpc-Message, Grpc-Status-Details-Bin, Connect-Content-Encoding")
				if r.Method == http.MethodOptions {
					// A browser asking leave before it asks in earnest.
					h.Set("Access-Control-Allow-Methods", "POST, PUT, GET, OPTIONS, DELETE")
					h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Connect-Protocol-Version, Connect-Timeout-Ms, Grpc-Timeout, X-Grpc-Web, X-User-Agent")
					h.Set("Access-Control-Max-Age", "600")
					w.WriteHeader(http.StatusNoContent)
					return
				}
			}
		} else if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if ui != nil && r.URL.Path == "/session" {
			switch r.Method {
			case http.MethodPost:
				if !validBearer(r, cfg.Token) {
					refuseWeb(w, http.StatusUnauthorized, "enter the node's API token to connect this browser")
					return
				}
				sessionID, err := sessions.create()
				if err != nil {
					http.Error(w, "could not create a browser session", http.StatusInternalServerError)
					return
				}
				cookie := &http.Cookie{Name: webSessionCookie, Value: sessionID, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: int((12 * time.Hour).Seconds()), Expires: time.Now().Add(12 * time.Hour), Secure: r.TLS != nil}
				http.SetCookie(w, cookie)
				w.Header().Set("Cache-Control", "no-store")
				w.WriteHeader(http.StatusNoContent)
				return
			case http.MethodDelete:
				if cookie, err := r.Cookie(webSessionCookie); err == nil {
					sessions.delete(cookie.Value)
				}
				http.SetCookie(w, &http.Cookie{Name: webSessionCookie, Value: "", Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: -1, Expires: time.Unix(1, 0), Secure: r.TLS != nil})
				w.Header().Set("Cache-Control", "no-store")
				w.WriteHeader(http.StatusNoContent)
				return
			default:
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
		}
		if r.URL.Path == "/pair" || r.URL.Path == "/pairing" || r.URL.Path == "/devices" || strings.HasPrefix(r.URL.Path, "/devices/") {
			webControl(w, r, cfg, sessions, devices)
			return
		}
		if ui != nil && !strings.HasPrefix(r.URL.Path, "/sisyphus.node.v1.NodeService/") && !strings.HasPrefix(r.URL.Path, "/files/") {
			appHandler.ServeHTTP(w, r)
			return
		}
		bearerAuthenticated := validBearer(r, cfg.Token)
		sessionAuthenticated := validSession(r, sessions) || validPairedSession(r, devices)
		if !bearerAuthenticated && !sessionAuthenticated {
			refuseWeb(w, http.StatusUnauthorized, "every call from a web page needs the node's API token, which is in api.token in its data directory")
			return
		}
		// Browser cookies are attached automatically. Require an exact same-origin
		// Origin (or Referer fallback) for cookie-authenticated mutations so a
		// hostile page cannot use a user's enrolled session to change node state.
		if sessionAuthenticated && !bearerAuthenticated && mutatesState(r) && !sameBrowserOrigin(r) {
			refuseWeb(w, http.StatusForbidden, "cookie-authenticated changes must come from this node's own web page")
			return
		}
		if sessionAuthenticated && !bearerAuthenticated {
			// The outer HTTP layer has authenticated this HttpOnly session. The
			// local gRPC service still enforces the node token for mutations, so
			// bridge that authorization internally without returning the token to
			// browser JavaScript.
			r = r.Clone(r.Context())
			r.Header = r.Header.Clone()
			r.Header.Set("Authorization", "Bearer "+cfg.Token)
		}
		appHandler.ServeHTTP(w, r)
	})
}

func mutatesState(r *http.Request) bool {
	if r.Method == http.MethodPut || r.Method == http.MethodPatch || r.Method == http.MethodDelete {
		return true
	}
	return r.Method == http.MethodPost && (strings.HasPrefix(r.URL.Path, "/files/") || strings.HasPrefix(r.URL.Path, "/sisyphus.node.v1.NodeService/"))
}

func sameBrowserOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin != "" {
		return sameOrigin(r, origin)
	}
	referer := r.Header.Get("Referer")
	if referer == "" {
		return false
	}
	u, err := url.Parse(referer)
	if err != nil {
		return false
	}
	return sameOrigin(r, u.Scheme+"://"+u.Host)
}

func staticWebUI(ui fs.FS) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
		requestedName := name
		if name == "." || name == "" {
			name = "index.html"
		}
		body, err := fs.ReadFile(ui, name)
		if errors.Is(err, fs.ErrNotExist) && !strings.Contains(path.Base(name), ".") {
			name = "index.html"
			body, err = fs.ReadFile(ui, name)
			if err == nil && requestedName != "." && requestedName != "" {
				body = bytes.Replace(body, []byte("<head>"), []byte("<head><base href=\"/\">"), 1)
			}
		}
		if err != nil {
			http.NotFound(w, r)
			return
		}
		if contentType := mime.TypeByExtension(path.Ext(name)); contentType != "" {
			w.Header().Set("Content-Type", contentType)
		}
		if strings.HasPrefix(name, "assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		if r.Method == http.MethodGet {
			_, _ = w.Write(body)
		}
	})
}

func validBearer(r *http.Request, token string) bool {
	if token == "" {
		return false
	}
	shown := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	return subtle.ConstantTimeCompare([]byte(shown), []byte(token)) == 1
}

func validSession(r *http.Request, sessions *webSessions) bool {
	cookie, err := r.Cookie(webSessionCookie)
	return err == nil && sessions.valid(cookie.Value)
}

func validPairedSession(r *http.Request, devices *browserAccess) bool {
	cookie, err := r.Cookie(webSessionCookie)
	return err == nil && devices.valid(cookie.Value)
}

func webControl(w http.ResponseWriter, r *http.Request, cfg LocalConfig, sessions *webSessions, devices *browserAccess) {
	if r.URL.Path == "/pair" {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if !sameBrowserOrigin(r) {
			refuseWeb(w, http.StatusForbidden, "pairing must be completed from this node's own web page")
			return
		}
		var request struct {
			Code string `json:"code"`
			Name string `json:"name"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&request); err != nil || len(request.Code) != 8 || strings.Trim(request.Code, "0123456789") != "" {
			refuseWeb(w, http.StatusBadRequest, "enter the eight-digit pairing code")
			return
		}
		request.Name = strings.TrimSpace(request.Name)
		if request.Name == "" {
			request.Name = "Browser"
		}
		if len(request.Name) > 80 {
			refuseWeb(w, http.StatusBadRequest, "device name is too long")
			return
		}
		remote, _, _ := net.SplitHostPort(r.RemoteAddr)
		id, token, paired, err := devices.pair(request.Code, request.Name, remote, time.Now())
		if err != nil {
			http.Error(w, "could not complete pairing", http.StatusInternalServerError)
			return
		}
		if !paired {
			refuseWeb(w, http.StatusUnauthorized, "pairing code is invalid, expired, or temporarily locked")
			return
		}
		value := id + "." + token
		expires := time.Now().Add(pairedBrowserLifetime)
		http.SetCookie(w, &http.Cookie{Name: webSessionCookie, Value: value, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: int(pairedBrowserLifetime.Seconds()), Expires: expires, Secure: r.TLS != nil})
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusNoContent)
		return
	}

	bearer := validBearer(r, cfg.Token)
	cookieSession := validSession(r, sessions) || validPairedSession(r, devices)
	if !bearer && !cookieSession {
		refuseWeb(w, http.StatusUnauthorized, "connect this browser before managing paired devices")
		return
	}
	if cookieSession && !bearer && !sameBrowserOrigin(r) {
		refuseWeb(w, http.StatusForbidden, "cookie-authenticated browser management must come from this node's own web page")
		return
	}

	switch {
	case r.URL.Path == "/pairing" && r.Method == http.MethodPost:
		if origin := r.Header.Get("Origin"); origin != "" && !sameOrigin(r, origin) {
			refuseWeb(w, http.StatusForbidden, "browser management is available only from this node's own web page")
			return
		}
		code, expires, err := devices.issuePairing(time.Now())
		if err != nil {
			http.Error(w, "could not create a pairing code", http.StatusInternalServerError)
			return
		}
		scheme := "http"
		if r.TLS != nil {
			scheme = "https"
		}
		pairingOrigin := cfg.PairingOrigin
		if pairingOrigin == "" {
			pairingOrigin = scheme + "://" + r.Host
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"code": code, "expiresAt": expires.UTC(), "pairUrl": strings.TrimRight(pairingOrigin, "/") + "/#pair=" + code})
	case r.URL.Path == "/devices" && r.Method == http.MethodGet:
		if origin := r.Header.Get("Origin"); origin != "" && !sameOrigin(r, origin) {
			refuseWeb(w, http.StatusForbidden, "browser management is available only from this node's own web page")
			return
		}
		stored, err := devices.list()
		if err != nil {
			http.Error(w, "could not list paired devices", http.StatusInternalServerError)
			return
		}
		public := make([]map[string]any, 0, len(stored))
		for _, device := range stored {
			public = append(public, map[string]any{"id": device.ID, "name": device.Name, "createdAt": device.CreatedAt.UTC()})
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"devices": public})
	case strings.HasPrefix(r.URL.Path, "/devices/") && r.Method == http.MethodDelete:
		if origin := r.Header.Get("Origin"); origin != "" && !sameOrigin(r, origin) {
			refuseWeb(w, http.StatusForbidden, "browser management is available only from this node's own web page")
			return
		}
		id := strings.TrimPrefix(r.URL.Path, "/devices/")
		if id == "" || strings.Contains(id, "/") {
			http.NotFound(w, r)
			return
		}
		if err := devices.revoke(id); err != nil {
			http.Error(w, "could not revoke paired device", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func webHostAllowed(host string, allowLAN bool) bool {
	if thisMachine(host) {
		return true
	}
	if !allowLAN {
		return false
	}
	name, _, err := net.SplitHostPort(host)
	if err != nil {
		name = strings.Trim(host, "[]")
	}
	address, err := netip.ParseAddr(name)
	if err != nil || !address.IsPrivate() || address.IsLoopback() || address.IsUnspecified() || address.IsMulticast() {
		return false
	}
	interfaces, err := net.Interfaces()
	if err != nil {
		return false
	}
	for _, network := range interfaces {
		addresses, err := network.Addrs()
		if err != nil {
			continue
		}
		for _, item := range addresses {
			prefix, err := netip.ParsePrefix(item.String())
			if err == nil && prefix.Addr().Unmap() == address.Unmap() {
				return true
			}
		}
	}
	return false
}

func sameOrigin(r *http.Request, origin string) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	wantScheme := "http"
	if r.TLS != nil {
		wantScheme = "https"
	}
	return strings.EqualFold(u.Scheme, wantScheme) && strings.EqualFold(u.Host, r.Host)
}

// thisMachine reports whether a request's Host names the machine itself:
// a loopback address or localhost, with or without a port.
func thisMachine(host string) bool {
	if name, _, err := net.SplitHostPort(host); err == nil {
		host = name
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

// refuseWeb answers a request that is not let through, in a form a page
// can read.
func refuseWeb(w http.ResponseWriter, code int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"code": http.StatusText(code), "message": message}) // strings always encode
}
