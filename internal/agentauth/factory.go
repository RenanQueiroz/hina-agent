package agentauth

import (
	"context"
	"fmt"
	"io"
	"strconv"

	"github.com/RenanQueiroz/hina-agent/internal/platform"
	"github.com/RenanQueiroz/hina-agent/internal/sandbox"
)

// CLIFactory is the production SessionFactory: it runs a provider login command
// interactively in a short-lived `sbx` container with a TTY (`sbx run -it`), the
// user's credential store mounted read-write, and network on. The host attaches via
// pipes — stdin for pasted codes, stdout for the streamed view — so no host-side PTY
// is needed (the container's TTY comes from `-t`, and device/paste-code is the
// mandatory fallback when a localhost browser callback can't reach the container).
//
// Like the sandbox Runner, the real container round-trip is validated on a host with
// `sbx` installed (Linux/macOS this phase; Windows in Phase 12); without sbx the
// factory reports unavailable and browser login is disabled. The argv assembly is
// unit-tested without a Docker daemon.
type CLIFactory struct {
	sbxPath   string
	kit       string         // admin-controlled sbx kit/template (same as normal runs)
	limits    sandbox.Limits // CPU/memory/PID caps (same as normal runs; timeout is loginTimeout)
	available bool
	reason    string
}

// NewCLIFactory resolves `sbx`, reusing the sandbox runner's availability decision
// (via Status) so the login path and the run path agree on whether sbx is usable. kit
// + limits are applied to the auth container so a login is no weaker than a normal run.
func NewCLIFactory(runner sandbox.Runner, kit string, limits sandbox.Limits) *CLIFactory {
	f := &CLIFactory{kit: kit, limits: limits}
	if runner == nil {
		f.reason = "sandbox runtime not configured"
		return f
	}
	st := runner.Status()
	f.available = st.Available
	f.sbxPath = st.Path
	f.reason = st.Reason
	if st.Path == "" {
		f.available = false
		if f.reason == "" {
			f.reason = "sbx binary not resolved"
		}
	}
	return f
}

// Available implements SessionFactory.
func (f *CLIFactory) Available() bool { return f.available && f.sbxPath != "" }

// Start launches the interactive login container and wires host pipes to it.
func (f *CLIFactory) Start(ctx context.Context, spec SessionSpec) (Session, error) {
	if !f.Available() {
		return nil, fmt.Errorf("agentauth: sbx unavailable: %s", f.reason)
	}
	if len(spec.Argv) == 0 {
		return nil, fmt.Errorf("agentauth: empty login argv")
	}
	args := buildAuthArgs(spec, f.kit, f.limits)
	cmd := platform.Command(ctx, f.sbxPath, args...)
	return newCLISession(ctx, cmd)
}

// newCLISession wires host pipes to cmd, starts it, and runs two goroutines:
//   - a deadline watcher that reaps the whole process TREE when ctx fires (a login
//     TIMEOUT or cancel) — exec.CommandContext only kills the immediate `sbx` process,
//     so without this a child holding the output pipe (or the network-on container +
//     credential-store mount) survives, AND cmd.Wait() blocks until that child exits.
//   - a wait goroutine that closes the output pipe writer when the process exits, so
//     the broker's read loop sees EOF on a NORMAL exit too (cmd.Stdout/Stderr are an
//     arbitrary io.Writer, which os/exec never closes itself — otherwise a clean login
//     would block forever in the read loop and never finalize).
func newCLISession(ctx context.Context, cmd *platform.Cmd) (*cliSession, error) {
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdoutR, stdoutW := io.Pipe()
	// Merge stdout+stderr into one stream so a prompt printed to either is seen.
	// io.Pipe serializes concurrent writers, so the two exec copy goroutines are safe.
	cmd.Stdout = stdoutW
	cmd.Stderr = stdoutW
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdoutW.Close()
		return nil, fmt.Errorf("agentauth: start login: %w", err)
	}
	s := &cliSession{cmd: cmd, stdin: stdin, stdout: stdoutR, stdoutW: stdoutW, done: make(chan struct{})}
	go func() {
		// Reap the tree on deadline/cancel (closes the pipe → unblocks Wait); exit
		// without acting if the process already finished (no leak, no double-kill harm).
		select {
		case <-ctx.Done():
			_ = cmd.KillTree()
		case <-s.done:
		}
	}()
	go func() {
		err := cmd.Wait()
		_ = cmd.KillTree() // reap any detached child a normal exit left behind
		s.waitErr = err
		_ = stdoutW.Close() // EOF the reader so the broker's read loop ends + finalizes
		close(s.done)
	}()
	return s, nil
}

// buildAuthArgs assembles the `sbx run -it` argv for an interactive login. The flag
// surface mirrors the pinned runner (B6); the smoke test there guards drift. It applies
// the SAME kit + CPU/memory/PID caps as a normal run, so a credential-bearing, network-on
// login container is no less bounded than a tool run. Pure (unit-tested without sbx).
func buildAuthArgs(spec SessionSpec, kit string, limits sandbox.Limits) []string {
	args := []string{"run", "-it", "--name", spec.ID}
	if kit != "" {
		args = append(args, "--kit", kit)
	}
	if limits.CPUs != "" {
		args = append(args, "--cpus", limits.CPUs)
	}
	if limits.Memory != "" {
		args = append(args, "-m", limits.Memory)
	}
	if limits.PIDs > 0 {
		args = append(args, "--pids-limit", strconv.Itoa(limits.PIDs))
	}
	// Mount the credential store read-write so the login writes tokens into it.
	if spec.StateDir != "" && spec.StateContainerDir != "" {
		args = append(args, spec.StateDir+":"+spec.StateContainerDir)
	}
	for _, e := range spec.Env {
		args = append(args, "--env", e)
	}
	args = append(args, "--")
	args = append(args, spec.Argv...)
	return args
}

// cliSession is a running `sbx run -it` login process with host pipes attached. A
// background goroutine (started in newCLISession) waits for the process and closes
// the output pipe writer on exit; Wait blocks on that goroutine's result.
type cliSession struct {
	cmd     *platform.Cmd
	stdin   io.WriteCloser
	stdout  *io.PipeReader
	stdoutW *io.PipeWriter

	done    chan struct{}
	waitErr error
}

func (s *cliSession) Stdout() io.Reader { return s.stdout }

func (s *cliSession) Write(p []byte) (int, error) { return s.stdin.Write(p) }

// Wait blocks until the wait goroutine has observed the process exit. waitErr is
// written before done is closed, so reading it after <-done is race-free.
func (s *cliSession) Wait() error {
	<-s.done
	return s.waitErr
}

func (s *cliSession) Kill() {
	_ = s.stdin.Close()
	_ = s.cmd.KillTree()
	// Reaping the tree makes cmd.Wait return, which closes stdoutW (the read loop
	// then ends); CloseWithError here also unblocks a reader immediately.
	_ = s.stdoutW.CloseWithError(io.EOF)
}
