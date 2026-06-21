package sandbox

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// fakeOpts controls the behavior baked into a fake `sbx` shim.
type fakeOpts struct {
	versionLine string // default "sbx version 0.33.0"
	stdout      string
	stderr      string
	echoEnv     string // if set, echo $<name> to stdout (to prove env passthrough)
	exit        int
	sleep       string // seconds, e.g. "2" (to exercise the timeout path)
}

// fakeSbx writes an executable shell shim that mimics the `sbx` surface our
// runner uses, logging every invocation's argv to a file so tests can assert the
// constructed command line. Unix-only (a shell script); callers skip on Windows.
func fakeSbx(t *testing.T, o fakeOpts) (path, argvLog string) {
	t.Helper()
	dir := t.TempDir()
	argvLog = filepath.Join(dir, "argv.log")
	path = filepath.Join(dir, "sbx")
	version := o.versionLine
	if version == "" {
		version = "sbx version 0.33.0"
	}
	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	b.WriteString("echo \"$@\" >> " + shQuote(argvLog) + "\n")
	fmt.Fprintf(&b, "if [ \"$1\" = \"--version\" ]; then echo %s; exit 0; fi\n", shQuote(version))
	if o.sleep != "" {
		b.WriteString("sleep " + o.sleep + "\n")
	}
	if o.stdout != "" {
		b.WriteString("printf %s " + shQuote(o.stdout) + "\n")
	}
	if o.echoEnv != "" {
		// Echo the named env var's value (proves a secret reached the process env
		// without appearing on the argv).
		fmt.Fprintf(&b, "printf '%%s' \"$%s\"\n", o.echoEnv)
	}
	if o.stderr != "" {
		b.WriteString("printf %s " + shQuote(o.stderr) + " 1>&2\n")
	}
	fmt.Fprintf(&b, "exit %d\n", o.exit)
	if err := os.WriteFile(path, []byte(b.String()), 0o755); err != nil {
		t.Fatalf("write shim: %v", err)
	}
	return path, argvLog
}

func shQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// testRedactor is a minimal OutputRedactor for the runner tests (keeps this file
// independent of internal/vault).
type testRedactor struct{ vals []string }

func newTestRedactor(vals ...string) *testRedactor { return &testRedactor{vals: vals} }
func (r *testRedactor) RedactBytes(b []byte) []byte {
	s := string(b)
	for _, v := range r.vals {
		s = strings.ReplaceAll(s, v, "[redacted]")
	}
	return []byte(s)
}
func (r *testRedactor) MaxValueLen() int {
	max := 0
	for _, v := range r.vals {
		if len(v) > max {
			max = len(v)
		}
	}
	return max
}

func skipOnWindows(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake sbx shim is a POSIX shell script; real sbx on Windows is Phase 12")
	}
}

func TestBuildRunArgs(t *testing.T) {
	r := &CLIRunner{kit: "mykit", defaults: Limits{CPUs: "1", Memory: "1g"}}
	spec := RunSpec{
		Tool:      ToolShell,
		Argv:      []string{"echo", "hello world"},
		Workspace: "/host/ws",
		Env:       []string{"FOO=bar"},
		Mounts:    []Mount{{Host: "/host/ro", Container: "/data", ReadOnly: true}},
		Limits:    Limits{CPUs: "2", Memory: "2g", PIDs: 256},
	}
	args := r.buildRunArgs("sbx_abc", spec)
	joined := strings.Join(args, " ")

	for _, want := range []string{
		"run --name sbx_abc",
		"--kit mykit",
		"--cpus 2",
		"-m 2g",
		"--pids-limit 256",
		"/host/ws:/workspace",
		"/host/ro:/data:ro",
		"--workdir /workspace",
		"--env FOO=bar",
		"-- echo hello world",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("args missing %q\nfull: %s", want, joined)
		}
	}
	// The spec limits override the defaults.
	if strings.Contains(joined, "--cpus 1") {
		t.Fatalf("spec cpus should override default: %s", joined)
	}
}

func TestBuildRunArgsClone(t *testing.T) {
	r := &CLIRunner{}
	args := r.buildRunArgs("sbx_x", RunSpec{Argv: []string{"git", "status"}, Workspace: "/repo", Clone: true})
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--clone") || !strings.Contains(joined, "/repo:/workspace:ro") {
		t.Fatalf("clone mount should be read-only: %s", joined)
	}
}

func TestRunHappyPath(t *testing.T) {
	skipOnWindows(t)
	path, argvLog := fakeSbx(t, fakeOpts{stdout: "hi from sandbox", exit: 0})
	r := NewCLIRunner(Config{Path: path, OutputDir: t.TempDir()})
	if !r.Available() {
		t.Fatalf("runner unavailable: %s", r.Status().Reason)
	}
	res, err := r.Run(context.Background(), RunSpec{Tool: ToolShell, Argv: []string{"echo", "hi"}, Workspace: t.TempDir()})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Err != nil {
		t.Fatalf("run result err: %v", res.Err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit = %d, want 0", res.ExitCode)
	}
	if res.Stdout != "hi from sandbox" {
		t.Fatalf("stdout = %q", res.Stdout)
	}
	// Output was captured to a file.
	if b, err := os.ReadFile(res.StdoutPath); err != nil || string(b) != "hi from sandbox" {
		t.Fatalf("stdout file = %q err=%v", b, err)
	}
	// The argv log records a `run --name <id> -- echo hi`.
	log, _ := os.ReadFile(argvLog)
	if !strings.Contains(string(log), "run --name "+res.SandboxID) || !strings.Contains(string(log), "-- echo hi") {
		t.Fatalf("argv log = %q", log)
	}
}

func TestRunNonZeroExit(t *testing.T) {
	skipOnWindows(t)
	path, _ := fakeSbx(t, fakeOpts{stderr: "boom", exit: 3})
	r := NewCLIRunner(Config{Path: path, OutputDir: t.TempDir()})
	res, err := r.Run(context.Background(), RunSpec{Tool: ToolShell, Argv: []string{"false"}})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	// A non-zero command exit is reported via ExitCode, not Err.
	if res.Err != nil {
		t.Fatalf("non-zero exit should not be an execution error: %v", res.Err)
	}
	if res.ExitCode != 3 {
		t.Fatalf("exit = %d, want 3", res.ExitCode)
	}
	if res.Stderr != "boom" {
		t.Fatalf("stderr = %q", res.Stderr)
	}
}

func TestRunSecretEnvNotInArgvAndRedacted(t *testing.T) {
	skipOnWindows(t)
	// The shim echoes $API_KEY to stdout — proving the value reached the process
	// env without appearing on the argv; the redactor then scrubs it from output.
	path, argvLog := fakeSbx(t, fakeOpts{echoEnv: "API_KEY"})
	dir := t.TempDir()
	r := NewCLIRunner(Config{Path: path, OutputDir: dir})
	red := newTestRedactor("topsecret")
	res, err := r.Run(context.Background(), RunSpec{
		Tool:      ToolShell,
		Argv:      []string{"printenv", "API_KEY"},
		SecretEnv: []string{"API_KEY=topsecret"},
		Redactor:  red,
	})
	if err != nil || res.Err != nil {
		t.Fatalf("run: err=%v res.Err=%v", err, res.Err)
	}
	// 1. The secret value is NEVER on the constructed argv (only `--env API_KEY`).
	log, _ := os.ReadFile(argvLog)
	if strings.Contains(string(log), "topsecret") {
		t.Fatalf("secret value leaked onto the argv: %q", log)
	}
	if !strings.Contains(string(log), "--env API_KEY") {
		t.Fatalf("secret env name not forwarded: %q", log)
	}
	// 2. The value reached the process env (shim echoed it) but is redacted in the
	//    model-visible result...
	if strings.Contains(res.Stdout, "topsecret") {
		t.Fatalf("secret leaked into inline output: %q", res.Stdout)
	}
	if !strings.Contains(res.Stdout, "[redacted]") {
		t.Fatalf("expected redaction marker in output: %q", res.Stdout)
	}
	// 3. ...and the captured file on disk holds only redacted bytes.
	blob, _ := os.ReadFile(res.StdoutPath)
	if strings.Contains(string(blob), "topsecret") {
		t.Fatalf("secret persisted unredacted in the capture file: %q", blob)
	}
}

func TestRunTimeout(t *testing.T) {
	skipOnWindows(t)
	path, _ := fakeSbx(t, fakeOpts{sleep: "10"})
	r := NewCLIRunner(Config{Path: path, OutputDir: t.TempDir()})
	start := time.Now()
	res, err := r.Run(context.Background(), RunSpec{
		Tool:   ToolShell,
		Argv:   []string{"sleep", "10"},
		Limits: Limits{Timeout: 200 * time.Millisecond},
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !res.TimedOut {
		t.Fatal("expected TimedOut")
	}
	if res.Err == nil {
		t.Fatal("timeout should set an execution error")
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("KillTree did not reap the sleeping process promptly (took %v)", time.Since(start))
	}
}

func TestRunDropsDangerousSecretEnv(t *testing.T) {
	skipOnWindows(t)
	path, argvLog := fakeSbx(t, fakeOpts{})
	r := NewCLIRunner(Config{Path: path, OutputDir: t.TempDir()})
	if _, err := r.Run(context.Background(), RunSpec{
		Tool: ToolShell, Argv: []string{"echo"},
		SecretEnv: []string{"LD_PRELOAD=/evil.so", "DOCKER_HOST=tcp://evil", "API_KEY=ok"},
	}); err != nil {
		t.Fatalf("run: %v", err)
	}
	log, _ := os.ReadFile(argvLog)
	if strings.Contains(string(log), "LD_PRELOAD") || strings.Contains(string(log), "DOCKER_HOST") {
		t.Fatalf("a host-interpreted secret env name reached the sbx invocation: %q", log)
	}
	if !strings.Contains(string(log), "--env API_KEY") {
		t.Fatalf("a safe secret env name should still be forwarded: %q", log)
	}
}

func TestRunRedactsSecretAcrossCaptureCap(t *testing.T) {
	skipOnWindows(t)
	old := fileCaptureCap
	fileCaptureCap = 16
	defer func() { fileCaptureCap = old }()

	secret := "SECRETVALUE" // 11 bytes
	// 12 bytes of padding then the secret: the secret straddles the 16-byte cap, so
	// the retained buffer ends with a partial-secret prefix ("SECR") that exact-match
	// redaction can't catch. The trailing-margin drop must remove it.
	path, _ := fakeSbx(t, fakeOpts{stdout: strings.Repeat("x", 12) + secret})
	r := NewCLIRunner(Config{Path: path, OutputDir: t.TempDir()})
	res, err := r.Run(context.Background(), RunSpec{Tool: ToolShell, Argv: []string{"echo"}, Redactor: newTestRedactor(secret)})
	if err != nil || res.Err != nil {
		t.Fatalf("run: err=%v res.Err=%v", err, res.Err)
	}
	blob, _ := os.ReadFile(res.StdoutPath)
	if strings.Contains(string(blob), "SECR") {
		t.Fatalf("a partial secret survived at the cap boundary: %q", blob)
	}
}

func TestRunCaptureFailureRecorded(t *testing.T) {
	skipOnWindows(t)
	path, _ := fakeSbx(t, fakeOpts{stdout: "ran", exit: 0})
	// Make the capture dir un-creatable by nesting it under a regular file, so
	// writeCapture fails AFTER the command has already run.
	blocker := filepath.Join(t.TempDir(), "afile")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := NewCLIRunner(Config{Path: path, OutputDir: filepath.Join(blocker, "out")})
	res, err := r.Run(context.Background(), RunSpec{Tool: ToolShell, Argv: []string{"echo"}})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.CaptureErr == "" {
		t.Fatal("a failed capture write must be recorded, not silently dropped")
	}
	if res.ExitCode != 0 {
		t.Fatalf("the command itself still ran: exit = %d", res.ExitCode)
	}
}

func TestRunUnavailable(t *testing.T) {
	r := NewCLIRunner(Config{Path: filepath.Join(t.TempDir(), "does-not-exist")})
	if r.Available() {
		t.Fatal("runner should be unavailable when sbx is missing")
	}
	res, err := r.Run(context.Background(), RunSpec{Tool: ToolShell, Argv: []string{"echo"}})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Err == nil {
		t.Fatal("run on an unavailable runner must return an execution error")
	}
}

func TestSmoke(t *testing.T) {
	skipOnWindows(t)
	path, _ := fakeSbx(t, fakeOpts{exit: 0})
	r := NewCLIRunner(Config{Path: path, OutputDir: t.TempDir()})
	if err := r.Smoke(context.Background()); err != nil {
		t.Fatalf("smoke: %v", err)
	}
}

func TestSmokeCreatesMissingOutputDir(t *testing.T) {
	skipOnWindows(t)
	// A fresh install only creates the runtime root, not this child capture dir.
	// Smoke must create it rather than fail-closed a valid sbx.
	path, _ := fakeSbx(t, fakeOpts{exit: 0})
	missing := filepath.Join(t.TempDir(), "run", "sandbox-output")
	r := NewCLIRunner(Config{Path: path, OutputDir: missing})
	if err := r.Smoke(context.Background()); err != nil {
		t.Fatalf("smoke with a non-existent output dir: %v", err)
	}
}

func TestSmokeDetectsBrokenCommandLine(t *testing.T) {
	skipOnWindows(t)
	// A drifted sbx that rejects our command line (non-zero) must fail the smoke.
	path, _ := fakeSbx(t, fakeOpts{exit: 2, stderr: "unknown flag"})
	r := NewCLIRunner(Config{Path: path, OutputDir: t.TempDir()})
	if err := r.Smoke(context.Background()); err == nil {
		t.Fatal("smoke should fail when sbx rejects the command line")
	}
}

func TestVersionProbeTimeout(t *testing.T) {
	skipOnWindows(t)
	// A shim that hangs on --version must not block discovery: the bounded probe
	// times out and the runner reports unavailable.
	dir := t.TempDir()
	path := filepath.Join(dir, "sbx")
	script := "#!/bin/sh\nif [ \"$1\" = \"--version\" ]; then sleep 30; fi\nexit 0\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	old := versionProbeTimeout
	versionProbeTimeout = 200 * time.Millisecond
	defer func() { versionProbeTimeout = old }()

	start := time.Now()
	r := NewCLIRunner(Config{Path: path})
	if time.Since(start) > 5*time.Second {
		t.Fatalf("version probe did not respect the timeout (took %v)", time.Since(start))
	}
	if r.Available() {
		t.Fatal("a hung sbx --version must leave the runner unavailable")
	}
}

func TestDangerousEnvName(t *testing.T) {
	for _, bad := range []string{"LD_PRELOAD", "LD_LIBRARY_PATH", "DYLD_INSERT_LIBRARIES", "DOCKER_HOST", "PATH", "HOME", "HTTP_PROXY", "FTP_PROXY", "GIT_SSH", "GIT_SSH_COMMAND", "GIT_SSH_VARIANT", "BASH_FUNC_x"} {
		if !DangerousEnvName(bad) {
			t.Fatalf("DangerousEnvName(%q) = false, want true", bad)
		}
	}
	for _, ok := range []string{"API_KEY", "DB_PASSWORD", "MY_TOKEN", "PROXY_USER"} {
		if DangerousEnvName(ok) {
			t.Fatalf("DangerousEnvName(%q) = true, want false", ok)
		}
	}
}

func TestVersionMismatchFailsClosed(t *testing.T) {
	skipOnWindows(t)
	path, _ := fakeSbx(t, fakeOpts{versionLine: "sbx version 0.99.0"})
	// Fail closed: a minor mismatch is unavailable until explicitly allowed.
	r := NewCLIRunner(Config{Path: path})
	if r.Available() {
		t.Fatal("a version-mismatched sbx must be unavailable by default")
	}
	if !strings.Contains(r.Status().Reason, "allow_version_mismatch") {
		t.Fatalf("reason should point at the override: %q", r.Status().Reason)
	}
	// Opt in -> available, with the mismatch flagged.
	r2 := NewCLIRunner(Config{Path: path, AllowVersionMismatch: true})
	if !r2.Available() || !strings.Contains(r2.Status().Reason, "differs") {
		t.Fatalf("opt-in should be available + flagged: avail=%v reason=%q", r2.Available(), r2.Status().Reason)
	}
}

func TestCaptureBuffer(t *testing.T) {
	var c captureBuffer
	big := strings.Repeat("x", fileCaptureCap+1000)
	n, _ := c.Write([]byte(big))
	if n != len(big) {
		t.Fatalf("Write returned %d, want %d (full write expected)", n, len(big))
	}
	if !c.Truncated() {
		t.Fatal("oversized output should be marked truncated")
	}
	if len(c.Bytes()) > fileCaptureCap {
		t.Fatalf("capture not capped: %d bytes", len(c.Bytes()))
	}
}
