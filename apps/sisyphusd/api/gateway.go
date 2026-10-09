package api

import (
	"context"
	"crypto/subtle"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ipfs/go-cid"

	"github.com/sisyphus-network/Sisyphus/packages/sealed"
	"github.com/sisyphus-network/Sisyphus/packages/storage"
)

// Opener is the part of a store a gateway reads from.
type Opener interface {
	Open(ctx context.Context, c cid.Cid) (storage.Blob, error)
}

// NewGateway returns a read-only gateway onto a node's stored data: a file
// is fetched by its content ID with a plain GET of /ipfs/<cid>, as from any
// IPFS gateway, so that a browser, a web page or curl can have a result
// without being a node. It only ever reads.
//
// Who may read is the owner's choice. Unless open is set, a request must
// show token, as a bearer token or as ?token=, which is for the owner's own
// pages and scripts. With open set, anyone who can reach the gateway and
// knows a file's content ID can read it, which is how a link to a result is
// shared. Either way a sealed file is refused: what the gateway would send
// is what was sealed, which is of no use to anyone, and serving it would
// suggest otherwise.
func NewGateway(store Opener, token string, open bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "this gateway only reads", http.StatusMethodNotAllowed)
			return
		}
		id, isFile := strings.CutPrefix(r.URL.Path, "/ipfs/")
		if !isFile || id == "" || strings.Contains(id, "/") {
			http.Error(w, "ask for a file as /ipfs/<content ID>", http.StatusNotFound)
			return
		}
		if !open {
			shown := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if shown == "" {
				shown = r.URL.Query().Get("token")
			}
			if subtle.ConstantTimeCompare([]byte(shown), []byte(token)) != 1 {
				w.Header().Set("WWW-Authenticate", "Bearer")
				http.Error(w, "this gateway is its owner's: show the node's API token, or have the owner start it with --gateway-open", http.StatusUnauthorized)
				return
			}
		}
		c, err := cid.Decode(id)
		if err != nil {
			http.Error(w, "that is not a content ID", http.StatusBadRequest)
			return
		}
		blob, err := store.Open(r.Context(), c)
		if errors.Is(err, storage.ErrNotFound) {
			http.Error(w, "this node does not hold that file", http.StatusNotFound)
			return
		}
		if err != nil {
			http.Error(w, "the file could not be read: "+err.Error(), http.StatusInternalServerError)
			return
		}
		defer blob.Close()
		start := make([]byte, 8)
		n, _ := io.ReadFull(blob, start)
		if sealed.IsSealed(start[:n]) {
			http.Error(w, "that file is sealed: it is fetched through the node, by whoever holds its key", http.StatusForbidden)
			return
		}
		blob.Seek(0, io.SeekStart) // a blob that could be opened can be rewound
		h := w.Header()
		// The same ID is always the same bytes, so it may be kept for good.
		h.Set("Cache-Control", "public, max-age=29030400, immutable")
		h.Set("Etag", `"`+c.String()+`"`)
		h.Set("X-Ipfs-Path", "/ipfs/"+c.String())
		h.Set("X-Content-Type-Options", "nosniff")
		if open {
			// A page anywhere may fetch what anyone may fetch.
			h.Set("Access-Control-Allow-Origin", "*")
		}
		name := r.URL.Query().Get("filename")
		if name != "" {
			h.Set("Content-Disposition", `inline; filename="`+strings.NewReplacer(`"`, "", "\\", "", "\n", "", "\r", "").Replace(name)+`"`)
		}
		// By name where one is given, and otherwise by what it begins with.
		http.ServeContent(w, r, name, time.Time{}, blob)
	})
}
