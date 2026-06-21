package assets

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
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
