// Package kubo runs and talks to Kubo, the reference IPFS implementation, as
// a separate process beside sisyphusd.
package kubo

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// ErrNotFound reports that Kubo does not have a block.
var ErrNotFound = errors.New("block not found")

// Client calls a Kubo daemon's RPC API, the one the ipfs command itself
// uses. Blocks and CIDs pass through it as plain bytes and strings.
type Client struct {
	// mu is held shared for the length of a call, and exclusively while the
	// daemon behind the client is replaced.
	mu   sync.RWMutex
	base string
	http *http.Client
	// secret is what the daemon's API wants shown, if it wants anything.
	secret string
	// PeerTimeout is how long BlockGet lets Kubo look among its peers for a
	// block it does not hold before reporting it not found. Zero leaves it
	// to the caller's context.
	PeerTimeout time.Duration
}

// NewClient returns a client for the Kubo API listening at addr, a host and
// port such as "127.0.0.1:5001".
func NewClient(addr string) *Client {
	return &Client{base: "http://" + addr + "/api/v0/", http: &http.Client{}}
}

// Address returns the host and port the Kubo API listens at, for another
// program that is to call it. It changes when the daemon is restarted.
func (c *Client) Address() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return strings.TrimSuffix(strings.TrimPrefix(c.base, "http://"), "/api/v0/")
}

// call makes one API call and returns its response body, which the caller
// must close. Kubo reports failures as a JSON object with an HTTP error
// status; those come back as errors.
func (c *Client) call(ctx context.Context, command string, args url.Values, body io.Reader, contentType string) (io.ReadCloser, error) {
	c.mu.RLock()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+command+"?"+args.Encode(), body)
	if err != nil {
		c.mu.RUnlock()
		return nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if c.secret != "" {
		req.SetBasicAuth(APIUser, c.secret)
	}
	res, err := c.http.Do(req)
	if err != nil {
		c.mu.RUnlock()
		return nil, fmt.Errorf("kubo %s: %w", command, err)
	}
	if res.StatusCode == http.StatusOK {
		// The call lasts until its reply has been read, and the daemon must
		// not be replaced before then.
		return &heldBody{ReadCloser: res.Body, release: c.mu.RUnlock}, nil
	}
	defer c.mu.RUnlock()
	defer res.Body.Close()
	var failure struct{ Message string }
	text, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
	if json.Unmarshal(text, &failure) != nil || failure.Message == "" {
		failure.Message = strings.TrimSpace(string(text))
	}
	// Kubo words "nobody has it" several ways, one of them being that the
	// time it was given to look ran out.
	if strings.Contains(failure.Message, "not found") || strings.Contains(failure.Message, "could not find") ||
		strings.Contains(failure.Message, "context deadline exceeded") {
		return nil, fmt.Errorf("kubo %s: %w", command, ErrNotFound)
	}
	return nil, fmt.Errorf("kubo %s: %s (HTTP %d)", command, failure.Message, res.StatusCode)
}

// heldBody is a reply that keeps its client's daemon from being replaced
// until it is closed.
type heldBody struct {
	io.ReadCloser
	once    sync.Once
	release func()
}

func (h *heldBody) Close() error {
	err := h.ReadCloser.Close()
	h.once.Do(h.release)
	return err
}

// callJSON makes a call whose response is one JSON object and decodes it.
func (c *Client) callJSON(ctx context.Context, command string, args url.Values, into any) error {
	body, err := c.call(ctx, command, args, nil, "")
	if err != nil {
		return err
	}
	defer body.Close()
	if err := json.NewDecoder(body).Decode(into); err != nil {
		return fmt.Errorf("kubo %s: reading the reply: %w", command, err)
	}
	return nil
}

// ID returns the peer ID of the Kubo node.
func (c *Client) ID(ctx context.Context) (string, error) {
	var reply struct{ ID string }
	err := c.callJSON(ctx, "id", nil, &reply)
	return reply.ID, err
}

// Addresses returns the addresses other peers can reach the Kubo node at,
// each ending in its peer ID. An offline node has none.
func (c *Client) Addresses(ctx context.Context) ([]string, error) {
	var reply struct{ Addresses []string }
	err := c.callJSON(ctx, "id", nil, &reply)
	return reply.Addresses, err
}

// Connect connects the Kubo node to the peer at the given address, which
// must end in the peer's ID.
func (c *Client) Connect(ctx context.Context, address string) error {
	body, err := c.call(ctx, "swarm/connect", url.Values{"arg": {address}}, nil, "")
	if err != nil {
		return err
	}
	return body.Close()
}

// PeerAddresses returns, for every peer the Kubo node is connected to, the
// addresses that peer listens on, each ending in the peer's ID. They are
// what a third node needs to connect to those peers itself.
func (c *Client) PeerAddresses(ctx context.Context) ([]string, error) {
	connected, err := c.Peers(ctx)
	if err != nil {
		return nil, err
	}
	var reply struct{ Addrs map[string][]string }
	if err := c.callJSON(ctx, "swarm/addrs", nil, &reply); err != nil {
		return nil, err
	}
	var addresses []string
	for _, id := range connected {
		for _, address := range reply.Addrs[id] {
			addresses = append(addresses, address+"/p2p/"+id)
		}
	}
	return addresses, nil
}

// Peers returns the IDs of the peers the Kubo node is connected to.
func (c *Client) Peers(ctx context.Context) ([]string, error) {
	var reply struct{ Peers []struct{ Peer string } }
	if err := c.callJSON(ctx, "swarm/peers", nil, &reply); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(reply.Peers))
	for _, p := range reply.Peers {
		ids = append(ids, p.Peer)
	}
	return ids, nil
}

// PutName hands the node a signed IPNS record for a name, which is a peer
// ID, to keep and to pass to the peers it would look the name up among. Any
// node may hand over any name's record: Kubo checks the signature itself,
// and takes no record older than one it has. With no peers to pass it to,
// the node keeps it for when it has.
func (c *Client) PutName(ctx context.Context, name string, record []byte) error {
	var form bytes.Buffer
	writer := multipart.NewWriter(&form)
	part, _ := writer.CreateFormFile("value-file", "record") // writing to a buffer cannot fail
	part.Write(record)
	writer.Close()

	args := url.Values{"arg": {"/ipns/" + name}, "allow-offline": {"true"}}
	body, err := c.call(ctx, "routing/put", args, &form, writer.FormDataContentType())
	if err != nil {
		return err
	}
	defer body.Close()
	// Kubo reports each peer it tries as it goes; the call is over when it
	// stops.
	if _, err := io.Copy(io.Discard, body); err != nil {
		return fmt.Errorf("kubo routing/put: %w", err)
	}
	return nil
}

// BlockPut stores one block and returns the CID Kubo gave it. codec is the
// block's format, "raw", "dag-pb" or "dag-cbor"; the CID is version 1 with a SHA-256
// hash.
func (c *Client) BlockPut(ctx context.Context, codec string, data []byte) (string, error) {
	var form bytes.Buffer
	writer := multipart.NewWriter(&form)
	part, _ := writer.CreateFormFile("data", "block") // writing to a buffer cannot fail
	part.Write(data)
	writer.Close()

	args := url.Values{"cid-codec": {codec}, "mhtype": {"sha2-256"}, "pin": {"false"}}
	body, err := c.call(ctx, "block/put", args, &form, writer.FormDataContentType())
	if err != nil {
		return "", err
	}
	defer body.Close()
	var reply struct{ Key string }
	if err := json.NewDecoder(body).Decode(&reply); err != nil {
		return "", fmt.Errorf("kubo block/put: reading the reply: %w", err)
	}
	return reply.Key, nil
}

// BlockGet returns a block's bytes, or ErrNotFound. Unless the daemon is
// offline, a block it does not hold is looked for among its peers first,
// for PeerTimeout or as long as ctx allows.
func (c *Client) BlockGet(ctx context.Context, id string) ([]byte, error) {
	args := url.Values{"arg": {id}}
	if c.PeerTimeout > 0 {
		args.Set("timeout", c.PeerTimeout.String())
	}
	body, err := c.call(ctx, "block/get", args, nil, "")
	if err != nil {
		return nil, err
	}
	defer body.Close()
	data, err := io.ReadAll(body)
	if err != nil {
		return nil, fmt.Errorf("kubo block/get: %w", err)
	}
	return data, nil
}

// BlockSize returns the size of a block the daemon itself holds, or
// ErrNotFound. It never asks peers.
func (c *Client) BlockSize(ctx context.Context, id string) (int, error) {
	var reply struct{ Size int }
	err := c.callJSON(ctx, "block/stat", url.Values{"arg": {id}, "offline": {"true"}}, &reply)
	return reply.Size, err
}

// BlockRemove deletes a block. Removing one that is not there is not an
// error.
func (c *Client) BlockRemove(ctx context.Context, id string) error {
	body, err := c.call(ctx, "block/rm", url.Values{"arg": {id}, "force": {"true"}}, nil, "")
	if err != nil {
		return err
	}
	defer body.Close()
	// Kubo answers 200 and reports a failed removal in the body.
	var reply struct{ Error string }
	json.NewDecoder(body).Decode(&reply)
	if reply.Error != "" {
		return fmt.Errorf("kubo block/rm: %s", reply.Error)
	}
	return nil
}

// LocalBlocks calls visit with the CID of every block the daemon holds,
// until visit returns false or the blocks run out.
func (c *Client) LocalBlocks(ctx context.Context, visit func(id string) bool) error {
	body, err := c.call(ctx, "refs/local", nil, nil, "")
	if err != nil {
		return err
	}
	defer body.Close()
	lines := bufio.NewScanner(body)
	for lines.Scan() {
		var entry struct{ Ref, Err string }
		if err := json.Unmarshal(lines.Bytes(), &entry); err != nil {
			return fmt.Errorf("kubo refs/local: reading the reply: %w", err)
		}
		if entry.Err != "" {
			return fmt.Errorf("kubo refs/local: %s", entry.Err)
		}
		if !visit(entry.Ref) {
			return nil
		}
	}
	if err := lines.Err(); err != nil {
		return fmt.Errorf("kubo refs/local: %w", err)
	}
	return nil
}

// Pinned calls visit with the CID of every block Kubo itself has pinned,
// directly or as part of something pinned.
func (c *Client) Pinned(ctx context.Context, visit func(id string)) error {
	var reply struct{ Keys map[string]json.RawMessage }
	if err := c.callJSON(ctx, "pin/ls", url.Values{"type": {"all"}}, &reply); err != nil {
		return err
	}
	for id := range reply.Keys {
		visit(id)
	}
	return nil
}

// RepoSize returns how many bytes the daemon's repository occupies.
func (c *Client) RepoSize(ctx context.Context) (uint64, error) {
	var reply struct{ RepoSize uint64 }
	err := c.callJSON(ctx, "repo/stat", url.Values{"size-only": {"true"}}, &reply)
	return reply.RepoSize, err
}
