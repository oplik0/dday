// The Matrix operations the bot logic needs, behind an interface so the command
// logic (HandleRegister, resolveDM, announce, nudge, …) is unit-testable with a
// fake and stays completely independent of the concrete Matrix transport.
//
// The production implementation is mxClient (mautrix.go, built with -tags
// goolm): a thin adapter over maunium.net/go/mautrix + cryptohelper that speaks
// the real (optionally end-to-end encrypted) client-server API. Tests inject an
// in-memory fake instead of standing up a homeserver.

package matrixbot

import "context"

// matrixOps is the minimal set of Matrix operations the bot logic drives. All
// user/room ids are plain strings ("@user:hs", "!room:hs") so the logic never
// depends on mautrix's id types; the adapter converts at the boundary.
type matrixOps interface {
	// Self returns the bot's own full MXID ("@ddaybot:hs").
	Self() string

	// SendHTML posts an m.text message with an HTML formatted body to roomID.
	// In an encrypted room the adapter transparently encrypts it (megolm); in a
	// plaintext room it is sent as-is. plain is the fallback body, html the
	// org.matrix.custom.html formatted_body.
	SendHTML(ctx context.Context, roomID, plain, html string) error

	// CreateDM creates a private 1:1 direct-message room inviting user
	// (is_direct, trusted_private_chat) and returns its room id. The room is
	// left unencrypted, matching the historical behaviour — the bot answers in
	// whatever encryption state the room the user wrote from already has.
	CreateDM(ctx context.Context, user string) (roomID string, err error)

	// JoinRoom joins roomID (used to accept an invite).
	JoinRoom(ctx context.Context, roomID string) error

	// JoinedMembers returns the set of joined member MXIDs in roomID.
	JoinedMembers(ctx context.Context, roomID string) (map[string]struct{}, error)

	// GetDirect reads the bot's m.direct account data (user MXID -> DM room
	// ids). A never-set value yields an empty map and no error.
	GetDirect(ctx context.Context) (map[string][]string, error)

	// SetDirect writes the bot's m.direct account data.
	SetDirect(ctx context.Context, direct map[string][]string) error
}
