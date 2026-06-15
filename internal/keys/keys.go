// Package keys mints and hashes gateway API keys. Plaintext keys are shown to the
// operator exactly once at creation; only their SHA-256 hash is ever persisted.
package keys

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
)

// Prefix is the human-recognizable leading marker of every minted key.
const Prefix = "sk-gw-"

// Generate returns a new random API key and a short display prefix (safe to store and
// show). The full key is returned only here and never recoverable afterwards.
func Generate() (key, display string, err error) {
	b := make([]byte, 24)
	if _, err = rand.Read(b); err != nil {
		return "", "", err
	}
	key = Prefix + hex.EncodeToString(b)
	return key, key[:len(Prefix)+8], nil
}

// Hash returns the hex-encoded SHA-256 of a key, used as its lookup identity.
func Hash(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}
