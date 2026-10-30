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
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
	"github.com/cjohnstoniv/wardyn/internal/macvm/admission"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
)

// writeBaseList writes an install list of one pkg item pinned at pin and returns its path.
func writeBaseList(t *testing.T, dir, pin string) string {
	t.Helper()
	p := filepath.Join(dir, "base-items.json")
	b := `{"version":1,"items":[{"kind":"pkg","name":"edr","path":"/opt/edr.pkg","sha256":"` + pin + `"}]}`
	if err := os.WriteFile(p, []byte(b), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func writeToolsRecord(t *testing.T, base string, rec macvm.ToolsRecord) {
	t.Helper()
	b, _ := json.Marshal(rec)
	if err := os.WriteFile(filepath.Join(base, macvm.ToolsRecordFile), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// macos_vm_base_file has a managed key and a runnerd.json field, and no flag or environment variable, so
// the managed value is the value.
func TestRunnerConfigMacOSVMBaseFile(t *testing.T) {
	c := resolveRunnerConfig(inputs(nil, nil, map[string]string{"MacOSVMBaseFile": "/Library/Org/items.json"}, runnerFile{MacOSVMBaseFile: "/x.json"}))
	if f := c.field("macos_vm_base_file"); f.Value != "/Library/Org/items.json" || !strings.HasPrefix(f.Source, sourceManaged) {
		t.Fatalf("%+v", f)
	}
	if got := resolveRunnerConfig(inputs(nil, nil, nil, runnerFile{MacOSVMBaseFile: "/x.json"})).get("macos_vm_base_file"); got != "/x.json" {
		t.Fatalf("file: %q", got)
	}
	for _, f := range runnerFields {
		if f.name == "macos_vm_base_file" && (f.flag != "" || f.env != nil) {
			t.Fatalf("%+v", f)
		}
	}
}

// A prepare and a tools update pass the helper the install list; a setting that could not be read starts
// neither.
func TestMacOSVMJobsPassTheInstallList(t *testing.T) {
	r := newMacLedgerRig(t)
	r.host.setProcs(admission.Proc{PID: os.Getpid(), UID: 501, Start: 3, Name: "wardyn-runnerd"})
	m := r.open(t)
	prev, prevEx := prepareClaimWait, excludeFromBackups
	prepareClaimWait, excludeFromBackups = 2*time.Second, func(string) error { return nil }
	t.Cleanup(func() { prepareClaimWait, excludeFromBackups = prev, prevEx })
	var args string
	m.helper, args = fakeHelper(t, 0)
	m.baseFile = "/Library/Org/items.json"
	man, _ := macvm.ReadManifest(m.layout.BaseDir(0))
	_ = m.startToolsUpdate(context.Background(), man)
	_ = m.startPrepare(context.Background(), prepareArgs{helper: m.helper, runnerID: "runner-a", cpus: 4, memoryMiB: 8192, newImage: true})
	var jobs int
	for _, c := range calls(t, args) {
		f := strings.Fields(c)
		if len(f) == 0 || f[0] == "probe" {
			continue
		}
		jobs++
		if i := slices.Index(f, "--"+macvm.FlagBaseFile); i < 0 || i+1 >= len(f) || f[i+1] != m.baseFile {
			t.Errorf("%s has no --%s %s", f[0], macvm.FlagBaseFile, m.baseFile)
		}
	}
	if jobs != 2 {
		t.Fatalf("jobs: %q", calls(t, args))
	}
	m.baseFileErr = errors.New("managed preference unreadable")
	if err := m.startToolsUpdate(context.Background(), man); err == nil || !strings.Contains(err.Error(), "macos_vm_base_file") {
		t.Fatalf("tools update: %v", err)
	}
	if err := m.startPrepare(context.Background(), prepareArgs{helper: m.helper, runnerID: "runner-a", cpus: 4, memoryMiB: 8192, newImage: true}); err == nil ||
		!strings.Contains(err.Error(), "macos_vm_base_file") {
		t.Fatalf("prepare: %v", err)
	}
}

// A base built with another list than the setting names now gets one tools update per list digest; a
// base whose agent drifted is left to that update, and a list that cannot be read starts none.
func TestMacOSVMInstallListChangeStartsAToolsUpdate(t *testing.T) {
	r := newMacLedgerRig(t)
	m := r.open(t)
	base := m.layout.BaseDir(0)
	var asked int
	m.toolsUpdate = func(macvm.Manifest) { asked++ }
	man, err := macvm.ReadManifest(base)
	if err != nil {
		t.Fatal(err)
	}
	m.checkItems(man)
	if asked != 0 {
		t.Fatal("no list and no record asked for an update")
	}
	dir := t.TempDir()
	m.baseFile = writeBaseList(t, dir, strings.Repeat("a", 64))
	m.checkItems(man)
	m.checkItems(man)
	if asked != 1 {
		t.Fatalf("a new list: %d updates", asked)
	}
	l, _ := macvm.LoadBaseList(m.baseFile)
	writeToolsRecord(t, base, macvm.ToolsRecord{ItemsDigest: l.Digest()})
	m.baseFile = writeBaseList(t, dir, strings.Repeat("b", 64))
	m.checkItems(man)
	if asked != 2 {
		t.Fatalf("a changed list: %d updates", asked)
	}
	m.baseFile = writeBaseList(t, dir, "not-a-pin")
	m.checkItems(man)
	drifted := man
	drifted.AgentVersion = "0.0.1-old"
	m.baseFile = writeBaseList(t, dir, strings.Repeat("c", 64))
	m.checkItems(drifted)
	if asked != 2 {
		t.Fatalf("a bad list or a drifted agent: %d updates", asked)
	}
}

// The caps carry the base's items as tools.json records them.
func TestMacOSVMCapsCarryTheBaseItems(t *testing.T) {
	r := newMacLedgerRig(t)
	m := r.open(t)
	writeToolsRecord(t, m.layout.BaseDir(0), macvm.ToolsRecord{Items: []macvm.ItemRecord{{Kind: "pkg", Name: "edr", SHA256: strings.Repeat("a", 64), State: macvm.ItemInstalled}}})
	_, b := m.base()
	if want := []runnerwire.MacVMItem{{Kind: runnerwire.MacVMItemKindPkg, Name: "edr", State: runnerwire.MacVMItemInstalled}}; !slices.Equal(b.Customisation, want) {
		t.Fatalf("%+v", b.Customisation)
	}
}
