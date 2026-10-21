// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package main

import (
	"context"
	"testing"
	"time"

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

// Close joins a running full-spool pass, so the runner never exits with a goroutine still ending a VM,
// and gives up on a pass that outlives its own budget. No pass starts once Close has begun.
func TestDockerSinkCloseJoinsTheFullSpoolPass(t *testing.T) {
	prev := fullSpoolBound
	fullSpoolBound = 300 * time.Millisecond
	t.Cleanup(func() { fullSpoolBound = prev })
	start := func(t *testing.T) (*dockerSink, *endCalls, func(context.Context)) {
		h := newSinkHarness(t)
		e := &endCalls{isVM: true, hold: make(chan struct{})}
		endVMsOnFullSpool(h.sink, &runnerwire.VMCaps{}, e.end)
		h.fillSpool(false)
		d := &dockerSink{spoolSink: h.sink}
		tick := d.extras(nil, nil).options.Tick
		tick(t.Context())
		return d, e, tick
	}
	closed := func(d *dockerSink) chan struct{} {
		done := make(chan struct{})
		go func() { _ = d.Close(); close(done) }()
		return done
	}
	t.Run("waits for the pass", func(t *testing.T) {
		d, e, tick := start(t)
		done := closed(d)
		select {
		case <-done:
			t.Fatal("Close returned while a full-spool pass was still ending a VM")
		case <-time.After(100 * time.Millisecond):
		}
		tick(t.Context()) // after Close began: starts nothing
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
	t.Run("gives up at the pass's budget", func(t *testing.T) {
		d, e, _ := start(t)
		t.Cleanup(func() { close(e.hold) })
		select {
		case <-closed(d):
		case <-time.After(5 * time.Second):
			t.Fatal("Close waited past the pass's budget")
		}
	})
}
