// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/client"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// attachShell launches the interactive shell for Attach. It PREFERS a PERSISTENT
// tmux session ("wardyn") so the terminal survives WebSocket detaches — switching
// UI tabs, a browser refresh, or a dropped connection re-attaches to the SAME
// session (same cwd, env, scrollback, and any running `claude`), giving one
// durable terminal per run. tmux runs bash inside, so readline (tab-completion,
// history, line editing) works. Fallbacks keep the attach portable: bash when
// tmux is absent, then /bin/sh for minimal/busybox images. NOT a login shell
// (-l): minimal images may lack profile scripts. A real TERM is set on the exec
// env (see Attach) so readline and TUIs render correctly.
//
// `new-session -A -s wardyn bash`: create the session running bash, or (if it
// already exists) attach to it — the bash arg is ignored on attach, so the
// session persists exactly as first created.
//
// Two prep guards run first — agent-run's session prep does the same work,
// but only after slower steps (a measured 18s on a live session), while an
// attach shell opens the instant the container runs, so the operator's first
// command can win that race. Both are runtime-and-requirements-driven (from
// the run's own env / the operator's own mounts), so nothing
// toolchain-specific is ever baked into an image:
//   - GOTMPDIR mkdir: dispatch points it at a dir the go tool refuses to
//     create itself; a run whose env doesn't set it does nothing.
//   - git safe.directory '*': a dir-mounted workspace keeps its HOST
//     ownership while the session may run as another uid — git's
//     dubious-ownership refusal (exit 128) breaks git AND go's VCS stamping.
//     Everything mounted here is what the operator onboarded, and the config
//     dies with the container. Lockstep: trust_mounted_repos, agent-run-lib.sh.
var attachShell = []string{"/bin/sh", "-c",
	`[ -n "${GOTMPDIR:-}" ] && mkdir -p "$GOTMPDIR" 2>/dev/null; ` +
		`command -v git >/dev/null 2>&1 && git config --global --add safe.directory '*' 2>/dev/null; ` +
		`if command -v tmux >/dev/null 2>&1; then exec tmux new-session -A -s wardyn bash; ` +
		`elif command -v bash >/dev/null 2>&1; then exec bash -i; else exec /bin/sh -i; fi`}

// Attach opens a NEW interactive exec (an interactive shell) inside the running
// sandbox ref and returns a live PTY runner.Session. It mirrors Exec's
// interactive-style hijack (Tty + AttachStdin/out/err + ExecAttach)
// but is deliberately SEPARATE from the agent process:
//
//   - The new exec is NOT registered in d.agentExecs. That map is exclusively
//     the agent process Wait observes; an interactive shell is a distinct,
//     human-owned stream whose lifecycle is the WebSocket attach, not the run.
//   - Closing the returned Session tears down only this exec stream (resp.Close)
//     — it never touches the sandbox, the agent, or the sidecars.
//
// SECURITY (invariant 3): the shell runs inside the already-confined sandbox, so
// it inherits the same L0 structural-egress + confinement envelope as the agent.
// No new network path is opened: the PTY bytes flow control-plane -> dockerd ->
// container over the Docker exec hijack, never through the sandbox's HTTP_PROXY
// egress path. Egress and credential-mint enforcement remain at the proxy/broker
// regardless of this attach. The human principal is recorded for attribution
// (invariant 4) by the caller (the API layer), not here — the driver is
// identity-agnostic by the parity rule.
func (d *Driver) Attach(ctx context.Context, ref string, opts runner.AttachOptions) (runner.Session, error) {
	if ref == "" {
		return nil, fmt.Errorf("docker: attach: empty sandbox ref")
	}

	execCfg := client.ExecCreateOptions{
		TTY:          true,
		AttachStdin:  true,
		AttachStdout: true,
		AttachStderr: true,
		Cmd:          attachShell,
		// A real TERM so readline (tab-completion, history) and TUIs (claude's
		// own UI) render correctly; the image leaves TERM unset otherwise.
		// A UTF-8 locale is REQUIRED: tmux re-encodes its cell buffer for the
		// attach client, and with a non-UTF-8 client locale it transcodes
		// Unicode it can't represent (block elements ▐▛█, symbols like ❯) to
		// "_" — the underscores operators saw. C.UTF-8 is built into glibc, so
		// no locale package is needed.
		Env: []string{
			"TERM=xterm-256color",
			"LANG=C.UTF-8",
			"LC_ALL=C.UTF-8",
		},
	}
	// Seed the initial PTY size when the client supplied one; ExecCreate
	// accepts ConsoleSize so the very first output is already correctly wrapped.
	if opts.Cols > 0 && opts.Rows > 0 {
		execCfg.ConsoleSize = client.ConsoleSize{Height: uint(opts.Rows), Width: uint(opts.Cols)}
	}

	created, err := d.cli.ExecCreate(ctx, ref, execCfg)
	if err != nil {
		return nil, fmt.Errorf("docker: attach exec create: %w", err)
	}

	// TTY hijack: resp.Reader is the PTY output, resp.Conn is the input writer.
	attachRes, err := d.cli.ExecAttach(ctx, created.ID, client.ExecAttachOptions{TTY: true})
	if err != nil {
		return nil, fmt.Errorf("docker: attach exec attach: %w", err)
	}

	return &dockerSession{
		cli:    d.cli,
		execID: created.ID,
		resp:   attachRes.HijackedResponse,
	}, nil
}

// dockerSession is the runner.Session backed by a Docker exec TTY hijack. The
// hijacked response carries the bidirectional PTY: Reader is terminal output,
// Conn is keystroke input. Resize drives ExecResize; Close closes the
// hijack (and only the hijack).
type dockerSession struct {
	cli    dockerAPI
	execID string
	resp   client.HijackedResponse
}

var _ runner.Session = (*dockerSession)(nil)

// Read copies terminal output from the hijacked PTY. With Tty:true the stream is
// raw (no Docker stdcopy multiplexing header), so the bytes are the literal
// terminal output and can be forwarded verbatim as a binary WebSocket frame.
func (s *dockerSession) Read(p []byte) (int, error) {
	return s.resp.Reader.Read(p)
}

// Write sends keystrokes into the PTY via the hijacked connection.
func (s *dockerSession) Write(p []byte) (int, error) {
	return s.resp.Conn.Write(p)
}

// Resize informs the exec PTY of a new window size. Docker's ExecResizeOptions
// takes Height (rows) and Width (cols).
func (s *dockerSession) Resize(ctx context.Context, cols, rows uint16) error {
	if cols == 0 || rows == 0 {
		return nil // ignore degenerate sizes rather than erroring the stream
	}
	if _, err := s.cli.ExecResize(ctx, s.execID, client.ExecResizeOptions{
		Height: uint(rows),
		Width:  uint(cols),
	}); err != nil {
		return fmt.Errorf("docker: attach resize: %w", err)
	}
	return nil
}

// Close tears down ONLY the interactive exec stream (the hijacked connection).
// The sandbox, the agent process, and the sidecars are untouched: detaching a
// human leaves the run exactly as it was. HijackedResponse.Close is idempotent.
func (s *dockerSession) Close() error {
	s.resp.Close()
	return nil
}

// execStdin adapts a docker exec's hijacked write side to io.WriteCloser:
// Write sends bytes to the exec's stdin; Close HALF-closes the write side
// (HijackedResponse.CloseWrite) so the exec observes EOF on stdin while
// Stdout/Stderr keep flowing. A full resp.Close() would tear down the whole
// hijacked connection out from under the still-live output streams, so Close
// here MUST NOT call it.
type execStdin struct {
	resp *client.HijackedResponse
}

func (s execStdin) Write(p []byte) (int, error) { return s.resp.Conn.Write(p) }
func (s execStdin) Close() error                { return s.resp.CloseWrite() }

// ExecStream launches spec.Argv inside ref as a fresh, streamable exec —
// distinct from the agent process Exec starts (tracked in d.agentExecs,
// observed by Wait) and from Attach's interactive shell (attachShell wraps a
// login-style shell in tmux/bash; ExecStream runs spec.Argv directly, no
// wrapping).
//
// When spec.TTY is false, stdout and stderr are demultiplexed (stdcopy) into
// SEPARATE streams so a binary protocol riding stdout (SFTP, socat) is never
// corrupted by interleaved stderr bytes. When spec.TTY is true the PTY merges
// both onto Stdout (PTY semantics), so Stderr is a reader that yields io.EOF
// immediately — present (never nil) so callers can read it uniformly without
// a TTY check, but empty.
func (d *Driver) ExecStream(ctx context.Context, ref string, spec runner.ExecSpec) (*runner.ExecSession, error) {
	if len(spec.Argv) == 0 {
		return nil, errors.New("docker: exec stream: empty argv")
	}

	execCfg := client.ExecCreateOptions{
		TTY:          spec.TTY,
		AttachStdin:  true,
		AttachStdout: true,
		AttachStderr: true,
		Env:          spec.Env,
		Cmd:          spec.Argv,
	}
	// ConsoleSize is only valid alongside TTY (getConsoleSize rejects it
	// otherwise), mirroring Attach's guard above.
	if spec.TTY && spec.Cols > 0 && spec.Rows > 0 {
		execCfg.ConsoleSize = client.ConsoleSize{Height: uint(spec.Rows), Width: uint(spec.Cols)}
	}

	created, err := d.cli.ExecCreate(ctx, ref, execCfg)
	if err != nil {
		return nil, fmt.Errorf("docker: exec stream create: %w", err)
	}

	attachRes, err := d.cli.ExecAttach(ctx, created.ID, client.ExecAttachOptions{TTY: spec.TTY})
	if err != nil {
		return nil, fmt.Errorf("docker: exec stream attach: %w", err)
	}
	resp := attachRes.HijackedResponse

	var stdout, stderr io.Reader
	if spec.TTY {
		stdout = resp.Reader
		stderr = bytes.NewReader(nil)
	} else {
		// Non-TTY: the hijacked stream multiplexes stdout/stderr behind an
		// 8-byte frame header per chunk (see stdcopy). Demux it live into a
		// pipe pair so Stdout/Stderr stream progressively rather than
		// buffering the whole exec's output before either is readable.
		outR, outW := io.Pipe()
		errR, errW := io.Pipe()
		go func() {
			_, cerr := stdcopy.StdCopy(outW, errW, resp.Reader)
			_ = outW.CloseWithError(cerr)
			_ = errW.CloseWithError(cerr)
		}()
		stdout, stderr = outR, errR
	}

	execID := created.ID
	return &runner.ExecSession{
		Stdin:  execStdin{resp: &resp},
		Stdout: stdout,
		Stderr: stderr,
		Resize: func(cols, rows uint16) error {
			if cols == 0 || rows == 0 {
				return nil // ignore degenerate sizes rather than erroring the stream
			}
			if _, err := d.cli.ExecResize(ctx, execID, client.ExecResizeOptions{
				Height: uint(rows),
				Width:  uint(cols),
			}); err != nil {
				return fmt.Errorf("docker: exec stream resize: %w", err)
			}
			return nil
		},
		Wait: func() (int, error) { return d.pollExecExit(ctx, execID) },
		Close: func() error {
			resp.Close()
			// Also close the pipe READ ends (non-TTY branch): resp.Close alone
			// leaves the demux goroutine parked on a pipe write and any reader
			// parked on a pipe read — closing the read side fails both with
			// io.ErrClosedPipe so every blocked caller (evidence endpoints, the
			// SSH exec/sftp/forward bridges) actually unblocks. TTY branch
			// readers aren't Closers; the type assertions no-op there.
			if c, ok := stdout.(io.Closer); ok {
				_ = c.Close()
			}
			if c, ok := stderr.(io.Closer); ok {
				_ = c.Close()
			}
			return nil
		},
	}, nil
}
