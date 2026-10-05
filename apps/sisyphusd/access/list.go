// Package access decides what each node that connects to this one may do.
//
// A node is identified by its key, through TLS. This package keeps the list
// of nodes that have been admitted and in what role, issues the invitations
// by which new ones are admitted, and enforces, call by call, what each role
// may use.
package access

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"sync"
	"time"
)

// Role is what an admitted node is allowed to be.
type Role string

const (
	// Owner is the node itself: its own worker and its own command line.
	// It may do anything. It is never stored; it is whoever holds the
	// node's key.
	Owner Role = "owner"
	// Worker may take tasks and move the blobs they need.
	Worker Role = "worker"
	// Client may submit and inspect jobs and manage stored data.
	Client Role = "client"
)

// Member is a node that has been admitted.
type Member struct {
	ID     string    `json:"id"`
	Role   Role      `json:"role"`
	Joined time.Time `json:"joined"`
}

type invite struct {
	role    Role
	expires time.Time
}

// List is the set of nodes admitted to this one. It is safe for concurrent
// use and saves itself to a file on every change.
type List struct {
	owner string
	file  string

	mu      sync.Mutex
	members map[string]Member
	// invites are kept in memory only: an invitation not used before the
	// node restarts is void.
	invites map[string]invite
}

// Open loads the list kept in file, or starts an empty one if there is no
// such file. ownerID is this node's own ID.
func Open(file, ownerID string) (*List, error) {
	l := &List{owner: ownerID, file: file, members: make(map[string]Member), invites: make(map[string]invite)}
	data, err := os.ReadFile(file)
	if errors.Is(err, os.ErrNotExist) {
		return l, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read access list: %w", err)
	}
	var members []Member
	if err := json.Unmarshal(data, &members); err != nil {
		return nil, fmt.Errorf("access list %s: %w", file, err)
	}
	for _, m := range members {
		l.members[m.ID] = m
	}
	return l, nil
}

// Role returns the role of the node with the given ID, and whether it has
// one at all.
func (l *List) Role(id string) (Role, bool) {
	if id == l.owner {
		return Owner, true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	m, ok := l.members[id]
	return m.Role, ok
}

// Invite returns a token that admits whichever node first presents it, in
// the given role, if it does so before ttl has passed.
func (l *List) Invite(role Role, ttl time.Duration, now time.Time) (string, error) {
	if role != Worker && role != Client {
		return "", fmt.Errorf("cannot invite a node as %q: the roles are %q and %q", role, Worker, Client)
	}
	var raw [16]byte
	rand.Read(raw[:]) // never fails; see crypto/rand
	token := hex.EncodeToString(raw[:])
	l.mu.Lock()
	defer l.mu.Unlock()
	l.invites[token] = invite{role: role, expires: now.Add(ttl)}
	return token, nil
}

// ErrBadInvite reports a token that is unknown, already used or expired.
// The three are not told apart, so that a guesser learns nothing.
var ErrBadInvite = errors.New("invitation is not valid")

// Redeem admits the node with the given ID in the role its token was issued
// for. A token works once.
func (l *List) Redeem(token, id string, now time.Time) (Role, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	inv, ok := l.invites[token]
	if !ok {
		return "", ErrBadInvite
	}
	delete(l.invites, token)
	if now.After(inv.expires) {
		return "", ErrBadInvite
	}
	l.members[id] = Member{ID: id, Role: inv.role, Joined: now}
	return inv.role, l.saveLocked()
}

// Admit admits a node directly, without an invitation.
func (l *List) Admit(id string, role Role, now time.Time) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.members[id] = Member{ID: id, Role: role, Joined: now}
	return l.saveLocked()
}

// Remove takes a node off the list. It reports whether the node was on it.
func (l *List) Remove(id string) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.members[id]; !ok {
		return false, nil
	}
	delete(l.members, id)
	return true, l.saveLocked()
}

// Members returns the admitted nodes, ordered by ID.
func (l *List) Members() []Member {
	l.mu.Lock()
	defer l.mu.Unlock()
	members := make([]Member, 0, len(l.members))
	for _, m := range l.members {
		members = append(members, m)
	}
	sort.Slice(members, func(i, j int) bool { return members[i].ID < members[j].ID })
	return members
}

func (l *List) saveLocked() error {
	if l.file == "" {
		return nil
	}
	members := make([]Member, 0, len(l.members))
	for _, m := range l.members {
		members = append(members, m)
	}
	sort.Slice(members, func(i, j int) bool { return members[i].ID < members[j].ID })
	data, _ := json.MarshalIndent(members, "", "\t") // plain strings and times always encode
	// Write beside the file and rename, so a crash leaves the old list or
	// the new one and never half of either.
	tmp := l.file + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("save access list: %w", err)
	}
	if err := os.Rename(tmp, l.file); err != nil {
		return fmt.Errorf("save access list: %w", err)
	}
	return nil
}
