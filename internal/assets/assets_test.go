package assets

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func sha(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func TestManifestPlatform(t *testing.T) {
	if _, ok := ORTAsset("linux", "amd64"); !ok {
		t.Fatal("linux/amd64 should have an ORT build")
	}
	if _, ok := ORTAsset("darwin", "amd64"); ok {
		t.Fatal("darwin/amd64 should have no ORT CPU build at this version")
	}
	if _, ok := ORTAsset("windows", "amd64"); ok != !WindowsLocalVoiceGated {
		t.Fatalf("windows/amd64 ORT availability = %v, but gate is %v", ok, WindowsLocalVoiceGated)
	}
	if _, unsup := Manifest("windows", "amd64"); unsup == !WindowsLocalVoiceGated {
		t.Fatal("windows/amd64 manifest must flag ORT unsupported while gated to Phase 11")
	}
	list, unsupported := Manifest("linux", "amd64")
	if unsupported {
		t.Fatal("linux/amd64 should be supported")
	}
	if want := 1 + len(SupertonicAssets()) + len(NemotronAssets()) + len(VADAssets()); len(list) != want {
		t.Fatalf("manifest has %d assets, want %d (ORT + Supertonic + Nemotron + VAD)", len(list), want)
	}
	if len(VADAssets()) != 1 {
		t.Fatalf("expected exactly one Silero VAD asset, got %d", len(VADAssets()))
	}
	if _, unsup := Manifest("darwin", "amd64"); !unsup {
		t.Fatal("darwin/amd64 manifest should flag ORT unsupported")
	}
	if TotalBytes(list) <= 0 {
		t.Fatal("total bytes should be positive")
	}
}

func TestVerifyAssetChecksum(t *testing.T) {
	root := t.TempDir()
	content := []byte("hello onnx")
	a := Asset{Name: "f", SHA256: sha(content), Size: int64(len(content)), Dest: filepath.Join("supertonic", "x.bin"), Archive: ArchiveNone}

	// Not present yet.
	if verifyAsset(root, a).Verified {
		t.Fatal("should be unverified before install")
	}
	dest := filepath.Join(root, a.Dest)
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dest, content, 0o644); err != nil {
		t.Fatal(err)
	}
	if as := verifyAsset(root, a); !as.Verified {
		t.Fatalf("should verify: %+v", as)
	}
	// Corrupt it -> checksum mismatch.
	if err := os.WriteFile(dest, []byte("tampered!!"), 0o644); err != nil {
		t.Fatal(err)
	}
	if verifyAsset(root, a).Verified {
		t.Fatal("tampered file must not verify")
	}
}

func TestInstallDirectDownload(t *testing.T) {
	content := bytes.Repeat([]byte("abc"), 100)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write(content)
	}))
	defer srv.Close()

	root := t.TempDir()
	a := Asset{Name: "model", URL: srv.URL, SHA256: sha(content), Size: int64(len(content)), Dest: filepath.Join("supertonic", "onnx", "m.onnx"), Archive: ArchiveNone}
	if err := install(context.Background(), root, a, nil); err != nil {
		t.Fatalf("install: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(root, a.Dest))
	if err != nil || !bytes.Equal(got, content) {
		t.Fatalf("installed content mismatch (err=%v)", err)
	}

	// Wrong checksum must fail and not install.
	bad := a
	bad.SHA256 = sha([]byte("different"))
	bad.Dest = filepath.Join("supertonic", "onnx", "bad.onnx")
	if err := install(context.Background(), root, bad, nil); err == nil {
		t.Fatal("expected checksum mismatch error")
	}
	if _, err := os.Stat(filepath.Join(root, bad.Dest)); !os.IsNotExist(err) {
		t.Fatal("failed install must not leave a file at dest")
	}
}

func TestInstallRetriesTransientFailure(t *testing.T) {
	// Shrink the backoff so the test is fast.
	defer swapRetryDelay(time.Millisecond)()

	content := bytes.Repeat([]byte("xyz"), 100)
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Fail the first three attempts with the transient responses a flaky CDN/proxy
		// returns — a 503, a 408 Request Timeout, then a reset mid-body — and only then
		// serve the real content. All three must be RETRIED, not treated as fatal.
		switch atomic.AddInt32(&hits, 1) {
		case 1:
			w.WriteHeader(http.StatusServiceUnavailable)
		case 2:
			w.WriteHeader(http.StatusRequestTimeout) // 408 — a transient 4xx
		case 3:
			w.Header().Set("Content-Length", "100000")
			w.WriteHeader(http.StatusOK)
			w.Write(content[:10]) // short body -> size mismatch / copy error
		default:
			w.Write(content)
		}
	}))
	defer srv.Close()

	root := t.TempDir()
	a := Asset{Name: "model", URL: srv.URL, SHA256: sha(content), Size: int64(len(content)), Dest: filepath.Join("supertonic", "onnx", "m.onnx"), Archive: ArchiveNone}
	if err := install(context.Background(), root, a, nil); err != nil {
		t.Fatalf("install should retry past transient failures: %v", err)
	}
	if got := atomic.LoadInt32(&hits); got < 4 {
		t.Fatalf("expected >= 4 attempts (503 + 408 + short body + success), got %d", got)
	}
	got, err := os.ReadFile(filepath.Join(root, a.Dest))
	if err != nil || !bytes.Equal(got, content) {
		t.Fatalf("installed content mismatch after retry (err=%v)", err)
	}
}

func TestInstallRetries421Misdirected(t *testing.T) {
	defer swapRetryDelay(time.Millisecond)()

	content := bytes.Repeat([]byte("q"), 64)
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// 421 is an HTTP/2 connection-coalescing miss — transient, recovered by a
		// fresh-connection retry, NOT a permanent 4xx.
		if atomic.AddInt32(&hits, 1) == 1 {
			w.WriteHeader(http.StatusMisdirectedRequest)
			return
		}
		w.Write(content)
	}))
	defer srv.Close()

	root := t.TempDir()
	a := Asset{Name: "model", URL: srv.URL, SHA256: sha(content), Size: int64(len(content)), Dest: filepath.Join("supertonic", "onnx", "m.onnx"), Archive: ArchiveNone}
	if err := install(context.Background(), root, a, nil); err != nil {
		t.Fatalf("install should retry a 421 Misdirected Request: %v", err)
	}
	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Fatalf("expected exactly 2 attempts (421 then success), got %d", got)
	}
}

func TestParseRetryAfter(t *testing.T) {
	if d := parseRetryAfter("5"); d != 5*time.Second {
		t.Fatalf("delta-seconds: got %v, want 5s", d)
	}
	if d := parseRetryAfter(""); d != 0 {
		t.Fatalf("empty: got %v, want 0", d)
	}
	if d := parseRetryAfter("garbage"); d != 0 {
		t.Fatalf("garbage: got %v, want 0", d)
	}
	if d := parseRetryAfter("-3"); d != 0 {
		t.Fatalf("negative: got %v, want 0", d)
	}
	// An HTTP-date in the near future yields a positive, bounded delay.
	future := time.Now().Add(30 * time.Second).UTC().Format(http.TimeFormat)
	if d := parseRetryAfter(future); d <= 0 || d > 31*time.Second {
		t.Fatalf("http-date: got %v, want ~30s", d)
	}
	// A past HTTP-date yields 0 (don't wait).
	past := time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat)
	if d := parseRetryAfter(past); d != 0 {
		t.Fatalf("past date: got %v, want 0", d)
	}
}

func TestInstallHonorsRetryAfter(t *testing.T) {
	// Default backoff is ~0 (1ms), so any wait of ~1s proves the server's Retry-After
	// header was honored over the default schedule.
	defer swapRetryDelay(time.Millisecond)()

	content := bytes.Repeat([]byte("r"), 32)
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&hits, 1) == 1 {
			w.Header().Set("Retry-After", "1") // the server asks for 1s
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Write(content)
	}))
	defer srv.Close()

	root := t.TempDir()
	a := Asset{Name: "model", URL: srv.URL, SHA256: sha(content), Size: int64(len(content)), Dest: filepath.Join("supertonic", "onnx", "m.onnx"), Archive: ArchiveNone}
	start := time.Now()
	if err := install(context.Background(), root, a, nil); err != nil {
		t.Fatalf("install should retry a 429: %v", err)
	}
	if d := time.Since(start); d < 900*time.Millisecond {
		t.Fatalf("install should have honored Retry-After: 1 (waited %v, want >= ~1s)", d)
	}
	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Fatalf("expected exactly 2 attempts (429 then success), got %d", got)
	}
}

func TestInstallDoesNotRetryPermanent(t *testing.T) {
	defer swapRetryDelay(time.Millisecond)()

	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusNotFound) // a 4xx — retrying won't help
	}))
	defer srv.Close()

	root := t.TempDir()
	a := Asset{Name: "model", URL: srv.URL, SHA256: sha([]byte("x")), Size: 1, Dest: filepath.Join("supertonic", "onnx", "m.onnx"), Archive: ArchiveNone}
	if err := install(context.Background(), root, a, nil); err == nil {
		t.Fatal("a 404 must fail")
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("a permanent 404 must NOT be retried, got %d hits", got)
	}
	if _, err := os.Stat(filepath.Join(root, a.Dest)); !os.IsNotExist(err) {
		t.Fatal("a failed install must not leave a file at dest")
	}
}

// TestDownloadClientHasBoundedDial guards against silently dropping the default
// dialer: a custom transport with a nil DialContext has NO TCP connect timeout, so
// a blackholed CDN IP would burn the whole client timeout per attempt and defeat the
// retry. The client must keep a bounded dial + an overall timeout.
func TestDownloadClientHasBoundedDial(t *testing.T) {
	tr, ok := downloadClient.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("downloadClient.Transport is %T, want *http.Transport", downloadClient.Transport)
	}
	if tr.DialContext == nil {
		t.Fatal("transport must set DialContext — a nil dialer has no TCP connect timeout")
	}
	if tr.TLSHandshakeTimeout == 0 {
		t.Fatal("transport should bound the TLS handshake")
	}
	if downloadClient.Timeout == 0 {
		t.Fatal("downloadClient should have an overall timeout")
	}
}

// failingWriter simulates a local destination failure (disk full / I/O error) on
// the first write.
type failingWriter struct{ writes int }

func (f *failingWriter) Write(b []byte) (int, error) {
	f.writes++
	return 0, errors.New("simulated disk-full write error")
}

// TestDownloadLocalWriteErrorIsPermanent: a temp-file write failure (disk full) must
// be classified PERMANENT — not retried as a network transient — so install doesn't
// re-download a 600MB model repeatedly against a full disk. download() makes exactly
// one HTTP request; the local write error is wrapped as *permanentError, which
// withRetry stops on after a single attempt (see TestInstallDoesNotRetryPermanent).
func TestDownloadLocalWriteErrorIsPermanent(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Write(bytes.Repeat([]byte("z"), 4096))
	}))
	defer srv.Close()

	fw := &failingWriter{}
	_, _, err := download(context.Background(), srv.URL, fw)
	if err == nil {
		t.Fatal("a local write failure must surface an error")
	}
	var perm *permanentError
	if !errors.As(err, &perm) {
		t.Fatalf("a local write failure must be permanent (no retry), got %T: %v", err, err)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("download must make exactly one request, got %d", got)
	}
}

// swapRetryDelay shrinks the retry backoff for a test and returns a restore func.
func swapRetryDelay(d time.Duration) func() {
	old := retryBaseDelay
	retryBaseDelay = d
	return func() { retryBaseDelay = old }
}

func TestInstallTarGzExtract(t *testing.T) {
	lib := bytes.Repeat([]byte("LIB"), 50)
	member := "onnxruntime-x/lib/libonnxruntime.so.1.26.0"
	tgz := makeTarGz(t, member, lib)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write(tgz)
	}))
	defer srv.Close()

	root := t.TempDir()
	a := Asset{
		Name: "ort", URL: srv.URL, SHA256: sha(tgz), Size: int64(len(tgz)),
		Dest: filepath.Join("ort", "lib", "libonnxruntime.so.1.26.0"), Archive: ArchiveTarGz, Member: member,
		MemberSHA256: sha(lib), MemberSize: int64(len(lib)),
	}
	if err := install(context.Background(), root, a, nil); err != nil {
		t.Fatalf("install: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(root, a.Dest))
	if err != nil || !bytes.Equal(got, lib) {
		t.Fatalf("extracted member mismatch (err=%v)", err)
	}
	// The extracted library is verified against its pinned member digest, not just
	// presence: a tampered file on disk must not verify.
	if as := verifyAsset(root, a); !as.Verified {
		t.Fatalf("extracted library should verify against its member digest: %+v", as)
	}
	if err := os.WriteFile(filepath.Join(root, a.Dest), bytes.Repeat([]byte("X"), len(lib)), 0o644); err != nil {
		t.Fatal(err)
	}
	if verifyAsset(root, a).Verified {
		t.Fatal("a tampered extracted library must not verify")
	}
}

func TestORTVerified(t *testing.T) {
	// No assets installed -> not verified.
	if ok, reason := ORTVerified(t.TempDir(), "linux", "amd64"); ok || reason == "" {
		t.Fatalf("empty root: ok=%v reason=%q, want not-verified + reason", ok, reason)
	}
	// Unsupported platform -> not verified, with a reason.
	if ok, reason := ORTVerified(t.TempDir(), "darwin", "amd64"); ok || reason == "" {
		t.Fatalf("unsupported platform: ok=%v reason=%q", ok, reason)
	}

	// Install a correct ORT lib (matching size+sha) -> verified.
	root := t.TempDir()
	a, _ := ORTAsset("linux", "amd64")
	dest := filepath.Join(root, a.Dest)
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		t.Fatal(err)
	}
	content := bytes.Repeat([]byte("L"), int(a.MemberSize))
	if err := os.WriteFile(dest, content, 0o644); err != nil {
		t.Fatal(err)
	}
	// The real member sha won't match our placeholder, so it should NOT verify
	// (size matches, checksum doesn't) — proving the checksum gate is real.
	if ok, _ := ORTVerified(root, "linux", "amd64"); ok {
		t.Fatal("a size-matching but wrong-checksum lib must not verify")
	}
}

func TestVerifyVoice(t *testing.T) {
	root := t.TempDir()
	// Not installed -> error.
	if err := VerifyVoice(root, "M1"); err == nil {
		t.Fatal("expected error for a missing voice")
	}
	// Unknown id -> error.
	if err := VerifyVoice(root, "ZZ"); err == nil {
		t.Fatal("expected error for an unknown voice id")
	}
	// Present but wrong checksum (right size) -> error.
	var m1 supModel
	for _, m := range supModels {
		if m.path == "voice_styles/M1.json" {
			m1 = m
		}
	}
	dest := filepath.Join(root, m1.dest)
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dest, bytes.Repeat([]byte("x"), int(m1.size)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := VerifyVoice(root, "M1"); err == nil {
		t.Fatal("a size-matching but wrong-checksum voice must fail verification")
	}
}

func TestLayout(t *testing.T) {
	lib, onnx, voice := Layout("/root")
	if lib != filepath.Join("/root", "ort") ||
		onnx != filepath.Join("/root", "supertonic", "onnx") ||
		voice != filepath.Join("/root", "supertonic", "voice_styles") {
		t.Fatalf("layout = %s %s %s", lib, onnx, voice)
	}
	if got := ASRDir("/root"); got != filepath.Join("/root", "nemotron") {
		t.Fatalf("ASRDir = %s", got)
	}
	if got := ASREncoderPath("/root"); got != filepath.Join("/root", "nemotron", "encoder.onnx") {
		t.Fatalf("ASREncoderPath = %s", got)
	}
}

func TestASRVerified(t *testing.T) {
	// Nothing installed -> not verified, with a reason naming the first asset.
	if ok, reason := ASRVerified(t.TempDir()); ok || reason == "" {
		t.Fatalf("empty root: ok=%v reason=%q, want not-verified + reason", ok, reason)
	}
	// A size-matching but wrong-checksum encoder must not verify (checksum gate).
	root := t.TempDir()
	var enc supModel
	for _, m := range nemoModels {
		if m.path == "encoder.onnx" {
			enc = m
		}
	}
	dest := filepath.Join(root, enc.dest)
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		t.Fatal(err)
	}
	// Use a small placeholder (size won't match the real 42 MB) -> size mismatch.
	if err := os.WriteFile(dest, []byte("not the real encoder"), 0o644); err != nil {
		t.Fatal(err)
	}
	if ok, reason := ASRVerified(root); ok || reason == "" {
		t.Fatalf("wrong encoder: ok=%v reason=%q, want not-verified", ok, reason)
	}
}

func TestVADVerified(t *testing.T) {
	// Nothing installed -> not verified, with a reason naming the model.
	if ok, reason := VADVerified(t.TempDir()); ok || reason == "" {
		t.Fatalf("empty root: ok=%v reason=%q, want not-verified + reason", ok, reason)
	}
	// A present-but-wrong-content model must not verify (checksum/size gate).
	root := t.TempDir()
	dest := filepath.Join(root, vadModels[0].dest)
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dest, []byte("not the real silero model"), 0o644); err != nil {
		t.Fatal(err)
	}
	if ok, reason := VADVerified(root); ok || reason == "" {
		t.Fatalf("wrong model: ok=%v reason=%q, want not-verified", ok, reason)
	}
}

func makeTarGz(t *testing.T, name string, content []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
