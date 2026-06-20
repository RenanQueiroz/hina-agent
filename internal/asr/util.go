package asr

import "os"

// fileExists reports whether p is an existing non-directory file.
func fileExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && !info.IsDir()
}
