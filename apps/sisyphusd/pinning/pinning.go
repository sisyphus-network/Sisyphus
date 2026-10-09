// Package pinning speaks the IPFS Pinning Service API, the HTTP interface
// by which one machine asks another to keep data: a node serves it, so that
// the ipfs program and other standard tools can pin to a pool, and a node
// uses it, to have its data kept by somebody else's service.
//
// The interface is specified at
// https://ipfs.github.io/pinning-services-api-spec/.
package pinning

import "time"

// Pin is a request to keep the data with a CID.
type Pin struct {
	CID string `json:"cid"`
	// Name is what the asker calls the data.
	Name string `json:"name,omitempty"`
	// Origins are addresses of peers that have the data, each ending in
	// /p2p/ and the peer's ID.
	Origins []string `json:"origins,omitempty"`
	// Meta is whatever else the asker wants kept with the request.
	Meta map[string]string `json:"meta,omitempty"`
}

// The states a request is in.
const (
	// Queued is waiting for its turn to be fetched.
	Queued = "queued"
	// Pinning is being fetched.
	Pinning = "pinning"
	// Pinned is held and will be kept.
	Pinned = "pinned"
	// Failed could not be had. Info says why.
	Failed = "failed"
)

// Status is a request and how far the service has got with it.
type Status struct {
	// RequestID names the request, for asking after it and removing it.
	RequestID string    `json:"requestid"`
	Status    string    `json:"status"`
	Created   time.Time `json:"created"`
	Pin       Pin       `json:"pin"`
	// Delegates are addresses of the service's own peers, which a peer
	// holding the data may connect to so that the service can fetch it.
	Delegates []string `json:"delegates"`
	// Info is whatever else the service says of the request.
	Info map[string]string `json:"info,omitempty"`
}

// Details is the entry of Info under which a service says more about a
// request's state, such as why it failed.
const Details = "status_details"

// results is one page of a listing. Count is how many requests matched,
// which may be more than the page holds.
type results struct {
	Count   int      `json:"count"`
	Results []Status `json:"results"`
}

// failure is how the API reports an error.
type failure struct {
	Error struct {
		Reason  string `json:"reason"`
		Details string `json:"details,omitempty"`
	} `json:"error"`
}

// The most the specification allows of each.
const (
	maxName    = 255
	maxOrigins = 20
	maxCIDs    = 10
	maxLimit   = 1000
)
