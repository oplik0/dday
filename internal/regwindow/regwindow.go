// Package regwindow is the single source of truth for every date the project
// shows: when registration opens and when the event itself runs. Both binaries
// (web server and Matrix bot) and — via /api/count — the landing page read the
// dates from here, so a date change is one environment variable, not a sweep
// through templates.
package regwindow

import (
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
	_ "time/tzdata" // embed the tz database so Europe/Warsaw resolves on distroless
)

// Environment variables overriding the built-in defaults. Each accepts either
// RFC3339 ("2026-10-31T15:00:00+01:00") or the simpler "2006-01-02 15:04",
// which is interpreted in Europe/Warsaw. An unparseable value is logged and the
// default is used — a typo must not take the site down.
const (
	EnvOpenAt     = "REGISTRATION_OPEN_AT"
	EnvEventStart = "EVENT_START_AT"
	EnvEventEnd   = "EVENT_END_AT"
)

// Defaults, used whenever the matching environment variable is unset or bad.
var (
	defaultOpenAt     = func() time.Time { return time.Date(2026, 10, 31, 15, 0, 0, 0, Warsaw()) }
	defaultEventStart = func() time.Time { return time.Date(2026, 11, 7, 14, 0, 0, 0, Warsaw()) }
	defaultEventEnd   = func() time.Time { return time.Date(2026, 11, 7, 22, 0, 0, 0, Warsaw()) }
)

// OpenAt is the instant registration opens (REGISTRATION_OPEN_AT).
func OpenAt() time.Time { return lookup(EnvOpenAt, defaultOpenAt) }

// EventStart is the instant the event begins (EVENT_START_AT).
func EventStart() time.Time { return lookup(EnvEventStart, defaultEventStart) }

// EventEnd is the instant the event ends (EVENT_END_AT).
func EventEnd() time.Time { return lookup(EnvEventEnd, defaultEventEnd) }

// lookup parses the named environment variable, falling back to def.
func lookup(env string, def func() time.Time) time.Time {
	v := strings.TrimSpace(os.Getenv(env))
	if v == "" {
		return def()
	}
	t, err := parseMoment(v)
	if err != nil {
		logBadValue(env, v)
		return def()
	}
	return t.In(Warsaw())
}

// parseMoment accepts RFC3339 or "2006-01-02 15:04" (Europe/Warsaw).
func parseMoment(v string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t, nil
	}
	for _, layout := range []string{"2006-01-02 15:04:05", "2006-01-02 15:04", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, v, Warsaw()); err == nil {
			return t, nil
		}
	}
	return time.Time{}, &time.ParseError{Layout: time.RFC3339, Value: v}
}

// badValues remembers which env/value pairs were already reported, so a bad
// value on a per-request accessor logs once instead of on every hit.
var badValues sync.Map

func logBadValue(env, v string) {
	if _, seen := badValues.LoadOrStore(env+"="+v, true); !seen {
		log.Printf("regwindow: cannot parse %s=%q (want RFC3339 or \"2006-01-02 15:04\"); using default", env, v)
	}
}

// Open reports whether registration is currently open. A truthy
// REGISTRATION_OPEN (anything strconv.ParseBool reads as true — 1/t/T/TRUE/
// true/True — plus "yes") forces it open. Any other value, including a falsy or
// unparseable one, falls through to the time gate: open once we pass OpenAt().
func Open() bool {
	v := strings.TrimSpace(os.Getenv("REGISTRATION_OPEN"))
	if b, err := strconv.ParseBool(v); err == nil && b {
		return true
	}
	if strings.EqualFold(v, "yes") {
		return true
	}
	return !time.Now().Before(OpenAt())
}

// Warsaw returns the Europe/Warsaw location, falling back to a fixed +02:00
// (CEST) zone if the tz database is unavailable. time/tzdata is imported so the
// lookup succeeds even on distroless images without system tzdata.
func Warsaw() *time.Location {
	loc, err := time.LoadLocation("Europe/Warsaw")
	if err != nil {
		return time.FixedZone("CEST", 2*60*60)
	}
	return loc
}

// Polish date vocabulary. Go has no locale support, so the small tables below
// are the whole localisation: weekday names (nominative and accusative, the
// latter for "w sobotę …") plus months in the genitive ("31 października").
var (
	weekdays = [...]string{"niedziela", "poniedziałek", "wtorek", "środa", "czwartek", "piątek", "sobota"}
	// weekdaysAcc is used after the preposition "w": "w niedzielę", "w sobotę".
	weekdaysAcc = [...]string{"niedzielę", "poniedziałek", "wtorek", "środę", "czwartek", "piątek", "sobotę"}
	weekdaysAbb = [...]string{"Nd", "Pn", "Wt", "Śr", "Cz", "Pt", "Sob"}
	monthsGen   = [...]string{"stycznia", "lutego", "marca", "kwietnia", "maja", "czerwca",
		"lipca", "sierpnia", "września", "października", "listopada", "grudnia"}
)

func warsaw(t time.Time) time.Time { return t.In(Warsaw()) }

// dayMonth renders "31 października".
func dayMonth(t time.Time) string {
	t = warsaw(t)
	return strconv.Itoa(t.Day()) + " " + monthsGen[int(t.Month())-1]
}

// hourMinute renders "15:00".
func hourMinute(t time.Time) string { return warsaw(t).Format("15:04") }

// OpenStartText renders the registration start the way the "closed"/"expired"
// pages and the bot's "not open yet" DM state it, e.g.
// "sobota 31 października 2026, 15:00 (czasu polskiego)".
func OpenStartText() string {
	t := warsaw(OpenAt())
	return weekdays[int(t.Weekday())] + " " + dayMonth(t) + " " + strconv.Itoa(t.Year()) +
		", " + hourMinute(t) + " (czasu polskiego)"
}

// OpenHowtoText renders the registration start for use after the preposition
// "w", as the landing page's "Zapisy startują w …" line does, e.g.
// "sobotę 31 października, 15:00 czasu PL".
func OpenHowtoText() string {
	t := warsaw(OpenAt())
	return weekdaysAcc[int(t.Weekday())] + " " + dayMonth(t) + ", " + hourMinute(t) + " czasu PL"
}

// OpenShort renders the "Zapisy od" tile headline, e.g. "Sob 31 października".
func OpenShort() string {
	t := warsaw(OpenAt())
	return weekdaysAbb[int(t.Weekday())] + " " + dayMonth(t)
}

// OpenShortTime renders that tile's sub-line, e.g. "15:00 czasu PL".
func OpenShortTime() string { return hourMinute(OpenAt()) + " czasu PL" }

// EventText renders the event window in full, e.g.
// "sobota 7 listopada 2026, 14:00–22:00".
func EventText() string {
	t := warsaw(EventStart())
	return weekdays[int(t.Weekday())] + " " + dayMonth(t) + " " + strconv.Itoa(t.Year()) +
		", " + hourMinute(t) + "–" + hourMinute(EventEnd())
}

// EventShort renders the "Kiedy" tile headline, e.g. "Sob, 7 listopada".
func EventShort() string {
	t := warsaw(EventStart())
	return weekdaysAbb[int(t.Weekday())] + ", " + dayMonth(t)
}

// EventShortTime renders that tile's sub-line, e.g. "14:00 – 22:00".
func EventShortTime() string { return hourMinute(EventStart()) + " – " + hourMinute(EventEnd()) }

// EventBadge renders the header badge, e.g. "7 listopada 2026".
func EventBadge() string {
	t := warsaw(EventStart())
	return dayMonth(t) + " " + strconv.Itoa(t.Year())
}
