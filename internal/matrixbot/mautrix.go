//go:build goolm

// The production matrixOps adapter: a thin wrapper over maunium.net/go/mautrix
// with end-to-end encryption via cryptohelper and pure-Go olm/megolm.
//
// It MUST be built with `-tags goolm` (and CGO_ENABLED=0 for a static binary):
// without the tag mautrix pulls in libolm via cgo, which does not link at
// CGO_ENABLED=0. The whole file is therefore behind the goolm build tag; a
// non-goolm build (plain `go build ./...`) simply omits it, and the bot's
// package main provides a stub that refuses to run without the tag.
//
// The crypto store is SQLite via modernc.org/sqlite (pure Go) wrapped in a
// *dbutil.Database — passing that concrete type to cryptohelper.NewCryptoHelper
// bypasses its string-path branch, which would route to the cgo sqlite3-fk-wal
// driver and fail to link. Device identity and megolm sessions persist in that
// store: reusing the same DB file + pickle key across restarts reuses the same
// device (cryptohelper finds the stored device id at login), so encrypted
// history is not lost on redeploy.
package matrixbot

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/rs/zerolog"
	"go.mau.fi/util/dbutil"
	_ "modernc.org/sqlite" // registers the pure-Go "sqlite" database/sql driver

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/crypto/cryptohelper"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

// mxClient is the production matrixOps implementation.
type mxClient struct {
	cli *mautrix.Client
	ch  *cryptohelper.CryptoHelper
	log zerolog.Logger
}

var _ matrixOps = (*mxClient)(nil)

// MxConfig configures the Matrix connection and crypto store.
type MxConfig struct {
	Homeserver   string // base URL, e.g. https://matrix.org
	User         string // MXID or localpart to log in as
	Password     string // account password
	CryptoDBPath string // path to the SQLite crypto store, e.g. /data/crypto.db
	PickleKey    []byte // encrypts the olm account at rest; must be stable + non-empty
	Log          zerolog.Logger
}

// NewMxClient logs in, opens the pure-Go crypto store and initialises the olm
// machine. It reuses the device already stored in CryptoDBPath (so a restart
// keeps the same device id and its megolm sessions). It must be called before
// Serve. The caller owns Close via the returned *mxClient's ch.
func NewMxClient(ctx context.Context, cfg MxConfig) (*mxClient, error) {
	if len(cfg.PickleKey) == 0 {
		return nil, fmt.Errorf("crypto pickle key must be non-empty")
	}
	cli, err := mautrix.NewClient(cfg.Homeserver, "", "")
	if err != nil {
		return nil, fmt.Errorf("new mautrix client: %w", err)
	}
	cli.Log = cfg.Log

	db, err := newCryptoDB(cfg.CryptoDBPath, cfg.Log.With().Str("db", "crypto").Logger())
	if err != nil {
		return nil, fmt.Errorf("open crypto db: %w", err)
	}

	ch, err := cryptohelper.NewCryptoHelper(cli, cfg.PickleKey, db)
	if err != nil {
		return nil, fmt.Errorf("new crypto helper: %w", err)
	}
	// Password login. cryptohelper reuses the device id already stored in the
	// crypto DB (FindDeviceID), so across restarts the bot keeps the same device
	// and its megolm sessions instead of registering a fresh, unverified one.
	ch.LoginAs = &mautrix.ReqLogin{
		Type:                     mautrix.AuthTypePassword,
		Identifier:               mautrix.UserIdentifier{Type: mautrix.IdentifierTypeUser, User: cfg.User},
		Password:                 cfg.Password,
		InitialDeviceDisplayName: "ddaybot",
	}
	if err := ch.Init(ctx); err != nil {
		return nil, fmt.Errorf("crypto init: %w", err)
	}
	// Required so SendMessageEvent auto-encrypts in encrypted rooms.
	cli.Crypto = ch

	// Share megolm keys to as-yet unverified devices (the bot does no
	// cross-signing/verification). The defaults already permit this; set them
	// explicitly so a future library default change cannot silently make the bot
	// stop answering in encrypted rooms.
	mach := ch.Machine()
	mach.SendKeysMinTrust = id.TrustStateUnset
	mach.ShareKeysMinTrust = id.TrustStateUnset

	cfg.Log.Info().
		Str("user_id", cli.UserID.String()).
		Str("device_id", cli.DeviceID.String()).
		Msg("logged in, crypto initialised")
	return &mxClient{cli: cli, ch: ch, log: cfg.Log}, nil
}

// newCryptoDB opens a pure-Go modernc.org/sqlite database wrapped in a
// *dbutil.Database. The query params reproduce the pragmas the cgo
// sqlite3-fk-wal driver would set; passing the *dbutil.Database (not a path) to
// cryptohelper keeps the whole thing on the pure-Go, CGO_ENABLED=0 path.
func newCryptoDB(path string, log zerolog.Logger) (*dbutil.Database, error) {
	uri := fmt.Sprintf("file:%s?_txlock=immediate"+
		"&_pragma=foreign_keys(1)"+
		"&_pragma=journal_mode(WAL)"+
		"&_pragma=busy_timeout(5000)"+
		"&_pragma=synchronous(NORMAL)", path)
	db, err := dbutil.NewWithDialect(uri, "sqlite")
	if err != nil {
		return nil, err
	}
	db.Log = dbutil.ZeroLogger(log)
	return db, nil
}

// Close releases the crypto store.
func (m *mxClient) Close() error { return m.ch.Close() }

func (m *mxClient) Self() string { return m.cli.UserID.String() }

func (m *mxClient) SendHTML(ctx context.Context, roomID, plain, html string) error {
	_, err := m.cli.SendMessageEvent(ctx, id.RoomID(roomID), event.EventMessage, &event.MessageEventContent{
		MsgType:       event.MsgText,
		Body:          plain,
		Format:        event.FormatHTML,
		FormattedBody: html,
	})
	return err
}

func (m *mxClient) CreateDM(ctx context.Context, user string) (string, error) {
	resp, err := m.cli.CreateRoom(ctx, &mautrix.ReqCreateRoom{
		Preset:   "trusted_private_chat",
		IsDirect: true,
		Invite:   []id.UserID{id.UserID(user)},
	})
	if err != nil {
		return "", err
	}
	return resp.RoomID.String(), nil
}

func (m *mxClient) JoinRoom(ctx context.Context, roomID string) error {
	_, err := m.cli.JoinRoomByID(ctx, id.RoomID(roomID))
	return err
}

func (m *mxClient) JoinedMembers(ctx context.Context, roomID string) (map[string]struct{}, error) {
	resp, err := m.cli.JoinedMembers(ctx, id.RoomID(roomID))
	if err != nil {
		return nil, err
	}
	out := make(map[string]struct{}, len(resp.Joined))
	for uid := range resp.Joined {
		out[uid.String()] = struct{}{}
	}
	return out, nil
}

const mDirectType = "m.direct"

func (m *mxClient) GetDirect(ctx context.Context) (map[string][]string, error) {
	var raw map[id.UserID][]id.RoomID
	if err := m.cli.GetAccountData(ctx, mDirectType, &raw); err != nil {
		if errors.Is(err, mautrix.MNotFound) {
			return map[string][]string{}, nil // never set yet
		}
		return nil, err
	}
	out := make(map[string][]string, len(raw))
	for user, rooms := range raw {
		list := make([]string, 0, len(rooms))
		for _, r := range rooms {
			list = append(list, r.String())
		}
		out[user.String()] = list
	}
	return out, nil
}

func (m *mxClient) SetDirect(ctx context.Context, direct map[string][]string) error {
	raw := make(map[id.UserID][]id.RoomID, len(direct))
	for user, rooms := range direct {
		list := make([]id.RoomID, 0, len(rooms))
		for _, r := range rooms {
			list = append(list, id.RoomID(r))
		}
		raw[id.UserID(user)] = list
	}
	return m.cli.SetAccountData(ctx, mDirectType, raw)
}

// Serve wires the sync handlers and runs the bot until ctx is cancelled: it
// dispatches decrypted text messages to the logic, auto-joins on invite, and
// runs the announcement loop alongside the sync. It blocks until sync stops.
//
// The crypto store persists the sync next_batch token, so a restart resumes
// where it left off. To avoid reacting to backlog on the very first sync of a
// fresh device, messages older than the moment Serve started are ignored.
func (c *Client) Serve(ctx context.Context, mx *mxClient) error {
	startTS := time.Now().UnixMilli()
	syncer := mx.cli.Syncer.(*mautrix.DefaultSyncer)

	// Auto-join rooms the bot is invited to (megolm keys are shared only to
	// joined members, so joining is a prerequisite to reading/answering).
	syncer.OnEventType(event.StateMember, func(ctx context.Context, evt *event.Event) {
		if evt.GetStateKey() != mx.cli.UserID.String() {
			return
		}
		if evt.Content.AsMember().Membership != event.MembershipInvite {
			return
		}
		mx.log.Info().Str("room", evt.RoomID.String()).Msg("invited, joining")
		if err := mx.JoinRoom(ctx, evt.RoomID.String()); err != nil {
			mx.log.Error().Err(err).Str("room", evt.RoomID.String()).Msg("auto-join failed")
		}
	})

	// Decrypted (or plaintext) text messages: hand to the transport-agnostic
	// command logic. cryptohelper re-dispatches decrypted m.room.encrypted
	// events as m.room.message before this handler runs.
	syncer.OnEventType(event.EventMessage, func(ctx context.Context, evt *event.Event) {
		if evt.Timestamp < startTS {
			return // backlog from before we started; do not re-answer
		}
		if evt.Sender == mx.cli.UserID {
			return
		}
		msg := evt.Content.AsMessage()
		if msg == nil || msg.MsgType != event.MsgText {
			return
		}
		c.HandleMessage(ctx, evt.RoomID.String(), evt.Sender.String(), msg.Body)
	})

	if c.announcementsEnabled() {
		go c.announceLoop(ctx)
	} else {
		mx.log.Info().Msg("registration announcements disabled (no announce room or no feed)")
	}

	mx.log.Info().Str("bot", mx.Self()).Msg("listening for !start")
	err := mx.cli.SyncWithContext(ctx)
	if err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}
