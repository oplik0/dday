package matrixbot

import (
	"strings"
	"testing"

	"github.com/d33mobile/dday/internal/regwindow"
)

// TestHandleRegisterClosed asserts that when the registration gate is closed
// (IsOpen returns false), the bot does NOT open a DM: it replies publicly in
// the origin room with a "not started yet" notice carrying the start date and
// no registration link (no "?t=" token).
func TestHandleRegisterClosed(t *testing.T) {
	recipient, _ := genKeypair(t)

	f := newFakeOps()
	c := New(f)
	c.Recipient = recipient
	c.LinkBase = "https://dday.hs-ldz.pl/register"
	// PanelBase is configured on purpose: the closed branch must stay a plain
	// public reply, never a magic link, even when panel links are available.
	c.PanelBase = "https://dday.hs-ldz.pl/panel"
	c.TokenSecret = testSecret
	c.IsOpen = func() bool { return false }

	room, err := c.HandleRegister(bg(), "!chan:mock", "@alice:mock")
	if err != nil {
		t.Fatalf("HandleRegister: %v", err)
	}
	if room != "" {
		t.Errorf("HandleRegister returned room %q, want \"\" (no DM opened)", room)
	}
	if n := f.createCount(); n != 0 {
		t.Fatalf("closed registration must NOT create a DM room; got %d creates", n)
	}

	msgs := f.sentMessages()
	if len(msgs) != 1 {
		t.Fatalf("sends = %d; want 1 public reply", len(msgs))
	}
	body := msgs[0].plain
	if !strings.Contains(body, "nie wystartowały") && !strings.Contains(body, "jeszcze") {
		t.Errorf("message body = %q, want the 'not started yet' notice", body)
	}
	if !strings.Contains(body, regwindow.OpenStartText()) {
		t.Errorf("message body = %q, want the start date %q", body, regwindow.OpenStartText())
	}
	if strings.Contains(body, "?t=") {
		t.Errorf("message body = %q, must NOT contain a registration link (?t=)", body)
	}
	if strings.Contains(msgs[0].html, "?t=") {
		t.Errorf("formatted_body = %q, must NOT contain a registration link (?t=)", msgs[0].html)
	}
}
