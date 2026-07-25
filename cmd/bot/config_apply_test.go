package main

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/d33mobile/dday/internal/matrixbot"
)

// mockRegistrations stands up a fake web /api/registrations feed that demands
// the shared bearer token and echoes the `since` cursor it was queried with, so
// a test can assert both the auth header and the cursor the closure forwards.
func mockRegistrations(t *testing.T, wantToken string, regs []matrixbot.NewRegistration) (*httptest.Server, *int) {
	t.Helper()
	var gotSince int
	mux := http.NewServeMux()
	mux.HandleFunc("/api/registrations", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+wantToken {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		gotSince, _ = strconv.Atoi(r.URL.Query().Get("since"))
		var out []matrixbot.NewRegistration
		for _, reg := range regs {
			if reg.ID > gotSince {
				out = append(out, reg)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"registrations": out})
	})
	return httptest.NewServer(mux), &gotSince
}

// TestRegistrationsFetcher exercises the announcement-feed closure: it sends the
// bearer token and the `since` cursor, and parses back the registrations newer
// than that cursor — carrying only handle/nick/number/standing, never e-mail or
// city (the NewRegistration type has no such fields, verified structurally).
func TestRegistrationsFetcher(t *testing.T) {
	const token = "internal-secret"
	regs := []matrixbot.NewRegistration{
		{ID: 5, Handle: "@alice:hs.org", Nick: "alice", Rank: 1, Confirmed: true},
		{ID: 6, Handle: "@bob:hs.org", Nick: "bob", Rank: 21, WaitlistPos: 1},
	}
	srv, gotSince := mockRegistrations(t, token, regs)
	defer srv.Close()

	fetch := registrationsFetcher(srv.Client(), srv.URL, token)

	out, err := fetch(4)
	if err != nil {
		t.Fatalf("fetch since 4: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("fetched %d registrations; want 2", len(out))
	}
	if out[0].ID != 5 || out[0].Handle != "@alice:hs.org" || !out[0].Confirmed {
		t.Errorf("first registration = %+v; want id 5 alice confirmed", out[0])
	}
	if out[1].ID != 6 || out[1].WaitlistPos != 1 {
		t.Errorf("second registration = %+v; want id 6 waitlist pos 1", out[1])
	}
	if *gotSince != 4 {
		t.Errorf("server saw since=%d; want 4", *gotSince)
	}

	// The cursor is forwarded verbatim: asking from 5 drops what's already seen.
	out, err = fetch(5)
	if err != nil {
		t.Fatalf("fetch since 5: %v", err)
	}
	if len(out) != 1 || out[0].ID != 6 {
		t.Errorf("fetch(5) = %+v; want only id 6", out)
	}
	if *gotSince != 5 {
		t.Errorf("server saw since=%d; want 5", *gotSince)
	}
}

// TestRegistrationsFetcherNoPersonalFields is a structural guard: the type the
// feed decodes into has no e-mail/city field, so nothing personal can ever be
// transported to the announce loop even if the web side regressed.
func TestRegistrationsFetcherNoPersonalFields(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// The web side sends extra personal fields; they must be dropped on decode.
		_, _ = w.Write([]byte(`{"registrations":[{"id":9,"handle":"@a:hs","email":"a@example.com","city":"Łódź"}]}`))
	}))
	defer srv.Close()

	out, err := registrationsFetcher(srv.Client(), srv.URL, "tok")(0)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(out) != 1 || out[0].ID != 9 || out[0].Handle != "@a:hs" {
		t.Fatalf("decoded %+v; want a single id-9 record", out)
	}
	// json.Marshal round-trips only the whitelisted fields; the address/city are gone.
	b, _ := json.Marshal(out[0])
	if s := string(b); strings.Contains(s, "example.com") || strings.Contains(s, "Łódź") {
		t.Errorf("re-marshalled registration %q leaks personal data", s)
	}
}

// TestRegistrationsFetcherNon200Errors: a rejected bearer token (401) surfaces
// as an error rather than an empty (and wrongly advancing) result.
func TestRegistrationsFetcherNon200Errors(t *testing.T) {
	srv, _ := mockRegistrations(t, "right", nil)
	defer srv.Close()
	if _, err := registrationsFetcher(srv.Client(), srv.URL, "wrong")(0); err == nil {
		t.Fatal("expected an error when the bearer token is rejected (401)")
	}
}

// TestRegistrationsFetcherBadJSONErrors: a malformed body is an error, not a
// silent empty slice.
func TestRegistrationsFetcherBadJSONErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{not json`))
	}))
	defer srv.Close()
	if _, err := registrationsFetcher(srv.Client(), srv.URL, "tok")(0); err == nil {
		t.Fatal("expected an error decoding a malformed feed body")
	}
}

// TestRegistrationsFetcherTransportError: an unreachable origin is an error.
func TestRegistrationsFetcherTransportError(t *testing.T) {
	srv, _ := mockRegistrations(t, "tok", nil)
	srv.Close() // shut it down before querying
	if _, err := registrationsFetcher(srv.Client(), srv.URL, "tok")(0); err == nil {
		t.Fatal("expected a transport error against a closed server")
	}
}

// TestRegisteredCheckerTransportError: an unreachable origin surfaces as an
// error (the bot then treats it fail-open).
func TestRegisteredCheckerTransportError(t *testing.T) {
	srv := mockRegistered(t, "tok", nil)
	srv.Close() // down before the query
	if _, _, err := registeredChecker(srv.Client(), srv.URL, "tok")("@a:hs"); err == nil {
		t.Fatal("expected a transport error against a closed server")
	}
}

// TestRegisteredCheckerBadJSONErrors: a 200 with a malformed body is an error,
// not a silent (0,false).
func TestRegisteredCheckerBadJSONErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{not json`))
	}))
	defer srv.Close()
	if _, _, err := registeredChecker(srv.Client(), srv.URL, "tok")("@a:hs"); err == nil {
		t.Fatal("expected an error decoding a malformed /api/registered body")
	}
}

// TestAnnounceInterval covers the ANNOUNCE_INTERVAL parsing: a valid duration is
// honoured, and anything unset/unparsable/non-positive degrades to 0 (the
// package default cadence) rather than a hot loop.
func TestAnnounceInterval(t *testing.T) {
	cases := []struct {
		raw  string
		want time.Duration
	}{
		{"30s", 30 * time.Second},
		{"  1m  ", time.Minute},
		{"", 0},
		{"garbage", 0},
		{"-5s", 0},
		{"0s", 0},
	}
	for _, tc := range cases {
		t.Setenv("ANNOUNCE_INTERVAL", tc.raw)
		if got := announceInterval(); got != tc.want {
			t.Errorf("announceInterval(%q) = %v; want %v", tc.raw, got, tc.want)
		}
	}
}

// TestHTTPClient: the shared client is non-nil, reused across calls, and carries
// a bounded timeout so a stuck internal API cannot hang the bot forever.
func TestHTTPClient(t *testing.T) {
	c := httpClient()
	if c == nil {
		t.Fatal("httpClient returned nil")
	}
	if httpClient() != c {
		t.Error("httpClient must return the same shared client")
	}
	if c.Timeout <= 0 {
		t.Errorf("httpClient timeout = %v; want a positive bound", c.Timeout)
	}
}

// applyConfigEnv sets the full environment applyConfig reads to controlled
// values, isolating the test from whatever the host environment happens to hold.
func applyConfigEnv(t *testing.T) {
	t.Helper()
	t.Setenv("REGISTER_URL", "https://dday.hs-ldz.pl/register")
	t.Setenv("AGE_PUB_DATA", base64.StdEncoding.EncodeToString([]byte(genPubKey(t))))
	t.Setenv("AGE_PUB", "")
	t.Setenv("TOKEN_SECRET", "shared-hmac")
	t.Setenv("PANEL_URL", "")
	t.Setenv("ALLOWED_ROOMS", "!a:hs,!b:hs")
	t.Setenv("ANNOUNCE_ROOM", "!home:hs")
	t.Setenv("MATRIX_ROOM", "")
	t.Setenv("ANNOUNCE_INTERVAL", "45s")
	t.Setenv("DM_CACHE_PATH", filepath.Join(t.TempDir(), "cache.json"))
}

// TestApplyConfig wires the full environment and asserts applyConfig maps each
// variable onto the right Client field: the panel base derived from
// REGISTER_URL's origin, the room allowlist, the announce target and cadence,
// the DM cache path, and — because INTERNAL_TOKEN is set — the already-registered
// check and the announcement feed.
func TestApplyConfig(t *testing.T) {
	applyConfigEnv(t)
	t.Setenv("INTERNAL_TOKEN", "internal-secret")

	c := matrixbot.New(nil)
	if err := applyConfig(c); err != nil {
		t.Fatalf("applyConfig: %v", err)
	}

	if c.Recipient == nil {
		t.Error("Recipient not set from AGE_PUB_DATA")
	}
	if c.LinkBase != "https://dday.hs-ldz.pl/register" {
		t.Errorf("LinkBase = %q; want the REGISTER_URL", c.LinkBase)
	}
	if c.TokenSecret != "shared-hmac" {
		t.Errorf("TokenSecret = %q; want the TOKEN_SECRET", c.TokenSecret)
	}
	if c.PanelBase != "https://dday.hs-ldz.pl/panel" {
		t.Errorf("PanelBase = %q; want the origin-derived /panel", c.PanelBase)
	}
	if !c.AllowedRooms["!a:hs"] || !c.AllowedRooms["!b:hs"] || len(c.AllowedRooms) != 2 {
		t.Errorf("AllowedRooms = %v; want the two-room allowlist", c.AllowedRooms)
	}
	if c.CheckRegistered == nil {
		t.Error("CheckRegistered nil despite INTERNAL_TOKEN being set")
	}
	if c.FetchNewRegistrations == nil {
		t.Error("FetchNewRegistrations nil despite INTERNAL_TOKEN being set")
	}
	if c.AnnounceRoom != "!home:hs" {
		t.Errorf("AnnounceRoom = %q; want ANNOUNCE_ROOM", c.AnnounceRoom)
	}
	if c.AnnounceInterval != 45*time.Second {
		t.Errorf("AnnounceInterval = %v; want 45s", c.AnnounceInterval)
	}
	if c.CachePath == "" {
		t.Error("CachePath not set from DM_CACHE_PATH")
	}
	if c.IsOpen == nil {
		t.Error("IsOpen gate not wired")
	}
}

// TestApplyConfigWithoutInternalToken: with no INTERNAL_TOKEN the internal-API
// closures stay nil (the already-registered check is fail-open and there is no
// announcement feed), while the rest of the config is still populated.
func TestApplyConfigWithoutInternalToken(t *testing.T) {
	applyConfigEnv(t)
	t.Setenv("INTERNAL_TOKEN", "")

	c := matrixbot.New(nil)
	if err := applyConfig(c); err != nil {
		t.Fatalf("applyConfig: %v", err)
	}
	if c.CheckRegistered != nil {
		t.Error("CheckRegistered must be nil without INTERNAL_TOKEN (fail-open)")
	}
	if c.FetchNewRegistrations != nil {
		t.Error("FetchNewRegistrations must be nil without INTERNAL_TOKEN")
	}
	// The transport-independent config is still applied.
	if c.AnnounceRoom != "!home:hs" || c.PanelBase == "" {
		t.Errorf("non-token config not applied: AnnounceRoom=%q PanelBase=%q", c.AnnounceRoom, c.PanelBase)
	}
}

// TestApplyConfigFallsBackToMatrixRoom: when ANNOUNCE_ROOM is unset the announce
// target falls back to the home channel MATRIX_ROOM.
func TestApplyConfigFallsBackToMatrixRoom(t *testing.T) {
	applyConfigEnv(t)
	t.Setenv("INTERNAL_TOKEN", "")
	t.Setenv("ANNOUNCE_ROOM", "")
	t.Setenv("MATRIX_ROOM", "!fallback:hs")

	c := matrixbot.New(nil)
	if err := applyConfig(c); err != nil {
		t.Fatalf("applyConfig: %v", err)
	}
	if c.AnnounceRoom != "!fallback:hs" {
		t.Errorf("AnnounceRoom = %q; want the MATRIX_ROOM fallback", c.AnnounceRoom)
	}
}

// TestApplyConfigBadRecipient: an unloadable recipient (bad AGE_PUB_DATA base64)
// aborts applyConfig with an error rather than a half-configured client.
func TestApplyConfigBadRecipient(t *testing.T) {
	applyConfigEnv(t)
	t.Setenv("AGE_PUB_DATA", "!!! not base64 !!!")
	t.Setenv("INTERNAL_TOKEN", "")

	if err := applyConfig(matrixbot.New(nil)); err == nil {
		t.Fatal("expected applyConfig to fail on an unloadable recipient")
	}
}
