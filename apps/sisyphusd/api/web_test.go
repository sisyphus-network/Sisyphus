package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/sisyphus-network/Sisyphus/packages/nodedb"
	"github.com/sisyphus-network/Sisyphus/packages/storage"
)

func TestRevokingPairedBrowserCancelsActiveRequests(t *testing.T) {
	const deviceID, token = "browser-id", "browser-secret"
	store := &volatileBrowserAccess{devices: make(map[string]nodedb.BrowserDevice)}
	hash := sha256.Sum256([]byte(token))
	store.devices[deviceID] = nodedb.BrowserDevice{ID: deviceID, Name: "Owner phone", TokenHash: hash[:], ExpiresAt: time.Now().Add(time.Hour)}
	devices := newBrowserAccess(store, "")
	ctx, finish, ok := devices.authorizeRequest(deviceID+"."+token, context.Background())
	if !ok {
		t.Fatal("paired browser request was not authorized")
	}
	actor, ok := actionApprovalActorFromContext(ctx)
	if !ok || !actor.verified || actor.kind != "paired_browser" || actor.id != deviceID || actor.name != "Owner phone" {
		t.Fatalf("paired browser approval actor = %#v, %t", actor, ok)
	}
	defer finish()
	if err := devices.revoke(deviceID); err != nil {
		t.Fatalf("revoke paired browser: %v", err)
	}
	select {
	case <-ctx.Done():
	case <-time.After(eventualWait):
		t.Fatal("revoking the browser did not cancel its active request immediately")
	}
	if _, _, ok := devices.authorizeRequest(deviceID+"."+token, context.Background()); ok {
		t.Fatal("revoked browser authorized a new request")
	}
}

type failingBrowserAccessStore struct {
	devices                                 []nodedb.BrowserDevice
	err                                     error
	saveErr, consumeErr, listErr, deleteErr error
	consume                                 bool
}

func (s *failingBrowserAccessStore) SaveBrowserPairing([]byte, time.Time) error {
	if s.saveErr != nil {
		return s.saveErr
	}
	return s.err
}
func (s *failingBrowserAccessStore) ConsumeBrowserPairing([]byte, time.Time, nodedb.BrowserDevice) (bool, error) {
	if s.consumeErr != nil {
		return false, s.consumeErr
	}
	return s.consume, s.err
}
func (s *failingBrowserAccessStore) BrowserDevices() ([]nodedb.BrowserDevice, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	return s.devices, s.err
}
func (s *failingBrowserAccessStore) DeleteBrowserDevice(string) error {
	if s.deleteErr != nil {
		return s.deleteErr
	}
	return s.err
}

type errorReader struct{}

func (errorReader) Read([]byte) (int, error) { return 0, errors.New("random source unavailable") }

type partialErrorReader struct{ remaining int }

func (r *partialErrorReader) Read(p []byte) (int, error) {
	if r.remaining == 0 {
		return 0, errors.New("random source ended")
	}
	n := min(len(p), r.remaining)
	clear(p[:n])
	r.remaining -= n
	return n, nil
}

type testNetworkAddress string

func (testNetworkAddress) Network() string  { return "test" }
func (a testNetworkAddress) String() string { return string(a) }

// page is a web page's request of a node: how it is made and what from.
type page struct {
	method, path, body string
	origin, host, key  string
	kind               string
}

// asWeb serves the local API to web pages and returns how to ask it.
func asWeb(t *testing.T, cfg LocalConfig, origins ...string) func(page) (int, string, http.Header) {
	t.Helper()
	cfg.NodeID, cfg.Token = "12D3KooWnode", "the-token"
	cfg.Listen = func() []string { return []string{"/ip4/127.0.0.1/tcp/7700"} }
	server := httptest.NewServer(NewWebHandler(cfg, origins))
	t.Cleanup(server.Close)
	return func(p page) (int, string, http.Header) {
		t.Helper()
		if p.method == "" {
			p.method = http.MethodPost
		}
		req, err := http.NewRequest(p.method, server.URL+p.path, strings.NewReader(p.body))
		if err != nil {
			t.Fatal(err)
		}
		if p.kind == "" {
			p.kind = "application/json"
		}
		req.Header.Set("Content-Type", p.kind)
		req.Header.Set("Connect-Protocol-Version", "1")
		if p.key != "" {
			req.Header.Set("Authorization", "Bearer "+p.key)
		}
		if p.origin != "" {
			req.Header.Set("Origin", p.origin)
		}
		if p.host != "" {
			req.Host = p.host
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		said, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(said), res.Header
	}
}

const nodeInfo = "/sisyphus.node.v1.NodeService/GetNodeInfo"

func TestAWebPageCallsTheNodeAsABrowserCan(t *testing.T) {
	ask := asWeb(t, LocalConfig{Store: storage.NewMemory(), Files: newFileList(t)}, "http://localhost:5173")

	// A call that returns one thing is a POST of JSON.
	status, body, header := ask(page{path: nodeInfo, body: `{}`, key: "the-token", origin: "http://localhost:5173"})
	if status != http.StatusOK || !strings.Contains(body, `"peerId":"12D3KooWnode"`) || header.Get("Access-Control-Allow-Origin") != "http://localhost:5173" {
		t.Fatalf("GetNodeInfo: %d %s %v", status, body, header)
	}
	// What the node refuses comes back as the node said it.
	if status, body, _ := ask(page{path: "/sisyphus.node.v1.NodeService/RemoveFile", body: `{"cid":"not-a-cid"}`, key: "the-token"}); status == http.StatusOK || !strings.Contains(body, `"code"`) {
		t.Errorf("a call the node refuses: %d %s", status, body)
	}
	// A call that returns many things returns them one after another,
	// each with its length before it, and then says how it ended.
	stored, body, _ := ask(page{method: http.MethodPut, path: "/files/notes.txt", body: "to be or not to be", key: "the-token", kind: "text/plain"})
	var file struct{ Cid, Name, SizeBytes string }
	if json.Unmarshal([]byte(body), &file); stored != http.StatusOK || file.Name != "notes.txt" || file.SizeBytes != "18" || file.Cid == "" {
		t.Fatalf("storing a file: %d %s", stored, body)
	}
	request, _ := json.Marshal(map[string]string{"cid": file.Cid})
	framed := append([]byte{0, 0, 0, 0, byte(len(request))}, request...)
	status, body, _ = ask(page{path: "/sisyphus.node.v1.NodeService/FetchFile", body: string(framed), key: "the-token", kind: "application/connect+json"})
	var fetched bytes.Buffer
	for raw := []byte(body); len(raw) >= 5; {
		flags, size := raw[0], binary.BigEndian.Uint32(raw[1:5])
		var piece struct{ Data []byte }
		if json.Unmarshal(raw[5:5+size], &piece); flags == 0 {
			fetched.Write(piece.Data)
		}
		raw = raw[5+size:]
	}
	if status != http.StatusOK || fetched.String() != "to be or not to be" {
		t.Errorf("fetching it back as a stream: %d %q", status, fetched.String())
	}
	// And in gRPC-Web, which is what older pages speak.
	if status, _, header := ask(page{path: nodeInfo, body: "\x00\x00\x00\x00\x00", key: "the-token", kind: "application/grpc-web+proto"}); status != http.StatusOK || !strings.HasPrefix(header.Get("Content-Type"), "application/grpc-web") {
		t.Errorf("gRPC-Web: %d %v", status, header)
	}

	// A file may be sealed, if the node has a key to seal with.
	if status, body, _ := ask(page{method: http.MethodPut, path: "/files/secret.txt?private=true", body: "x", key: "the-token"}); status != http.StatusBadRequest || !strings.Contains(body, "key") {
		t.Errorf("a private file on a node with no key: %d %s", status, body)
	}
}

func TestAWebPageIsHeldToTheTokenItsOriginAndThisMachine(t *testing.T) {
	ask := asWeb(t, LocalConfig{}, "http://localhost:5173", "https://app.example")
	for name, tt := range map[string]struct {
		page   page
		status int
		want   string
	}{
		"no token, even to read":                      {page{path: nodeInfo, body: `{}`}, http.StatusUnauthorized, "needs the node's API token"},
		"the wrong token":                             {page{path: nodeInfo, body: `{}`, key: "another"}, http.StatusUnauthorized, "needs the node's API token"},
		"a page from elsewhere":                       {page{path: nodeInfo, body: `{}`, key: "the-token", origin: "https://evil.example"}, http.StatusForbidden, "has not listed that origin"},
		"a name for this machine that is not its own": {page{path: nodeInfo, body: `{}`, key: "the-token", host: "evil.example:50052"}, http.StatusForbidden, "only to its own machine's address"},
		"a file with no store to put it in":           {page{method: http.MethodPut, path: "/files/a.txt", body: "x", key: "the-token"}, http.StatusPreconditionFailed, "no store"},
	} {
		if status, body, _ := ask(tt.page); status != tt.status || !strings.Contains(body, tt.want) {
			t.Errorf("%s: %d %s, want %d and %q", name, status, body, tt.status, tt.want)
		}
	}
	// A browser asks leave first, without the token, and is told what it may send.
	status, _, header := ask(page{method: http.MethodOptions, path: nodeInfo, origin: "https://app.example"})
	if status != http.StatusNoContent || header.Get("Access-Control-Allow-Origin") != "https://app.example" || !strings.Contains(header.Get("Access-Control-Allow-Headers"), "Authorization") {
		t.Errorf("asking leave: %d %v", status, header)
	}
	// This machine goes by several names, and a request with no origin is
	// not from a page at all.
	for host, mine := range map[string]bool{"localhost:50052": true, "127.0.0.1": true, "[::1]:50052": true, "192.168.1.5:50052": false, "localhost.evil.example": false} {
		if thisMachine(host) != mine {
			t.Errorf("thisMachine(%q) = %v", host, !mine)
		}
	}
	if status, _, _ := ask(page{path: nodeInfo, body: `{}`, key: "the-token", host: "localhost:50052"}); status != http.StatusOK {
		t.Errorf("asked for as localhost: %d", status)
	}
}

func TestAnEmptyConfiguredTokenNeverAuthenticatesAnEmptyBearer(t *testing.T) {
	server := httptest.NewServer(NewWebHandler(LocalConfig{Listen: func() []string { return nil }}, nil))
	defer server.Close()
	request, err := http.NewRequest(http.MethodPost, server.URL+nodeInfo, strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Connect-Protocol-Version", "1")
	request.Header.Set("Authorization", "Bearer ")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("empty token authenticated: %d", response.StatusCode)
	}
}

func TestNodeServesTheWebAppAndUsesAnHttpOnlySession(t *testing.T) {
	ui := fstest.MapFS{
		"index.html":        &fstest.MapFile{Data: []byte("<main>Sisyphus UI</main>")},
		"assets/app.js":     &fstest.MapFile{Data: []byte("console.log('app')")},
		"assets/app.css":    &fstest.MapFile{Data: []byte("body { color: black }")},
		"logo.png":          &fstest.MapFile{Data: []byte("logo")},
		"not-a-page.js":     &fstest.MapFile{Data: []byte("not found")},
		"nested/index.html": &fstest.MapFile{Data: []byte("nested")},
	}
	cfg := LocalConfig{
		NodeID: "12D3KooWnode", Token: "the-token",
		Listen: func() []string { return []string{"/ip4/127.0.0.1/tcp/7700"} },
		Book:   &book{}, Bootstrap: func(context.Context, []string) {},
	}
	server := httptest.NewServer(NewWebAppHandler(cfg, nil, fs.FS(ui)))
	defer server.Close()

	request := func(method, path, origin, bearer string, cookie *http.Cookie) *http.Response {
		t.Helper()
		req, err := http.NewRequest(method, server.URL+path, strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Connect-Protocol-Version", "1")
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		if cookie != nil {
			req.AddCookie(cookie)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	readBody := func(res *http.Response) string {
		t.Helper()
		defer res.Body.Close()
		body, err := io.ReadAll(res.Body)
		if err != nil {
			t.Fatal(err)
		}
		return string(body)
	}

	if res := request(http.MethodGet, "/", "", "", nil); res.StatusCode != http.StatusOK || !strings.Contains(readBody(res), "Sisyphus UI") {
		t.Fatalf("serve index without exposing API: %d", res.StatusCode)
	}
	if res := request(http.MethodGet, "/operations", "", "", nil); res.StatusCode != http.StatusOK || !strings.Contains(readBody(res), "Sisyphus UI") {
		t.Fatalf("serve SPA route: %d", res.StatusCode)
	}
	if res := request(http.MethodGet, "/assets/app.js", "", "", nil); res.StatusCode != http.StatusOK || res.Header.Get("Content-Type") != "text/javascript; charset=utf-8" {
		readBody(res)
		t.Fatalf("serve asset: %d %v", res.StatusCode, res.Header)
	}
	if res := request(http.MethodGet, "/missing.js", "", "", nil); res.StatusCode != http.StatusNotFound {
		readBody(res)
		t.Fatalf("missing asset must not fall back to index: %d", res.StatusCode)
	}
	if res := request(http.MethodPost, "/session", "https://attacker.example", "the-token", nil); res.StatusCode != http.StatusForbidden {
		readBody(res)
		t.Fatalf("reject foreign session origin: %d", res.StatusCode)
	}
	res := request(http.MethodPost, "/session", server.URL, "the-token", nil)
	if res.StatusCode != http.StatusNoContent {
		body := readBody(res)
		t.Fatalf("enroll session: %d %s", res.StatusCode, body)
	}
	var session *http.Cookie
	for _, cookie := range res.Cookies() {
		if cookie.Name == webSessionCookie {
			session = cookie
		}
	}
	res.Body.Close()
	if session == nil || !session.HttpOnly || session.SameSite != http.SameSiteStrictMode || session.Value == "the-token" {
		t.Fatalf("session cookie must be HttpOnly and SameSite=Strict: %#v", session)
	}
	res = request(http.MethodPost, nodeInfo, server.URL, "", session)
	body := readBody(res)
	if res.StatusCode != http.StatusOK || !strings.Contains(body, `"peerId":"12D3KooWnode"`) {
		t.Fatalf("call API with enrolled cookie: %d %s", res.StatusCode, body)
	}
	// State-changing cookie-authenticated calls must prove they came from the
	// node's own page. This protects the session from cross-site form/fetch use.
	res = request(http.MethodPost, nodeInfo, "https://attacker.example", "", session)
	if res.StatusCode != http.StatusForbidden {
		readBody(res)
		t.Fatalf("reject cross-site cookie-authenticated mutation: %d", res.StatusCode)
	}
	res = request(http.MethodPost, nodeInfo, "", "", session)
	if res.StatusCode != http.StatusForbidden {
		readBody(res)
		t.Fatalf("reject cookie-authenticated mutation without Origin: %d", res.StatusCode)
	}
	res = request(http.MethodPost, nodeInfo, server.URL, "", session)
	if res.StatusCode != http.StatusOK {
		body := readBody(res)
		t.Fatalf("allow same-origin cookie-authenticated call: %d %s", res.StatusCode, body)
	}
	res.Body.Close()
	res = request(http.MethodPost, "/sisyphus.node.v1.NodeService/SetBootstrapPeers", server.URL, "", session)
	body = readBody(res)
	if res.StatusCode != http.StatusOK || !strings.Contains(body, `"peers":[]`) {
		t.Fatalf("allow an authenticated state change through the session: %d %s", res.StatusCode, body)
	}
	res = request(http.MethodDelete, "/session", server.URL, "", session)
	if res.StatusCode != http.StatusNoContent {
		readBody(res)
		t.Fatalf("end session: %d", res.StatusCode)
	}
	if len(res.Cookies()) == 0 || res.Cookies()[0].MaxAge >= 0 {
		res.Body.Close()
		t.Fatalf("session cookie was not cleared: %v", res.Cookies())
	}
	res.Body.Close()
	if res = request(http.MethodPost, nodeInfo, server.URL, "", nil); res.StatusCode != http.StatusUnauthorized {
		readBody(res)
		t.Fatalf("logged-out API call must fail: %d", res.StatusCode)
	}
}

// The desktop recognises this node's own browser API, before it sends the
// node's token there, by the exact answer an unauthenticated request for
// /devices gets (apps/sisyphus/src/main/index.ts, requestBrowserManagement).
// Changing this answer stops the desktop managing paired devices, so it is
// held fixed here.
func TestAnUnauthenticatedDevicesRequestIdentifiesTheNode(t *testing.T) {
	cfg := LocalConfig{NodeID: "12D3KooWnode", Token: "owner-token", Listen: func() []string { return nil }}
	server := httptest.NewServer(NewWebAppHandler(cfg, nil, fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("UI")}}))
	t.Cleanup(server.Close)
	res, err := http.Get(server.URL + "/devices")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var answer struct{ Code, Message string }
	if err := json.NewDecoder(res.Body).Decode(&answer); err != nil {
		t.Fatal(err)
	}
	want := struct{ Code, Message string }{"Unauthorized", "connect this browser before managing paired devices"}
	if res.StatusCode != http.StatusUnauthorized || answer != want {
		t.Fatalf("unauthenticated /devices = %d %+v, want 401 %+v", res.StatusCode, answer, want)
	}
}

func TestOneTimePairingCreatesARevocableBrowserSession(t *testing.T) {
	db, err := nodedb.Open(filepath.Join(t.TempDir(), "node.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	changes := NewNodeInfoChanges()
	deviceChanges, unsubscribe := changes.subscribe()
	defer unsubscribe()
	cfg := LocalConfig{
		NodeID: "12D3KooWnode", Token: "owner-token", Listen: func() []string { return nil },
		BrowserAccess: db, LocationDiscovery: db, GatewayDiscoverySupported: true,
		BrowserDeviceChanges: changes, PoolLimits: db, RunsCoordinator: true, OwnerLocation: db,
	}
	ui := fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("UI")}}
	server := httptest.NewServer(NewWebAppHandler(cfg, nil, ui))
	t.Cleanup(func() { server.Close() })
	request := func(method, route, body string, cookie *http.Cookie, owner bool) *http.Response {
		t.Helper()
		req, err := http.NewRequest(method, server.URL+route, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Origin", server.URL)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Connect-Protocol-Version", "1")
		if owner {
			req.Header.Set("Authorization", "Bearer owner-token")
		}
		if cookie != nil {
			req.AddCookie(cookie)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	res := request(http.MethodPost, "/pairing", "{}", nil, true)
	var invitation struct {
		Code    string `json:"code"`
		PairURL string `json:"pairUrl"`
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusOK || json.Unmarshal(body, &invitation) != nil || len(invitation.Code) != 8 || !strings.Contains(invitation.PairURL, "#pair="+invitation.Code) {
		t.Fatalf("create pairing invitation: %d %s", res.StatusCode, body)
	}
	if denied := request(http.MethodPost, "/pair", `{"code":"00000000","name":"Phone"}`, nil, false); denied.StatusCode != http.StatusUnauthorized {
		denied.Body.Close()
		t.Fatalf("wrong one-time code status: %d", denied.StatusCode)
	} else {
		denied.Body.Close()
	}
	res = request(http.MethodPost, "/pair", `{"code":"`+invitation.Code+`","name":"Phone"}`, nil, false)
	if res.StatusCode != http.StatusNoContent {
		body, _ = io.ReadAll(res.Body)
		res.Body.Close()
		t.Fatalf("complete pairing: %d %s", res.StatusCode, body)
	}
	select {
	case <-deviceChanges:
	case <-time.After(eventualWait):
		t.Fatal("pairing did not notify paired-device subscribers")
	}
	var cookie *http.Cookie
	for _, item := range res.Cookies() {
		if item.Name == webSessionCookie {
			cookie = item
		}
	}
	res.Body.Close()
	if cookie == nil || !cookie.HttpOnly || cookie.MaxAge < int((24*time.Hour).Seconds()) {
		t.Fatalf("paired device needs a durable HttpOnly cookie: %#v", cookie)
	}
	server.Close()
	server = httptest.NewServer(NewWebAppHandler(cfg, nil, ui)) // daemon restart: device credential is recovered from SQLite
	res = request(http.MethodGet, "/devices", "", cookie, false)
	body, _ = io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("paired cookie did not authorize device listing: %d %s", res.StatusCode, body)
	}
	res = request(http.MethodPost, nodeInfo, `{}`, cookie, false)
	body, _ = io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("paired browser API call: %d %s", res.StatusCode, body)
	}
	res = request(http.MethodPost, "/sisyphus.node.v1.NodeService/ResolveActionConfirmation", `{"id":"missing","approved":true}`, cookie, false)
	body, _ = io.ReadAll(res.Body)
	res.Body.Close()
	if !strings.Contains(string(body), "pending action no longer exists") {
		t.Fatalf("paired browser did not reach the authenticated approval resolver: %d %s", res.StatusCode, body)
	}
	res = request(http.MethodPost, "/sisyphus.node.v1.NodeService/GetLocationDiscoverySettings", `{}`, cookie, false)
	body, _ = io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusOK || !strings.Contains(string(body), `"configured":false`) {
		t.Fatalf("paired browser could not read discovery settings: %d %s", res.StatusCode, body)
	}
	res = request(http.MethodPost, "/sisyphus.node.v1.NodeService/SetLocationDiscoverySettings", `{"settings":{"locateCountry":true,"stunServer":"stun.example.org:3478"}}`, cookie, false)
	body, _ = io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusOK || !strings.Contains(string(body), `"locateCountry":true`) {
		t.Fatalf("paired browser could not save discovery settings: %d %s", res.StatusCode, body)
	}
	settings, configured, err := db.LocationDiscovery()
	if err != nil || !configured || settings != (nodedb.LocationDiscovery{LocateCountry: true, StunServer: "stun.example.org:3478"}) {
		t.Fatalf("paired browser discovery settings = %#v, configured %v, error %v", settings, configured, err)
	}
	res = request(http.MethodPost, "/sisyphus.node.v1.NodeService/SetNodeLocation", `{"location":{"latitude":51.5,"longitude":-0.12,"share":"LOCATION_SHARE_CITY"}}`, cookie, false)
	body, _ = io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusOK || !strings.Contains(string(body), `"share":"LOCATION_SHARE_CITY"`) {
		t.Fatalf("paired browser could not set the node's location: %d %s", res.StatusCode, body)
	}
	for _, route := range []string{"GetNodeLocation", "GetNodeInfo"} {
		res = request(http.MethodPost, "/sisyphus.node.v1.NodeService/"+route, `{}`, cookie, false)
		body, _ = io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode != http.StatusOK || !strings.Contains(string(body), `"latitude":51.5`) {
			t.Fatalf("paired browser could not read the node's location with %s: %d %s", route, res.StatusCode, body)
		}
	}
	if location, set, err := db.OwnerLocation(); err != nil || !set || location != (nodedb.OwnerLocation{Latitude: 51.5, Longitude: -0.12, Share: "city"}) {
		t.Fatalf("paired browser location = %#v, set %v, error %v", location, set, err)
	}
	res = request(http.MethodPost, "/sisyphus.node.v1.NodeService/GetPoolLimits", `{}`, cookie, false)
	body, _ = io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusOK || !strings.Contains(string(body), `"coordinator":true`) || strings.Contains(string(body), `"configured":true`) {
		t.Fatalf("paired browser could not read pool limits: %d %s", res.StatusCode, body)
	}
	res = request(http.MethodPost, "/sisyphus.node.v1.NodeService/SetPoolLimits", `{"limits":{"maxJobsPerClient":3,"maxTaskTimeoutSeconds":"600","maxStoreBytesPerClient":"1048576"}}`, cookie, false)
	body, _ = io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusOK || !strings.Contains(string(body), `"maxJobsPerClient":3`) || !strings.Contains(string(body), `"restartNeeded":true`) {
		t.Fatalf("paired browser could not save pool limits: %d %s", res.StatusCode, body)
	}
	limits, configured, err := db.PoolLimits()
	if err != nil || !configured || limits != (nodedb.PoolLimits{MaxJobsPerClient: 3, MaxTaskTimeout: 10 * time.Minute, MaxStoreBytesPerClient: 1 << 20}) {
		t.Fatalf("paired browser pool limits = %#v, configured %v, error %v", limits, configured, err)
	}
	// A client of a separately managed node, such as Electron given the
	// node's endpoint and API token, reads and saves them with the token;
	// without a session or the token it may do neither.
	for _, route := range []string{"GetPoolLimits", "SetPoolLimits"} {
		res = request(http.MethodPost, "/sisyphus.node.v1.NodeService/"+route, `{"limits":{"maxJobsPerClient":2}}`, nil, false)
		body, _ = io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode == http.StatusOK {
			t.Fatalf("%s without a session or token: %d %s", route, res.StatusCode, body)
		}
	}
	res = request(http.MethodPost, "/sisyphus.node.v1.NodeService/SetPoolLimits", `{"limits":{"maxTasksPerClient":8,"maxUploadBytesPerAttempt":"4096"}}`, nil, true)
	body, _ = io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusOK || !strings.Contains(string(body), `"maxTasksPerClient":8`) {
		t.Fatalf("token-authorized client could not save pool limits: %d %s", res.StatusCode, body)
	}
	res = request(http.MethodPost, "/sisyphus.node.v1.NodeService/GetPoolLimits", `{}`, nil, true)
	body, _ = io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusOK || !strings.Contains(string(body), `"maxUploadBytesPerAttempt":"4096"`) || !strings.Contains(string(body), `"configured":true`) {
		t.Fatalf("token-authorized client could not read pool limits: %d %s", res.StatusCode, body)
	}
	if limits, _, err := db.PoolLimits(); err != nil || limits != (nodedb.PoolLimits{MaxTasksPerClient: 8, MaxUploadBytesPerAttempt: 4096}) {
		t.Fatalf("token-authorized pool limits = %#v, error %v", limits, err)
	}
	res = request(http.MethodGet, "/devices", "", cookie, false)
	body, _ = io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusOK || !strings.Contains(string(body), `"name":"Phone"`) || strings.Contains(string(body), "tokenHash") {
		t.Fatalf("list paired devices without credentials: %d %s", res.StatusCode, body)
	}
	var devices struct {
		Devices []struct {
			ID string `json:"id"`
		} `json:"devices"`
	}
	if err := json.Unmarshal(body, &devices); err != nil || len(devices.Devices) != 1 {
		t.Fatalf("decode paired devices: %s, %v", body, err)
	}
	res = request(http.MethodDelete, "/devices/"+devices.Devices[0].ID, "", cookie, false)
	res.Body.Close()
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("revoke paired device: %d", res.StatusCode)
	}
	select {
	case <-deviceChanges:
	case <-time.After(eventualWait):
		t.Fatal("revocation did not notify paired-device subscribers")
	}
	res = request(http.MethodPost, nodeInfo, `{}`, cookie, false)
	body, _ = io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("revoked cookie remains active: %d %s", res.StatusCode, body)
	}
}

// A web app given no browser access manager, or an empty one, makes its own
// and still serves.
func TestWebAppWithoutABrowserAccessManagerMakesItsOwn(t *testing.T) {
	ui := fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("UI")}}
	for name, manager := range map[string]*BrowserAccessManager{"none": nil, "empty": {}} {
		server := httptest.NewServer(NewWebAppHandlerWithBrowserAccess(LocalConfig{Token: "owner-token"}, nil, ui, manager))
		res, err := http.Get(server.URL + "/")
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		server.Close()
		if res.StatusCode != http.StatusOK || string(body) != "UI" {
			t.Errorf("%s manager: %d %q, want the UI", name, res.StatusCode, body)
		}
	}
}

func TestRevocationIsSharedAcrossLocalAndRemoteWebListeners(t *testing.T) {
	db, err := nodedb.Open(filepath.Join(t.TempDir(), "node.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cfg := LocalConfig{NodeID: "12D3KooWnode", Token: "owner-token", Listen: func() []string { return nil }, BrowserAccess: db}
	manager := NewBrowserAccessManager(cfg)
	ui := fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("UI")}}
	local := httptest.NewServer(NewWebAppHandlerWithBrowserAccess(cfg, nil, ui, manager))
	defer local.Close()
	remote := httptest.NewServer(NewWebAppHandlerWithBrowserAccess(cfg, nil, ui, manager))
	defer remote.Close()
	request := func(server *httptest.Server, method, route, body string, cookie *http.Cookie, owner bool) *http.Response {
		t.Helper()
		req, err := http.NewRequest(method, server.URL+route, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Origin", server.URL)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Connect-Protocol-Version", "1")
		if owner {
			req.Header.Set("Authorization", "Bearer owner-token")
		}
		if cookie != nil {
			req.AddCookie(cookie)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	res := request(local, http.MethodPost, "/pairing", "{}", nil, true)
	var invitation struct {
		Code string `json:"code"`
	}
	if res.StatusCode != http.StatusOK || json.NewDecoder(res.Body).Decode(&invitation) != nil {
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		t.Fatalf("create pairing invitation: %d %s", res.StatusCode, body)
	}
	res.Body.Close()
	res = request(remote, http.MethodPost, "/pair", `{"code":"`+invitation.Code+`","name":"Phone"}`, nil, false)
	var cookie *http.Cookie
	for _, item := range res.Cookies() {
		if item.Name == webSessionCookie {
			cookie = item
		}
	}
	res.Body.Close()
	if res.StatusCode != http.StatusNoContent || cookie == nil {
		t.Fatalf("pair through remote listener: status %d, cookie present %t", res.StatusCode, cookie != nil)
	}
	res = request(remote, http.MethodPost, nodeInfo, `{}`, cookie, false)
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("paired browser was not authorized by remote listener: %d", res.StatusCode)
	}
	deviceID, _, _ := strings.Cut(cookie.Value, ".")
	ctx, finish, ok := manager.devices.authorizeRequest(cookie.Value, context.Background())
	if !ok {
		t.Fatal("could not track active remote-listener request")
	}
	defer finish()
	res = request(local, http.MethodDelete, "/devices/"+deviceID, "", nil, true)
	res.Body.Close()
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("revoke through local listener: %d", res.StatusCode)
	}
	select {
	case <-ctx.Done():
	case <-time.After(eventualWait):
		t.Fatal("local-listener revocation did not cancel the remote-listener request")
	}
	res = request(remote, http.MethodPost, nodeInfo, `{}`, cookie, false)
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("remote listener accepted revoked browser: %d", res.StatusCode)
	}
}

func TestWebControlRejectsBadPairingAndStoreFailures(t *testing.T) {
	errStore := &failingBrowserAccessStore{err: errors.New("store unavailable")}
	cfg := LocalConfig{Token: "owner-token", BrowserAccess: errStore}
	server := httptest.NewServer(NewWebAppHandler(cfg, []string{"https://allowed.example"}, fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("UI")}}))
	defer server.Close()
	request := func(method, route, body, origin, bearer string) *http.Response {
		t.Helper()
		req, err := http.NewRequest(method, server.URL+route, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		req.Header.Set("Content-Type", "application/json")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	check := func(method, route, body, origin, bearer string, want int) string {
		t.Helper()
		res := request(method, route, body, origin, bearer)
		defer res.Body.Close()
		said, _ := io.ReadAll(res.Body)
		if res.StatusCode != want {
			t.Errorf("%s %s: got %d %s, want %d", method, route, res.StatusCode, said, want)
		}
		return string(said)
	}
	check(http.MethodGet, "/session", "", "", "", http.StatusMethodNotAllowed)
	check(http.MethodPost, "/session", `{}`, server.URL, "wrong", http.StatusUnauthorized)
	check(http.MethodGet, "/pair", `{}`, server.URL, "", http.StatusMethodNotAllowed)
	check(http.MethodPost, "/pair", `{"code":"12345678"}`, "", "", http.StatusForbidden)
	check(http.MethodPost, "/pair", `not-json`, server.URL, "", http.StatusBadRequest)
	check(http.MethodPost, "/pair", `{"code":"12345678","name":"`+strings.Repeat("x", 81)+`"}`, server.URL, "", http.StatusBadRequest)
	check(http.MethodPost, "/pairing", `{}`, server.URL, "wrong", http.StatusUnauthorized)
	check(http.MethodPost, "/pairing", `{}`, "https://elsewhere.example", "owner-token", http.StatusForbidden)
	check(http.MethodOptions, "/anything", "", "", "", http.StatusNoContent)
	check(http.MethodPost, "/pairing", `{}`, server.URL, "owner-token", http.StatusInternalServerError)
	check(http.MethodGet, "/devices", `{}`, server.URL, "owner-token", http.StatusInternalServerError)
	check(http.MethodDelete, "/devices/device-1", `{}`, server.URL, "owner-token", http.StatusInternalServerError)
	check(http.MethodPost, "/devices", `{}`, server.URL, "owner-token", http.StatusMethodNotAllowed)
	check(http.MethodPost, "/pairing", `{}`, "", "", http.StatusUnauthorized)
	errStore.err = nil
	check(http.MethodPost, "/pair", `{"code":"12345678"}`, server.URL, "", http.StatusUnauthorized)
	check(http.MethodDelete, "/devices/a/b", `{}`, server.URL, "owner-token", http.StatusNotFound)
	check(http.MethodPost, "/devices/device-1", `{}`, server.URL, "owner-token", http.StatusMethodNotAllowed)
	check(http.MethodDelete, "/devices/device-1", `{}`, "https://allowed.example", "owner-token", http.StatusForbidden)
	check(http.MethodGet, "/devices", `{}`, "https://allowed.example", "owner-token", http.StatusForbidden)
	check(http.MethodPost, "/pairing", `{}`, "https://allowed.example", "owner-token", http.StatusForbidden)
	errStore.consumeErr = errors.New("consume unavailable")
	check(http.MethodPost, "/pair", `{"code":"12345678"}`, server.URL, "", http.StatusInternalServerError)
	sessions := newWebSessions()
	sessionID, err := sessions.create()
	if err != nil {
		t.Fatal(err)
	}
	cookieRequest := httptest.NewRequest(http.MethodGet, "http://node.test/devices", nil)
	cookieRequest.AddCookie(&http.Cookie{Name: webSessionCookie, Value: sessionID})
	cookieRequest.Header.Set("Origin", "https://allowed.example")
	cookieResponse := httptest.NewRecorder()
	webControl(cookieResponse, cookieRequest, cfg, sessions, newBrowserAccess(errStore, "pepper"))
	if cookieResponse.Code != http.StatusForbidden {
		t.Fatalf("cross-origin cookie device management: %d", cookieResponse.Code)
	}
	secureCfg := LocalConfig{Token: "owner-token"}
	secureRequest := httptest.NewRequest(http.MethodPost, "https://node.test/pairing", strings.NewReader(`{}`))
	secureRequest.Header.Set("Authorization", "Bearer owner-token")
	secureRequest.Header.Set("Origin", "https://node.test")
	secureResponse := httptest.NewRecorder()
	webControl(secureResponse, secureRequest, secureCfg, newWebSessions(), newBrowserAccess(nil, "pepper"))
	if secureResponse.Code != http.StatusOK || !strings.Contains(secureResponse.Body.String(), "https://node.test/#pair=") {
		t.Fatalf("secure pairing URL: %d %s", secureResponse.Code, secureResponse.Body.String())
	}
}

func TestBrowserPairingFallbackThrottleAndSessionExpiry(t *testing.T) {
	now := time.Now()
	volatile := &volatileBrowserAccess{devices: make(map[string]nodedb.BrowserDevice)}
	if err := volatile.SaveBrowserPairing([]byte("hash"), now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if consumed, err := volatile.ConsumeBrowserPairing([]byte("wrong"), now, nodedb.BrowserDevice{ID: "unused"}); err != nil || consumed {
		t.Fatalf("wrong code: %v, %v", consumed, err)
	}
	if consumed, err := volatile.ConsumeBrowserPairing([]byte("hash"), now, nodedb.BrowserDevice{ID: "device"}); err != nil || !consumed {
		t.Fatalf("valid code: %v, %v", consumed, err)
	}
	if devices, err := volatile.BrowserDevices(); err != nil || len(devices) != 1 {
		t.Fatalf("volatile listing: %+v, %v", devices, err)
	}
	if err := volatile.DeleteBrowserDevice("device"); err != nil {
		t.Fatal(err)
	}

	access := newBrowserAccess(nil, "pepper")
	for i := 0; i < 8; i++ {
		if _, _, paired, err := access.pair("wrong", "Browser", "remote", now); err != nil || paired {
			t.Fatalf("bad pairing attempt %d: %v, %v", i, paired, err)
		}
	}
	if _, _, paired, err := access.pair("wrong", "Browser", "remote", now); err != nil || paired {
		t.Fatalf("blocked address: %v, %v", paired, err)
	}
	access.blocked["expired"] = now.Add(-time.Second)
	access.failed["other"] = 1
	for i := 0; i < 512; i++ {
		access.blocked[fmt.Sprintf("stale-%d", i)] = now.Add(-time.Second)
	}
	if _, _, _, err := access.pair("wrong", "Browser", "remote-new", now); err != nil {
		t.Fatal(err)
	}
	if _, exists := access.blocked["expired"]; exists {
		t.Fatal("expired throttle entry was not cleaned up")
	}
	access.failed = make(map[string]int)
	access.blocked = make(map[string]time.Time)
	for i := 0; i < 4096; i++ {
		access.blocked[fmt.Sprintf("full-%d", i)] = now.Add(time.Minute)
	}
	if _, _, paired, err := access.pair("wrong", "Browser", "new", now); err != nil || paired {
		t.Fatalf("bounded throttle map: %v, %v", paired, err)
	}

	sessions := newWebSessions()
	sessions.active["expired"] = time.Now().Add(-time.Second)
	if sessions.valid("expired") {
		t.Fatal("expired web session remained valid")
	}
	if sessions.valid("missing") {
		t.Fatal("unknown web session was valid")
	}
	sessions.active["old"] = time.Now().Add(-time.Second)
	if _, err := sessions.create(); err != nil {
		t.Fatal(err)
	}
	if _, exists := sessions.active["old"]; exists {
		t.Fatal("session creation did not prune expired sessions")
	}
	if access.valid("malformed") || access.valid("missing.token") {
		t.Fatal("accepted malformed or unknown browser credential")
	}

	access.random = errorReader{}
	sessions.random = errorReader{}
	if _, _, err := access.issuePairing(now); err == nil {
		t.Fatal("pairing code generation ignored a failing random source")
	}
	if _, err := sessions.create(); err == nil {
		t.Fatal("session generation ignored a failing random source")
	}
	handler := newWebAppHandler(LocalConfig{Token: "owner-token"}, nil, fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("UI")}}, errorReader{})
	failedSession := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/session", nil)
	failedSession.Header.Set("Authorization", "Bearer owner-token")
	failedResponse := httptest.NewRecorder()
	handler.ServeHTTP(failedResponse, failedSession)
	if failedResponse.Code != http.StatusInternalServerError {
		t.Fatalf("failed session entropy status: %d", failedResponse.Code)
	}
	access.blocked = make(map[string]time.Time)
	if _, _, _, err := access.pair("code", "Browser", "random-failure", now); err == nil {
		t.Fatal("browser token generation ignored a failing random source")
	}
	access.random = &partialErrorReader{remaining: 32}
	if _, _, _, err := access.pair("code", "Browser", "id-random-failure", now); err == nil {
		t.Fatal("browser ID generation ignored a failing random source")
	}
}

func TestHTTPSWebListenerAcceptsOnlyItsConfiguredPublicHost(t *testing.T) {
	ui := fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("secure UI")}}
	server := httptest.NewTLSServer(NewWebAppHandler(LocalConfig{
		AllowLAN: true, PairingOrigin: "https://node.example:9443",
	}, nil, ui))
	defer server.Close()

	request := func(host string) *http.Response {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, server.URL+"/", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Host = host
		response, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return response
	}
	response := request("node.example:9443")
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || string(body) != "secure UI" {
		t.Fatalf("configured public host: %d %s", response.StatusCode, body)
	}
	response = request("attacker.example:9443")
	response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("unconfigured host: %d, want forbidden", response.StatusCode)
	}
}

func TestWebOriginHostAndStaticMethodEdges(t *testing.T) {
	if sameOrigin(httptest.NewRequest(http.MethodGet, "http://example.test/", nil), "https://example.test") {
		t.Fatal("accepted an origin with the wrong scheme")
	}
	for _, origin := range []string{"not an origin", "http://user@example.test", "http://example.test/path", "http://example.test/?x=1", "http://example.test/#part"} {
		if sameOrigin(httptest.NewRequest(http.MethodGet, "http://example.test/", nil), origin) {
			t.Errorf("accepted malformed origin %q", origin)
		}
	}
	request := httptest.NewRequest(http.MethodPost, "http://example.test/", nil)
	if sameBrowserOrigin(request) {
		t.Fatal("accepted a request without origin or referer")
	}
	request.Header.Set("Referer", "://bad")
	if sameBrowserOrigin(request) {
		t.Fatal("accepted malformed referer")
	}
	request.Header.Set("Referer", "http://example.test/a")
	if !sameBrowserOrigin(request) {
		t.Fatal("did not accept same-origin referer fallback")
	}

	if webHostAllowed("example.com:1234", true) {
		t.Fatal("accepted public host")
	}
	if webHostAllowed("10.0.0.1:1234", true) && localPrivateAddress("") != "10.0.0.1" {
		t.Fatal("accepted an unassigned private host")
	}
	if address := localPrivateAddress(""); address != "" && !webHostAllowed(net.JoinHostPort(address, "1234"), true) {
		t.Fatalf("did not accept local private address %s", address)
	}
	if webHostAllowedWithInterfaces("10.0.0.1:1234", true, func() ([]net.Interface, error) { return nil, errors.New("interfaces unavailable") }, func(net.Interface) ([]net.Addr, error) { return nil, nil }) {
		t.Fatal("accepted LAN host when interfaces could not be enumerated")
	}
	if webHostAllowedWithInterfaces("10.0.0.1:1234", true, func() ([]net.Interface, error) { return []net.Interface{{Index: -1}}, nil }, func(net.Interface) ([]net.Addr, error) { return nil, errors.New("address unavailable") }) {
		t.Fatal("accepted LAN host when an interface address could not be read")
	}
	if webHostAllowedWithInterfaces("10.0.0.1", true, func() ([]net.Interface, error) { return nil, nil }, func(net.Interface) ([]net.Addr, error) { return nil, nil }) {
		t.Fatal("accepted an unassigned host without a port")
	}
	if !webHostAllowedWithInterfaces("10.0.0.1", true, func() ([]net.Interface, error) { return []net.Interface{{Index: 1}}, nil }, func(net.Interface) ([]net.Addr, error) { return []net.Addr{testNetworkAddress("10.0.0.1/24")}, nil }) {
		t.Fatal("did not accept an assigned private interface address")
	}

	static := staticWebUI(fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("<head></head>")}})
	response := httptest.NewRecorder()
	static.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "http://example.test/", nil))
	if response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("static POST status: %d", response.Code)
	}
	response = httptest.NewRecorder()
	static.ServeHTTP(response, httptest.NewRequest(http.MethodHead, "http://example.test/", nil))
	if response.Code != http.StatusOK || response.Body.Len() != 0 {
		t.Fatalf("static HEAD: %d %q", response.Code, response.Body.String())
	}
	for _, method := range []string{http.MethodPut, http.MethodPatch, http.MethodDelete} {
		if !mutatesState(httptest.NewRequest(method, "http://example.test/node", nil)) {
			t.Errorf("%s should be a state mutation", method)
		}
	}
	if !mutatesState(httptest.NewRequest(http.MethodPost, "http://example.test/files/a", nil)) || !mutatesState(httptest.NewRequest(http.MethodPost, "http://example.test/sisyphus.node.v1.NodeService/Call", nil)) || mutatesState(httptest.NewRequest(http.MethodPost, "http://example.test/read", nil)) {
		t.Fatal("POST mutation path classification is wrong")
	}
	secure := httptest.NewRequest(http.MethodGet, "https://example.test/", nil)
	secure.TLS = &tls.ConnectionState{}
	if !sameOrigin(secure, "HTTPS://EXAMPLE.TEST") {
		t.Fatal("did not compare secure origin case-insensitively")
	}
}

func localPrivateAddress(exclude string) string {
	interfaces, err := net.Interfaces()
	if err != nil {
		return ""
	}
	for _, network := range interfaces {
		addresses, _ := network.Addrs()
		for _, item := range addresses {
			prefix, err := netip.ParsePrefix(item.String())
			if err == nil && prefix.Addr().IsPrivate() && !prefix.Addr().IsLoopback() && prefix.Addr().String() != exclude {
				return prefix.Addr().String()
			}
		}
	}
	return ""
}
