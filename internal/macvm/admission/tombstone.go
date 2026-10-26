// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package admission

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func (v *view) alive(pid int, start int64) bool {
	for _, p := range v.procs {
		if p.PID == pid && p.Start == start {
			return true
		}
	}
	return false
}

// vzCount is how many Virtualization processes uid has.
func (v *view) vzCount(uid int) int { return v.count(uid, VirtualizationProcess) }

// helperCount is how many helper processes uid has.
func (v *view) helperCount(uid int) int { return v.count(uid, HelperProcess) }

// count is how many of uid's processes have a (possibly truncated) name that
// is a prefix of name.
func (v *view) count(uid int, name string) int {
	n := 0
	for _, p := range v.procs {
		if p.UID == uid && named(p, name) {
			n++
		}
	}
	return n
}

// named: p's (possibly truncated) name is a prefix of name.
func named(p Proc, name string) bool { return p.Name != "" && strings.HasPrefix(name, p.Name) }

// startSlack is how far before e's helper a process must have started to be
// proven not e's (mayBeVM): it absorbs a small step back of the clock that
// start times are read on.
const startSlack = 10 * time.Second

// mayBeVM: a process in the snapshot may still be e's VM. e's VM, and every
// helper process of e's (an install process included), started after e's
// helper did, or for an entry with no helper recorded, after its owner did. A
// Virtualization or helper process of e's uid that started more than
// startSlack before that is proven not to be them: Docker's Linux VM, or
// another macOS VM's, from earlier. Any other one may be e's VM, whatever it
// belongs to: its name, its PID (reused or not) and its parent (reparented
// once the helper died) prove nothing. A start time after now (the clock went
// back) proves nothing either, so it leaves every process possibly e's.
func (v *view) mayBeVM(e Entry, now int64) bool {
	since := e.HelperStart
	if e.HelperPID == 0 {
		since = e.OwnerStart
	}
	if since > now {
		return true
	}
	for _, p := range v.procs {
		if p.Start > now {
			return true
		}
		if p.UID == e.UID && (named(p, VirtualizationProcess) || named(p, HelperProcess)) &&
			p.Start >= since-startSlack.Microseconds() {
			return true
		}
	}
	return false
}

// helperGone: the entry's helper (or, for an entry with no helper yet, its
// owner runnerd) is no longer the process it was.
func (v *view) helperGone(e Entry) bool {
	if e.HelperPID != 0 {
		return !v.alive(e.HelperPID, e.HelperStart)
	}
	return !v.alive(e.OwnerPID, e.OwnerStart)
}

// tombstonable is the tombstone condition: an entry of this boot, not
// leaked, whose helper is gone while no Virtualization process and no helper
// process of its uid remains. (A helper whose runnerd died before recording
// it is still a helper process, so its entry is never tombstoned under it.)
func (v *view) tombstonable(e Entry) bool {
	return e.BootTime == v.boot && e.Kind != KindLeaked && v.helperGone(e) &&
		v.vzCount(e.UID) == 0 && v.helperCount(e.UID) == 0
}

// tombstoned: a tombstone exists and the condition still holds now.
func (v *view) tombstoned(e Entry) bool {
	_, ok := v.tombs[e.file]
	return ok && v.tombstonable(e)
}

// ReclaimOwn removes one of this runner's entries whose helper died without
// reporting a stop, or that another login tombstoned: allowed once the helper
// is gone, no process may still be the entry's VM (mayBeVM), and the run dir
// (empty: none) is gone. That last step is the owner's reconcile, which frees
// a tombstoned entry's disk debt.
func (l *Ledger) ReclaimOwn(id, runDir string) error {
	return l.withLock(func() error {
		v, err := l.load()
		if err != nil {
			return err
		}
		e, err := l.own(v, id)
		if err != nil {
			return err
		}
		if e.BootTime == v.boot {
			if e.Kind == KindLeaked {
				return errors.New("admission: a leaked slot clears only when the Mac restarts")
			}
			if !v.helperGone(e) {
				return errors.New("admission: the helper is still running")
			}
			if v.mayBeVM(e, time.Now().UnixMicro()) {
				return errors.New("admission: a Virtualization or helper process of this user may still be this VM")
			}
		}
		if runDir != "" {
			if _, err := os.Lstat(runDir); !os.IsNotExist(err) {
				return errors.New("admission: the run directory is not gone yet")
			}
		}
		return l.remove(e)
	})
}

// Sweep tidies this login's files and tombstones other logins' dead entries
// (helper gone and no Virtualization process of that uid), never a leaked
// one. It never deletes another login's file. It returns how many
// tombstones it wrote.
func (l *Ledger) Sweep() (int, error) {
	n := 0
	err := l.withLock(func() error {
		v, err := l.load()
		if err != nil {
			return err
		}
		l.tidy(v)
		for _, e := range v.all {
			if e.UID == l.c.UID || !v.tombstonable(e) {
				continue
			}
			if _, ok := v.tombs[e.file]; ok {
				continue
			}
			if err := l.writeFile(e.file+tombSuffix, tombstoneBody(e)); err != nil {
				return err
			}
			n++
		}
		return nil
	})
	return n, err
}

// tidy runs in each writing transaction: this login deletes its own entries
// from an earlier boot, and its own tombstones whose entry is gone or from an
// earlier boot. Its own tombstoned entries stay until ReclaimOwn reconciles
// them, since their disk is a debt until the run files are gone.
func (l *Ledger) tidy(v *view) {
	byFile := map[string]Entry{}
	for _, e := range v.all {
		byFile[e.file] = e
		if e.UID == l.c.UID && e.BootTime != v.boot {
			if os.Remove(filepath.Join(l.c.Dir, e.file)) == nil {
				delete(byFile, e.file)
			}
		}
	}
	for entry, t := range v.tombs {
		if t.owner != l.c.UID {
			continue
		}
		if e, ok := byFile[entry]; !ok || e.BootTime != v.boot {
			_ = os.Remove(filepath.Join(l.c.Dir, t.name))
		}
	}
}

// tombstoneBody records the tombstone's state: the slot and memory are
// recovered, the disk and the owner's files are not.
func tombstoneBody(e Entry) []byte {
	b, _ := json.Marshal(map[string]any{
		"entry": e.ID, "capacity": "recovered", "owner_cleanup": "pending", "disk_debt_bytes": e.DiskReserve,
	})
	return append(b, '\n')
}

// Debts returns the honoured-tombstoned entries whose disk is still held
// (owner-file cleanup pending).
func (l *Ledger) Debts() ([]Entry, error) {
	var out []Entry
	err := l.withLock(func() error {
		v, err := l.load()
		if err != nil {
			return err
		}
		out = v.debts
		return nil
	})
	return out, err
}
