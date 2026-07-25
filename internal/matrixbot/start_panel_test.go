package matrixbot

import (
	"net/url"
	"strings"
	"testing"
)

// TestIsStartCmd pins down which messages trigger the bot: "!start" is the
// official command and "!register" is kept as a legacy alias, both
// case-insensitively and regardless of trailing words. Anything else — another
// bang-command, the word embedded mid-sentence, an empty body — is ignored.
func TestIsStartCmd(t *testing.T) {
	cases := []struct {
		body string
		want bool
	}{
		{"!start", true},
		{"  !start  ", true},
		{"!START", true},
		{"!Start please", true},
		{"!register", true},
		{"!REGISTER", true},
		{"!register now", true},
		{"!stop", false},
		{"!starting", false},
		{"!startup", false},
		{"start", false},
		{"hej, napisz !start do bota", false},
		{"", false},
		{"   ", false},
	}
	for _, tc := range cases {
		if got := isStartCmd(tc.body); got != tc.want {
			t.Errorf("isStartCmd(%q) = %v; want %v", tc.body, got, tc.want)
		}
	}
}

// TestHandleRegisterAlreadyRegisteredSendsPanelLink asserts the already-signed-up
// branch: with PanelBase configured the bot does NOT publicly refuse. It DMs the
// user a magic link to their panel — whose token must decode to KindPanel for
// that very handle — and leaves only a public nudge in the origin room. The
// nudge must not carry the link, and the participant number must stay out of the
// channel.
func TestHandleRegisterAlreadyRegisteredSendsPanelLink(t *testing.T) {
	recipient, identity := genKeypair(t)

	f := newFakeOps()
	c := New(f)
	c.Recipient = recipient
	c.LinkBase = "https://dday.hs-ldz.pl/register"
	c.PanelBase = "https://dday.hs-ldz.pl/panel"
	c.TokenSecret = testSecret
	c.CheckRegistered = func(string) (int, bool, error) { return 7, true, nil }

	room, err := c.HandleRegister(bg(), "!chan:mock", "@alice:mock")
	if err != nil {
		t.Fatalf("HandleRegister: %v", err)
	}
	if room != "!dm:mock" {
		t.Errorf("HandleRegister returned room %q; want the DM room", room)
	}

	var dm, nudge recordedSend
	for _, s := range f.sentMessages() {
		if s.room == "!chan:mock" {
			nudge = s
		} else {
			dm = s
		}
	}
	if dm.room == "" || nudge.room == "" {
		t.Fatalf("expected both a DM and a nudge; got %+v", f.sentMessages())
	}

	// The DM carries the panel magic link and (privately) the participant number.
	if !strings.Contains(dm.plain, "https://dday.hs-ldz.pl/panel?t=") {
		t.Fatalf("dm body = %q; want a /panel?t= magic link", dm.plain)
	}
	if !strings.Contains(dm.plain, "#7") {
		t.Errorf("dm body = %q; want the participant number", dm.plain)
	}
	if strings.Contains(dm.plain, "nie jest możliwa") {
		t.Errorf("dm body = %q; must not be the old refusal", dm.plain)
	}
	if !strings.Contains(dm.html, `<a href="https://dday.hs-ldz.pl/panel?t=`) {
		t.Errorf("dm formatted_body = %q; want an <a href> to the panel", dm.html)
	}

	// The token in that link must be a panel token for exactly this handle.
	payload, err := DecodeRegToken(identity, testSecret, tokenFromLink(t, dm.plain))
	if err != nil {
		t.Fatalf("DecodeRegToken: %v", err)
	}
	if payload.Kind != KindPanel {
		t.Errorf("token kind = %q; want %q", payload.Kind, KindPanel)
	}
	if payload.Handle != "@alice:mock" {
		t.Errorf("token handle = %q; want @alice:mock", payload.Handle)
	}

	// The channel gets a nudge only: no link, no participant number.
	if !strings.Contains(nudge.plain, "sprawdź prywatne wiadomości") {
		t.Errorf("nudge body = %q; want 'sprawdź prywatne wiadomości'", nudge.plain)
	}
	if !strings.Contains(nudge.plain, "link do Twojego panelu") {
		t.Errorf("nudge body = %q; want it to mention the panel link", nudge.plain)
	}
	if strings.Contains(nudge.plain, "?t=") || strings.Contains(nudge.plain, "#7") {
		t.Errorf("nudge body = %q; must leak neither the link nor the number", nudge.plain)
	}
	if strings.Contains(nudge.html, "?t=") {
		t.Errorf("nudge formatted_body = %q; must NOT contain the magic link", nudge.html)
	}
}

// tokenFromLink pulls the `t` query parameter out of the first URL found in a
// message body, undoing the query escaping applied when the link was built.
func tokenFromLink(t *testing.T, body string) string {
	t.Helper()
	i := strings.Index(body, "https://")
	if i < 0 {
		t.Fatalf("no link in %q", body)
	}
	raw := strings.Fields(body[i:])[0]
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse link %q: %v", raw, err)
	}
	tok := u.Query().Get("t")
	if tok == "" {
		t.Fatalf("link %q carries no t= token", raw)
	}
	return tok
}
