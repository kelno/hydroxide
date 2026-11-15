package database

import (
	"bytes"
	"encoding/binary"
	"errors"

	bolt "go.etcd.io/bbolt"

	"github.com/emersion/hydroxide/protonmail"
)

func serializeUID(uid uint32) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, uid)
	return b
}

func unserializeUID(b []byte) uint32 {
	return binary.BigEndian.Uint32(b)
}

func mailboxCreateMessage(b *bolt.Bucket, apiID string) (seqNum uint32, err error) {
	want := []byte(apiID)
	c := b.Cursor()
	var n uint32 = 1
	for k, v := c.First(); k != nil; k, v = c.Next() {
		if bytes.Equal(v, want) {
			return n, nil
		}
		n++
	}

	id, _ := b.NextSequence()
	uid := uint32(id)
	return n, b.Put(serializeUID(uid), want)
}

// mailboxUnmarkDeleted drops the \Deleted mark of a message that left a
// mailbox, so that it doesn't come back flagged if it returns there later.
func mailboxUnmarkDeleted(tx *bolt.Tx, labelID, apiID string) error {
	d := tx.Bucket(deletedBucket)
	if d == nil {
		return nil
	}
	b := d.Bucket([]byte(labelID))
	if b == nil {
		return nil
	}
	return b.Delete([]byte(apiID))
}

func mailboxDeleteMessage(b *bolt.Bucket, apiID string) (seqNum uint32, err error) {
	want := []byte(apiID)
	c := b.Cursor()
	var n uint32 = 1
	for k, v := c.First(); k != nil; k, v = c.Next() {
		if bytes.Equal(v, want) {
			return n, b.Delete(k)
		}
		n++
	}
	return 0, nil
}

type Mailbox struct {
	labelID string
	u       *User
}

func (mbox *Mailbox) bucket(tx *bolt.Tx) (*bolt.Bucket, error) {
	b := tx.Bucket(mailboxesBucket)
	if b == nil {
		return nil, errors.New("cannot find mailboxes bucket")
	}
	b = b.Bucket([]byte(mbox.labelID))
	if b == nil {
		return nil, errors.New("cannot find mailbox bucket")
	}
	return b, nil
}

// Sync adds the messages that aren't in the mailbox yet, each with a new UID.
// Messages already in the mailbox keep their UID.
func (mbox *Mailbox) Sync(messages []*protonmail.Message) error {
	return mbox.u.db.Update(func(tx *bolt.Tx) error {
		b, err := mbox.bucket(tx)
		if err != nil {
			return err
		}

		existing := make(map[string]struct{})
		err = b.ForEach(func(k, v []byte) error {
			existing[string(v)] = struct{}{}
			return nil
		})
		if err != nil {
			return err
		}

		for _, msg := range messages {
			if _, ok := existing[msg.ID]; ok {
				continue
			}
			id, err := b.NextSequence()
			if err != nil {
				return err
			}
			if err := b.Put(serializeUID(uint32(id)), []byte(msg.ID)); err != nil {
				return err
			}
			existing[msg.ID] = struct{}{}
		}

		return userSync(tx, messages)
	})
}

// RemoveExcept removes from the mailbox every message whose API ID is not in
// keep, along with its \Deleted mark. The other messages keep their UID.
func (mbox *Mailbox) RemoveExcept(keep map[string]struct{}) error {
	return mbox.u.db.Update(func(tx *bolt.Tx) error {
		b, err := mbox.bucket(tx)
		if err != nil {
			return err
		}

		var gone [][]byte
		var goneIDs []string
		err = b.ForEach(func(k, v []byte) error {
			if _, ok := keep[string(v)]; !ok {
				gone = append(gone, append([]byte(nil), k...))
				goneIDs = append(goneIDs, string(v))
			}
			return nil
		})
		if err != nil {
			return err
		}
		for _, k := range gone {
			if err := b.Delete(k); err != nil {
				return err
			}
		}

		return mbox.unmarkDeleted(tx, goneIDs)
	})
}

// UidValidity returns the mailbox's UIDVALIDITY. It is chosen when the
// mailbox first appears in the database, from the current time, so that a
// mailbox rebuilt from scratch (a lost or new database) gets a higher value
// and clients drop the UIDs they cached.
func (mbox *Mailbox) UidValidity() (uint32, error) {
	var v uint32
	err := mbox.u.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(uidValidityBucket)
		if b == nil {
			return errors.New("cannot find UIDVALIDITY bucket")
		}
		raw := b.Get([]byte(mbox.labelID))
		if raw == nil {
			return errors.New("cannot find mailbox UIDVALIDITY")
		}
		v = unserializeUID(raw)
		return nil
	})
	return v, err
}

func (mbox *Mailbox) deletedBucket(tx *bolt.Tx) (*bolt.Bucket, error) {
	b, err := tx.CreateBucketIfNotExists(deletedBucket)
	if err != nil {
		return nil, err
	}
	return b.CreateBucketIfNotExists([]byte(mbox.labelID))
}

// MarkDeleted sets or removes the \Deleted flag on messages. The flag is kept
// in the database, so it survives clients disconnecting and hydroxide
// restarting until the mailbox is expunged.
func (mbox *Mailbox) MarkDeleted(apiIDs []string, deleted bool) error {
	return mbox.u.db.Update(func(tx *bolt.Tx) error {
		if !deleted {
			return mbox.unmarkDeleted(tx, apiIDs)
		}
		b, err := mbox.deletedBucket(tx)
		if err != nil {
			return err
		}
		for _, apiID := range apiIDs {
			if err := b.Put([]byte(apiID), []byte{1}); err != nil {
				return err
			}
		}
		return nil
	})
}

func (mbox *Mailbox) unmarkDeleted(tx *bolt.Tx, apiIDs []string) error {
	b, err := mbox.deletedBucket(tx)
	if err != nil {
		return err
	}
	for _, apiID := range apiIDs {
		if err := b.Delete([]byte(apiID)); err != nil {
			return err
		}
	}
	return nil
}

// IsDeleted reports whether a message carries the \Deleted flag.
func (mbox *Mailbox) IsDeleted(apiID string) (bool, error) {
	var deleted bool
	err := mbox.u.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(deletedBucket)
		if b == nil {
			return nil
		}
		b = b.Bucket([]byte(mbox.labelID))
		if b == nil {
			return nil
		}
		deleted = b.Get([]byte(apiID)) != nil
		return nil
	})
	return deleted, err
}

// Deleted returns the messages of the mailbox that carry the \Deleted flag.
// Marks left on messages that are no longer in the mailbox are dropped.
func (mbox *Mailbox) Deleted() ([]string, error) {
	var apiIDs []string
	err := mbox.u.db.Update(func(tx *bolt.Tx) error {
		b, err := mbox.bucket(tx)
		if err != nil {
			return err
		}
		present := make(map[string]struct{})
		err = b.ForEach(func(k, v []byte) error {
			present[string(v)] = struct{}{}
			return nil
		})
		if err != nil {
			return err
		}

		del, err := mbox.deletedBucket(tx)
		if err != nil {
			return err
		}
		var stale []string
		err = del.ForEach(func(k, v []byte) error {
			if _, ok := present[string(k)]; ok {
				apiIDs = append(apiIDs, string(k))
			} else {
				stale = append(stale, string(k))
			}
			return nil
		})
		if err != nil {
			return err
		}
		return mbox.unmarkDeleted(tx, stale)
	})
	return apiIDs, err
}

func (mbox *Mailbox) UidNext() (uint32, error) {
	var uid uint32
	err := mbox.u.db.View(func(tx *bolt.Tx) error {
		b, err := mbox.bucket(tx)
		if err != nil {
			return err
		}

		uid = uint32(b.Sequence() + 1)
		return nil
	})
	return uid, err
}

func (mbox *Mailbox) FromUid(uid uint32) (apiID string, err error) {
	err = mbox.u.db.View(func(tx *bolt.Tx) error {
		b, err := mbox.bucket(tx)
		if err != nil {
			return err
		}

		k := serializeUID(uid)
		v := b.Get(k)
		if v == nil {
			return ErrNotFound
		}
		apiID = string(v)
		return nil
	})
	return
}

func (mbox *Mailbox) FromSeqNum(seqNum uint32) (apiID string, err error) {
	err = mbox.u.db.View(func(tx *bolt.Tx) error {
		b, err := mbox.bucket(tx)
		if err != nil {
			return err
		}

		c := b.Cursor()
		var n uint32 = 1
		for k, v := c.First(); k != nil; k, v = c.Next() {
			if seqNum == n {
				apiID = string(v)
				return nil
			}
			n++
		}

		return ErrNotFound
	})
	return
}

func (mbox *Mailbox) FromApiID(apiID string) (seqNum uint32, uid uint32, err error) {
	err = mbox.u.db.View(func(tx *bolt.Tx) error {
		b, err := mbox.bucket(tx)
		if err != nil {
			return err
		}

		want := []byte(apiID)
		c := b.Cursor()
		var n uint32 = 1
		for k, v := c.First(); k != nil; k, v = c.Next() {
			if bytes.Equal(v, want) {
				seqNum = n
				uid = unserializeUID(k)
				return nil
			}
			n++
		}

		return ErrNotFound
	})
	return
}

func (mbox *Mailbox) ForEach(f func(seqNum, uid uint32, apiID string) error) error {
	return mbox.u.db.View(func(tx *bolt.Tx) error {
		b, err := mbox.bucket(tx)
		if err != nil {
			return err
		}

		c := b.Cursor()
		var n uint32 = 1
		for k, v := c.First(); k != nil; k, v = c.Next() {
			if err := f(n, unserializeUID(k), string(v)); err != nil {
				return err
			}
			n++
		}

		return nil
	})
}
