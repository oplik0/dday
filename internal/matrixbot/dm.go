// DM resolution and the state cache behind it: finding or creating the 1:1 room
// for a handle, the m.direct account data, and persisting the state to disk.

package matrixbot

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
)

// resolveDM decides which room the private reply for user should go to,
// given the room the "!start" arrived in.
//
//   - If originRoom is itself a 1:1 DM with user (exactly the bot and user are
//     joined), that room is reused and isOrigin is true — the user wrote from
//     their own private chat, so no public nudge is warranted.
//   - Otherwise an existing DM is reused (in-memory cache, then the m.direct
//     account data, so a restart doesn't spawn a duplicate) or a fresh one is
//     created; isOrigin is false.
func (c *Client) resolveDM(ctx context.Context, originRoom, user string) (roomID string, isOrigin bool, err error) {
	if originRoom != "" && c.isDMWith(ctx, originRoom, user) {
		return originRoom, true, nil
	}
	room, err := c.existingOrNewDM(ctx, user)
	if err != nil {
		return "", false, err
	}
	return room, false, nil
}

// isDMWith reports whether roomID is a 1:1 private chat between the bot and
// user: exactly two joined members, the bot and user. Any query error is
// treated as "not a DM" (fail-safe: fall through to normal DM handling).
func (c *Client) isDMWith(ctx context.Context, roomID, user string) bool {
	joined, err := c.ops.JoinedMembers(ctx, roomID)
	if err != nil {
		log.Printf("joined_members %s: %v (treating as non-DM)", roomID, err)
		return false
	}
	if len(joined) != 2 {
		return false
	}
	_, hasBot := joined[c.Self()]
	_, hasUser := joined[user]
	return hasBot && hasUser
}

// existingOrNewDM returns a DM room with user, preferring an existing one over
// handing the user a fresh, empty chat. Lookup order: the in-memory cache, then
// the m.direct account data (which survives a bot restart), and only failing
// both does it create a room. A freshly created room is recorded in the cache
// and appended to m.direct. m.direct maintenance is best-effort — its errors are
// logged, never fatal.
func (c *Client) existingOrNewDM(ctx context.Context, user string) (string, error) {
	c.mu.Lock()
	if room := c.dmRooms[user]; room != "" {
		c.mu.Unlock()
		return room, nil
	}
	c.mu.Unlock()

	if room := c.directRoomFor(ctx, user); room != "" {
		c.cacheDM(user, room)
		return room, nil
	}

	roomID, err := c.ops.CreateDM(ctx, user)
	if err != nil {
		return "", err
	}
	c.cacheDM(user, roomID)
	c.recordDirect(ctx, user, roomID)
	return roomID, nil
}

// cacheDM records room as the DM for user in the in-memory cache and, when a
// CachePath is configured, persists the whole cache to disk. The persist is
// best-effort: an I/O error is logged but never propagated, so a failing disk
// cannot break DM handling. The write happens under c.mu so the on-disk file
// always reflects a consistent snapshot and concurrent writers cannot lose an
// update; the cache is tiny, so holding the lock across the write is cheap.
func (c *Client) cacheDM(user, room string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.dmRooms == nil {
		c.dmRooms = make(map[string]string)
	}
	c.dmRooms[user] = room
	c.persistLocked()
}

// cacheFile is the on-disk shape of the bot state at CachePath: the DM room
// cache plus the id of the last registration announced. It replaced the bare
// handle->roomID map; loadCacheFile still accepts that legacy flat form.
type cacheFile struct {
	DMs           map[string]string `json:"dms"`
	LastAnnounced int               `json:"lastAnnounced"`
}

// persistLocked writes the current state to CachePath. It is best-effort (an
// I/O error is logged, never propagated) and a no-op without a CachePath.
// Callers must hold c.mu — the file always reflects a consistent snapshot.
func (c *Client) persistLocked() {
	if c.CachePath == "" {
		return
	}
	if err := writeCacheFile(c.CachePath, cacheFile{DMs: c.dmRooms, LastAnnounced: c.lastAnnounced}); err != nil {
		log.Printf("persist bot cache to %s: %v", c.CachePath, err)
	}
}

// writeCacheFile atomically writes the state as JSON to path. It writes to a
// temporary file in the same directory and renames it into place, so a crash
// mid-write never leaves a truncated/corrupt cache. Callers hold the lock
// guarding the state, so the snapshot is consistent.
func writeCacheFile(path string, state cacheFile) error {
	if state.DMs == nil {
		state.DMs = map[string]string{}
	}
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".dm_cache-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}

// LoadCache populates the in-memory state (DM rooms and lastAnnounced) from the
// JSON file at CachePath. A missing file is not an error (the bot simply starts
// empty). A present-but-corrupt file is logged and treated as empty rather than
// fatal, so a mangled cache cannot crash-loop the bot. It returns the number of
// DM entries loaded. When CachePath is empty it is a no-op returning 0.
func (c *Client) LoadCache() (int, error) {
	if c.CachePath == "" {
		return 0, nil
	}
	data, err := os.ReadFile(c.CachePath)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	loaded, ok := parseCacheFile(data)
	if !ok {
		log.Printf("bot cache %s is corrupt; starting empty", c.CachePath)
		return 0, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.dmRooms == nil {
		c.dmRooms = make(map[string]string)
	}
	for user, room := range loaded.DMs {
		if user != "" && room != "" {
			c.dmRooms[user] = room
		}
	}
	c.lastAnnounced = loaded.LastAnnounced
	return len(c.dmRooms), nil
}

// parseCacheFile decodes the cache, accepting both the current object form
// ({"dms":{...},"lastAnnounced":N}) and the legacy flat handle->roomID map
// written by older builds. The legacy form loads with lastAnnounced 0, which
// makes the bot re-baseline on the current max id instead of replaying history.
func parseCacheFile(data []byte) (cacheFile, bool) {
	var cur cacheFile
	if err := json.Unmarshal(data, &cur); err == nil && cur.DMs != nil {
		return cur, true
	}
	var legacy map[string]string
	if err := json.Unmarshal(data, &legacy); err == nil {
		return cacheFile{DMs: legacy}, true
	}
	return cacheFile{}, false
}

// directRoomFor returns the first DM room the m.direct account data records for
// user, or "" if none is known. Any read error yields "".
func (c *Client) directRoomFor(ctx context.Context, user string) string {
	if c.Self() == "" {
		return ""
	}
	direct, err := c.ops.GetDirect(ctx)
	if err != nil {
		return ""
	}
	for _, room := range direct[user] {
		if room != "" {
			return room
		}
	}
	return ""
}

// recordDirect appends room to the m.direct account-data entry for user, so the
// DM stays rediscoverable across a bot restart. Best-effort: any error (and the
// case where the bot's MXID is unknown) is logged and swallowed.
func (c *Client) recordDirect(ctx context.Context, user, room string) {
	if c.Self() == "" || room == "" {
		return
	}
	direct, err := c.ops.GetDirect(ctx)
	if err != nil || direct == nil {
		direct = map[string][]string{}
	}
	for _, r := range direct[user] {
		if r == room {
			return // already present, nothing to write
		}
	}
	direct[user] = append(direct[user], room)
	if err := c.ops.SetDirect(ctx, direct); err != nil {
		log.Printf("update m.direct for %s: %v", user, err)
	}
}
