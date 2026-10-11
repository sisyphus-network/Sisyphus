package api

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/sisyphus-network/Sisyphus/packages/nodedb"
	"github.com/sisyphus-network/Sisyphus/packages/storage"
)

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

func TestOneTimePairingCreatesARevocableBrowserSession(t *testing.T) {
	db, err := nodedb.Open(filepath.Join(t.TempDir(), "node.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cfg := LocalConfig{NodeID: "12D3KooWnode", Token: "owner-token", Listen: func() []string { return nil }, BrowserAccess: db}
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
	res = request(http.MethodPost, nodeInfo, `{}`, cookie, false)
	body, _ = io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("revoked cookie remains active: %d %s", res.StatusCode, body)
	}
}
