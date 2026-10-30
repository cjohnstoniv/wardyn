// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package macvm_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
	"github.com/cjohnstoniv/wardyn/internal/vm/guestagent"
)

// itemGuest is guest with pushes of any mode, recorded in order beside the runs, and item commands that
// fail when fail names them.
type itemGuest struct {
	*guest
	steps []string // "push <dest> <mode>" and "run <argv>", in order
	argvs [][]string
	fail  func(argv []string) int
}

func newItemGuest() *itemGuest { return &itemGuest{guest: newGuest()} }

// itemStage is a ToolsStage with f's options whose boot hands out g.
func (f *toolsFixture) itemStage(g *itemGuest) *macvm.ToolsStage {
	return &macvm.ToolsStage{ToolsOptions: f.opts,
		Boot: func(context.Context, macvm.MachineSpec) (macvm.ToolsGuest, func(context.Context) error, error) {
			return g, func(context.Context) error { return nil }, nil
		}}
}

func (g *itemGuest) Push(_ context.Context, dest string, mode fs.FileMode, r io.Reader, size int64) error {
	b, err := io.ReadAll(r)
	if err != nil || int64(len(b)) != size {
		return fmt.Errorf("push %s: %d bytes of %d: %v", dest, len(b), size, err)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.files[dest] = b
	g.steps = append(g.steps, fmt.Sprintf("push %s %o", dest, mode))
	return nil
}

func (g *itemGuest) Run(ctx context.Context, argv []string) (string, int, error) {
	if !strings.HasPrefix(argv[0], "/usr/bin/security") && !slices.Contains([]string{"/bin/mkdir", "/bin/rm", "/usr/sbin/installer", "/usr/bin/ditto", "/usr/bin/hdiutil"}, argv[0]) {
		return g.guest.Run(ctx, argv)
	}
	g.mu.Lock()
	g.steps = append(g.steps, "run "+strings.Join(argv, " "))
	g.argvs = append(g.argvs, argv)
	g.mu.Unlock()
	if g.fail != nil {
		return "", g.fail(argv), nil
	}
	return "", 0, nil
}

// itemFile writes body to a file in dir and returns its path and pin.
func itemFile(t *testing.T, dir, name, body string) (string, string) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p, shaHex([]byte(body))
}

func listJSON(t *testing.T, items ...map[string]any) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{"version": 1, "items": items})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

var pin64 = strings.Repeat("a", 64)

func TestParseBaseListAcceptsEachKind(t *testing.T) {
	l, err := macvm.ParseBaseList(listJSON(t,
		map[string]any{"kind": "pkg", "name": "edr", "path": "/opt/x/edr.pkg", "sha256": pin64},
		map[string]any{"kind": "app", "name": "tool-zip", "path": "/opt/x/Tool.zip", "sha256": pin64},
		map[string]any{"kind": "app", "name": "tool-dmg", "path": "/opt/x/Tool.dmg", "sha256": pin64, "app": "Tool.app"},
		map[string]any{"kind": "file", "name": "conf", "path": "/opt/x/conf.json", "sha256": pin64, "dest": "/Library/Application Support/Org/conf.json"},
	))
	if err != nil || len(l.Items) != 4 {
		t.Fatalf("%v %+v", err, l)
	}
	if l.Digest() == "" || (macvm.BaseList{}).Digest() != "" {
		t.Fatal("a list has a digest and no list has none")
	}
}

func TestParseBaseListRefuses(t *testing.T) {
	ok := map[string]any{"kind": "pkg", "name": "edr", "path": "/opt/edr.pkg", "sha256": pin64}
	with := func(k string, v any) map[string]any {
		m := map[string]any{}
		for a, b := range ok {
			m[a] = b
		}
		m[k] = v
		return m
	}
	file := func(dest string) map[string]any {
		return map[string]any{"kind": "file", "name": "f", "path": "/opt/f", "sha256": pin64, "dest": dest}
	}
	for name, b := range map[string][]byte{
		"version 2":         []byte(`{"version":2,"items":[]}`),
		"an unknown field":  []byte(`{"version":1,"items":[{"kind":"pkg","name":"a","path":"/a.pkg","sha256":"` + pin64 + `","flags":"-allowUntrusted"}]}`),
		"an unknown kind":   listJSON(t, with("kind", "script")),
		"a bad name":        listJSON(t, with("name", "../x")),
		"a name twice":      listJSON(t, ok, ok),
		"a relative path":   listJSON(t, with("path", "edr.pkg")),
		"a short pin":       listJSON(t, with("sha256", "abc")),
		"a pkg not .pkg":    listJSON(t, with("path", "/opt/edr.zip")),
		"a pkg with dest":   listJSON(t, with("dest", "/Library/x")),
		"a dmg with no app": listJSON(t, map[string]any{"kind": "app", "name": "a", "path": "/opt/a.dmg", "sha256": pin64}),
		"a zip with an app": listJSON(t, map[string]any{"kind": "app", "name": "a", "path": "/opt/a.zip", "sha256": pin64, "app": "A.app"}),
		// A harness's managed settings are written per run (P0), so no item may write them.
		"claude managed settings":      listJSON(t, file(guestagent.MacClaudeCodeManagedSettingsPath)),
		"claude managed, another case": listJSON(t, file("/library/application support/claudecode/managed-settings.json")),
		"codex requirements":           listJSON(t, file(guestagent.MacCodexRequirementsPath)),
		"the run secrets dir":          listJSON(t, file(guestagent.MacRunDir+"/x")),
		"the stage dir":                listJSON(t, file(guestagent.MacItemStageDir+"/edr/item.pkg")),
		"a harness CLI":                listJSON(t, file("/usr/local/bin/claude")),
		"the agent":                    listJSON(t, file(guestagent.MacAgentBinaryPath)),
		"the system keychain":          listJSON(t, file("/Library/Keychains/System.keychain")),
		"outside the roots":            listJSON(t, file("/Users/agent/.zshrc")),
		"a root itself":                listJSON(t, file("/Library")),
		"not clean":                    listJSON(t, file("/Library/Org/../Keychains/x")),
	} {
		if _, err := macvm.ParseBaseList(b); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// The argv sequence of each kind, in order, each one a form the guest agent allows, and every staged file
// in its item's directory.
func TestInstallItemsArgvSequence(t *testing.T) {
	f, dir := newTools(t), t.TempDir()
	pkg, pkgPin := itemFile(t, dir, "edr.pkg", "pkg-bytes")
	zip, zipPin := itemFile(t, dir, "Tool.zip", "zip-bytes")
	dmg, dmgPin := itemFile(t, dir, "Tool.dmg", "dmg-bytes")
	conf, confPin := itemFile(t, dir, "conf.json", "{}")
	l, err := macvm.ParseBaseList(listJSON(t,
		map[string]any{"kind": "pkg", "name": "edr", "path": pkg, "sha256": pkgPin},
		map[string]any{"kind": "app", "name": "zip", "path": zip, "sha256": zipPin},
		map[string]any{"kind": "app", "name": "dmg", "path": dmg, "sha256": dmgPin, "app": "Tool.app"},
		map[string]any{"kind": "file", "name": "conf", "path": conf, "sha256": confPin, "dest": "/Library/Application Support/Org/conf.json"},
	))
	if err != nil {
		t.Fatal(err)
	}
	f.opts.Items = l
	g := newItemGuest()
	res := runStage(t, f.itemStage(g))
	S := guestagent.MacItemStageDir
	want := []string{
		"run /bin/mkdir -p -m 0700 " + S,
		"run /bin/mkdir -p -m 0700 " + S + "/edr", "push " + S + "/edr/item.pkg 600",
		"run /usr/sbin/installer -pkg " + S + "/edr/item.pkg -target /", "run /bin/rm -rf " + S + "/edr",
		"run /bin/mkdir -p -m 0700 " + S + "/zip", "push " + S + "/zip/item.zip 600",
		"run /usr/bin/ditto -x -k " + S + "/zip/item.zip /Applications", "run /bin/rm -rf " + S + "/zip",
		"run /bin/mkdir -p -m 0700 " + S + "/dmg", "push " + S + "/dmg/item.dmg 600",
		"run /usr/bin/hdiutil attach -nobrowse -readonly -noautoopen -mountpoint " + S + "/dmg/mnt " + S + "/dmg/item.dmg",
		"run /usr/bin/ditto " + S + "/dmg/mnt/Tool.app /Applications/Tool.app",
		"run /usr/bin/hdiutil detach -force " + S + "/dmg/mnt", "run /bin/rm -rf " + S + "/dmg",
		"run /bin/mkdir -p -m 0755 /Library/Application Support/Org", "push /Library/Application Support/Org/conf.json 644",
	}
	steps := g.steps[slices.Index(g.steps, want[0]):] // after the harnesses
	if !slices.Equal(steps, want) {
		t.Fatalf("steps:\n%s\nwant:\n%s", strings.Join(steps, "\n"), strings.Join(want, "\n"))
	}
	for _, r := range g.argvs {
		if err := guestagent.ToolsExecAllowed(r, guestagent.MacToolsBinDir); err != nil {
			t.Errorf("the guest refuses %q: %v", r, err)
		}
	}
	if len(res.Items) != 4 || res.Items[0] != (macvm.ItemRecord{Kind: "pkg", Name: "edr", SHA256: pkgPin, State: macvm.ItemInstalled}) || res.ItemsDigest != l.Digest() {
		t.Fatalf("items %+v digest %q", res.Items, res.ItemsDigest)
	}
}

// A file that does not match its pin is never pushed, and stops stage 2; an installer that fails stops it
// too, and its stage directory is still removed.
func TestInstallItemsFailures(t *testing.T) {
	dir := t.TempDir()
	pkg, pkgPin := itemFile(t, dir, "edr.pkg", "pkg-bytes")
	for name, tc := range map[string]struct {
		pin  string
		fail func([]string) int
	}{
		"pin mismatch":    {pin: pin64},
		"installer fails": {pin: pkgPin, fail: func(argv []string) int { return map[bool]int{true: 1}[argv[0] == "/usr/sbin/installer"] }},
	} {
		t.Run(name, func(t *testing.T) {
			f := newTools(t)
			f.opts.Items = macvm.BaseList{Version: 1, Items: []macvm.BaseItem{{Kind: "pkg", Name: "edr", Path: pkg, SHA256: tc.pin}}}
			g := newItemGuest()
			g.fail = tc.fail
			_, err := f.itemStage(g).Run(context.Background(), macvm.Stage2Input{})
			if err == nil || !strings.Contains(err.Error(), "base item edr (pkg)") {
				t.Fatalf("stage 2: %v", err)
			}
			if last := g.steps[len(g.steps)-1]; last != "run /bin/rm -rf "+guestagent.MacItemStageDir+"/edr" {
				t.Fatalf("the stage dir was not removed last: %q", g.steps)
			}
			if _, pushed := g.files[guestagent.MacItemStageDir+"/edr/item.pkg"]; pushed == (tc.fail == nil) {
				t.Fatalf("pushed %v with fail %v", pushed, tc.fail != nil)
			}
			if len(g.agentRuns) != 0 {
				t.Fatal("the self-test ran after a failed item")
			}
		})
	}
}

// The record keeps the list's digest; a changed list is a changed digest, which is what starts a tools update.
func TestItemsDigestRecordedAndCompared(t *testing.T) {
	dir := t.TempDir()
	pkg, pkgPin := itemFile(t, dir, "edr.pkg", "pkg-bytes")
	l := macvm.BaseList{Version: 1, Items: []macvm.BaseItem{{Kind: "pkg", Name: "edr", Path: pkg, SHA256: pkgPin}}}
	base := t.TempDir()
	if macvm.ItemsChanged(base, "") {
		t.Fatal("a base with no record was built with no list")
	}
	rec := macvm.ToolsRecord{ItemsDigest: l.Digest()}
	b, _ := json.Marshal(rec)
	if err := os.WriteFile(filepath.Join(base, macvm.ToolsRecordFile), b, 0o600); err != nil {
		t.Fatal(err)
	}
	l2 := l
	l2.Items = []macvm.BaseItem{{Kind: "pkg", Name: "edr", Path: pkg, SHA256: pin64}}
	if macvm.ItemsChanged(base, l.Digest()) || !macvm.ItemsChanged(base, l2.Digest()) || !macvm.ItemsChanged(base, "") {
		t.Fatal("ItemsChanged")
	}
}
