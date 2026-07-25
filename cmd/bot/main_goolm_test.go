//go:build goolm

package main

import "testing"

// TestPickleKeyFromEnv: a configured CRYPTO_PICKLE_KEY is returned verbatim, so
// the same key (with the same crypto DB) keeps the bot's device and megolm
// sessions across restarts.
func TestPickleKeyFromEnv(t *testing.T) {
	t.Setenv("CRYPTO_PICKLE_KEY", "  stable-pickle-key  ")
	// Leading/trailing whitespace is trimmed before use.
	if got := string(pickleKey()); got != "stable-pickle-key" {
		t.Errorf("pickleKey = %q; want the trimmed CRYPTO_PICKLE_KEY", got)
	}
}

// TestPickleKeyGeneratedWhenUnset: with no CRYPTO_PICKLE_KEY the bot generates a
// non-empty ephemeral key (and warns). It must not be empty — an empty pickle
// key would leave the olm account unencrypted at rest.
func TestPickleKeyGeneratedWhenUnset(t *testing.T) {
	t.Setenv("CRYPTO_PICKLE_KEY", "")
	k := pickleKey()
	if len(k) == 0 {
		t.Fatal("pickleKey generated an empty key")
	}
	// 32 random bytes hex-encoded => 64 characters.
	if len(k) != 64 {
		t.Errorf("generated key length = %d; want 64 hex chars", len(k))
	}
	// Two generations differ: it is random, not a fixed constant.
	if string(pickleKey()) == string(k) {
		t.Error("two generated pickle keys are identical; expected randomness")
	}
}
