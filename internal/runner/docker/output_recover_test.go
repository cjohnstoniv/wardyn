// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/moby/moby/client"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// An exec-less agent's main-process log is re-read by a driver that never saw
// the sandbox created (a restart): followed while the container runs, read to
// its end once it has exited.
func TestRecoverOutput_ExecLessAgentLog(t *testing.T) {
	f := newFakeDocker()
	f.info = infoWithRuntimes("krun")
	f.images["busybox:latest"] = true
	spec := testSpec()
	spec.ConfinementClass = types.CC3
	d := newTestDriver(f)
	sb, err := d.CreateSandbox(context.Background(), spec)
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	if _, err := d.Exec(context.Background(), sb.Ref, []string{"agent-run", "task"}); err != nil {
		t.Fatalf("Exec (main process): %v", err)
	}
	f.mu.Lock()
	f.logs = map[string][]byte{sb.Ref: []byte("hello\r\nworld\r\n")}
	f.logOpts = nil
	f.mu.Unlock()

	restarted := newTestDriver(f) // no in-memory record of how the agent was launched
	out := &bytes.Buffer{}
	if err := restarted.RecoverOutput(context.Background(), sb.Ref, out); err != nil {
		t.Fatalf("RecoverOutput (running): %v", err)
	}
	if out.String() != "hello\r\nworld\r\n" {
		t.Fatalf("recovered %q", out.String())
	}
	if _, err := f.ContainerStop(context.Background(), sb.Ref, client.ContainerStopOptions{}); err != nil {
		t.Fatal(err)
	}
	out = &bytes.Buffer{}
	if err := restarted.RecoverOutput(context.Background(), sb.Ref, out); err != nil || out.String() != "hello\r\nworld\r\n" {
		t.Fatalf("RecoverOutput (exited) = %q, %v", out.String(), err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.logOpts) != 2 || !f.logOpts[0].Follow || f.logOpts[1].Follow || !f.logOpts[0].ShowStdout || !f.logOpts[0].ShowStderr || f.logOpts[0].Since != "" || f.logOpts[0].Tail != "" {
		t.Fatalf("log reads %+v, want the whole log twice: followed while running, not once exited", f.logOpts)
	}
}

// An exec agent's output is a hijacked stream dockerd keeps no log of, so it is
// unrecoverable, and no log read is made; a container that is gone is gone.
func TestRecoverOutput_ExecAgentIsUnrecoverable(t *testing.T) {
	f := newFakeDocker()
	f.images["busybox:latest"] = true
	d := newTestDriver(f)
	sb, err := d.CreateSandbox(context.Background(), testSpec())
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	if _, err := d.Exec(context.Background(), sb.Ref, []string{"agent-run", "task"}); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	f.mu.Lock()
	f.logOpts = nil
	f.mu.Unlock()
	out := &bytes.Buffer{}
	if err := newTestDriver(f).RecoverOutput(context.Background(), sb.Ref, out); !errors.Is(err, runner.ErrOutputUnrecoverable) {
		t.Fatalf("RecoverOutput = %v, want ErrOutputUnrecoverable", err)
	}
	if err := d.RecoverOutput(context.Background(), "no-such-container", out); !errors.Is(err, runner.ErrSandboxGone) {
		t.Fatalf("RecoverOutput of a missing container = %v, want ErrSandboxGone", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if out.String() != "" || len(f.logOpts) != 0 {
		t.Fatalf("an unrecoverable read wrote %q after %d log reads, want none", out.String(), len(f.logOpts))
	}
}
