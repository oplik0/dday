package regwindow

import (
	"strings"
	"testing"
	"time"
)

func TestOpenEnvOverride(t *testing.T) {
	// Every truthy form ParseBool accepts, plus the non-ParseBool "yes"/"YES".
	for _, v := range []string{"1", "t", "T", "true", "True", "TRUE", "yes", "YES", " 1 "} {
		t.Setenv("REGISTRATION_OPEN", v)
		if !Open() {
			t.Errorf("Open() with REGISTRATION_OPEN=%q = false, want true", v)
		}
	}
}

// TestOpenFalsyFallsToGate asserts that a falsy or unparseable REGISTRATION_OPEN
// does NOT force-open; it falls through to the time gate. The gate moment is
// pinned via REGISTRATION_OPEN_AT so the test does not expire with the calendar.
func TestOpenFalsyFallsToGate(t *testing.T) {
	t.Setenv(EnvOpenAt, "2999-01-01 12:00")
	for _, v := range []string{"0", "f", "false", "False", "FALSE", "nope", ""} {
		t.Setenv("REGISTRATION_OPEN", v)
		if Open() {
			t.Errorf("Open() with REGISTRATION_OPEN=%q = true, want false (gate at %v)", v, OpenAt())
		}
	}
}

// TestOpenRespectsOpenAt asserts the gate follows REGISTRATION_OPEN_AT in both
// directions, with no REGISTRATION_OPEN override in play.
func TestOpenRespectsOpenAt(t *testing.T) {
	t.Setenv("REGISTRATION_OPEN", "")
	t.Setenv(EnvOpenAt, "2000-01-01 12:00")
	if !Open() {
		t.Error("Open() with a past REGISTRATION_OPEN_AT = false, want true")
	}
	t.Setenv(EnvOpenAt, "2999-01-01T12:00:00+01:00")
	if Open() {
		t.Error("Open() with a future REGISTRATION_OPEN_AT = true, want false")
	}
}

func TestDefaults(t *testing.T) {
	want := []struct {
		name string
		got  time.Time
		when string
	}{
		{"OpenAt", OpenAt(), "2026-10-31 15:00"},
		{"EventStart", EventStart(), "2026-11-07 14:00"},
		{"EventEnd", EventEnd(), "2026-11-07 22:00"},
	}
	for _, c := range want {
		if got := c.got.In(Warsaw()).Format("2006-01-02 15:04"); got != c.when {
			t.Errorf("%s() = %s, want %s", c.name, got, c.when)
		}
	}
}

func TestParseFormats(t *testing.T) {
	cases := map[string]string{
		"2026-09-05T10:30:00+02:00": "2026-09-05 10:30",
		"2026-09-05 10:30":          "2026-09-05 10:30",
		"2026-09-05 10:30:15":       "2026-09-05 10:30",
		"2026-09-05":                "2026-09-05 00:00",
		" 2026-09-05 10:30 ":        "2026-09-05 10:30", // trimmed before parsing
		"2026-09-05T08:30:00Z":      "2026-09-05 10:30", // UTC rendered in Warsaw
	}
	for in, want := range cases {
		t.Setenv(EnvEventStart, in)
		if got := EventStart().In(Warsaw()).Format("2006-01-02 15:04"); got != want {
			t.Errorf("EVENT_START_AT=%q → %s, want %s", in, got, want)
		}
	}
}

// TestParseFallback asserts an unparseable value does not panic or zero the
// date: it logs and keeps the default.
func TestParseFallback(t *testing.T) {
	for _, v := range []string{"not-a-date", "31.10.2026", "2026-13-45 99:99"} {
		t.Setenv(EnvOpenAt, v)
		if got := OpenAt().Format("2006-01-02 15:04"); got != "2026-10-31 15:00" {
			t.Errorf("REGISTRATION_OPEN_AT=%q → %s, want the default 2026-10-31 15:00", v, got)
		}
	}
}

// TestTextsFollowDates is the point of the refactor: the Polish strings are
// generated from the configured moments, never hand-synced.
func TestTextsFollowDates(t *testing.T) {
	t.Setenv(EnvOpenAt, "2026-09-07 09:05")     // a Monday
	t.Setenv(EnvEventStart, "2026-12-05 10:00") // a Saturday
	t.Setenv(EnvEventEnd, "2026-12-05 18:30")

	cases := map[string]string{
		OpenStartText():  "poniedziałek 7 września 2026, 09:05 (czasu polskiego)",
		OpenHowtoText():  "poniedziałek 7 września, 09:05 czasu PL",
		OpenShort():      "Pn 7 września",
		OpenShortTime():  "09:05 czasu PL",
		EventText():      "sobota 5 grudnia 2026, 10:00–18:30",
		EventShort():     "Sob, 5 grudnia",
		EventShortTime(): "10:00 – 18:30",
		EventBadge():     "5 grudnia 2026",
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
}

// TestDefaultTexts pins the wording produced for the built-in dates — the exact
// strings the pre-refactor constant carried.
func TestDefaultTexts(t *testing.T) {
	if got, want := OpenStartText(), "sobota 31 października 2026, 15:00 (czasu polskiego)"; got != want {
		t.Errorf("OpenStartText() = %q, want %q", got, want)
	}
	if got, want := EventText(), "sobota 7 listopada 2026, 14:00–22:00"; got != want {
		t.Errorf("EventText() = %q, want %q", got, want)
	}
	if got, want := OpenShort(), "Sob 31 października"; got != want {
		t.Errorf("OpenShort() = %q, want %q", got, want)
	}
	if got, want := EventShort(), "Sob, 7 listopada"; got != want {
		t.Errorf("EventShort() = %q, want %q", got, want)
	}
	if got, want := EventBadge(), "7 listopada 2026"; got != want {
		t.Errorf("EventBadge() = %q, want %q", got, want)
	}
	// Accusative day name after "w" — grammatically different from the
	// nominative used in OpenStartText.
	if got := OpenHowtoText(); !strings.HasPrefix(got, "sobotę ") {
		t.Errorf("OpenHowtoText() = %q, want an accusative weekday prefix", got)
	}
}

// TestAllWeekdaysAndMonths walks a whole week and a whole year so no table
// entry is left untested (an off-by-one in either table would surface here).
func TestAllWeekdaysAndMonths(t *testing.T) {
	for i, want := range []string{"czwartek", "piątek", "sobota", "niedziela", "poniedziałek", "wtorek", "środa"} {
		day := time.Date(2026, 1, 1+i, 12, 0, 0, 0, Warsaw())
		if got := weekdays[int(day.Weekday())]; got != want {
			t.Errorf("weekday for %s = %q, want %q", day.Format("2006-01-02"), got, want)
		}
	}
	months := []string{"stycznia", "lutego", "marca", "kwietnia", "maja", "czerwca",
		"lipca", "sierpnia", "września", "października", "listopada", "grudnia"}
	for i, want := range months {
		t.Setenv(EnvEventStart, time.Date(2026, time.Month(i+1), 15, 12, 0, 0, 0, Warsaw()).Format(time.RFC3339))
		if got := EventBadge(); got != "15 "+want+" 2026" {
			t.Errorf("EventBadge() for month %d = %q, want %q", i+1, got, "15 "+want+" 2026")
		}
	}
}

func TestWarsawIsCET(t *testing.T) {
	loc := Warsaw()
	if loc == nil {
		t.Fatal("Warsaw() returned nil")
	}
	// The default OpenAt (31 October 2026) falls after the DST switch on
	// 25 October, so the offset must be CET +01:00 (3600s).
	_, offset := OpenAt().Zone()
	if offset != 1*60*60 {
		t.Errorf("OpenAt() offset = %ds, want 3600s", offset)
	}
}
