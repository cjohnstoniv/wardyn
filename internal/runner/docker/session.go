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

// attachShell launches the interactive shell for Attach. It prefers a
// persistent tmux session ("wardyn") so a WebSocket detach (tab switch,
// refresh, dropped connection) re-attaches to the same session (cwd, env,
// scrollback, any running `claude`) rather than starting a new one; falls
// back to bash, then /bin/sh, on images without tmux. Not a login shell (-l):
// minimal images may lack profile scripts. `new-session -A -s wardyn bash`
// creates or attaches; the bash arg is ignored on attach so the session
// persists exactly as first created.
//
// Two prep guards run first, ahead of agent-run's session prep which does the
// same work but only after slower steps (measured 18s), so the operator's
// first command wins that race. Both come only from the run's own env/mounts,
// never baked into an image:
//   - GOTMPDIR mkdir: the go tool won't create it itself; a no-op if unset.
//   - git safe.directory '*': a mounted workspace keeps host ownership while
//     the session may run as another uid, so without this git's
//     dubious-ownership refusal (exit 128) breaks git and go's VCS stamping;
//     config dies with the container. Lockstep: trust_mounted_repos, agent-run-lib.sh.
var attachShell = []string{"/bin/sh", "-c",
	`[ -n "${GOTMPDIR:-}" ] && mkdir -p "$GOTMPDIR" 2>/dev/null; ` +
		`command -v git >/dev/null 2>&1 && git config --global --add safe.directory '*' 2>/dev/null; ` +
		`if command -v tmux >/dev/null 2>&1; then ` + runner.TmuxAttachSh + `; ` +
		`elif command -v bash >/dev/null 2>&1; then exec bash -i; else exec /bin/sh -i; fi`}

// Attach opens a new interactive exec inside the running sandbox ref and
// returns a live PTY runner.Session, mirroring Exec's hijack style (Tty +
// AttachStdin/out/err + ExecAttach) but kept deliberately separate from the
// agent process: not registered in d.agentExecs (only the agent's own Wait
// watches that map — an interactive shell's lifecycle is the WebSocket
// attach, not the run), and closing the Session tears down only this exec
// stream (resp.Close), never the sandbox, agent, or sidecars.
//
// Security (invariant 3): the shell runs inside the already-confined sandbox,
// inheriting the same L0 structural-egress + confinement envelope as the
// agent. No new network path opens: PTY bytes flow control-plane -> dockerd ->
// container over the Docker exec hijack, never the sandbox's HTTP_PROXY egress
// path; egress and credential-mint enforcement stay at the proxy/broker
// regardless of this attach. The human principal is recorded for attribution
// (invariant 4) by the caller (the API layer), not here — the driver stays
// identity-agnostic per the parity rule.
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
		// TERM makes readline/TUIs render correctly; the image leaves it unset
		// otherwise. UTF-8 locale is required: tmux re-encodes its cell buffer
		// for the attach client, and without it transcodes unrepresentable
		// Unicode (▐▛█, ❯) to "_" (the underscores operators saw). C.UTF-8 is
		// built into glibc.
		Env: []string{
			"TERM=xterm-256color",
			"LANG=C.UTF-8",
			"LC_ALL=C.UTF-8",
		},
	}
	// Seed initial PTY size, if given, so the first output is already wrapped correctly.
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

// dockerSession is the runner.Session backed by a Docker exec TTY hijack:
// Reader is terminal output, Conn is keystroke input; Resize drives
// ExecResize, Close closes only the hijack.
type dockerSession struct {
	cli    dockerAPI
	execID string
	resp   client.HijackedResponse
}

var _ runner.Session = (*dockerSession)(nil)

// Read copies terminal output from the hijacked PTY. With Tty:true the stream
// is raw (no stdcopy multiplexing header), so bytes forward verbatim as a
// binary WebSocket frame.
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

// Close tears down only the interactive exec stream (the hijacked
// connection); the sandbox, agent process, and sidecars are untouched, so
// detaching a human leaves the run exactly as it was. Idempotent.
func (s *dockerSession) Close() error {
	s.resp.Close()
	return nil
}

// execStdin adapts a docker exec's hijacked write side to io.WriteCloser:
// Close half-closes the write side (CloseWrite) so the exec sees EOF on
// stdin while Stdout/Stderr keep flowing. It must never call resp.Close(),
// which would tear down the whole hijacked connection out from under the
// still-live output streams.
type execStdin struct {
	resp *client.HijackedResponse
}

func (s execStdin) Write(p []byte) (int, error) { return s.resp.Conn.Write(p) }
func (s execStdin) Close() error                { return s.resp.CloseWrite() }

// ExecStream launches spec.Argv inside ref as a fresh, streamable exec,
// distinct from the agent process Exec starts (tracked/observed via
// d.agentExecs) and from Attach's shell (which wraps tmux/bash; this runs
// spec.Argv directly, unwrapped).
//
// Non-TTY: stdout/stderr are demultiplexed (stdcopy) into separate streams so
// a binary protocol on stdout (SFTP, socat) is never corrupted by interleaved
// stderr bytes. TTY: the PTY merges both onto Stdout, so Stderr yields io.EOF
// immediately — present (never nil) so callers can read it uniformly.
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
	// ConsoleSize is only valid alongside TTY; mirrors Attach's guard above.
	if spec.TTY && spec.Cols > 0 && spec.Rows > 0 {
		execCfg.ConsoleSize = client.ConsoleSize{Height: uint(spec.Rows), Width: uint(spec.Cols)}
	}

	created, err := d.cli.ExecCreate(ctx, ref, execCfg)
	if err != nil {
		err = fmt.Errorf("docker: exec stream create: %w", err)
		if isNotFound(err) {
			// A finished run's container is removed a moment before its state
			// flips; callers tell that from a real fault by this sentinel.
			err = fmt.Errorf("%w: %w", runner.ErrSandboxGone, err)
		}
		return nil, err
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
		// Non-TTY: demux the stdcopy-framed stream live into a pipe pair so
		// Stdout/Stderr stream progressively instead of buffering it all first.
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
			// Also close the pipe read ends (non-TTY): resp.Close alone leaves
			// the demux goroutine and any reader parked; closing the read side
			// fails both with io.ErrClosedPipe so blocked callers (evidence
			// endpoints, SSH exec/sftp/forward bridges) unblock. TTY readers
			// aren't Closers, so the assertions no-op there.
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
