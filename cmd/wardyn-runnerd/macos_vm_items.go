// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
)

// The macOS base's install list (macos_vm_base_file): the helper flag a prepare and a tools update pass,
// the trigger that starts a tools update when the list changes, and what the caps carry of it.

// macOSVMItems is the install list's state: file is the setting (err: it could not be read, so neither a
// prepare nor a tools update starts); asked is the read error last logged.
type macOSVMItems struct {
	file  string
	err   error
	asked string
	// ca is this Mac's inspection CA, which the list gains as its last item (macvm.WithInspectionCA); nil when
	// macos_vm_inspection_ca is off. caErr says why it could not be opened: then no prepare or tools update
	// starts, as for a list that cannot be read, since the list it would install is not known.
	ca    *macvm.InspectionCA
	caErr error
}

// openInspectionCA opens this Mac's inspection CA in the VM directory when macos_vm_inspection_ca is on. on is
// nil when the caller resolved no settings (setup's prepare) or the setting is neither on nor off: the CA is then
// neither opened nor removed. owner is the runner's handle (enableMacOSVM): only it makes the CA, and only it removes it, when the setting is off or one
// of its drive roots is or holds the CA's directory, since a key left on disk is one a run's mount could read. A
// macos-vm command resolves its own config, which may not be the runner's, so it only reads a CA that is there.
func (m *macOSVMs) openInspectionCA(on *bool, driveRoots []string, owner bool) {
	if on == nil {
		return
	}
	dir := m.layout.InspectionCADir()
	over := driveRootOver(dir, driveRoots)
	switch {
	case *on && over == "":
		// A profile that is there but cannot be read could be hiding an organisation's 0, so it makes no CA either.
		open := macvm.OpenInspectionCA
		_, perr := managedPrefErr("MacOSVMInspectionCA")
		readOnly := !owner || perr != nil
		if readOnly {
			open = macvm.ReadInspectionCA
		} else if err := excludeFromBackups(m.layout.Root); err != nil {
			// Before the key is made: the first prepare's exclusion can come long after this start.
			slog.Warn("macOS VM directory: Time Machine exclusion", "error", err.Error())
		}
		ca, err := open(context.Background(), dir)
		if readOnly && errors.Is(err, os.ErrNotExist) {
			if owner {
				slog.Warn("macOS VM base: macos_vm_inspection_ca is on, but the managed profile could not be read, so this Mac's inspection CA is not made; the runner makes it at its first start that reads the profile", "error", perr.Error())
			} else {
				slog.Info("macOS VM base: this Mac's inspection CA is not made yet; the runner makes it at its next start")
			}
			return
		}
		if err != nil {
			slog.Warn("macOS VM base: this Mac's inspection CA cannot be used; no prepare or tools update starts until it can", "error", err.Error())
			m.items.caErr = fmt.Errorf("macos_vm_inspection_ca: %w", err)
			return
		}
		m.items.ca = ca
		return
	case !owner:
		return
	case over != "":
		slog.Warn("macOS VM base: this Mac's inspection CA is not kept: a drive root holds the VM directory, so a run could mount the CA's key", "drive_root", over)
	default:
		// A profile that is there but cannot be read reads as unset, which could turn an organisation's 1 into off.
		if _, perr := managedPrefErr("MacOSVMInspectionCA"); perr != nil {
			slog.Warn("macOS VM base: macos_vm_inspection_ca reads as off, but the managed profile could not be read, so this Mac's inspection CA is kept", "error", perr.Error())
			return
		}
	}
	removed, err := macvm.RemoveInspectionCA(context.Background(), dir)
	if err != nil {
		slog.Warn("macOS VM base: this Mac's inspection CA could not be removed", "error", err.Error())
	} else if removed {
		slog.Warn("macOS VM base: this Mac's inspection CA was removed; on macOS 26 a base that trusted it stops its tools updates until macos-vm prepare --new-image")
	}
}

// driveRootOver is the drive root in roots that is dir or one of its parents; "" when there is none. dir's
// parent and each root's are resolved first, since a mount of any folder above the real one holds the key. Roots are compared as
// files (os.SameFile), so another spelling of the same folder counts, and by spelling, so a root that names a folder not made yet counts.
func driveRootOver(dir string, roots []string) string {
	resolve := func(p string) string {
		if parent, err := filepath.EvalSymlinks(filepath.Dir(p)); err == nil {
			return filepath.Join(parent, filepath.Base(p))
		}
		return filepath.Clean(p)
	}
	for d := resolve(dir); ; d = filepath.Dir(d) {
		di, derr := os.Stat(d)
		for _, r := range roots {
			if resolve(r) == d {
				return r
			}
			if ri, rerr := os.Stat(r); derr == nil && rerr == nil && os.SameFile(di, ri) {
				return r
			}
		}
		if filepath.Dir(d) == d {
			return ""
		}
	}
}

// baseFileArgs appends the helper's install list flag for a prepare or a tools update to extra, and this Mac's
// inspection CA when it is on: none without either setting.
func (m *macOSVMs) baseFileArgs(extra []string) ([]string, error) {
	if m.items.err != nil {
		return nil, fmt.Errorf("macos_vm_base_file: %w", m.items.err)
	}
	if m.items.caErr != nil {
		return nil, m.items.caErr
	}
	if m.items.file != "" {
		extra = append(extra, "--"+macvm.FlagBaseFile, m.items.file)
	}
	if m.items.ca != nil {
		extra = append(extra, "--"+macvm.FlagInspectionCA, m.items.ca.CertPath())
	}
	return extra, nil
}

// editItems is the install list flag for an edit-base, whose files stage again the profiles still waiting:
// none when the setting is unset or could not be read, and the edit then offers none.
func (m *macOSVMs) editItems() []string {
	a, _ := m.baseFileArgs(nil)
	return a
}

// checkItems is the install list trigger: a base built with another list than the one macos_vm_base_file
// names now needs a tools update, asked for as agent drift is (askDrift, keyed by driftKey): only while
// serving, again after a refused start at most once per driftRetry, and never again once one started for
// that list. A base whose agent drifted is left to checkAgent's update, which installs the list too. A
// list that cannot be read is logged once per error, and the base keeps serving.
func (m *macOSVMs) checkItems(man macvm.Manifest) {
	if m.items.err != nil || (man.AgentVersion != "" && man.AgentVersion != m.version) {
		return
	}
	l, err := m.loadItems()
	if err == nil && !macvm.ItemsChanged(m.layout.BaseDir(0), l.Digest()) {
		return
	}
	if err != nil {
		m.mu.Lock()
		asked := m.items.asked == err.Error()
		m.items.asked = err.Error()
		m.mu.Unlock()
		if !asked {
			slog.Warn("macOS VM base: the install list cannot be read; the base keeps serving as it was built", "error", err.Error())
		}
		return
	}
	m.mu.Lock()
	m.items.asked = "" // a read error after this list is logged again
	m.mu.Unlock()
	if !m.askDrift(m.driftKey(man)) {
		return
	}
	slog.Warn("macOS VM base: the install list changed; a tools update installs it")
	if m.toolsUpdate != nil {
		m.toolsUpdate(man)
	}
}

// loadItems reads the install list macos_vm_base_file names (empty without the setting), with this Mac's
// inspection CA as its last item when it is on.
func (m *macOSVMs) loadItems() (macvm.BaseList, error) {
	if m.items.caErr != nil {
		return macvm.BaseList{}, m.items.caErr
	}
	var l macvm.BaseList
	if m.items.file != "" {
		var err error
		if l, err = macvm.LoadBaseList(m.items.file); err != nil {
			return macvm.BaseList{}, err
		}
	}
	if m.items.ca != nil {
		return macvm.WithInspectionCA(l, m.items.ca.CertPath())
	}
	return l, nil
}

// driftKey is the drift record's key (askDrift, driftStarted) for the tools update of the base whose
// manifest is man: its guest agent's version when that is not this runner's (checkAgent), else the
// install list's digest (checkItems).
func (m *macOSVMs) driftKey(man macvm.Manifest) string {
	if man.AgentVersion != "" && man.AgentVersion != m.version {
		return man.AgentVersion
	}
	l, _ := m.loadItems()
	return "items:" + l.Digest()
}

// baseCustomisation is the install list's items and the guest posture as the base in baseDir records
// them, for the caps.
func baseCustomisation(baseDir string) ([]runnerwire.MacVMItem, *runnerwire.MacVMGuest) {
	rec, _ := macvm.ReadToolsRecord(baseDir)
	var out []runnerwire.MacVMItem
	for _, it := range rec.Items {
		out = append(out, runnerwire.MacVMItem{Kind: it.Kind, Name: it.Name, State: it.State})
	}
	if rec.Guest == nil {
		return out, nil
	}
	p := rec.Guest
	return out, &runnerwire.MacVMGuest{OSVersion: p.OSVersion, OSBuild: p.OSBuild, SIP: p.SIP, Gatekeeper: p.Gatekeeper,
		Profiles: p.Profiles, MDM: p.MDM, OrgCA: p.OrgCA}
}

// dropInspectionCA is the runner's start with macOS VMs off, when no handle opens: it removes this Mac's
// inspection CA as openInspectionCA does, with the setting off or under a drive root, and never makes one.
func dropInspectionCA(set macOSVMSettings) {
	root, err := macvm.DefaultRoot()
	if err != nil || set.inspectionCA == nil {
		return
	}
	m := &macOSVMs{layout: macvm.Layout{Root: root}}
	if _, err := os.Lstat(m.layout.InspectionCADir()); err != nil {
		return
	}
	if *set.inspectionCA && driveRootOver(m.layout.InspectionCADir(), set.driveRoots) == "" {
		return
	}
	m.openInspectionCA(set.inspectionCA, set.driveRoots, true)
}
