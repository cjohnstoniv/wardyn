// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package admission

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
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

// mayBeVM: a process in the snapshot may still be e's VM. e's VM, and every
// helper process of e's (an install process included), was created after e's
// helper was, or for an entry with no helper recorded, after its owner was.
// Creation order is read from MonoStart, never from calendar start times: a
// clock stepped back while the VM was created, then restored, would make the
// VM look older than its helper. A Virtualization or helper process of e's
// uid is proven not to be them only when its MonoStart and e's are both known
// and its is earlier: Docker's Linux VM, or another macOS VM's, from earlier.
// Anything unknown (an entry an earlier release wrote, a start that could not
// be read) leaves it possibly e's, as does its name, its PID (reused or not)
// and its parent (reparented once the helper died). One exception is
// positive: a process that is exactly (PID and start) the recorded helper of
// another counted entry of the same uid is that entry's helper, not e's; its
// VM, if it has one yet, still counts.
func (v *view) mayBeVM(e Entry) bool {
	since := e.HelperMonoStart
	if e.HelperPID == 0 {
		since = e.OwnerMonoStart
	}
	type incarnation struct {
		pid   int
		start int64
	}
	helpers := map[incarnation]bool{}
	for _, o := range v.counted {
		if o.ID != e.ID && o.UID == e.UID && o.HelperPID != 0 {
			helpers[incarnation{o.HelperPID, o.HelperStart}] = true
		}
	}
	for _, p := range v.procs {
		if helpers[incarnation{p.PID, p.Start}] {
			continue
		}
		if p.UID == e.UID && (named(p, VirtualizationProcess) || named(p, HelperProcess)) &&
			(since == 0 || p.MonoStart == 0 || p.MonoStart >= since) {
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
	return v.sameBoot(e) && e.Kind != KindLeaked && v.helperGone(e) &&
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
		if v.sameBoot(e) {
			if e.Kind == KindLeaked {
				return errors.New("admission: a leaked slot clears only when the Mac restarts")
			}
			if !v.helperGone(e) {
				return errors.New("admission: the helper is still running")
			}
			if v.mayBeVM(e) {
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
// from an earlier boot (sameBoot), and its own tombstones whose entry is gone
// or from an earlier boot. Its own tombstoned entries stay until ReclaimOwn
// reconciles them, since their disk is a debt until the run files are gone.
func (l *Ledger) tidy(v *view) {
	byFile := map[string]Entry{}
	for _, e := range v.all {
		byFile[e.file] = e
		if e.UID == l.c.UID && !v.sameBoot(e) {
			if os.Remove(filepath.Join(l.c.Dir, e.file)) == nil {
				delete(byFile, e.file)
			}
		}
	}
	for entry, t := range v.tombs {
		if t.owner != l.c.UID {
			continue
		}
		if e, ok := byFile[entry]; !ok || !v.sameBoot(e) {
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
