// Package matrixbot implements the D-Day Matrix bot: it listens for the
// "!start" command (legacy alias: "!register") and opens a private DM with the
// sender containing a private link whose token is an age-encrypted timestamp —
// a registration link for a newcomer, a participant-panel magic link for
// someone who is already signed up.
//
// The logic lives in this importable package (not in package main) so the
// end-to-end test can drive the real code paths.
//
// The package is split by responsibility: this file holds the client type and
// the "!start" command handling, transport.go the Matrix HTTP calls, dm.go the
// DM resolution and its on-disk cache, policy.go the room allowlist and rate
// limit, announce.go the public registration announcements and token.go the
// link tokens.
package matrixbot

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"filippo.io/age"

	"github.com/d33mobile/dday/internal/regwindow"
)

// Client is a minimal Matrix client-server API client for the bot.
type Client struct {
	HS          string        // homeserver base URL, no trailing slash
	Token       string        // access token (set by Login)
	Self        string        // full MXID of the bot (set by Login)
	HTTP        *http.Client  // HTTP client
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

	// AllowedRooms, when non-empty, restricts which rooms Run reacts to
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
	mu        sync.Mutex
	dmRooms   map[string]string    // handle -> cached DM room id (reused across commands)
	lastSeen  map[string]time.Time // handle -> last acted-on "!start" time
	warnedEnc map[string]bool      // roomID -> already logged an "encrypted, unreadable" warning

	// lastAnnounced is the highest registration id already announced. It is
	// persisted with the DM cache so a restart does not re-announce.
	lastAnnounced int
	// baselined records that the announce loop has established its starting
	// point, so the "first run without state" branch runs at most once.
	baselined bool
}

// New returns a Client for the given homeserver.
func New(hs string) *Client {
	return &Client{
		HS:       strings.TrimRight(hs, "/"),
		HTTP:     &http.Client{Timeout: 60 * time.Second},
		dmRooms:  make(map[string]string),
		lastSeen: make(map[string]time.Time),
	}
}

// Run drives the sync loop, reacting to "!start" until ctx is cancelled.
func (c *Client) Run(ctx context.Context) error {
	// Prime with an initial sync so we only react to messages from now on,
	// not to historical backlog.
	first, err := c.sync("", 0)
	if err != nil {
		return fmt.Errorf("initial sync: %w", err)
	}
	since := first.NextBatch
	for roomID := range first.Rooms.Invite {
		c.joinRoom(roomID)
	}
	log.Printf("listening for !start (bot is %s)", c.Self)

	// Announcements run on their own clock, independent of the sync loop: the
	// registration completes on the web side, so the only way the bot learns
	// about it is by polling. The goroutine exits with ctx.
	if c.announcementsEnabled() {
		go c.announceLoop(ctx)
	} else {
		log.Printf("registration announcements disabled (no announce room or no feed)")
	}

	for {
		if ctx.Err() != nil {
			return nil
		}
		res, err := c.sync(since, 30000)
		if err != nil {
			log.Printf("sync error: %v (retrying in 5s)", err)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(5 * time.Second):
			}
			continue
		}
		since = res.NextBatch

		for roomID := range res.Rooms.Invite {
			log.Printf("invited to %s, joining", roomID)
			c.joinRoom(roomID)
		}
		for roomID, room := range res.Rooms.Join {
			if !c.roomAllowed(roomID) {
				continue
			}
			for _, ev := range room.Timeline.Events {
				if ev.Sender == c.Self {
					continue
				}
				// The bot has no E2EE support: an encrypted room's messages
				// arrive as m.room.encrypted with no readable body, so any
				// "!start" there is invisible. Element encrypts DMs by default,
				// so warn (once per room) instead of silently doing nothing.
				if ev.Type == "m.room.encrypted" {
					c.warnEncrypted(roomID)
					continue
				}
				if ev.Type != "m.room.message" || ev.Content.MsgType != "m.text" {
					continue
				}
				if !isStartCmd(ev.Content.Body) {
					continue
				}
				log.Printf("!start from %s in %s", ev.Sender, roomID)
				if _, err := c.HandleRegister(roomID, ev.Sender); err != nil {
					log.Printf("!start for %s failed: %v", ev.Sender, err)
				}
			}
		}
	}
}

// warnEncrypted logs, once per room, that the bot received an event it cannot
// read because the room is end-to-end encrypted. The bot has no E2EE support,
// so "!start" sent in an encrypted room (e.g. an Element-default encrypted DM)
// is invisible to it. Surfacing this in the logs turns a silent no-op into a
// diagnosable cause.
func (c *Client) warnEncrypted(roomID string) {
	c.mu.Lock()
	if c.warnedEnc == nil {
		c.warnedEnc = make(map[string]bool)
	}
	already := c.warnedEnc[roomID]
	c.warnedEnc[roomID] = true
	c.mu.Unlock()
	if !already {
		log.Printf("cannot read %s: it is end-to-end encrypted and the bot has no E2EE support "+
			"— users must send !start in an unencrypted room", roomID)
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
func (c *Client) HandleRegister(originRoom, user string) (string, error) {
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
			return c.sendPanelLink(originRoom, user, number)
		}
	}

	// Registration not open yet: reply publicly in originRoom with the start
	// time — no DM is opened.
	if c.IsOpen != nil && !c.IsOpen() {
		name := localpart(user)
		plain := fmt.Sprintf("Hej %s, zapisy jeszcze nie wystartowały — ruszają w %s. Wróć wtedy i napisz !start.", name, regwindow.OpenStartText())
		html := fmt.Sprintf("Hej %s, zapisy jeszcze nie wystartowały — ruszają w <b>%s</b>. Wróć wtedy i napisz <code>!start</code>.", mention(user), regwindow.OpenStartText())
		return c.replyPublic(originRoom, plain, html)
	}

	link, err := c.registerLink(user)
	if err != nil {
		return "", fmt.Errorf("build link: %w", err)
	}
	dmRoom, _, err := c.resolveDM(originRoom, user)
	if err != nil {
		return "", fmt.Errorf("resolve dm: %w", err)
	}
	plain := fmt.Sprintf("Cześć! Zapisy na D-Day (unconference w Hakierspejsie): %s", link)
	html := fmt.Sprintf("Cześć! Zapisy na <b>D-Day</b> (unconference w Hakierspejsie):<br><a href=%q>zarejestruj się</a>", link)
	if err := c.sendHTML(dmRoom, plain, html); err != nil {
		return dmRoom, fmt.Errorf("send: %w", err)
	}
	c.nudge(originRoom, dmRoom, user, "wysłałem Ci tam link do rejestracji.")
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
func (c *Client) sendPanelLink(originRoom, user string, number int) (string, error) {
	link, err := c.panelLink(user)
	if err != nil {
		log.Printf("panel link for %s: %v (falling back to public reply)", user, err)
		return c.replyAlreadyRegistered(originRoom, user)
	}
	dmRoom, _, err := c.resolveDM(originRoom, user)
	if err != nil {
		log.Printf("resolve dm for %s: %v (falling back to public reply)", user, err)
		return c.replyAlreadyRegistered(originRoom, user)
	}

	who := "Jesteś już zapisany 🎉"
	if number > 0 {
		who = fmt.Sprintf("Jesteś już zapisany 🎉 (#%d)", number)
	}
	plain := fmt.Sprintf("%s Oto link do Twojego panelu — możesz tam sprawdzić status i w razie czego wycofać udział: %s", who, link)
	html := fmt.Sprintf("%s Oto link do Twojego panelu — możesz tam sprawdzić status i w razie czego wycofać udział:<br><a href=%q>otwórz panel</a>", who, link)
	if err := c.sendHTML(dmRoom, plain, html); err != nil {
		log.Printf("send panel link to %s: %v (falling back to public reply)", user, err)
		return c.replyAlreadyRegistered(originRoom, user)
	}
	c.nudge(originRoom, dmRoom, user, "wysłałem Ci tam link do Twojego panelu.")
	return dmRoom, nil
}

// replyAlreadyRegistered is the link-less fallback for the already-registered
// branch: a public reply in originRoom that deliberately omits the participant
// number so it is not leaked in a channel.
func (c *Client) replyAlreadyRegistered(originRoom, user string) (string, error) {
	plain := fmt.Sprintf("Hej %s, jesteś już zapisany 🎉 Ponowna rejestracja nie jest możliwa.", localpart(user))
	html := fmt.Sprintf("Hej %s, jesteś już zapisany 🎉 Ponowna rejestracja nie jest możliwa.", mention(user))
	return c.replyPublic(originRoom, plain, html)
}

// replyPublic sends a public reply into originRoom (the channel the "!start"
// came from). When originRoom is unknown ("") it only logs — nothing is sent
// and no DM is ever opened — so a command from an unknown context is a no-op
// rather than spawning a private chat. It always returns ("", nil): the caller
// exposes "no DM room was created".
func (c *Client) replyPublic(originRoom, plain, html string) (string, error) {
	if originRoom == "" {
		log.Printf("replyPublic: no origin room, dropping message: %s", plain)
		return "", nil
	}
	if err := c.sendHTML(originRoom, plain, html); err != nil {
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
func (c *Client) nudge(originRoom, dmRoom, user, tail string) {
	if originRoom == "" || originRoom == dmRoom {
		return
	}
	name := localpart(user)
	plain := fmt.Sprintf("Hej %s, sprawdź prywatne wiadomości - %s", name, tail)
	html := fmt.Sprintf("Hej <a href=%q>%s</a>, sprawdź prywatne wiadomości - %s",
		"https://matrix.to/#/"+user, name, tail)
	if err := c.sendHTML(originRoom, plain, html); err != nil {
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
