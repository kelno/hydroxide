package smtp

import (
	"reflect"
	"testing"

	"github.com/emersion/go-message/mail"
)

func TestReplyToIDs(t *testing.T) {
	var h mail.Header
	h.Set("In-Reply-To", "<c@example.com>")
	h.Set("References", "<a@example.com> <b@example.com>\r\n <c@example.com>")

	got := replyToIDs(h)
	want := []string{"c@example.com", "b@example.com", "a@example.com"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("replyToIDs() = %q, want %q", got, want)
	}

	if ids := replyToIDs(mail.Header{}); len(ids) != 0 {
		t.Errorf("replyToIDs() of a new message = %q, want none", ids)
	}
}
