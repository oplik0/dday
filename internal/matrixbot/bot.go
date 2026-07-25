// Package matrixbot implements the D-Day Matrix bot: it listens for the
// "!start" command (legacy alias: "!register") and opens a private DM with the
// sender containing a private link whose token is an age-encrypted timestamp —
// a registration link for a newcomer, a participant-panel magic link for
// someone who is already signed up.
//
// The bot speaks the Matrix client-server API — including end-to-end encrypted
// rooms — through the mautrix library with pure-Go olm (build with -tags
// goolm). A user's "!start" in an Element-default encrypted DM is decrypted,
// handled by the exact same logic as a plaintext one, and answered with an
// encrypted reply.
//
// The design keeps all command logic (this file, dm.go, announce.go, policy.go,
// token.go) transport-agnostic: it operates on the matrixOps interface (ops.go)
// plus the client's own state. The concrete Matrix transport is the mxClient
// adapter (mautrix.go), injected via New. That split keeps the whole flow
// unit-testable with an in-memory fake and drove the original test suite.
package matrixbot

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"filippo.io/age"

	"github.com/d33mobile/dday/internal/regwindow"
)

// Client holds the bot's configuration and mutable state and drives the command
// logic against a matrixOps transport.
type Client struct {
	ops matrixOps // Matrix transport (production: mxClient; tests: a fake)

	Recipient   age.Recipient // age recipient used to encrypt the link token
	LinkBase    string        // registration URL the token is appended to
	PanelBase   string        // participant-panel URL the panel token is appended to
	TokenSecret string        // shared HMAC key authenticating the link token
	IsOpen      func() bool   // registration time gate; nil means always open

	// CheckRegistered reports whether a handle is already registered (and its
	// participant number), by asking the web service. nil skips the check;
	// any error is treated fail-open (proceed to issue a link, since the web
	// POST dedupes anyway).
	CheckRegistered func(handle string) (number int, registered bool, err error)

	// AllowedRooms, when non-empty, restricts which rooms the bot reacts to
	// "!start" in. An empty (or nil) set reacts everywhere — the default,
	// unchanged behaviour. Populate it from the ALLOWED_ROOMS env var.
	AllowedRooms map[string]bool

	// RateWindow is the minimum interval between two "!start" from the same
	// handle that the bot acts on; commands inside the window are ignored. Zero
	// uses defaultRateWindow.
	RateWindow time.Duration

	// CachePath, when non-empty, is the path to a JSON file that persists the
	// dmRooms handle->roomID map across restarts. LoadCache reads it at startup
	// and cacheDM rewrites it (atomically) on every change. Empty disables all
	// disk I/O — behaviour is exactly as if the cache were purely in-memory.
	CachePath string

	// AnnounceRoom is the room new registrations are announced in (typically the
	// home channel, MATRIX_ROOM). Empty disables announcements entirely.
	AnnounceRoom string

	// AnnounceInterval is how often the announce loop polls the web service for
	// new registrations. Zero or negative uses defaultAnnounceInterval.
	AnnounceInterval time.Duration

	// FetchNewRegistrations returns the registrations with id > sinceID, in id
	// order, by querying the web service's internal feed. nil disables
	// announcements (as does an empty AnnounceRoom).
	FetchNewRegistrations func(sinceID int) ([]NewRegistration, error)

	// now returns the current time; overridable in tests to drive the rate
	// limiter deterministically. nil means time.Now.
	now func() time.Time

	// mu guards the anti-spam maps below.
	mu       sync.Mutex
	dmRooms  map[string]string    // handle -> cached DM room id (reused across commands)
	lastSeen map[string]time.Time // handle -> last acted-on "!start" time

	// lastAnnounced is the highest registration id already announced. It is
	// persisted with the DM cache so a restart does not re-announce.
	lastAnnounced int
	// baselined records that the announce loop has established its starting
	// point, so the "first run without state" branch runs at most once.
	baselined bool
}

// New returns a Client driving the given Matrix transport.
func New(ops matrixOps) *Client {
	return &Client{
		ops:      ops,
		dmRooms:  make(map[string]string),
		lastSeen: make(map[string]time.Time),
	}
}

// Self returns the bot's own MXID via the transport.
func (c *Client) Self() string { return c.ops.Self() }

// HandleMessage is the entry point the transport calls for every (already
// decrypted) text message it observes. It applies the room allowlist, ignores
// the bot's own echoes and non-command messages, and dispatches a "!start" to
// HandleRegister. The reply is sent back into the same room's encryption
// context: encrypted where the room is encrypted, plaintext otherwise.
func (c *Client) HandleMessage(ctx context.Context, roomID, sender, body string) {
	if !c.roomAllowed(roomID) {
		return
	}
	if sender == c.Self() {
		return
	}
	if !isStartCmd(body) {
		return
	}
	log.Printf("!start from %s in %s", sender, roomID)
	if _, err := c.HandleRegister(ctx, roomID, sender); err != nil {
		log.Printf("!start for %s failed: %v", sender, err)
	}
}

// isStartCmd reports whether body's first word is the bot's command. "!start"
// is the official command; "!register" stays accepted as a legacy alias,
// because it was documented on the landing page before the rename and people
// still have it in their muscle memory. Matching is case-insensitive and
// ignores anything after the first word.
func isStartCmd(body string) bool {
	f := strings.Fields(strings.TrimSpace(body))
	return len(f) > 0 && (strings.EqualFold(f[0], "!start") || strings.EqualFold(f[0], "!register"))
}

// HandleRegister reacts to a "!start" that arrived in originRoom from user.
//
// A private DM is opened for one reason only: to deliver a private link. The
// branches are:
//
//   - already registered: a DM with the participant-panel magic link (where the
//     user can check their status and withdraw), plus a public nudge in
//     originRoom. If no panel link can be built (PanelBase unset or an encoding
//     error) it degrades to a public "you are already signed up" reply, so the
//     command never fails silently;
//   - not open yet (per c.IsOpen): a public "sign-ups haven't started" reply
//     carrying the start time — no DM is opened;
//   - open: a DM with the registration link, plus a public nudge back in
//     originRoom pointing the user at their DMs.
//
// For the public branches an empty originRoom means the reply is only logged.
// It returns the DM room id for the DM branches, and "" for the public ones.
func (c *Client) HandleRegister(ctx context.Context, originRoom, user string) (string, error) {
	// Per-handle rate limit: drop a burst of "!start" from the same user so a
	// single command yields a single DM. Returns an empty room id, no error.
	if c.rateLimited(user) {
		log.Printf("!start from %s ignored (rate limited)", user)
		return "", nil
	}

	// Already-registered wins over everything. Fail-open: on error we log and
	// fall through to the normal flow, since the web POST dedupes anyway.
	if c.CheckRegistered != nil {
		number, registered, err := c.CheckRegistered(user)
		if err != nil {
			log.Printf("check registered for %s: %v (proceeding)", user, err)
		} else if registered {
			return c.sendPanelLink(ctx, originRoom, user, number)
		}
	}

	// Registration not open yet: reply publicly in originRoom with the start
	// time — no DM is opened.
	if c.IsOpen != nil && !c.IsOpen() {
		name := localpart(user)
		plain := fmt.Sprintf("Hej %s, zapisy jeszcze nie wystartowały — ruszają w %s. Wróć wtedy i napisz !start.", name, regwindow.OpenStartText())
		html := fmt.Sprintf("Hej %s, zapisy jeszcze nie wystartowały — ruszają w <b>%s</b>. Wróć wtedy i napisz <code>!start</code>.", mention(user), regwindow.OpenStartText())
		return c.replyPublic(ctx, originRoom, plain, html)
	}

	link, err := c.registerLink(user)
	if err != nil {
		return "", fmt.Errorf("build link: %w", err)
	}
	dmRoom, _, err := c.resolveDM(ctx, originRoom, user)
	if err != nil {
		return "", fmt.Errorf("resolve dm: %w", err)
	}
	plain := fmt.Sprintf("Cześć! Zapisy na D-Day (unconference w Hakierspejsie): %s", link)
	html := fmt.Sprintf("Cześć! Zapisy na <b>D-Day</b> (unconference w Hakierspejsie):<br><a href=%q>zarejestruj się</a>", link)
	if err := c.ops.SendHTML(ctx, dmRoom, plain, html); err != nil {
		return dmRoom, fmt.Errorf("send: %w", err)
	}
	c.nudge(ctx, originRoom, dmRoom, user, "wysłałem Ci tam link do rejestracji.")
	return dmRoom, nil
}

// sendPanelLink handles the already-registered branch: it DMs user a magic link
// to their participant panel (status + withdrawing) and nudges them publicly in
// originRoom. The link is private, so delivering it by DM keeps the "a DM is
// only ever opened to deliver a private link" rule; the participant number is
// mentioned in the DM only, never in the channel.
//
// Every failure degrades to the old public "you are already signed up" reply
// rather than an error: an unconfigured PanelBase or a token/DM problem must
// not leave the user without an answer.
func (c *Client) sendPanelLink(ctx context.Context, originRoom, user string, number int) (string, error) {
	link, err := c.panelLink(user)
	if err != nil {
		log.Printf("panel link for %s: %v (falling back to public reply)", user, err)
		return c.replyAlreadyRegistered(ctx, originRoom, user)
	}
	dmRoom, _, err := c.resolveDM(ctx, originRoom, user)
	if err != nil {
		log.Printf("resolve dm for %s: %v (falling back to public reply)", user, err)
		return c.replyAlreadyRegistered(ctx, originRoom, user)
	}

	who := "Jesteś już zapisany 🎉"
	if number > 0 {
		who = fmt.Sprintf("Jesteś już zapisany 🎉 (#%d)", number)
	}
	plain := fmt.Sprintf("%s Oto link do Twojego panelu — możesz tam sprawdzić status i w razie czego wycofać udział: %s", who, link)
	html := fmt.Sprintf("%s Oto link do Twojego panelu — możesz tam sprawdzić status i w razie czego wycofać udział:<br><a href=%q>otwórz panel</a>", who, link)
	if err := c.ops.SendHTML(ctx, dmRoom, plain, html); err != nil {
		log.Printf("send panel link to %s: %v (falling back to public reply)", user, err)
		return c.replyAlreadyRegistered(ctx, originRoom, user)
	}
	c.nudge(ctx, originRoom, dmRoom, user, "wysłałem Ci tam link do Twojego panelu.")
	return dmRoom, nil
}

// replyAlreadyRegistered is the link-less fallback for the already-registered
// branch: a public reply in originRoom that deliberately omits the participant
// number so it is not leaked in a channel.
func (c *Client) replyAlreadyRegistered(ctx context.Context, originRoom, user string) (string, error) {
	plain := fmt.Sprintf("Hej %s, jesteś już zapisany 🎉 Ponowna rejestracja nie jest możliwa.", localpart(user))
	html := fmt.Sprintf("Hej %s, jesteś już zapisany 🎉 Ponowna rejestracja nie jest możliwa.", mention(user))
	return c.replyPublic(ctx, originRoom, plain, html)
}

// replyPublic sends a public reply into originRoom (the channel the "!start"
// came from). When originRoom is unknown ("") it only logs — nothing is sent
// and no DM is ever opened — so a command from an unknown context is a no-op
// rather than spawning a private chat. It always returns ("", nil): the caller
// exposes "no DM room was created".
func (c *Client) replyPublic(ctx context.Context, originRoom, plain, html string) (string, error) {
	if originRoom == "" {
		log.Printf("replyPublic: no origin room, dropping message: %s", plain)
		return "", nil
	}
	if err := c.ops.SendHTML(ctx, originRoom, plain, html); err != nil {
		return "", fmt.Errorf("send: %w", err)
	}
	return "", nil
}

// mention renders an @-mention of user as a matrix.to HTML anchor whose text is
// the localpart, matching the ping pattern used by nudge so the user is
// notified.
func mention(user string) string {
	return fmt.Sprintf("<a href=%q>%s</a>", "https://matrix.to/#/"+user, localpart(user))
}

// nudge posts a short public reply in originRoom (the channel the "!start"
// came from), @-mentioning the user and pointing them at their DMs. It is a
// best-effort side channel: it is skipped when originRoom is unknown or is the
// DM itself, and any send error is only logged.
func (c *Client) nudge(ctx context.Context, originRoom, dmRoom, user, tail string) {
	if originRoom == "" || originRoom == dmRoom {
		return
	}
	name := localpart(user)
	plain := fmt.Sprintf("Hej %s, sprawdź prywatne wiadomości - %s", name, tail)
	html := fmt.Sprintf("Hej <a href=%q>%s</a>, sprawdź prywatne wiadomości - %s",
		"https://matrix.to/#/"+user, name, tail)
	if err := c.ops.SendHTML(ctx, originRoom, plain, html); err != nil {
		log.Printf("nudge in %s: %v", originRoom, err)
	}
}

// localpart turns a Matrix MXID "@alice:hs.org" into "alice"; any other shape
// is returned unchanged.
func localpart(mxid string) string {
	s := strings.TrimPrefix(mxid, "@")
	if i := strings.IndexByte(s, ':'); i > 0 {
		return s[:i]
	}
	return s
}
