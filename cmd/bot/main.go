//go:build goolm

// Command bot runs the D-Day Matrix bot.
//
// On "!start" (legacy alias: "!register") it opens a private DM with the sender
// and sends a link whose token is an age-encrypted timestamp (see
// internal/matrixbot): a registration link for a newcomer, or a magic link to
// the participant panel for someone who is already signed up. The bot speaks the
// end-to-end encrypted client-server API through mautrix + cryptohelper with
// pure-Go olm, so a "!start" in an Element-default encrypted DM is decrypted,
// handled by the same logic as a plaintext one, and answered with an encrypted
// reply. It MUST be built with `-tags goolm` (and CGO_ENABLED=0 for a static
// binary); a build without the tag runs the stub in main_nogoolm.go.
//
// Config (from matrix.env / environment):
//
//	MATRIX_HOMESERVER   e.g. https://matrix.org
//	MATRIX_USER         e.g. @ddaybot:matrix.org
//	MATRIX_PASSWORD     bot password
//	CRYPTO_DB_PATH      path to the pure-Go SQLite crypto store on a volume
//	                    (default /data/crypto.db); persists device + megolm keys
//	CRYPTO_PICKLE_KEY   stable key encrypting the olm account at rest; unset
//	                    generates an ephemeral one (a restart loses all sessions)
//	AGE_PUB_DATA        base64 of the SSH ed25519 public key (recipient); preferred
//	AGE_PUB             SSH ed25519 public key file (recipient); fallback
//	REGISTER_URL        base link the token is appended to
//	PANEL_URL           participant-panel base; default REGISTER_URL origin + /panel
//	TOKEN_SECRET        shared HMAC key authenticating the link token (must match web)
//	ALLOWED_ROOMS       optional comma-separated room ids; if set, !start is
//	                    only honoured in those rooms (unset = every room)
//	INTERNAL_TOKEN      shared bearer token for the web service's internal API
//	                    (already-registered check and the announcement feed)
//	ANNOUNCE_ROOM       room new registrations are announced in; defaults to
//	                    MATRIX_ROOM. Empty (and both empty) disables announcing
//	ANNOUNCE_INTERVAL   how often to poll for new registrations (e.g. "30s")
//	DM_CACHE_PATH       optional JSON file persisting the DM cache across restarts
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/rs/zerolog"

	"github.com/d33mobile/dday/internal/matrixbot"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	mxLog := zerolog.New(zerolog.ConsoleWriter{Out: os.Stderr, TimeFormat: time.RFC3339}).
		With().Timestamp().Str("component", "matrix").Logger()

	// Build the Matrix transport (login + pure-Go crypto store) first; the command
	// logic is transport-agnostic and gets it injected via New.
	mx, err := matrixbot.NewMxClient(ctx, matrixbot.MxConfig{
		Homeserver:   mustEnv("MATRIX_HOMESERVER"),
		User:         mustEnv("MATRIX_USER"),
		Password:     mustEnv("MATRIX_PASSWORD"),
		CryptoDBPath: env("CRYPTO_DB_PATH", "/data/crypto.db"),
		PickleKey:    pickleKey(),
		Log:          mxLog,
	})
	if err != nil {
		log.Fatalf("matrix init: %v", err)
	}
	defer func() { _ = mx.Close() }()

	c := matrixbot.New(mx)
	if err := applyConfig(c); err != nil {
		log.Fatalf("config: %v", err)
	}
	log.Printf("logged in as %s", c.Self())

	if err := c.Serve(ctx, mx); err != nil {
		log.Fatal(err)
	}
}

// pickleKey returns the crypto store's pickle key. CRYPTO_PICKLE_KEY is strongly
// preferred and must stay stable across restarts: it encrypts the olm account at
// rest, so reusing it (with the same CRYPTO_DB_PATH) keeps the bot's device and
// its megolm sessions. When it is unset the bot generates a random one and warns
// — handy for a throwaway dev run, but a restart then loses every encrypted
// session, so production must set it.
func pickleKey() []byte {
	if k := strings.TrimSpace(os.Getenv("CRYPTO_PICKLE_KEY")); k != "" {
		return []byte(k)
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		log.Fatalf("generate ephemeral pickle key: %v", err)
	}
	log.Printf("WARNING: CRYPTO_PICKLE_KEY not set; generated an ephemeral one — " +
		"a restart will lose all megolm sessions. Set CRYPTO_PICKLE_KEY to persist them.")
	return []byte(hex.EncodeToString(buf))
}
