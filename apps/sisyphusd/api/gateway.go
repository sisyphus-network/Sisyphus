package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ipfs/go-cid"

	"github.com/sisyphus-network/Sisyphus/packages/names"
	"github.com/sisyphus-network/Sisyphus/packages/sealed"
	"github.com/sisyphus-network/Sisyphus/packages/storage"
)

// Opener is the part of a store a gateway reads from.
type Opener interface {
	Open(ctx context.Context, c cid.Cid) (storage.Blob, error)
}

// Holder is the part of a node's names a gateway resolves from.
type Holder interface {
	Held(id string) (*names.Record, error)
}

// NewGateway returns a read-only gateway onto a node's stored data: a file
// is fetched by its content ID with a plain GET of /ipfs/<cid>, as from any
// IPFS gateway, so that a browser, a web page or curl can have a result
// without being a node. It only ever reads.
//
// A file is also fetched by a name, as /ipns/<node ID>: the gateway looks
// up the record held for that name and serves the file the record points
// to. Asked for application/vnd.ipfs.ipns-record, it gives the signed
// record itself instead, for whoever would rather check it than trust the
// gateway. What a name stands for changes, so neither answer may be kept
// for longer than the record allows.
//
// Who may read is the owner's choice. Unless open is set, a request must
// show token, as a bearer token or as ?token=, which is for the owner's own
// pages and scripts. With open set, anyone who can reach the gateway and
// knows a file's content ID can read it, which is how a link to a result is
// shared. Either way a sealed file is refused: what the gateway would send
// is what was sealed, which is of no use to anyone, and serving it would
// suggest otherwise.
func NewGateway(store Opener, held Holder, token string, open bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "this gateway only reads", http.StatusMethodNotAllowed)
			return
		}
		kind, asked, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
		if (kind != "ipfs" && kind != "ipns") || asked == "" || strings.Contains(asked, "/") {
			http.Error(w, "ask for a file as /ipfs/<content ID>, or for what a node has named as /ipns/<node ID>", http.StatusNotFound)
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
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		if open {
			// A page anywhere may fetch what anyone may fetch.
			h.Set("Access-Control-Allow-Origin", "*")
		}
		// The same ID is always the same bytes, so it may be kept for good.
		keep := "public, max-age=29030400, immutable"
		var c cid.Cid
		if kind == "ipns" {
			node, err := names.ID(asked)
			if err != nil {
				http.Error(w, "that is not a name: a name is a node's ID", http.StatusBadRequest)
				return
			}
			record, err := held.Held(node)
			if errors.Is(err, ErrNoRecord) {
				http.Error(w, err.Error(), http.StatusNotFound)
				return
			}
			if err != nil {
				http.Error(w, "the record could not be read: "+err.Error(), http.StatusInternalServerError)
				return
			}
			now := time.Now()
			if record.Expired(now) {
				http.Error(w, "the record this node holds for that name expired at "+record.Expires.UTC().Format(time.RFC3339)+", and its node has published none since", http.StatusNotFound)
				return
			}
			// What a name stands for may be kept for as long as its record
			// says, and no longer than the record lasts.
			keep = "public, max-age=" + strconv.Itoa(int(record.Fresh(now).Seconds()))
			h.Set("X-Ipfs-Path", "/ipns/"+asked)
			if strings.Contains(r.Header.Get("Accept"), names.ContentType) || r.URL.Query().Get("format") == "ipns-record" {
				raw := record.Bytes()
				sum := sha256.Sum256(raw)
				h.Set("Cache-Control", keep)
				h.Set("Content-Type", names.ContentType)
				h.Set("Content-Disposition", `attachment; filename="`+node+`.ipns-record"`)
				h.Set("Etag", `"`+hex.EncodeToString(sum[:16])+`"`)
				http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(raw))
				return
			}
			c = record.Value
			h.Set("X-Ipfs-Roots", c.String())
		} else {
			var err error
			if c, err = cid.Decode(asked); err != nil {
				http.Error(w, "that is not a content ID", http.StatusBadRequest)
				return
			}
			h.Set("X-Ipfs-Path", "/ipfs/"+c.String())
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
		h.Set("Cache-Control", keep)
		h.Set("Etag", `"`+c.String()+`"`)
		filename := r.URL.Query().Get("filename")
		if filename != "" {
			h.Set("Content-Disposition", `inline; filename="`+strings.NewReplacer(`"`, "", "\\", "", "\n", "", "\r", "").Replace(filename)+`"`)
		}
		// By name where one is given, and otherwise by what it begins with.
		http.ServeContent(w, r, filename, time.Time{}, blob)
	})
}
