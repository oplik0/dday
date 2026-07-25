package matrixbot

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"sync"
	"testing"

	"filippo.io/age"
	"filippo.io/age/agessh"
	"golang.org/x/crypto/ssh"
)

// botSelf is the bot's MXID the fake transport reports.
const botSelf = "@ddaybot:mock"

// recordedSend is one message the fake transport was asked to deliver.
type recordedSend struct {
	room  string
	plain string
	html  string
}

// fakeOps is an in-memory matrixOps used by every unit test in place of a live
// homeserver. It records the sends and DM creations the logic drives and serves
// configurable membership and m.direct data, so the assertions the old
// httptest-based tests made (is_direct DM creation, message/nudge bodies, the
// absence of a ?t= link, DM reuse, m.direct lookups, …) carry over unchanged —
// only the injection mechanism differs.
type fakeOps struct {
	self string

	mu      sync.Mutex
	sends   []recordedSend
	creates []string // users CreateDM was invited for (one per DM created)
	joins   []string
	direct  map[string][]string

	// members maps a room id to its joined-member set; a room absent here uses
	// membersDefault, a >2-member room so it is never mistaken for a 1:1 DM.
	members        map[string]map[string]struct{}
	membersDefault map[string]struct{}

	dmRoomID  string // room id CreateDM returns (default "!dm:mock")
	createErr error
	sendErr   error

	// hits counts every server-touching call; cache tests assert it stays 0 to
	// prove the on-disk cache short-circuits the network.
	hits int

	// sendCh mirrors sends for tests that must block until a message arrives
	// (the announce loop). Best-effort: a full buffer drops the signal, never
	// blocking the caller.
	sendCh chan recordedSend
}

func newFakeOps() *fakeOps {
	return &fakeOps{
		self:    botSelf,
		direct:  map[string][]string{},
		members: map[string]map[string]struct{}{},
		membersDefault: map[string]struct{}{
			botSelf:         {},
			"@someone:mock": {},
			"@other:mock":   {},
		},
		dmRoomID: "!dm:mock",
		sendCh:   make(chan recordedSend, 128),
	}
}

func (f *fakeOps) Self() string { return f.self }

func (f *fakeOps) SendHTML(_ context.Context, room, plain, html string) error {
	f.mu.Lock()
	if f.sendErr != nil {
		f.mu.Unlock()
		return f.sendErr
	}
	rec := recordedSend{room: room, plain: plain, html: html}
	f.sends = append(f.sends, rec)
	f.mu.Unlock()
	select {
	case f.sendCh <- rec:
	default:
	}
	return nil
}

func (f *fakeOps) CreateDM(_ context.Context, user string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hits++
	if f.createErr != nil {
		return "", f.createErr
	}
	f.creates = append(f.creates, user)
	return f.dmRoomID, nil
}

func (f *fakeOps) JoinRoom(_ context.Context, room string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.joins = append(f.joins, room)
	return nil
}

func (f *fakeOps) JoinedMembers(_ context.Context, room string) (map[string]struct{}, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hits++
	if m, ok := f.members[room]; ok {
		return m, nil
	}
	return f.membersDefault, nil
}

func (f *fakeOps) GetDirect(context.Context) (map[string][]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hits++
	out := map[string][]string{}
	for k, v := range f.direct {
		out[k] = append([]string(nil), v...)
	}
	return out, nil
}

func (f *fakeOps) SetDirect(_ context.Context, d map[string][]string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hits++
	f.direct = map[string][]string{}
	for k, v := range d {
		f.direct[k] = append([]string(nil), v...)
	}
	return nil
}

// setMembers marks room as a specific joined-member set (e.g. a 1:1 DM).
func (f *fakeOps) setMembers(room string, users ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	set := make(map[string]struct{}, len(users))
	for _, u := range users {
		set[u] = struct{}{}
	}
	f.members[room] = set
}

func (f *fakeOps) sentMessages() []recordedSend {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recordedSend(nil), f.sends...)
}

func (f *fakeOps) sendCount() int   { f.mu.Lock(); defer f.mu.Unlock(); return len(f.sends) }
func (f *fakeOps) createCount() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.creates) }
func (f *fakeOps) hitCount() int    { f.mu.Lock(); defer f.mu.Unlock(); return f.hits }

// bg is a short alias for the background context used throughout the tests.
func bg() context.Context { return context.Background() }

// genKeypair creates an in-memory SSH ed25519 keypair and returns the matching
// age recipient (from the public key) and identity (from the private key),
// mirroring the on-disk config/*_ed25519[.pub] pair used in production.
func genKeypair(t *testing.T) (age.Recipient, age.Identity) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	block, err := ssh.MarshalPrivateKey(priv, "dday-test")
	if err != nil {
		t.Fatalf("marshal private key: %v", err)
	}
	id, err := agessh.ParseIdentity(pem.EncodeToMemory(block))
	if err != nil {
		t.Fatalf("parse identity: %v", err)
	}

	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatalf("new public key: %v", err)
	}
	r, err := agessh.ParseRecipient(string(ssh.MarshalAuthorizedKey(sshPub)))
	if err != nil {
		t.Fatalf("parse recipient: %v", err)
	}
	return r, id
}
