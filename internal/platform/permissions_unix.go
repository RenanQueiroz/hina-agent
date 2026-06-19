//go:build !windows

package platform

import (
	"fmt"
	"os"
)

func secureDir(path string) error  { return os.Chmod(path, 0o700) }
func secureFile(path string) error { return os.Chmod(path, 0o600) }

func isPermissionSafe(path string) (bool, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return false, fmt.Errorf("stat %s: %w", path, err)
	}
	// Unsafe if any group/other permission bit is set.
	return fi.Mode().Perm()&0o077 == 0, nil
}
