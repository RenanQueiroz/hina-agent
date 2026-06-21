package sandbox

import (
	"archive/tar"
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/RenanQueiroz/hina-agent/internal/platform"
)

// Agent-state archiving packs a callable agent's credential store (CODEX_HOME /
// CLAUDE_CONFIG_DIR / Cursor state) into a single blob for envelope encryption in
// the vault, and unpacks it into an owner-private temp dir to mount into a run.
// Both directions are hardened against a hostile archive: only regular files and
// directories cross the boundary (no symlinks/devices), each entry name is
// sanitized so it can't escape the target dir (tar-slip), and per-file + total
// size are capped so a corrupted/hostile blob can't exhaust the disk.

// Caps on agent-state archives (both write + extract), bounding a corrupted/hostile
// or runaway credential store. Package vars so tests can shrink them.
var (
	maxAgentStateFile  int64 = 25 << 20  // 25 MiB per file
	maxAgentStateTotal int64 = 200 << 20 // 200 MiB total
	maxAgentStateFiles       = 20000     // entry-count cap
)

// TarDir packs every regular file and directory under root into an in-memory tar
// archive with root-relative slash paths. Symlinks, devices, and other special
// files are skipped — a credential store is plain files, and skipping the rest
// avoids ever archiving a symlink that could be abused on extraction.
func TarDir(root string) ([]byte, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	var total int64
	var count int
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			// Path-free: a raw *os.PathError embeds the (untrusted, possibly token-shaped)
			// name, and this error is logged.
			return fmt.Errorf("a credential-store entry could not be read")
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return fmt.Errorf("a credential-store entry has an invalid path")
		}
		if rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		// Bound the archive on the WRITE path too (a buggy/compromised agent CLI could
		// fill its mounted state dir): cap EVERY entry — including directories, so a
		// deep directory tree with no file bytes still can't buffer unbounded tar
		// metadata — plus per-file and total-byte caps.
		if count++; count > maxAgentStateFiles {
			return fmt.Errorf("credential store has too many entries (> %d)", maxAgentStateFiles)
		}
		switch {
		case info.IsDir():
			return tw.WriteHeader(&tar.Header{Name: rel + "/", Mode: 0o700, Typeflag: tar.TypeDir})
		case info.Mode().IsRegular():
			if info.Size() > maxAgentStateFile {
				// Path-free: a credential-store filename is untrusted (a CLI could name a
				// file after a token), and this error is logged — never echo the name.
				return fmt.Errorf("a credential-store file exceeds the size cap")
			}
			if total += info.Size(); total > maxAgentStateTotal {
				return fmt.Errorf("credential store exceeds the total size cap")
			}
			if err := tw.WriteHeader(&tar.Header{Name: rel, Mode: 0o600, Size: info.Size(), Typeflag: tar.TypeReg}); err != nil {
				return fmt.Errorf("could not write a credential-store archive entry")
			}
			// Close each file IMMEDIATELY (not deferred inside the walk callback) so a
			// many-file credential store can't hold thousands of descriptors open at once
			// and hit EMFILE before the entry cap.
			return copyFileToTar(tw, path)
		default:
			return nil // skip symlinks/devices/sockets/etc.
		}
	})
	if err != nil {
		return nil, fmt.Errorf("sandbox: archive agent state: %w", err)
	}
	if err := tw.Close(); err != nil {
		return nil, fmt.Errorf("sandbox: finalize agent-state archive: %w", err)
	}
	return buf.Bytes(), nil
}

// copyFileToTar copies one file's contents into the tar writer and closes it before
// returning, so descriptors are never retained across the walk. LimitReader guards a
// file that grows past its header size between stat and copy.
func copyFileToTar(tw *tar.Writer, path string) error {
	f, err := os.Open(path)
	if err != nil {
		// Path-free: the *os.PathError embeds the untrusted filename, and this is logged.
		return fmt.Errorf("a credential-store file could not be opened")
	}
	_, err = io.Copy(tw, io.LimitReader(f, maxAgentStateFile))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("a credential-store file could not be archived")
	}
	return nil
}

// UntarToDir extracts a TarDir archive into dir (created owner-private). Each entry
// is sanitized: only regular files and directories are created, names that are
// absolute or escape dir via ".." are rejected, and the per-file/total/count caps
// bound resource use.
func UntarToDir(data []byte, dir string) error {
	if err := platform.EnsurePrivateDir(dir); err != nil {
		return err
	}
	tr := tar.NewReader(bytes.NewReader(data))
	var total int64
	var count int
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("sandbox: malformed agent-state archive")
		}
		if count++; count > maxAgentStateFiles {
			return fmt.Errorf("sandbox: agent-state archive has too many entries")
		}
		clean, err := safeJoin(dir, hdr.Name)
		if err != nil {
			return err
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			// Path-free: clean embeds the untrusted entry name, and these errors are logged.
			if err := platform.EnsurePrivateDir(clean); err != nil {
				return fmt.Errorf("sandbox: could not create an agent-state directory")
			}
		case tar.TypeReg:
			if hdr.Size > maxAgentStateFile {
				return fmt.Errorf("sandbox: an agent-state file exceeds the size cap")
			}
			if total += hdr.Size; total > maxAgentStateTotal {
				return fmt.Errorf("sandbox: agent-state archive exceeds the total size cap")
			}
			if err := platform.EnsurePrivateDir(filepath.Dir(clean)); err != nil {
				return fmt.Errorf("sandbox: could not create an agent-state directory")
			}
			if err := writeRegular(clean, tr); err != nil {
				return err
			}
		default:
			// Skip non-regular entries defensively.
		}
	}
	return nil
}

// writeRegular writes one tar entry to path with owner-only perms, bounded by the
// per-file cap (defense in depth against a header lying about Size).
func writeRegular(path string, tr io.Reader) error {
	// Path-free errors: path embeds the untrusted entry name and these are logged.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("sandbox: could not create an agent-state file")
	}
	if _, err := io.Copy(f, io.LimitReader(tr, maxAgentStateFile)); err != nil {
		_ = f.Close()
		return fmt.Errorf("sandbox: could not write an agent-state file")
	}
	// Path-free: a close/fsync failure can surface as an *os.PathError with the untrusted
	// destination name, and this error is logged on unpack.
	if err := f.Close(); err != nil {
		return fmt.Errorf("sandbox: could not finalize an agent-state file")
	}
	return nil
}

// safeJoin joins a slash-separated archive entry name onto base, rejecting an
// absolute name or any path that escapes base via "..".
func safeJoin(base, name string) (string, error) {
	name = strings.TrimSpace(filepath.ToSlash(name))
	if name == "" {
		return "", fmt.Errorf("sandbox: empty archive entry name")
	}
	// Path-free errors: the entry name is untrusted and these surface in logs.
	if filepath.IsAbs(name) || strings.HasPrefix(name, "/") {
		return "", fmt.Errorf("sandbox: absolute archive entry rejected")
	}
	clean := filepath.Clean(filepath.Join(base, filepath.FromSlash(name)))
	rel, err := filepath.Rel(base, clean)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("sandbox: archive entry escapes the target dir")
	}
	return clean, nil
}
