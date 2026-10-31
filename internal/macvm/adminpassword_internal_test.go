// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package macvm

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// heldGuest's setup admin has held: it takes a change only from held, and notes whether the write-ahead
// file already held the new password when it was asked.
type heldGuest struct {
	EditGuest
	dir, held string
	ahead     []bool
}

func (g *heldGuest) AdminCode(_ context.Context, old, code []byte) (bool, error) {
	b, _ := os.ReadFile(filepath.Join(g.dir, adminPasswordNext))
	g.ahead = append(g.ahead, string(b) == string(code))
	if string(old) != g.held {
		return false, nil
	}
	g.held = string(code)
	return true, nil
}

func put(t *testing.T, dir, name, pw string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(pw), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestRotateAdminPassword(t *testing.T) {
	for _, c := range []struct{ name, file, next, held string }{
		{"clean", "Prev2345Prev", "", "Prev2345Prev"},
		{"cut short before the guest was asked", "Prev2345Prev", "Ahead234Ahead", "Prev2345Prev"},
		{"cut short after the guest took it, before the rename", "Prev2345Prev", "Ahead234Ahead", "Ahead234Ahead"},
		{"cut short after the rename", "Ahead234Ahead", "", "Ahead234Ahead"},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			put(t, dir, AdminPasswordFile, c.file)
			if c.next != "" {
				put(t, dir, adminPasswordNext, c.next)
			}
			g := &heldGuest{dir: dir, held: c.held}
			if err := rotateAdmin(context.Background(), g, dir, []byte("Next2345Next")); err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(filepath.Join(dir, AdminPasswordFile))
			if err != nil || string(got) != "Next2345Next" || g.held != "Next2345Next" {
				t.Fatalf("file %q (%v), guest holds %q", got, err, g.held)
			}
			if fi, err := os.Stat(filepath.Join(dir, AdminPasswordFile)); err != nil || fi.Mode().Perm() != 0o600 {
				t.Fatalf("mode %v %v", fi, err)
			}
			if _, err := os.Lstat(filepath.Join(dir, adminPasswordNext)); !os.IsNotExist(err) {
				t.Fatalf("write-ahead copy left: %v", err)
			}
			for i, a := range g.ahead {
				if !a {
					t.Errorf("ask %d came before the new password was written ahead", i)
				}
			}
		})
	}
	t.Run("a guest that holds neither", func(t *testing.T) {
		dir := t.TempDir()
		put(t, dir, AdminPasswordFile, "Prev2345Prev")
		put(t, dir, adminPasswordNext, "Ahead234Ahead")
		g := &heldGuest{dir: dir, held: "Guest234Guest"}
		err := rotateAdmin(context.Background(), g, dir, []byte("Next2345Next"))
		if err == nil || err.Error() != "the guest did not take it" || g.held != "Guest234Guest" {
			t.Fatalf("err %v, guest holds %q", err, g.held)
		}
		if got, _ := os.ReadFile(filepath.Join(dir, AdminPasswordFile)); string(got) != "Prev2345Prev" {
			t.Fatalf("AdminPasswordFile = %q", got)
		}
	})
	t.Run("no password file", func(t *testing.T) {
		dir := t.TempDir()
		g := &heldGuest{dir: dir}
		if err := rotateAdmin(context.Background(), g, dir, []byte("Next2345Next")); !errors.Is(err, errNoAdminPassword) || len(g.ahead) != 0 {
			t.Fatalf("err %v, asks %d", err, len(g.ahead))
		}
	})
}

func TestReadAdminPasswordRefusesAnythingElse(t *testing.T) {
	write := func(mode os.FileMode, pw string) func(dir string) error {
		return func(dir string) error {
			p := filepath.Join(dir, AdminPasswordFile)
			if err := os.WriteFile(p, []byte(pw), mode); err != nil {
				return err
			}
			return os.Chmod(p, mode)
		}
	}
	for name, c := range map[string]func(dir string) error{
		"absent":     func(string) error { return nil },
		"group-read": write(0o640, "Abcd2345"),
		"7 bytes":    write(0o600, "Abcd234"),
		"65 bytes":   write(0o600, strings.Repeat("a", 65)),
		"a space":    write(0o600, "Abcd 2345"),
		"a zero":     write(0o600, "Abcd0123"),
		"a link to a good file": func(dir string) error {
			target := filepath.Join(dir, "elsewhere")
			if err := os.WriteFile(target, []byte("Abcd2345"), 0o600); err != nil {
				return err
			}
			return os.Symlink(target, filepath.Join(dir, AdminPasswordFile))
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if err := c(dir); err != nil {
				t.Fatal(err)
			}
			if _, err := readAdminPassword(dir, AdminPasswordFile); !errors.Is(err, errNoAdminPassword) {
				t.Fatalf("readAdminPassword = %v", err)
			}
		})
	}
	dir := t.TempDir()
	put(t, dir, AdminPasswordFile, "Abcd2345")
	if pw, err := readAdminPassword(dir, AdminPasswordFile); err != nil || string(pw) != "Abcd2345" {
		t.Fatalf("a good password: %q %v", pw, err)
	}
}
