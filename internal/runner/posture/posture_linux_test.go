// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package posture

import (
	"os"
	"path/filepath"
	"testing"
)

// cryptBacked reads a sysfs block-device graph: a dm-crypt mapping names itself in dm/uuid, and a
// device-mapper or md layer lists its backing devices under slaves/.
func TestCryptBackedNeedsEveryBackingDeviceEncrypted(t *testing.T) {
	root := t.TempDir()
	node := func(name, dmUUID string, slaves ...string) string {
		dir := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Join(dir, "slaves"), 0o755); err != nil {
			t.Fatal(err)
		}
		if dmUUID != "" {
			if err := os.MkdirAll(filepath.Join(dir, "dm"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "dm", "uuid"), []byte(dmUUID+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		for _, s := range slaves {
			if err := os.Symlink(filepath.Join(root, s), filepath.Join(dir, "slaves", s)); err != nil {
				t.Fatal(err)
			}
		}
		return dir
	}
	node("sda2", "")
	node("sdb2", "")
	node("luks-a", "CRYPT-LUKS2-aaaa-luks-a", "sda2")
	node("luks-b", "CRYPT-LUKS2-bbbb-luks-b", "sdb2")
	for name, tc := range map[string]struct {
		dev  string
		want bool
	}{
		"a crypt mapping":                       {node("dm-crypt", "CRYPT-LUKS2-cccc-root", "sda2"), true},
		"LVM over two encrypted disks":          {node("lvm-enc", "LVM-encrypted", "luks-a", "luks-b"), true},
		"LVM over an encrypted and a plain one": {node("lvm-mixed", "LVM-mixed", "luks-a", "sdb2"), false},
		"md over a plain and an encrypted one":  {node("md0", "", "sda2", "luks-b"), false},
		"a branch that cannot be read":          {node("lvm-gone", "LVM-gone", "luks-a", "missing"), false},
		"a plain disk":                          {filepath.Join(root, "sda2"), false},
		"LVM over a plain disk":                 {node("lvm-plain", "LVM-plain", "sda2"), false},
	} {
		if got := cryptBacked(tc.dev, 0); got != tc.want {
			t.Errorf("%s: cryptBacked = %v, want %v", name, got, tc.want)
		}
	}
}
