package matrixbot

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// TestCacheRoundTripSkipsServer verifies the persistence contract end to end: a
// Client with a CachePath writes an entry on cacheDM; a fresh Client pointed at
// the same file loads it via LoadCache; and existingOrNewDM then returns the
// recorded room WITHOUT touching the transport (no m.direct read, no CreateDM).
func TestCacheRoundTripSkipsServer(t *testing.T) {
	const user = "@alice:mock"
	const dm = "!persisted:mock"

	path := filepath.Join(t.TempDir(), "dm_cache.json")

	// Writer: persist a mapping to disk.
	w := New(newFakeOps())
	w.CachePath = path
	w.cacheDM(user, dm)

	// The file exists and is valid JSON with our entry.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("cache file not written: %v", err)
	}
	var onDisk cacheFile
	if err := json.Unmarshal(raw, &onDisk); err != nil {
		t.Fatalf("cache file is not valid JSON: %v", err)
	}
	if onDisk.DMs[user] != dm {
		t.Fatalf("on-disk cache = %v; want %s -> %s", onDisk.DMs, user, dm)
	}

	// Reader: brand-new Client, same path. Its fake transport counts every
	// server-touching call — the loaded cache must make all of them unnecessary.
	f := newFakeOps()
	r := New(f)
	r.CachePath = path
	n, err := r.LoadCache()
	if err != nil {
		t.Fatalf("LoadCache: %v", err)
	}
	if n != 1 {
		t.Fatalf("LoadCache loaded %d entries; want 1", n)
	}

	room, err := r.existingOrNewDM(bg(), user)
	if err != nil {
		t.Fatalf("existingOrNewDM: %v", err)
	}
	if room != dm {
		t.Errorf("existingOrNewDM = %q; want cached %q", room, dm)
	}
	if got := f.hitCount(); got != 0 {
		t.Errorf("transport was hit %d time(s); want 0 (cache should short-circuit)", got)
	}
}

// TestLoadCacheMissingFile confirms a missing cache file is not an error: the
// cache simply starts empty.
func TestLoadCacheMissingFile(t *testing.T) {
	c := New(newFakeOps())
	c.CachePath = filepath.Join(t.TempDir(), "does-not-exist.json")

	n, err := c.LoadCache()
	if err != nil {
		t.Fatalf("LoadCache on missing file returned error: %v", err)
	}
	if n != 0 {
		t.Errorf("LoadCache loaded %d entries from missing file; want 0", n)
	}
	if len(c.dmRooms) != 0 {
		t.Errorf("dmRooms = %v; want empty", c.dmRooms)
	}
}

// TestLoadCacheCorruptFile confirms a corrupt cache file does not crash or
// error: LoadCache logs and starts empty.
func TestLoadCacheCorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dm_cache.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("seed corrupt file: %v", err)
	}

	c := New(newFakeOps())
	c.CachePath = path

	n, err := c.LoadCache()
	if err != nil {
		t.Fatalf("LoadCache on corrupt file returned error: %v", err)
	}
	if n != 0 {
		t.Errorf("LoadCache loaded %d entries from corrupt file; want 0", n)
	}
	if len(c.dmRooms) != 0 {
		t.Errorf("dmRooms = %v; want empty after corrupt load", c.dmRooms)
	}
}

// TestCacheDMNoPathNoIO confirms that without a CachePath, cacheDM performs no
// disk I/O — behaviour is unchanged from a purely in-memory cache.
func TestCacheDMNoPathNoIO(t *testing.T) {
	dir := t.TempDir()
	c := New(newFakeOps())
	// CachePath deliberately left empty.
	c.cacheDM("@bob:mock", "!room:mock")

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read temp dir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("cacheDM without CachePath wrote %d file(s); want none", len(entries))
	}
	if c.dmRooms["@bob:mock"] != "!room:mock" {
		t.Errorf("in-memory cache not updated: %v", c.dmRooms)
	}
}

// TestCacheDMConcurrentNoRace exercises concurrent cacheDM writes to the same
// file, so `go test -race` can prove the persist path is data-race free and the
// final on-disk state is consistent JSON.
func TestCacheDMConcurrentNoRace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dm_cache.json")
	c := New(newFakeOps())
	c.CachePath = path

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c.cacheDM(
				"@u"+string(rune('a'+i%26))+":mock",
				"!r"+string(rune('a'+i%26))+":mock",
			)
		}(i)
	}
	wg.Wait()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("cache file: %v", err)
	}
	var m cacheFile
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("final cache is not valid JSON: %v", err)
	}
	if len(m.DMs) == 0 {
		t.Errorf("final cache has no DM entries: %v", m)
	}
}
