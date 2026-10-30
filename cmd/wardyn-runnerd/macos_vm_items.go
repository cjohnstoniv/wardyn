// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"log/slog"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
)

// The macOS base's install list (macos_vm_base_file): the helper flag a prepare and a tools update pass,
// the trigger that starts a tools update when the list changes, and what the caps carry of it.

// macOSVMBaseFile reads the runner setting macos_vm_base_file (tests replace it).
var macOSVMBaseFile = func() (string, error) {
	c, err := effectiveRunnerConfig("", nil)
	if err != nil {
		return "", err
	}
	return c.get("macos_vm_base_file"), nil
}

// macOSVMItems is the install list's state: file is the setting (err: it could not be read, so neither a
// prepare nor a tools update starts); asked is the list digest a tools update was last asked for.
type macOSVMItems struct {
	file  string
	err   error
	asked string
}

// openMacOSVMItems reads the setting once, when the runner opens its macOS VMs.
func openMacOSVMItems() (it macOSVMItems) {
	it.file, it.err = macOSVMBaseFile()
	return it
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
// names now needs a tools update, asked for once per list digest. A base whose agent drifted is left to
// checkAgent's update, which installs the list too. A list that cannot be read is logged once per error,
// and the base keeps serving.
func (m *macOSVMs) checkItems(man macvm.Manifest) {
	if m.items.err != nil || (man.AgentVersion != "" && man.AgentVersion != m.version) {
		return
	}
	var l macvm.BaseList
	var err error
	if m.items.file != "" {
		l, err = macvm.LoadBaseList(m.items.file)
	}
	if err == nil && !macvm.ItemsChanged(m.layout.BaseDir(0), l.Digest()) {
		return
	}
	key := l.Digest()
	if err != nil {
		key = err.Error()
	}
	m.mu.Lock()
	asked := m.items.asked == key
	m.items.asked = key
	m.mu.Unlock()
	switch {
	case asked:
	case err != nil:
		slog.Warn("macOS VM base: the install list cannot be read; the base keeps serving as it was built", "error", err.Error())
	default:
		slog.Warn("macOS VM base: the install list changed; a tools update installs it")
		if m.toolsUpdate != nil {
			m.toolsUpdate(man)
		}
	}
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
