// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package macvm_test

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
	"github.com/cjohnstoniv/wardyn/internal/macvm/macvmtest"
)

// adminPassword is the base's host copy of its setup admin's password, which must be a 0600 file.
func adminPassword(t *testing.T, dir string) string {
	t.Helper()
	fi, err := os.Stat(filepath.Join(dir, macvm.AdminPasswordFile))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("%s: %v %v", dir, fi, err)
	}
	pw, err := os.ReadFile(filepath.Join(dir, macvm.AdminPasswordFile))
	if err != nil {
		t.Fatal(err)
	}
	return string(pw)
}

// The password prepare kept rides a tools update with its disk, a derived base and a run's clone never get
// it.
func TestTheAdminPasswordFollowsItsDisk(t *testing.T) {
	f, tf, _ := stage2Prep(t)
	if _, err := macvm.Prepare(context.Background(), f.opts); err != nil {
		t.Fatal(err)
	}
	base := f.opts.Layout.BaseDir(0)
	if got := adminPassword(t, base); got != f.boot.in.Password {
		t.Fatal("prepare did not keep the password stage 1 left the guest with")
	}
	tf.claude = []byte("claude-binary-2.1.311")
	tf.tooling.Harnesses[0].Version, tf.tooling.Harnesses[0].SHA256, tf.tooling.Harnesses[0].Size = "2.1.311", shaHex(tf.claude), int64(len(tf.claude))
	tf.opts.Tooling = tf.tooling
	g := newGuest()
	g.version["/usr/local/bin/claude"] = "claude 2.1.311"
	if _, changed, err := macvm.UpdateTools(context.Background(), updateOpts(f, tf, g, "0.10.0")); err != nil || !changed {
		t.Fatalf("update: changed %v err %v", changed, err)
	}
	if got := adminPassword(t, base); got != f.boot.in.Password {
		t.Fatal("a tools update changed the base's password, but not its guest disk's")
	}
	p, pin := itemFile(t, t.TempDir(), "xcode.cfg", "xcode-config")
	list := macvm.BaseList{Version: 1, Items: []macvm.BaseItem{{Kind: "file", Name: "xcode-cfg", Path: p, SHA256: pin, Dest: "/Library/Application Support/Org/xcode.cfg"}}}
	ig := newItemGuest()
	tf.opts.Items = list
	o := updateOpts(f, tf, ig.guest, "0.10.0")
	o.Stage, o.ItemsDigest = tf.itemStage(ig), list.Digest()
	if _, _, err := macvm.DeriveBase(context.Background(), o, "xcode-16"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(f.opts.Layout.VariantDir("xcode-16"), macvm.AdminPasswordFile)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a derived base holds the password: %v", err)
	}
	run := filepath.Join(t.TempDir(), "run")
	if _, err := macvm.CloneRun(f.eng, base, run, macvm.IdentityTravel); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(run, macvm.PairKeyFile)); err != nil {
		t.Fatalf("a run's clone has no pairing key: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(run, macvm.AdminPasswordFile)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a run's clone holds the password: %v", err)
	}
}

// A prepare cut short leaves a partial base, and the next prepare discards its password with the rest.
func TestPrepareDiscardsAnInterruptedBasesAdminPassword(t *testing.T) {
	f := newPrep(t)
	base := f.opts.Layout.BaseDir(0)
	if err := os.MkdirAll(base, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"installing": "", macvm.AdminPasswordFile: "Stray234Stray"} {
		if err := os.WriteFile(filepath.Join(base, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	f.eng.InstallFunc = func(context.Context, *macvmtest.Machine, string, func(float64)) error {
		return errors.New("restore failed")
	}
	if _, err := macvm.Prepare(context.Background(), f.opts); err == nil {
		t.Fatal("prepare with a failing restore succeeded")
	}
	if _, err := os.Lstat(filepath.Join(base, macvm.AdminPasswordFile)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the interrupted base's password is still there: %v", err)
	}
}
