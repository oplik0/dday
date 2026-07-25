package matrixbot

import (
	"strings"
	"testing"
)

// TestRegisterInExistingDMRepliesThere asserts that a "!register" arriving in a
// room that is already a 1:1 DM with the user (joined_members == {bot, user}) is
// answered in that very room: the bot must NOT create a new room and must NOT
// post a public nudge (there is no separate channel to point back to).
func TestRegisterInExistingDMRepliesThere(t *testing.T) {
	recipient, _ := genKeypair(t)

	const user = "@alice:mock"
	const dm = "!existingdm:mock"

	f := newFakeOps()
	f.setMembers(dm, botSelf, user) // the origin room is a 1:1 with the user
	c := New(f)
	c.Recipient = recipient
	c.LinkBase = "https://dday.hs-ldz.pl/register"
	c.TokenSecret = testSecret

	room, err := c.HandleRegister(bg(), dm, user)
	if err != nil {
		t.Fatalf("HandleRegister: %v", err)
	}
	if room != dm {
		t.Errorf("HandleRegister room = %q; want the existing DM %q", room, dm)
	}

	// Exactly one send, into the existing DM, carrying the link — no nudge.
	msgs := f.sentMessages()
	if len(msgs) != 1 {
		t.Fatalf("sends = %d; want exactly 1 (nudge must be skipped in an existing DM)", len(msgs))
	}
	if msgs[0].room != dm {
		t.Errorf("send room = %q; want the existing DM %q", msgs[0].room, dm)
	}
	if !strings.Contains(msgs[0].plain, "?t=") {
		t.Errorf("send body = %q; want the registration link", msgs[0].plain)
	}
	if n := f.createCount(); n != 0 {
		t.Errorf("CreateDM calls = %d; want 0 (existing DM reused)", n)
	}
}

// TestExistingOrNewDMReusesMDirect asserts that when the m.direct account data
// already records a DM room for the user (e.g. after a bot restart, before the
// in-memory cache is warm), existingOrNewDM returns that room without creating a
// new one, and warms the cache with it.
func TestExistingOrNewDMReusesMDirect(t *testing.T) {
	const user = "@alice:mock"
	const dm = "!frommdirect:mock"

	f := newFakeOps()
	f.direct[user] = []string{dm}
	c := New(f)

	room, err := c.existingOrNewDM(bg(), user)
	if err != nil {
		t.Fatalf("existingOrNewDM: %v", err)
	}
	if room != dm {
		t.Errorf("existingOrNewDM room = %q; want the m.direct room %q", room, dm)
	}
	if n := f.createCount(); n != 0 {
		t.Errorf("CreateDM calls = %d; want 0 (reused from m.direct)", n)
	}
	if got := c.dmRooms[user]; got != dm {
		t.Errorf("dmRooms[%s] = %q; want the room warmed into cache %q", user, got, dm)
	}
}
