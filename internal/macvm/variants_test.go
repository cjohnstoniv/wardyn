// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package macvm_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
)

// A derived base is a clone of Standard whose stage-2 boot installs its own list and no tools; it is current
// until Standard changes, and is then derived again.
func TestDeriveBaseClonesStandardAndInstallsItsList(t *testing.T) {
	f, tf, _ := stage2Prep(t)
	if _, err := macvm.Prepare(context.Background(), f.opts); err != nil {
		t.Fatal(err)
	}
	std, dir := f.opts.Layout.BaseDir(0), f.opts.Layout.VariantDir("xcode-16")
	p, pin := itemFile(t, t.TempDir(), "xcode.cfg", "xcode-config")
	list := macvm.BaseList{Version: 1, Items: []macvm.BaseItem{{Kind: "file", Name: "xcode-cfg", Path: p, SHA256: pin, Dest: "/Library/Application Support/Org/xcode.cfg"}}}
	derive := func() (*itemGuest, macvm.Manifest, bool) {
		t.Helper()
		g := newItemGuest()
		tf.opts.Items = list
		o := updateOpts(f, tf, g.guest, "0.10.0")
		o.Stage, o.ItemsDigest = tf.itemStage(g), list.Digest()
		m, changed, err := macvm.DeriveBase(context.Background(), o, "xcode-16")
		if err != nil {
			t.Fatal(err)
		}
		return g, m, changed
	}
	g, m, changed := derive()
	stamp, err := macvm.BaseStamp(std)
	if err != nil || !changed || m.DerivedFrom != stamp || !macvm.VariantCurrent(dir, stamp, list.Digest()) {
		t.Fatalf("derive: changed %v manifest %+v stamp %s err %v", changed, m, stamp, err)
	}
	if slices.ContainsFunc(g.runs, func(argv []string) bool { return strings.HasSuffix(argv[0], "softwareupdate") }) {
		t.Fatal("a derivation installed the Command Line Tools again: it keeps Standard's tools")
	}
	if string(g.files["/Library/Application Support/Org/xcode.cfg"]) != "xcode-config" {
		t.Fatalf("the derived base's item was not installed: %v", g.steps)
	}
	rec, err := macvm.ReadToolsRecord(dir)
	if err != nil || rec.ItemsDigest != list.Digest() || len(rec.Items) != 1 || !slices.Contains(rec.Harnesses, macvm.HarnessClaudeCode) {
		t.Fatalf("derived tools record %+v %v", rec, err)
	}
	if _, err := os.Stat(dir + ".update"); !os.IsNotExist(err) {
		t.Fatal("the derivation's staging dir was left")
	}
	// Current: nothing runs.
	if g, _, changed := derive(); changed || len(g.steps) != 0 {
		t.Fatalf("a current derived base was derived again: %v", g.steps)
	}
	// Standard changes: the derived base is stale, and derived again.
	stdRec, _ := macvm.ReadToolsRecord(std)
	stdRec.Harnesses = append(stdRec.Harnesses, "extra")
	writeToolsJSON(t, std, stdRec)
	if macvm.VariantCurrent(dir, mustStamp(t, std), list.Digest()) {
		t.Fatal("a derived base of the old Standard reads as current")
	}
	if _, m, changed := derive(); !changed || m.DerivedFrom != mustStamp(t, std) || !slices.Contains(m.Harnesses, "extra") {
		t.Fatalf("re-derive: changed %v manifest %+v", changed, m)
	}
}

func mustStamp(t *testing.T, dir string) string {
	t.Helper()
	s, err := macvm.BaseStamp(dir)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func writeToolsJSON(t *testing.T, dir string, rec macvm.ToolsRecord) {
	t.Helper()
	b, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, macvm.ToolsRecordFile), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLoadBaseVariants(t *testing.T) {
	dir := t.TempDir()
	p, pin := itemFile(t, t.TempDir(), "a.pkg", "pkg")
	write := func(name string, b []byte) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("xcode-16.json", listJSON(t, map[string]any{"kind": "pkg", "name": "xcode", "path": p, "sha256": pin}))
	write("security-tools.json", listJSON(t))
	write("README.txt", []byte("not a list"))
	vs, err := macvm.LoadBaseVariants(dir)
	if err != nil || len(vs) != 2 || vs[0].Name != "security-tools" || vs[1].Name != "xcode-16" || len(vs[1].List.Items) != 1 {
		t.Fatalf("variants %+v %v", vs, err)
	}
	for name, body := range map[string][]byte{
		"Standard.json": listJSON(t),
		"xcode.json":    listJSON(t, map[string]any{"kind": "profile", "name": "wifi", "path": p, "sha256": pin, "identifier": "org.example.wifi"}),
	} {
		bad := t.TempDir()
		if err := os.WriteFile(filepath.Join(bad, name), body, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := macvm.LoadBaseVariants(bad); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

func TestNamedBaseDir(t *testing.T) {
	l := macvm.Layout{Root: "/r"}
	for name, want := range map[string]string{"": l.BaseDir(0), "Standard": l.BaseDir(0), "xcode-16": "/r/variants/xcode-16"} {
		if got, err := l.NamedBaseDir(name); err != nil || got != want {
			t.Errorf("NamedBaseDir(%q) = %q, %v", name, got, err)
		}
	}
	for _, name := range []string{"../bases/0", "Xcode", "a/b", "."} {
		if _, err := l.NamedBaseDir(name); err == nil {
			t.Errorf("NamedBaseDir(%q) was accepted", name)
		}
	}
}
