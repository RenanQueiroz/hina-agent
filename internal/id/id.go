// Package id generates random, prefixed, URL-safe identifiers.
package id

import (
	"crypto/rand"
	"encoding/hex"
)

// New returns a random 128-bit identifier with a typed prefix, e.g.
// New("usr") -> "usr_3f2a...". The prefix makes ids self-describing in logs.
func New(prefix string) string {
	var b [16]byte
	mustRandom(b[:])
	return prefix + "_" + hex.EncodeToString(b[:])
}

// Token returns a random URL-safe secret of n bytes, hex-encoded (for session
// tokens and similar bearer secrets). Callers store only its hash.
func Token(n int) string {
	b := make([]byte, n)
	mustRandom(b)
	return hex.EncodeToString(b)
}

// mustRandom fills b with cryptographically secure random bytes, panicking on
// any failure. These bytes back session cookies and the one-time bootstrap
// admin password, so an OS entropy failure or short read must fail closed —
// never silently issue a predictable or repeated secret. crypto/rand.Read fills
// the whole buffer when it returns a nil error, so checking err is sufficient.
func mustRandom(b []byte) {
	if _, err := rand.Read(b); err != nil {
		panic("id: crypto/rand failed: " + err.Error())
	}
}
