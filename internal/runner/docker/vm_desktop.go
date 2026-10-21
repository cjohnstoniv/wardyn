// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"context"
	"fmt"
	"io"
	"path/filepath"

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
	c, err := d.dialUnix(ctx, filepath.Join(v.dir, "ctl", qemu.VNCSocket))
	if err != nil {
		return nil, fmt.Errorf("docker: VM desktop: %w", err)
	}
	return c, nil
}
