package main

import (
	"context"
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/d33mobile/dday/internal/matrixbot"
)

// botTransport is a minimal in-memory matrixbot transport (implementing the
// package's unexported matrixOps interface structurally) for the cross-component
// e2e below: it captures the plain body the bot delivers so the test can drive
// the bot's ACTUAL emitted link through the real web handlers.
type botTransport struct{ sent chan string }

func (b *botTransport) Self() string { return "@ddaybot:mock" }
func (b *botTransport) SendHTML(_ context.Context, _, plain, _ string) error {
	select {
	case b.sent <- plain:
	default:
	}
	return nil
}
func (b *botTransport) CreateDM(context.Context, string) (string, error) { return "!dm:mock", nil }
func (b *botTransport) JoinRoom(context.Context, string) error           { return nil }
func (b *botTransport) JoinedMembers(context.Context, string) (map[string]struct{}, error) {
	return map[string]struct{}{"@ddaybot:mock": {}, "@a:mock": {}, "@b:mock": {}}, nil
}
func (b *botTransport) GetDirect(context.Context) (map[string][]string, error) {
	return map[string][]string{}, nil
}
func (b *botTransport) SetDirect(context.Context, map[string][]string) error { return nil }

func TestRegisterGetShowsForm(t *testing.T) {
	e := newTestEnv(t, true, 20)
	tok := e.token(t, "@alice:hs.org")

	rec := e.get(t, "/register?t="+url.QueryEscape(tok))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "alice") {
		t.Errorf("body missing nick 'alice'")
	}
	if !strings.Contains(body, `name="email"`) {
		t.Errorf("body missing email field")
	}
	if !strings.Contains(body, "/privacy") {
		t.Errorf("body missing privacy link")
	}
}

func TestRegisterPostSuccess(t *testing.T) {
	e := newTestEnv(t, true, 20)
	tok := e.token(t, "@alice:hs.org")

	rec := e.post(t, url.Values{"t": {tok}, "city": {"Łódź"}, "email": {"alice@example.com"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "#1") {
		t.Errorf("body missing participant number #1: %s", rec.Body.String())
	}
	if n, _ := e.store.Count(); n != 1 {
		t.Fatalf("count = %d; want 1", n)
	}

	// /api/count reflects the new state.
	crec := e.get(t, "/api/count")
	if ct := crec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("content-type = %q; want json", ct)
	}
	var got struct {
		Count int  `json:"count"`
		Limit int  `json:"limit"`
		Open  bool `json:"open"`
	}
	if err := json.Unmarshal(crec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode count: %v", err)
	}
	if got.Count != 1 || got.Limit != 20 || !got.Open {
		t.Errorf("api/count = %+v; want {1 20 true}", got)
	}
}

func TestRegisterDuplicate(t *testing.T) {
	e := newTestEnv(t, true, 20)
	tok := e.token(t, "@alice:hs.org")

	if rec := e.post(t, url.Values{"t": {tok}, "city": {"Łódź"}, "email": {"a@example.com"}}); rec.Code != http.StatusOK {
		t.Fatalf("first post status = %d", rec.Code)
	}
	rec := e.post(t, url.Values{"t": {tok}, "city": {"Kraków"}, "email": {"a@example.com"}})
	body := rec.Body.String()
	if !strings.Contains(body, "Jesteś już zapisany") {
		t.Errorf("duplicate body missing 'już zapisany': %s", body)
	}
	if n, _ := e.store.Count(); n != 1 {
		t.Fatalf("count = %d; want 1 after duplicate", n)
	}
}

func TestRegisterFull(t *testing.T) {
	e := newTestEnv(t, true, 2)
	for _, h := range []string{"@a:hs.org", "@b:hs.org"} {
		tok := e.token(t, h)
		if rec := e.post(t, url.Values{"t": {tok}, "city": {"Łódź"}, "email": {"x@example.com"}}); rec.Code != http.StatusOK {
			t.Fatalf("post %s status = %d", h, rec.Code)
		}
	}
	// Third distinct handle exceeds the limit.
	tok := e.token(t, "@c:hs.org")
	rec := e.post(t, url.Values{"t": {tok}, "city": {"Łódź"}, "email": {"c@example.com"}})
	if !strings.Contains(rec.Body.String(), "Brak miejsc") {
		t.Errorf("full body missing 'Brak miejsc': %s", rec.Body.String())
	}
	if n, _ := e.store.Count(); n != 2 {
		t.Fatalf("count = %d; want 2", n)
	}

	// GET should also advertise no seats.
	grec := e.get(t, "/register?t="+url.QueryEscape(tok))
	if !strings.Contains(grec.Body.String(), "Brak miejsc") {
		t.Errorf("full GET missing 'Brak miejsc'")
	}
}

// TestRegisterWaitlistClassification drives the two-tier capacity model through
// the web layer: with 2 confirmed seats + 2 waiting-list places, the first two
// signups are confirmed participants, the next two land on the waiting list
// (positions #1 and #2), and the fifth is refused with "Brak miejsc".
func TestRegisterWaitlistClassification(t *testing.T) {
	e := newTestEnvSeats(t, true, 2, 2, "")
	post := func(handle string) string {
		tok := e.token(t, handle)
		rec := e.post(t, url.Values{"t": {tok}, "city": {"Łódź"}, "email": {"x@example.com"}})
		if rec.Code != http.StatusOK {
			t.Fatalf("post %s status = %d", handle, rec.Code)
		}
		return rec.Body.String()
	}

	// Confirmed participants: numbers 1 and 2.
	if b := post("@a:hs.org"); !strings.Contains(b, "numer uczestnika") || !strings.Contains(b, "#1") {
		t.Errorf("first signup should be confirmed #1: %s", b)
	}
	if b := post("@b:hs.org"); !strings.Contains(b, "#2") {
		t.Errorf("second signup should be confirmed #2: %s", b)
	}
	// Waiting list: number 3 -> position #1, number 4 -> position #2.
	b3 := post("@c:hs.org")
	if !strings.Contains(b3, "rezerwow") || !strings.Contains(b3, "#1") {
		t.Errorf("third signup should be waitlist position #1: %s", b3)
	}
	if strings.Contains(b3, "numer uczestnika") {
		t.Errorf("waitlist page must not use the confirmed-participant wording: %s", b3)
	}
	b4 := post("@d:hs.org")
	if !strings.Contains(b4, "rezerwow") || !strings.Contains(b4, "#2") {
		t.Errorf("fourth signup should be waitlist position #2: %s", b4)
	}

	// Fifth signup exceeds total capacity.
	tok := e.token(t, "@e:hs.org")
	rec := e.post(t, url.Values{"t": {tok}, "city": {"Łódź"}, "email": {"e@example.com"}})
	if !strings.Contains(rec.Body.String(), "Brak miejsc") {
		t.Errorf("fifth signup should be 'Brak miejsc': %s", rec.Body.String())
	}
	if n, _ := e.store.Count(); n != 4 {
		t.Fatalf("count = %d; want 4 (capacity never exceeded)", n)
	}
}

// TestRegisterGetWaitlistNote verifies the GET form: once confirmed seats are
// gone but waiting-list places remain, the form warns the signup joins the
// waiting list; once total capacity is reached it shows "Brak miejsc" instead.
func TestRegisterGetWaitlistNote(t *testing.T) {
	e := newTestEnvSeats(t, true, 2, 2, "")
	// Fill the two confirmed seats directly.
	for _, h := range []string{"@a:hs.org", "@b:hs.org"} {
		if _, err := e.store.Register(h, "x", "Łódź", "x@example.com", 4); err != nil {
			t.Fatalf("seed %s: %v", h, err)
		}
	}
	// count == seatLimit (2) < total (4): form shown with waiting-list note.
	grec := e.get(t, "/register?t="+url.QueryEscape(e.token(t, "@c:hs.org")))
	body := grec.Body.String()
	if !strings.Contains(body, `name="email"`) {
		t.Errorf("form must still render when waiting-list places remain: %s", body)
	}
	if !strings.Contains(body, "listę rezerwową") {
		t.Errorf("form missing waiting-list warning: %s", body)
	}

	// Fill the waiting list too -> count == total: no places at all.
	for _, h := range []string{"@c:hs.org", "@d:hs.org"} {
		if _, err := e.store.Register(h, "x", "Łódź", "x@example.com", 4); err != nil {
			t.Fatalf("seed %s: %v", h, err)
		}
	}
	grec = e.get(t, "/register?t="+url.QueryEscape(e.token(t, "@e:hs.org")))
	if !strings.Contains(grec.Body.String(), "Brak miejsc") {
		t.Errorf("full GET missing 'Brak miejsc': %s", grec.Body.String())
	}
}

// TestApiCountWaitlistFields verifies the extended /api/count JSON exposes the
// two-tier capacity fields correctly across the interesting boundaries.
func TestApiCountWaitlistFields(t *testing.T) {
	e := newTestEnvSeats(t, true, 2, 2, "") // seatLimit 2, waitlist 2, total 4
	type countResp struct {
		Count         int  `json:"count"`
		Limit         int  `json:"limit"`
		Waitlist      int  `json:"waitlist"`
		Confirmed     int  `json:"confirmed"`
		WaitlistCount int  `json:"waitlistCount"`
		Full          bool `json:"full"`
		Open          bool `json:"open"`
	}
	check := func(wantCount, wantConfirmed, wantWaitlist int, wantFull bool) {
		t.Helper()
		var got countResp
		if err := json.Unmarshal(e.get(t, "/api/count").Body.Bytes(), &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if got.Count != wantCount || got.Confirmed != wantConfirmed ||
			got.WaitlistCount != wantWaitlist || got.Full != wantFull ||
			got.Limit != 2 || got.Waitlist != 2 {
			t.Errorf("count=%d api = %+v; want count=%d confirmed=%d waitlistCount=%d full=%v limit=2 waitlist=2",
				wantCount, got, wantCount, wantConfirmed, wantWaitlist, wantFull)
		}
	}

	check(0, 0, 0, false)
	seed := func(h string) {
		if _, err := e.store.Register(h, "x", "Łódź", "x@example.com", 4); err != nil {
			t.Fatalf("seed %s: %v", h, err)
		}
	}
	seed("@a:hs.org")
	check(1, 1, 0, false)
	seed("@b:hs.org")
	check(2, 2, 0, false) // confirmed seats full, waiting list empty, not full overall
	seed("@c:hs.org")
	check(3, 2, 1, false)
	seed("@d:hs.org")
	check(4, 2, 2, true) // total capacity reached
}

func TestRegisterClosed(t *testing.T) {
	e := newTestEnv(t, false, 20)
	tok := e.token(t, "@alice:hs.org")

	grec := e.get(t, "/register?t="+url.QueryEscape(tok))
	if !strings.Contains(grec.Body.String(), "nieotwarte") {
		t.Errorf("closed GET missing 'nieotwarte': %s", grec.Body.String())
	}

	prec := e.post(t, url.Values{"t": {tok}, "city": {"Łódź"}, "email": {"a@example.com"}})
	if !strings.Contains(prec.Body.String(), "nieotwarte") {
		t.Errorf("closed POST missing 'nieotwarte'")
	}
	if n, _ := e.store.Count(); n != 0 {
		t.Fatalf("count = %d; want 0 when closed", n)
	}

	// /api/count reports open=false.
	var got struct {
		Open bool `json:"open"`
	}
	_ = json.Unmarshal(e.get(t, "/api/count").Body.Bytes(), &got)
	if got.Open {
		t.Errorf("api/count open = true; want false")
	}
}

func TestRegisterBadToken(t *testing.T) {
	e := newTestEnv(t, true, 20)

	if rec := e.get(t, "/register?t=not-a-valid-token"); rec.Code != http.StatusBadRequest {
		t.Errorf("bad token GET status = %d; want 400", rec.Code)
	}
	if rec := e.get(t, "/register"); rec.Code != http.StatusBadRequest {
		t.Errorf("missing token GET status = %d; want 400", rec.Code)
	}
	if n, _ := e.store.Count(); n != 0 {
		t.Fatalf("count = %d; want 0", n)
	}
}

// TestRegisterExpiredToken verifies the TTL: a token whose Issued time is older
// than tokenTTL is rejected with the "Link wygasł" page and never registers.
func TestRegisterExpiredToken(t *testing.T) {
	e := newTestEnv(t, true, 20)
	old := time.Now().Add(-72 * time.Hour).Unix()
	tok := e.tokenAt(t, "@alice:hs.org", old)

	rec := e.get(t, "/register?t="+url.QueryEscape(tok))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expired token GET status = %d; want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "wygasł") {
		t.Errorf("expired token body missing 'wygasł': %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), `name="email"`) {
		t.Errorf("expired token must NOT render the form")
	}

	// POST with the same stale token must also be refused (no registration).
	prec := e.post(t, url.Values{"t": {tok}, "city": {"Łódź"}, "email": {"a@example.com"}})
	if !strings.Contains(prec.Body.String(), "wygasł") {
		t.Errorf("expired token POST missing 'wygasł': %s", prec.Body.String())
	}
	if n, _ := e.store.Count(); n != 0 {
		t.Fatalf("count = %d; want 0 after expired token", n)
	}
}

// TestRegisterFutureToken verifies the clock-skew guard: a token issued far in
// the future (beyond the tolerance) is treated as expired/invalid.
func TestRegisterFutureToken(t *testing.T) {
	e := newTestEnv(t, true, 20)
	future := time.Now().Add(1 * time.Hour).Unix()
	tok := e.tokenAt(t, "@alice:hs.org", future)

	rec := e.get(t, "/register?t="+url.QueryEscape(tok))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("future token GET status = %d; want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "wygasł") {
		t.Errorf("future token body missing 'wygasł': %s", rec.Body.String())
	}
}

func TestRegisterInvalidEmail(t *testing.T) {
	e := newTestEnv(t, true, 20)
	tok := e.token(t, "@alice:hs.org")

	rec := e.post(t, url.Values{"t": {tok}, "city": {"Łódź"}, "email": {"not-an-email"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 (re-render)", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "poprawny adres e-mail") {
		t.Errorf("invalid email body missing error: %s", body)
	}
	if !strings.Contains(body, `name="email"`) {
		t.Errorf("invalid email should re-render form")
	}
	if n, _ := e.store.Count(); n != 0 {
		t.Fatalf("count = %d; want 0 after invalid email", n)
	}
}

// TestRegistrationDisabled verifies that when the age key / store are missing
// the site still serves the landing (and privacy), and registration degrades to
// 503 instead of crashing the server.
func TestRegistrationDisabled(t *testing.T) {
	sub, err := fs.Sub(embedded, ".")
	if err != nil {
		t.Fatalf("embed sub: %v", err)
	}
	h := newMux(deps{
		store:     nil,
		identity:  nil,
		seatLimit: 20,
		isOpen:    func() bool { return true },
		files:     http.FS(sub),
	})

	do := func(method, path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
		return rec
	}

	if rec := do(http.MethodGet, "/"); rec.Code != http.StatusOK {
		t.Errorf("GET / = %d; want 200 (landing must always serve)", rec.Code)
	}
	if rec := do(http.MethodGet, "/privacy"); rec.Code != http.StatusOK {
		t.Errorf("GET /privacy = %d; want 200", rec.Code)
	}
	if rec := do(http.MethodGet, "/healthz"); rec.Code != http.StatusOK {
		t.Errorf("GET /healthz = %d; want 200", rec.Code)
	}
	if rec := do(http.MethodGet, "/register?t=x"); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("GET /register = %d; want 503", rec.Code)
	}
	// /api/count still answers, reporting zero.
	crec := do(http.MethodGet, "/api/count")
	if crec.Code != http.StatusOK {
		t.Fatalf("GET /api/count = %d; want 200", crec.Code)
	}
	var got struct {
		Count int `json:"count"`
		Limit int `json:"limit"`
	}
	if err := json.Unmarshal(crec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode count: %v", err)
	}
	if got.Count != 0 || got.Limit != 20 {
		t.Errorf("api/count = %+v; want count 0 limit 20", got)
	}
}

// TestRegisterFlowBotToWeb is the full cross-component e2e: the real bot
// (matrixbot.Client.HandleRegister) encrypts a link to the same key the real
// web handler decrypts, and the link the bot ACTUALLY emits is driven through
// GET+POST /register to a stored, numbered registration. It closes the gap
// between the bot-side and web-side tests, which otherwise only meet at the
// token format.
func TestRegisterFlowBotToWeb(t *testing.T) {
	// Web side: real handler + store, with an ephemeral keypair.
	e := newTestEnv(t, true, 20)

	// Bot side: real Client encrypting to the SAME recipient the web decrypts.
	// A local fake Matrix transport captures the message the bot delivers.
	fake := &botTransport{sent: make(chan string, 1)}
	bot := matrixbot.New(fake)
	bot.Recipient = e.recipient
	bot.LinkBase = "https://dday.hs-ldz.pl/register"
	bot.TokenSecret = testTokenSecret // same shared secret the web mux verifies with

	// Empty origin room: the bot DMs the link, and skips the public nudge (there
	// is no channel to nudge here).
	if _, err := bot.HandleRegister(context.Background(), "", "@alice:hs.org"); err != nil {
		t.Fatalf("bot HandleRegister: %v", err)
	}

	var msg string
	select {
	case msg = <-fake.sent:
	case <-time.After(3 * time.Second):
		t.Fatal("bot did not send a message")
	}

	// Pull the token out of the link the bot actually emitted.
	fields := strings.Fields(msg)
	if len(fields) == 0 {
		t.Fatalf("empty bot message")
	}
	link := fields[len(fields)-1]
	u, err := url.Parse(link)
	if err != nil {
		t.Fatalf("parse bot link %q: %v", link, err)
	}
	token := u.Query().Get("t")
	if token == "" {
		t.Fatalf("no token in bot link %q", link)
	}

	// GET with the bot's token renders the form pre-filled with the nick.
	grec := e.get(t, "/register?t="+url.QueryEscape(token))
	if grec.Code != http.StatusOK || !strings.Contains(grec.Body.String(), "alice") {
		t.Fatalf("GET with bot token: code=%d, missing nick 'alice'", grec.Code)
	}

	// POST the bot's token registers the participant.
	prec := e.post(t, url.Values{"t": {token}, "city": {"Łódź"}, "email": {"alice@example.com"}})
	if prec.Code != http.StatusOK {
		t.Fatalf("register status = %d", prec.Code)
	}
	if !strings.Contains(prec.Body.String(), "#1") {
		t.Errorf("expected participant number #1, body: %s", prec.Body.String())
	}
	if n, _ := e.store.Count(); n != 1 {
		t.Fatalf("count = %d; want 1", n)
	}

	// Link expiry: re-using the bot's token after registration no longer opens
	// the form — the web side shows the "already registered" confirmation.
	grec2 := e.get(t, "/register?t="+url.QueryEscape(token))
	if body := grec2.Body.String(); !strings.Contains(body, "już zapisany") || strings.Contains(body, `name="email"`) {
		t.Errorf("second GET should be 'już zapisany' without the form, got: %s", body)
	}
}

// TestRegisterLinkExpiresAfterRegistration verifies req 2: once a handle is
// registered, GET /register with the same token stops rendering the form and
// shows the "already registered / link spent" confirmation instead.
func TestRegisterLinkExpiresAfterRegistration(t *testing.T) {
	e := newTestEnv(t, true, 20)
	tok := e.token(t, "@alice:hs.org")

	if rec := e.post(t, url.Values{"t": {tok}, "city": {"Łódź"}, "email": {"a@example.com"}}); rec.Code != http.StatusOK {
		t.Fatalf("register status = %d", rec.Code)
	}

	rec := e.get(t, "/register?t="+url.QueryEscape(tok))
	body := rec.Body.String()
	if !strings.Contains(body, "już zapisany") {
		t.Errorf("expired-link GET missing 'już zapisany': %s", body)
	}
	if strings.Contains(body, `name="email"`) {
		t.Errorf(`expired-link GET must NOT render the form (found name="email")`)
	}
}

// TestApiRegistered covers req 3 (web side): the guarded internal endpoint the
// bot uses to detect an already-registered handle.
func TestApiRegistered(t *testing.T) {
	const tok = "s3cr3t-internal-token"
	e := newTestEnvToken(t, true, 20, tok)
	const handle = "@alice:hs.org"

	getReg := func(auth, h string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/api/registered?h="+url.QueryEscape(h), nil)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		rec := httptest.NewRecorder()
		e.handler.ServeHTTP(rec, req)
		return rec
	}

	if rec := getReg("", handle); rec.Code != http.StatusUnauthorized {
		t.Errorf("no auth = %d; want 401", rec.Code)
	}
	if rec := getReg("Bearer wrong", handle); rec.Code != http.StatusUnauthorized {
		t.Errorf("wrong token = %d; want 401", rec.Code)
	}
	if rec := getReg("Bearer "+tok, ""); rec.Code != http.StatusBadRequest {
		t.Errorf("empty handle = %d; want 400", rec.Code)
	}

	var got struct {
		Registered bool `json:"registered"`
		Number     int  `json:"number"`
	}
	rec := getReg("Bearer "+tok, handle)
	if rec.Code != http.StatusOK {
		t.Fatalf("authed GET = %d; want 200", rec.Code)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Registered || got.Number != 0 {
		t.Errorf("before register = %+v; want {false 0}", got)
	}

	if _, err := e.store.Register(handle, "alice", "Łódź", "a@example.com", 20); err != nil {
		t.Fatalf("register: %v", err)
	}
	rec = getReg("Bearer "+tok, handle)
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.Registered || got.Number != 1 {
		t.Errorf("after register = %+v; want {true 1}", got)
	}

	// An empty internal token disables the endpoint entirely (404), so the
	// registration list can never leak.
	edis := newTestEnv(t, true, 20)
	req := httptest.NewRequest(http.MethodGet, "/api/registered?h="+url.QueryEscape(handle), nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	drec := httptest.NewRecorder()
	edis.handler.ServeHTTP(drec, req)
	if drec.Code != http.StatusNotFound {
		t.Errorf("disabled endpoint = %d; want 404", drec.Code)
	}
}

// TestStaticDirDoesNotLeak simulates STATIC_DIR=. (repo root): the root handler
// must serve ONLY index.html for "/", and never expose secrets like matrix.env
// or the age key via an arbitrary path (http.FileServer would have leaked them).
func TestStaticDirDoesNotLeak(t *testing.T) {
	h := newMux(deps{
		seatLimit: 20,
		isOpen:    func() bool { return true },
		files:     http.Dir("."), // test cwd is the repo root
	})
	do := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec
	}
	for _, p := range []string{"/matrix.env", "/config/dday_ed25519", "/server.go", "/dday.db", "/.env"} {
		if rec := do(p); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d; want 404 (hidden path must not leak)", p, rec.Code)
		}
	}
	rec := do("/")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / = %d; want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "D-Day") {
		t.Errorf("GET / missing landing content")
	}
	// Relative href, so the same file also works on GitHub Pages and file://.
	if !strings.Contains(rec.Body.String(), `<link rel="stylesheet" href="style.css">`) {
		t.Errorf("landing page does not link the shared stylesheet")
	}
	if !strings.Contains(rec.Body.String(), `<body class="landing">`) {
		t.Errorf("landing page missing the body.landing scope class")
	}
	// The stylesheet is on the allowlist — every page links it — while the rest
	// of the directory stays invisible.
	if rec := do("/style.css"); rec.Code != http.StatusOK {
		t.Errorf("GET /style.css = %d; want 200 (allowlisted static file)", rec.Code)
	}
}

// TestReadEndpointsGETOnly verifies the read endpoints answer 405 (Allow: GET)
// to non-GET methods, and that /api/count is marked no-store.
func TestReadEndpointsGETOnly(t *testing.T) {
	e := newTestEnv(t, true, 20)
	for _, path := range []string{"/api/count", "/api/registered", "/privacy"} {
		req := httptest.NewRequest(http.MethodPost, path, nil)
		rec := httptest.NewRecorder()
		e.handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("POST %s = %d; want 405", path, rec.Code)
		}
		if allow := rec.Header().Get("Allow"); allow != "GET" {
			t.Errorf("POST %s Allow = %q; want GET", path, allow)
		}
	}
	if cc := e.get(t, "/api/count").Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("/api/count Cache-Control = %q; want no-store", cc)
	}
}

// TestRegisterFieldTooLong verifies server-side length bounds: an oversized city
// or e-mail re-renders the form with an error and stores nothing.
func TestRegisterFieldTooLong(t *testing.T) {
	e := newTestEnv(t, true, 20)
	long := strings.Repeat("a", 500)

	rec := e.post(t, url.Values{"t": {e.token(t, "@alice:hs.org")}, "city": {long}, "email": {"a@example.com"}})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `name="email"`) {
		t.Errorf("too-long city: code=%d, expected form re-render", rec.Code)
	}

	rec = e.post(t, url.Values{"t": {e.token(t, "@bob:hs.org")}, "city": {"Łódź"}, "email": {long + "@example.com"}})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `name="email"`) {
		t.Errorf("too-long email: code=%d, expected form re-render", rec.Code)
	}

	if n, _ := e.store.Count(); n != 0 {
		t.Fatalf("count = %d; want 0 after oversized submissions", n)
	}
}

// TestRegisterEmailNormalized verifies the stored e-mail is the bare address,
// not the raw "Name <addr>" form the parser accepts.
func TestRegisterEmailNormalized(t *testing.T) {
	e := newTestEnv(t, true, 20)
	tok := e.token(t, "@alice:hs.org")

	rec := e.post(t, url.Values{"t": {tok}, "city": {"Łódź"}, "email": {"Jan <jan@x.pl>"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rec.Code)
	}
	got, err := e.store.Email("@alice:hs.org")
	if err != nil {
		t.Fatalf("store email: %v", err)
	}
	if got != "jan@x.pl" {
		t.Errorf("stored email = %q; want jan@x.pl", got)
	}
}

// TestRegisterEmptyCity verifies city stays required: a blank city re-renders
// the form with an error and stores nothing.
func TestRegisterEmptyCity(t *testing.T) {
	e := newTestEnv(t, true, 20)
	tok := e.token(t, "@alice:hs.org")

	rec := e.post(t, url.Values{"t": {tok}, "city": {"   "}, "email": {"a@example.com"}})
	if !strings.Contains(rec.Body.String(), "Podaj miejscowość") {
		t.Errorf("empty city missing error: %s", rec.Body.String())
	}
	if n, _ := e.store.Count(); n != 0 {
		t.Fatalf("count = %d; want 0 after empty city", n)
	}
}

// TestStylesheet covers the single stylesheet every page links: it is served as
// text/css with real content, is cacheable, and answers GET only.
func TestStylesheet(t *testing.T) {
	e := newTestEnv(t, true, 20)

	rec := e.get(t, "/style.css")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /style.css = %d; want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/css; charset=utf-8" {
		t.Errorf("Content-Type = %q; want text/css; charset=utf-8", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); !strings.HasPrefix(cc, "public, max-age=") {
		t.Errorf("Cache-Control = %q; want a public max-age", cc)
	}
	body := rec.Body.String()
	if len(body) == 0 {
		t.Fatal("stylesheet body is empty")
	}
	// One source of truth: the tokens plus a rule from each page family.
	for _, want := range []string{"--accent:#39d353", "body.landing .wrap", "body.page .btn", "body.privacy .note"} {
		if !strings.Contains(body, want) {
			t.Errorf("stylesheet missing %q", want)
		}
	}

	post := httptest.NewRequest(http.MethodPost, "/style.css", nil)
	prec := httptest.NewRecorder()
	e.handler.ServeHTTP(prec, post)
	if prec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /style.css = %d; want 405", prec.Code)
	}
	if allow := prec.Header().Get("Allow"); allow != "GET" {
		t.Errorf("Allow = %q; want GET", allow)
	}
}

// TestServerPagesLinkStylesheet pins the de-duplication: the server-rendered
// pages must pull the shared stylesheet rather than carry an inline copy.
func TestServerPagesLinkStylesheet(t *testing.T) {
	e := newTestEnvAdmin(t, 20, 20, "admintok")
	tok := e.token(t, "@alice:matrix.org")
	if rec := e.postTo(t, "/register", url.Values{
		"t": {tok}, "city": {"Łódź"}, "email": {"alice@example.com"},
	}); rec.Code != http.StatusOK {
		t.Fatalf("seed registration = %d; want 200", rec.Code)
	}

	pages := map[string]string{
		"form":  "/register?t=" + url.QueryEscape(e.token(t, "@bob:matrix.org")),
		"panel": "/panel?t=" + url.QueryEscape(e.tokenKind(t, "@alice:matrix.org", matrixbot.KindPanel)),
		"admin": "/admin?t=admintok",
	}
	for name, path := range pages {
		rec := e.get(t, path)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s (%s) = %d; want 200", path, name, rec.Code)
		}
		body := rec.Body.String()
		if !strings.Contains(body, `<link rel="stylesheet" href="/style.css">`) {
			t.Errorf("%s page does not link /style.css", name)
		}
		if strings.Contains(body, "<style>") {
			t.Errorf("%s page still carries an inline <style> block", name)
		}
		if !strings.Contains(body, `<body class="page"`) {
			t.Errorf("%s page missing the body.page scope class", name)
		}
	}
}

func TestPrivacyPage(t *testing.T) {
	e := newTestEnv(t, true, 20)
	rec := e.get(t, "/privacy")
	if rec.Code != http.StatusOK {
		t.Fatalf("privacy status = %d; want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Stowarzyszenie Hakierspejs Łódź") {
		t.Errorf("privacy body missing administrator name")
	}
	if !strings.Contains(rec.Body.String(), "art. 6 ust. 1 lit. b RODO") {
		t.Errorf("privacy body missing legal basis")
	}
	// Relative href: /privacy has no trailing slash, so it resolves to
	// /style.css here and to /dday/style.css on GitHub Pages.
	if !strings.Contains(rec.Body.String(), `<link rel="stylesheet" href="style.css">`) {
		t.Errorf("privacy page does not link the shared stylesheet")
	}
	if !strings.Contains(rec.Body.String(), `<body class="privacy">`) {
		t.Errorf("privacy page missing the body.privacy scope class")
	}
}
