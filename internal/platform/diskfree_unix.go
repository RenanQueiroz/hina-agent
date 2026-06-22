//go:build !windows

package platform

import "syscall"

// FreeBytes returns the bytes available to an unprivileged writer on the filesystem
// backing path. Because it reflects the FILESYSTEM's allocated/free blocks (via statfs),
// it accounts for ALL usage — including blocks held by open-but-unlinked files that a
// directory walk can't see — making it the authoritative signal for the run scratch
// disk watchdog (a process that opens, unlinks, and keeps writing a file still shows up
// as a drop in free space here).
func FreeBytes(path string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	// Bavail = blocks free to an unprivileged user; Bsize = fundamental block size.
	// uint64 conversions keep this portable across Linux (int64 Bsize) and Darwin (uint32).
	return int64(uint64(st.Bavail) * uint64(st.Bsize)), nil
}
