package imap

import (
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/emersion/go-imap"
	imapbackend "github.com/emersion/go-imap/backend"
	"github.com/emersion/go-message/mail"

	"github.com/emersion/hydroxide/imap/database"
	"github.com/emersion/hydroxide/protonmail"
)

const delimiter = "/"

type mailbox struct {
	name  string
	label string
	attrs []string

	u  *user
	db *database.Mailbox

	sync.Mutex // protects everything below

	initialized   bool
	total, unread int
}

func newMailbox(name string, label string, attrs []string, u *user) (*mailbox, error) {
	mboxDB, err := u.db.Mailbox(label)
	if err != nil {
		return nil, err
	}

	return &mailbox{
		name:  name,
		label: label,
		attrs: attrs,
		u:     u,
		db:    mboxDB,
	}, nil
}

func (mbox *mailbox) Name() string {
	return mbox.name
}

func (mbox *mailbox) Info() (*imap.MailboxInfo, error) {
	return &imap.MailboxInfo{
		Attributes: append([]string{imap.NoInferiorsAttr}, mbox.attrs...),
		Delimiter:  delimiter,
		Name:       mbox.name,
	}, nil
}

func (mbox *mailbox) Status(items []imap.StatusItem) (*imap.MailboxStatus, error) {
	mbox.u.Lock()
	flags := []string{imap.SeenFlag, imap.DeletedFlag}
	permFlags := []string{imap.SeenFlag, imap.DeletedFlag}
	for _, flag := range mbox.u.flags {
		flags = append(flags, flag)
		permFlags = append(permFlags, flag)
	}
	mbox.u.Unlock()

	status := imap.NewMailboxStatus(mbox.name, items)
	status.Flags = flags
	status.PermanentFlags = permFlags
	status.UnseenSeqNum = 0 // TODO

	mbox.Lock()
	defer mbox.Unlock()

	for _, name := range items {
		switch name {
		case imap.StatusMessages:
			status.Messages = uint32(mbox.total)
		case imap.StatusUidNext:
			uidNext, err := mbox.db.UidNext()
			if err != nil {
				return nil, err
			}
			status.UidNext = uidNext
		case imap.StatusUidValidity:
			uidValidity, err := mbox.db.UidValidity()
			if err != nil {
				return nil, err
			}
			status.UidValidity = uidValidity
		case imap.StatusRecent:
			status.Recent = 0
		case imap.StatusUnseen:
			status.Unseen = uint32(mbox.unread)
		}
	}

	return status, nil
}

func (mbox *mailbox) SetSubscribed(subscribed bool) error {
	return errNotYetImplemented // TODO
}

func (mbox *mailbox) Check() error {
	return nil
}

func (mbox *mailbox) sync() error {
	log.Printf("Synchronizing mailbox %v...", mbox.name)

	// Messages already in the local database keep their UID, so that clients
	// can keep trusting the UIDs they cached; only new messages get one.
	seen := make(map[string]struct{})

	filter := &protonmail.MessageFilter{
		PageSize: 150,
		Label:    mbox.label,
		Sort:     "ID",
		Asc:      true,
	}

	total := -1
	for {
		offset := filter.PageSize * filter.Page
		if total >= 0 && offset > total {
			break
		}

		var page []*protonmail.Message
		var err error
		total, page, err = mbox.u.c.ListMessages(filter)
		if err != nil {
			return err
		}

		if err := mbox.db.Sync(page); err != nil {
			return err
		}
		for _, msg := range page {
			seen[msg.ID] = struct{}{}
		}

		filter.Page++
	}

	if err := mbox.db.RemoveExcept(seen); err != nil {
		return err
	}

	log.Printf("Synchronizing mailbox %v: done.", mbox.name)
	return nil
}

func (mbox *mailbox) init() error {
	mbox.Lock()
	defer mbox.Unlock()

	if mbox.initialized {
		return nil
	}

	if err := mbox.sync(); err != nil {
		return err
	}

	mbox.initialized = true
	return nil
}

func (mbox *mailbox) reset() error {
	mbox.Lock()
	defer mbox.Unlock()

	// The next access synchronizes the mailbox again. Its UIDs are kept.
	mbox.initialized = false
	return nil
}

func (mbox *mailbox) fetchFlags(msg *protonmail.Message) []string {
	var flags []string
	if msg.Unread != 1 {
		flags = append(flags, imap.SeenFlag)
	}
	if msg.IsReplied != 0 || msg.IsRepliedAll != 0 {
		flags = append(flags, imap.AnsweredFlag)
	}

	if deleted, err := mbox.db.IsDeleted(msg.ID); err != nil {
		log.Printf("cannot read the \\Deleted flag of message %s: %v", msg.ID, err)
	} else if deleted {
		flags = append(flags, imap.DeletedFlag)
	}

	mbox.u.Lock()
	for _, label := range msg.LabelIDs {
		if flag, ok := mbox.u.flags[label]; ok {
			flags = append(flags, flag)
		}
	}
	mbox.u.Unlock()

	return flags
}

func (mbox *mailbox) fetchMessage(isUid bool, id uint32, items []imap.FetchItem) (*imap.Message, error) {
	var apiID string
	var err error
	if isUid {
		apiID, err = mbox.db.FromUid(id)
	} else {
		apiID, err = mbox.db.FromSeqNum(id)
	}
	if err != nil {
		return nil, err
	}

	seqNum, uid, err := mbox.db.FromApiID(apiID)
	if err != nil {
		return nil, err
	}

	msg, err := mbox.u.db.Message(apiID)
	if err != nil {
		return nil, err
	}

	fetched := imap.NewMessage(seqNum, items)
	for _, item := range items {
		switch item {
		case imap.FetchEnvelope:
			fetched.Envelope = fetchEnvelope(msg)
		case imap.FetchBody, imap.FetchBodyStructure:
			bs, err := mbox.fetchBodyStructure(msg, item == imap.FetchBodyStructure)
			if err != nil {
				return nil, err
			}
			fetched.BodyStructure = bs
		case imap.FetchFlags:
			fetched.Flags = mbox.fetchFlags(msg)
		case imap.FetchInternalDate:
			fetched.InternalDate = msg.Time.Time()
		case imap.FetchRFC822Size:
			fetched.Size = uint32(msg.Size)
		case imap.FetchUid:
			fetched.Uid = uid
		default:
			section, err := imap.ParseBodySectionName(item)
			if err != nil {
				break
			}

			l, err := mbox.fetchBodySection(msg, section)
			if err != nil {
				return nil, err
			}
			fetched.Body[section] = l
		}
	}

	return fetched, nil
}

func (mbox *mailbox) ListMessages(uid bool, seqSet *imap.SeqSet, items []imap.FetchItem, ch chan<- *imap.Message) error {
	defer close(ch)

	if err := mbox.init(); err != nil {
		return err
	}

	for _, seq := range seqSet.Set {
		start := seq.Start
		if start == 0 {
			start = 1
		}

		stop := seq.Stop
		if stop == 0 {
			if uid {
				uidNext, err := mbox.db.UidNext()
				if err != nil {
					return err
				}
				stop = uidNext - 1
			} else {
				stop = uint32(mbox.total)
			}
		}

		for i := start; i <= stop; i++ {
			msg, err := mbox.fetchMessage(uid, i, items)
			if err == database.ErrNotFound {
				continue
			} else if err != nil {
				return err
			}
			if msg != nil {
				ch <- msg
			}
		}
	}

	return nil
}

func matchString(s, substr string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(substr))
}

func (mbox *mailbox) matchMessage(seqNum, uid uint32, msg *protonmail.Message, c *imap.SearchCriteria) bool {
	if c.SeqNum != nil && !c.SeqNum.Contains(seqNum) {
		return false
	}
	if c.Uid != nil && !c.Uid.Contains(uid) {
		return false
	}

	flags := make(map[string]bool)
	for _, flag := range mbox.fetchFlags(msg) {
		flags[flag] = true
	}
	for _, f := range c.WithFlags {
		if !flags[f] {
			return false
		}
	}
	for _, f := range c.WithoutFlags {
		if flags[f] {
			return false
		}
	}

	date := msg.Time.Time().Round(24 * time.Hour)
	if !c.Since.IsZero() && !date.After(c.Since) {
		return false
	}

	if !c.Before.IsZero() && !date.Before(c.Before) {
		return false
	}

	// TODO: this date should be from the Date MIME header
	if !c.SentBefore.IsZero() && !date.Before(c.SentBefore) {
		return false
	}

	if !c.SentSince.IsZero() && !date.After(c.SentSince) {
		return false
	}

	if c.Larger > 0 && uint32(msg.Size) < c.Larger {
		return false
	}

	if c.Smaller > 0 && uint32(msg.Size) > c.Smaller {
		return false
	}

	h := messageHeader(msg)
	for key, wantValues := range c.Header {
		fields := h.FieldsByKey(key)
		var values []string
		for fields.Next() {
			values = append(values, fields.Value())
		}

		for _, wantValue := range wantValues {
			if wantValue == "" && len(values) == 0 {
				return false
			}
			if wantValue != "" {
				ok := false
				for _, v := range values {
					if matchString(v, wantValue) {
						ok = true
						break
					}
				}
				if !ok {
					return false
				}
			}
		}
	}

	for _, e := range c.Body {
		if !matchString(msg.Body, e) {
			return false
		}
	}

	for _, e := range c.Text {
		if !matchString(msg.Body, e) && !matchString(msg.Header, e) {
			return false
		}
	}

	for _, e := range c.Or {
		if !mbox.matchMessage(seqNum, uid, msg, e[0]) && !mbox.matchMessage(seqNum, uid, msg, e[1]) {
			return false
		}
	}

	for _, e := range c.Not {
		if mbox.matchMessage(seqNum, uid, msg, e) {
			return false
		}
	}

	return true
}

func (mbox *mailbox) SearchMessages(isUID bool, c *imap.SearchCriteria) ([]uint32, error) {
	if err := mbox.init(); err != nil {
		return nil, err
	}

	var results []uint32
	err := mbox.db.ForEach(func(seqNum, uid uint32, apiID string) error {
		// TODO: fetch message from local DB only if needed
		msg, err := mbox.u.db.Message(apiID)
		if err != nil {
			return err
		}

		if mbox.matchMessage(seqNum, uid, msg, c) {
			if isUID {
				results = append(results, uid)
			} else {
				results = append(results, seqNum)
			}
		}

		return nil
	})

	if err != nil {
		return nil, err
	}
	return results, nil
}

func (mbox *mailbox) CreateMessage(flags []string, date time.Time, body imap.Literal) (err error) {
	defer func() {
		if err != nil {
			log.Printf("Cannot append message to mailbox %q: %v", mbox.name, err)
		}
	}()

	if mbox.label == protonmail.LabelSent {
		return mbox.acknowledgeSentCopy(body)
	}

	if mbox.label != protonmail.LabelDraft {
		return errors.New("cannot create messages outside the Drafts and Sent mailboxes")
	}

	if err := mbox.init(); err != nil {
		return err
	}

	_, err = createMessage(mbox.u.c, mbox.u.u, mbox.u.privateKeys, mbox.u.addrs, body)
	if err != nil {
		return err
	}

	return mbox.Poll()
}

// acknowledgeSentCopy handles a client saving a copy of a sent message in the
// Sent mailbox. Messages sent through hydroxide's SMTP server are already in
// Proton's Sent folder, with the Message-Id stored as their ExternalID, so the
// copy is accepted without creating a duplicate. A message that Proton doesn't
// know as sent is refused, rather than silently dropped.
func (mbox *mailbox) acknowledgeSentCopy(body imap.Literal) error {
	mr, err := mail.CreateReader(body)
	if err != nil {
		return err
	}
	defer mr.Close()

	msgID, err := mr.Header.MessageID()
	if err != nil {
		return fmt.Errorf("cannot parse Message-Id: %v", err)
	}
	if msgID == "" {
		return errors.New("cannot match message without Message-Id to a sent message")
	}

	total, _, err := mbox.u.c.ListMessages(&protonmail.MessageFilter{
		Label:      protonmail.LabelSent,
		ExternalID: msgID,
		Limit:      1,
	})
	if err != nil {
		return err
	}
	if total == 0 {
		return fmt.Errorf("no message with Message-Id <%s> in Proton's Sent folder", msgID)
	}

	if err := mbox.init(); err != nil {
		return err
	}
	return mbox.Poll()
}

func (mbox *mailbox) fromSeqSet(isUID bool, seqSet *imap.SeqSet) ([]string, error) {
	var apiIDs []string
	err := mbox.db.ForEach(func(seqNum, uid uint32, apiID string) error {
		var id uint32
		if isUID {
			id = uid
		} else {
			id = seqNum
		}

		if seqSet.Contains(id) {
			apiIDs = append(apiIDs, apiID)
		}
		return nil
	})
	return apiIDs, err
}

func (mbox *mailbox) UpdateMessagesFlags(uid bool, seqSet *imap.SeqSet, op imap.FlagsOp, flags []string) error {
	if err := mbox.init(); err != nil {
		return err
	}

	apiIDs, err := mbox.fromSeqSet(uid, seqSet)
	if err != nil {
		return err
	}

	// TODO: imap.SetFlags should remove currently set flags

	for _, flag := range flags {
		var err error
		switch flag {
		case imap.SeenFlag:
			switch op {
			case imap.SetFlags, imap.AddFlags:
				err = mbox.u.c.MarkMessagesRead(apiIDs)
			case imap.RemoveFlags:
				err = mbox.u.c.MarkMessagesUnread(apiIDs)
			}
		case imap.DeletedFlag:
			// TODO: send updates
			err = mbox.db.MarkDeleted(apiIDs, op != imap.RemoveFlags)
		case imap.DraftFlag:
			// No-op
		default:
			label := mbox.u.getFlag(flag)
			if label == "" {
				break
			}

			switch op {
			case imap.SetFlags, imap.AddFlags:
				err = mbox.u.c.LabelMessages(label, apiIDs)
			case imap.RemoveFlags:
				err = mbox.u.c.UnlabelMessages(label, apiIDs)
			}
		}
		if err != nil {
			return err
		}
	}

	return mbox.Poll()
}

func (mbox *mailbox) CopyMessages(uid bool, seqSet *imap.SeqSet, destName string) error {
	if err := mbox.init(); err != nil {
		return err
	}

	apiIDs, err := mbox.fromSeqSet(uid, seqSet)
	if err != nil {
		return err
	}

	dest := mbox.u.getMailbox(destName)
	if dest == nil {
		return imapbackend.ErrNoSuchMailbox
	}

	if err := mbox.u.c.LabelMessages(dest.label, apiIDs); err != nil {
		return err
	}
	return mbox.Poll()
}

func (mbox *mailbox) MoveMessages(uid bool, seqSet *imap.SeqSet, destName string) error {
	if err := mbox.init(); err != nil {
		return err
	}

	apiIDs, err := mbox.fromSeqSet(uid, seqSet)
	if err != nil {
		return err
	}

	dest := mbox.u.getMailbox(destName)
	if dest == nil {
		return imapbackend.ErrNoSuchMailbox
	}

	if err := mbox.u.c.LabelMessages(dest.label, apiIDs); err != nil {
		return err
	}
	if err := mbox.u.c.UnlabelMessages(mbox.label, apiIDs); err != nil {
		return err
	}
	return mbox.Poll()
}

// Expunge removes the messages flagged \Deleted from the mailbox. In Trash
// they are deleted for good; in Starred, which is a label in Proton, they
// lose that label; anywhere else they are moved to Trash, where they can
// still be recovered.
func (mbox *mailbox) Expunge() error {
	if err := mbox.init(); err != nil {
		return err
	}

	apiIDs, err := mbox.db.Deleted()
	if err != nil {
		return err
	}
	return mbox.expunge(apiIDs)
}

// UidExpunge expunges the messages flagged \Deleted among those whose UID is
// in seqSet (UID EXPUNGE, from the UIDPLUS extension).
func (mbox *mailbox) UidExpunge(seqSet *imap.SeqSet) error {
	if err := mbox.init(); err != nil {
		return err
	}

	deleted, err := mbox.db.Deleted()
	if err != nil {
		return err
	}
	inSet, err := mbox.fromSeqSet(true, seqSet)
	if err != nil {
		return err
	}
	wanted := make(map[string]struct{}, len(inSet))
	for _, apiID := range inSet {
		wanted[apiID] = struct{}{}
	}

	var apiIDs []string
	for _, apiID := range deleted {
		if _, ok := wanted[apiID]; ok {
			apiIDs = append(apiIDs, apiID)
		}
	}
	return mbox.expunge(apiIDs)
}

func (mbox *mailbox) expunge(apiIDs []string) error {
	if len(apiIDs) == 0 {
		return nil // Nothing to do
	}

	var err error
	switch mbox.label {
	case protonmail.LabelTrash:
		log.Printf("Expunging %v messages from %v: deleting them for good", len(apiIDs), mbox.name)
		err = mbox.u.c.DeleteMessages(apiIDs)
	case protonmail.LabelStarred:
		log.Printf("Expunging %v messages from %v: removing the label", len(apiIDs), mbox.name)
		err = mbox.u.c.UnlabelMessages(mbox.label, apiIDs)
	default:
		log.Printf("Expunging %v messages from %v: moving them to Trash", len(apiIDs), mbox.name)
		err = mbox.u.c.LabelMessages(protonmail.LabelTrash, apiIDs)
	}
	if err != nil {
		log.Printf("Cannot expunge messages from %v: %v", mbox.name, err)
		return err
	}

	// The \Deleted marks stay until Proton's events take the messages out of
	// the mailbox: until then the messages are still listed here, and
	// clearing the marks would show them again as if never deleted. If Proton
	// accepted the request but left a message in place, the next expunge
	// tries again.
	return mbox.Poll()
}

func (mbox *mailbox) Poll() error {
	mbox.u.poll()
	return nil
}
