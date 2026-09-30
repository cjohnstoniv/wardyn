// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// lockedBuffer is a SandboxSpec.ExecOutput the drain goroutine and the test
// can share.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) waitFor(t *testing.T, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		b.mu.Lock()
		got := b.buf.String()
		b.mu.Unlock()
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("exec output = %q, want %q", got, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestExec_TeesAgentOutputIntoExecOutput: the agent exec's PTY stream, which
// Exec already drained, now lands in SandboxSpec.ExecOutput; teardown forgets
// the writer.
func TestExec_TeesAgentOutputIntoExecOutput(t *testing.T) {
	f := newFakeDocker()
	f.images["busybox:latest"] = true
	f.execAttachConn = &bufConn{r: bytes.NewReader([]byte("go test ./...\r\nok\r\n"))}
	d := newTestDriver(f)
	out := &lockedBuffer{}
	spec := testSpec()
	spec.ExecOutput = out

	sb, err := d.CreateSandbox(context.Background(), spec)
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	if _, err := d.Exec(context.Background(), sb.Ref, []string{"agent-run", "go test ./..."}); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	out.waitFor(t, "go test ./...\r\nok\r\n")

	if err := d.KillSandbox(context.Background(), sb.Ref); err != nil {
		t.Fatalf("KillSandbox: %v", err)
	}
	if _, ok := d.execOutputs.Load(sb.Ref); ok {
		t.Fatal("teardown kept the sandbox's exec output writer")
	}
}

// TestExecLess_FollowsMainProcessLogIntoExecOutput: on an exec-less runtime
// the workload is the container's main process, so its output is dockerd's
// log of that container, followed into SandboxSpec.ExecOutput.
func TestExecLess_FollowsMainProcessLogIntoExecOutput(t *testing.T) {
	f := newFakeDocker()
	f.info = infoWithRuntimes("krun")
	f.images["busybox:latest"] = true
	d := newTestDriver(f)
	out := &lockedBuffer{}
	spec := testSpec()
	spec.ConfinementClass = types.CC3
	spec.ExecOutput = out

	sb, err := d.CreateSandbox(context.Background(), spec)
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	f.mu.Lock()
	f.logs = map[string][]byte{sb.Ref: []byte("main process says hi\r\n")}
	f.mu.Unlock()
	if _, err := d.Exec(context.Background(), sb.Ref, []string{"agent-run", "task"}); err != nil {
		t.Fatalf("Exec (main process): %v", err)
	}
	out.waitFor(t, "main process says hi\r\n")
}
