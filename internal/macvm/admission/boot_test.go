// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package admission

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// twoLive is a Mac with two counted entries, each with a live helper: a's
// (uid 501, helper 77) and b's (uid 502, helper 90).
func twoLive(t *testing.T) (h *fakeHost, r *rig, a, b *Ledger, ea, eb Entry) {
	t.Helper()
	h = bigMac()
	r = newRig(t, h)
	a, b = r.open(t, 501, "a"), r.open(t, 502, "b")
	h.setProcs(Proc{PID: 77, UID: 501, Start: 7, Name: HelperProcess}, Proc{PID: 90, UID: 502, Start: 9, Name: HelperProcess})
	var err error
	if ea, err = a.Admit(runReq(8 * gib)); err != nil {
		t.Fatal(err)
	}
	if ea, err = a.ClaimHelper(ea.ID, 77, 7); err != nil {
		t.Fatal(err)
	}
	if eb, err = b.Admit(runReq(8 * gib)); err != nil {
		t.Fatal(err)
	}
	if eb, err = b.ClaimHelper(eb.ID, 90, 9); err != nil {
		t.Fatal(err)
	}
	return h, r, a, b, ea, eb
}

// wantThisBoot: both entries are counted, a's helper may claim its entry
// again, and a's tidy deletes nothing.
func wantThisBoot(t *testing.T, r *rig, a *Ledger, ea Entry) {
	t.Helper()
	if es, err := a.Counted(); err != nil || len(es) != 2 {
		t.Fatalf("counted %d, %v; want both entries", len(es), err)
	}
	if _, err := a.Admit(runReq(8 * gib)); !IsRefusal(err) {
		t.Fatalf("admitted a third VM: %v", err)
	}
	if _, err := a.ClaimHelper(ea.ID, 77, 7); err != nil {
		t.Fatalf("the helper's claim refused: %v", err)
	}
	if _, err := a.Sweep(); err != nil {
		t.Fatal(err)
	}
	if left := entryFiles(t, r.dir); len(left) != 2 {
		t.Fatalf("entry files left: %v, want both", left)
	}
}

// The boot is told by its session, never by kern.boottime, which macOS moves
// with the clock. A clock change either way keeps this boot's entries counted
// and its own entries undeleted; a new session drops them.
func TestBootIdentityIsTheSession(t *testing.T) {
	for name, step := range map[string]int64{"clock forward": 3600e6, "clock back": -3600e6} {
		t.Run(name, func(t *testing.T) {
			h, r, a, _, ea, _ := twoLive(t)
			h.mu.Lock()
			h.boot += step
			h.mu.Unlock()
			wantThisBoot(t, r, a, ea)
		})
	}
	t.Run("restart", func(t *testing.T) {
		h, r, a, _, ea, _ := twoLive(t)
		h.restart()
		if es, err := a.Counted(); err != nil || len(es) != 0 {
			t.Fatalf("counted %+v, %v; want none after a restart", es, err)
		}
		if _, err := a.ClaimHelper(ea.ID, 77, 7); err == nil {
			t.Fatal("claimed an entry from an earlier boot")
		}
		if _, err := a.Sweep(); err != nil {
			t.Fatal(err)
		}
		if left := entryFiles(t, r.dir); len(left) != 1 || left[0] == "e-"+ea.ID+".json" {
			t.Fatalf("entry files left: %v, want only the other login's", left)
		}
	})
}

// noSessionHost is a Host with no BootSession method.
type noSessionHost struct{ Host }

// With this boot's session unknown (unreadable, empty or not read at all), no
// stored session proves an earlier boot: every entry stays this boot's.
func TestUnknownSessionKeepsEveryEntry(t *testing.T) {
	for name, tc := range map[string]struct {
		host func(*fakeHost)
		mod  func(*Config)
	}{
		"unreadable": {host: func(h *fakeHost) { h.sessionErr = errors.New("sysctl failed") }},
		"empty":      {host: func(h *fakeHost) { h.session = "" }},
		"missing":    {mod: func(c *Config) { c.Host = noSessionHost{c.Host} }},
	} {
		t.Run(name, func(t *testing.T) {
			h, r, a, _, ea, _ := twoLive(t)
			h.restart() // a stored session that differs, which proves nothing now
			h.mu.Lock()
			if tc.host != nil {
				tc.host(h)
			}
			h.mu.Unlock()
			if tc.mod != nil {
				a = r.open(t, 501, "a", tc.mod)
			}
			wantThisBoot(t, r, a, ea)
		})
	}
}

// legacy rewrites e as an earlier release wrote it: no session and no
// monotonic starts, with a boottime that is not this one's.
func legacy(t *testing.T, l *Ledger, e Entry) {
	t.Helper()
	if _, err := l.Update(e.ID, func(x *Entry) {
		x.BootSession, x.BootTime, x.OwnerMonoStart, x.HelperMonoStart = "", 12345, 0, 0
	}); err != nil {
		t.Fatal(err)
	}
}

// An entry an earlier release wrote is never taken for an earlier boot's, so
// it counts, and its owner's tidy keeps it, after a restart too. It frees
// only once nothing of its user may be its VM: then another login may
// tombstone it and its owner reclaim it. Docker's Linux VM keeps it held.
func TestLegacyEntryCountsUntilItsVMIsGone(t *testing.T) {
	h, r, a, b, ea, _ := twoLive(t)
	legacy(t, a, ea)
	wantThisBoot(t, r, a, ea)
	legacy(t, a, ea) // the claim above rewrote it

	h.restart()
	docker := Proc{PID: 50, UID: 501, Start: 1, MonoStart: 1, Name: vzName}
	h.setProcs(docker)
	if es, _ := a.Counted(); len(es) != 1 || es[0].ID != ea.ID {
		t.Fatalf("counted %+v after a restart, want the legacy entry", es)
	}
	if n, err := b.Sweep(); n != 0 || err != nil {
		t.Fatalf("tombstoned past Docker's VM: %d %v", n, err)
	}
	if err := a.ReclaimOwn(ea.ID, ""); err == nil || err.Error() != errMayBeVM {
		t.Fatalf("ReclaimOwn past Docker's VM = %v, want %q", err, errMayBeVM)
	}
	if _, err := a.Sweep(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(r.dir, "e-"+ea.ID+".json")); err != nil {
		t.Fatalf("the legacy entry was tidied: %v", err)
	}

	h.setProcs()
	if n, err := b.Sweep(); n != 1 || err != nil {
		t.Fatalf("sweep = %d, %v; want the legacy entry tombstoned", n, err)
	}
	if es, _ := a.Counted(); len(es) != 0 {
		t.Fatalf("counted %+v, want the tombstoned entry freed", es)
	}
	if err := a.ReclaimOwn(ea.ID, ""); err != nil {
		t.Fatalf("the owner could not reclaim its legacy entry: %v", err)
	}
}

func TestEarlierBootOnlyByADifferentSession(t *testing.T) {
	e := Entry{BootTime: 1, BootSession: "s1"}
	for _, c := range []struct {
		name string
		h    Host
		e    Entry
		want bool
	}{
		{"same session, boot time moved by the clock", &fakeHost{boot: 2, session: "s1"}, e, false},
		{"different session", &fakeHost{boot: 1, session: "s2"}, e, true},
		{"entry from an earlier release", &fakeHost{boot: 2, session: "s2"}, Entry{BootTime: 1}, false},
		{"session unreadable", &fakeHost{boot: 2, sessionErr: errors.New("sysctl")}, e, false},
		{"session empty", &fakeHost{boot: 2}, e, false},
		{"host without sessions", noSessionHost{&fakeHost{boot: 2, session: "s2"}}, e, false},
	} {
		if got := EarlierBoot(c.h, c.e); got != c.want {
			t.Errorf("%s: EarlierBoot = %v, want %v", c.name, got, c.want)
		}
	}
}
