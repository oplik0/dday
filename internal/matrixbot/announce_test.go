package matrixbot

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const announceRoom = "!home:mock"

// announceClient wires a Client to a fake transport that records every sent
// message, with the announce feed stubbed by fetch.
func announceClient(t *testing.T, fetch func(int) ([]NewRegistration, error)) (*Client, *fakeOps) {
	t.Helper()
	f := newFakeOps()
	c := New(f)
	c.AnnounceRoom = announceRoom
	c.FetchNewRegistrations = fetch
	return c, f
}

// drain collects everything queued on the fake's send channel without blocking.
func drain(f *fakeOps) []recordedSend {
	var out []recordedSend
	for {
		select {
		case m := <-f.sendCh:
			out = append(out, m)
		default:
			return out
		}
	}
}

func bodyOf(m recordedSend) string      { return m.plain }
func formattedOf(m recordedSend) string { return m.html }

// TestAnnounceOnceSendsNewRegistrations: with a baseline already established,
// one cycle posts one message per new signup — a participant line and a
// waiting-list line — advances lastAnnounced, and a second cycle with nothing
// new sends nothing.
func TestAnnounceOnceSendsNewRegistrations(t *testing.T) {
	regs := []NewRegistration{
		{ID: 5, Handle: "@alice:hs.org", Nick: "alice", Rank: 1, Confirmed: true},
		{ID: 6, Handle: "@bob:hs.org", Nick: "bob", Rank: 21, WaitlistPos: 1},
	}
	var calls []int
	c, f := announceClient(t, func(since int) ([]NewRegistration, error) {
		calls = append(calls, since)
		var out []NewRegistration
		for _, r := range regs {
			if r.ID > since {
				out = append(out, r)
			}
		}
		return out, nil
	})
	// Pretend a previous run already baselined at id 4.
	c.lastAnnounced = 4
	c.baselined = true

	if err := c.announceOnce(bg()); err != nil {
		t.Fatalf("announceOnce: %v", err)
	}
	msgs := drain(f)
	if len(msgs) != 2 {
		t.Fatalf("sent %d messages; want 2", len(msgs))
	}
	if got := bodyOf(msgs[0]); !strings.Contains(got, "alice") || !strings.Contains(got, "uczestnik #5") {
		t.Errorf("participant message = %q; want alice + \"uczestnik #5\"", got)
	}
	if got := bodyOf(msgs[1]); !strings.Contains(got, "bob") || !strings.Contains(got, "pozycja #1") ||
		!strings.Contains(got, "rezerwow") {
		t.Errorf("waiting-list message = %q; want bob + waiting-list position 1", got)
	}
	if got := formattedOf(msgs[0]); !strings.Contains(got, "https://matrix.to/#/@alice:hs.org") {
		t.Errorf("formatted body = %q; want a matrix.to mention", got)
	}
	if c.lastAnnounced != 6 {
		t.Errorf("lastAnnounced = %d; want 6", c.lastAnnounced)
	}

	// Second cycle: nothing new, nothing sent, and it asked from id 6.
	if err := c.announceOnce(bg()); err != nil {
		t.Fatalf("second announceOnce: %v", err)
	}
	if msgs := drain(f); len(msgs) != 0 {
		t.Errorf("second cycle sent %d messages; want 0", len(msgs))
	}
	if len(calls) != 2 || calls[0] != 4 || calls[1] != 6 {
		t.Errorf("fetch called with %v; want [4 6]", calls)
	}
}

// TestAnnounceOmitsPersonalData: the message text carries only the handle and
// the participant number/position. There is nowhere for an e-mail or city to
// appear even if the feed ever regressed and sent them.
func TestAnnounceOmitsPersonalData(t *testing.T) {
	c, f := announceClient(t, func(int) ([]NewRegistration, error) {
		return []NewRegistration{{ID: 2, Handle: "@alice:hs.org", Nick: "alice", Rank: 1, Confirmed: true}}, nil
	})
	c.lastAnnounced = 1
	c.baselined = true

	if err := c.announceOnce(bg()); err != nil {
		t.Fatalf("announceOnce: %v", err)
	}
	msgs := drain(f)
	if len(msgs) != 1 {
		t.Fatalf("sent %d messages; want 1", len(msgs))
	}
	// The plain body carries the bare localpart; the HTML body carries the
	// matrix.to mention. Neither may carry an address or a city — and since the
	// NewRegistration type has no such fields, there is nothing to leak.
	all := bodyOf(msgs[0]) + formattedOf(msgs[0])
	for _, forbidden := range []string{"@example.com", "example.com", "Łódź", "Warszawa"} {
		if strings.Contains(all, forbidden) {
			t.Errorf("announcement %q contains %q", all, forbidden)
		}
	}
	if !strings.Contains(bodyOf(msgs[0]), "alice") {
		t.Errorf("plain body %q does not name the participant", bodyOf(msgs[0]))
	}
}

// TestAnnounceFirstRunBaselines: a bot starting against an already-populated
// database must not replay history — the first cycle sends nothing and only
// adopts the current maximum id.
func TestAnnounceFirstRunBaselines(t *testing.T) {
	fetched := 0
	c, f := announceClient(t, func(since int) ([]NewRegistration, error) {
		fetched++
		if since >= 3 {
			return nil, nil
		}
		return []NewRegistration{
			{ID: 1, Handle: "@a:hs.org", Confirmed: true},
			{ID: 2, Handle: "@b:hs.org", Confirmed: true},
			{ID: 3, Handle: "@c:hs.org", Confirmed: true},
		}, nil
	})

	if err := c.announceOnce(bg()); err != nil {
		t.Fatalf("baseline cycle: %v", err)
	}
	if msgs := drain(f); len(msgs) != 0 {
		t.Fatalf("baseline cycle sent %d messages; want 0 (no history replay)", len(msgs))
	}
	if c.lastAnnounced != 3 {
		t.Errorf("lastAnnounced after baseline = %d; want 3", c.lastAnnounced)
	}
	// The next cycle announces normally.
	if err := c.announceOnce(bg()); err != nil {
		t.Fatalf("second cycle: %v", err)
	}
	if fetched != 2 {
		t.Errorf("fetch called %d times; want 2", fetched)
	}
}

// TestAnnouncePersistsLastAnnounced: the high-water mark survives a restart, so
// a redeployed bot does not re-announce what it already posted.
func TestAnnouncePersistsLastAnnounced(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.json")

	feed := func(since int) ([]NewRegistration, error) {
		if since >= 7 {
			return nil, nil
		}
		return []NewRegistration{{ID: 7, Handle: "@alice:hs.org", Confirmed: true}}, nil
	}

	first, f1 := announceClient(t, feed)
	first.CachePath = path
	first.lastAnnounced = 6
	first.baselined = true
	first.cacheDM("@alice:hs.org", "!dm:mock")
	if err := first.announceOnce(bg()); err != nil {
		t.Fatalf("first announceOnce: %v", err)
	}
	if msgs := drain(f1); len(msgs) != 1 {
		t.Fatalf("first run sent %d messages; want 1", len(msgs))
	}

	// A fresh Client on the same file resumes where the first left off.
	second, f2 := announceClient(t, feed)
	second.CachePath = path
	n, err := second.LoadCache()
	if err != nil {
		t.Fatalf("LoadCache: %v", err)
	}
	if n != 1 {
		t.Errorf("LoadCache loaded %d DM entries; want 1", n)
	}
	if second.lastAnnounced != 7 {
		t.Fatalf("restored lastAnnounced = %d; want 7", second.lastAnnounced)
	}
	if err := second.announceOnce(bg()); err != nil {
		t.Fatalf("second announceOnce: %v", err)
	}
	if msgs := drain(f2); len(msgs) != 0 {
		t.Errorf("restarted bot re-announced %d message(s); want 0", len(msgs))
	}
}

// TestLoadCacheLegacyFlatFormat: a cache written by an older build (a bare
// handle->roomID map) still loads, with lastAnnounced 0 so the bot re-baselines
// instead of replaying history.
func TestLoadCacheLegacyFlatFormat(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.json")
	legacy := map[string]string{"@alice:hs.org": "!dm:mock", "@bob:hs.org": "!dm2:mock"}
	data, err := json.Marshal(legacy)
	if err != nil {
		t.Fatalf("marshal legacy: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write legacy cache: %v", err)
	}

	c := New(newFakeOps())
	c.CachePath = path
	n, err := c.LoadCache()
	if err != nil {
		t.Fatalf("LoadCache on legacy file: %v", err)
	}
	if n != 2 {
		t.Errorf("loaded %d entries; want 2", n)
	}
	if c.dmRooms["@alice:hs.org"] != "!dm:mock" {
		t.Errorf("dmRooms = %v; want the legacy mapping preserved", c.dmRooms)
	}
	if c.lastAnnounced != 0 {
		t.Errorf("lastAnnounced from legacy cache = %d; want 0", c.lastAnnounced)
	}

	// Writing again upgrades the file to the current object format.
	c.cacheDM("@carol:hs.org", "!dm3:mock")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read upgraded cache: %v", err)
	}
	var upgraded cacheFile
	if err := json.Unmarshal(raw, &upgraded); err != nil {
		t.Fatalf("upgraded cache is not valid JSON: %v", err)
	}
	if len(upgraded.DMs) != 3 {
		t.Errorf("upgraded cache DMs = %v; want 3 entries", upgraded.DMs)
	}
}

// TestAnnounceDisabled: no announce room (or no feed) means the cycle is inert.
func TestAnnounceDisabled(t *testing.T) {
	c, f := announceClient(t, func(int) ([]NewRegistration, error) {
		t.Error("feed must not be queried when announcements are disabled")
		return nil, nil
	})
	c.AnnounceRoom = ""
	if c.announcementsEnabled() {
		t.Fatal("announcementsEnabled with empty AnnounceRoom")
	}
	if err := c.announceOnce(bg()); err != nil {
		t.Fatalf("announceOnce disabled: %v", err)
	}
	if msgs := drain(f); len(msgs) != 0 {
		t.Errorf("disabled bot sent %d messages; want 0", len(msgs))
	}

	c2, _ := announceClient(t, nil)
	if c2.announcementsEnabled() {
		t.Error("announcementsEnabled with nil FetchNewRegistrations")
	}
}

// TestAnnounceFetchErrorKeepsState: a failing poll is reported but never
// advances lastAnnounced, so the next cycle retries the same range.
func TestAnnounceFetchErrorKeepsState(t *testing.T) {
	c, f := announceClient(t, func(int) ([]NewRegistration, error) {
		return nil, fmt.Errorf("boom")
	})
	c.lastAnnounced = 4
	c.baselined = true

	if err := c.announceOnce(bg()); err == nil {
		t.Fatal("announceOnce with a failing feed returned nil error")
	}
	if c.lastAnnounced != 4 {
		t.Errorf("lastAnnounced = %d; want 4 (unchanged after a failed fetch)", c.lastAnnounced)
	}
	if msgs := drain(f); len(msgs) != 0 {
		t.Errorf("failed fetch sent %d messages; want 0", len(msgs))
	}
}

// TestAnnounceLoopTicksAndStops drives the real loop with a tiny interval: it
// announces, and it returns promptly when the context is cancelled — the
// goroutine cannot outlive Run. Race detector included.
func TestAnnounceLoopTicksAndStops(t *testing.T) {
	c, f := announceClient(t, func(since int) ([]NewRegistration, error) {
		if since >= 9 {
			return nil, nil
		}
		return []NewRegistration{{ID: 9, Handle: "@alice:hs.org", Confirmed: true}}, nil
	})
	c.lastAnnounced = 8
	c.baselined = true
	c.AnnounceInterval = time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		c.announceLoop(ctx)
		close(done)
	}()

	select {
	case m := <-f.sendCh:
		if !strings.Contains(bodyOf(m), "uczestnik #9") {
			t.Errorf("loop sent %q; want the participant announcement", bodyOf(m))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("announceLoop sent nothing within 5s")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("announceLoop did not return after context cancellation")
	}
}
