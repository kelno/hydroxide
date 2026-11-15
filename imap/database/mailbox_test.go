package database

import (
	"path/filepath"
	"testing"

	bolt "go.etcd.io/bbolt"

	"github.com/emersion/hydroxide/protonmail"
)

func openTestUser(t *testing.T, path string) *User {
	t.Helper()
	db, err := bolt.Open(path, 0600, nil)
	if err != nil {
		t.Fatal(err)
	}
	return &User{db}
}

func messages(ids ...string) []*protonmail.Message {
	var msgs []*protonmail.Message
	for _, id := range ids {
		msgs = append(msgs, &protonmail.Message{ID: id})
	}
	return msgs
}

func keep(ids ...string) map[string]struct{} {
	m := make(map[string]struct{})
	for _, id := range ids {
		m[id] = struct{}{}
	}
	return m
}

func uidOf(t *testing.T, mbox *Mailbox, apiID string) uint32 {
	t.Helper()
	_, uid, err := mbox.FromApiID(apiID)
	if err != nil {
		t.Fatalf("FromApiID(%q): %v", apiID, err)
	}
	return uid
}

// A message keeps its UID when the mailbox is synchronized again, including
// after the database is closed and reopened, and a message that left the
// mailbox doesn't hand its UID to another one.
func TestMailboxUIDsStayStable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "user.db")
	u := openTestUser(t, path)

	mbox, err := u.Mailbox(protonmail.LabelInbox)
	if err != nil {
		t.Fatal(err)
	}
	validity, err := mbox.UidValidity()
	if err != nil || validity == 0 {
		t.Fatalf("UidValidity() = %v, %v", validity, err)
	}

	if err := mbox.Sync(messages("a", "b", "c")); err != nil {
		t.Fatal(err)
	}
	uidC := uidOf(t, mbox, "c")
	if err := u.Close(); err != nil {
		t.Fatal(err)
	}

	// "b" left the mailbox and "d" arrived while no client was connected.
	u = openTestUser(t, path)
	defer u.Close()
	mbox, err = u.Mailbox(protonmail.LabelInbox)
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := mbox.UidValidity(); v != validity {
		t.Errorf("UidValidity changed on reopen: %v, was %v", v, validity)
	}
	if err := mbox.Sync(messages("a", "c", "d")); err != nil {
		t.Fatal(err)
	}
	if err := mbox.RemoveExcept(keep("a", "c", "d")); err != nil {
		t.Fatal(err)
	}

	if got := uidOf(t, mbox, "c"); got != uidC {
		t.Errorf("UID of c = %v, was %v", got, uidC)
	}
	if _, _, err := mbox.FromApiID("b"); err != ErrNotFound {
		t.Errorf("b still in mailbox: %v", err)
	}
	if got := uidOf(t, mbox, "d"); got <= uidC {
		t.Errorf("UID of new message d = %v, want above %v", got, uidC)
	}
}

// A \Deleted mark survives reopening the database, and marks on messages
// that left the mailbox are dropped.
func TestMailboxDeletedMarks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "user.db")
	u := openTestUser(t, path)

	mbox, err := u.Mailbox(protonmail.LabelInbox)
	if err != nil {
		t.Fatal(err)
	}
	if err := mbox.Sync(messages("a", "b", "c")); err != nil {
		t.Fatal(err)
	}
	if err := mbox.MarkDeleted([]string{"a", "b"}, true); err != nil {
		t.Fatal(err)
	}
	if err := u.Close(); err != nil {
		t.Fatal(err)
	}

	u = openTestUser(t, path)
	defer u.Close()
	mbox, err = u.Mailbox(protonmail.LabelInbox)
	if err != nil {
		t.Fatal(err)
	}
	if deleted, err := mbox.IsDeleted("a"); err != nil || !deleted {
		t.Errorf("IsDeleted(a) = %v, %v after reopen", deleted, err)
	}
	if deleted, _ := mbox.IsDeleted("c"); deleted {
		t.Errorf("c is marked deleted")
	}

	// Another mailbox has its own marks.
	other, err := u.Mailbox(protonmail.LabelArchive)
	if err != nil {
		t.Fatal(err)
	}
	if deleted, _ := other.IsDeleted("a"); deleted {
		t.Errorf("a is marked deleted in Archive")
	}

	if err := mbox.RemoveExcept(keep("a", "c")); err != nil {
		t.Fatal(err)
	}
	if err := mbox.MarkDeleted([]string{"a"}, false); err != nil {
		t.Fatal(err)
	}
	if err := mbox.MarkDeleted([]string{"c"}, true); err != nil {
		t.Fatal(err)
	}
	got, err := mbox.Deleted()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "c" {
		t.Errorf("Deleted() = %v, want [c]", got)
	}
}

// A message that leaves the mailbox through Proton's events loses its
// \Deleted mark there, so it isn't flagged if it comes back.
func TestMailboxDeletedMarkDroppedWhenMessageLeaves(t *testing.T) {
	u := openTestUser(t, filepath.Join(t.TempDir(), "user.db"))
	defer u.Close()

	mbox, err := u.Mailbox(protonmail.LabelInbox)
	if err != nil {
		t.Fatal(err)
	}
	msg := &protonmail.Message{ID: "a", LabelIDs: []string{protonmail.LabelInbox}}
	if _, err := u.CreateMessage(msg); err != nil {
		t.Fatal(err)
	}
	if err := mbox.MarkDeleted([]string{"a"}, true); err != nil {
		t.Fatal(err)
	}

	if _, err := u.DeleteMessage("a"); err != nil {
		t.Fatal(err)
	}
	if deleted, err := mbox.IsDeleted("a"); err != nil || deleted {
		t.Errorf("IsDeleted(a) = %v, %v after the message left", deleted, err)
	}
}
