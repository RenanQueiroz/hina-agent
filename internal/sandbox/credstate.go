package sandbox

import (
	"bytes"
	"fmt"
)

// Agent-state blobs are self-describing: a small magic header tags whether the
// (encrypted) payload is a raw credential value (an API key / OAuth token) or a tar
// of a browser/subscription credential store. The run path cross-checks this tag
// against the profile's auth type before using the blob, so a profile row that has
// drifted from its blob (e.g. a partial write) FAILS CLOSED — a tar is never
// mis-injected as an env-var value, and a key is never untarred — instead of leaking
// the wrong material into the wrong place.

var credMagic = []byte("HINAAST1")

// Credential-state kinds (the byte after the magic header).
const (
	CredKindKey byte = 'k' // a raw credential value (API key / OAuth token)
	CredKindTar byte = 't' // a tar of a credential-store directory (browser_state)
)

// EncodeCredState frames a payload with its kind for storage in the vault.
func EncodeCredState(kind byte, data []byte) []byte {
	out := make([]byte, 0, len(credMagic)+1+len(data))
	out = append(out, credMagic...)
	out = append(out, kind)
	return append(out, data...)
}

// DecodeCredState parses a framed agent-state blob, returning its kind and payload.
// A blob without the magic header is rejected (it can't be safely interpreted).
func DecodeCredState(blob []byte) (kind byte, data []byte, err error) {
	if len(blob) < len(credMagic)+1 || !bytes.Equal(blob[:len(credMagic)], credMagic) {
		return 0, nil, fmt.Errorf("sandbox: unrecognized agent-state blob")
	}
	return blob[len(credMagic)], blob[len(credMagic)+1:], nil
}
