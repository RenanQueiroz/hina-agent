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
	_, _ = rand.Read(b[:])
	return prefix + "_" + hex.EncodeToString(b[:])
}

// Token returns a random URL-safe secret of n bytes, hex-encoded (for session
// tokens and similar bearer secrets). Callers store only its hash.
func Token(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
