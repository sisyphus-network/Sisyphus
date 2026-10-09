package api

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ipfs/go-cid"

	"github.com/sisyphus-network/Sisyphus/packages/sealed"
	"github.com/sisyphus-network/Sisyphus/packages/storage"
)

// unreadable is a store that cannot be read.
type unreadable struct{}

func (unreadable) Open(context.Context, cid.Cid) (storage.Blob, error) {
	return nil, errors.New("the disk has failed")
}

// fetchFrom asks a gateway for something.
func fetchFrom(gateway http.Handler, method, path string, headers ...string) (int, string, http.Header) {
	req := httptest.NewRequest(method, path, nil)
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	gateway.ServeHTTP(rec, req)
	body, _ := io.ReadAll(rec.Result().Body)
	return rec.Code, string(body), rec.Result().Header
}

func TestAStoredFileIsFetchedByItsContentIDWithAPlainGET(t *testing.T) {
	ctx := context.Background()
	store := storage.NewMemory()
	page, _ := store.Put(ctx, strings.NewReader("<html><body>the boulder rolls</body></html>"))
	key := sealed.NewKey()
	secret, _ := store.Put(ctx, sealed.Encrypt(key, strings.NewReader("for the owner's eyes")))
	absent, _ := storage.NewMemory().Put(ctx, strings.NewReader("held elsewhere"))
	owners := NewGateway(store, noNames{}, "the-token", false)

	status, body, header := fetchFrom(owners, http.MethodGet, "/ipfs/"+page.String(), "Authorization", "Bearer the-token")
	if status != http.StatusOK || !strings.Contains(body, "the boulder rolls") || !strings.HasPrefix(header.Get("Content-Type"), "text/html") ||
		header.Get("Etag") != `"`+page.String()+`"` || !strings.Contains(header.Get("Cache-Control"), "immutable") || header.Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("GET: %d %q %v", status, body, header)
	}
	// The token may ride in the address, for a link; a name may be given
	// for the file, and says what kind it is; part of it may be asked for.
	status, body, header = fetchFrom(owners, http.MethodGet, "/ipfs/"+page.String()+"?token=the-token&filename=notes\".txt", "Range", "bytes=12-22")
	if status != http.StatusPartialContent || body != "the boulder" || header.Get("Content-Disposition") != `inline; filename="notes.txt"` || !strings.HasPrefix(header.Get("Content-Type"), "text/plain") {
		t.Errorf("a named part: %d %q %v", status, body, header)
	}
	if status, body, header := fetchFrom(owners, http.MethodHead, "/ipfs/"+page.String(), "Authorization", "Bearer the-token"); status != http.StatusOK || body != "" || header.Get("Content-Length") != "43" {
		t.Errorf("HEAD: %d %q %v", status, body, header)
	}

	for name, tt := range map[string]struct {
		method, path, key string
		status            int
		want              string
	}{
		"without the token":    {http.MethodGet, "/ipfs/" + page.String(), "", http.StatusUnauthorized, "show the node's API token"},
		"with another token":   {http.MethodGet, "/ipfs/" + page.String(), "another", http.StatusUnauthorized, "show the node's API token"},
		"to write":             {http.MethodPut, "/ipfs/" + page.String(), "the-token", http.StatusMethodNotAllowed, "only reads"},
		"for something else":   {http.MethodGet, "/api/v0/add", "the-token", http.StatusNotFound, "/ipfs/<content ID>"},
		"for a path in a file": {http.MethodGet, "/ipfs/" + page.String() + "/index.html", "the-token", http.StatusNotFound, "/ipfs/<content ID>"},
		"for what is no ID":    {http.MethodGet, "/ipfs/not-a-cid", "the-token", http.StatusBadRequest, "not a content ID"},
		"for what is not held": {http.MethodGet, "/ipfs/" + absent.String(), "the-token", http.StatusNotFound, "does not hold that file"},
		"for a sealed file":    {http.MethodGet, "/ipfs/" + secret.String(), "the-token", http.StatusForbidden, "sealed"},
	} {
		if status, body, _ := fetchFrom(owners, tt.method, tt.path, "Authorization", "Bearer "+tt.key); status != tt.status || !strings.Contains(body, tt.want) {
			t.Errorf("asked %s: %d %q, want %d and %q", name, status, body, tt.status, tt.want)
		}
	}
	if status, body, _ := fetchFrom(NewGateway(unreadable{}, noNames{}, "the-token", false), http.MethodGet, "/ipfs/"+page.String(), "Authorization", "Bearer the-token"); status != http.StatusInternalServerError || !strings.Contains(body, "the disk has failed") {
		t.Errorf("from a store that cannot be read: %d %q", status, body)
	}

	// Opened by its owner, a gateway gives what is not sealed to anyone
	// who knows its ID, pages elsewhere included, and still not what is.
	anyones := NewGateway(store, noNames{}, "the-token", true)
	if status, body, header := fetchFrom(anyones, http.MethodGet, "/ipfs/"+page.String()); status != http.StatusOK || !bytes.Contains([]byte(body), []byte("boulder")) || header.Get("Access-Control-Allow-Origin") != "*" {
		t.Errorf("from an open gateway: %d %q %v", status, body, header)
	}
	if status, _, _ := fetchFrom(anyones, http.MethodGet, "/ipfs/"+secret.String()); status != http.StatusForbidden {
		t.Errorf("a sealed file from an open gateway: %d", status)
	}
}
