package imap

import (
	"errors"
	"time"

	"github.com/emersion/go-imap"
	imapbackend "github.com/emersion/go-imap/backend"
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

// uidAppender is a mailbox that can tell the UID of a message it appends, for
// the APPENDUID response code.
type uidAppender interface {
	CreateMessageUid(flags []string, date time.Time, body imap.Literal) (uidValidity, uid uint32, err error)
}

// appendUid handles APPEND as the built-in handler does, and answers with
// APPENDUID when the mailbox knows the new message's UID. Thunderbird up to
// 160, when a server offers UIDPLUS, takes the saved copy's UID from it
// without checking that one came: without it, the copy is taken for message 0
// or for a stale UID from an earlier FETCH, and Thunderbird reports an error
// running filters on a sent message, or runs them on another one. Thunderbird
// 161 and later (bug 2083369) search for the message by Message-ID instead.
type appendUid struct {
	imapserver.Append
}

func (cmd *appendUid) Handle(conn imapserver.Conn) error {
	ctx := conn.Context()
	if ctx.User == nil {
		return imapserver.ErrNotAuthenticated
	}

	mbox, err := ctx.User.GetMailbox(cmd.Mailbox)
	if err == imapbackend.ErrNoSuchMailbox {
		return imapserver.ErrStatusResp(&imap.StatusResp{
			Type: imap.StatusRespNo,
			Code: imap.CodeTryCreate,
			Info: err.Error(),
		})
	} else if err != nil {
		return err
	}

	appender, ok := mbox.(uidAppender)
	if !ok {
		return cmd.Append.Handle(conn)
	}

	uidValidity, uid, err := appender.CreateMessageUid(cmd.Flags, cmd.Date, cmd.Message)
	if err == imapbackend.ErrTooBig {
		return imapserver.ErrStatusResp(&imap.StatusResp{
			Type: imap.StatusRespNo,
			Code: "TOOBIG",
			Info: "Message size exceeding limit",
		})
	} else if err != nil {
		return err
	}
	if uid == 0 {
		return nil
	}

	// Clients are told about the new message by the backend's updates.
	return imapserver.ErrStatusResp(&imap.StatusResp{
		Type:      imap.StatusRespOk,
		Code:      "APPENDUID",
		Arguments: []interface{}{uidValidity, uid},
		Info:      "APPEND completed",
	})
}

type uidPlusExtension struct{}

// NewUidPlusExtension returns the server side of the UIDPLUS extension (RFC
// 4315): UID EXPUNGE, which lets a client expunge just the messages it
// flagged \Deleted, so that a move to another server or a local folder
// completes at once; and the APPENDUID response code on APPEND, with the
// appended message's UID. The COPYUID response code is not sent yet.
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
	switch name {
	case "EXPUNGE":
		return func() imapserver.Handler {
			return &uidExpunge{}
		}
	case "APPEND":
		return func() imapserver.Handler {
			return &appendUid{}
		}
	}
	return nil
}
