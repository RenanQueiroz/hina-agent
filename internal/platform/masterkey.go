package platform

import (
	"crypto/rand"
	"fmt"
	"os"
)

// MasterKeyLen is the length of the secret-vault master key.
const MasterKeyLen = 32

// LoadOrCreateMasterKey returns the local secret-vault master key, creating a
// new random key at path if none exists.
//
// On Unix the permission check fails closed: an unsafe (group/world-readable)
// key file is rejected. On Windows, DPAPI/Credential-Manager protection and the
// ACL check are built and validated in the Windows hardening phase; until then
// the key is stored as a private file via EnsurePrivateFile.
func LoadOrCreateMasterKey(path string) ([]byte, error) {
	if b, err := os.ReadFile(path); err == nil {
		safe, perr := IsPermissionSafe(path)
		if perr != nil {
			return nil, perr
		}
		if !safe {
			return nil, fmt.Errorf("master key %s has unsafe permissions; refusing to use it", path)
		}
		if len(b) != MasterKeyLen {
			return nil, fmt.Errorf("master key %s has unexpected length %d (want %d)", path, len(b), MasterKeyLen)
		}
		return b, nil
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("read master key: %w", err)
	}

	key := make([]byte, MasterKeyLen)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("generate master key: %w", err)
	}
	if err := os.WriteFile(path, key, 0o600); err != nil {
		return nil, fmt.Errorf("write master key: %w", err)
	}
	if err := secureFile(path); err != nil {
		return nil, fmt.Errorf("secure master key: %w", err)
	}
	return key, nil
}
