// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package main

import (
	"fmt"
	"log/slog"

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
}

// baseFileArgs appends the helper's install list flag for a prepare or a tools update to extra: none
// without the setting.
func (m *macOSVMs) baseFileArgs(extra []string) ([]string, error) {
	if m.items.err != nil {
		return nil, fmt.Errorf("macos_vm_base_file: %w", m.items.err)
	}
	if m.items.file == "" {
		return extra, nil
	}
	return append(extra, "--"+macvm.FlagBaseFile, m.items.file), nil
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

// loadItems reads the install list macos_vm_base_file names: an empty list without the setting.
func (m *macOSVMs) loadItems() (macvm.BaseList, error) {
	if m.items.file == "" {
		return macvm.BaseList{}, nil
	}
	return macvm.LoadBaseList(m.items.file)
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
