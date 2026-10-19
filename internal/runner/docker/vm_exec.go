// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/internal/vm/guestagent"
)

// A VM ref's execs go through the Wardyn guest agent, never `docker exec`: the QEMU container holds
// QEMU, not the sandbox. The agent's process is a detached guest exec that outlives a runner
// restart; its output follows into the run's exec output as a container exec's does.

// vmExec starts argv as the VM's agent process.
func (d *Driver) vmExec(ctx context.Context, v *vmRun, ref string, argv []string, out io.Writer) (string, error) {
	id, err := v.agent.Start(ctx, guestagent.ExecRequest{Argv: argv, TTY: true})
	if err != nil {
		return "", fmt.Errorf("docker: VM exec: %w", err)
	}
	d.mu.Lock()
	d.agentExecs[ref] = id
	d.mu.Unlock()
	w := out
	if w == nil {
		w = d.execOutput(ref)
	}
	endDrain := runner.BeginOutputDrain(w)
	go func() { endDrain(d.followVMOutput(v, id, w)) }()
	return id, nil
}

// followVMOutput copies a detached exec's output to w until it exits, picking the stream up again
// at the offset it reached when the connection to the guest drops; teardown ends it.
func (d *Driver) followVMOutput(v *vmRun, id string, w io.Writer) error {
	var offset int64
	for {
		rc, err := v.agent.Output(v.ctx, id, offset)
		if err == nil {
			n, cerr := io.Copy(w, rc)
			_ = rc.Close()
			offset += n
			if cerr == nil {
				return nil // the exec exited and its output is all here
			}
			err = cerr
		}
		if errors.Is(err, guestagent.ErrNoExec) {
			return err
		}
		select {
		case <-v.ctx.Done():
			return v.ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// vmWait waits for the VM's agent process (the latest exec, or the guest's own record of it after
// a runner restart) and returns its exit code.
func (d *Driver) vmWait(ctx context.Context, v *vmRun, ref string) (int, error) {
	d.mu.Lock()
	id := d.agentExecs[ref]
	d.mu.Unlock()
	errs := 0
	for {
		var st guestagent.ExecState
		var err error
		if id == "" {
			st, err = v.agent.Agent(ctx)
		} else {
			st, err = v.agent.State(ctx, id)
		}
		switch {
		case errors.Is(err, guestagent.ErrNoExec):
			return 0, fmt.Errorf("docker: wait: the VM has no agent process for ref %q", ref)
		case err != nil:
			// The guest unreachable is an exit only when the VM itself is gone.
			if s, serr := d.Status(ctx, ref); serr == nil && s.State != types.RunRunning {
				return 0, fmt.Errorf("docker: wait: the VM stopped: %s", s.Message)
			}
			if errs++; errs > waitMaxProbeErrors || ctx.Err() != nil {
				return 0, fmt.Errorf("docker: wait: %w", err)
			}
		case !st.Running && st.ExitCode != nil:
			return *st.ExitCode, nil
		default:
			errs = 0
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}

// vmAgentStatus is AgentStatus for a VM: the QEMU container's state first (a stopped VM is a
// stopped agent), then the guest's record of the exec.
func (d *Driver) vmAgentStatus(ctx context.Context, v *vmRun, ref, execID string) (runner.Status, error) {
	cst, err := d.Status(ctx, ref)
	if err != nil || cst.State != types.RunRunning || execID == "" || execID == mainProcessExecID {
		return cst, err
	}
	st, err := v.agent.State(ctx, execID)
	switch {
	case errors.Is(err, guestagent.ErrNoExec):
		return runner.Status{State: types.RunStopped, Message: "the agent process is not in the VM (the guest restarted?)"}, nil
	case err != nil:
		return runner.Status{}, fmt.Errorf("docker: the VM's guest agent: %w", err)
	case st.Running || st.ExitCode == nil:
		return runner.Status{State: types.RunRunning}, nil
	}
	return runner.Status{State: types.RunStopped, ExitCode: st.ExitCode}, nil
}

// vmShell is the terminal a VM's attach opens: the agent user's login shell.
var vmShell = []string{"/bin/sh", "-c", "if command -v bash >/dev/null; then exec bash -l; fi; exec sh -l"}

// vmAttach is Attach for a VM: an attached guest exec of a login shell with a pty.
func (d *Driver) vmAttach(ctx context.Context, v *vmRun, opts runner.AttachOptions) (runner.Session, error) {
	if opts.Observer {
		// No shared terminal to watch or take over in this release: each attach is its own shell.
		return nil, fmt.Errorf("%w: watching a terminal", errVMUnsupported)
	}
	s, err := v.agent.Exec(ctx, guestagent.ExecRequest{Argv: vmShell, TTY: true, Cols: opts.Cols, Rows: opts.Rows})
	if err != nil {
		return nil, fmt.Errorf("docker: VM attach: %w", err)
	}
	return vmSession{s}, nil
}

type vmSession struct{ s *guestagent.Session }

func (v vmSession) Read(p []byte) (int, error)  { return v.s.Stdout.Read(p) }
func (v vmSession) Write(p []byte) (int, error) { return v.s.Stdin.Write(p) }
func (v vmSession) Resize(_ context.Context, cols, rows uint16) error {
	return v.s.Resize(cols, rows)
}
func (v vmSession) Close() error { return v.s.Close() }

// vmExecStream is ExecStream for a VM: one attached guest exec.
func (d *Driver) vmExecStream(ctx context.Context, v *vmRun, spec runner.ExecSpec) (*runner.ExecSession, error) {
	if len(spec.Argv) == 0 {
		return nil, errors.New("docker: exec stream: empty argv")
	}
	s, err := v.agent.Exec(ctx, guestagent.ExecRequest{Argv: spec.Argv, Env: spec.Env, TTY: spec.TTY, Cols: spec.Cols, Rows: spec.Rows})
	if err != nil {
		return nil, fmt.Errorf("docker: VM exec stream: %w", err)
	}
	resize := func(cols, rows uint16) error {
		if !spec.TTY {
			return nil
		}
		return s.Resize(cols, rows)
	}
	return &runner.ExecSession{Stdin: s.Stdin, Stdout: s.Stdout, Stderr: s.Stderr, Resize: resize, Wait: s.Wait, Close: s.Close}, nil
}
