// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
	"github.com/cjohnstoniv/wardyn/internal/macvm/admission"
	"github.com/cjohnstoniv/wardyn/internal/macvm/macvmtest"
	"github.com/cjohnstoniv/wardyn/internal/version"
)

// Kept equal to cmd/wardyn-macvm's runnerdJobEnv and runnerdJobPushed (runnerd_job_test.go).
const (
	helperJobEnv    = "WARDYN_TEST_RUNNERD_JOB"
	helperJobPushed = "runnerd job: stage 2 pushed the guest agent beside the helper"
)

// R3 M1, real to real: a base whose pins are current but whose guest agent is another build gets the
// tools update runnerd starts for agent drift, on runnerd's own argv (startToolsUpdate, no --force),
// read by the helper's own update-tools: cmd/wardyn-macvm's test binary is the helper here, so its flag
// parse, its NeedsToolsUpdate and its stage 2 are the shipped ones, on a fake engine. Stage 2 pushes the
// helper's guest agent and the base records that build.
func TestMacOSVMAgentDriftUpdatesWithoutForce(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "wardyn-macvm.test")
	if out, err := exec.Command("go", "test", "-c", "-o", bin, "../wardyn-macvm").CombinedOutput(); err != nil {
		t.Fatalf("build the helper's test binary: %v\n%s", err, out)
	}
	helper := filepath.Join(filepath.Dir(bin), "wardyn-macvm")
	script := "#!/bin/sh\nexec " + bin + " -test.run '^TestUpdateToolsAsRunnerdStartsIt$' -test.count=1 -- \"$@\"\n"
	if err := os.WriteFile(helper, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv(helperJobEnv, "1")

	r := newMacLedgerRig(t)
	r.host.setProcs(admission.Proc{PID: os.Getpid(), UID: 501, Start: 3, Name: "wardyn-runnerd"})
	m := r.open(t)
	m.helper = helper
	nets := &prepNets{t: t}
	nets.use(m, macvmtest.SockRoot(t))
	base := m.layout.BaseDir(0)
	for n, b := range map[string]string{macvm.DiskFile: "d", macvm.AuxFile: "a", macvm.MachineIDFile: "base-id",
		macvm.ManifestFile: `{"format":1,"state":"ready","hardware_model":"aHc=","min_cpus":2,"min_memory_mib":4096,"desktop":"control","helper_version":"old","agent_version":"0.0.0-old"}`} {
		if err := os.WriteFile(filepath.Join(base, n), []byte(b), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	tooling, err := macvm.ShippedTooling()
	if err != nil {
		t.Fatal(err)
	}
	rec := macvm.ToolsRecord{AgentVersion: "0.0.0-old", Pins: map[string]macvm.PinRecord{}, Harnesses: []string{macvm.HarnessNone}, TriedAt: time.Now().UTC()}
	for _, p := range tooling.Harnesses {
		rec.Pins[p.ID] = macvm.PinRecord{Version: p.Version, SHA256: p.SHA256}
	}
	b, _ := json.Marshal(rec)
	if err := os.WriteFile(filepath.Join(base, macvm.ToolsRecordFile), b, 0o600); err != nil {
		t.Fatal(err)
	}
	if macvm.NeedsToolsUpdate(base, tooling, "", time.Now()) {
		t.Fatal("the base's pins are not current: only the agent should drift")
	}
	man, err := macvm.ReadManifest(base)
	if err != nil {
		t.Fatal(err)
	}

	defer serving(m)()
	if err := m.startToolsUpdate(context.Background(), man); err != nil {
		t.Fatal(err)
	}
	counted, _ := m.ledger.Counted()
	if len(counted) != 1 || counted[0].HelperPID == 0 {
		t.Fatalf("the helper did not claim its entry: %+v", counted)
	}
	// The helper's own fake host starts every process at 7.
	r.host.setProcs(admission.Proc{PID: os.Getpid(), UID: 501, Start: 3, Name: "wardyn-runnerd"},
		admission.Proc{PID: counted[0].HelperPID, UID: 501, Start: 7, Name: admission.HelperProcess})
	m.recompute() // hands the job its stage-2 network

	logPath := filepath.Join(m.layout.Root, toolsUpdateLog)
	var log string
	for deadline := time.Now().Add(120 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		b, _ := os.ReadFile(logPath)
		if log = string(b); strings.Contains(log, "\nPASS\n") || strings.Contains(log, "FAIL") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the helper never finished:\n%s", log)
		}
	}
	if !strings.Contains(log, helperJobPushed) || !strings.Contains(log, "\nPASS\n") {
		t.Fatalf("the helper's update-tools on runnerd's argv:\n%s", log)
	}
	if got, err := macvm.ReadyBase(base); err != nil || got.AgentVersion != version.String() {
		t.Fatalf("the base after the update: agent %q (%v), want the helper's %s", got.AgentVersion, err, version.String())
	}
}
