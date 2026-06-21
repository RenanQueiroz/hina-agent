package sandbox

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestTarDirRoundTrip(t *testing.T) {
	src := t.TempDir()
	if err := os.MkdirAll(filepath.Join(src, "sub", "deep"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(src, "auth.json"), "token-abc")
	writeFile(t, filepath.Join(src, "sub", "deep", "config"), "x=1")

	data, err := TarDir(src)
	if err != nil {
		t.Fatalf("TarDir: %v", err)
	}
	dst := filepath.Join(t.TempDir(), "out")
	if err := UntarToDir(data, dst); err != nil {
		t.Fatalf("UntarToDir: %v", err)
	}
	if got := readFile(t, filepath.Join(dst, "auth.json")); got != "token-abc" {
		t.Errorf("auth.json = %q", got)
	}
	if got := readFile(t, filepath.Join(dst, "sub", "deep", "config")); got != "x=1" {
		t.Errorf("config = %q", got)
	}
	// Extracted files must be owner-only (Unix perm bits don't apply on Windows).
	if runtime.GOOS != "windows" {
		info, _ := os.Stat(filepath.Join(dst, "auth.json"))
		if info.Mode().Perm() != 0o600 {
			t.Errorf("extracted file mode = %v, want 0600", info.Mode().Perm())
		}
	}
}

func TestUntarRejectsTraversal(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	_ = tw.WriteHeader(&tar.Header{Name: "../escape", Mode: 0o600, Size: 3, Typeflag: tar.TypeReg})
	_, _ = tw.Write([]byte("bad"))
	_ = tw.Close()

	dst := filepath.Join(t.TempDir(), "out")
	if err := UntarToDir(buf.Bytes(), dst); err == nil {
		t.Fatal("UntarToDir must reject a path-traversal entry")
	}
	// Nothing should have been written outside the target.
	if _, err := os.Stat(filepath.Join(filepath.Dir(dst), "escape")); !os.IsNotExist(err) {
		t.Fatal("traversal entry escaped the target dir")
	}
}

func TestUntarRejectsAbsolutePath(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	_ = tw.WriteHeader(&tar.Header{Name: "/etc/evil", Mode: 0o600, Size: 1, Typeflag: tar.TypeReg})
	_, _ = tw.Write([]byte("x"))
	_ = tw.Close()
	if err := UntarToDir(buf.Bytes(), filepath.Join(t.TempDir(), "out")); err == nil {
		t.Fatal("UntarToDir must reject an absolute entry path")
	}
}

func TestUntarSkipsSymlink(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	_ = tw.WriteHeader(&tar.Header{Name: "link", Linkname: "/etc/passwd", Typeflag: tar.TypeSymlink})
	_ = tw.WriteHeader(&tar.Header{Name: "ok.txt", Mode: 0o600, Size: 2, Typeflag: tar.TypeReg})
	_, _ = tw.Write([]byte("hi"))
	_ = tw.Close()
	dst := filepath.Join(t.TempDir(), "out")
	if err := UntarToDir(buf.Bytes(), dst); err != nil {
		t.Fatalf("untar with symlink: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dst, "link")); !os.IsNotExist(err) {
		t.Error("symlink entry should have been skipped")
	}
	if readFile(t, filepath.Join(dst, "ok.txt")) != "hi" {
		t.Error("regular entry should still extract")
	}
}

func TestTarDirSkipsSymlink(t *testing.T) {
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "real"), "data")
	if err := os.Symlink("/etc/passwd", filepath.Join(src, "sneaky")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	data, err := TarDir(src)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(bytes.NewReader(data))
	for {
		hdr, err := tr.Next()
		if err != nil {
			break
		}
		if hdr.Typeflag == tar.TypeSymlink || hdr.Name == "sneaky" {
			t.Errorf("TarDir must not archive symlinks, found %q", hdr.Name)
		}
	}
}

func TestTarDirEnforcesCaps(t *testing.T) {
	// Entry-count cap.
	t.Run("count", func(t *testing.T) {
		old := maxAgentStateFiles
		maxAgentStateFiles = 2
		defer func() { maxAgentStateFiles = old }()
		dir := t.TempDir()
		for i := 0; i < 3; i++ {
			writeFile(t, filepath.Join(dir, "f"+string(rune('a'+i))), "x")
		}
		if _, err := TarDir(dir); err == nil {
			t.Fatal("TarDir should refuse more files than the count cap")
		}
	})
	// Directory entries count too (a deep tree with no file bytes must still be capped).
	t.Run("dirs", func(t *testing.T) {
		old := maxAgentStateFiles
		maxAgentStateFiles = 2
		defer func() { maxAgentStateFiles = old }()
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, "a", "b", "c"), 0o700); err != nil {
			t.Fatal(err)
		}
		if _, err := TarDir(dir); err == nil {
			t.Fatal("directory entries must count toward the cap")
		}
	})
	// Total-size cap.
	t.Run("total", func(t *testing.T) {
		old := maxAgentStateTotal
		maxAgentStateTotal = 10
		defer func() { maxAgentStateTotal = old }()
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, "big"), "this is more than ten bytes")
		if _, err := TarDir(dir); err == nil {
			t.Fatal("TarDir should refuse a store over the total-size cap")
		}
	})
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
