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
	"github.com/cjohnstoniv/wardyn/internal/vm/guestagent"
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

// The run's sign-in choice reaches the guest at Init, and what the guest made of its sign-in store
// comes back on the sandbox (#2972).
func TestVMSignInStoreRidesInitAndComesBack(t *testing.T) {
	for _, off := range []bool{false, true} {
		r := newVMRig(t)
		spec := vmSpec()
		spec.DesktopSignInsOff = off
		sb, err := r.d.CreateSandbox(context.Background(), spec)
		if err != nil {
			t.Fatal(err)
		}
		r.mu.Lock()
		got := r.init.SignInsOff
		r.mu.Unlock()
		want := map[bool]string{false: guestagent.SignInsMemory, true: guestagent.SignInsNone}[off]
		if got != off || sb.SignIns != want {
			t.Fatalf("off=%v: init carried %v, sandbox says %q; want %q", off, got, sb.SignIns, want)
		}
	}
}

// Pause and resume reach the guest agent of a VM's running agent; a container is refused.
func TestVMPauseAgentReachesTheGuestAgent(t *testing.T) {
	r := newVMRig(t)
	ctx := context.Background()
	sb, err := r.d.CreateSandbox(ctx, vmSpec())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.d.PauseAgent(ctx, sb.Ref, 1); !errors.Is(err, guestagent.ErrNoExec) {
		t.Fatalf("pause before the agent started = %v, want ErrNoExec", err)
	}
	if _, err := r.d.Exec(ctx, sb.Ref, []string{"sleep", "5"}); err != nil {
		t.Fatal(err)
	}
	if fenced, err := r.d.PauseAgent(ctx, sb.Ref, 1); err != nil || !fenced {
		t.Fatalf("pause = fenced %v, %v; want the guest agent's ordered answer", fenced, err)
	}
	if fenced, err := r.d.ResumeAgent(ctx, sb.Ref, 1); err != nil || !fenced {
		t.Fatalf("resume = fenced %v, %v; want the guest agent's ordered answer", fenced, err)
	}
	id := uuid.New()
	seedSandbox(r.f, id)
	for _, f := range []func(context.Context, string, int64) (bool, error){r.d.PauseAgent, r.d.ResumeAgent} {
		if _, err := f(ctx, agentContainerName(id), 1); !errors.Is(err, runner.ErrAgentPauseUnsupported) {
			t.Fatalf("a container's agent = %v, want ErrAgentPauseUnsupported", err)
		}
	}
}
