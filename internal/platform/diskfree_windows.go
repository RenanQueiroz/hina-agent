//go:build windows

package platform

// FreeBytes is not implemented on Windows yet (the automation disk watchdog's free-space
// guard is a Linux/host-server concern; Windows validation is Phase 12). Returning a large
// sentinel makes the free-space guard a no-op there, leaving the directory-walk per-run
// cap as the only disk check on Windows until Phase 12 wires a real query.
func FreeBytes(string) (int64, error) { return 1 << 62, nil }
