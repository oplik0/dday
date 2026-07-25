package matrixbot

import (
	"testing"
	"time"
)

// TestRateLimitSkipsSecondRegister asserts that a second "!register" from the
// same handle inside the rate window is dropped entirely: no second DM is
// created and no second message is sent.
func TestRateLimitSkipsSecondRegister(t *testing.T) {
	recipient, _ := genKeypair(t)

	f := newFakeOps()
	c := New(f)
	c.Recipient = recipient
	c.LinkBase = "https://dday.hs-ldz.pl/register"
	c.TokenSecret = testSecret
	// Frozen clock: both calls happen "at the same instant", inside the window.
	frozen := time.Now()
	c.now = func() time.Time { return frozen }

	if _, err := c.HandleRegister(bg(), "", "@alice:mock"); err != nil {
		t.Fatalf("first HandleRegister: %v", err)
	}
	if _, err := c.HandleRegister(bg(), "", "@alice:mock"); err != nil {
		t.Fatalf("second HandleRegister: %v", err)
	}

	if n := f.createCount(); n != 1 {
		t.Errorf("CreateDM calls = %d; want 1 (second !register rate limited)", n)
	}
	if n := f.sendCount(); n != 1 {
		t.Errorf("SendHTML calls = %d; want 1 (second !register rate limited)", n)
	}

	// A different handle is independent and is not rate limited.
	if _, err := c.HandleRegister(bg(), "", "@bob:mock"); err != nil {
		t.Fatalf("HandleRegister bob: %v", err)
	}
	if n := f.createCount(); n != 2 {
		t.Errorf("CreateDM calls after bob = %d; want 2", n)
	}
}

// TestDMReuseAfterWindow asserts that once the rate window has elapsed a
// returning handle IS processed again, but the DM room is reused from cache
// rather than created afresh: exactly one createRoom, two sends.
func TestDMReuseAfterWindow(t *testing.T) {
	recipient, _ := genKeypair(t)

	f := newFakeOps()
	c := New(f)
	c.Recipient = recipient
	c.LinkBase = "https://dday.hs-ldz.pl/register"
	c.TokenSecret = testSecret
	c.RateWindow = time.Second

	base := time.Now()
	now := base
	c.now = func() time.Time { return now }

	if _, err := c.HandleRegister(bg(), "", "@alice:mock"); err != nil {
		t.Fatalf("first HandleRegister: %v", err)
	}

	// Advance past the rate window so the second command is accepted.
	now = base.Add(time.Hour)
	if _, err := c.HandleRegister(bg(), "", "@alice:mock"); err != nil {
		t.Fatalf("second HandleRegister: %v", err)
	}

	if n := f.createCount(); n != 1 {
		t.Errorf("CreateDM calls = %d; want 1 (DM reused from cache)", n)
	}
	if n := f.sendCount(); n != 2 {
		t.Errorf("SendHTML calls = %d; want 2 (both DMs delivered)", n)
	}
}
