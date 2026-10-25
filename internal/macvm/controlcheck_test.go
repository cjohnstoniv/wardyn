// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package macvm_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
)

// probe is a fake RFB client: logging in with the guest's current password
// moves the guest's pointer, unless broken.
type probe struct {
	g      *guest
	broken bool
	used   []string
}

func (p *probe) MovePointer(_ context.Context, pw []byte, x, y int) error {
	p.g.mu.Lock()
	defer p.g.mu.Unlock()
	p.used = append(p.used, string(pw))
	if p.broken || len(p.g.passwords) == 0 || p.g.passwords[len(p.g.passwords)-1] != string(pw) {
		return errors.New("VncAuth failed")
	}
	p.g.pointerX, p.g.pointerY = x, y
	return nil
}

func controlPrep(t *testing.T, broken bool) (*prepFixture, *guest, *probe) {
	t.Helper()
	f, _, g := stage2Prep(t)
	pr := &probe{g: g, broken: broken}
	f.opts.Stage2.(*macvm.ToolsStage).Desktop = pr
	return f, g, pr
}

// grepTree fails if needle is in any file under root or in the logs.
func grepTree(t *testing.T, root string, needle string, logs ...[]byte) {
	t.Helper()
	for _, l := range logs {
		if bytes.Contains(l, []byte(needle)) {
			t.Fatalf("%q is in a log", needle)
		}
	}
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if b, _ := os.ReadFile(p); bytes.Contains(b, []byte(needle)) {
			t.Fatalf("%q is in %s", needle, p)
		}
		return nil
	})
}

func eventsJSON(t *testing.T, e *events) []byte {
	t.Helper()
	e.mu.Lock()
	defer e.mu.Unlock()
	b, err := json.Marshal(e.evs)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Stage 2's control check sets a fresh per-boot password, logs in with it,
// finds the pointer where it moved it, then sets a second fresh password:
// the base is control, and neither password is in a log, an event or any
// file of the VM directory.
func TestControlCheckUsesAFreshPasswordThenResetsIt(t *testing.T) {
	f, g, pr := controlPrep(t, false)
	m, err := macvm.Prepare(context.Background(), f.opts)
	if err != nil {
		t.Fatal(err)
	}
	if m.Desktop != macvm.DesktopControl {
		t.Fatalf("desktop %q, want control", m.Desktop)
	}
	if len(g.passwords) != 2 || g.passwords[0] == g.passwords[1] || len(g.passwords[0]) != 8 || len(g.passwords[1]) != 8 {
		t.Fatalf("passwords %d set, want two distinct 8-character ones", len(g.passwords))
	}
	if len(pr.used) != 1 || pr.used[0] != g.passwords[0] {
		t.Fatal("the RFB login did not use the check's own password")
	}
	for _, pw := range g.passwords {
		grepTree(t, f.opts.Layout.Root, pw, f.logs.Bytes(), eventsJSON(t, f.ev))
	}
	var dial string
	for _, ev := range f.ev.evs {
		if ev.ToolsDial != "" {
			dial = ev.ToolsDial
		}
	}
	if dial != macvm.ToolsDialConnected || !bytes.Contains(f.logs.Bytes(), []byte(`"tools_dial":"connected"`)) {
		t.Fatalf("tools_dial in prepare's events %q / log", dial)
	}
}

// A pointer that did not move is observe, and the base is still ready.
func TestControlCheckFallsBackToObserveOnlyForThePointer(t *testing.T) {
	f, g, _ := controlPrep(t, true)
	m, err := macvm.Prepare(context.Background(), f.opts)
	if err != nil || m.Desktop != macvm.DesktopObserve {
		t.Fatalf("desktop %q, err %v; want observe", m.Desktop, err)
	}
	if len(g.passwords) != 2 {
		t.Fatalf("%d passwords set, want the check's and the reset", len(g.passwords))
	}
}

// A password the guest did not take, the check's or the reset, fails
// prepare by name and publishes no ready base.
func TestBaseIsNeverReadyWithoutItsPasswordInstallAndReset(t *testing.T) {
	for _, failing := range []int{1, 2} {
		f, g, _ := controlPrep(t, false)
		g.passwordSet = func(n int) bool { return n != failing }
		if _, err := macvm.Prepare(context.Background(), f.opts); !errors.Is(err, macvm.ErrDesktopPassword) {
			t.Fatalf("password %d not set: %v, want ErrDesktopPassword", failing, err)
		}
		if _, err := macvm.ReadyBase(f.opts.Layout.BaseDir(0)); err == nil {
			t.Fatalf("password %d not set, and the base is ready", failing)
		}
	}
}

// A tools update keeps the manifest's desktop value: no control check, no
// password set.
func TestToolsUpdateRunsNoControlCheck(t *testing.T) {
	f, tf, _ := stage2Prep(t)
	before, err := macvm.Prepare(context.Background(), f.opts)
	if err != nil {
		t.Fatal(err)
	}
	g2 := newGuest()
	o := updateOpts(f, tf, g2, "0.10.0")
	o.Force = true
	m, changed, err := macvm.UpdateTools(context.Background(), o)
	if err != nil || !changed || m.Desktop != before.Desktop || len(g2.passwords) != 0 {
		t.Fatalf("update: desktop %q (was %q), %d passwords, changed %v, err %v", m.Desktop, before.Desktop, len(g2.passwords), changed, err)
	}
}
