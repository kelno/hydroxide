package imap

import (
	"errors"

	"github.com/emersion/go-imap"
	imapserver "github.com/emersion/go-imap/server"
)

// uidExpunger is a mailbox that can expunge only some of its messages
// flagged \Deleted, as UID EXPUNGE (RFC 4315) asks.
type uidExpunger interface {
	UidExpunge(seqSet *imap.SeqSet) error
}

// uidExpunge handles EXPUNGE as the built-in handler does, and UID EXPUNGE.
type uidExpunge struct {
	imapserver.Expunge
	seqSet *imap.SeqSet
}

func (cmd *uidExpunge) Parse(fields []interface{}) error {
	if len(fields) == 0 {
		return nil // Plain EXPUNGE
	}

	s, err := imap.ParseString(fields[0])
	if err != nil {
		return err
	}
	cmd.seqSet, err = imap.ParseSeqSet(s)
	return err
}

func (cmd *uidExpunge) UidHandle(conn imapserver.Conn) error {
	if cmd.seqSet == nil {
		return errors.New("UID EXPUNGE needs a set of UIDs")
	}

	ctx := conn.Context()
	if ctx.Mailbox == nil {
		return imapserver.ErrNoMailboxSelected
	}
	if ctx.MailboxReadOnly {
		return imapserver.ErrMailboxReadOnly
	}

	mbox, ok := ctx.Mailbox.(uidExpunger)
	if !ok {
		return errors.New("UID EXPUNGE not supported by this mailbox")
	}
	// Clients are told about the expunged messages by the backend's updates,
	// as for EXPUNGE.
	return mbox.UidExpunge(cmd.seqSet)
}

type uidPlusExtension struct{}

// NewUidPlusExtension returns the server side of the UIDPLUS extension (RFC
// 4315): UID EXPUNGE, which lets a client expunge just the messages it
// flagged \Deleted, so that a move to another server or a local folder
// completes at once. The APPENDUID and COPYUID response codes are not sent;
// clients treat them as optional.
func NewUidPlusExtension() imapserver.Extension {
	return uidPlusExtension{}
}

func (uidPlusExtension) Capabilities(c imapserver.Conn) []string {
	if c.Context().State&imap.AuthenticatedState != 0 {
		return []string{"UIDPLUS"}
	}
	return nil
}

func (uidPlusExtension) Command(name string) imapserver.HandlerFactory {
	if name != "EXPUNGE" {
		return nil
	}
	return func() imapserver.Handler {
		return &uidExpunge{}
	}
}
