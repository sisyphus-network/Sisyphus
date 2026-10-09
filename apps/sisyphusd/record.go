package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/ipfs/go-cid"

	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/access"
	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/api"
	"github.com/sisyphus-network/Sisyphus/packages/identity"
	"github.com/sisyphus-network/Sisyphus/packages/names"
)

// With --publish-record a coordinator keeps a description of itself under
// its own name: a small JSON document, stored as a file like any other,
// which the node's name is pointed at. Whoever resolves the name gets the
// description as it now stands.

// nodeRecord is that description. It says who the pool's members are, which
// is why a node publishes it only when its owner asks.
type nodeRecord struct {
	ID      string `json:"id"`
	Version string `json:"version"`
	// Workloads are the kinds of job the node takes.
	Workloads []string       `json:"workloads"`
	Members   []recordMember `json:"members"`
}

type recordMember struct {
	ID   string `json:"id"`
	Role string `json:"role"`
}

// describeNode writes a node's description. The same facts always give the
// same bytes, so the description changes only when they do.
func describeNode(id, version string, workloads []string, members []access.Member) []byte {
	record := nodeRecord{ID: id, Version: version, Workloads: workloads, Members: []recordMember{}}
	for _, member := range members {
		record.Members = append(record.Members, recordMember{ID: member.ID, Role: string(member.Role)})
	}
	document, _ := json.MarshalIndent(record, "", "  ") // strings always encode
	return append(document, '\n')
}

var (
	// recordLifetime is how long each record a node publishes of itself is
	// good for. It publishes another when half of that has gone.
	recordLifetime = names.DefaultLifetime
	// recordCheck is how often a node looks at whether its description has
	// changed, or its record needs renewing.
	recordCheck = 30 * time.Second
)

// recordPins owns the pins that keep a node's descriptions in its store for
// as long as a record points at them.
const recordPins = "node-record"

// recordStore is the part of a store a node keeps its description in.
type recordStore interface {
	Put(ctx context.Context, r io.Reader) (cid.Cid, error)
	Pin(ctx context.Context, owner string, expires time.Time, cids ...cid.Cid) error
}

// ownNames is the part of a node's names it publishes itself among.
type ownNames interface {
	Held(id string) (*names.Record, error)
	Publish(record *names.Record, now time.Time) error
}

// recordKeeper keeps a node's description published under its name.
type recordKeeper struct {
	ident *identity.Identity
	// describe returns the description as it now stands.
	describe func() []byte
	store    recordStore
	names    ownNames
	log      *slog.Logger

	// published is the description last published, and at the content ID
	// it was stored under.
	published []byte
	at        cid.Cid
}

// run publishes the description, and again whenever it changes or its
// record is half way to expiring, until ctx ends.
func (k *recordKeeper) run(ctx context.Context) {
	ticker := time.NewTicker(recordCheck)
	defer ticker.Stop()
	for {
		if err := k.publish(ctx, time.Now()); err != nil {
			k.log.Warn("could not publish this node's record under its name; trying again", "in", recordCheck.String(), "error", err)
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			return
		}
	}
}

// publish points the node's name at its description, unless it does so
// already and will for a good while yet. A node has the one name, so
// whatever else was published under it is replaced.
func (k *recordKeeper) publish(ctx context.Context, now time.Time) error {
	document := k.describe()
	var sequence uint64
	held, err := k.names.Held(k.ident.ID())
	switch {
	case errors.Is(err, api.ErrNoRecord):
	case err != nil:
		return err
	case bytes.Equal(document, k.published) && held.Value == k.at && held.Expires.Sub(now) > recordLifetime/2:
		return nil
	default:
		sequence = held.Sequence + 1
	}
	stored, err := k.store.Put(ctx, bytes.NewReader(document))
	if err != nil {
		return fmt.Errorf("store the record: %w", err)
	}
	// The description is kept for as long as the record that points at it
	// is good. An earlier one is let go when its own record would have
	// expired.
	record := names.Make(k.ident, stored, sequence, recordLifetime, names.DefaultTTL, now)
	if err := k.store.Pin(ctx, recordPins, record.Expires, stored); err != nil {
		return fmt.Errorf("keep the record: %w", err)
	}
	if err := k.names.Publish(record, now); err != nil {
		return err
	}
	k.published, k.at = document, stored
	k.log.Info("published this node's record under its name", "name", record.Name, "cid", stored.String(), "sequence", sequence)
	return nil
}
