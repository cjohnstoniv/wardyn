// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
	"github.com/cjohnstoniv/wardyn/internal/macvm/admission"
)

// argvHost is the rig's Mac that can also read argv; a pid with no argv here cannot be read.
type argvHost struct {
	*macHost
	mu   sync.Mutex
	argv map[int][]string
	// fail is how many of the next process-table reads fail.
	fail int
}

func (h *argvHost) Processes() ([]admission.Proc, error) {
	h.mu.Lock()
	if h.fail > 0 {
		h.fail--
		h.mu.Unlock()
		return nil, errors.New("process table unreadable")
	}
	h.mu.Unlock()
	return h.macHost.Processes()
}

func (h *argvHost) failReads(n int) { h.mu.Lock(); h.fail = n; h.mu.Unlock() }

func (h *argvHost) Args(pid int) ([]string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	a, ok := h.argv[pid]
	if !ok {
		return nil, errors.New("argv unreadable")
	}
	return a, nil
}

// reapRig is runner-a (this runnerd, started at 100) with the processes a reap looks at.
type reapRig struct {
	r     *macLedgerRig
	m     *macOSVMs
	host  *argvHost
	procs []admission.Proc
	mu    sync.Mutex
	sent  []string // "TERM 40", "KILL 40"
	at    []time.Time
	// onTerm, when set, is what a helper does on SIGTERM; nil: it exits.
	onTerm func(pid int)
}

const (
	reapSelfStart  = 100
	reapDeadOwner  = 30 // the earlier runnerd: not in the process table
	reapOwnerStart = 5
)

func newReapRig(t *testing.T) *reapRig {
	t.Helper()
	rr := &reapRig{r: newMacLedgerRig(t)}
	rr.procs = []admission.Proc{{PID: os.Getpid(), UID: 501, Start: reapSelfStart, Name: "wardyn-runnerd"}}
	rr.r.host.setProcs(rr.procs...)
	rr.m = rr.r.open(t)
	rr.host = &argvHost{macHost: rr.r.host, argv: map[int][]string{}}
	rr.m.host = rr.host
	prevSig, prevGrace := reapSignal, reapGrace
	reapGrace = 200 * time.Millisecond
	reapSignal = func(pid int, sig syscall.Signal) error {
		rr.mu.Lock()
		name := map[syscall.Signal]string{syscall.SIGTERM: "TERM", syscall.SIGKILL: "KILL"}[sig]
		rr.sent = append(rr.sent, name+" "+strconv.Itoa(pid))
		rr.at = append(rr.at, time.Now())
		onTerm := rr.onTerm
		rr.mu.Unlock()
		if sig == syscall.SIGTERM && onTerm != nil {
			onTerm(pid)
			return nil
		}
		rr.drop(pid)
		return nil
	}
	t.Cleanup(func() { reapSignal, reapGrace = prevSig, prevGrace })
	return rr
}

func (rr *reapRig) drop(pid int) {
	rr.mu.Lock()
	rr.procs = slices.DeleteFunc(rr.procs, func(p admission.Proc) bool { return p.PID == pid })
	ps := slices.Clone(rr.procs)
	rr.mu.Unlock()
	rr.r.host.setProcs(ps...)
}

// helper adds a wardyn-macvm process of uid 501 with argv (nil: unreadable).
func (rr *reapRig) helper(pid int, start int64, argv []string) {
	rr.mu.Lock()
	rr.procs = append(rr.procs, admission.Proc{PID: pid, UID: 501, Start: start, Name: admission.HelperProcess})
	ps := slices.Clone(rr.procs)
	rr.mu.Unlock()
	rr.r.host.setProcs(ps...)
	if argv != nil {
		rr.host.mu.Lock()
		rr.host.argv[pid] = argv
		rr.host.mu.Unlock()
	}
}

// entry admits one of runner-a's entries of kind k, owned by the earlier runnerd.
func (rr *reapRig) entry(t *testing.T, k admission.Kind) admission.Entry {
	t.Helper()
	e, err := rr.m.ledger.Admit(admission.Request{Kind: k, GuestCPUs: 4, GuestMemory: 8 * gib,
		DiskReserve: admission.RunDiskReservation(macvm.BaseDiskBytes), OwnerPID: reapDeadOwner, OwnerStart: reapOwnerStart})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func (rr *reapRig) signals() []string {
	rr.mu.Lock()
	defer rr.mu.Unlock()
	return slices.Clone(rr.sent)
}

// jobArgv is the argv runnerd's startJob gives a helper.
func jobArgv(sub, runner, entry string) []string {
	return []string{"/usr/local/libexec/wardyn-macvm", sub, "--" + macvm.FlagRoot, "/Users/a/Library/macos-vm.noindex", "--" + macvm.FlagCPUs, "4",
		"--" + macvm.FlagMemoryMiB, "8192", "--" + macvm.FlagRunnerID, runner, "--" + macvm.FlagLedgerEntry, entry}
}

// The reap ends a helper only when it is positively this runner's unclaimed job helper from an earlier
// runnerd: same uid, started before this runnerd, an update-tools or prepare argv naming this runner and
// an entry, and that entry absent or not claimed by it with its owner gone. Anything else stays.
func TestReapEndsOnlyAPreviousRunnersUnclaimedHelper(t *testing.T) {
	const pid = 40
	for _, tc := range []struct {
		name string
		set  func(t *testing.T, rr *reapRig)
		end  bool
	}{
		{"unclaimed, owner dead", func(t *testing.T, rr *reapRig) {
			e := rr.entry(t, admission.KindToolsUpdate)
			rr.helper(pid, 50, jobArgv(macvm.UpdateToolsCommand, "runner-a", e.ID))
		}, true},
		{"unclaimed prepare, owner dead", func(t *testing.T, rr *reapRig) {
			e := rr.entry(t, admission.KindPrepare)
			rr.helper(pid, 50, jobArgv(macvm.PrepareCommand, "runner-a", e.ID))
		}, true},
		{"entry already gone", func(t *testing.T, rr *reapRig) {
			rr.helper(pid, 50, jobArgv(macvm.UpdateToolsCommand, "runner-a", "0123456789abcdef"))
		}, true},
		{"flag=value argv", func(t *testing.T, rr *reapRig) {
			e := rr.entry(t, admission.KindToolsUpdate)
			rr.helper(pid, 50, []string{"wardyn-macvm", macvm.UpdateToolsCommand, "--runner-id=runner-a", "-ledger-entry=" + e.ID})
		}, true},
		{"entry claimed by another process: this one is still unclaimed", func(t *testing.T, rr *reapRig) {
			e := rr.entry(t, admission.KindToolsUpdate)
			rr.claim(t, e.ID, 41, 51)
			rr.helper(pid, 50, jobArgv(macvm.UpdateToolsCommand, "runner-a", e.ID))
		}, true},
		{"claimed by its pid", func(t *testing.T, rr *reapRig) {
			e := rr.entry(t, admission.KindToolsUpdate)
			rr.claim(t, e.ID, pid, 50)
			rr.helper(pid, 50, jobArgv(macvm.UpdateToolsCommand, "runner-a", e.ID))
		}, false},
		{"owner still running", func(t *testing.T, rr *reapRig) {
			e := rr.entry(t, admission.KindToolsUpdate)
			rr.mu.Lock()
			rr.procs = append(rr.procs, admission.Proc{PID: reapDeadOwner, UID: 501, Start: reapOwnerStart, Name: "wardyn-runnerd"})
			rr.mu.Unlock()
			rr.helper(pid, 50, jobArgv(macvm.UpdateToolsCommand, "runner-a", e.ID))
		}, false},
		{"other runner id", func(t *testing.T, rr *reapRig) {
			e := rr.entry(t, admission.KindToolsUpdate)
			rr.helper(pid, 50, jobArgv(macvm.UpdateToolsCommand, "runner-b", e.ID))
		}, false},
		{"started after this runnerd", func(t *testing.T, rr *reapRig) {
			e := rr.entry(t, admission.KindToolsUpdate)
			rr.helper(pid, reapSelfStart+1, jobArgv(macvm.UpdateToolsCommand, "runner-a", e.ID))
		}, false},
		{"argv unreadable", func(t *testing.T, rr *reapRig) {
			rr.entry(t, admission.KindToolsUpdate)
			rr.helper(pid, 50, nil)
		}, false},
		{"edit-base", func(t *testing.T, rr *reapRig) {
			e := rr.entry(t, kindEditBase)
			rr.helper(pid, 50, jobArgv(macvm.EditBaseCommand, "runner-a", e.ID))
		}, false},
		{"no ledger entry flag", func(t *testing.T, rr *reapRig) {
			rr.helper(pid, 50, []string{"wardyn-macvm", macvm.UpdateToolsCommand, "--runner-id", "runner-a"})
		}, false},
		{"another user's process", func(t *testing.T, rr *reapRig) {
			e := rr.entry(t, admission.KindToolsUpdate)
			rr.helper(pid, 50, jobArgv(macvm.UpdateToolsCommand, "runner-a", e.ID))
			rr.mu.Lock()
			rr.procs[len(rr.procs)-1].UID = 502
			ps := slices.Clone(rr.procs)
			rr.mu.Unlock()
			rr.r.host.setProcs(ps...)
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rr := newReapRig(t)
			tc.set(t, rr)
			rr.r.host.setProcs(rr.procs...)
			rr.m.reapOrphanHelpers(context.Background())
			got := rr.signals()
			if tc.end && !slices.Equal(got, []string{"TERM 40"}) {
				t.Fatalf("signals %q, want one SIGTERM to the helper", got)
			}
			if !tc.end && len(got) != 0 {
				t.Fatalf("signals %q, want none", got)
			}
		})
	}
}

// claim records pid (started at start) as entry id's helper, as the helper's ClaimHelper does.
func (rr *reapRig) claim(t *testing.T, id string, pid int, start int64) {
	t.Helper()
	if _, err := rr.m.ledger.Update(id, func(e *admission.Entry) { e.HelperPID, e.HelperStart, e.State = pid, start, admission.StateStarting }); err != nil {
		t.Fatal(err)
	}
}

// A helper that ignores SIGTERM gets SIGKILL after the grace; one that claims its entry during the grace
// is adopted, not killed.
func TestReapKillsAfterTheGraceUnlessTheHelperClaimed(t *testing.T) {
	rr := newReapRig(t)
	e := rr.entry(t, admission.KindToolsUpdate)
	rr.helper(40, 50, jobArgv(macvm.UpdateToolsCommand, "runner-a", e.ID))
	rr.onTerm = func(int) {} // ignores it
	rr.m.reapOrphanHelpers(context.Background())
	if got := rr.signals(); !slices.Equal(got, []string{"TERM 40", "KILL 40"}) {
		t.Fatalf("a helper that ignored SIGTERM: signals %q", got)
	}

	rr = newReapRig(t)
	e = rr.entry(t, admission.KindToolsUpdate)
	rr.helper(40, 50, jobArgv(macvm.UpdateToolsCommand, "runner-a", e.ID))
	rr.onTerm = func(pid int) { rr.claim(t, e.ID, pid, 50) }
	rr.m.reapOrphanHelpers(context.Background())
	if got := rr.signals(); !slices.Equal(got, []string{"TERM 40"}) {
		t.Fatalf("a helper that claimed during the grace: signals %q", got)
	}
}

// The slots an earlier runnerd's unclaimed helpers held come back: before the reap the ledger refuses to
// release their entries (a helper process of this user may be the VM), after it reconcile frees both.
func TestReapFreesTheSlotsItsHelpersHeld(t *testing.T) {
	rr := newReapRig(t)
	a, b := rr.entry(t, admission.KindToolsUpdate), rr.entry(t, admission.KindToolsUpdate)
	rr.helper(40, 50, jobArgv(macvm.UpdateToolsCommand, "runner-a", a.ID))
	rr.helper(41, 52, jobArgv(macvm.UpdateToolsCommand, "runner-a", b.ID))
	perRunDisk := admission.RunDiskReservation(macvm.BaseDiskBytes)
	rr.m.reconcilePrepare()
	if o, _ := rr.m.ledger.Offer(8*gib, perRunDisk); o.Max != 0 {
		t.Fatalf("before the reap: max %d, want 0 (both slots held)", o.Max)
	}
	rr.m.reapOrphanHelpers(context.Background())
	if counted, _ := rr.m.ledger.Counted(); len(counted) != 0 {
		t.Fatalf("entries left after the reap: %+v", counted)
	}
	if o, _ := rr.m.ledger.Offer(8*gib, perRunDisk); o.Max != admission.LicenceCap {
		t.Fatalf("after the reap: max %d, want %d", o.Max, admission.LicenceCap)
	}
}

// A process-table read that fails during the grace never stands in for a live check: the helper exits,
// its pid goes to another process of this login (same name and argv, a later start), and SIGKILL must not
// reach that process although the ledger read after it succeeds.
func TestReapNeverKillsAReusedPidAfterAFailedRead(t *testing.T) {
	rr := newReapRig(t)
	e := rr.entry(t, admission.KindToolsUpdate)
	rr.helper(40, 50, jobArgv(macvm.UpdateToolsCommand, "runner-a", e.ID))
	rr.onTerm = func(pid int) {
		rr.drop(pid)
		rr.helper(pid, 60, jobArgv(macvm.UpdateToolsCommand, "runner-a", e.ID))
		rr.host.failReads(1)
	}
	rr.m.reapOrphanHelpers(context.Background())
	if got := rr.signals(); !slices.Equal(got, []string{"TERM 40"}) {
		t.Fatalf("signals %q, want only the SIGTERM to the old helper", got)
	}
}

// A failed read mid-grace does not end the grace early; a helper still there at its end, read again and
// still unclaimed, gets SIGKILL then. When the process table cannot be read at all, nothing is killed.
func TestReapFailedReadDoesNotEscalateEarly(t *testing.T) {
	rr := newReapRig(t)
	e := rr.entry(t, admission.KindToolsUpdate)
	rr.helper(40, 50, jobArgv(macvm.UpdateToolsCommand, "runner-a", e.ID))
	rr.onTerm = func(int) { rr.host.failReads(1) } // ignores SIGTERM
	rr.m.reapOrphanHelpers(context.Background())
	if got := rr.signals(); !slices.Equal(got, []string{"TERM 40", "KILL 40"}) {
		t.Fatalf("signals %q", got)
	}
	rr.mu.Lock()
	waited := rr.at[1].Sub(rr.at[0])
	rr.mu.Unlock()
	if waited < reapGrace {
		t.Fatalf("SIGKILL %v after SIGTERM, before the %v grace ended", waited, reapGrace)
	}

	rr = newReapRig(t)
	e = rr.entry(t, admission.KindToolsUpdate)
	rr.helper(40, 50, jobArgv(macvm.UpdateToolsCommand, "runner-a", e.ID))
	rr.onTerm = func(int) { rr.host.failReads(1 << 20) }
	rr.m.reapOrphanHelpers(context.Background())
	if got := rr.signals(); !slices.Equal(got, []string{"TERM 40"}) {
		t.Fatalf("with the process table unreadable: signals %q, want no SIGKILL", got)
	}
}

// A reap that stands down because the ledger cannot be read, or this process is not in the process
// table, says so once: the symptom is both VM slots staying taken.
func TestReapStandingDownLogsOneWarning(t *testing.T) {
	for _, tc := range []struct {
		name string
		stop func(rr *reapRig)
		want string
	}{
		{"ledger unreadable", func(rr *reapRig) { _ = os.RemoveAll(rr.r.dir) }, "ledger"},
		{"this process not in the table", func(rr *reapRig) { rr.drop(os.Getpid()) }, "process table"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rr := newReapRig(t)
			tc.stop(rr)
			var logs strings.Builder
			var mu sync.Mutex
			prev := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(lockedWriter{&mu, &logs}, nil)))
			defer slog.SetDefault(prev)
			rr.m.reapOrphanHelpers(context.Background())
			mu.Lock()
			got := logs.String()
			mu.Unlock()
			if n := strings.Count(got, "level=WARN"); n != 1 || !strings.Contains(got, tc.want) {
				t.Errorf("log %q: want exactly one WARN naming %q", got, tc.want)
			}
		})
	}
}
