// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
	"github.com/cjohnstoniv/wardyn/internal/macvm/admission"
	"github.com/cjohnstoniv/wardyn/internal/macvm/macvmtest"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
)

// ev is one prepare.log event line.
func ev(t *testing.T, e macvm.PrepareEvent) string {
	t.Helper()
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// writeLog replaces prepare.log with lines.
func writeLog(t *testing.T, m *macOSVMs, lines ...string) {
	t.Helper()
	if err := os.WriteFile(m.layout.PrepareLog(), []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// preparingRig is a runner with a live prepare and no base: its caps' base is preparing.
func preparingRig(t *testing.T) (*macLedgerRig, *macOSVMs) {
	t.Helper()
	r := newMacLedgerRig(t)
	r.host.setProcs(admission.Proc{PID: 30, UID: 501, Start: 3, Name: "wardyn-runnerd"})
	m := r.open(t)
	if err := os.Remove(filepath.Join(m.layout.BaseDir(0), macvm.ManifestFile)); err != nil {
		t.Fatal(err)
	}
	if _, err := m.ledger.Admit(admission.Request{Kind: admission.KindPrepare, GuestCPUs: 4, GuestMemory: 8 * gib,
		DiskReserve: admission.PrepareDiskReservation(macvm.BaseDiskBytes), OwnerPID: 30, OwnerStart: 3}); err != nil {
		t.Fatal(err)
	}
	return r, m
}

// preparingBase is the caps' base while a prepare whose log is lines runs.
func preparingBase(t *testing.T, lines ...string) runnerwire.MacVMBase {
	t.Helper()
	_, m := preparingRig(t)
	writeLog(t, m, lines...)
	_, b := m.base()
	if b.State != runnerwire.MacVMBasePreparing {
		t.Fatalf("base %+v, want preparing", b)
	}
	return b
}

// failedBase is the caps' base after a prepare ended with the helper's error line.
func failedBase(t *testing.T, lines ...string) (*macOSVMs, runnerwire.MacVMBase) {
	t.Helper()
	r := newMacLedgerRig(t)
	m := r.open(t)
	if err := os.Remove(filepath.Join(m.layout.BaseDir(0), macvm.ManifestFile)); err != nil {
		t.Fatal(err)
	}
	writeLog(t, m, lines...)
	_, b := m.base()
	if b.State != runnerwire.MacVMBaseFailed {
		t.Fatalf("base %+v, want failed", b)
	}
	return m, b
}

// Each helper phase maps to its step; a check starts the log over; the percent is set for download and
// install only, and only an event that carries a fraction moves it.
func TestMacOSVMStatusStep(t *testing.T) {
	for phase, want := range map[string]string{
		macvm.PhaseCheck: "check", macvm.PhaseDownload: "download", macvm.PhaseValidate: "download",
		macvm.PhaseCreate: "install", macvm.PhaseInstall: "install", macvm.PhaseProvision: "setup",
		macvm.PhaseSetupByHand: "setup", macvm.PhaseBootstrap: "setup", macvm.PhaseShutdown: "setup", macvm.PhaseTools: "tools",
	} {
		b := preparingBase(t, ev(t, macvm.PrepareEvent{Phase: phase, Fraction: 0.5}))
		pct := 0
		if phase == macvm.PhaseDownload || phase == macvm.PhaseInstall {
			pct = 50
		}
		if b.Step != want || b.StepPercent != pct {
			t.Errorf("phase %s: step %q percent %d, want %q %d", phase, b.Step, b.StepPercent, want, pct)
		}
	}
	// A check resets the step, the percent, the size and the version of the prepare before it.
	b := preparingBase(t,
		ev(t, macvm.PrepareEvent{Phase: macvm.PhaseDownload, Fraction: 0.456, Total: 1000, Version: "27.0"}),
		ev(t, macvm.PrepareEvent{Phase: macvm.PhaseCheck}))
	if b.Step != "check" || b.StepPercent != 0 || b.DownloadBytes != 0 || b.ImageVersion != "" {
		t.Fatalf("after a check: %+v", b)
	}
	// The percent is the fraction floored; a pause and its end carry none and leave it; a new step starts at 0.
	b = preparingBase(t,
		ev(t, macvm.PrepareEvent{Phase: macvm.PhaseDownload, Fraction: 0.456, Bytes: 1, Total: 2}),
		ev(t, macvm.PrepareEvent{Phase: macvm.PhaseDownload, Paused: true}),
		`{"phase":"download"}`)
	if b.StepPercent != 45 {
		t.Fatalf("percent across a pause: %+v", b)
	}
	b = preparingBase(t, ev(t, macvm.PrepareEvent{Phase: macvm.PhaseDownload, Fraction: 1}), ev(t, macvm.PrepareEvent{Phase: macvm.PhaseCreate}))
	if b.Step != "install" || b.StepPercent != 0 {
		t.Fatalf("a new step: %+v", b)
	}
}

// ac_power: a pause before the download's first byte, and in validate, create, install and tools.
// battery: a download pause after bytes. Only a check resets that: the event that ends a pause marshals as
// {"phase":"download"}, the same as the download's start. The next unpaused event clears every wait.
func TestMacOSVMStatusWait(t *testing.T) {
	paused := func(phase string) string {
		return ev(t, macvm.PrepareEvent{Phase: phase, Paused: true, Detail: "waiting for AC power"})
	}
	bytes := ev(t, macvm.PrepareEvent{Phase: macvm.PhaseDownload, Bytes: 100, Total: 1000, Fraction: 0.1, BytesPerSec: 5})
	for _, phase := range []string{macvm.PhaseDownload, macvm.PhaseValidate, macvm.PhaseCreate, macvm.PhaseInstall, macvm.PhaseTools} {
		if b := preparingBase(t, ev(t, macvm.PrepareEvent{Phase: macvm.PhaseCheck}), paused(phase)); b.Wait != runnerwire.MacVMWaitACPower {
			t.Errorf("paused in %s: wait %q, want ac_power", phase, b.Wait)
		}
		// ...and the next unpaused event clears it.
		if b := preparingBase(t, paused(phase), ev(t, macvm.PrepareEvent{Phase: phase})); b.Wait != "" {
			t.Errorf("%s resumed: wait %q", phase, b.Wait)
		}
	}
	if b := preparingBase(t, ev(t, macvm.PrepareEvent{Phase: macvm.PhaseCheck}), bytes, paused(macvm.PhaseDownload)); b.Wait != runnerwire.MacVMWaitBattery {
		t.Fatalf("paused after bytes: wait %q, want battery", b.Wait)
	}
	// The scanner: pause (ac_power), its end, bytes, pause again (battery); a new check makes the next pause ac_power.
	lines := []string{ev(t, macvm.PrepareEvent{Phase: macvm.PhaseCheck}), `{"phase":"download"}`, paused(macvm.PhaseDownload)}
	if b := preparingBase(t, lines...); b.Wait != runnerwire.MacVMWaitACPower {
		t.Fatalf("first pause: %q", b.Wait)
	}
	lines = append(lines, `{"phase":"download"}`, bytes, paused(macvm.PhaseDownload))
	if b := preparingBase(t, lines...); b.Wait != runnerwire.MacVMWaitBattery {
		t.Fatalf("pause after bytes: %q", b.Wait)
	}
	lines = append(lines, `{"phase":"download"}`, paused(macvm.PhaseDownload))
	if b := preparingBase(t, lines...); b.Wait != runnerwire.MacVMWaitBattery {
		t.Fatalf("pause after a pause's end, without a check: %q, want battery", b.Wait)
	}
	lines = append(lines, ev(t, macvm.PrepareEvent{Phase: macvm.PhaseCheck}), `{"phase":"download"}`, paused(macvm.PhaseDownload))
	if b := preparingBase(t, lines...); b.Wait != runnerwire.MacVMWaitACPower {
		t.Fatalf("pause after a new check: %q, want ac_power", b.Wait)
	}
	// download_retry: a retry event, cleared by the next progress event; the percent stays.
	retry := ev(t, macvm.PrepareEvent{Phase: macvm.PhaseDownload, Retry: true, Bytes: 100, Total: 1000})
	b := preparingBase(t, bytes, retry)
	if b.Wait != runnerwire.MacVMWaitDownloadRetry || b.StepPercent != 10 {
		t.Fatalf("retry: %+v", b)
	}
	if b := preparingBase(t, bytes, retry, bytes); b.Wait != "" {
		t.Fatalf("progress after a retry: wait %q", b.Wait)
	}
}

// setup_by_hand: set for a non-empty 0600 setup-code file, never for a 0644 or an empty one; the code in it
// is not read.
func TestMacOSVMStatusSetupByHand(t *testing.T) {
	for _, tc := range []struct {
		mode os.FileMode
		body string
		want string
	}{{0o600, `{"code":"K7MP2QXR"}`, runnerwire.MacVMWaitSetupByHand}, {0o644, `{"code":"K7MP2QXR"}`, ""}, {0o600, "", ""}} {
		_, m := preparingRig(t)
		writeLog(t, m, ev(t, macvm.PrepareEvent{Phase: macvm.PhaseSetupByHand}))
		f := macvm.Layout{Root: m.layout.Root}.SetupCode()
		_ = os.WriteFile(f, []byte(tc.body), tc.mode)
		_ = os.Chmod(f, tc.mode)
		_, b := m.base()
		if b.Wait != tc.want || b.Step != "setup" {
			t.Errorf("mode %o body %q: %+v, want wait %q", tc.mode, tc.body, b, tc.want)
		}
		if raw, _ := json.Marshal(b); strings.Contains(string(raw), "K7MP2QXR") {
			t.Errorf("the setup code is in the base: %s", raw)
		}
	}
}

// A failed base carries the failure and the cause through the helper's exported prefixes; download's
// doubled prefix strips to the cause; a stage 2 error has no failure and stays raw; the step says where it
// failed; a tools failure sits on a ready base.
func TestMacOSVMStatusFailure(t *testing.T) {
	log := func(msg string) []string {
		return []string{ev(t, macvm.PrepareEvent{Phase: macvm.PhaseCheck}), ev(t, macvm.PrepareEvent{Phase: macvm.PhaseInstall, Fraction: 0.3}), "wardyn-macvm: " + msg}
	}
	_, b := failedBase(t, log(macvm.InstallFailurePrefix+"the installer crashed")...)
	if b.Failure != "install" || b.Error != "the installer crashed" || b.Step != "install" || b.StepPercent != 30 || b.Wait != "" {
		t.Fatalf("install failure: %+v", b)
	}
	_, b = failedBase(t, log(macvm.DownloadFailurePrefix+macvm.DownloadFailurePrefix+"HTTP 500")...)
	if b.Failure != "download" || b.Error != "HTTP 500" {
		t.Fatalf("download failure: %+v", b)
	}
	_, b = failedBase(t, log("macvm: stage 2: boot: no route")...)
	if b.Failure != "" || b.Error != "macvm: stage 2: boot: no route" || b.Step != "install" {
		t.Fatalf("stage 2 failure: %+v", b)
	}
	// A tools failure: a ready base, the cause in the error, no step.
	r := newMacLedgerRig(t)
	m := r.open(t)
	rec := `{"pins":{},"withheld":{"claude-code":{"version":"1","sha256":"x","reason":"` + macvm.CLTFailurePrefix + `network down"},` +
		`"aider":{"version":"1","sha256":"x","reason":"some other reason"}},"harnesses":[]}`
	if err := os.WriteFile(filepath.Join(m.layout.BaseDir(0), macvm.ToolsRecordFile), []byte(rec), 0o600); err != nil {
		t.Fatal(err)
	}
	_, b = m.base()
	if b.State != runnerwire.MacVMBaseReady || b.Failure != "tools" || b.Error != "network down" || b.Step != "" {
		t.Fatalf("tools failure: %+v", b)
	}
	// A withheld harness for another reason is no failure.
	rec = `{"pins":{},"withheld":{"aider":{"version":"1","sha256":"x","reason":"some other reason"}},"harnesses":[]}`
	_ = os.WriteFile(filepath.Join(m.layout.BaseDir(0), macvm.ToolsRecordFile), []byte(rec), 0o600)
	if _, b = m.base(); b.Failure != "" || b.Error != "" {
		t.Fatalf("another withheld reason: %+v", b)
	}
}

// The rate and the size come from the last progress event; there is no rate during a wait, and a prepare
// from the cache (validate events only) gets its size from the second validate event.
func TestMacOSVMStatusRate(t *testing.T) {
	start := ev(t, macvm.PrepareEvent{Phase: macvm.PhaseDownload, Version: "27.0"})
	p1 := ev(t, macvm.PrepareEvent{Phase: macvm.PhaseDownload, Bytes: 100, Total: 1000, Fraction: 0.1})
	p2 := ev(t, macvm.PrepareEvent{Phase: macvm.PhaseDownload, Bytes: 200, Total: 1000, Fraction: 0.2, BytesPerSec: 38.9e6})
	b := preparingBase(t, ev(t, macvm.PrepareEvent{Phase: macvm.PhaseCheck}), start, p1, p2)
	if b.BytesPerSec != 38900000 || b.DownloadBytes != 1000 || b.ImageVersion != "27.0" || b.StepPercent != 20 {
		t.Fatalf("download: %+v", b)
	}
	if b := preparingBase(t, start, p1); b.BytesPerSec != 0 {
		t.Fatalf("the first progress event has no rate: %+v", b)
	}
	if b := preparingBase(t, start, p2, ev(t, macvm.PrepareEvent{Phase: macvm.PhaseDownload, Paused: true})); b.BytesPerSec != 0 || b.Wait != "battery" {
		t.Fatalf("during a wait: %+v", b)
	}
	if b := preparingBase(t, start, p2, ev(t, macvm.PrepareEvent{Phase: macvm.PhaseDownload, Paused: true}), start); b.BytesPerSec != 0 || b.Wait != "" {
		t.Fatalf("after a wait, before the next progress event: %+v", b)
	}
	var failed prepareStatus
	for _, e := range []macvm.PrepareEvent{{Phase: macvm.PhaseDownload}, {Phase: macvm.PhaseDownload, Bytes: 200, Total: 1000, Fraction: 0.2, BytesPerSec: 38.9e6}} {
		failed.add(e)
	}
	var fb runnerwire.MacVMBase
	failed.apply(&fb, false)
	if fb.BytesPerSec != 0 {
		t.Fatalf("a base that is not preparing has no rate: %+v", fb)
	}
	if b := preparingBase(t, start, p2, ev(t, macvm.PrepareEvent{Phase: macvm.PhaseCreate})); b.BytesPerSec != 0 {
		t.Fatalf("after the download: %+v", b)
	}
	b = preparingBase(t, ev(t, macvm.PrepareEvent{Phase: macvm.PhaseCheck}), ev(t, macvm.PrepareEvent{Phase: macvm.PhaseValidate}),
		ev(t, macvm.PrepareEvent{Phase: macvm.PhaseValidate, Total: 25e9, Version: "27.0.1"}))
	if b.DownloadBytes != 25e9 || b.ImageVersion != "27.0.1" || b.Step != "download" || b.BytesPerSec != 0 {
		t.Fatalf("from the cache: %+v", b)
	}
}

// probeScript is a wardyn-macvm that prints out for probe and exits code, and appends each call to args.
func probeScript(t *testing.T, out string, code int) (helper, args string) {
	t.Helper()
	dir := t.TempDir()
	args, helper = filepath.Join(dir, "args"), filepath.Join(dir, "wardyn-macvm")
	script := "#!/bin/sh\necho \"$@\" >>" + args + "\ncat <<'EOF'\n" + out + "\nEOF\nexit " + strconv.Itoa(code) + "\n"
	if err := os.WriteFile(helper, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return helper, args
}

// fakeTool is a program that prints out.
func fakeTool(t *testing.T, name, out string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\necho '"+out+"'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return p
}

// withSwVers makes sw_vers print version.
func withSwVers(t *testing.T, version string) {
	t.Helper()
	prev := swVersPath
	swVersPath = fakeTool(t, "sw_vers", version)
	t.Cleanup(func() { swVersPath = prev })
}

// reasonOf is the caps after a gate whose probe prints out and exits code.
func reasonOf(t *testing.T, out string, code int) runnerwire.MacVMCaps {
	t.Helper()
	r := newMacLedgerRig(t)
	m := r.open(t)
	m.helper, _ = probeScript(t, out, code)
	m.gate(context.Background())
	m.recompute()
	return *m.current()
}

// Every reason code comes from its own trigger, with its numbers, and the Refusal's words stay as they were.
func TestMacOSVMStatusReason(t *testing.T) {
	// A ledger missing or damaged, and a missing helper.
	damaged := newMacLedgerRig(t)
	_ = os.Remove(ledgerLock(damaged.dir))
	m := openMacOSVMs(macOSVMSetup{ledgerDir: damaged.dir, root: damaged.root, runnerID: "runner-a", uid: 501, host: damaged.host, owner: damaged.owner, fstat: damaged.fstat})
	m.recompute()
	if c := m.current(); c.Reason != "ledger_damaged" || c.Refusal != macOSVMLedgerDamaged {
		t.Fatalf("ledger: %+v", c)
	}
	r := newMacLedgerRig(t)
	m = r.open(t)
	m.ledger, m.why, m.reason = nil, "This Mac's Wardyn runner has no macOS VM helper.", runnerwire.MacVMReasonNoHelper
	m.recompute()
	if c := m.current(); c.Reason != "no_helper" || c.Refusal != "" {
		t.Fatalf("helper: %+v", c)
	}

	// An old macOS: the probe fails and sw_vers says 25.4.
	withSwVers(t, "25.4")
	hostFails := `{"host":"macOS VMs need macOS 26 or later","provisioning":"ok","cgo":true,"vz":true}`
	if c := reasonOf(t, hostFails, 1); c.Reason != "host_os_too_old" || c.HostOSVersion != "25.4" || c.HostReason != "" || c.Refusal != "" {
		t.Fatalf("old macOS: %+v", c)
	}
	// A new macOS and a probe that fails for the framework's own answer.
	withSwVers(t, "27.0")
	for _, tc := range []struct{ out, host string }{
		{`{"host":"ok","provisioning":"x","cgo":true,"vz":true,"reason":"no_virtualization"}`, "no_virtualization"},
		{`{"host":"ok","provisioning":"x","cgo":true,"vz":true,"reason":"no_provisioning"}`, "no_provisioning"},
		{`{"host":"ok","provisioning":"x","cgo":false,"vz":false}`, "helper_build"},
		{`{"host":"ok","provisioning":"x","cgo":true,"vz":false}`, "helper_build"},
	} {
		if c := reasonOf(t, tc.out, 1); c.Reason != "host_unsupported" || c.HostReason != tc.host || c.HostOSVersion != "" {
			t.Errorf("%s: %+v", tc.out, c)
		}
	}
	// A probe that says nothing sets no code.
	if c := reasonOf(t, "not json", 1); c.Reason != "" {
		t.Fatalf("silent probe: %+v", c)
	}
	// The earlier code wins when two hold: a Mac on macOS 25 whose helper also lacks the framework.
	withSwVers(t, "25.4")
	if c := reasonOf(t, `{"host":"x","provisioning":"x","cgo":false,"vz":false}`, 1); c.Reason != "host_os_too_old" {
		t.Fatalf("old macOS and no framework: %+v", c)
	}
	withSwVers(t, "27.0")

	// Per-run limits below the base's minimum (4 CPUs, 8192 MiB).
	m = r.open(t)
	m.setPerRunLimits(0, 4096)
	m.recompute()
	if c := m.current(); c.Reason != "memory_below_minimum" || c.Refusal == "" {
		t.Fatalf("memory below minimum: %+v", c)
	}
	m = r.open(t)
	m.setPerRunLimits(2000, 0)
	m.recompute()
	if c := m.current(); c.Reason != "cpu_below_minimum" || c.Refusal == "" {
		t.Fatalf("cpu below minimum: %+v", c)
	}

	// Too little disk to prepare a base: 60 GiB free with none ready (it needs 76 GiB and the 5 GiB floor).
	m = r.open(t)
	_ = os.Remove(filepath.Join(m.layout.BaseDir(0), macvm.ManifestFile))
	m.host = &statusHost{macHost: r.host, free: 60 * gib, avail: 48 * gib}
	m.recompute()
	if c := m.current(); c.Reason != "disk_short" || c.FreeGB != 60 || c.Refusal != "" {
		t.Fatalf("disk short: %+v", c)
	}
	m.host = &statusHost{macHost: r.host, free: 500 * gib, avail: 48 * gib}
	m.recompute()
	if c := m.current(); c.Reason != "" {
		t.Fatalf("disk enough: %+v", c)
	}

	// A slot the framework keeps and nothing accounts for: a leaked entry. It ranks after disk_short and
	// before low_memory and too_hot, and Refusal stays empty.
	lr := newMacLedgerRig(t)
	m = lr.open(t)
	if _, err := m.ledger.AddLeaked("", 10, 1); err != nil {
		t.Fatal(err)
	}
	m.recompute()
	if c := m.current(); c.Reason != "slot_leaked" || c.Refusal != "" || c.Max != 1 {
		t.Fatalf("leaked slot: %+v", c)
	}
	m.setThermal("serious")
	m.host = &statusHost{macHost: lr.host, free: 500 * gib, avail: 5 * gib}
	m.recompute()
	if c := m.current(); c.Reason != "slot_leaked" {
		t.Fatalf("leaked slot, low memory and too hot: %+v", c)
	}
	m.setPerRunLimits(0, 4096)
	m.recompute()
	if c := m.current(); c.Reason != "memory_below_minimum" {
		t.Fatalf("leaked slot and a limit below the minimum: %+v", c)
	}
	m = lr.open(t)
	_, _ = m.ledger.AddLeaked("", 10, 1)
	_ = os.Remove(filepath.Join(m.layout.BaseDir(0), macvm.ManifestFile))
	m.host = &statusHost{macHost: lr.host, free: 60 * gib, avail: 48 * gib}
	m.recompute()
	if c := m.current(); c.Reason != "disk_short" {
		t.Fatalf("leaked slot and too little disk: %+v", c)
	}
	m.host = &statusHost{macHost: lr.host, free: 500 * gib, avail: 48 * gib}
	m.recompute()
	if c := m.current(); c.Reason != "slot_leaked" || c.Base.State != "none" {
		t.Fatalf("leaked slot with no base: %+v", c)
	}

	// Too little free memory for the next guest: it needs the guest's 8 GiB and the 2 GiB margin.
	m = r.open(t)
	m.host = &statusHost{macHost: r.host, free: 500 * gib, avail: 5 * gib}
	m.recompute()
	if c := m.current(); c.Reason != "low_memory" || c.FreeGB != 5 || c.NeedGB != 10 {
		t.Fatalf("low memory: %+v", c)
	}
	m.host = &statusHost{macHost: r.host, free: 500 * gib, avail: 9*gib + gib/2}
	m.recompute()
	if c := m.current(); c.Reason != "low_memory" || c.FreeGB != 9 || c.NeedGB != 10 {
		t.Fatalf("low memory, fractional: %+v", c)
	}

	// Too hot: serious and critical, not nominal or fair; low memory ranks before it, below-minimum before both.
	m = r.open(t)
	for therm, want := range map[string]string{"nominal": "", "fair": "", "serious": "too_hot", "critical": "too_hot", "": ""} {
		m.setThermal(therm)
		m.recompute()
		if c := m.current(); c.Reason != want || c.Base.State != "ready" {
			t.Errorf("thermal %q: %+v, want reason %q", therm, c, want)
		}
	}
	m.setThermal("serious")
	m.host = &statusHost{macHost: r.host, free: 500 * gib, avail: 5 * gib}
	m.recompute()
	if c := m.current(); c.Reason != "low_memory" {
		t.Fatalf("low memory and too hot: %+v", c)
	}
	m.setPerRunLimits(0, 4096)
	m.recompute()
	if c := m.current(); c.Reason != "memory_below_minimum" {
		t.Fatalf("below minimum and low memory: %+v", c)
	}
}

// statusHost is a Mac whose free disk and available memory the test sets.
type statusHost struct {
	*macHost
	free  int64
	avail uint64
}

func (h *statusHost) FreeDiskBytes(string) (int64, error)   { return h.free, nil }
func (h *statusHost) AvailableMemoryBytes() (uint64, error) { return h.avail, nil }

// no_session: launchctl's manager is Aqua in a GUI session; any other answer sets the bool, and what it
// printed is nowhere in the caps.
func TestMacOSVMStatusNoSession(t *testing.T) {
	prev := launchctlPath
	t.Cleanup(func() { launchctlPath = prev })
	for out, want := range map[string]bool{"Background": true, "System": true, "Aqua": false} {
		launchctlPath = fakeTool(t, "launchctl", out)
		m := newMacLedgerRig(t).open(t)
		m.checkSession(context.Background())
		m.recompute()
		c := m.current()
		raw, _ := json.Marshal(c)
		if c.NoSession != want || (want && strings.Contains(string(raw), out)) || strings.Contains(string(raw), "Aqua") {
			t.Errorf("manager %q: %s", out, raw)
		}
	}
	// A launchctl that cannot be run says nothing.
	launchctlPath = filepath.Join(t.TempDir(), "absent")
	m := newMacLedgerRig(t).open(t)
	m.checkSession(context.Background())
	if m.noSession {
		t.Fatal("an absent launchctl set no_session")
	}
	// A runner without a ledger still says it.
	launchctlPath = fakeTool(t, "launchctl", "Background")
	m = openMacOSVMs(macOSVMSetup{ledgerDir: t.TempDir(), root: t.TempDir(), runnerID: "r", uid: 501, host: &macHost{}})
	m.checkSession(context.Background())
	m.recompute()
	if c := m.current(); !c.NoSession || c.Reason != "ledger_damaged" {
		t.Fatalf("no ledger: %+v", c)
	}
}

// Nothing in the base's error names the login: its home is ~ and the uid in a socket path is <uid>, other
// paths stay, and the error is at most 512 bytes (on a rune boundary).
func TestMacOSVMStatusScrubsTheError(t *testing.T) {
	t.Setenv("HOME", "/Users/dana-test")
	msg := "dial /Users/Shared/Wardyn/sock/u501/abc/ctl.sock: no such file; disk /Users/dana-test/Library/Wardyn/macos-vm.noindex/bases/0/disk.img; see /var/log/u99/x"
	m, b := failedBase(t, "wardyn-macvm: "+msg)
	raw, _ := json.Marshal(m.current())
	want := "dial /Users/Shared/Wardyn/sock/u<uid>/abc/ctl.sock: no such file; disk ~/Library/Wardyn/macos-vm.noindex/bases/0/disk.img; see /var/log/u99/x"
	if b.Error != want || strings.Contains(string(raw), "dana-test") || strings.Contains(string(raw), "u501") {
		t.Fatalf("error %q, want %q", b.Error, want)
	}
	// The data volume's spelling of the home is the home too.
	if got := m.scrubError("open /System/Volumes/Data/Users/dana-test/x"); got != "open ~/x" {
		t.Fatalf("data volume home: %q", got)
	}
	// A configured socket root is the anchor.
	m.sockRoot = "/tmp/socks"
	if got := m.scrubError("bind /tmp/socks/u502/k and /Users/Shared/Wardyn/sock/u502/k"); got != "bind /tmp/socks/u<uid>/k and /Users/Shared/Wardyn/sock/u502/k" {
		t.Fatalf("custom sock root: %q", got)
	}
	_, b = failedBase(t, "wardyn-macvm: "+strings.Repeat("x", 2000))
	if len(b.Error) != 512 {
		t.Fatalf("a 2000-byte error is %d bytes", len(b.Error))
	}
	_, b = failedBase(t, "wardyn-macvm: "+strings.Repeat("é", 400))
	if len(b.Error) > 512 || !utf8.ValidString(b.Error) {
		t.Fatalf("a multi-byte error is %d bytes, valid %v", len(b.Error), utf8.ValidString(b.Error))
	}
	// The tools failure's cause is scrubbed too.
	r := newMacLedgerRig(t)
	rm := r.open(t)
	rec := `{"pins":{},"withheld":{"h":{"version":"1","sha256":"x","reason":"` + macvm.CLTFailurePrefix + `/Users/dana-test/x"}},"harnesses":[]}`
	_ = os.WriteFile(filepath.Join(rm.layout.BaseDir(0), macvm.ToolsRecordFile), []byte(rec), 0o600)
	if _, tb := rm.base(); tb.Error != "~/x" {
		t.Fatalf("tools failure error %q", tb.Error)
	}
}

// A macOS version or an image version outside ^\d+(\.\d+){0,2}$ is dropped, and an image version that
// matched earlier is kept.
func TestMacOSVMStatusVersionPattern(t *testing.T) {
	withSwVers(t, "25.4-beta")
	c := reasonOf(t, `{"host":"x","provisioning":"ok","cgo":true,"vz":true}`, 1)
	if c.Reason != "host_os_too_old" || c.HostOSVersion != "" {
		t.Fatalf("25.4-beta: %+v", c)
	}
	good := ev(t, macvm.PrepareEvent{Phase: macvm.PhaseDownload, Version: "27.0"})
	for _, bad := range []string{"27.0.1-rc", "../x", strings.Repeat("9", 200), "1.2.3.4", ""} {
		b := preparingBase(t, good, ev(t, macvm.PrepareEvent{Phase: macvm.PhaseValidate, Version: bad}))
		if b.ImageVersion != "27.0" {
			t.Errorf("version %q replaced the earlier one: %q", bad, b.ImageVersion)
		}
		if b := preparingBase(t, ev(t, macvm.PrepareEvent{Phase: macvm.PhaseValidate, Version: bad})); b.ImageVersion != "" {
			t.Errorf("version %q kept: %q", bad, b.ImageVersion)
		}
	}
	if b := preparingBase(t, good, ev(t, macvm.PrepareEvent{Phase: macvm.PhaseValidate, Version: "27.0.1"})); b.ImageVersion != "27.0.1" {
		t.Fatalf("a later matching version: %q", b.ImageVersion)
	}
}

// Nothing is resent while the inputs are unchanged, and the thermal re-probe runs on every 4th tick only,
// while the base is ready.
func TestMacOSVMStatusCadence(t *testing.T) {
	r := newMacLedgerRig(t)
	m := r.open(t)
	if !m.recompute() || m.recompute() {
		t.Fatal("a second recompute with unchanged inputs must report no change")
	}
	var args string
	m.helper, args = probeScript(t, `{"host":"ok","provisioning":"ok","thermal":"serious"}`, 0)
	probes := func() int {
		b, _ := os.ReadFile(args)
		return strings.Count(string(b), "probe")
	}
	for i := 1; i <= 8; i++ {
		m.tick(context.Background())
		if want := i / 4; probes() != want {
			t.Fatalf("after %d ticks: %d probes, want %d", i, probes(), want)
		}
	}
	if !m.recompute() {
		t.Fatal("a hot Mac changed nothing")
	}
	if c := m.current(); c.Reason != "too_hot" || c.Base.State != "ready" {
		t.Fatalf("after the re-probe: %+v", c)
	}
	// A base that is not ready is not re-probed, and a re-probe that fails changes nothing.
	m = r.open(t)
	m.helper, args = probeScript(t, `{"host":"ok","provisioning":"ok","thermal":"serious"}`, 0)
	_ = os.Remove(filepath.Join(m.layout.BaseDir(0), macvm.ManifestFile))
	m.recompute()
	for range 4 {
		m.tick(context.Background())
	}
	if probes() != 0 {
		t.Fatalf("a base that is not ready was re-probed %d times", probes())
	}
	m = r.open(t)
	m.helper, _ = probeScript(t, "garbage", 1)
	m.setThermal("serious")
	m.recompute()
	for range 4 {
		m.tick(context.Background())
	}
	if _, _, _, th, _ := m.hostStatus(); th != "serious" {
		t.Fatalf("a failed re-probe changed the thermal state to %q", th)
	}
}

// dockerWaitRig is a prepare whose stage-2 boot listens for its network, and a prep network that fails
// while fail is set.
func dockerWaitRig(t *testing.T, dockerDown *atomic.Bool, fail *atomic.Bool) (r *macLedgerRig, m *macOSVMs, e admission.Entry, attached <-chan error, nets *prepNets) {
	t.Helper()
	r = newMacLedgerRig(t)
	m = r.open(t)
	if err := os.Remove(filepath.Join(m.layout.BaseDir(0), macvm.ManifestFile)); err != nil {
		t.Fatal(err)
	}
	m.sockRoot, m.sockFstat = macvmtest.SockRoot(t), macvmtest.SockFstat
	e, attached = liveStage2JobOf(t, r, m, admission.KindPrepare)
	nets = &prepNets{t: t}
	m.prepNet = func(ctx context.Context, id string) (*os.File, func(), error) {
		if fail.Load() {
			return nil, nil, errors.New("docker: the stage-2 network did not start")
		}
		return nets.start(ctx, id)
	}
	m.prepRemove = nets.remove
	m.dockerUp = func(context.Context) bool { return !dockerDown.Load() }
	return r, m, e, attached, nets
}

// awaitWait recomputes until the base's wait is want.
func awaitWait(t *testing.T, m *macOSVMs, want string) {
	t.Helper()
	var got string
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		m.recompute()
		if got = m.current().Base.Wait; got == want {
			return
		}
	}
	t.Fatalf("the base's wait is %q, want %q", got, want)
}

// While the stage-2 network cannot start and Docker does not answer, the preparing base waits for Docker.
// Another network error with Docker up sets no wait; a hand-off that succeeds clears the wait; so does
// the job's helper going away.
func TestMacOSVMStatusDockerWait(t *testing.T) {
	var down, fail atomic.Bool
	down.Store(true)
	fail.Store(true)
	_, m, _, attached, _ := dockerWaitRig(t, &down, &fail)
	stop := serving(m)
	defer handled(t, m)
	defer stop()
	m.recompute() // starts the job's handler
	awaitWait(t, m, runnerwire.MacVMWaitDocker)
	if c := m.current(); c.Base.State != runnerwire.MacVMBasePreparing || c.Base.Wait != "docker" {
		t.Fatalf("caps: %+v", c.Base)
	}
	// Docker answers again but the network still fails for another reason: no wait.
	down.Store(false)
	awaitWait(t, m, "")
	// Docker down again, then the hand-off succeeds: the wait is gone.
	down.Store(true)
	awaitWait(t, m, runnerwire.MacVMWaitDocker)
	fail.Store(false)
	for deadline := time.After(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		m.recompute() // each recompute starts the handler again
		select {
		case err := <-attached:
			if err != nil {
				t.Fatalf("the stage-2 boot: %v", err)
			}
			awaitWait(t, m, "")
			return
		case <-deadline:
			t.Fatal("the stage-2 boot never got its network")
		default:
		}
	}
}

func TestMacOSVMStatusDockerWaitEndsWithTheHelper(t *testing.T) {
	var down, fail atomic.Bool
	down.Store(true)
	fail.Store(true)
	r, m, _, _, _ := dockerWaitRig(t, &down, &fail)
	stop := serving(m)
	defer handled(t, m)
	defer stop()
	m.recompute()
	awaitWait(t, m, runnerwire.MacVMWaitDocker)
	r.host.setProcs(admission.Proc{PID: os.Getpid(), UID: 501, Start: 3, Name: "wardyn-runnerd"})
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		m.recompute()
		m.mu.Lock()
		n := len(m.dockerWait)
		m.mu.Unlock()
		if n == 0 && m.current().Base.Wait == "" {
			return
		}
	}
	t.Fatalf("the wait outlived the job's helper: %+v", m.current().Base)
}

// MAC26: a Mac on macOS 26, whose helper's probe passes with no guest provisioning, is offered macOS VMs once
// its base is ready, and `status` says its prepare needs setup by hand while it has no base; a Mac on macOS 25
// is refused with host_os_too_old and its version.
func TestMacOSVMOfferedOnMacOS26(t *testing.T) {
	withSwVers(t, "26.7")
	const probe26 = `{"host":"ok","provisioning":"macvm provision: this Mac's Virtualization framework has no class VZMacGuestProvisioningOptions; guest provisioning needs macOS 27 or later","cgo":true,"vz":true,"reason":"no_provisioning","thermal":"nominal"}`
	r := newMacLedgerRig(t)
	r.host.setProcs(admission.Proc{PID: os.Getpid(), UID: 501, Start: 3, Name: "wardyn-runnerd"})
	m := r.open(t)
	m.helper, _ = probeScript(t, probe26, 0)
	m.gate(context.Background())
	m.recompute()
	if c := m.current(); !m.enabled() || c.Reason != "" || c.Max == 0 || m.why != "" {
		t.Fatalf("macOS 26 with a ready base: enabled %v, caps %+v, why %q", m.enabled(), c, m.why)
	}
	text := func() string {
		st, err := m.status(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		var b strings.Builder
		st.writeText(&b)
		return b.String()
	}
	if got := text(); strings.Contains(got, "setup by hand") {
		t.Fatalf("a ready base still asks for setup by hand:\n%s", got)
	}
	_ = os.Remove(filepath.Join(m.layout.BaseDir(0), macvm.ManifestFile))
	if got := text(); !strings.Contains(got, "needs setup by hand once") || !strings.Contains(got, macOSVMSetupWindow) {
		t.Fatalf("no base on macOS 26: status does not say setup by hand:\n%s", got)
	}

	withSwVers(t, "25.6")
	c := reasonOf(t, `{"host":"macOS VMs need macOS 26 or later on this Mac (it runs 25.6)","provisioning":"x","cgo":true,"vz":true,"reason":"no_provisioning"}`, 1)
	if c.Reason != "host_os_too_old" || c.HostOSVersion != "25.6" {
		t.Fatalf("macOS 25: %+v", c)
	}
}

// MAC26: on a Mac whose probe says no guest provisioning, `macos-vm prepare` (and setup's prepare) refuses
// up front, before the helper's prepare, the Time Machine exclusion or the job's log, when the runner is
// not in the desktop's GUI session or runs as root: the guest's window could not open. In the desktop it
// starts; on a Mac with provisioning, no session is needed.
func TestMacOSVMPrepareNeedsTheDesktopForSetupByHand(t *testing.T) {
	const probe26 = `{"host":"ok","provisioning":"no class X; guest provisioning needs macOS 27 or later","cgo":true,"vz":true,"reason":"no_provisioning","thermal":"nominal"}`
	const probe27 = `{"host":"ok","provisioning":"ok","cgo":true,"vz":true,"thermal":"nominal"}`
	prevL, prevE, prevEx, prevWait := launchctlPath, processEUID, excludeFromBackups, prepareClaimWait
	t.Cleanup(func() {
		launchctlPath, processEUID, excludeFromBackups, prepareClaimWait = prevL, prevE, prevEx, prevWait
	})
	prepareClaimWait = 2 * time.Second
	for _, tc := range []struct {
		name, probe, manager string
		euid                 int
		refused              bool
	}{
		{"macOS 26 over SSH", probe26, "Background", 501, true},
		{"macOS 26 as root", probe26, "Aqua", 0, true},
		{"macOS 26 at the desktop", probe26, "Aqua", 501, false},
		{"macOS 27 over SSH", probe27, "Background", 501, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			launchctlPath = fakeTool(t, "launchctl", tc.manager)
			processEUID = func() int { return tc.euid }
			excluded := 0
			excludeFromBackups = func(string) error { excluded++; return nil }
			r := newMacLedgerRig(t)
			r.host.setProcs(admission.Proc{PID: os.Getpid(), UID: 501, Start: 3, Name: "wardyn-runnerd"})
			m := r.open(t)
			_ = os.Remove(filepath.Join(m.layout.BaseDir(0), macvm.ManifestFile))
			helper, args := probeScript(t, tc.probe, 0)
			err := m.startPrepare(context.Background(), prepareArgs{helper: helper, runnerID: "runner-a", cpus: 4, memoryMiB: 8192})
			got := calls(t, args)
			if !tc.refused {
				if errors.Is(err, macvm.ErrNoDesktopSession) || len(got) != 2 || !strings.HasPrefix(got[1], "prepare") {
					t.Fatalf("not refused: %v, helper calls %q", err, got)
				}
				return
			}
			if !errors.Is(err, macvm.ErrNoDesktopSession) || !strings.Contains(err.Error(), "run prepare in Terminal on the Mac, signed in at its desktop, not over SSH") {
				t.Fatalf("got %v, want the desktop refusal", err)
			}
			if len(got) != 1 || got[0] != "probe" || excluded != 0 {
				t.Fatalf("refused, yet helper calls %q and %d exclusions", got, excluded)
			}
			if _, err := os.Stat(m.layout.PrepareLog()); !os.IsNotExist(err) {
				t.Fatalf("refused, yet the job's log was made: %v", err)
			}
		})
	}
}

// MAC26: the setup-by-hand window's AppKit writes to the helper's stderr, which is prepare.log. A foreign
// line after the helper's own keeps the progress and the failure; the helper's own last line decides
// whether its prepare ended with an error.
func TestPrepareLogSkipsForeignLines(t *testing.T) {
	const appKit = "2026-10-25 13:00:00.123 wardyn-macvm[4242:77] WARNING: Secure coding is not enabled for restorable state!"
	write := func(lines ...string) string {
		p := filepath.Join(t.TempDir(), "prepare.log")
		if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	ev := `{"phase":"setup_by_hand"}`
	want := prepareProgress(write(`{"phase":"check"}`, ev))
	if want == 0 {
		t.Fatal("no progress from a setup_by_hand event")
	}
	if got := prepareProgress(write(`{"phase":"check"}`, ev, appKit, "")); got != want {
		t.Fatalf("progress %d after an AppKit line, want %d", got, want)
	}
	if got := prepareFailure(write(ev, "wardyn-macvm: macvm: stage 1: boom", appKit)); got != "macvm: stage 1: boom" {
		t.Fatalf("failure %q after an AppKit line", got)
	}
	if got := prepareFailure(write("wardyn-macvm: an earlier prepare's error", `{"phase":"download","fraction":0.5}`, appKit)); got != "" {
		t.Fatalf("a running prepare reads as failed: %q", got)
	}
}

// A foreign line over bufio's 64 KiB default sits before the helper's error: the error is still read.
func TestPrepareLogReadsPastALongForeignLine(t *testing.T) {
	p := filepath.Join(t.TempDir(), "prepare.log")
	long := strings.Repeat("x", 200<<10)
	body := `{"phase":"check"}` + "\n" + long + "\n" + "wardyn-macvm: macvm: stage 1: boom\n"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := prepareFailure(p); got != "macvm: stage 1: boom" {
		t.Fatalf("failure %q after a 200 KiB line, want the helper's error", got)
	}
	huge := `{"phase":"check"}` + "\n" + strings.Repeat("x", prepareLogLineMax+1) + "\n" + "wardyn-macvm: macvm: stage 1: boom\n"
	if err := os.WriteFile(p, []byte(huge), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := prepareFailure(p); !strings.Contains(got, "could not be read to its end") {
		t.Fatalf("failure %q after a line over the limit, want the read error", got)
	}
	// An error line read before the scan stopped is shown as before.
	early := "wardyn-macvm: macvm: stage 1: boom\n" + strings.Repeat("x", prepareLogLineMax+1) + "\n"
	if err := os.WriteFile(p, []byte(early), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := prepareFailure(p); got != "macvm: stage 1: boom" {
		t.Fatalf("failure %q with the error line before the long one, want that line", got)
	}
}
