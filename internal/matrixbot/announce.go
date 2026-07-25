// Public announcements of new registrations: the polling loop, one poll-and-post
// cycle and the message rendering.

package matrixbot

import (
	"context"
	"fmt"
	"log"
	"time"
)

// NewRegistration is one signup as reported by the web service's internal
// registrations feed. It intentionally carries no e-mail or city: announcements
// go to a public room, so personal data must not even be transported here.
// The json tags match the web service's /api/registrations feed.
type NewRegistration struct {
	ID          int    `json:"id"`
	Handle      string `json:"handle"`
	Nick        string `json:"nick"`
	Rank        int    `json:"rank"`
	Confirmed   bool   `json:"confirmed"`
	WaitlistPos int    `json:"waitlistPos"`
}

// defaultAnnounceInterval is how often the announce loop polls the web service
// for registrations it has not announced yet.
const defaultAnnounceInterval = 30 * time.Second

// announcementsEnabled reports whether the bot should announce new signups: it
// needs both a target room and a way to fetch them.
func (c *Client) announcementsEnabled() bool {
	return c.AnnounceRoom != "" && c.FetchNewRegistrations != nil
}

// announceLoop polls for new registrations every AnnounceInterval and posts
// them in AnnounceRoom, until ctx is done. It owns no logic beyond the ticking:
// one poll-and-post cycle is announceOnce, which tests drive directly.
func (c *Client) announceLoop(ctx context.Context) {
	every := c.AnnounceInterval
	if every <= 0 {
		every = defaultAnnounceInterval
	}
	log.Printf("announcing new registrations in %s every %s", c.AnnounceRoom, every)
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if err := c.announceOnce(ctx); err != nil {
			// A failing poll or send must not kill the loop: the next tick
			// retries from the same lastAnnounced, so nothing is skipped.
			log.Printf("announce cycle: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// announceOnce runs a single announce cycle: fetch everything newer than
// lastAnnounced and post one message per registration in AnnounceRoom, then
// persist the new high-water mark.
//
// The very first cycle after a start without persisted state does not post
// anything — it only adopts the current maximum id as the baseline, so a bot
// deployed against an already-populated database does not spam the room with
// the whole history. Only signups arriving after that are announced.
func (c *Client) announceOnce(ctx context.Context) error {
	if !c.announcementsEnabled() {
		return nil
	}
	c.mu.Lock()
	since := c.lastAnnounced
	baseline := !c.baselined && since == 0
	c.mu.Unlock()

	regs, err := c.FetchNewRegistrations(since)
	if err != nil {
		return fmt.Errorf("fetch registrations since %d: %w", since, err)
	}

	if baseline {
		max := since
		for _, r := range regs {
			if r.ID > max {
				max = r.ID
			}
		}
		c.mu.Lock()
		c.baselined = true
		c.lastAnnounced = max
		c.persistLocked()
		c.mu.Unlock()
		log.Printf("announce baseline set to registration #%d (%d existing signup(s) not announced)", max, len(regs))
		return nil
	}

	var firstErr error
	for _, r := range regs {
		if r.ID <= since {
			continue
		}
		plain, html := announceText(r)
		if err := c.ops.SendHTML(ctx, c.AnnounceRoom, plain, html); err != nil {
			// Stop at the first failure and keep lastAnnounced where it is, so
			// the next cycle retries this registration instead of losing it.
			if firstErr == nil {
				firstErr = fmt.Errorf("announce registration #%d: %w", r.ID, err)
			}
			break
		}
		c.mu.Lock()
		if r.ID > c.lastAnnounced {
			c.lastAnnounced = r.ID
		}
		c.baselined = true
		c.persistLocked()
		c.mu.Unlock()
	}
	return firstErr
}

// announceText renders the public announcement for one registration, as a
// (plain, html) pair. It uses only the Matrix handle and the participant
// number/waiting-list position — never the e-mail or city, which stay on the
// web side and must not reach a public room.
func announceText(r NewRegistration) (string, string) {
	name := localpart(r.Handle)
	link := mention(r.Handle)
	if r.Confirmed {
		return fmt.Sprintf("🎉 %s dołącza do D-Day — uczestnik #%d", name, r.ID),
			fmt.Sprintf("🎉 %s dołącza do D-Day — uczestnik #%d", link, r.ID)
	}
	return fmt.Sprintf("🎉 %s zapisał(a) się na D-Day — lista rezerwowa, pozycja #%d", name, r.WaitlistPos),
		fmt.Sprintf("🎉 %s zapisał(a) się na D-Day — lista rezerwowa, pozycja #%d", link, r.WaitlistPos)
}
