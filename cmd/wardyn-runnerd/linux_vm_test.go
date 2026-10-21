// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package main

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner/docker"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
)

// Linux VM sandboxes are off unless the operator turns them on: no probe runs and the caps offer none.
func TestLinuxVMOffByDefault(t *testing.T) {
	for _, v := range []string{"", "0", "true", "yes"} {
		env := func(k string) string {
			if k == "WARDYN_RUNNER_LINUX_VM" {
				return v
			}
			return ""
		}
		// A nil driver: turning VMs on would probe it and panic, so reaching nil proves no probe ran.
		if caps := enableLinuxVM(context.Background(), nil, t.TempDir(), env); caps != nil {
			t.Fatalf("WARDYN_RUNNER_LINUX_VM=%q offered %+v", v, caps)
		}
	}
}

// The caps say whether the probe found virtiofsd: the organisation sends a VM run that carries a
// folder only to a runner that says so.
func TestVMCapsCarryTheFolderShareProbe(t *testing.T) {
	for _, found := range []bool{true, false} {
		c := docker.VMConfig{MachineType: "pc-q35-10.0", CPUModel: "Haswell-v4", QEMUVersion: "10.0.13", Virtiofs: found}
		if caps := vmCaps(c); caps.Virtiofs != found || caps.MachineType != c.MachineType {
			t.Fatalf("probe found virtiofsd %v, caps %+v", found, caps)
		}
	}
}

// waitFullPass waits for the background full-spool pass a tick started to finish.
func waitFullPass(t *testing.T, d *dockerSink) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); d.fullBusy.Load(); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the full-spool pass did not finish")
		}
	}
}

// The full-spool rule is behind the Linux VM switch: a runner whose caps offer no VM never ends a
// run for a full spool, and the containment tick drives the rule on one that does.
func TestFullSpoolRuleFollowsTheLinuxVMSwitch(t *testing.T) {
	for _, tc := range []struct {
		name string
		caps *runnerwire.VMCaps
		want int
	}{{"VMs off", nil, 0}, {"VMs on", &runnerwire.VMCaps{}, 1}} {
		t.Run(tc.name, func(t *testing.T) {
			h := newSinkHarness(t)
			e := &endCalls{isVM: true}
			endVMsOnFullSpool(h.sink, tc.caps, e.end)
			h.fillSpool(false)
			d := &dockerSink{spoolSink: h.sink}
			d.extras(nil, nil).options.Tick(t.Context())
			waitFullPass(t, d)
			e.mu.Lock()
			defer e.mu.Unlock()
			if len(e.runs) != tc.want {
				t.Fatalf("a tick over a full spool ended %d runs, want %d", len(e.runs), tc.want)
			}
		})
	}
}

// Ending a VM takes as long as the guest's power down, and the containment tick shares one safety
// context with the lapse check that follows it: the tick returns while the pass is still running,
// and a second tick does not start a second pass.
func TestFullSpoolPassDoesNotHoldTheTick(t *testing.T) {
	h := newSinkHarness(t)
	e := &endCalls{isVM: true, hold: make(chan struct{})}
	endVMsOnFullSpool(h.sink, &runnerwire.VMCaps{}, e.end)
	h.fillSpool(false)
	d := &dockerSink{spoolSink: h.sink}
	tick := d.extras(nil, nil).options.Tick
	returned := make(chan struct{})
	go func() { tick(t.Context()); tick(t.Context()); close(returned) }()
	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		close(e.hold)
		t.Fatal("the containment tick waited for a VM to end")
	}
	close(e.hold)
	waitFullPass(t, d)
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.runs) != 1 {
		t.Fatalf("two ticks ended %d runs, want one pass", len(e.runs))
	}
}

// ctxEnds is an end that, like the real one, works until its context ends: it blocks until cancelled,
// takes a moment to unwind, and records both. release lets a test that never cancels it finish.
type ctxEnds struct {
	mu               sync.Mutex
	sawCancel, wound bool
	release          chan struct{}
}

func (c *ctxEnds) end(ctx context.Context, _ uuid.UUID) (bool, error) {
	select {
	case <-ctx.Done():
		c.mu.Lock()
		c.sawCancel = true
		c.mu.Unlock()
		time.Sleep(100 * time.Millisecond) // the guest's power down being abandoned
		c.mu.Lock()
		c.wound = true
		c.mu.Unlock()
		return false, ctx.Err()
	case <-c.release:
		return false, errors.New("released")
	}
}

// Close joins a running full-spool pass, then cancels one that outlives closeJoinBound and waits for
// it to return, so the runner never exits, or closes the sink, with a goroutine still ending a VM. No
// pass starts once Close has begun.
func TestDockerSinkCloseJoinsTheFullSpoolPass(t *testing.T) {
	prevJoin, prevUnwind := closeJoinBound, cancelUnwindBound
	closeJoinBound, cancelUnwindBound = 300*time.Millisecond, 2*time.Second
	t.Cleanup(func() { closeJoinBound, cancelUnwindBound = prevJoin, prevUnwind })
	start := func(t *testing.T, end func(context.Context, uuid.UUID) (bool, error)) (*dockerSink, func(context.Context)) {
		h := newSinkHarness(t)
		endVMsOnFullSpool(h.sink, &runnerwire.VMCaps{}, end)
		h.fillSpool(false)
		d := &dockerSink{spoolSink: h.sink}
		tick := d.extras(nil, nil).options.Tick
		tick(t.Context())
		return d, tick
	}
	closed := func(d *dockerSink) chan struct{} {
		done := make(chan struct{})
		go func() { _ = d.Close(); close(done) }()
		return done
	}
	t.Run("waits for the pass", func(t *testing.T) {
		e := &endCalls{isVM: true, hold: make(chan struct{})}
		d, _ := start(t, e.end)
		done := closed(d)
		select {
		case <-done:
			t.Fatal("Close returned while a full-spool pass was still ending a VM")
		case <-time.After(100 * time.Millisecond):
		}
		close(e.hold)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("Close did not return once the pass finished")
		}
		e.mu.Lock()
		defer e.mu.Unlock()
		if len(e.runs) != 1 {
			t.Fatalf("the pass ended %d VMs, want 1", len(e.runs))
		}
	})
	t.Run("cancels a pass that outlives the join bound and waits for it to unwind", func(t *testing.T) {
		c := &ctxEnds{release: make(chan struct{})}
		t.Cleanup(func() { close(c.release) })
		d, _ := start(t, c.end)
		select {
		case <-closed(d):
		case <-time.After(10 * time.Second):
			t.Fatal("Close never returned")
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		if !c.sawCancel {
			t.Fatal("Close returned and the pass never saw its context cancelled")
		}
		if !c.wound {
			t.Fatal("Close returned before the cancelled pass had returned")
		}
	})
	t.Run("no pass starts once Close has begun", func(t *testing.T) {
		h := newSinkHarness(t)
		e := &endCalls{isVM: true, fails: 100} // the run stays unhandled: a new pass would end it again
		endVMsOnFullSpool(h.sink, &runnerwire.VMCaps{}, e.end)
		h.fillSpool(false)
		d := &dockerSink{spoolSink: h.sink}
		tick := d.extras(nil, nil).options.Tick
		tick(t.Context())
		d.fullPasses.Wait() // the first pass has finished
		// Close's first step, with the sink left open: Close would also drop the registered runs, and a
		// pass started after it would find nothing to end, hiding the pass this asserts is absent.
		d.joinFullPass()
		tick(t.Context())
		d.fullPasses.Wait()
		e.mu.Lock()
		defer e.mu.Unlock()
		if len(e.runs) != 1 {
			t.Fatalf("%d end calls: a pass started after Close, want only the first", len(e.runs))
		}
	})
}

// Without the Linux VM switch the full-spool path does not exist: a tick starts no pass (no goroutine,
// no budget), and Close neither joins nor cancels anything.
func TestNoFullSpoolPathWithoutTheLinuxVMSwitch(t *testing.T) {
	prev := closeJoinBound
	closeJoinBound = time.Hour // a Close that entered the join would hang here
	t.Cleanup(func() { closeJoinBound = prev })
	h := newSinkHarness(t)
	e := &endCalls{isVM: true}
	endVMsOnFullSpool(h.sink, nil, e.end) // VMs off
	h.fillSpool(false)
	d := &dockerSink{spoolSink: h.sink}
	d.extras(nil, nil).options.Tick(t.Context())
	d.fullMu.Lock()
	started := d.fullCancel != nil
	d.fullMu.Unlock()
	if started || d.fullBusy.Load() {
		t.Fatal("a tick started a full-spool pass on a runner without Linux VM sandboxes")
	}
	done := make(chan struct{})
	go func() { _ = d.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close waited on a full-spool pass on a runner without Linux VM sandboxes")
	}
	d.fullMu.Lock()
	defer d.fullMu.Unlock()
	if d.fullClosed {
		t.Fatal("Close entered the full-spool join on a runner without Linux VM sandboxes")
	}
}
