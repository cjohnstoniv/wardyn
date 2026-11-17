// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
	"github.com/cjohnstoniv/wardyn/internal/macvm/admission"
)

// macos_vm_inspection_ca has a managed key and a runnerd.json field, and no flag or environment variable, so
// the managed value is the value; a managed 0 keeps it off.
func TestRunnerConfigMacOSVMInspectionCA(t *testing.T) {
	c := resolveRunnerConfig(inputs(nil, nil, map[string]string{"MacOSVMInspectionCA": "1"}, runnerFile{MacOSVMInspectionCA: "0"}))
	if f := c.field("macos_vm_inspection_ca"); f.Value != "1" || !strings.HasPrefix(f.Source, sourceManaged) {
		t.Fatalf("managed 1: %+v", f)
	}
	if s := c.macOSVMSettings(); s.inspectionCA == nil || !*s.inspectionCA {
		t.Errorf("managed 1: %v", s.inspectionCA)
	}
	c = resolveRunnerConfig(inputs(nil, nil, map[string]string{"MacOSVMInspectionCA": "0"}, runnerFile{MacOSVMInspectionCA: "1"}))
	if f := c.field("macos_vm_inspection_ca"); f.Value != "0" || !strings.HasPrefix(f.Source, sourceManaged) {
		t.Fatalf("managed 0: %+v", f)
	}
	if s := c.macOSVMSettings(); s.inspectionCA == nil || *s.inspectionCA {
		t.Errorf("managed 0: %v", s.inspectionCA)
	}
	c = resolveRunnerConfig(inputs(nil, nil, nil, runnerFile{MacOSVMInspectionCA: "1"}))
	if f := c.field("macos_vm_inspection_ca"); f.Value != "1" {
		t.Fatalf("file: %+v", f)
	}
	if s := c.macOSVMSettings(); s.inspectionCA == nil || !*s.inspectionCA {
		t.Errorf("file: %v", s.inspectionCA)
	}
	if s := resolveRunnerConfig(inputs(nil, nil, nil, runnerFile{})).macOSVMSettings(); s.inspectionCA == nil || *s.inspectionCA {
		t.Errorf("empty: %v", s.inspectionCA)
	}
	c = resolveRunnerConfig(inputs(nil, nil, nil, runnerFile{DriveRoot: []string{"/a", "/b"}}))
	if got := c.macOSVMSettings().driveRoots; !slices.Equal(got, []string{"/a", "/b"}) {
		t.Errorf("drive roots: %q", got)
	}
	for _, f := range runnerFields {
		if f.name == "macos_vm_inspection_ca" && (f.flag != "" || f.env != nil || f.locked) {
			t.Fatalf("%+v", f)
		}
	}
}

// inspectionCAJobRig is a runner handle with a ready base and a fake helper.
func inspectionCAJobRig(t *testing.T) (*macLedgerRig, *macOSVMs, string) {
	t.Helper()
	r := newMacLedgerRig(t)
	r.host.setProcs(admission.Proc{PID: os.Getpid(), UID: 501, Start: 3, Name: "wardyn-runnerd"})
	m := r.open(t)
	prev, prevEx := prepareClaimWait, excludeFromBackups
	prepareClaimWait, excludeFromBackups = 2*time.Second, func(string) error { return nil }
	t.Cleanup(func() { prepareClaimWait, excludeFromBackups = prev, prevEx })
	var args string
	m.helper, args = fakeHelper(t, 0)
	return r, m, args
}

// With the setting on, the install list gains the CA, and a prepare, a tools update and an edit-base each pass
// the helper its certificate.
func TestMacOSVMInspectionCAJoinsEveryListJob(t *testing.T) {
	_, m, args := inspectionCAJobRig(t)
	on := true
	m.openInspectionCA(&on, nil, true)
	if m.items.ca == nil || m.items.caErr != nil {
		t.Fatalf("ca %v, error %v", m.items.ca, m.items.caErr)
	}
	cert := m.items.ca.CertPath()
	if want := filepath.Join(m.layout.InspectionCADir(), "ca.pem"); cert != want {
		t.Errorf("CertPath %q, want %q", cert, want)
	}
	l, err := m.loadItems()
	if err != nil || len(l.Items) != 1 || l.Items[0].Name != macvm.InspectionCAItem {
		t.Fatalf("loadItems: %v, %+v", err, l.Items)
	}
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
		if i := slices.Index(f, "--"+macvm.FlagInspectionCA); i < 0 || i+1 >= len(f) || f[i+1] != cert {
			t.Errorf("%s has no --%s %s", f[0], macvm.FlagInspectionCA, cert)
		}
	}
	if jobs != 2 {
		t.Fatalf("jobs: %q", calls(t, args))
	}
	if a := m.editItems(); !slices.Equal(a, []string{"--" + macvm.FlagInspectionCA, cert}) {
		t.Errorf("edit-base's args %q", a)
	}
}

// The VM directory is excluded from Time Machine before the CA's key is made, not only at the first prepare;
// a start that only reads the CA adds no exclusion.
func TestMacOSVMInspectionCAExcludesTheDirectoryBeforeTheKey(t *testing.T) {
	_, m, _ := inspectionCAJobRig(t)
	var got []string
	excludeFromBackups = func(dir string) error {
		if _, err := os.Stat(filepath.Join(m.layout.InspectionCADir(), "key.pem")); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("the key was there before the exclusion: %v", err)
		}
		got = append(got, dir)
		return nil
	}
	on := true
	m.openInspectionCA(&on, nil, false)
	if len(got) != 0 {
		t.Fatalf("a read-only start excluded %q", got)
	}
	m.openInspectionCA(&on, nil, true)
	if m.items.ca == nil || !slices.Equal(got, []string{m.layout.Root}) {
		t.Fatalf("ca %v, exclusions %q, want [%s]", m.items.ca, got, m.layout.Root)
	}
}

// A CA that cannot be opened starts no prepare or tools update, as a list that cannot be read starts none.
func TestMacOSVMInspectionCAThatCannotOpenStartsNoListJob(t *testing.T) {
	_, m, _ := inspectionCAJobRig(t)
	if err := os.MkdirAll(m.layout.InspectionCADir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(m.layout.InspectionCADir(), 0o755); err != nil {
		t.Fatal(err)
	}
	on := true
	m.openInspectionCA(&on, nil, true)
	if m.items.ca != nil || m.items.caErr == nil || !strings.Contains(m.items.caErr.Error(), "macos_vm_inspection_ca") {
		t.Fatalf("ca %v, error %v", m.items.ca, m.items.caErr)
	}
	if a := m.editItems(); a != nil {
		t.Errorf("edit-base's args: %q", a)
	}
	man, _ := macvm.ReadManifest(m.layout.BaseDir(0))
	if err := m.startToolsUpdate(context.Background(), man); err == nil || !strings.Contains(err.Error(), "macos_vm_inspection_ca") {
		t.Fatalf("tools update: %v", err)
	}
	if _, err := m.loadItems(); err == nil {
		t.Error("loadItems read a list with no usable CA")
	}
}

func TestMacOSVMInspectionCAOffRemovesTheKey(t *testing.T) {
	r := newMacLedgerRig(t)
	m := r.open(t)
	on, off := true, false
	m.openInspectionCA(&on, nil, true)
	inspectionCAGone(t, m, false, "on")
	m.items = macOSVMItems{}
	m.openInspectionCA(nil, nil, true) // setup's prepare resolves no settings
	inspectionCAGone(t, m, false, "no settings")
	if m.items.ca != nil {
		t.Error("no settings opened a CA")
	}
	m.items = macOSVMItems{}
	m.openInspectionCA(&off, nil, true)
	inspectionCAGone(t, m, true, "off")
	if m.items.ca != nil || m.items.caErr != nil {
		t.Errorf("ca %v, error %v", m.items.ca, m.items.caErr)
	}
	if l, err := m.loadItems(); err != nil || len(l.Items) != 0 {
		t.Errorf("loadItems: %v, %d items", err, len(l.Items))
	}
	if slices.Contains(m.editItems(), "--"+macvm.FlagInspectionCA) {
		t.Error("edit-base still passes the CA")
	}
}

// A drive root that is, or holds, the VM directory keeps the CA off, and removes one that is there: a run's
// mount of that folder could read the key.
func TestMacOSVMInspectionCAUnderADriveRootStaysOff(t *testing.T) {
	r := newMacLedgerRig(t)
	m := r.open(t)
	on := true
	m.openInspectionCA(&on, []string{t.TempDir()}, true)
	if m.items.ca == nil {
		t.Fatalf("a drive root elsewhere kept the CA off: %v", m.items.caErr)
	}
	for _, roots := range [][]string{{filepath.Dir(m.layout.Root)}, {m.layout.InspectionCADir()}} {
		m.items = macOSVMItems{}
		m.openInspectionCA(&on, roots, true)
		if m.items.ca != nil || m.items.caErr != nil {
			t.Errorf("%q: ca %v, error %v", roots, m.items.ca, m.items.caErr)
		}
		inspectionCAGone(t, m, true, roots[0])
		if slices.Contains(m.editItems(), "--"+macvm.FlagInspectionCA) {
			t.Errorf("%q: edit-base still passes the CA", roots)
		}
		m.items = macOSVMItems{}
		m.openInspectionCA(&on, nil, true) // makes a new CA
		inspectionCAGone(t, m, false, "made again")
	}
}

// A drive root over the real folder the VM directory is in, reached through a symlink, counts.
func TestMacOSVMInspectionCAUnderADriveRootThroughASymlinkStaysOff(t *testing.T) {
	r := newMacLedgerRig(t)
	real := t.TempDir()
	inner := filepath.Join(real, "inner")
	if err := os.Mkdir(inner, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(inner, link); err != nil {
		t.Fatal(err)
	}
	r.root = filepath.Join(link, "macos-vm.noindex")
	m := r.open(t)
	on := true
	m.openInspectionCA(&on, []string{real}, true)
	if m.items.ca != nil {
		t.Error("a CA opened under a drive root")
	}
	k, _ := inspectionCAFiles(m)
	if _, err := os.Lstat(k); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("key.pem: %v", err)
	}
}

// A macos-vm command only reads the CA: its off and its drive roots remove nothing.
func TestMacOSVMInspectionCACommandOffLeavesTheKey(t *testing.T) {
	r := newMacLedgerRig(t)
	m := r.open(t)
	on, off := true, false
	m.openInspectionCA(&on, nil, true)
	k, c := inspectionCAFiles(m)
	keyBefore, err := os.ReadFile(k)
	if err != nil {
		t.Fatal(err)
	}
	certBefore, err := os.ReadFile(c)
	if err != nil {
		t.Fatal(err)
	}
	same := func(what string) {
		t.Helper()
		keyNow, err := os.ReadFile(k)
		if err != nil || !bytes.Equal(keyNow, keyBefore) {
			t.Errorf("%s: key.pem changed (%v)", what, err)
		}
		certNow, err := os.ReadFile(c)
		if err != nil || !bytes.Equal(certNow, certBefore) {
			t.Errorf("%s: ca.pem changed (%v)", what, err)
		}
	}
	m.items = macOSVMItems{}
	m.openInspectionCA(&off, nil, false)
	same("a command's off")
	if m.items.ca != nil {
		t.Error("a command's off opened a CA")
	}
	if slices.Contains(m.editItems(), "--"+macvm.FlagInspectionCA) {
		t.Error("a command's off passes the CA")
	}
	m.openInspectionCA(&on, []string{filepath.Dir(m.layout.Root)}, false)
	same("a command's on under a drive root")
}

// A macos-vm command makes no CA; it reads the runner's.
func TestMacOSVMInspectionCACommandOnMakesNone(t *testing.T) {
	r := newMacLedgerRig(t)
	m := r.open(t)
	on := true
	m.openInspectionCA(&on, nil, false)
	inspectionCAGone(t, m, true, "a command's on")
	if m.items.ca != nil || m.items.caErr != nil {
		t.Errorf("ca %v, error %v", m.items.ca, m.items.caErr)
	}
	if l, err := m.loadItems(); err != nil || len(l.Items) != 0 {
		t.Errorf("loadItems: %v, %d items", err, len(l.Items))
	}
	m.openInspectionCA(&on, nil, true)
	if m.items.ca == nil {
		t.Fatalf("the runner made no CA: %v", m.items.caErr)
	}
	fp := m.items.ca.Fingerprint()
	m.items = macOSVMItems{}
	m.openInspectionCA(&on, nil, false)
	if m.items.ca == nil || m.items.ca.Fingerprint() != fp {
		t.Fatalf("a command did not read the runner's CA: %v", m.items.caErr)
	}
	if a := m.editItems(); !slices.Equal(a, []string{"--" + macvm.FlagInspectionCA, m.items.ca.CertPath()}) {
		t.Errorf("edit-base's args %q", a)
	}
}

// A profile that is there but cannot be read reads as unset, which could turn an organisation's 1 into off, so
// the CA is kept until it reads.
func TestMacOSVMInspectionCAOffKeepsTheKeyWhenTheProfileCannotBeRead(t *testing.T) {
	r := newMacLedgerRig(t)
	m := r.open(t)
	on, off := true, false
	m.openInspectionCA(&on, nil, true)
	inspectionCAGone(t, m, false, "on")
	oldOS, oldExtract := prefsOS, prefsExtract
	restore := func() { prefsOS, prefsExtract = oldOS, oldExtract }
	t.Cleanup(restore)
	prefsOS = "darwin"
	prefsExtract = func(string, string) (string, error) { return "", errors.New("unreadable") }
	m.items = macOSVMItems{}
	m.openInspectionCA(&off, nil, true)
	inspectionCAGone(t, m, false, "off, profile unreadable")
	// A drive root over the VM directory outranks the unreadable profile: the key goes.
	m.items = macOSVMItems{}
	m.openInspectionCA(&off, []string{filepath.Dir(m.layout.Root)}, true)
	inspectionCAGone(t, m, true, "off, profile unreadable, drive root over")
	restore()
	m.items = macOSVMItems{}
	m.openInspectionCA(&off, nil, true)
	inspectionCAGone(t, m, true, "off, profile readable")
}

// A drive root that names the CA's folder before it exists is over it: no CA is made there.
func TestMacOSVMInspectionCADriveRootNamingTheNotYetMadeFolderStaysOff(t *testing.T) {
	r := newMacLedgerRig(t)
	m := r.open(t)
	on := true
	m.openInspectionCA(&on, []string{m.layout.InspectionCADir()}, true)
	if m.items.ca != nil || m.items.caErr != nil {
		t.Errorf("ca %v, error %v", m.items.ca, m.items.caErr)
	}
	inspectionCAGone(t, m, true, "drive root names the folder")

	// The same, with the VM directory reached through a symlink and the root named by the spelling the operator sees.
	r = newMacLedgerRig(t)
	inner := filepath.Join(t.TempDir(), "inner")
	if err := os.Mkdir(inner, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(inner, link); err != nil {
		t.Fatal(err)
	}
	r.root = filepath.Join(link, "macos-vm.noindex")
	m = r.open(t)
	m.openInspectionCA(&on, []string{m.layout.InspectionCADir()}, true)
	if m.items.ca != nil || m.items.caErr != nil {
		t.Errorf("through a symlink: ca %v, error %v", m.items.ca, m.items.caErr)
	}
	inspectionCAGone(t, m, true, "drive root names the folder through a symlink")
}

// A profile that cannot be read could be hiding an organisation's 0, so runnerd.json's 1 makes no CA then: an
// existing one is read, and none is made nor listed.
func TestMacOSVMInspectionCAUnreadableProfileMakesNone(t *testing.T) {
	r := newMacLedgerRig(t)
	m := r.open(t)
	on := true
	oldOS, oldExtract := prefsOS, prefsExtract
	t.Cleanup(func() { prefsOS, prefsExtract = oldOS, oldExtract })
	prefsOS = "darwin"
	prefsExtract = func(string, string) (string, error) { return "", errors.New("unreadable") }
	m.openInspectionCA(&on, nil, true)
	inspectionCAGone(t, m, true, "unreadable profile, no CA")
	if m.items.ca != nil || m.items.caErr != nil {
		t.Errorf("ca %v, error %v", m.items.ca, m.items.caErr)
	}
	if slices.Contains(m.editItems(), "--"+macvm.FlagInspectionCA) {
		t.Error("edit-base passes a CA that was never made")
	}
	if l, err := m.loadItems(); err != nil || len(l.Items) != 0 {
		t.Errorf("loadItems: %v, %d items", err, len(l.Items))
	}
	prefsOS, prefsExtract = oldOS, oldExtract
	m.openInspectionCA(&on, nil, true)
	inspectionCAGone(t, m, false, "profile readable")
	prefsOS = "darwin"
	prefsExtract = func(string, string) (string, error) { return "", errors.New("unreadable") }
	m.items = macOSVMItems{}
	m.openInspectionCA(&on, nil, true)
	if m.items.ca == nil {
		t.Errorf("an existing CA was not read: %v", m.items.caErr)
	}
}

func TestMacOSVMInspectionCADroppedWithMacOSVMsOff(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root, err := macvm.DefaultRoot()
	if err != nil {
		t.Fatal(err)
	}
	dir := macvm.Layout{Root: root}.InspectionCADir()
	on, off := true, false
	make := func() {
		t.Helper()
		if err := os.MkdirAll(root, 0o700); err != nil {
			t.Fatal(err)
		}
		if _, err := macvm.OpenInspectionCA(context.Background(), dir); err != nil {
			t.Fatal(err)
		}
	}
	has := func() bool { _, err := os.Lstat(filepath.Join(dir, "key.pem")); return err == nil }
	dropInspectionCA(macOSVMSettings{inspectionCA: &on})
	if _, err := os.Lstat(root); err == nil {
		t.Error("made the VM directory")
	}
	make()
	dropInspectionCA(macOSVMSettings{})
	dropInspectionCA(macOSVMSettings{inspectionCA: &on})
	dropInspectionCA(macOSVMSettings{inspectionCA: &on, driveRoots: []string{t.TempDir()}})
	if !has() {
		t.Fatal("removed with the setting on and no drive root over it")
	}
	dropInspectionCA(macOSVMSettings{inspectionCA: &on, driveRoots: []string{home}})
	if has() {
		t.Error("kept under a drive root")
	}
	make()
	dropInspectionCA(macOSVMSettings{inspectionCA: &off})
	if has() {
		t.Error("kept with the setting off")
	}
	// With macOS VMs off, the setting on and the folder empty (an earlier removal), no CA is made.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	dropInspectionCA(macOSVMSettings{inspectionCA: &on})
	if has() {
		t.Error("made a CA with macOS VMs off")
	}
}

func inspectionCAFiles(m *macOSVMs) (string, string) {
	d := m.layout.InspectionCADir()
	return filepath.Join(d, "key.pem"), filepath.Join(d, "ca.pem")
}

func inspectionCAGone(t *testing.T, m *macOSVMs, want bool, what string) {
	t.Helper()
	k, c := inspectionCAFiles(m)
	for _, f := range []string{k, c} {
		_, err := os.Lstat(f)
		if gone := errors.Is(err, os.ErrNotExist); gone != want {
			t.Errorf("%s: %s gone=%v, want %v (%v)", what, filepath.Base(f), gone, want, err)
		}
	}
}

// The wiring through openMacOSVMs. A handle that does not own the CA makes none with the setting on and removes
// none with it off; the owning handle does both.
func TestOpenMacOSVMsOnlyServeOwnsTheInspectionCA(t *testing.T) {
	r := newMacLedgerRig(t)
	on, off := true, false
	open := func(set *bool, owns bool) *macOSVMs {
		return openMacOSVMs(macOSVMSetup{ledgerDir: r.dir, root: r.root, runnerID: "runner-a", version: "1.0.0", uid: 501, dockerMemory: 8 * gib,
			host: r.host, owner: r.owner, fstat: r.fstat, settings: macOSVMSettings{inspectionCA: set}, ownsInspectionCA: owns})
	}
	m := open(&on, false)
	inspectionCAGone(t, m, true, "a command, on")
	m = open(&on, true)
	inspectionCAGone(t, m, false, "serve, on")
	if m.items.ca == nil {
		t.Fatal("serve has no CA")
	}
	m = open(&off, false)
	inspectionCAGone(t, m, false, "a command, off")
	m = open(&on, false)
	if m.items.ca == nil {
		t.Error("a command, on, did not read serve's CA")
	}
	m = open(&off, true)
	inspectionCAGone(t, m, true, "serve, off")
}

// Two starts of the runner with the setting on, and the helper given the flag they pass, compute one install list
// digest, so a base that recorded it does not read as changed at the next start.
func TestMacOSVMInspectionCADigestIsTheSameAtEveryStart(t *testing.T) {
	r := newMacLedgerRig(t)
	on := true
	digest := func() string {
		t.Helper()
		m := openMacOSVMs(macOSVMSetup{ledgerDir: r.dir, root: r.root, runnerID: "runner-a", version: "1.0.0", uid: 501, dockerMemory: 8 * gib,
			host: r.host, owner: r.owner, fstat: r.fstat, settings: macOSVMSettings{inspectionCA: &on}, ownsInspectionCA: true})
		l, err := m.loadItems()
		if err != nil || len(l.Items) != 1 {
			t.Fatalf("loadItems: %v, %d items", err, len(l.Items))
		}
		args := m.editItems()
		if len(args) != 2 || args[0] != "--"+macvm.FlagInspectionCA {
			t.Fatalf("the flag: %q", args)
		}
		helper, err := macvm.WithInspectionCA(macvm.BaseList{}, args[1])
		if err != nil {
			t.Fatal(err)
		}
		if l.Digest() != helper.Digest() {
			t.Errorf("runnerd's digest %s is not the helper's %s", l.Digest(), helper.Digest())
		}
		return l.Digest()
	}
	if a, b := digest(), digest(); a != b {
		t.Errorf("two starts: %s, then %s", a, b)
	}
}

// A runnerd.json macos_vm_inspection_ca that is not a spelling of on or off is refused, as the kept limit is,
// rather than read as off, which removes the Mac's key.
func TestCheckRunnerFileInspectionCA(t *testing.T) {
	for _, v := range []string{"", "0", "1", "true", "false", "on", "off", "yes", "no", " ON ", "Yes"} {
		if err := checkRunnerFile(runnerFile{MacOSVMInspectionCA: v}); err != nil {
			t.Errorf("%q refused: %v", v, err)
		}
	}
	for _, v := range []string{"enabled", "2", "tru", "disabled"} {
		err := checkRunnerFile(runnerFile{MacOSVMInspectionCA: v})
		if err == nil || !strings.HasPrefix(err.Error(), "macos_vm_inspection_ca: ") || !strings.Contains(err.Error(), `"`+v+`"`) {
			t.Errorf("%q: error %v, want one naming the key and the value", v, err)
		}
	}
}

// A managed preference (the key has no flag or environment variable) or a file value that is neither on nor off
// keeps this Mac's key, and says so once; 0 and off still remove it.
func TestMacOSVMInspectionCAUnknownValueKeepsTheKey(t *testing.T) {
	r := newMacLedgerRig(t)
	m := r.open(t)
	on := true
	m.openInspectionCA(&on, nil, true)
	inspectionCAGone(t, m, false, "made")
	for _, v := range []string{"enabled", "2"} {
		for name, in := range map[string]runnerInputs{
			"managed": inputs(nil, nil, map[string]string{"MacOSVMInspectionCA": v}, runnerFile{}),
			"file":    inputs(nil, nil, nil, runnerFile{MacOSVMInspectionCA: v}),
		} {
			logs := captureRunnerLog(t)
			set := resolveRunnerConfig(in).macOSVMSettings()
			if set.inspectionCA != nil {
				t.Fatalf("%s %q: settings say %v, want none", name, v, *set.inspectionCA)
			}
			if got := logs.String(); strings.Count(got, "neither made nor removed") != 1 || !strings.Contains(got, v) {
				t.Errorf("%s %q: log %q, want one warning naming the value", name, v, got)
			}
			m.items = macOSVMItems{}
			m.openInspectionCA(set.inspectionCA, nil, true)
			inspectionCAGone(t, m, false, name+" "+v)
		}
	}
	for _, v := range []string{"0", "off"} {
		set := resolveRunnerConfig(inputs(nil, nil, map[string]string{"MacOSVMInspectionCA": v}, runnerFile{})).macOSVMSettings()
		if set.inspectionCA == nil || *set.inspectionCA {
			t.Fatalf("%q: settings say %v, want off", v, set.inspectionCA)
		}
		m.items = macOSVMItems{}
		m.openInspectionCA(set.inspectionCA, nil, true)
		inspectionCAGone(t, m, true, v)
		m.openInspectionCA(&on, nil, true) // make it again for the next value
	}
}
