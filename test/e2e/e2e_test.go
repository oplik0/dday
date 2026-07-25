//go:build e2e && goolm

// Package e2e holds the end-to-end encryption integration test: it runs the
// REAL bot code (internal/matrixbot's mautrix + cryptohelper adapter) against a
// live Synapse and proves the full encrypted round-trip — the bot decrypts a
// user's "!start" sent in an Element-default encrypted DM, and the user decrypts
// the bot's encrypted reply carrying a valid registration link.
//
// It is deliberately gated behind BOTH build tags:
//
//   - goolm selects the pure-Go olm/megolm crypto (same as the production bot);
//   - e2e keeps it out of the ordinary `go test -race -tags goolm ./...` CI run,
//     which uses an in-memory fake and needs no homeserver.
//
// Run it with `make e2e` (stands up a throwaway Synapse, runs this, tears it
// down) or, against an already-running homeserver:
//
//	E2E_HOMESERVER=http://localhost:8008 go test -tags 'e2e goolm' -run E2E -v ./test/e2e/...
//
// If no homeserver is reachable the test SKIPS (never fails), so a developer
// without Docker is not blocked.
package e2e

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	"filippo.io/age/agessh"
	"github.com/rs/zerolog"
	"go.mau.fi/util/dbutil"
	"golang.org/x/crypto/ssh"
	_ "modernc.org/sqlite" // registers the pure-Go "sqlite" database/sql driver

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/crypto/cryptohelper"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/d33mobile/dday/internal/matrixbot"
	"github.com/d33mobile/dday/internal/regwindow"
)

// Local-only throwaway accounts, matching scripts/e2e-synapse.sh.
const (
	botUser   = "bot"
	botPass   = "botpass123"
	aliceUser = "alice"
	alicePass = "alicepass123"
)

// TestE2EEncryptedRegisterRoundTrip is the whole proof. Architecture:
//
//	@bot   <- the actual production adapter (matrixbot.NewMxClient + Client.Serve)
//	@alice <- a second cryptohelper client standing in for a real Element user
//
// alice creates an ENCRYPTED (m.megolm.v1.aes-sha2) DM, invites the bot, waits
// for it to join (so the megolm session targets the bot's device), then sends an
// encrypted "!start". The bot must decrypt it (proof of INPUT), run its normal
// HandleRegister, and — because the room is a 1:1 DM the bot is in — reply IN
// THAT encrypted room with the registration link. alice must decrypt that reply
// (proof of OUTPUT). Finally we decrypt the link's age token with alice's own
// ephemeral identity and assert Handle == "@alice:...", proving it is a real,
// correct link for that exact user.
func TestE2EEncryptedRegisterRoundTrip(t *testing.T) { registerRoundTrip(t, true) }

// TestE2EPlaintextRegisterRoundTrip is the no-regression guard for the plaintext
// path: the SAME real bot, in an UNENCRYPTED 1:1 DM, must still decrypt-free
// receive "!start" and answer with a plaintext registration link. It proves the
// goolm/cryptohelper port did not break the ordinary channel behaviour.
func TestE2EPlaintextRegisterRoundTrip(t *testing.T) { registerRoundTrip(t, false) }

func registerRoundTrip(t *testing.T, encrypted bool) {
	hs := os.Getenv("E2E_HOMESERVER")
	if hs == "" {
		hs = "http://localhost:8008"
	}
	if !homeserverUp(hs) {
		t.Skipf("no Synapse reachable at %s (set E2E_HOMESERVER or run `make e2e`); skipping E2EE integration test", hs)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	// Ephemeral link-token crypto, generated per-run exactly like the unit tests.
	recipient, identity := genKeypair(t)
	secret := randomHex(t, 16)
	// Force the registration window open through the real gate the bot uses.
	t.Setenv("REGISTRATION_OPEN", "1")

	// ---- BOT: the real production adapter + command logic ----
	mx, err := matrixbot.NewMxClient(ctx, matrixbot.MxConfig{
		Homeserver:   hs,
		User:         botUser,
		Password:     botPass,
		CryptoDBPath: filepath.Join(t.TempDir(), "bot-crypto.db"),
		PickleKey:    []byte(randomHex(t, 16)),
		Log:          testLogger("bot"),
	})
	if err != nil {
		t.Fatalf("bot NewMxClient: %v", err)
	}
	defer func() { _ = mx.Close() }()

	bot := matrixbot.New(mx)
	bot.Recipient = recipient
	bot.LinkBase = "https://dday.example/register"
	bot.TokenSecret = secret
	bot.IsOpen = regwindow.Open // REGISTRATION_OPEN=1 -> always open
	// CheckRegistered stays nil: fail-open, so the bot issues a registration link.
	// PanelBase stays empty: this run only exercises the newcomer link path.

	serveDone := make(chan error, 1)
	go func() { serveDone <- bot.Serve(ctx, mx) }()
	botID := id.UserID(bot.Self())
	t.Logf("bot online as %s", botID)

	// ---- ALICE: a second real cryptohelper client (the "user") ----
	alice, aliceMsgs := newAliceClient(t, ctx, hs)
	t.Logf("alice online as %s", alice.UserID)

	// Let both initial syncs settle and device keys propagate before encrypting.
	time.Sleep(2 * time.Second)

	// ---- alice creates the DM (ENCRYPTED or plaintext) and invites the bot ----
	req := &mautrix.ReqCreateRoom{
		Preset:   "trusted_private_chat",
		Invite:   []id.UserID{botID},
		IsDirect: true,
	}
	if encrypted {
		req.InitialState = []*event.Event{{
			Type: event.StateEncryption,
			Content: event.Content{Parsed: &event.EncryptionEventContent{
				Algorithm: id.AlgorithmMegolmV1,
			}},
		}}
	}
	resp, err := alice.CreateRoom(ctx, req)
	if err != nil {
		t.Fatalf("alice CreateRoom: %v", err)
	}
	roomID := resp.RoomID
	t.Logf("alice created room %s (encrypted=%v), invited %s", roomID, encrypted, botID)

	// The bot must be joined before the first message so the megolm session
	// covers its device (otherwise the first ciphertext is undecryptable).
	waitForJoin(t, ctx, alice, roomID, botID)

	// ---- alice sends "!start" (auto-encrypted iff the room is encrypted) ----
	if _, err := alice.SendMessageEvent(ctx, roomID, event.EventMessage, &event.MessageEventContent{
		MsgType: event.MsgText,
		Body:    "!start",
	}); err != nil {
		t.Fatalf("alice send !start: %v", err)
	}
	t.Logf("alice sent \"!start\" (encrypted=%v) to %s", encrypted, roomID)

	// ---- assert: alice receives a DECRYPTED reply from the bot with the link ----
	deadline := time.After(90 * time.Second)
	for {
		select {
		case evt := <-aliceMsgs:
			if evt.Sender != botID {
				continue
			}
			decrypted := evt.Mautrix.EventSource&event.SourceDecrypted != 0
			body := evt.Content.AsMessage().Body
			t.Logf("alice received from bot in %s (decrypted=%v): %q", evt.RoomID, decrypted, body)
			if !strings.Contains(body, "/register?t=") {
				continue // e.g. a nudge; keep waiting for the link message
			}
			// The reply's encryption state must match the room's: end-to-end
			// decrypted in the encrypted room, plaintext in the channel.
			if decrypted != encrypted {
				t.Fatalf("bot reply decrypted=%v, want %v (room encrypted=%v): %q", decrypted, encrypted, encrypted, body)
			}
			if !strings.Contains(body, "Zapisy na D-Day") {
				t.Errorf("reply missing expected copy %q: %q", "Zapisy na D-Day", body)
			}

			// PROOF-OF-OUTPUT confirmed. Now prove the link is a real, correct
			// registration token for THIS user by decrypting it with alice's
			// ephemeral age identity and verifying the embedded handle.
			payload := decodeLinkToken(t, identity, secret, body)
			if payload.Handle != alice.UserID.String() {
				t.Fatalf("token Handle = %q; want %q", payload.Handle, alice.UserID.String())
			}
			if payload.Kind != matrixbot.KindReg {
				t.Fatalf("token Kind = %q; want %q", payload.Kind, matrixbot.KindReg)
			}
			t.Logf("PASS (encrypted=%v): round-trip complete — bot received \"!start\" (it only ever "+
				"answers !start), alice got the reply (decrypted=%v), token.Handle=%s kind=%s",
				encrypted, decrypted, payload.Handle, payload.Kind)
			return

		case err := <-serveDone:
			t.Fatalf("bot Serve exited before reply: %v", err)

		case <-deadline:
			t.Fatalf("timed out waiting for the bot's encrypted registration link")
		}
	}
}

// decodeLinkToken pulls the https://.../register?t=<token> URL out of the bot's
// message body and age-decrypts + MAC-verifies the token with alice's identity,
// returning the payload. Any failure fails the test.
func decodeLinkToken(t *testing.T, identity age.Identity, secret, body string) matrixbot.RegPayload {
	t.Helper()
	idx := strings.Index(body, "https://")
	if idx < 0 {
		t.Fatalf("no URL in reply body: %q", body)
	}
	raw := strings.TrimSpace(body[idx:])
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse link %q: %v", raw, err)
	}
	tok := u.Query().Get("t")
	if tok == "" {
		t.Fatalf("link has no ?t= token: %q", raw)
	}
	payload, err := matrixbot.DecodeRegToken(identity, secret, tok)
	if err != nil {
		t.Fatalf("decode registration token: %v", err)
	}
	return payload
}

// newAliceClient logs a second cryptohelper client in as @alice, wires a syncer
// that forwards every message NOT from alice onto the returned channel, and
// starts syncing. The crypto store is a fresh pure-Go SQLite db in t.TempDir().
func newAliceClient(t *testing.T, ctx context.Context, hs string) (*mautrix.Client, chan *event.Event) {
	t.Helper()
	cli, err := mautrix.NewClient(hs, "", "")
	if err != nil {
		t.Fatalf("alice NewClient: %v", err)
	}
	cli.Log = testLogger("alice")

	db := openCryptoDB(t, filepath.Join(t.TempDir(), "alice-crypto.db"))
	ch, err := cryptohelper.NewCryptoHelper(cli, []byte(randomHex(t, 16)), db)
	if err != nil {
		t.Fatalf("alice NewCryptoHelper: %v", err)
	}
	ch.LoginAs = &mautrix.ReqLogin{
		Type:                     mautrix.AuthTypePassword,
		Identifier:               mautrix.UserIdentifier{Type: mautrix.IdentifierTypeUser, User: aliceUser},
		Password:                 alicePass,
		InitialDeviceDisplayName: "alice-e2e",
	}
	if err := ch.Init(ctx); err != nil {
		t.Fatalf("alice crypto init: %v", err)
	}
	cli.Crypto = ch
	t.Cleanup(func() { _ = ch.Close() })

	msgs := make(chan *event.Event, 32)
	syncer := cli.Syncer.(*mautrix.DefaultSyncer)
	syncer.OnEventType(event.EventMessage, func(_ context.Context, evt *event.Event) {
		if evt.Sender == cli.UserID {
			return
		}
		select {
		case msgs <- evt:
		default:
		}
	})
	go func() { _ = cli.SyncWithContext(ctx) }()
	return cli, msgs
}

// waitForJoin polls JoinedMembers until user is joined to roomID, failing the
// test if it never appears.
func waitForJoin(t *testing.T, ctx context.Context, cli *mautrix.Client, roomID id.RoomID, user id.UserID) {
	t.Helper()
	for i := 0; i < 120; i++ {
		if members, err := cli.JoinedMembers(ctx, roomID); err == nil {
			if _, ok := members.Joined[user]; ok {
				t.Logf("%s joined %s", user, roomID)
				return
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("%s never joined %s within timeout", user, roomID)
}

// homeserverUp reports whether the client-server API answers at hs. Used to
// t.Skip cleanly when no Synapse is running.
func homeserverUp(hs string) bool {
	c := &http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get(strings.TrimRight(hs, "/") + "/_matrix/client/versions")
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// openCryptoDB builds a *dbutil.Database backed by pure-Go modernc.org/sqlite,
// mirroring the production adapter's store so alice runs the same crypto stack.
func openCryptoDB(t *testing.T, path string) *dbutil.Database {
	t.Helper()
	dsn := fmt.Sprintf(
		"file:%s?_txlock=immediate&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)",
		path,
	)
	db, err := dbutil.NewWithDialect(dsn, "sqlite")
	if err != nil {
		t.Fatalf("open crypto db %s: %v", path, err)
	}
	return db
}

// genKeypair mints an in-memory SSH ed25519 keypair and returns the matching age
// recipient (encrypts link tokens, given to the bot) and identity (decrypts +
// verifies them, used by the assertion), mirroring the unit-test helper.
func genKeypair(t *testing.T) (age.Recipient, age.Identity) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "dday-e2e")
	if err != nil {
		t.Fatalf("marshal private key: %v", err)
	}
	identity, err := agessh.ParseIdentity(pem.EncodeToMemory(block))
	if err != nil {
		t.Fatalf("parse identity: %v", err)
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatalf("new public key: %v", err)
	}
	recipient, err := agessh.ParseRecipient(string(ssh.MarshalAuthorizedKey(sshPub)))
	if err != nil {
		t.Fatalf("parse recipient: %v", err)
	}
	return recipient, identity
}

// randomHex returns 2n hex chars of randomness (for secrets / pickle keys).
func randomHex(t *testing.T, n int) string {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return hex.EncodeToString(b)
}

// testLogger returns a zerolog logger tagged with component, writing to stderr so
// `go test -v` surfaces the bot/alice crypto and sync activity as round-trip proof.
func testLogger(component string) zerolog.Logger {
	return zerolog.New(zerolog.ConsoleWriter{Out: os.Stderr, TimeFormat: "15:04:05"}).
		With().Timestamp().Str("component", component).Logger()
}
