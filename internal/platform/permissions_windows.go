//go:build windows

package platform

// On Windows, os.Chmod only toggles the read-only bit, so owner-only ACL
// tightening is a deliberate no-op for now. Real ACL restriction (and the
// DPAPI-backed master key) is built and validated in the Windows hardening
// phase; the control plane compiles and runs without it.
func secureDir(_ string) error  { return nil }
func secureFile(_ string) error { return nil }

func isPermissionSafe(_ string) (bool, error) {
	// TODO(windows-hardening): real ACL check. Assume safe until validated.
	return true, nil
}
