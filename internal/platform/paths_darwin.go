//go:build darwin

package platform

import "os"

// dataBase returns the base directory for application data on macOS: the
// OS-standard ~/Library/Application Support (the same base macOS apps use for
// durable application state). os.UserConfigDir resolves to it, so config and
// data share the Application Support base, each under the "hina" app dir. Phase
// 1 locks this so SQLite and master-key material never need migrating off a
// nonstandard location later.
func dataBase() (string, error) {
	return os.UserConfigDir()
}
