// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"context"
	"fmt"

	dockertypes "github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"

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
var attachShell = []string{"/bin/sh", "-c", "if command -v tmux >/dev/null 2>&1; then exec tmux new-session -A -s wardyn bash; elif command -v bash >/dev/null 2>&1; then exec bash -i; else exec /bin/sh -i; fi"}

// Attach opens a NEW interactive exec (an interactive shell) inside the running
// sandbox ref and returns a live PTY runner.Session. It mirrors Exec's
// interactive-style hijack (Tty + AttachStdin/out/err + ContainerExecAttach)
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

	execCfg := container.ExecOptions{
		Tty:          true,
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
	// Seed the initial PTY size when the client supplied one; ContainerExecCreate
	// accepts ConsoleSize so the very first output is already correctly wrapped.
	if opts.Cols > 0 && opts.Rows > 0 {
		execCfg.ConsoleSize = &[2]uint{uint(opts.Rows), uint(opts.Cols)}
	}

	created, err := d.cli.ContainerExecCreate(ctx, ref, execCfg)
	if err != nil {
		return nil, fmt.Errorf("docker: attach exec create: %w", err)
	}

	// TTY hijack: resp.Reader is the PTY output, resp.Conn is the input writer.
	resp, err := d.cli.ContainerExecAttach(ctx, created.ID, container.ExecAttachOptions{Tty: true})
	if err != nil {
		return nil, fmt.Errorf("docker: attach exec attach: %w", err)
	}

	return &dockerSession{
		cli:    d.cli,
		execID: created.ID,
		resp:   resp,
	}, nil
}

// dockerSession is the runner.Session backed by a Docker exec TTY hijack. The
// hijacked response carries the bidirectional PTY: Reader is terminal output,
// Conn is keystroke input. Resize drives ContainerExecResize; Close closes the
// hijack (and only the hijack).
type dockerSession struct {
	cli    dockerAPI
	execID string
	resp   dockertypes.HijackedResponse
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

// Resize informs the exec PTY of a new window size. Docker's ResizeOptions takes
// Height (rows) and Width (cols).
func (s *dockerSession) Resize(ctx context.Context, cols, rows uint16) error {
	if cols == 0 || rows == 0 {
		return nil // ignore degenerate sizes rather than erroring the stream
	}
	if err := s.cli.ContainerExecResize(ctx, s.execID, container.ResizeOptions{
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
