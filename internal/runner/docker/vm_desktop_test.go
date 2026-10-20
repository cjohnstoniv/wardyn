// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/vm/qemu"
)

// The desktop of a VM is QEMU's RFB socket in the run's ctl/ directory, reached from the runner; a
// container has none, and is refused rather than dialled.
func TestVMDialDesktopReachesQEMUsDisplaySocket(t *testing.T) {
	r := newVMRig(t)
	ctx := context.Background()
	spec := vmSpec()
	sb, err := r.d.CreateSandbox(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	ln := r.listen(t, filepath.Join(r.vc.runDir(spec.RunID), "ctl", qemu.VNCSocket))
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		_, _ = c.Write([]byte("RFB 003.008\n"))
		_ = c.Close()
	}()
	conn, err := r.d.DialDesktop(ctx, sb.Ref)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(conn)
	_ = conn.Close()
	if string(got) != "RFB 003.008\n" {
		t.Fatalf("desktop stream read %q, want QEMU's RFB greeting", got)
	}

	id := uuid.New()
	seedSandbox(r.f, id)
	if _, err := r.d.DialDesktop(ctx, agentContainerName(id)); !errors.Is(err, runner.ErrDesktopUnsupported) {
		t.Fatalf("a container's desktop = %v, want ErrDesktopUnsupported", err)
	}
}
