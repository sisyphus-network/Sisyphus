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

func TestStaticWebListenerIsLoopbackOnlyAndRemoteAccessHasNoCLIBypass(t *testing.T) {
	for _, address := range []string{"127.0.0.1:50052", "[::1]:50052"} {
		if err := validateWebListen(address); err != nil {
			t.Errorf("validateWebListen(%q): %v", address, err)
		}
	}
	for _, address := range []string{"0.0.0.0:50052", "192.168.1.2:50052", "8.8.8.8:50052", "nonsense"} {
		if err := validateWebListen(address); err == nil {
			t.Errorf("validateWebListen(%q) unexpectedly allows a non-loopback listener", address)
		}
	}
	if _, err := cli(t, "run", "--data-dir", t.TempDir(), "--web-lan"); err == nil || !strings.Contains(err.Error(), "flag provided but not defined") {
		t.Errorf("legacy plain-HTTP --web-lan option: %v", err)
	}
}

func TestTheNodeServesItsWebBuildAndEnrolledBrowserSession(t *testing.T) {
	dataDir, uiDir := t.TempDir(), t.TempDir()
	indexHTML := `<html><head></head><body>node web ui</body><script type="module" src="/assets/app.js"></script><link rel="stylesheet" href="/assets/app.css"></html>`
	if err := os.WriteFile(filepath.Join(uiDir, "index.html"), []byte(indexHTML), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(uiDir, "assets"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(uiDir, "assets", "app.js"), []byte("console.log('sisyphus')"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(uiDir, "assets", "app.css"), []byte("#root { display: block }"), 0o600); err != nil {
		t.Fatal(err)
	}
	webAddr := freeAddr(t)
	nodeAddr := freeAddr(t)
	startDaemon(t, "--data-dir", dataDir, "--listen", nodeAddr, "--web-listen", webAddr, "--web-ui-dir", uiDir, "--name", "web-ui")
	baseURL := "http://" + webAddr
	waitForOutput(t, "web-ui", "nodes", "--addr", nodeAddr)

	get := func(path string) *http.Response {
		t.Helper()
		res, err := http.Get(baseURL + path)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	res := get("/")
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusOK || !strings.Contains(string(body), "node web ui") {
		t.Fatalf("serve index: %d %s", res.StatusCode, body)
	}
	res = get("/settings")
	body, _ = io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusOK || !strings.Contains(string(body), `<base href="/">`) {
		t.Fatalf("serve deep SPA route with root assets: %d %s", res.StatusCode, body)
	}
	for _, asset := range []struct{ path, contentType, body string }{
		{"/assets/app.js", "text/javascript", "console.log('sisyphus')"},
		{"/assets/app.css", "text/css", "#root { display: block }"},
	} {
		res = get(asset.path)
		body, _ = io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode != http.StatusOK || !strings.HasPrefix(res.Header.Get("Content-Type"), asset.contentType) || string(body) != asset.body {
			t.Errorf("serve renderer asset %s: status=%d content-type=%q body=%q", asset.path, res.StatusCode, res.Header.Get("Content-Type"), body)
		}
	}

	token, err := os.ReadFile(filepath.Join(dataDir, "api.token"))
	if err != nil {
		t.Fatal(err)
	}
	sessionRequest, _ := http.NewRequest(http.MethodPost, baseURL+"/session", nil)
	sessionRequest.Header.Set("Origin", baseURL)
	sessionRequest.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	res, err = http.DefaultClient.Do(sessionRequest)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusNoContent || len(res.Cookies()) == 0 || !res.Cookies()[0].HttpOnly || res.Cookies()[0].Value == strings.TrimSpace(string(token)) {
		body, _ = io.ReadAll(res.Body)
		res.Body.Close()
		t.Fatalf("create browser session: %d %s %v", res.StatusCode, body, res.Cookies())
	}
	cookie := res.Cookies()[0]
	res.Body.Close()
	apiRequest, _ := http.NewRequest(http.MethodPost, baseURL+"/sisyphus.node.v1.NodeService/GetNodeInfo", strings.NewReader(`{}`))
	apiRequest.Header.Set("Origin", baseURL)
	apiRequest.Header.Set("Content-Type", "application/json")
	apiRequest.Header.Set("Connect-Protocol-Version", "1")
	apiRequest.AddCookie(cookie)
	res, err = http.DefaultClient.Do(apiRequest)
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusOK || !strings.Contains(string(body), `"peerId"`) {
		t.Fatalf("call node API with cookie: %d %s", res.StatusCode, body)
	}
}

func TestWebOptionDependenciesAreValidated(t *testing.T) {
	notDirectory := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(notDirectory, []byte("file"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name string
		args []string
		want string
	}{
		{"UI needs listener", []string{"--web-ui-dir", t.TempDir()}, "--web-ui-dir needs --web-listen"},
		{"UI directory is missing", []string{"--web-listen", freeAddr(t), "--web-ui-dir", filepath.Join(t.TempDir(), "missing")}, "must name a directory"},
		{"UI path is a file", []string{"--web-listen", freeAddr(t), "--web-ui-dir", notDirectory}, "must name a directory"},
		{"UI needs index", []string{"--web-listen", freeAddr(t), "--web-ui-dir", t.TempDir()}, "has no index.html"},
	} {
		args := append([]string{"run", "--data-dir", t.TempDir()}, test.args...)
		if _, err := cli(t, args...); err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("%s: got %v, want error containing %q", test.name, err, test.want)
		}
	}
}
