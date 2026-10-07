// Package access decides what each node that connects to this one may do.
//
// A node is identified by its key, through TLS. This package keeps the list
// of nodes that have been admitted and in what role, issues the invitations
// by which new ones are admitted, and enforces, call by call, what each role
// may use.
package access

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/sisyphus-network/Sisyphus/packages/nodedb"
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

// Store is where a list keeps its members and the invitations it has
// issued. A nodedb.DB is one.
type Store interface {
	Members() ([]nodedb.Member, error)
	SaveMember(nodedb.Member) error
	DeleteMember(id string) error
	SaveInvitation(tokenHash, role string, expires, now time.Time) error
	TakeInvitation(tokenHash string) (role string, expires time.Time, found bool, err error)
}

// List is the set of nodes admitted to this one. It is safe for concurrent
// use and records every change in its store before the change takes effect.
type List struct {
	owner string
	store Store

	mu      sync.Mutex
	members map[string]Member
	// changed, if set, is called with the ID of each node admitted or
	// removed, after the change has taken effect.
	changed func(id string)
}

// OnChange has fn called with the ID of each node admitted or removed from
// now on, after the fact. It must be set before the list is shared.
func (l *List) OnChange(fn func(id string)) {
	l.changed = fn
}

func (l *List) tell(id string) {
	if l.changed != nil {
		l.changed(id)
	}
}

// Open loads the list kept in store. ownerID is this node's own ID.
func Open(store Store, ownerID string) (*List, error) {
	l := &List{owner: ownerID, store: store, members: make(map[string]Member)}
	saved, err := store.Members()
	if err != nil {
		return nil, err
	}
	for _, m := range saved {
		l.members[m.ID] = Member{ID: m.ID, Role: Role(m.Role), Joined: m.Joined}
	}
	return l, nil
}

// InMemory returns a store that lasts as long as the process does.
func InMemory() Store {
	return &memory{invitations: make(map[string]memoryInvitation)}
}

type memory struct {
	mu          sync.Mutex
	invitations map[string]memoryInvitation
}

type memoryInvitation struct {
	role    string
	expires time.Time
}

func (*memory) Members() ([]nodedb.Member, error) { return nil, nil }
func (*memory) SaveMember(nodedb.Member) error    { return nil }
func (*memory) DeleteMember(string) error         { return nil }

func (m *memory) SaveInvitation(tokenHash, role string, expires, _ time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.invitations[tokenHash] = memoryInvitation{role: role, expires: expires}
	return nil
}

func (m *memory) TakeInvitation(tokenHash string) (string, time.Time, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	inv, found := m.invitations[tokenHash]
	delete(m.invitations, tokenHash)
	return inv.role, inv.expires, found, nil
}

// Import admits the members listed in file, which is where earlier versions
// kept the list, and renames the file so that it is imported once. A file
// that is not there is nothing to do.
func (l *List) Import(file string) error {
	data, err := os.ReadFile(file)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read access list: %w", err)
	}
	var members []Member
	if err := json.Unmarshal(data, &members); err != nil {
		return fmt.Errorf("access list %s: %w", file, err)
	}
	for _, m := range members {
		if err := l.Admit(m.ID, m.Role, m.Joined); err != nil {
			return fmt.Errorf("import access list %s: %w", file, err)
		}
	}
	if err := os.Rename(file, file+".imported"); err != nil {
		return fmt.Errorf("import access list: %w", err)
	}
	return nil
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
	if err := l.store.SaveInvitation(hashed(token), string(role), now.Add(ttl), now); err != nil {
		return "", err
	}
	return token, nil
}

// hashed is what an invitation is recorded under, so that the record alone
// admits nobody.
func hashed(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// ErrBadInvite reports a token that is unknown, already used or expired.
// The three are not told apart, so that a guesser learns nothing.
var ErrBadInvite = errors.New("invitation is not valid")

// Redeem admits the node with the given ID in the role its token was issued
// for. A token works once.
func (l *List) Redeem(token, id string, now time.Time) (Role, error) {
	role, expires, found, err := l.store.TakeInvitation(hashed(token))
	if err != nil {
		return "", err
	}
	if !found || now.After(expires) {
		return "", ErrBadInvite
	}
	return Role(role), l.Admit(id, Role(role), now)
}

// Admit admits a node directly, without an invitation.
func (l *List) Admit(id string, role Role, now time.Time) error {
	l.mu.Lock()
	err := l.store.SaveMember(nodedb.Member{ID: id, Role: string(role), Joined: now})
	if err == nil {
		l.members[id] = Member{ID: id, Role: role, Joined: now}
	}
	l.mu.Unlock()
	if err == nil {
		l.tell(id)
	}
	return err
}

// Remove takes a node off the list. It reports whether the node was on it.
func (l *List) Remove(id string) (bool, error) {
	removed, err := l.remove(id)
	if removed {
		l.tell(id)
	}
	return removed, err
}

func (l *List) remove(id string) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.members[id]; !ok {
		return false, nil
	}
	if err := l.store.DeleteMember(id); err != nil {
		return false, err
	}
	delete(l.members, id)
	return true, nil
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
