package access

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var now = time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)

func open(t *testing.T, file string) *List {
	t.Helper()
	l, err := Open(file, "owner-id")
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
	file := filepath.Join(t.TempDir(), "access.json")
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
	if info, err := os.Stat(file); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("the list is stored as %v (%v), want mode 600", info.Mode().Perm(), err)
	}

	reopened := open(t, file)
	members := reopened.Members()
	if len(members) != 2 || members[0].ID != "client-id" || members[0].Role != Client || members[1].ID != "worker-id" || !members[1].Joined.Equal(now) {
		t.Errorf("after reopening: %+v", members)
	}
	// An invitation does not survive a restart.
	unused, _ := l.Invite(Worker, time.Hour, now)
	if _, err := reopened.Redeem(unused, "node", now); !errors.Is(err, ErrBadInvite) {
		t.Errorf("an invitation issued before a restart was honoured after it: %v", err)
	}
}

func TestOpeningAListThatCannotBeRead(t *testing.T) {
	dir := t.TempDir()
	if _, err := Open(dir, "owner-id"); err == nil {
		t.Error("opened a directory as an access list")
	}
	file := filepath.Join(dir, "access.json")
	if err := os.WriteFile(file, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(file, "owner-id"); err == nil {
		t.Error("opened a list that is not JSON")
	}
}

func TestChangesThatCannotBeSavedAreReported(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "access.json")
	l := open(t, file)
	if err := l.Admit("member", Worker, now); err != nil {
		t.Fatal(err)
	}

	// The temporary file's name is taken by a directory.
	blocker := file + ".tmp"
	if err := os.Mkdir(blocker, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := l.Admit("another", Worker, now); err == nil {
		t.Error("Admit succeeded without saving")
	}
	if _, err := l.Remove("member"); err == nil {
		t.Error("Remove succeeded without saving")
	}
	token, _ := l.Invite(Worker, time.Hour, now)
	if _, err := l.Redeem(token, "joiner", now); err == nil || errors.Is(err, ErrBadInvite) {
		t.Errorf("Redeem without saving: %v, want a save failure", err)
	}
	if err := os.Remove(blocker); err != nil {
		t.Fatal(err)
	}

	// The list's own name is taken by a directory that is not empty.
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(file, "occupied"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := l.Admit("third", Worker, now); err == nil || !strings.Contains(err.Error(), "save access list") {
		t.Errorf("Admit when the file cannot be replaced: %v", err)
	}
}
