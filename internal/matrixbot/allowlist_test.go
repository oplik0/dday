package matrixbot

import (
	"reflect"
	"testing"
)

func TestParseAllowedRooms(t *testing.T) {
	got := ParseAllowedRooms(" !a:hs , ,!b:hs,  ")
	want := map[string]bool{"!a:hs": true, "!b:hs": true}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ParseAllowedRooms = %v; want %v", got, want)
	}
	if m := ParseAllowedRooms(""); len(m) != 0 {
		t.Errorf("empty input = %v; want empty set", m)
	}
	if m := ParseAllowedRooms("   ,  , "); len(m) != 0 {
		t.Errorf("blank-only input = %v; want empty set", m)
	}
}

func TestRoomAllowed(t *testing.T) {
	c := New(newFakeOps())
	// No allowlist configured: every room is allowed.
	if !c.roomAllowed("!any:hs") {
		t.Error("with no allowlist every room must be allowed")
	}
	c.AllowedRooms = ParseAllowedRooms("!ok:hs")
	if !c.roomAllowed("!ok:hs") {
		t.Error("a listed room must be allowed")
	}
	if c.roomAllowed("!other:hs") {
		t.Error("an unlisted room must be rejected")
	}
}

// TestHandleMessageIgnoresDisallowedRoom drives the message entry point with an
// allowlist that does NOT include the room the "!register" (legacy alias)
// arrives in, and asserts the bot takes no action: no DM is created and nothing
// is sent.
func TestHandleMessageIgnoresDisallowedRoom(t *testing.T) {
	recipient, _ := genKeypair(t)

	f := newFakeOps()
	c := New(f)
	c.Recipient = recipient
	c.LinkBase = "https://dday.hs-ldz.pl/register"
	c.TokenSecret = testSecret
	c.AllowedRooms = ParseAllowedRooms("!different:mock") // NOT !room:mock

	c.HandleMessage(bg(), "!room:mock", "@alice:mock", "!register")

	if n := f.createCount(); n != 0 {
		t.Errorf("CreateDM called %d time(s); want 0 (origin room not on allowlist)", n)
	}
	if n := f.sendCount(); n != 0 {
		t.Errorf("SendHTML called %d time(s); want 0 (origin room not on allowlist)", n)
	}
}

// TestHandleMessageIgnoresOwnEcho asserts the bot never reacts to its own
// messages, even a literal "!start" it might post itself.
func TestHandleMessageIgnoresOwnEcho(t *testing.T) {
	f := newFakeOps()
	c := New(f)
	c.HandleMessage(bg(), "!room:mock", botSelf, "!start")
	if n := f.sendCount(); n != 0 {
		t.Errorf("SendHTML called %d time(s) for the bot's own echo; want 0", n)
	}
}
