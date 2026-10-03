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

// drainSpyBuffer is an ExecOutput that also implements runner.OutputDrainer, so
// a test sees when the driver opens and ends its copy.
type drainSpyBuffer struct {
	lockedBuffer
	dmu          sync.Mutex
	begun, ended int
	endErr       error
}

func (d *drainSpyBuffer) BeginDrain() { d.dmu.Lock(); d.begun++; d.dmu.Unlock() }
func (d *drainSpyBuffer) EndDrain(err error) {
	d.dmu.Lock()
	d.ended, d.endErr = d.ended+1, err
	d.dmu.Unlock()
}
func (d *drainSpyBuffer) counts() (begun, ended int, err error) {
	d.dmu.Lock()
	defer d.dmu.Unlock()
	return d.begun, d.ended, d.endErr
}

// TestExec_ReportsTheDrainToTheOutputWriter: the copy is opened before Exec
// returns, so a caller that waits for the process then finds it open, and it is
// ended exactly once, cleanly, when the stream reaches EOF.
func TestExec_ReportsTheDrainToTheOutputWriter(t *testing.T) {
	f := newFakeDocker()
	f.images["busybox:latest"] = true
	f.execAttachConn = &bufConn{r: bytes.NewReader([]byte("done\r\n"))}
	d := newTestDriver(f)
	out := &drainSpyBuffer{}
	spec := testSpec()
	spec.ExecOutput = out
	sb, err := d.CreateSandbox(context.Background(), spec)
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	if _, err := d.Exec(context.Background(), sb.Ref, []string{"agent-run", "task"}); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if begun, _, _ := out.counts(); begun != 1 {
		t.Fatalf("drains begun when Exec returned = %d, want 1", begun)
	}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		if _, ended, err := out.counts(); ended == 1 {
			if err != nil {
				t.Fatalf("the drain ended with %v, want a clean EOF", err)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the drain never ended")
		}
	}
}

// TestExecLess_ReportsTheMainProcessDrain is the same for the exec-less path:
// the log follow is a drain, opened when it starts and ended when it stops.
func TestExecLess_ReportsTheMainProcessDrain(t *testing.T) {
	f := newFakeDocker()
	f.info = infoWithRuntimes("krun")
	f.images["busybox:latest"] = true
	d := newTestDriver(f)
	out := &drainSpyBuffer{}
	spec := testSpec()
	spec.ConfinementClass = types.CC3
	spec.ExecOutput = out
	sb, err := d.CreateSandbox(context.Background(), spec)
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	f.mu.Lock()
	f.logs = map[string][]byte{sb.Ref: []byte("hi\r\n")}
	f.mu.Unlock()
	if _, err := d.Exec(context.Background(), sb.Ref, []string{"agent-run", "task"}); err != nil {
		t.Fatalf("Exec (main process): %v", err)
	}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		begun, ended, err := out.counts()
		if begun == 1 && ended == 1 && err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("drains begun %d ended %d err %v, want one clean drain", begun, ended, err)
		}
	}
}
