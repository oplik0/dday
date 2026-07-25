package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"filippo.io/age"

	"github.com/d33mobile/dday/internal/matrixbot"
	"github.com/d33mobile/dday/internal/regwindow"
)

// applyConfig populates c with all transport-independent configuration read from
// the environment: the age recipient, the registration/panel link bases, the
// registration time gate, an optional room allowlist, the internal-API-backed
// already-registered check and announcement feed, the announcement target and
// cadence, and the on-disk DM cache (which it loads). It deliberately holds no
// Matrix wiring, so it stays free of the goolm build tag and unit-testable. The
// Matrix transport is injected separately (New) and started by Serve in main.go.
func applyConfig(c *matrixbot.Client) error {
	registerURL := env("REGISTER_URL", "https://dday.hs-ldz.pl/")

	recipient, err := loadRecipient()
	if err != nil {
		return fmt.Errorf("load recipient: %w", err)
	}
	c.Recipient = recipient
	c.LinkBase = registerURL
	c.TokenSecret = os.Getenv("TOKEN_SECRET")
	c.IsOpen = regwindow.Open

	// Participant-panel base URL. PANEL_URL wins; otherwise it is derived from
	// REGISTER_URL's origin (where the web service serves /panel). A REGISTER_URL
	// without scheme/host leaves PanelBase empty, and the bot then falls back to
	// the link-less "already signed up" reply instead of handing out a broken URL.
	c.PanelBase = panelURL(registerURL)
	if c.PanelBase == "" {
		log.Printf("no PANEL_URL and REGISTER_URL has no usable origin; panel magic links disabled")
	} else {
		log.Printf("participant panel links point at %s", c.PanelBase)
	}

	// Optional allowlist: when ALLOWED_ROOMS is set (comma-separated room ids),
	// the bot reacts to "!start" only in those rooms. Unset means everywhere.
	if rooms := strings.TrimSpace(os.Getenv("ALLOWED_ROOMS")); rooms != "" {
		c.AllowedRooms = matrixbot.ParseAllowedRooms(rooms)
		log.Printf("restricting !start to %d allowed room(s)", len(c.AllowedRooms))
	}

	// When INTERNAL_TOKEN is set, ask the web service whether a handle is already
	// registered before issuing a link (fail-open otherwise), and poll the same
	// internal feed for new signups to announce.
	if tok := strings.TrimSpace(os.Getenv("INTERNAL_TOKEN")); tok != "" {
		origin, err := originOf(registerURL)
		if err != nil {
			return fmt.Errorf("REGISTER_URL: %w", err)
		}
		c.CheckRegistered = registeredChecker(httpClient(), origin, tok)
		log.Printf("already-registered check enabled against %s", origin)
		c.FetchNewRegistrations = registrationsFetcher(httpClient(), origin, tok)
	} else {
		log.Printf("no INTERNAL_TOKEN; already-registered check and registration announcements disabled")
	}

	// Announcement target: ANNOUNCE_ROOM wins, else the home channel MATRIX_ROOM.
	c.AnnounceRoom = env("ANNOUNCE_ROOM", os.Getenv("MATRIX_ROOM"))
	c.AnnounceInterval = announceInterval()
	if c.AnnounceRoom == "" {
		log.Printf("no ANNOUNCE_ROOM/MATRIX_ROOM; registration announcements disabled")
	}

	// Optional on-disk persistence of the DM room cache (handle -> room id) and the
	// last-announced high-water mark. Unset keeps the state purely in-memory.
	c.CachePath = env("DM_CACHE_PATH", "")
	if c.CachePath != "" {
		n, err := c.LoadCache()
		if err != nil {
			log.Printf("load DM cache from %s: %v (continuing empty)", c.CachePath, err)
		} else {
			log.Printf("loaded %d DM cache entr(y/ies) from %s", n, c.CachePath)
		}
	}
	return nil
}

// This file holds the transport-agnostic configuration helpers, free of any
// mautrix import, so they compile and are unit-tested under a plain
// `go test ./...` (no -tags goolm). The Matrix run loop lives in main.go, which
// is built only with -tags goolm; a stub in main_nogoolm.go keeps the package
// buildable without it.

// loadRecipient resolves the age recipient (SSH ed25519 public key) used to
// encrypt registration link tokens. AGE_PUB_DATA (base64 of the public key)
// takes precedence over the AGE_PUB file path — passing the key by value avoids
// container config-mount issues (matrix.env sets AGE_PUB to a host path that
// does not exist inside the image).
func loadRecipient() (age.Recipient, error) {
	if b64 := strings.TrimSpace(os.Getenv("AGE_PUB_DATA")); b64 != "" {
		data, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			return nil, fmt.Errorf("AGE_PUB_DATA base64: %w", err)
		}
		return matrixbot.ParseRecipient(string(data))
	}
	return matrixbot.LoadRecipient(mustEnv("AGE_PUB"))
}

// panelURL resolves the base URL of the participant panel: the explicit
// PANEL_URL env var, else the origin of registerURL with "/panel" appended. It
// returns "" when neither is available (registerURL lacks a scheme/host), which
// the caller treats as "panel magic links disabled".
func panelURL(registerURL string) string {
	if v := strings.TrimSpace(os.Getenv("PANEL_URL")); v != "" {
		return v
	}
	origin, err := originOf(registerURL)
	if err != nil {
		return ""
	}
	return origin + "/panel"
}

// originOf extracts the scheme://host origin from a full URL.
func originOf(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	if u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("URL %q lacks scheme/host", raw)
	}
	return u.Scheme + "://" + u.Host, nil
}

// registeredChecker returns a CheckRegistered function that queries the web
// service's internal GET /api/registered?h= endpoint with the shared bearer
// token. A non-200 response or transport error surfaces as an error, which the
// bot treats fail-open.
func registeredChecker(httpc *http.Client, origin, token string) func(string) (int, bool, error) {
	return func(handle string) (int, bool, error) {
		req, err := http.NewRequest(http.MethodGet, origin+"/api/registered?h="+url.QueryEscape(handle), nil)
		if err != nil {
			return 0, false, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := httpc.Do(req)
		if err != nil {
			return 0, false, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return 0, false, fmt.Errorf("GET /api/registered -> %d", resp.StatusCode)
		}
		var out struct {
			Registered bool `json:"registered"`
			Number     int  `json:"number"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			return 0, false, err
		}
		return out.Number, out.Registered, nil
	}
}

// registrationsFetcher returns a FetchNewRegistrations function that queries the
// web service's internal GET /api/registrations?since= endpoint with the shared
// bearer token. The endpoint returns handle/nick plus the participant number and
// standing — no e-mail or city, so nothing personal can reach the public room.
func registrationsFetcher(httpc *http.Client, origin, token string) func(int) ([]matrixbot.NewRegistration, error) {
	return func(since int) ([]matrixbot.NewRegistration, error) {
		req, err := http.NewRequest(http.MethodGet,
			origin+"/api/registrations?since="+strconv.Itoa(since), nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := httpc.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("GET /api/registrations -> %d", resp.StatusCode)
		}
		var out struct {
			Registrations []matrixbot.NewRegistration `json:"registrations"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			return nil, err
		}
		return out.Registrations, nil
	}
}

// announceInterval reads ANNOUNCE_INTERVAL as a Go duration. Unset, unparsable
// or non-positive values fall back to the package default (logged), so a typo
// degrades to the normal cadence instead of a hot loop.
func announceInterval() time.Duration {
	raw := strings.TrimSpace(os.Getenv("ANNOUNCE_INTERVAL"))
	if raw == "" {
		return 0
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		log.Printf("ignoring ANNOUNCE_INTERVAL=%q (%v); using the default", raw, err)
		return 0
	}
	return d
}

// sharedHTTPClient is the HTTP client used for the web service's internal API
// (already-registered check + announcement feed). A single client reuses
// connections across both closures.
var sharedHTTPClient = &http.Client{Timeout: 60 * time.Second}

// httpClient returns the shared HTTP client for the internal web-service API.
func httpClient() *http.Client { return sharedHTTPClient }

func env(k, fallback string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return fallback
}

func mustEnv(k string) string {
	v := os.Getenv(k)
	if v == "" {
		log.Fatalf("missing required env %s", k)
	}
	return v
}
