package matrixbot

import (
	"net/url"
	"strings"
	"testing"
	"time"
)

// TestRegisterFlowEndToEnd drives the full "!start" flow through HandleMessage
// against the fake transport and asserts:
//
//	see !start from a user in a public room -> create a direct (DM) room
//	inviting that user -> send a message containing a registration link whose
//	token decrypts (with the private key) back to the sender's handle plus a
//	fresh unix timestamp -> post a public nudge back in the origin room.
func TestRegisterFlowEndToEnd(t *testing.T) {
	recipient, identity := genKeypair(t)

	const userMXID = "@alice:mock"
	const originRoom = "!room:mock"

	f := newFakeOps()
	c := New(f)
	c.Recipient = recipient
	c.LinkBase = "https://dday.hs-ldz.pl/register"
	c.TokenSecret = testSecret

	c.HandleMessage(bg(), originRoom, userMXID, "!start")

	// The bot must open exactly one DM inviting the user (is_direct is implicit
	// in CreateDM; the invitee is the recorded argument).
	if got := f.createCount(); got != 1 {
		t.Fatalf("CreateDM calls = %d; want 1", got)
	}
	if f.creates[0] != userMXID {
		t.Errorf("CreateDM invited %q; want %q", f.creates[0], userMXID)
	}

	// Two sends: the DM (with the link) and the public nudge in the origin room.
	msgs := f.sentMessages()
	if len(msgs) != 2 {
		t.Fatalf("sends = %d; want 2 (DM + nudge)", len(msgs))
	}
	var dm recordedSend
	for _, m := range msgs {
		if m.room == "!dm:mock" {
			dm = m
		}
	}
	if dm.room == "" {
		t.Fatalf("no send to the DM room among %+v", msgs)
	}

	link := lastField(dm.plain)
	u, err := url.Parse(link)
	if err != nil {
		t.Fatalf("parse link %q: %v", link, err)
	}
	token := u.Query().Get("t")
	if token == "" {
		t.Fatalf("no token in link %q", link)
	}

	payload, err := DecodeRegToken(identity, testSecret, token)
	if err != nil {
		t.Fatalf("decode token: %v", err)
	}
	if payload.Handle != userMXID {
		t.Fatalf("token handle = %q, want %q", payload.Handle, userMXID)
	}
	if delta := time.Now().Unix() - payload.Issued; delta < 0 || delta > 300 {
		t.Fatalf("token issued %d not fresh (delta %ds)", payload.Issued, delta)
	}
}

func lastField(s string) string {
	f := strings.Fields(s)
	if len(f) == 0 {
		return ""
	}
	return f[len(f)-1]
}
