// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
)

func writeToolsUpdateLog(t *testing.T, root string, evs ...macvm.PrepareEvent) {
	t.Helper()
	var b bytes.Buffer
	for _, ev := range evs {
		l, _ := json.Marshal(ev)
		b.Write(append(l, '\n'))
	}
	if err := os.WriteFile(filepath.Join(root, toolsUpdateLog), b.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

// holdUpdateLock holds the base's update lock as a running tools update does, until the test ends.
func holdUpdateLock(t *testing.T, base string) {
	t.Helper()
	f, err := os.OpenFile(base+updateLockSuffix, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
}

// A tools update waits for AC power only while its log's last event is the paused tools phase and the update
// lock is held: an unpaused event after it, a lock nobody holds (the helper died) and no log say it does not.
func TestToolsUpdateWaitingForAC(t *testing.T) {
	root := t.TempDir()
	base := macvm.Layout{Root: root}.BaseDir(0)
	if err := os.MkdirAll(filepath.Dir(base), 0o700); err != nil {
		t.Fatal(err)
	}
	paused := macvm.PrepareEvent{Phase: macvm.PhaseTools, Paused: true, Detail: "waiting for AC power"}
	if toolsUpdateWaiting(root) {
		t.Fatal("waiting with no log")
	}
	writeToolsUpdateLog(t, root, paused)
	if toolsUpdateWaiting(root) {
		t.Fatal("waiting although no update holds the lock")
	}
	holdUpdateLock(t, base)
	if !toolsUpdateWaiting(root) {
		t.Fatal("not waiting: paused tools event, lock held")
	}
	writeToolsUpdateLog(t, root, paused, macvm.PrepareEvent{Phase: macvm.PhaseTools})
	if toolsUpdateWaiting(root) {
		t.Fatal("waiting after the update resumed")
	}
	writeToolsUpdateLog(t, root, macvm.PrepareEvent{Phase: macvm.PhaseDownload, Paused: true})
	if toolsUpdateWaiting(root) {
		t.Fatal("a paused download counted as a tools update")
	}
}

// status prints the line, and the JSON carries it, only while a tools update waits.
func TestStatusShowsToolsUpdateWaitingForAC(t *testing.T) {
	var out bytes.Buffer
	macOSVMStatus{State: runnerwire.MacVMBaseReady, ToolsUpdateWaiting: true}.writeText(&out)
	if !strings.Contains(out.String(), "tools update waiting for AC power\n") {
		t.Fatalf("status text: %q", out.String())
	}
	out.Reset()
	macOSVMStatus{State: runnerwire.MacVMBaseReady}.writeText(&out)
	if strings.Contains(out.String(), "AC power") {
		t.Fatalf("status text without a waiting update: %q", out.String())
	}
	b, _ := json.Marshal(macOSVMStatus{ToolsUpdateWaiting: true})
	if !strings.Contains(string(b), `"tools_update_waiting_for_ac":true`) {
		t.Fatalf("status json: %s", b)
	}
}

// doctor adds the line to the guest-agent stage while a tools update waits, and not otherwise.
func TestDoctorShowsToolsUpdateWaitingForAC(t *testing.T) {
	tools := macvm.ToolsRecord{AgentSignature: macvm.AgentSignedThisMac}
	st := macOSVMStagesFor(t, nil, tools, nil)
	for _, s := range st {
		if strings.Contains(s.Message(), "AC power") {
			t.Fatalf("a stage mentions AC power with no update waiting: %+v", s)
		}
	}
	root, err := macvm.DefaultRoot()
	if err != nil {
		t.Fatal(err)
	}
	writeToolsUpdateLog(t, root, macvm.PrepareEvent{Phase: macvm.PhaseTools, Paused: true, Detail: "waiting for AC power"})
	holdUpdateLock(t, macvm.Layout{Root: root}.BaseDir(0))
	var got string
	for _, s := range macOSVMDoctorStages(nil, "") {
		if s.Name == "guest-agent" {
			got = s.Message()
		}
	}
	if !strings.Contains(got, "; warning: tools update waiting for AC power") {
		t.Fatalf("guest-agent stage: %q", got)
	}
}
