package imap

import (
	"bufio"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap"
	imapbackend "github.com/emersion/go-imap/backend"
	"github.com/emersion/go-imap/backend/memory"
	imapserver "github.com/emersion/go-imap/server"
)

// Wrappers around go-imap's in-memory backend that record UID EXPUNGE calls.
type testBackend struct {
	imapbackend.Backend
	expunged chan string
	appended *appendResult
}

// appendResult is what testMailbox.CreateMessageUid returns.
type appendResult struct {
	uidValidity, uid uint32
	err              error
}

func (be *testBackend) Login(info *imap.ConnInfo, username, password string) (imapbackend.User, error) {
	u, err := be.Backend.Login(info, username, password)
	if err != nil {
		return nil, err
	}
	return &testUser{u, be.expunged, be.appended}, nil
}

type testUser struct {
	imapbackend.User
	expunged chan string
	appended *appendResult
}

func (u *testUser) GetMailbox(name string) (imapbackend.Mailbox, error) {
	mbox, err := u.User.GetMailbox(name)
	if err != nil {
		return nil, err
	}
	return &testMailbox{mbox, u.expunged, u.appended}, nil
}

type testMailbox struct {
	imapbackend.Mailbox
	expunged chan string
	appended *appendResult
}

func (mbox *testMailbox) UidExpunge(seqSet *imap.SeqSet) error {
	mbox.expunged <- seqSet.String()
	return nil
}

func (mbox *testMailbox) CreateMessageUid(flags []string, date time.Time, body imap.Literal) (uidValidity, uid uint32, err error) {
	if err := mbox.Mailbox.CreateMessage(flags, date, body); err != nil {
		return 0, 0, err
	}
	r := mbox.appended
	return r.uidValidity, r.uid, r.err
}

func TestUidExpunge(t *testing.T) {
	be := &testBackend{memory.New(), make(chan string, 1), &appendResult{}}
	s := imapserver.New(be)
	s.AllowInsecureAuth = true
	s.Enable(NewUidPlusExtension())

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go s.Serve(l)
	defer s.Close()

	c, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	r := bufio.NewReader(c)

	// cmd sends a tagged command and returns every line up to and including
	// its tagged response.
	cmd := func(tag, line string) []string {
		t.Helper()
		if _, err := c.Write([]byte(tag + " " + line + "\r\n")); err != nil {
			t.Fatal(err)
		}
		var lines []string
		for {
			l, err := r.ReadString('\n')
			if err != nil {
				t.Fatalf("%s: %v (got %q)", line, err, lines)
			}
			l = strings.TrimRight(l, "\r\n")
			lines = append(lines, l)
			if strings.HasPrefix(l, tag+" ") {
				return lines
			}
		}
	}
	ok := func(lines []string) bool {
		return strings.HasPrefix(strings.SplitN(lines[len(lines)-1], " ", 2)[1], "OK")
	}

	if _, err := r.ReadString('\n'); err != nil { // Greeting
		t.Fatal(err)
	}
	if lines := cmd("a1", "CAPABILITY"); strings.Contains(strings.Join(lines, "\n"), "UIDPLUS") {
		t.Errorf("UIDPLUS advertised before login: %q", lines)
	}
	if lines := cmd("a2", "LOGIN username password"); !ok(lines) {
		t.Fatalf("LOGIN: %q", lines)
	}
	if lines := cmd("a3", "CAPABILITY"); !strings.Contains(strings.Join(lines, "\n"), "UIDPLUS") {
		t.Errorf("UIDPLUS not advertised after login: %q", lines)
	}
	if lines := cmd("a4", "SELECT INBOX"); !ok(lines) {
		t.Fatalf("SELECT: %q", lines)
	}

	if lines := cmd("a5", "UID EXPUNGE 4:7,9"); !ok(lines) {
		t.Fatalf("UID EXPUNGE: %q", lines)
	}
	select {
	case got := <-be.expunged:
		if got != "4:7,9" {
			t.Errorf("UidExpunge got %q, want 4:7,9", got)
		}
	default:
		t.Error("UID EXPUNGE didn't reach the mailbox")
	}

	if lines := cmd("a6", "UID EXPUNGE"); ok(lines) {
		t.Errorf("UID EXPUNGE without UIDs accepted: %q", lines)
	}

	// Plain EXPUNGE still goes to the mailbox's Expunge.
	if lines := cmd("a7", "EXPUNGE"); !ok(lines) {
		t.Errorf("EXPUNGE: %q", lines)
	}
	select {
	case got := <-be.expunged:
		t.Errorf("EXPUNGE went to UidExpunge with %q", got)
	default:
	}

	// APPEND answers with the new message's UID (APPENDUID): Thunderbird up
	// to 160 reads it without checking that it came. hydroxide's lookups of
	// that UID in Proton aren't tested here.
	appendMsg := "APPEND INBOX {17+}\r\nSubject: hi\r\n\r\nhi"
	*be.appended = appendResult{uidValidity: 7, uid: 42}
	lines := cmd("a8", appendMsg)
	if last := lines[len(lines)-1]; last != "a8 OK [APPENDUID 7 42] APPEND completed" {
		t.Errorf("APPEND answered %q, want APPENDUID 7 42", lines)
	}

	// An unknown UID gives a plain OK.
	*be.appended = appendResult{}
	lines = cmd("a9", appendMsg)
	if last := lines[len(lines)-1]; last != "a9 OK APPEND completed" {
		t.Errorf("APPEND with unknown UID answered %q, want a plain OK", lines)
	}

	// A failed append gives NO.
	*be.appended = appendResult{err: errors.New("refused")}
	lines = cmd("a10", appendMsg)
	if last := lines[len(lines)-1]; !strings.HasPrefix(last, "a10 NO ") {
		t.Errorf("failed APPEND answered %q, want NO", lines)
	}
}
