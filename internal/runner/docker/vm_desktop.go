// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"context"
	"fmt"
	"io"
	"path/filepath"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/vm/qemu"
)

// DialDesktop implements runner.DesktopDialer: a connection to the VM's display, QEMU's RFB server
// on the ctl/ socket of its run directory. A container has no such display (its desktop, when it has
// one, is a UI app inside the sandbox), so it is refused.
func (d *Driver) DialDesktop(ctx context.Context, ref string) (io.ReadWriteCloser, error) {
	v := d.vmFor(ctx, ref)
	if v == nil {
		return nil, runner.ErrDesktopUnsupported
	}
	sock := filepath.Join(v.dir, "ctl", qemu.VNCSocket)
	if v.mac {
		if macDesktopOff(v.dir) {
			return nil, errMacDesktopOff
		}
		c := d.macVMConfig()
		if c == nil {
			return nil, runner.ErrDesktopUnsupported
		}
		var err error
		if sock, err = c.sockPath(v.runID, macvm.DesktopSock); err != nil {
			return nil, fmt.Errorf("docker: VM desktop: %w", err)
		}
	}
	c, err := d.dialUnix(ctx, sock)
	if err != nil {
		return nil, fmt.Errorf("docker: VM desktop: %w", err)
	}
	return c, nil
}

// PauseAgent implements runner.AgentPauser: the guest agent stops the agent's process group, and the
// guest and its display keep running. A container has no guest agent, so it is refused. fenced is the
// guest agent's own answer: false from a guest image that predates the generation.
func (d *Driver) PauseAgent(ctx context.Context, ref string, gen int64) (bool, error) {
	v := d.vmFor(ctx, ref)
	if v == nil {
		return false, runner.ErrAgentPauseUnsupported
	}
	fenced, err := v.agent.PauseAgent(ctx, gen)
	if err != nil {
		return false, fmt.Errorf("docker: pause the VM's agent: %w", err)
	}
	return fenced, nil
}

// ResumeAgent implements runner.AgentPauser: the guest agent continues the agent's process group.
func (d *Driver) ResumeAgent(ctx context.Context, ref string, gen int64) (bool, error) {
	v := d.vmFor(ctx, ref)
	if v == nil {
		return false, runner.ErrAgentPauseUnsupported
	}
	fenced, err := v.agent.ResumeAgent(ctx, gen)
	if err != nil {
		return false, fmt.Errorf("docker: resume the VM's agent: %w", err)
	}
	return fenced, nil
}
