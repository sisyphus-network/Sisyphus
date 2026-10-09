package pinning

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client asks a pinning service to keep data: this node's own service on
// another node, somebody's IPFS Cluster, or a commercial one. Whatever it
// sends, the service learns.
type Client struct {
	endpoint string
	key      string
	http     *http.Client
}

// NewClient returns a client of the service at the given address, such as
// https://api.pinata.cloud/psa, which is shown key with every request.
func NewClient(service, key string) (*Client, error) {
	at, err := url.Parse(service)
	if err != nil || (at.Scheme != "http" && at.Scheme != "https") || at.Host == "" {
		return nil, fmt.Errorf("a pinning service's address looks like https://host/path, not %q", service)
	}
	// The address is the one the paths of the API are added to, though
	// some give it with /pins on the end.
	endpoint := strings.TrimSuffix(strings.TrimRight(service, "/"), "/pins")
	return &Client{endpoint: endpoint, key: key, http: &http.Client{}}, nil
}

// call makes one request of the service and decodes its answer into into,
// if there is anything to decode it into.
func (c *Client) call(ctx context.Context, method, path string, send, into any) error {
	var body io.Reader
	if send != nil {
		encoded, _ := json.Marshal(send) // strings, lists and maps of them always encode
		body = bytes.NewReader(encoded)
	}
	req, _ := http.NewRequestWithContext(ctx, method, c.endpoint+path, body) // the address was checked when the client was made
	req.Header.Set("Authorization", "Bearer "+c.key)
	req.Header.Set("Accept", "application/json")
	if send != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("ask the pinning service: %w", err)
	}
	defer res.Body.Close()
	answer, _ := io.ReadAll(io.LimitReader(res.Body, 16<<20))
	if res.StatusCode < 200 || res.StatusCode > 299 {
		var f failure
		if json.Unmarshal(answer, &f) != nil || f.Error.Reason == "" {
			f.Error.Reason = strings.TrimSpace(string(answer[:min(len(answer), 200)]))
		}
		return fmt.Errorf("the pinning service refused (HTTP %d): %s", res.StatusCode, strings.TrimSpace(f.Error.Reason+" "+f.Error.Details))
	}
	if into == nil {
		return nil
	}
	if err := json.Unmarshal(answer, into); err != nil {
		return fmt.Errorf("the pinning service's answer could not be read: %w", err)
	}
	return nil
}

// Add asks the service to keep what pin names, and returns what the
// service makes of the request.
func (c *Client) Add(ctx context.Context, pin Pin) (Status, error) {
	var status Status
	err := c.call(ctx, http.MethodPost, "/pins", pin, &status)
	return status, err
}

// Status asks how far the service has got with a request.
func (c *Client) Status(ctx context.Context, requestID string) (Status, error) {
	var status Status
	err := c.call(ctx, http.MethodGet, "/pins/"+url.PathEscape(requestID), nil, &status)
	return status, err
}

// Remove has the service forget a request and stop keeping its data.
func (c *Client) Remove(ctx context.Context, requestID string) error {
	return c.call(ctx, http.MethodDelete, "/pins/"+url.PathEscape(requestID), nil, nil)
}

// List returns every request the service has for this key, in whatever
// state, newest first.
func (c *Client) List(ctx context.Context) ([]Status, error) {
	var all []Status
	seen := make(map[string]bool)
	query := url.Values{"status": {strings.Join([]string{Queued, Pinning, Pinned, Failed}, ",")}, "limit": {"1000"}}
	for {
		var page results
		if err := c.call(ctx, http.MethodGet, "/pins?"+query.Encode(), nil, &page); err != nil {
			return nil, err
		}
		fresh := 0
		for _, status := range page.Results {
			if !seen[status.RequestID] {
				seen[status.RequestID] = true
				all = append(all, status)
				fresh++
			}
		}
		// A service gives a page at a time, and the next page is of what was
		// made before the oldest on this one. A page with nothing new on it
		// is the last, whatever the service says is still to come.
		if fresh == 0 || page.Count <= len(page.Results) {
			return all, nil
		}
		query.Set("before", page.Results[len(page.Results)-1].Created.Format(time.RFC3339Nano))
	}
}

// Wait asks after a request every so often until the service has pinned
// it or failed to, and returns it as it then stands.
func (c *Client) Wait(ctx context.Context, requestID string, every time.Duration) (Status, error) {
	for {
		status, err := c.Status(ctx, requestID)
		if err != nil {
			return status, err
		}
		switch status.Status {
		case Pinned, Failed:
			return status, nil
		case Queued, Pinning:
		default:
			return status, errors.New("the pinning service gave a status that is not one: " + status.Status)
		}
		select {
		case <-time.After(every):
		case <-ctx.Done():
			return status, ctx.Err()
		}
	}
}
