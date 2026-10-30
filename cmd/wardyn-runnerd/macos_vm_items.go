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

// baseFileArgs is the helper's install list flag for a prepare or a tools update: none without the setting.
func (m *macOSVMs) baseFileArgs() ([]string, error) {
	if m.baseFileErr != nil {
		return nil, fmt.Errorf("macos_vm_base_file: %w", m.baseFileErr)
	}
	if m.baseFile == "" {
		return nil, nil
	}
	return []string{"--" + macvm.FlagBaseFile, m.baseFile}, nil
}

// checkItems is the install list trigger: a base built with another list than the one macos_vm_base_file
// names now needs a tools update, asked for once per list digest. A base whose agent drifted is left to
// checkAgent's update, which installs the list too. A list that cannot be read is logged once per error,
// and the base keeps serving.
func (m *macOSVMs) checkItems(man macvm.Manifest) {
	if m.baseFileErr != nil || (man.AgentVersion != "" && man.AgentVersion != m.version) {
		return
	}
	var l macvm.BaseList
	var err error
	if m.baseFile != "" {
		l, err = macvm.LoadBaseList(m.baseFile)
	}
	if err == nil && !macvm.ItemsChanged(m.layout.BaseDir(0), l.Digest()) {
		return
	}
	key := l.Digest()
	if err != nil {
		key = err.Error()
	}
	m.mu.Lock()
	asked := m.itemsAsked == key
	m.itemsAsked = key
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
