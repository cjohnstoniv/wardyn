// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package main

import (
	"cmp"
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
	"github.com/cjohnstoniv/wardyn/internal/macvm/admission"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
)

// The Mac's other macOS bases (macos_vm_bases_dir): what the caps say of each, and their derivation from
// Standard. When Standard changes, each other base is derived from it again, one at a time, under a
// tools-update entry (one of the 2 VM slots), and never while a run on that base lives.

// macOSVMBasesDir reads the runner setting macos_vm_bases_dir (tests replace it).
var macOSVMBasesDir = func() (string, error) {
	c, err := effectiveRunnerConfig("", nil)
	if err != nil {
		return "", err
	}
	return c.get("macos_vm_bases_dir"), nil
}

// deriveBaseLog, in the VM directory, is the derivations' output.
const deriveBaseLog = "derive-base.log"

// macOSVMBases is the other bases' state. dir is the setting, read once (err: it could not be read);
// asked is the derivation last started ("<name> <stamp> <digest>") and starting says one is being
// started; failed holds, per base, the key a derivation ended at with the base still not current; warned
// is the last error logged. All but once and start are under macOSVMs.mu.
type macOSVMBases struct {
	once     sync.Once
	dir      string
	err      error
	asked    string
	starting bool
	failed   map[string]string
	warned   string
	// start starts a derivation (tests replace it); nil is startDerive under the serve's ctx.
	start func(v macvm.BaseVariant, man macvm.Manifest) error
}

// derivedBases is the caps' other bases, with Standard ready (man is its manifest), and the derivation
// trigger: while no prepare, tools update, edit or derivation of this runner lives and a VM slot is free,
// the first base that is not current, has no live run, and has not failed from this Standard is derived.
func (m *macOSVMs) derivedBases(man macvm.Manifest) []runnerwire.MacVMDerivedBase {
	b := &m.bases
	b.once.Do(func() { b.dir, b.err = macOSVMBasesDir() })
	if b.err != nil || b.dir == "" {
		if b.err != nil {
			m.warnBases(b.err)
		}
		return nil
	}
	vs, err := macvm.LoadBaseVariants(b.dir)
	if err == nil {
		var stamp string
		if stamp, err = macvm.BaseStamp(m.layout.BaseDir(0)); err == nil {
			return m.offerBases(vs, stamp, man)
		}
	}
	m.warnBases(err)
	return nil
}

func (m *macOSVMs) offerBases(vs []macvm.BaseVariant, stamp string, man macvm.Manifest) []runnerwire.MacVMDerivedBase {
	b := &m.bases
	busy := m.jobBusy()
	m.mu.Lock()
	asked, starting := b.asked, b.starting
	m.mu.Unlock()
	var out []runnerwire.MacVMDerivedBase
	next := -1
	for i, v := range vs {
		dir, digest := m.layout.VariantDir(v.Name), v.List.Digest()
		key := v.Name + " " + stamp + " " + digest
		d := runnerwire.MacVMDerivedBase{Name: v.Name, State: runnerwire.MacVMDerivedRebuilding, Items: len(v.List.Items)}
		m.mu.Lock()
		failed := b.failed[v.Name] == key
		m.mu.Unlock()
		switch {
		case macvm.VariantCurrent(dir, stamp, digest):
			vm, _ := macvm.ReadManifest(dir)
			d.State, d.GuestOS, d.Harnesses, d.BuildVersion = runnerwire.MacVMDerivedReady, vm.GuestOS, slices.Clone(vm.Harnesses), vm.BuildVersion
			if !vm.CreatedAt.IsZero() {
				d.CreatedAt = vm.CreatedAt.UTC().Format(time.RFC3339)
			}
		case asked == key && (busy || starting):
			if ev, ok, _, _ := lastHelperLines(filepath.Join(m.layout.Root, deriveBaseLog)); ok {
				d.Progress = min(int(max(ev.Fraction, 0)*100), 99)
			}
		case asked == key || failed:
			// Its derivation from this Standard ended, and the base is still not current.
			d.State = runnerwire.MacVMDerivedFailed
			if !failed {
				slog.Warn("macOS VM bases: a derivation from Standard failed; see "+deriveBaseLog, "base", v.Name)
				m.mu.Lock()
				if b.failed == nil {
					b.failed = map[string]string{}
				}
				b.failed[v.Name] = key
				m.mu.Unlock()
			}
		case next < 0 && !m.runsOn(dir):
			next = i
		}
		out = append(out, d)
	}
	if next >= 0 && !busy && !starting {
		m.deriveNext(vs[next], vs[next].Name+" "+stamp+" "+vs[next].List.Digest(), man)
	}
	return out
}

// deriveNext starts v's derivation in the background (startJob waits for the helper's claim), marking it
// asked once it started.
func (m *macOSVMs) deriveNext(v macvm.BaseVariant, key string, man macvm.Manifest) {
	b := &m.bases
	m.mu.Lock()
	ctx, start := m.ctx, b.start
	m.mu.Unlock()
	if start == nil {
		if ctx == nil {
			return // not serving: nothing starts a job
		}
		start = func(v macvm.BaseVariant, man macvm.Manifest) error {
			return m.startDerive(context.WithoutCancel(ctx), v, man)
		}
	}
	m.mu.Lock()
	b.starting = true
	m.mu.Unlock()
	go func() {
		err := start(v, man)
		m.mu.Lock()
		b.starting = false
		if err == nil {
			b.asked = key
		}
		m.mu.Unlock()
		if err != nil {
			slog.Warn("macOS VM bases: the derivation did not start", "base", v.Name, "error", err.Error())
		}
		m.changed()
	}()
}

// startDerive starts the helper's derivation of v from Standard (man is Standard's manifest): update-tools
// --variant under its own tools-update entry, sized as a tools update.
func (m *macOSVMs) startDerive(ctx context.Context, v macvm.BaseVariant, man macvm.Manifest) error {
	if m.ledger == nil {
		return errors.New(cmp.Or(m.refusal, m.why))
	}
	extra := []string{"--" + macvm.FlagVariant, v.Name, "--" + macvm.FlagBaseFile, filepath.Join(m.bases.dir, v.Name+".json")}
	return m.startJob(ctx, helperJob{kind: admission.KindToolsUpdate, extra: extra, disk: admission.RunDiskReservation(cmp.Or(man.DiskBytes, macvm.BaseDiskBytes)),
		log: filepath.Join(m.layout.Root, deriveBaseLog), sub: macvm.UpdateToolsCommand, helper: m.helper, cpus: max(man.MinCPUs, 1),
		memMiB: m.guestMemory(man) >> 20, runner: m.runnerID, ledger: m.ledgerDir, descr: "derivation"})
}

// jobBusy says a derivation must wait: a prepare, tools update (Standard's, or a derivation) or edit of
// this runner lives, or both VM slots are taken.
func (m *macOSVMs) jobBusy() bool {
	for _, k := range []admission.Kind{admission.KindPrepare, admission.KindToolsUpdate, kindEditBase} {
		if slices.ContainsFunc(m.own(k), func(e admission.Entry) bool { return !m.vmGone(e) }) {
			return true
		}
	}
	counted, err := m.ledger.Counted()
	return err != nil || len(counted) >= admission.LicenceCap
}

// runsOn says a live run of this runner may be on the base in dir: its run.json names dir, or cannot be
// read yet.
func (m *macOSVMs) runsOn(dir string) bool {
	for _, e := range m.own(admission.KindRun) {
		if m.vmGone(e) {
			continue
		}
		if c, err := macvm.ReadRunConfig(m.layout.RunDir(e.RunID)); err != nil || c.BaseDir == dir {
			return true
		}
	}
	return false
}

// warnBases logs err once, until another error replaces it.
func (m *macOSVMs) warnBases(err error) {
	m.mu.Lock()
	again := m.bases.warned == err.Error()
	m.bases.warned = err.Error()
	m.mu.Unlock()
	if !again {
		slog.Warn("macOS VM bases: the other bases cannot be read; only Standard is offered", "error", err.Error())
	}
}
