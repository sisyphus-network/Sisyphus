package api

import (
	"crypto/subtle"
	"encoding/json"
	"net"
	"net/http"
	"slices"
	"strings"

	"connectrpc.com/vanguard/vanguardgrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"

	nodepb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/node/v1"
)

// NewWebHandler serves the local API to web pages. A page cannot speak
// gRPC as the desktop app does, so the same calls are offered in the ways a
// browser can make them: gRPC-Web, and Connect, in which a call that
// returns one thing is a plain POST of JSON.
//
// A browser is a more dangerous caller than a program on the machine: any
// page the user visits can try the node's address. So here every call
// needs the node's token, reading included; a page is answered only if it
// comes from one of origins; and a request that names the node by anything
// but this machine's own address is refused, which stops a page reaching
// it by a name of the page's own choosing.
//
// Sending a file in pieces is the one thing a browser cannot do in either
// protocol, so a file is stored by PUT to /files/<name>, its body the
// file, with ?private=true to seal it; the answer is the File as JSON.
func NewWebHandler(cfg LocalConfig, origins []string) http.Handler {
	service := &localService{cfg: normal(cfg)}
	server := grpc.NewServer()
	nodepb.RegisterNodeServiceServer(server, service)
	calls, _ := vanguardgrpc.NewTranscoder(server) // fails only for a service with no description, and this one has it
	mux := http.NewServeMux()
	mux.Handle("/", calls)
	mux.HandleFunc("PUT /files/{name}", func(w http.ResponseWriter, r *http.Request) {
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
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !thisMachine(r.Host) {
			refuseWeb(w, http.StatusForbidden, "this is a node's local API, and answers only to its own machine's address")
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" {
			if !slices.Contains(origins, origin) {
				refuseWeb(w, http.StatusForbidden, "pages from "+origin+" may not use this node: its owner has not listed that origin with --web-origin")
				return
			}
			h := w.Header()
			h.Set("Access-Control-Allow-Origin", origin)
			h.Add("Vary", "Origin")
			h.Set("Access-Control-Expose-Headers", "Grpc-Status, Grpc-Message, Grpc-Status-Details-Bin, Connect-Content-Encoding")
			if r.Method == http.MethodOptions {
				// A browser asking leave before it asks in earnest.
				h.Set("Access-Control-Allow-Methods", "POST, PUT, GET, OPTIONS")
				h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Connect-Protocol-Version, Connect-Timeout-Ms, Grpc-Timeout, X-Grpc-Web, X-User-Agent")
				h.Set("Access-Control-Max-Age", "600")
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
		shown := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(shown), []byte(cfg.Token)) != 1 {
			refuseWeb(w, http.StatusUnauthorized, "every call from a web page needs the node's API token, which is in api.token in its data directory")
			return
		}
		mux.ServeHTTP(w, r)
	})
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
