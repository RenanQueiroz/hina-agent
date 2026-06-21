package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/RenanQueiroz/hina-agent/internal/id"
	"github.com/RenanQueiroz/hina-agent/internal/platform"
)

// maxInlineOutput bounds how much stdout/stderr is kept in memory (and ultimately
// fed back to the model); the full streams always go to the captured files.
const maxInlineOutput = 64 << 10 // 64 KiB

// defaultTimeout is the hard wall-clock cap applied when a spec sets none.
const defaultTimeout = 5 * time.Minute

// semverRe extracts a major.minor.patch from `sbx --version` output.
var semverRe = regexp.MustCompile(`(\d+)\.(\d+)\.(\d+)`)

// versionProbeTimeout bounds the `sbx --version` discovery call (a package var so
// tests can shorten it). A hung sbx then degrades the optional feature instead of
// blocking startup.
var versionProbeTimeout = 5 * time.Second

// CLIRunner is the production Runner: it shells out to a pinned `sbx` binary
// through internal/platform (process-tree-aware) so a runaway sandbox helper is
// fully reaped, captures output to owner-private files, and enforces the per-run
// timeout itself rather than trusting an sbx flag.
type CLIRunner struct {
	path      string // resolved sbx binary path ("" when unavailable)
	version   string // detected version ("" when unknown)
	available bool
	reason    string

	kit       string
	defaults  Limits
	outputDir string
	log       *slog.Logger
}

// Config configures a CLIRunner.
type Config struct {
	Path                 string // override sbx path ("" -> PATH lookup)
	Kit                  string // admin-controlled kit/template (optional)
	Defaults             Limits // default cpu/memory/pids/timeout
	OutputDir            string // directory for captured stdout/stderr files
	AllowVersionMismatch bool   // run even when the detected sbx minor != PinnedVersion
	Log                  *slog.Logger
}

// NewCLIRunner resolves and version-checks `sbx`. It never fails hard: when sbx
// is absent or unparseable it returns a runner that reports unavailable, so the
// rest of the server (and `hina doctor`) keeps working and sandbox-dependent
// features degrade gracefully.
func NewCLIRunner(cfg Config) *CLIRunner {
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	r := &CLIRunner{kit: cfg.Kit, defaults: cfg.Defaults, outputDir: cfg.OutputDir, log: log}

	path := cfg.Path
	if path == "" {
		p, err := platform.LookPath("sbx")
		if err != nil {
			r.reason = "sbx not found on PATH"
			return r
		}
		path = p
	}
	r.path = path

	// Bound the version probe so a hung/broken sbx degrades the optional sandbox
	// feature rather than blocking `hina server` / `hina doctor` indefinitely. The
	// probe reaps the whole process tree on timeout (a child holding the output pipe
	// must not keep the call alive past the deadline).
	out, err := probeSbxVersion(path, versionProbeTimeout)
	if err != nil {
		// A present-but-unrunnable/timed-out binary is unavailable, not fatal.
		r.reason = "sbx --version failed: " + err.Error()
		return r
	}
	v := semverRe.FindString(string(out))
	if v == "" {
		r.reason = "could not parse sbx version from: " + strings.TrimSpace(string(out))
		return r
	}
	r.version = v
	switch {
	case sameMinor(v, PinnedVersion):
		r.available = true
	case cfg.AllowVersionMismatch:
		// Operator opted in after vetting; the smoke test is the real drift guard.
		r.available = true
		r.reason = fmt.Sprintf("sbx %s differs from pinned %s (allowed via [sandbox] allow_version_mismatch); verify the smoke test", v, PinnedVersion)
	default:
		// Fail closed: the pinned command-line surface is part of the security
		// boundary, so an unvetted sbx does not run tools until explicitly allowed.
		r.available = false
		r.reason = fmt.Sprintf("sbx %s differs from pinned %s; set [sandbox] allow_version_mismatch=true after verifying the command-line smoke test", v, PinnedVersion)
	}
	return r
}

// Available implements Runner.
func (r *CLIRunner) Available() bool { return r.available }

// MarkUnavailable disables the runner with a reason (e.g. a failed startup smoke
// test), so `Available`/`Status` and everything gated on them report it off.
func (r *CLIRunner) MarkUnavailable(reason string) {
	r.available = false
	if reason != "" {
		r.reason = reason
	}
}

// Status implements Runner.
func (r *CLIRunner) Status() Status {
	return Status{Available: r.available, Version: r.version, Pinned: PinnedVersion, Path: r.path, Reason: r.reason}
}

// Run implements Runner.
func (r *CLIRunner) Run(ctx context.Context, spec RunSpec) (RunResult, error) {
	if !r.available {
		return RunResult{Err: errors.New("sandbox: sbx unavailable: " + r.reason)}, nil
	}
	if len(spec.Argv) == 0 {
		return RunResult{}, errors.New("sandbox: empty argv")
	}
	sandboxID := id.New("sbx")
	// Defense in depth: drop any granted secret whose name the host loader / Docker
	// client / shell would interpret (the policy layer already rejects these at save
	// time; this guards a pre-existing grant or any path that skipped validation).
	spec.SecretEnv = r.safeSecretEnv(spec.SecretEnv)
	args := r.buildRunArgs(sandboxID, spec)

	timeout := spec.Limits.Timeout
	if timeout <= 0 {
		timeout = r.defaults.Timeout
	}
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Capture stdout/stderr to bounded in-memory buffers. Output is redacted BEFORE
	// anything is written to disk (a tool that echoes a granted secret must not
	// leave plaintext in the capture file), so nothing is streamed to a file live.
	var outBuf, errBuf captureBuffer
	cmd := platform.Command(runCtx, r.path, args...)
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	if len(spec.Stdin) > 0 {
		cmd.Stdin = bytes.NewReader(spec.Stdin)
	}
	// Forward granted secrets through the sbx PROCESS environment (paired with the
	// name-only `--env <NAME>` flags in buildRunArgs), so their values never appear
	// on the argv / in `ps` / in command logs. os.Environ() is kept so sbx itself
	// has PATH/HOME/Docker creds; only the named vars cross into the sandbox.
	if len(spec.SecretEnv) > 0 {
		cmd.Env = append(os.Environ(), spec.SecretEnv...)
	}

	start := time.Now()
	if err := cmd.Start(); err != nil {
		return RunResult{SandboxID: sandboxID, Err: fmt.Errorf("sandbox: start sbx: %w", err)}, nil
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	var waitErr error
	select {
	case waitErr = <-done:
		// The main process exited on its own; still reap any detached child it may
		// have spawned — exec.CommandContext only kills the direct process.
		_ = cmd.KillTree()
	case <-runCtx.Done():
		// Timeout / cancel: reap the whole process tree (also unblocks Wait if a child
		// is holding the output pipe), then collect Wait.
		_ = cmd.KillTree()
		waitErr = <-done
	}
	// Classify by the deadline itself, not by which select branch won: a race where
	// Wait returns just as the deadline fires must still count as timed out (and the
	// tree is reaped in both branches above).
	timedOut := errors.Is(runCtx.Err(), context.DeadlineExceeded)

	// Redact the full captured output, THEN persist it — the file on disk only ever
	// holds redacted bytes. When the buffer was truncated at the cap, a granted secret
	// could have straddled the boundary, leaving an unredactable prefix at the cut;
	// drop a trailing margin of (maxSecretLen-1) so no partial secret reaches disk.
	outRed := safeRedact(spec.Redactor, outBuf.Bytes(), outBuf.Truncated())
	errRed := safeRedact(spec.Redactor, errBuf.Bytes(), errBuf.Truncated())
	stdoutPath, soErr := r.writeCapture(sandboxID, "stdout", outRed)
	stderrPath, seErr := r.writeCapture(sandboxID, "stderr", errRed)

	res := RunResult{
		SandboxID:  sandboxID,
		Stdout:     inlineOutput(outRed, outBuf.Truncated()),
		Stderr:     inlineOutput(errRed, errBuf.Truncated()),
		StdoutPath: stdoutPath,
		StderrPath: stderrPath,
		Duration:   time.Since(start),
		TimedOut:   timedOut,
	}
	// A capture-write failure must not be swallowed: the command already ran, so
	// record it (logged here, folded into the audit row by the Router) rather than
	// returning as if the output had been saved.
	if captErr := errors.Join(soErr, seErr); captErr != nil {
		r.log.Warn("sandbox: capture write failed", "sandbox", sandboxID, "err", captErr)
		res.CaptureErr = captErr.Error()
	}
	res.ExitCode, res.Err = classifyWait(waitErr, timedOut)
	return res, nil
}

// buildRunArgs assembles the `sbx run` argv for a spec. It is a pure function so
// the command-line construction — the part that drifts with sbx releases — is
// unit-tested in full without a Docker daemon. The flag surface matches the
// pinned v0.33.0 documentation (B6); the smoke test guards against drift.
func (r *CLIRunner) buildRunArgs(sandboxID string, spec RunSpec) []string {
	args := []string{"run", "--name", sandboxID}
	if r.kit != "" {
		args = append(args, "--kit", r.kit)
	}

	cpus := spec.Limits.CPUs
	if cpus == "" {
		cpus = r.defaults.CPUs
	}
	if cpus != "" {
		args = append(args, "--cpus", cpus)
	}
	mem := spec.Limits.Memory
	if mem == "" {
		mem = r.defaults.Memory
	}
	if mem != "" {
		args = append(args, "-m", mem)
	}
	pids := spec.Limits.PIDs
	if pids == 0 {
		pids = r.defaults.PIDs
	}
	if pids > 0 {
		args = append(args, "--pids-limit", strconv.Itoa(pids))
	}

	// Workspace mount: read-only (with --clone the agent works on an in-sandbox
	// git clone) or read-write.
	if spec.Workspace != "" {
		mount := spec.Workspace + ":/workspace"
		if spec.Clone {
			args = append(args, "--clone")
			mount += ":ro"
		}
		args = append(args, mount)
	}
	for _, m := range spec.Mounts {
		mount := m.Host + ":" + m.Container
		if m.ReadOnly {
			mount += ":ro"
		}
		args = append(args, mount)
	}

	workdir := spec.Workdir
	if workdir == "" && spec.Workspace != "" {
		workdir = "/workspace"
	}
	if workdir != "" {
		args = append(args, "--workdir", workdir)
	}
	// Non-secret env may appear as a value on the argv...
	for _, e := range spec.Env {
		args = append(args, "--env", e)
	}
	// ...but a granted secret is forwarded by NAME only (`--env <NAME>`), its value
	// coming from the sbx process environment (set in Run) so it never hits the argv.
	for _, name := range secretEnvNames(spec.SecretEnv) {
		args = append(args, "--env", name)
	}

	// Everything after `--` is the in-sandbox command (argv-first, no shell).
	args = append(args, "--")
	args = append(args, spec.Argv...)
	return args
}

// safeSecretEnv drops secret-env entries whose name is host-interpreted (loader /
// Docker client / shell), logging each — so a poisoned grant can never reach the
// host sbx process environment.
func (r *CLIRunner) safeSecretEnv(pairs []string) []string {
	out := make([]string, 0, len(pairs))
	for _, p := range pairs {
		name := p
		if i := strings.IndexByte(p, '='); i >= 0 {
			name = p[:i]
		}
		if DangerousEnvName(name) {
			r.log.Warn("sandbox: dropping secret grant with a host-interpreted env name", "name", name)
			continue
		}
		out = append(out, p)
	}
	return out
}

// secretEnvNames extracts the NAME from each "NAME=VALUE" secret-env entry.
func secretEnvNames(pairs []string) []string {
	names := make([]string, 0, len(pairs))
	for _, p := range pairs {
		if i := strings.IndexByte(p, '='); i > 0 {
			names = append(names, p[:i])
		}
	}
	return names
}

// Smoke runs a representative command line through sbx and checks it is accepted.
// It is the drift guard B6 mandates — gate any sbx upgrade on it (run by
// `hina doctor` and CI when sbx is present). A non-nil error means the pinned
// command-line surface no longer matches the installed sbx.
func (r *CLIRunner) Smoke(ctx context.Context) error {
	if !r.available {
		return errors.New("sbx unavailable: " + r.reason)
	}
	// Ensure the configured capture dir exists before using it as the temp parent —
	// on a fresh install only the runtime root is created, not this child dir, so a
	// valid sbx must not be marked unavailable just because the dir is missing.
	if r.outputDir != "" {
		if err := platform.EnsurePrivateDir(r.outputDir); err != nil {
			return err
		}
	}
	dir, err := os.MkdirTemp(r.outputDir, "smoke-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	spec := RunSpec{Tool: ToolShell, Argv: []string{"/bin/true"}, Workspace: dir}
	res, err := r.Run(ctx, spec)
	if err != nil {
		return err
	}
	if res.Err != nil {
		return res.Err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("smoke command exited %d (stderr: %s)", res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return nil
}

// writeCapture writes one already-redacted stream to an owner-private file and
// returns its path. The bytes are redacted by the caller before this point, so the
// file never contains a plaintext secret.
func (r *CLIRunner) writeCapture(sandboxID, stream string, data []byte) (string, error) {
	dir := r.outputDir
	if dir == "" {
		f, err := os.CreateTemp("", "sbx-"+stream+"-")
		if err != nil {
			return "", fmt.Errorf("sandbox: capture %s: %w", stream, err)
		}
		_, _ = f.Write(data)
		_ = f.Close()
		return f.Name(), nil
	}
	if err := platform.EnsurePrivateDir(dir); err != nil {
		return "", fmt.Errorf("sandbox: capture dir: %w", err)
	}
	path := filepath.Join(dir, sandboxID+"."+stream+".log")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return "", fmt.Errorf("sandbox: capture %s: %w", stream, err)
	}
	return path, nil
}

// safeRedact redacts captured output and, when it was truncated at the cap, drops a
// trailing margin of (maxSecretLen-1) bytes so a secret straddling the cut — whose
// retained prefix exact-match redaction can't catch — never reaches disk.
func safeRedact(r OutputRedactor, b []byte, truncated bool) []byte {
	if r == nil {
		return b
	}
	out := r.RedactBytes(b)
	if truncated {
		if margin := r.MaxValueLen() - 1; margin > 0 {
			if margin >= len(out) {
				return nil
			}
			out = out[:len(out)-margin]
		}
	}
	return out
}

// inlineOutput truncates captured output to the model-visible inline cap.
func inlineOutput(b []byte, truncated bool) string {
	if len(b) > maxInlineOutput {
		b = b[:maxInlineOutput]
		truncated = true
	}
	out := string(b)
	if truncated {
		out += "\n…[output truncated]"
	}
	return out
}

// classifyWait turns a cmd.Wait() error into an exit code + execution error. A
// non-zero exit is reported via ExitCode, not Err; Err is for spawn/timeout/IO
// failures the model can't recover from by reading stderr.
func classifyWait(waitErr error, timedOut bool) (int, error) {
	if timedOut {
		return -1, errors.New("sandbox: run timed out")
	}
	if waitErr == nil {
		return 0, nil
	}
	var ee *exec.ExitError
	if errors.As(waitErr, &ee) {
		return ee.ExitCode(), nil
	}
	return -1, fmt.Errorf("sandbox: run failed: %w", waitErr)
}

// probeSbxVersion runs `sbx --version` with a hard timeout, reaping the whole
// process tree if it overruns so a child holding the output pipe can't keep the
// call alive past the deadline.
func probeSbxVersion(path string, timeout time.Duration) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := platform.Command(ctx, path, "--version")
	var buf bytes.Buffer
	cmd.Stdout = &buf
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		// Reap any background child the probe may have spawned (CommandContext only
		// kills the direct process).
		_ = cmd.KillTree()
		return buf.Bytes(), err
	case <-ctx.Done():
		_ = cmd.KillTree()
		<-done
		return nil, ctx.Err()
	}
}

// sameMinor reports whether two semvers share a major.minor (patch drift is fine).
func sameMinor(a, b string) bool {
	am := semverRe.FindStringSubmatch(a)
	bm := semverRe.FindStringSubmatch(b)
	if am == nil || bm == nil {
		return false
	}
	return am[1] == bm[1] && am[2] == bm[2]
}

// fileCaptureCap bounds how much of each stream is captured (so a runaway command
// can't exhaust memory). The whole captured buffer is redacted at once, so secrets
// spanning write boundaries are scrubbed; a secret straddling the cap itself is
// handled by dropping a trailing margin (see Run). A package var so tests can
// shrink it.
var fileCaptureCap = 1 << 20 // 1 MiB

// captureBuffer accumulates up to fileCaptureCap bytes, recording truncation.
type captureBuffer struct {
	buf       bytes.Buffer
	truncated bool
}

func (c *captureBuffer) Write(p []byte) (int, error) {
	if remaining := fileCaptureCap - c.buf.Len(); remaining > 0 {
		if len(p) <= remaining {
			c.buf.Write(p)
		} else {
			c.buf.Write(p[:remaining])
			c.truncated = true
		}
	} else if len(p) > 0 {
		c.truncated = true
	}
	return len(p), nil // always report a full write so the writer isn't starved
}

func (c *captureBuffer) Bytes() []byte   { return c.buf.Bytes() }
func (c *captureBuffer) Truncated() bool { return c.truncated }
