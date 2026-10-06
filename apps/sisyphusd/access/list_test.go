package access

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
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
		if got, ok := l.Role(id); !ok || got != want {
			t.Errorf("Role(%s) = %q, %v; want %q", id, got, ok, want)
		}
	}
	if got, ok := l.Role("stranger-id"); ok || got != "" {
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
	if got, _ := l.Role("first"); got != Client {
		t.Errorf("the redeeming node's role is %q", got)
	}
	if _, err := l.Redeem(token, "second", now); !errors.Is(err, ErrBadInvite) {
		t.Errorf("a used invitation was accepted again: %v", err)
	}
	if _, ok := l.Role("second"); ok {
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
	if _, ok := l.Role("worker-id"); ok {
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
		if m.Role != Worker || !m.Joined.Equal(now) {
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
	if len(members) != 2 || members[0].ID != "client-id" || members[0].Role != Client || members[1].ID != "worker-id" || !members[1].Joined.Equal(now) {
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
	if _, ok := l.Role("another"); ok {
		t.Error("a node whose admission could not be saved was admitted")
	}
	if removed, err := l.Remove("member"); removed || !errors.Is(err, errStore) {
		t.Errorf("Remove: %v, %v", removed, err)
	}
	if _, ok := l.Role("member"); !ok {
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
	if members := l.Members(); len(members) != 2 || members[1].Role != Worker || !members[1].Joined.Equal(now) {
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
	if _, ok := l.Role("worker-id"); ok {
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
