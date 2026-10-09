// Package ipfscluster runs and talks to an IPFS Cluster peer, as a separate
// process beside sisyphusd and the Kubo daemon it pins on.
package ipfscluster

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
)

// apiUser is the name a client gives a peer's REST API. The password is what
// keeps other programs on the machine out.
const apiUser = "sisyphusd"

// Client calls a cluster peer's REST API, the one ipfs-cluster-ctl uses.
type Client struct {
	// mu guards where the peer is, which changes when it is restarted.
	mu       sync.Mutex
	base     string
	password string
	http     *http.Client
}

// NewClient returns a client for the REST API listening at addr, a host and
// port such as "127.0.0.1:9094", which asks for the given password.
func NewClient(addr, password string) *Client {
	return &Client{base: "http://" + addr, password: password, http: &http.Client{}}
}

// point aims the client at a peer that has just started.
func (c *Client) point(addr, password string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.base, c.password = "http://"+addr, password
}

// call makes one API call and hands its reply to read. The peer reports a
// failure as a JSON object with an HTTP error status; those come back as
// errors, and one saying the thing asked about does not exist as errNotFound.
func (c *Client) call(ctx context.Context, method, path string, query url.Values, read func(io.Reader) error) error {
	c.mu.Lock()
	base, password := c.base, c.password
	c.mu.Unlock()
	what := "ipfs-cluster " + method + " " + path
	req, err := http.NewRequestWithContext(ctx, method, base+path+"?"+query.Encode(), nil)
	if err != nil {
		return err
	}
	req.SetBasicAuth(apiUser, password)
	res, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	defer res.Body.Close()
	if res.StatusCode >= 300 {
		var failure struct{ Message string }
		text, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
		if json.Unmarshal(text, &failure) != nil || failure.Message == "" {
			failure.Message = strings.TrimSpace(string(text))
		}
		err := fmt.Errorf("%s: %s (HTTP %d)", what, failure.Message, res.StatusCode)
		if res.StatusCode == http.StatusNotFound {
			err = fmt.Errorf("%w: %w", errNotFound, err)
		}
		return err
	}
	if err := read(res.Body); err != nil {
		return fmt.Errorf("%s: reading the reply: %w", what, err)
	}
	// A list is sent as it is made. If making it fails part-way the peer
	// can only say so after what it has already sent.
	if failure := res.Trailer.Get("X-Stream-Error"); failure != "" {
		return fmt.Errorf("%s: %s", what, failure)
	}
	return nil
}

// errNotFound reports that the peer knows nothing of what was asked about.
var errNotFound = errors.New("not found")

// one reads a reply that is a single JSON object.
func one[T any](into *T) func(io.Reader) error {
	return func(body io.Reader) error { return json.NewDecoder(body).Decode(into) }
}

// each reads a reply that is one JSON object after another.
func each[T any](visit func(T)) func(io.Reader) error {
	return func(body io.Reader) error {
		decoder := json.NewDecoder(body)
		for {
			var item T
			if err := decoder.Decode(&item); errors.Is(err, io.EOF) {
				return nil
			} else if err != nil {
				return err
			}
			visit(item)
		}
	}
}

// Peer is one member of a cluster, as another member sees it.
type Peer struct {
	ID string `json:"id"`
	// Name is the label the peer's owner gave it.
	Name string `json:"peername"`
	// Addresses are where the peer listens for the other members, each
	// ending in /p2p/ and its ID.
	Addresses []string `json:"addresses"`
	// Error says why the peer could not be asked about itself, if it could
	// not.
	Error string `json:"error"`
}

// ID describes the peer the client talks to.
func (c *Client) ID(ctx context.Context) (Peer, error) {
	var self Peer
	err := c.call(ctx, http.MethodGet, "/id", nil, one(&self))
	return self, err
}

// Listening returns where on this machine the peer accepts the other
// members, as a host and port.
func (c *Client) Listening(ctx context.Context) (string, error) {
	self, err := c.ID(ctx)
	if err != nil {
		return "", err
	}
	for _, address := range self.Addresses {
		parts := strings.Split(address, "/")
		if len(parts) >= 5 && parts[1] == "ip4" && parts[3] == "tcp" && net.ParseIP(parts[2]).IsLoopback() {
			return net.JoinHostPort(parts[2], parts[4]), nil
		}
	}
	return "", fmt.Errorf("the cluster peer is not listening on this machine's loopback address (it listens on %v)", self.Addresses)
}

// Peers lists the members of the cluster that the peer has heard from
// lately, itself among them.
func (c *Client) Peers(ctx context.Context) ([]Peer, error) {
	var peers []Peer
	err := c.call(ctx, http.MethodGet, "/peers", nil, each(func(p Peer) { peers = append(peers, p) }))
	return peers, err
}

// Pin is something the cluster has been asked to keep.
type Pin struct {
	CID string `json:"cid"`
	// Name is a label given when pinning.
	Name string `json:"name"`
	// Min and Max bound how many members hold it: the cluster aims for Max,
	// and looks for new holders when fewer than Min are left.
	Min int `json:"replication_factor_min"`
	Max int `json:"replication_factor_max"`
	// Allocations are the IDs of the members chosen to hold it.
	Allocations []string `json:"allocations"`
}

// PinOptions says how the cluster should keep something.
type PinOptions struct {
	Name     string
	Min, Max int
	// Prefer lists members to choose as holders before any other.
	Prefer []string
}

// Pin has the cluster keep the content with the given CID, or changes how it
// keeps it. The members chosen fetch it in their own time; Pin does not wait
// for them. It fails if fewer than opts.Min members can take it.
func (c *Client) Pin(ctx context.Context, id string, opts PinOptions) (Pin, error) {
	query := url.Values{
		"name":            {opts.Name},
		"mode":            {"recursive"},
		"replication-min": {strconv.Itoa(opts.Min)},
		"replication-max": {strconv.Itoa(opts.Max)},
	}
	if len(opts.Prefer) > 0 {
		query.Set("user-allocations", strings.Join(opts.Prefer, ","))
	}
	var pinned Pin
	err := c.call(ctx, http.MethodPost, "/pins/"+url.PathEscape(id), query, one(&pinned))
	return pinned, err
}

// Unpin has the cluster stop keeping something. Unpinning what is not pinned
// is not an error.
func (c *Client) Unpin(ctx context.Context, id string) error {
	err := c.call(ctx, http.MethodDelete, "/pins/"+url.PathEscape(id), nil, func(io.Reader) error { return nil })
	if errors.Is(err, errNotFound) {
		return nil
	}
	return err
}

// Pins lists everything the cluster has been asked to keep.
func (c *Client) Pins(ctx context.Context) ([]Pin, error) {
	var pins []Pin
	err := c.call(ctx, http.MethodGet, "/allocations", url.Values{"filter": {"pin"}}, each(func(p Pin) { pins = append(pins, p) }))
	return pins, err
}

// Pinned is what a member reports of a pin it holds all of, and Remote of
// one it was not chosen to hold.
const (
	Pinned = "pinned"
	Remote = "remote"
)

// Status says how each member stands with one pin.
type Status struct {
	CID  string `json:"cid"`
	Name string `json:"name"`
	// Members is keyed by member ID. A member that was not chosen to hold
	// the pin reports "remote".
	Members map[string]MemberStatus `json:"peer_map"`
}

// MemberStatus is one member's part in a pin.
type MemberStatus struct {
	Name string `json:"peername"`
	// Status is "pinned" once the member holds all of the content, and
	// before that says how far it has got, or that it failed.
	Status string `json:"status"`
	Error  string `json:"error"`
}

// Holders returns the IDs of the members that hold all of the content.
func (s Status) Holders() []string {
	var holders []string
	for id, member := range s.Members {
		if member.Status == Pinned {
			holders = append(holders, id)
		}
	}
	return holders
}

// Status asks every member how it stands with every pin.
func (c *Client) Status(ctx context.Context) ([]Status, error) {
	var all []Status
	err := c.call(ctx, http.MethodGet, "/pins", nil, each(func(s Status) { all = append(all, s) }))
	return all, err
}
