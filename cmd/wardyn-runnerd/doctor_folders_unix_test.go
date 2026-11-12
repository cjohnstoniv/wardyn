// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/doctor"
	"github.com/cjohnstoniv/wardyn/internal/macvm"
)

// folderSharesFor is doctor's folder-shares stage among macOSVMStagesFor's.
func folderSharesFor(t *testing.T, roots []string, tools macvm.ToolsRecord, edit *macvm.EditRecord) []doctor.Stage {
	t.Helper()
	return slices.DeleteFunc(macOSVMStagesFor(t, roots, tools, edit), func(s doctor.Stage) bool { return s.Name != "folder-shares" })
}

// macOSVMStagesFor is doctor's macOS VM stages against fake records in a temporary home: the base's
// manifest, its tools.json and, when edit is not nil, its edit record.
func macOSVMStagesFor(t *testing.T, roots []string, tools macvm.ToolsRecord, edit *macvm.EditRecord) []doctor.Stage {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	root, err := macvm.DefaultRoot()
	if err != nil {
		t.Fatal(err)
	}
	base := macvm.Layout{Root: root}.BaseDir(0)
	if err := os.MkdirAll(base, 0o700); err != nil {
		t.Fatal(err)
	}
	created := time.Date(2026, 10, 20, 8, 0, 0, 0, time.UTC)
	put := func(path string, v any) {
		b, _ := json.Marshal(v)
		if err := os.WriteFile(path, b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	put(filepath.Join(base, macvm.ManifestFile), macvm.Manifest{Format: 2, State: "ready", CreatedAt: created})
	put(filepath.Join(base, macvm.ToolsRecordFile), tools)
	if edit != nil {
		edit.BaseCreatedAt = created
		put(base+".edit.json", edit)
	}
	return macOSVMDoctorStages(roots, "WARDYN_DRIVE_ROOT")
}

// The stage reads the tools record's guest.full_disk_access and agent_signature and the edit that read it:
// the same fake records give the kept reading for an agent signed on this Mac and the "updated after"
// line for one signed ad hoc.
func TestMacOSVMDoctorFolderSharesReadsTheBaseRecords(t *testing.T) {
	read := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	edit := &macvm.EditRecord{ReadAt: read, FullDiskAccess: "allowed"}
	tools := func(sig string) macvm.ToolsRecord {
		return macvm.ToolsRecord{TriedAt: read.Add(72 * time.Hour), AgentSignature: sig, Guest: &macvm.GuestPosture{FullDiskAccess: "allowed"}}
	}
	roots := []string{"/Users/a/Work"}

	st := folderSharesFor(t, roots, tools(macvm.AgentSignedThisMac), edit)
	if len(st) != 1 || st[0].Name != "folder-shares" || st[0].Status != doctor.StatusOK || !strings.Contains(st[0].Detail, "as the base last read it") {
		t.Fatalf("signed on this Mac, tools newer: %+v", st)
	}
	st = folderSharesFor(t, roots, tools(macvm.AgentSignedAdHoc), edit)
	if len(st) != 1 || st[0].Status != doctor.StatusSkip || !strings.Contains(st[0].Detail, "tools were updated after") {
		t.Fatalf("signed ad hoc, tools newer: %+v", st)
	}
	// A tools update that dropped the reading leaves none: not read, whatever the edit record held.
	dropped := tools(macvm.AgentSignedAdHoc)
	dropped.Guest.FullDiskAccess = ""
	if st = folderSharesFor(t, roots, dropped, edit); len(st) != 1 || !strings.Contains(st[0].Detail, "has not been read") {
		t.Fatalf("dropped reading: %+v", st)
	}
	// No base, no stage.
	t.Setenv("HOME", t.TempDir())
	if st = macOSVMDoctorStages(roots, "x"); len(st) != 0 {
		t.Fatalf("no base: %+v", st)
	}
}

// A base whose guest FileVault was last read as on adds a file-vault skip beside folder-shares; off, unread
// and no guest posture add nothing.
func TestMacOSVMDoctorWarnsWhenFileVaultIsOn(t *testing.T) {
	roots := []string{"/Users/a/Work"}
	st := fileVaultAndFolders(t, roots, macvm.ToolsRecord{Guest: &macvm.GuestPosture{FileVault: "on"}}, nil)
	if len(st) != 2 || st[0].Name != "file-vault" || st[0].Status != doctor.StatusSkip || !strings.Contains(st[0].Detail, "Turn it off") || st[1].Name != "folder-shares" {
		t.Fatalf("FileVault on: %+v", st)
	}
	for _, g := range []*macvm.GuestPosture{{FileVault: "off"}, {}, nil} {
		if st := fileVaultAndFolders(t, roots, macvm.ToolsRecord{Guest: g}, nil); len(st) != 1 || st[0].Name != "folder-shares" {
			t.Errorf("guest %+v: %+v", g, st)
		}
	}
}

// fileVaultAndFolders is doctor's macOS VM stages without the guest-agent stage: file-vault, when shown, then
// folder-shares.
func fileVaultAndFolders(t *testing.T, roots []string, tools macvm.ToolsRecord, edit *macvm.EditRecord) []doctor.Stage {
	t.Helper()
	return slices.DeleteFunc(macOSVMStagesFor(t, roots, tools, edit), func(s doctor.Stage) bool { return s.Name == "guest-agent" })
}
