package assets

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/RenanQueiroz/hina-agent/internal/platform"
)

// securePath walks root + rel one component at a time, rejecting a SYMLINK at
// EVERY level (not just the final file) so the asset can't be redirected outside
// the owner-private tree by an intermediate link (e.g. a root/ort symlink into
// attacker-writable storage). Intermediates must be real directories; when
// createDirs is set, missing intermediates are created 0700 (so they can't be
// links). When requireFile is set, the final existing component must be a regular
// file. Returns the validated absolute path. Combined with SecureRoot (the 0700
// root others can't traverse into), this makes every asset a regular file under an
// owner-only tree with no link redirection.
func securePath(root, rel string, createDirs, requireFile bool) (string, error) {
	ri, err := os.Lstat(root)
	if err != nil {
		if createDirs && os.IsNotExist(err) {
			if err := os.MkdirAll(root, 0o700); err != nil {
				return "", err
			}
			ri, err = os.Lstat(root)
		}
		if err != nil {
			return "", err
		}
	}
	if ri.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("%s is a symlink", root)
	}
	if !ri.IsDir() {
		return "", fmt.Errorf("%s is not a directory", root)
	}
	rel = filepath.ToSlash(rel)
	if rel == "." || rel == "" {
		return root, nil // the asset (or its dir) is the root itself, already validated
	}
	cur := root
	parts := strings.Split(rel, "/")
	for i, p := range parts {
		if p == "" || p == "." || p == ".." {
			return "", fmt.Errorf("unsafe path component %q in %q", p, rel)
		}
		cur = filepath.Join(cur, p)
		last := i == len(parts)-1
		info, err := os.Lstat(cur)
		if err != nil {
			if !os.IsNotExist(err) {
				return "", err
			}
			// Missing: create it as a directory when this is a directory path
			// (createDirs and either an intermediate or a non-file target). A missing
			// final FILE (requireFile) or any missing component when not creating is a
			// plain "not found".
			if createDirs && (!last || !requireFile) {
				if err := os.Mkdir(cur, 0o700); err != nil {
					return "", err
				}
				continue
			}
			return "", err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("%s is a symlink", cur)
		}
		if last {
			if requireFile && !info.Mode().IsRegular() {
				return "", fmt.Errorf("%s is not a regular file", cur)
			}
		} else if !info.IsDir() {
			return "", fmt.Errorf("%s is not a directory", cur)
		}
	}
	return cur, nil
}

// SecureRoot makes the asset root owner-private (0700 on Unix / owner-only ACL on
// Windows) and confirms it, so no other local principal can traverse in to swap an
// asset in the verify->load window. It is called by both `hina assets pull` (the
// writer) and the server/doctor (the readers) so the installed tree and the later
// loads share the same trust invariant. Returns an error if Hina can't secure it
// (e.g. the directory is owned by another user).
func SecureRoot(root string) error {
	if err := platform.EnsurePrivateDir(root); err != nil {
		return err
	}
	if safe, err := platform.IsPermissionSafe(root); err != nil {
		return err
	} else if !safe {
		return fmt.Errorf("%s is not owner-private", root)
	}
	return nil
}

// AssetStatus is one asset's on-disk state.
type AssetStatus struct {
	Name     string `json:"name"`
	Path     string `json:"path"`
	Present  bool   `json:"present"`
	Verified bool   `json:"verified"` // checksum/size confirmed (or present, for libraries)
	Reason   string `json:"reason,omitempty"`
}

// Status is the install state of the whole asset set at a root.
type Status struct {
	Root           string        `json:"root"`
	LibDir         string        `json:"lib_dir"`
	OnnxDir        string        `json:"onnx_dir"`
	VoiceDir       string        `json:"voice_dir"`
	ORTUnsupported bool          `json:"ort_unsupported"` // no ORT CPU build for this platform
	Complete       bool          `json:"complete"`        // every asset present + verified
	Assets         []AssetStatus `json:"assets"`
}

// Verify reports the install state at root for the given platform without
// downloading anything.
func Verify(root, goos, goarch string) Status {
	list, ortUnsupported := Manifest(goos, goarch)
	libDir, onnxDir, voiceDir := Layout(root)
	st := Status{Root: root, LibDir: libDir, OnnxDir: onnxDir, VoiceDir: voiceDir, ORTUnsupported: ortUnsupported, Complete: true}
	for _, a := range list {
		as := verifyAsset(root, a)
		if !as.Verified {
			st.Complete = false
		}
		st.Assets = append(st.Assets, as)
	}
	if ortUnsupported {
		st.Complete = false // can't run locally without the runtime
	}
	return st
}

// VerifyLocal verifies for the current platform.
func VerifyLocal(root string) Status { return Verify(root, runtime.GOOS, runtime.GOARCH) }

// ORTVerified reports whether the pinned ONNX Runtime shared LIBRARY is installed
// and matches its checksum on disk. It is the gate to call BEFORE loading the
// library (dlopen executes native code), so a stale/corrupted/swapped lib with
// the expected filename is never loaded. ok=false carries a human reason.
func ORTVerified(root, goos, goarch string) (ok bool, reason string) {
	a, supported := ORTAsset(goos, goarch)
	if !supported {
		return false, "no ONNX Runtime build for this platform"
	}
	st := verifyAsset(root, a)
	if st.Verified {
		return true, ""
	}
	if st.Reason != "" {
		return false, st.Reason
	}
	return false, "not installed"
}

// VerifyVoice re-checks a single preset voice file's checksum on disk (cheap —
// ~290 KB). Used to re-verify an on-demand voice load against the pinned digest,
// closing the gap between startup verification and a later (warm-bundle) load.
func VerifyVoice(root, id string) error {
	target := "voice_styles/" + id + ".json"
	for _, m := range supModels {
		if m.path != target {
			continue
		}
		st := verifyAsset(root, Asset{Name: m.path, SHA256: m.sha256, Size: m.size, Dest: m.dest})
		if st.Verified {
			return nil
		}
		if st.Reason != "" {
			return fmt.Errorf("voice %s: %s", id, st.Reason)
		}
		return fmt.Errorf("voice %s: not installed", id)
	}
	return fmt.Errorf("unknown voice %q", id)
}

// ReadVerified reads a Supertonic asset (identified by its installed Dest path
// relative to the asset root) and returns its bytes ONLY if they match the pinned
// size + SHA256. The bytes that are verified are the bytes that are returned, so a
// caller loads exactly the verified content — closing the verify-then-reopen TOCTOU
// where a concurrent writer could swap the file between the hash and the load.
func ReadVerified(root, destRel string) ([]byte, error) {
	for _, m := range supModels {
		if m.dest != destRel {
			continue
		}
		// Validate every path component no-follow (no symlink at any level) before
		// reading + checksumming the bytes.
		path, err := securePath(root, destRel, false, true)
		if err != nil {
			return nil, err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		if int64(len(data)) != m.size {
			return nil, fmt.Errorf("%s: size %d, want %d", m.path, len(data), m.size)
		}
		sum := sha256.Sum256(data)
		if !strings.EqualFold(hex.EncodeToString(sum[:]), m.sha256) {
			return nil, fmt.Errorf("%s: checksum mismatch", m.path)
		}
		return data, nil
	}
	return nil, fmt.Errorf("unknown asset %q", destRel)
}

// ORTLibPath returns the EXACT installed path of the ORT shared library for a
// platform (root + the manifest's Dest), or "" if there is no build. Pass this to
// onnx.Config.LibFile after ORTVerified so the library that is loaded is exactly
// the one whose checksum was verified.
func ORTLibPath(root, goos, goarch string) string {
	a, ok := ORTAsset(goos, goarch)
	if !ok {
		return ""
	}
	return filepath.Join(root, a.Dest)
}

func verifyAsset(root string, a Asset) AssetStatus {
	dest := filepath.Join(root, a.Dest)
	as := AssetStatus{Name: a.Name, Path: dest}
	// Walk every path component no-follow: the asset must be a REGULAR file under
	// the (owner-private) root with no symlink at ANY level, never a link (or a
	// link's parent) into attacker-writable storage whose target could be swapped
	// after the checksum.
	if _, err := securePath(root, a.Dest, false, true); err != nil {
		if os.IsNotExist(err) {
			as.Reason = "not installed"
		} else {
			as.Reason = err.Error()
		}
		return as
	}
	info, err := os.Stat(dest)
	if err != nil {
		as.Reason = "not installed"
		return as
	}
	as.Present = true
	// The installed file is verified against its pinned digest — for a direct
	// download that's the file itself, for an archive the extracted member — so a
	// later corruption / zero-byte / partial install is reported, not trusted.
	wantSHA, wantSize := a.DiskDigest()
	if wantSize > 0 && info.Size() != wantSize {
		as.Reason = fmt.Sprintf("size %d, want %d", info.Size(), wantSize)
		return as
	}
	if wantSHA != "" {
		sum, err := sha256File(dest)
		if err != nil {
			as.Reason = err.Error()
			return as
		}
		if !strings.EqualFold(sum, wantSHA) {
			as.Reason = "checksum mismatch"
			return as
		}
	}
	as.Verified = true
	return as
}

// Pull downloads and installs every missing or mismatched asset for the platform
// into root, verifying each artifact's SHA256 before installing. Already-valid
// assets are skipped. It returns an error if the platform has no ORT build, or on
// the first download/verify/extract failure.
func Pull(ctx context.Context, root, goos, goarch string, log *slog.Logger) error {
	if log == nil {
		log = slog.Default()
	}
	list, ortUnsupported := Manifest(goos, goarch)
	if ortUnsupported {
		return fmt.Errorf("assets: no ONNX Runtime CPU build is published for %s/%s at %s; local TTS is unavailable on this platform", goos, goarch, ORTVersion)
	}
	for _, a := range list {
		if verifyAsset(root, a).Verified {
			log.Info("asset ok", "name", a.Name)
			continue
		}
		if err := install(ctx, root, a, log); err != nil {
			return fmt.Errorf("assets: %s: %w", a.Name, err)
		}
	}
	return nil
}

// PullLocal pulls for the current platform.
func PullLocal(ctx context.Context, root string, log *slog.Logger) error {
	return Pull(ctx, root, runtime.GOOS, runtime.GOARCH, log)
}

// install downloads one asset to a temp file (verifying SHA256), then writes it to
// its Dest — directly, or by extracting Member from the archive. The install is
// atomic (temp + rename) so a crash mid-download never leaves a half file at Dest.
func install(ctx context.Context, root string, a Asset, log *slog.Logger) error {
	if log == nil {
		log = slog.Default()
	}
	// Create the destination directory chain no-follow (each component a real,
	// owner-private dir — never a symlink into attacker-writable storage), so the
	// download/extract/rename can't be redirected outside the private tree.
	destDir, err := securePath(root, filepath.Dir(a.Dest), true, false)
	if err != nil {
		return err
	}
	dest := filepath.Join(destDir, filepath.Base(a.Dest))
	tmp, err := os.CreateTemp(destDir, ".download-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once renamed away

	log.Info("downloading", "name", a.Name, "url", a.URL, "size", a.Size)
	sum, n, err := download(ctx, a.URL, tmp)
	cerr := tmp.Close()
	if err != nil {
		return err
	}
	if cerr != nil {
		return cerr
	}
	if a.Size > 0 && n != a.Size {
		return fmt.Errorf("downloaded %d bytes, want %d", n, a.Size)
	}
	if !strings.EqualFold(sum, a.SHA256) {
		return fmt.Errorf("checksum mismatch: got %s want %s", sum, a.SHA256)
	}

	if a.Archive == ArchiveNone {
		if err := os.Chmod(tmpName, 0o644); err != nil {
			return err
		}
		return os.Rename(tmpName, dest)
	}
	if err := extractMember(tmpName, a, dest); err != nil {
		return err
	}
	// Verify the extracted library against its pinned digest so a truncated read or
	// a tampered archive member can't install a bad library that onnx.New loads.
	if a.MemberSHA256 != "" {
		sum, err := sha256File(dest)
		if err != nil {
			return err
		}
		if !strings.EqualFold(sum, a.MemberSHA256) {
			_ = os.Remove(dest)
			return fmt.Errorf("extracted member checksum mismatch: got %s want %s", sum, a.MemberSHA256)
		}
	}
	return nil
}

// download streams url into w, returning the artifact's hex SHA256 and byte count.
func download(ctx context.Context, url string, w io.Writer) (string, int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", 0, err
	}
	client := &http.Client{Timeout: 30 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", 0, fmt.Errorf("http %s", resp.Status)
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(w, h), resp.Body)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// extractMember pulls the single Member out of the archive at archivePath and
// writes it to dest (atomically). Only the known member is extracted, so there is
// no zip/tar-slip exposure (dest is fixed, not derived from archive entry names).
func extractMember(archivePath string, a Asset, dest string) error {
	want := strings.TrimPrefix(a.Member, "./")
	open := func() (io.ReadCloser, error) { return archiveMember(archivePath, a.Archive, want) }
	rc, err := open()
	if err != nil {
		return err
	}
	defer rc.Close()

	tmp, err := os.CreateTemp(filepath.Dir(dest), ".extract-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := io.Copy(tmp, rc); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return err
	}
	return os.Rename(tmpName, dest)
}

// archiveMember returns a reader for the named member inside a tar.gz or zip. The
// caller closes it.
func archiveMember(path string, kind ArchiveKind, member string) (io.ReadCloser, error) {
	switch kind {
	case ArchiveTarGz:
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		gz, err := gzip.NewReader(f)
		if err != nil {
			f.Close()
			return nil, err
		}
		tr := tar.NewReader(gz)
		for {
			hdr, err := tr.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				f.Close()
				return nil, err
			}
			if strings.TrimPrefix(hdr.Name, "./") == member {
				return &tarMember{f: f, gz: gz, r: tr}, nil
			}
		}
		f.Close()
		return nil, fmt.Errorf("member %q not found in archive", member)
	case ArchiveZip:
		zr, err := zip.OpenReader(path)
		if err != nil {
			return nil, err
		}
		for _, zf := range zr.File {
			if strings.TrimPrefix(zf.Name, "./") == member {
				rc, err := zf.Open()
				if err != nil {
					zr.Close()
					return nil, err
				}
				return &zipMember{zr: zr, rc: rc}, nil
			}
		}
		zr.Close()
		return nil, fmt.Errorf("member %q not found in archive", member)
	default:
		return nil, errors.New("not an archive")
	}
}

// tarMember keeps the file + gzip readers alive for the tar entry's lifetime.
type tarMember struct {
	f  *os.File
	gz *gzip.Reader
	r  io.Reader
}

func (m *tarMember) Read(p []byte) (int, error) { return m.r.Read(p) }
func (m *tarMember) Close() error {
	gerr := m.gz.Close()
	ferr := m.f.Close()
	return errors.Join(gerr, ferr)
}

type zipMember struct {
	zr *zip.ReadCloser
	rc io.ReadCloser
}

func (m *zipMember) Read(p []byte) (int, error) { return m.rc.Read(p) }
func (m *zipMember) Close() error {
	rerr := m.rc.Close()
	zerr := m.zr.Close()
	return errors.Join(rerr, zerr)
}

func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
