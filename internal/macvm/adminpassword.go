// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package macvm

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// errNoAdminPassword: the base (or the staging copy of it) holds no good admin password, so nothing can
// change the setup admin's password from the old one.
var errNoAdminPassword = errors.New("macvm: this macOS base keeps no admin password")

// adminPasswordNext is AdminPasswordFile's write-ahead copy: the password a rotate is about to set.
const adminPasswordNext = AdminPasswordFile + ".next"

// readAdminPassword reads the admin password file name in dir: a regular file (not a link), mode 0600, 8 to 64
// bytes, every byte in codeAlphabet or typableAlphabet (every password Wardyn sets is: the edit's one-time
// code is typableAlphabet's). Anything else, absence included, is no password.
func readAdminPassword(dir, name string) ([]byte, error) {
	p := filepath.Join(dir, name)
	fi, err := os.Lstat(p)
	if err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm() != 0o600 || fi.Size() < 8 || fi.Size() > 64 {
		return nil, errNoAdminPassword
	}
	pw, err := os.ReadFile(p)
	if err != nil || len(pw) < 8 || len(pw) > 64 {
		return nil, errNoAdminPassword
	}
	for _, c := range pw {
		if strings.IndexByte(codeAlphabet, c) < 0 && strings.IndexByte(typableAlphabet, c) < 0 {
			clear(pw)
			return nil, errNoAdminPassword
		}
	}
	return pw, nil
}

// writeAdminPassword writes pw into dir as name at 0600, through a synced temp file, a rename and a synced
// dir: a crash leaves the old file or the new one, never a torn one.
func writeAdminPassword(dir, name string, pw []byte) error {
	tmp, err := os.CreateTemp(dir, "."+name+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(pw); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), filepath.Join(dir, name)); err != nil {
		return err
	}
	return syncDir(dir)
}

// syncDir makes dir's entries durable.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	err = d.Sync()
	if cerr := d.Close(); err == nil {
		err = cerr
	}
	return err
}

// copyAdminPassword gives dst (a staging copy's clone of src) src's password, the one its guest disk holds.
func copyAdminPassword(src, dst string) error {
	pw, err := readAdminPassword(src, AdminPasswordFile)
	if err != nil {
		return err
	}
	defer clear(pw)
	return writeAdminPassword(dst, AdminPasswordFile, pw)
}

// cloneStaging is cloneRun for a staging copy that is swapped in as the base (a tools update's or an
// edit's), never a run's: the copy also carries the base's AdminPasswordFile, which its guest disk
// holds. A base prepared before Wardyn kept one carries none.
func cloneStaging(e Engine, base, staging string) (Identity, error) {
	id, err := cloneRun(e, base, staging, IdentityTravel)
	if err != nil {
		return id, err
	}
	if err := copyAdminPassword(base, staging); err != nil && !errors.Is(err, errNoAdminPassword) {
		return Identity{}, err
	}
	return id, nil
}

// rotateAdmin changes the setup admin's password in g, whose guest disk is dir's, to next, write-ahead:
// next goes to adminPasswordNext (synced) before the guest is asked, and replaces AdminPasswordFile only
// once the guest has taken it. The old password it gives is AdminPasswordFile's, then, on a refusal, the
// adminPasswordNext it found (a rotate cut short after the guest took it). A refusal of each is "the
// guest did not take it" and leaves AdminPasswordFile as it was; an error from the guest ends it at once.
func rotateAdmin(ctx context.Context, g EditGuest, dir string, next []byte) error {
	sealed, err := readAdminPassword(dir, AdminPasswordFile)
	if err != nil {
		return err
	}
	defer clear(sealed)
	olds := [][]byte{sealed}
	if prev, err := readAdminPassword(dir, adminPasswordNext); err == nil {
		defer clear(prev)
		olds = append(olds, prev)
	}
	if err := writeAdminPassword(dir, adminPasswordNext, next); err != nil {
		return err
	}
	for _, old := range olds {
		set, err := g.AdminCode(ctx, old, next)
		if err != nil {
			return err
		}
		if set {
			if err := os.Rename(filepath.Join(dir, adminPasswordNext), filepath.Join(dir, AdminPasswordFile)); err != nil {
				return err
			}
			return syncDir(dir)
		}
	}
	return errors.New("the guest did not take it")
}
