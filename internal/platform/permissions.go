package platform

import (
	"fmt"
	"os"
)

// EnsurePrivateDir creates dir (and parents) and restricts it to the current
// user — 0700 on Unix; owner-only ACL on Windows (see permissions_windows.go).
func EnsurePrivateDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create dir %s: %w", dir, err)
	}
	return secureDir(dir)
}

// EnsurePrivateFile ensures the file exists and is restricted to the current
// user — 0600 on Unix; owner-only ACL on Windows.
func EnsurePrivateFile(path string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create file %s: %w", path, err)
	}
	_ = f.Close()
	return secureFile(path)
}

// IsPermissionSafe reports whether path is inaccessible to group/other. On Unix
// this is a real check that callers (e.g. the master key) use to fail closed; on
// Windows it is a placeholder pending the Windows hardening ACL work.
func IsPermissionSafe(path string) (bool, error) {
	return isPermissionSafe(path)
}
