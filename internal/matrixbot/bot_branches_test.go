package matrixbot

import (
	"fmt"
	"testing"
)

// registerClient builds a Client wired for the open-registration happy path,
// with a fresh recipient and the fake transport, for the error-branch tests
// that only need to reach past link building.
func registerClient(t *testing.T) (*Client, *fakeOps) {
	t.Helper()
	recipient, _ := genKeypair(t)
	f := newFakeOps()
	c := New(f)
	c.Recipient = recipient
	c.LinkBase = "https://dday.hs-ldz.pl/register"
	c.PanelBase = "https://dday.hs-ldz.pl/panel"
	c.TokenSecret = testSecret
	return c, f
}

// TestHandleMessageIgnoresNonCommand: a plain chat message that is not the
// command (and not an echo) is dropped — no DM, no send. This pins the
// "ignore non-command" guard in HandleMessage.
func TestHandleMessageIgnoresNonCommand(t *testing.T) {
	f := newFakeOps()
	c := New(f)
	c.HandleMessage(bg(), "!room:mock", "@alice:mock", "hej, jak leci?")
	if n := f.sendCount(); n != 0 {
		t.Errorf("SendHTML called %d time(s) for a non-command; want 0", n)
	}
	if n := f.createCount(); n != 0 {
		t.Errorf("CreateDM called %d time(s) for a non-command; want 0", n)
	}
}

// TestHandleMessageLogsRegisterError drives HandleMessage down the failure path:
// resolveDM fails (CreateDM errors), so HandleRegister returns an error that
// HandleMessage swallows after logging — the entry point never panics or blocks.
func TestHandleMessageLogsRegisterError(t *testing.T) {
	c, f := registerClient(t)
	f.createErr = fmt.Errorf("create boom")
	// Must not panic; the error is only logged inside HandleMessage.
	c.HandleMessage(bg(), "!room:mock", "@alice:mock", "!start")
	// A CreateDM was attempted (it bumps hits before failing), and because it
	// failed nothing was ever sent.
	if n := f.hitCount(); n == 0 {
		t.Error("expected the register path to touch the transport (a DM attempt)")
	}
	if n := f.sendCount(); n != 0 {
		t.Errorf("SendHTML called %d time(s) despite a DM failure; want 0", n)
	}
}

// TestHandleRegisterCheckErrorFailsOpen: a CheckRegistered that errors must not
// block registration — the bot logs and proceeds to issue a normal link.
func TestHandleRegisterCheckErrorFailsOpen(t *testing.T) {
	c, f := registerClient(t)
	c.CheckRegistered = func(string) (int, bool, error) {
		return 0, false, fmt.Errorf("web down")
	}
	room, err := c.HandleRegister(bg(), "!chan:mock", "@alice:mock")
	if err != nil {
		t.Fatalf("HandleRegister: %v", err)
	}
	if room != "!dm:mock" {
		t.Errorf("returned room %q; want the DM room (fail-open issues a link)", room)
	}
	var dm recordedSend
	for _, s := range f.sentMessages() {
		if s.room == "!dm:mock" {
			dm = s
		}
	}
	if dm.room == "" {
		t.Fatalf("no DM sent; fail-open must still deliver a registration link")
	}
	if !contains(dm.plain, "?t=") {
		t.Errorf("dm body = %q; want a registration link", dm.plain)
	}
}

// TestHandleRegisterResolveDMError: a DM that cannot be created surfaces as an
// error from the registration branch (and nothing is sent).
func TestHandleRegisterResolveDMError(t *testing.T) {
	c, f := registerClient(t)
	f.createErr = fmt.Errorf("create boom")
	if _, err := c.HandleRegister(bg(), "!chan:mock", "@alice:mock"); err == nil {
		t.Fatal("expected an error when the DM cannot be created")
	}
	if n := f.sendCount(); n != 0 {
		t.Errorf("SendHTML called %d time(s) after a DM failure; want 0", n)
	}
}

// TestHandleRegisterSendError: a failing DM send is reported as an error, and
// the returned room id is still the resolved DM (so the caller can log context).
func TestHandleRegisterSendError(t *testing.T) {
	c, f := registerClient(t)
	f.sendErr = fmt.Errorf("send boom")
	room, err := c.HandleRegister(bg(), "!chan:mock", "@alice:mock")
	if err == nil {
		t.Fatal("expected an error when the DM send fails")
	}
	if room != "!dm:mock" {
		t.Errorf("returned room %q; want the resolved DM room even on send failure", room)
	}
}

// TestSendPanelLinkResolveDMFallback: when the already-registered branch cannot
// open a DM, it degrades to the link-less public "already signed up" reply
// instead of erroring, so the user always gets an answer.
func TestSendPanelLinkResolveDMFallback(t *testing.T) {
	c, f := registerClient(t)
	c.CheckRegistered = func(string) (int, bool, error) { return 7, true, nil }
	f.createErr = fmt.Errorf("create boom")

	room, err := c.HandleRegister(bg(), "!chan:mock", "@alice:mock")
	if err != nil {
		t.Fatalf("HandleRegister: %v", err)
	}
	if room != "" {
		t.Errorf("returned room %q; want \"\" (fell back to a public reply)", room)
	}
	msgs := f.sentMessages()
	if len(msgs) != 1 || msgs[0].room != "!chan:mock" {
		t.Fatalf("sends = %+v; want a single public reply in the origin room", msgs)
	}
	if !contains(msgs[0].plain, "już zapisany") {
		t.Errorf("fallback body = %q; want the 'already signed up' reply", msgs[0].plain)
	}
	if contains(msgs[0].plain, "?t=") || contains(msgs[0].plain, "#7") {
		t.Errorf("fallback body = %q; must leak neither a link nor the number", msgs[0].plain)
	}
}

// TestSendPanelLinkSendErrorFallback: a panel DM that cannot be sent falls back
// to the public reply too — and since that public send also fails here, the
// error propagates (exercising both the sendPanelLink fallback and replyPublic's
// send-error path).
func TestSendPanelLinkSendErrorFallback(t *testing.T) {
	c, f := registerClient(t)
	c.CheckRegistered = func(string) (int, bool, error) { return 7, true, nil }
	f.sendErr = fmt.Errorf("send boom")

	if _, err := c.HandleRegister(bg(), "!chan:mock", "@alice:mock"); err == nil {
		t.Fatal("expected an error when both the DM and the public fallback send fail")
	}
}

// TestReplyPublicNoOriginRoom: a public reply with no origin room is a no-op —
// nothing is sent and no DM is spawned — rather than an error.
func TestReplyPublicNoOriginRoom(t *testing.T) {
	f := newFakeOps()
	c := New(f)
	room, err := c.replyPublic(bg(), "", "plain", "html")
	if err != nil {
		t.Fatalf("replyPublic: %v", err)
	}
	if room != "" {
		t.Errorf("replyPublic returned room %q; want \"\"", room)
	}
	if n := f.sendCount(); n != 0 {
		t.Errorf("SendHTML called %d time(s) with no origin room; want 0", n)
	}
}

// TestNudgeSendErrorSwallowed: the public nudge is best-effort — a send failure
// is logged, never propagated, so it cannot break the main DM flow.
func TestNudgeSendErrorSwallowed(t *testing.T) {
	f := newFakeOps()
	f.sendErr = fmt.Errorf("send boom")
	c := New(f)
	// Must not panic and returns nothing; the error is only logged.
	c.nudge(bg(), "!chan:mock", "!dm:mock", "@alice:mock", "tail")
}

// TestNudgeSkippedWhenOriginIsDM: no nudge is posted when the command already
// arrived from the DM itself (originRoom == dmRoom).
func TestNudgeSkippedWhenOriginIsDM(t *testing.T) {
	f := newFakeOps()
	c := New(f)
	c.nudge(bg(), "!dm:mock", "!dm:mock", "@alice:mock", "tail")
	if n := f.sendCount(); n != 0 {
		t.Errorf("nudge sent %d message(s) when origin is the DM; want 0", n)
	}
}

// TestLocalpartNoColon: an MXID-shaped string without a ":" server part is
// returned with only the leading "@" stripped.
func TestLocalpartNoColon(t *testing.T) {
	if got := localpart("@alice"); got != "alice" {
		t.Errorf("localpart(@alice) = %q; want alice", got)
	}
	if got := localpart("plain"); got != "plain" {
		t.Errorf("localpart(plain) = %q; want plain", got)
	}
}

// contains is a tiny substring helper local to this file's assertions.
func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
