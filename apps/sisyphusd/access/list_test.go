package access

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sisyphus-network/Sisyphus/packages/nodedb"
)

var now = time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)

// open opens the list kept in the node database in file, or one kept in
// memory if no file is named.
func open(t *testing.T, file string) *List {
	t.Helper()
	store := InMemory()
	if file != "" {
		db, err := nodedb.Open(file)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })
		store = db
	}
	l, err := Open(store, "owner-id")
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func TestRoles(t *testing.T) {
	l := open(t, "")
	if err := l.Admit("worker-id", Worker, now); err != nil {
		t.Fatal(err)
	}
	if err := l.Admit("client-id", Client, now); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]Role{"owner-id": Owner, "worker-id": Worker, "client-id": Client} {
		if got, ok := l.Roles(id); !ok || !slices.Equal(got, []Role{want}) {
			t.Errorf("Roles(%s) = %q, %v; want %q", id, got, ok, want)
		}
	}
	if got, ok := l.Roles("stranger-id"); ok || got != nil {
		t.Errorf("a node never admitted has role %q", got)
	}
}

func TestAnInvitationAdmitsOneNodeOnce(t *testing.T) {
	l := open(t, "")
	token, err := l.Invite(Client, time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(token) != 32 {
		t.Errorf("token %q is not 128 bits of hex", token)
	}
	if again, _ := l.Invite(Client, time.Hour, now); again == token {
		t.Error("two invitations carry the same token")
	}

	role, err := l.Redeem(token, "first", now.Add(59*time.Minute))
	if err != nil || role != Client {
		t.Fatalf("Redeem = %q, %v", role, err)
	}
	if got, _ := l.Roles("first"); !slices.Equal(got, []Role{Client}) {
		t.Errorf("the redeeming node's roles are %q", got)
	}
	if _, err := l.Redeem(token, "second", now); !errors.Is(err, ErrBadInvite) {
		t.Errorf("a used invitation was accepted again: %v", err)
	}
	if _, ok := l.Roles("second"); ok {
		t.Error("a node was admitted on a used invitation")
	}
}

func TestInvitationsThatDoNotAdmit(t *testing.T) {
	l := open(t, "")
	if _, err := l.Redeem("never-issued", "node", now); !errors.Is(err, ErrBadInvite) {
		t.Errorf("an unknown token: %v", err)
	}

	token, err := l.Invite(Worker, time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.Redeem(token, "late", now.Add(time.Hour+time.Second)); !errors.Is(err, ErrBadInvite) {
		t.Errorf("an expired token: %v", err)
	}
	// Trying it late used it up.
	if _, err := l.Redeem(token, "late", now); !errors.Is(err, ErrBadInvite) {
		t.Errorf("an expired token worked when presented again as if in time: %v", err)
	}

	for _, role := range []Role{Owner, "", "admin"} {
		if _, err := l.Invite(role, time.Hour, now); err == nil {
			t.Errorf("issued an invitation for role %q", role)
		}
	}
}

func TestRemove(t *testing.T) {
	l := open(t, "")
	if err := l.Admit("worker-id", Worker, now); err != nil {
		t.Fatal(err)
	}
	if removed, err := l.Remove("worker-id"); err != nil || !removed {
		t.Errorf("Remove of a member = %v, %v", removed, err)
	}
	if _, ok := l.Roles("worker-id"); ok {
		t.Error("a removed node still has a role")
	}
	if removed, err := l.Remove("worker-id"); err != nil || removed {
		t.Errorf("Remove of a node not on the list = %v, %v", removed, err)
	}
}

func TestMembersAreListedInOrder(t *testing.T) {
	l := open(t, "")
	for _, id := range []string{"c", "a", "b"} {
		if err := l.Admit(id, Worker, now); err != nil {
			t.Fatal(err)
		}
	}
	var ids []string
	for _, m := range l.Members() {
		ids = append(ids, m.ID)
		if !m.Has(Worker) || !m.Joined.Equal(now) {
			t.Errorf("member %+v", m)
		}
	}
	if strings.Join(ids, "") != "abc" {
		t.Errorf("members listed as %v", ids)
	}
}

func TestTheListSurvivesReopening(t *testing.T) {
	file := filepath.Join(t.TempDir(), "node.db")
	l := open(t, file)
	token, err := l.Invite(Client, time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.Redeem(token, "client-id", now); err != nil {
		t.Fatal(err)
	}
	if err := l.Admit("worker-id", Worker, now); err != nil {
		t.Fatal(err)
	}
	if err := l.Admit("gone-id", Worker, now); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Remove("gone-id"); err != nil {
		t.Fatal(err)
	}
	unused, err := l.Invite(Worker, time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}

	reopened := open(t, file)
	members := reopened.Members()
	if len(members) != 2 || members[0].ID != "client-id" || !slices.Equal(members[0].Roles, []Role{Client}) || members[1].ID != "worker-id" || !members[1].Joined.Equal(now) {
		t.Errorf("after reopening: %+v", members)
	}
	// An invitation not yet used is still good, once, and one already used
	// is still spent.
	if role, err := reopened.Redeem(unused, "late-joiner", now); err != nil || role != Worker {
		t.Errorf("an invitation issued before a restart: %q, %v", role, err)
	}
	for _, spent := range []string{unused, token} {
		if _, err := reopened.Redeem(spent, "node", now); !errors.Is(err, ErrBadInvite) {
			t.Errorf("a used invitation was honoured after a restart: %v", err)
		}
	}
}

func TestTheRecordOfAnInvitationAdmitsNobody(t *testing.T) {
	file := filepath.Join(t.TempDir(), "node.db")
	l := open(t, file)
	token, err := l.Invite(Worker, time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{file, file + "-wal"} {
		if content, _ := os.ReadFile(name); strings.Contains(string(content), token) {
			t.Errorf("%s holds an invitation's token in the clear", filepath.Base(name))
		}
	}
}

// failing is a store in which nothing can be read or changed.
type failing struct{ Store }

var errStore = errors.New("the disk is full")

func (failing) Members() ([]nodedb.Member, error) { return nil, errStore }
func (failing) SaveMember(nodedb.Member) error    { return errStore }
func (failing) DeleteMember(string) error         { return errStore }
func (failing) SaveInvitation(string, string, time.Time, time.Time) error {
	return errStore
}
func (failing) TakeInvitation(string) (string, time.Time, bool, error) {
	return "", time.Time{}, false, errStore
}

// stuck is a store that hands out what it has but takes no changes.
type stuck struct{ Store }

func (stuck) SaveMember(nodedb.Member) error { return errStore }
func (stuck) DeleteMember(string) error      { return errStore }

func TestOpeningAListThatCannotBeRead(t *testing.T) {
	if _, err := Open(failing{}, "owner-id"); !errors.Is(err, errStore) {
		t.Errorf("Open: %v, want the store's error", err)
	}
}

func TestChangesThatCannotBeSavedAreReportedAndDoNotTakeEffect(t *testing.T) {
	l, err := Open(InMemory(), "owner-id")
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Admit("member", Worker, now); err != nil {
		t.Fatal(err)
	}
	token, _ := l.Invite(Worker, time.Hour, now)

	l.store = stuck{l.store}
	if err := l.Admit("another", Worker, now); !errors.Is(err, errStore) {
		t.Errorf("Admit: %v", err)
	}
	if _, ok := l.Roles("another"); ok {
		t.Error("a node whose admission could not be saved was admitted")
	}
	if removed, err := l.Remove("member"); removed || !errors.Is(err, errStore) {
		t.Errorf("Remove: %v, %v", removed, err)
	}
	if _, ok := l.Roles("member"); !ok {
		t.Error("a member whose removal could not be saved is gone")
	}
	if _, err := l.Redeem(token, "joiner", now); !errors.Is(err, errStore) {
		t.Errorf("Redeem: %v, want the failure to save", err)
	}

	l.store = failing{}
	if _, err := l.Invite(Worker, time.Hour, now); !errors.Is(err, errStore) {
		t.Errorf("Invite: %v", err)
	}
	if _, err := l.Redeem("any", "joiner", now); !errors.Is(err, errStore) {
		t.Errorf("Redeem with a store that cannot be read: %v", err)
	}
}

func TestImportingTheListAnEarlierVersionKept(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "access.json")
	old := `[{"id":"worker-id","role":"worker","joined":"2026-10-05T12:00:00Z"},{"id":"client-id","role":"client","joined":"2026-10-05T12:00:00Z"}]`
	if err := os.WriteFile(file, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	l := open(t, filepath.Join(dir, "node.db"))
	if err := l.Import(file); err != nil {
		t.Fatal(err)
	}
	if members := l.Members(); len(members) != 2 || !slices.Equal(members[1].Roles, []Role{Worker}) || !members[1].Joined.Equal(now) {
		t.Errorf("imported %+v", members)
	}
	// The file is set aside, so a member removed later does not come back.
	if _, err := os.Stat(file + ".imported"); err != nil {
		t.Errorf("the old file was not set aside: %v", err)
	}
	l.Remove("worker-id")
	if err := l.Import(file); err != nil {
		t.Errorf("importing with no file left: %v", err)
	}
	if _, ok := l.Roles("worker-id"); ok {
		t.Error("a second import brought back a removed member")
	}
}

func TestImportsThatFail(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		file := filepath.Join(dir, name)
		if err := os.WriteFile(file, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return file
	}
	l := open(t, "")
	if err := l.Import(dir); err == nil || !strings.Contains(err.Error(), "read access list") {
		t.Errorf("importing a directory: %v", err)
	}
	if err := l.Import(write("junk.json", "not json")); err == nil || !strings.Contains(err.Error(), "junk.json") {
		t.Errorf("importing what is not JSON: %v", err)
	}
	// Somewhere the file cannot be set aside to.
	blocked := write("blocked.json", "[]")
	if err := os.MkdirAll(filepath.Join(blocked+".imported", "in-the-way"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := l.Import(blocked); err == nil || !strings.Contains(err.Error(), "import access list") {
		t.Errorf("importing a file that cannot be set aside: %v", err)
	}
	l.store = stuck{l.store}
	if err := l.Import(write("members.json", `[{"id":"a","role":"worker"}]`)); !errors.Is(err, errStore) {
		t.Errorf("importing into a store that takes no changes: %v", err)
	}
}

func TestTheListSaysWhoWasAdmittedOrRemoved(t *testing.T) {
	l := open(t, "")
	var told []string
	l.OnChange(func(id string) { told = append(told, id) })
	l.Admit("a", Worker, now)
	token, _ := l.Invite(Client, time.Hour, now)
	l.Redeem(token, "b", now)
	l.Remove("a")
	// Nothing happened here, so nothing is said.
	l.Remove("never-admitted")
	l.Redeem("no-such-token", "c", now)
	l.store = stuck{l.store}
	l.Admit("d", Worker, now)
	l.Remove("b")
	if strings.Join(told, ",") != "a,b,a" {
		t.Errorf("told of %v, want a admitted, b admitted, a removed", told)
	}
}

// A worker that redeems a client invitation holds both roles, keeps the time
// it first joined, and keeps both across a restart (#474).
func TestAMemberCanHoldBothRoles(t *testing.T) {
	file := filepath.Join(t.TempDir(), "node.db")
	l := open(t, file)
	if err := l.Admit("node", Worker, now); err != nil {
		t.Fatal(err)
	}
	token, _ := l.Invite(Client, time.Hour, now)
	if role, err := l.Redeem(token, "node", now.Add(time.Hour)); err != nil || role != Client {
		t.Fatalf("Redeem = %q, %v", role, err)
	}
	// Admitting it again in a role it has changes nothing.
	if err := l.Admit("node", Worker, now.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	for _, list := range []*List{l, open(t, file)} {
		members := list.Members()
		if len(members) != 1 || !slices.Equal(members[0].Roles, []Role{Worker, Client}) || !members[0].Joined.Equal(now) {
			t.Errorf("a worker that redeemed a client invitation is listed as %+v, want both roles since it first joined", members)
		}
		if !list.Has("node", Worker) || !list.Has("node", Client) || list.Has("owner-id", Worker) || list.Has("stranger", Client) {
			t.Error("Has does not say what each node holds")
		}
	}
}

// Taking one role leaves the other; taking the last takes the member off the
// list, as removing it would.
func TestRevokingARole(t *testing.T) {
	l := open(t, "")
	var told []string
	l.OnChange(func(id string) { told = append(told, id) })
	l.Admit("node", Worker, now)
	l.Admit("node", Client, now)
	told = nil

	if revoked, err := l.Revoke("node", Worker); !revoked || err != nil {
		t.Fatalf("Revoke(worker) = %v, %v", revoked, err)
	}
	if roles, _ := l.Roles("node"); !slices.Equal(roles, []Role{Client}) {
		t.Errorf("after the worker role was taken, the node has %q, want client", roles)
	}
	for _, id := range []string{"node", "stranger"} {
		if revoked, err := l.Revoke(id, Worker); revoked || err != nil {
			t.Errorf("Revoke of a role %s does not hold = %v, %v", id, revoked, err)
		}
	}
	if revoked, err := l.Revoke("node", Client); !revoked || err != nil {
		t.Fatalf("Revoke(client) = %v, %v", revoked, err)
	}
	if _, ok := l.Roles("node"); ok {
		t.Error("a node with no role left is still a member")
	}
	if strings.Join(told, ",") != "node,node" {
		t.Errorf("told of %v, want the node once for each role taken", told)
	}

	// One that cannot be saved takes nothing.
	l.Admit("kept", Worker, now)
	l.Admit("kept", Client, now)
	l.Admit("alone", Worker, now)
	l.store = stuck{l.store}
	if revoked, err := l.Revoke("kept", Client); revoked || !errors.Is(err, errStore) {
		t.Errorf("Revoke with a store that cannot save = %v, %v", revoked, err)
	}
	if roles, _ := l.Roles("kept"); !slices.Equal(roles, []Role{Worker, Client}) {
		t.Errorf("after a revocation that could not be saved, the node has %q", roles)
	}
	if revoked, err := l.Revoke("alone", Worker); revoked || !errors.Is(err, errStore) {
		t.Errorf("Revoke of a last role with a store that cannot save = %v, %v", revoked, err)
	}
	if !l.Has("alone", Worker) {
		t.Error("a member whose last role could not be taken is gone")
	}
}

// Roles are read back as a store keeps them, in either order, and what is
// not a role is no role.
func TestRolesAreReadAsAStoreKeepsThem(t *testing.T) {
	for stored, want := range map[string][]Role{
		"worker": {Worker}, "client": {Client}, "client,worker": {Worker, Client}, "worker,client": {Worker, Client}, "owner": nil, "": nil,
	} {
		if got := rolesOf(stored); !slices.Equal(got, want) {
			t.Errorf("rolesOf(%q) = %q, want %q", stored, got, want)
		}
	}
}

// Taking a member's last role and admitting it in another, at once, never
// loses the role admitted: either it was admitted first and outlives the
// one taken, or the member went first and was admitted again. Were the
// role and the member to go apart, an admission landing between them would
// be lost; the many tries make that all but certain to show.
func TestARoleAdmittedWhileTheLastIsTakenIsKept(t *testing.T) {
	l := open(t, "")
	for range 20000 {
		if err := l.Admit("node", Worker, now); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		start := make(chan struct{})
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			l.Admit("node", Client, now)
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			l.Revoke("node", Worker)
		}()
		close(start)
		wg.Wait()
		if !l.Has("node", Client) {
			t.Fatal("a client role admitted while the worker role was taken was lost")
		}
		l.Remove("node")
	}
}
