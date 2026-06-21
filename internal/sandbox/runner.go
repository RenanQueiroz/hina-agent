// Package sandbox is Hina's per-user security boundary. Every user-scoped side
// effect a model requests — a shell command, a file read/write, an HTTP fetch —
// runs inside that user's Docker `sbx` sandbox with explicit grants, resource
// limits, and an audit log, NEVER on the host. It wraps a PINNED `sbx` version
// (the CLI moves fast and breaks compatibility) behind a command-line smoke test,
// manages the durable/ephemeral workspaces those runs use, and enforces the
// per-user Sandbox Environment policy (allowed tools + the network allow-list,
// gated at REQUEST time by the Router). Injected secrets are forwarded via the
// sbx process environment (never the argv) and redacted from captured output
// before it is written to disk.
//
// `sbx` itself is validated hands-on per OS in the Windows hardening phase
// (Phase 12) and on any host that has it installed; in environments without `sbx`
// the runner reports unavailable (like the ONNX runtime), and the command-line
// construction is unit-tested against a fake `sbx` shim so our argv/limits/policy
// logic is covered without a real Docker daemon.
package sandbox

import (
	"context"
	"time"
)

// PinnedVersion is the `sbx` release this runner is built and smoke-tested
// against. The CLI ships breaking changes between minors (B6: re-attach became
// `sbx run --name`, `sbx policy -g` was removed), so an upgrade must pass the
// command-line smoke test before it is trusted.
const PinnedVersion = "0.33.0"

// Tool names recorded in the audit log + used by the policy allow-list.
const (
	ToolShell   = "shell"
	ToolFSRead  = "fs_read"
	ToolFSWrite = "fs_write"
	ToolHTTP    = "http_fetch"
)

// Mount is one host path made visible inside the sandbox. ReadOnly maps to the
// `:ro` suffix on the positional `sbx run` mount.
type Mount struct {
	Host      string
	Container string
	ReadOnly  bool
}

// NetworkRule is one host-service the tool asked to reach. The Router enforces
// the per-user network allow-list at REQUEST time (a host:port the policy denies
// is rejected before any sandbox work), so this is the authoritative per-user
// gate. The container's actual egress is governed by `sbx`'s own policy
// (default-deny under Balanced/Locked-Down): Hina does NOT mutate the host-global
// `sbx policy` per run — that would leak grants across users/runs — so a NetworkRule
// here is informational/audit metadata, not a container-level open. Per-run
// container egress grants are the host-inference gateway's job (Phase 8/11).
type NetworkRule struct {
	Host string
	Port int
}

// OutputRedactor scrubs known secret values from captured output before it is
// written to disk or returned. *vault.Redactor satisfies it; nil means no
// redaction. Keeping the interface here lets the runner redact at capture time
// without importing the vault package. MaxValueLen is the longest secret value's
// length, used as a safe trailing margin when output is truncated (a secret split
// across the cap leaves at most MaxValueLen-1 unredactable bytes).
type OutputRedactor interface {
	RedactBytes([]byte) []byte
	MaxValueLen() int
}

// Limits bounds a run's resources. Zero fields fall back to the runner defaults.
type Limits struct {
	CPUs    string        // e.g. "2" (sbx --cpus)
	Memory  string        // e.g. "2g" (sbx -m)
	PIDs    int           // max processes (0 = runner default / omit)
	Timeout time.Duration // hard wall-clock cap, enforced by the runner via KillTree
}

// RunSpec is a typed sandbox invocation. The Go server owns every field — the
// model supplies structured tool arguments, never a raw CLI string — so command
// injection and host escape are not reachable from a model response.
type RunSpec struct {
	UserID         string // owner (audit + workspace scoping)
	ConversationID string // optional, for audit correlation
	Tool           string // ToolShell/ToolFSRead/... (audit + policy)

	Argv      []string // command to run inside the sandbox (argv-first, no shell)
	Workdir   string   // working directory inside the sandbox (default /workspace)
	Env       []string // NON-secret env ("K=V") — safe to appear on the argv
	SecretEnv []string // run-scoped injected secrets ("K=V") — forwarded via the sbx
	// process environment + a name-only `--env K` flag, NEVER as a value on the argv,
	// so plaintext never appears in `ps`/process accounting/command logs.
	Stdin []byte // optional stdin fed to the command (e.g. a file's contents)

	Workspace string  // host workspace dir mounted read-write at /workspace
	Mounts    []Mount // additional mounts
	Clone     bool    // mount the workspace read-only and work on an in-sandbox clone

	Network  []NetworkRule // informational (see NetworkRule); the Router gates requests
	Limits   Limits
	Redactor OutputRedactor // scrub injected secret values from captured output (may be nil)
}

// RunResult is the normalized outcome of a sandbox run. Stdout/Stderr are the
// inline-captured (and, by the time a caller returns them, secret-redacted) output;
// the full streams are written to StdoutPath/StderrPath. ExitCode is the command's
// exit status; Err is reserved for an execution-layer failure (sbx missing, policy
// rejected, spawn error) distinct from a non-zero command exit.
type RunResult struct {
	SandboxID  string
	ExitCode   int
	Stdout     string
	Stderr     string
	StdoutPath string
	StderrPath string
	Duration   time.Duration
	TimedOut   bool
	Err        error
	// CaptureErr is set when the command ran but its output could not be persisted
	// to disk (unwritable dir / full disk). The run's side effects happened, so this
	// is recorded in the audit row rather than silently lost — the forensic gap is
	// visible.
	CaptureErr string
}

// Status reports runner availability for `hina doctor` and the admin UI.
type Status struct {
	Available bool   `json:"available"`
	Version   string `json:"version,omitempty"` // detected sbx version
	Pinned    string `json:"pinned"`            // version this build expects
	Path      string `json:"path,omitempty"`    // resolved sbx binary path
	Reason    string `json:"reason,omitempty"`  // why unavailable
}

// Runner executes typed specs inside per-user `sbx` sandboxes.
type Runner interface {
	// Available reports whether a usable, version-matched sbx is present.
	Available() bool
	// Status returns the detailed availability report.
	Status() Status
	// Run executes spec and returns the normalized result. ctx cancellation (a
	// client abort or the per-run timeout) terminates the sandbox process tree.
	Run(ctx context.Context, spec RunSpec) (RunResult, error)
}
