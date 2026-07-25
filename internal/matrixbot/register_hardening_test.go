package matrixbot

import (
	"strings"
	"testing"
)

// TestHandleRegisterAlreadyRegisteredNoPanel asserts the fallback of the
// already-registered branch: with no PanelBase configured no magic link can be
// built, so the bot replies publicly in the origin room with a
// "re-registration impossible" notice — it does NOT open a DM, does NOT hand
// out a link (no "?t="), and does NOT leak the participant number.
func TestHandleRegisterAlreadyRegisteredNoPanel(t *testing.T) {
	recipient, _ := genKeypair(t)

	f := newFakeOps()
	c := New(f)
	c.Recipient = recipient
	c.LinkBase = "https://dday.hs-ldz.pl/register"
	c.TokenSecret = testSecret
	c.CheckRegistered = func(string) (int, bool, error) { return 7, true, nil }

	room, err := c.HandleRegister(bg(), "!chan:mock", "@alice:mock")
	if err != nil {
		t.Fatalf("HandleRegister: %v", err)
	}
	if room != "" {
		t.Errorf("HandleRegister returned room %q, want \"\" (no DM opened)", room)
	}
	if n := f.createCount(); n != 0 {
		t.Fatalf("already-registered must NOT create a DM room; got %d creates", n)
	}

	msgs := f.sentMessages()
	if len(msgs) != 1 {
		t.Fatalf("sends = %d; want 1 public reply", len(msgs))
	}
	sb := msgs[0]
	if sb.room != "!chan:mock" {
		t.Errorf("reply room = %q; want the origin channel", sb.room)
	}
	if !strings.Contains(sb.plain, "już zapisany") {
		t.Errorf("body = %q; want 'już zapisany'", sb.plain)
	}
	if !strings.Contains(sb.plain, "nie jest możliwa") {
		t.Errorf("body = %q; want 'nie jest możliwa'", sb.plain)
	}
	if strings.Contains(sb.plain, "#7") {
		t.Errorf("body = %q; must NOT reveal the participant number in public", sb.plain)
	}
	if strings.Contains(sb.plain, "?t=") {
		t.Errorf("body = %q; must NOT contain a registration link", sb.plain)
	}
	if strings.Contains(sb.html, "?t=") {
		t.Errorf("formatted_body = %q; must NOT contain a registration link", sb.html)
	}
	if !strings.Contains(sb.html, "matrix.to/#/@alice:mock") {
		t.Errorf("formatted_body = %q; want a matrix.to mention of the user", sb.html)
	}
}

// TestHandleRegisterNudge asserts that a "!register" arriving in a public room
// produces both a DM (with the link) and a public nudge in the origin room that
// @-mentions the user and points them at their DMs.
func TestHandleRegisterNudge(t *testing.T) {
	recipient, _ := genKeypair(t)

	f := newFakeOps()
	c := New(f)
	c.Recipient = recipient
	c.LinkBase = "https://dday.hs-ldz.pl/register"
	c.TokenSecret = testSecret

	if _, err := c.HandleRegister(bg(), "!chan:mock", "@alice:mock"); err != nil {
		t.Fatalf("HandleRegister: %v", err)
	}

	var dm, nudge recordedSend
	for _, s := range f.sentMessages() {
		if s.room == "!chan:mock" {
			nudge = s
		} else {
			dm = s
		}
	}
	if nudge.room != "!chan:mock" {
		t.Fatalf("expected a public nudge in !chan:mock, got none")
	}
	if !strings.Contains(nudge.plain, "sprawdź prywatne wiadomości") {
		t.Errorf("nudge body = %q; want 'sprawdź prywatne wiadomości'", nudge.plain)
	}
	if !strings.Contains(nudge.html, "matrix.to/#/@alice:mock") {
		t.Errorf("nudge formatted_body = %q; want a matrix.to mention of the user", nudge.html)
	}
	if !strings.Contains(dm.plain, "?t=") {
		t.Errorf("dm body = %q; want the registration link", dm.plain)
	}
}
