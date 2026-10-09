package api

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
