// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build macvm_live

package macvmlive

import (
	"encoding/json"
	"fmt"
	"os"
	"os/user"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/macvm/admission"
)

// setLimits changes the window runner's totals with no run live on it (runbook P12 sizing).
func setLimits(t *testing.T, total, perRun int) {
	t.Helper()
	mustOK(t, wardyn(t, "runner", "set", cfg.runner, "--total-memory", strconv.Itoa(total), "--per-run-memory", strconv.Itoa(perRun)),
		fmt.Sprintf("runner set %d / %d", total, perRun))
}

// runnerRow is the window runner's row from `wardyn runner list --json`, kept whole.
func runnerRow(t *testing.T) map[string]any {
	t.Helper()
	var rows []map[string]any
	if err := json.Unmarshal([]byte(mustOK(t, wardyn(t, "runner", "list", "--json"), "runner list")), &rows); err != nil {
		t.Fatalf("runner list JSON: %v", err)
	}
	for _, r := range rows {
		if fmt.Sprint(r["id"]) == cfg.runner || fmt.Sprint(r["name"]) == cfg.runner {
			return r
		}
	}
	t.Fatalf("runner %q is not in runner list", cfg.runner)
	return nil
}

// fits is how many runs of g GiB guest memory each independent resource admits, recorded before
// a step so the refusal that fires can be attributed.
type fits struct {
	Licence, Runner, HostMemory, Disk int
}

// preconditions records every independent resource and run-count precondition before a step: the
// licence count, the runner's totals, the host memory budget and the disk. total and perRun are the
// runner limits just set (GiB); live is the runs already live on this Mac.
func preconditions(t *testing.T, step string, total, perRun int) fits {
	t.Helper()
	fs := ledger(t)
	live := counted(fs)
	memsize, _ := strconv.ParseInt(strings.TrimSpace(sh(t, 10*time.Second, "sysctl", "-n", "hw.memsize").out), 10, 64)
	docker := map[int]uint64{os.Getuid(): uint64(cfg.dockerMemGiB) * gib}
	var reserved uint64
	for _, f := range fs {
		if !f.Tomb {
			docker[f.UID] = max(docker[f.UID], f.Entry.DockerMemory)
			reserved += f.Entry.GuestMemory + f.Entry.Overhead
		}
	}
	var dockerSum uint64
	for _, d := range docker {
		dockerSum += d
	}
	per := uint64(perRun)*gib + admission.DefaultOverheadBytes
	budget := int64(memsize) - int64(dockerSum) - int64(admission.MacOSReserveBytes) - int64(reserved)
	reserve := int64(cfg.runDiskGiB)*gib + admission.DiskExtraBytes
	free := freeBytes(t, layout(t).Root) - admission.DiskFloorBytes
	f := fits{
		Licence:    admission.LicenceCap - live,
		Runner:     total/perRun - ownLive(t, fs),
		HostMemory: int(max(budget, 0) / int64(per)),
		Disk:       int(max(free, 0) / reserve),
	}
	row := runnerRow(t)
	keepJSON(t, "pre-"+step+".json", map[string]any{
		"ledger_counted": live, "licence_cap": admission.LicenceCap, "runner": row,
		"runner_set_gib": []int{total, perRun}, "hw_memsize": memsize, "docker_bytes_by_uid": docker,
		"host_memory_budget_bytes": budget, "free_disk_bytes": free + admission.DiskFloorBytes,
		"runs_more_admitted_by": f, "runnerd_status": runnerdStatus(t),
	})
	t.Logf("RECORD %s more runs fit: licence=%d runner=%d host_memory=%d disk=%d", step, f.Licence, f.Runner, f.HostMemory, f.Disk)
	return f
}

func ownLive(t *testing.T, fs []ledgerFile) int {
	t.Helper()
	n := 0
	for _, f := range fs {
		if !f.Tomb && f.UID == os.Getuid() && f.Entry.Kind != admission.KindLeaked {
			n++
		}
	}
	return n
}

// noLeak fails P12 on a code-6 slot leak: a leaked entry in the ledger.
func noLeak(t *testing.T) {
	t.Helper()
	for _, f := range ledger(t) {
		if f.Entry.Kind == admission.KindLeaked {
			keepJSON(t, "leaked.json", f)
			t.Fatalf("a code-6 slot leak fails P12: leaked entry %s; check `max` lowered and \"needs a restart\" in the drawer separately", f.Name)
		}
	}
}

// waitCounted waits until the ledger counts n live entries.
func waitCounted(t *testing.T, n int, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for counted(ledger(t)) != n {
		if time.Now().After(deadline) {
			t.Fatalf("the ledger counts %d entries, want %d after %s: a slot was not freed", counted(ledger(t)), n, within)
		}
		time.Sleep(time.Second)
	}
}

// TestP12_CapSlotsAndLogins: the cap, `max`=1, slot release, the second login's removal attempt
// (phases 1-3, automated) and two logins with a logout and return (phases 4-6, a person's).
// Admission is never loosened to make a phase fit; a phase that cannot isolate its witness says
// NOT RUN with the reason. Licence isolation itself is a ledger unit test, not this suite's claim.
func TestP12_CapSlotsAndLogins(t *testing.T) {
	live(t)
	if n := counted(ledger(t)); n != 0 {
		t.Fatalf("P12 starts with no VM on this Mac; the ledger counts %d", n)
	}
	t.Run("phase1_cap", func(t *testing.T) {
		g := 4
		if cfg.baseMinMemGiB > 4 {
			g = 6 // the fallback: the runner's total then refuses too, recorded below
		}
		setLimits(t, 12, g)
		for i := range 2 {
			preconditions(t, fmt.Sprintf("phase1-start%d", i+1), 12, g)
			start(t, nil)
		}
		f := preconditions(t, "phase1-third", 12, g)
		r := askRefused(t, nil)
		t.Logf("RECORD phase1 third_refused_by=%s reason=%q", r.By, r.Reason)
		if r.By == "none" || strings.HasPrefix(r.By, "none") {
			t.Fatalf("the 3rd macOS VM was not refused (run %s is live)", r.RunID)
		}
		t.Run("licence_witness", func(t *testing.T) {
			if f.Runner < 1 || f.HostMemory < 1 || f.Disk < 1 {
				t.Skipf("NOT RUN: the licence cap is not the only limit here (more runs fit: runner=%d host_memory=%d disk=%d); the refusal is recorded, and the ledger unit tests prove the cap", f.Runner, f.HostMemory, f.Disk)
			}
			if !msg["licence"].MatchString(r.Reason) {
				t.Errorf("the 3rd was refused by %s without the licence message: %q", r.By, r.Reason)
			}
		})
		noLeak(t)
	})
	endAll(t)
	t.Run("phase2_max1", func(t *testing.T) {
		setLimits(t, 12, 7)
		preconditions(t, "phase2-start", 12, 7)
		start(t, nil)
		preconditions(t, "phase2-second", 12, 7)
		r := askRefused(t, nil)
		t.Logf("RECORD phase2 second_refused_by=%s reason=%q", r.By, r.Reason)
		if strings.HasPrefix(r.By, "none") {
			t.Fatalf("with one run's room the 2nd was not refused (run %s)", r.RunID)
		}
		if r.By != "placement" {
			t.Errorf("the 2nd was refused by %s; placement's total (12 < 2 x 7) should refuse it, while the ledger max stays %d", r.By, admission.LicenceCap)
		}
		noLeak(t)
	})
	endAll(t)
	t.Run("phase3_stop_loops", func(t *testing.T) {
		setLimits(t, 12, 6)
		for _, path := range []string{"guest", "host"} {
			for i := range cfg.stopLoops {
				preconditions(t, fmt.Sprintf("phase3-%s-%d", path, i+1), 12, 6)
				id := start(t, nil)
				if path == "guest" {
					guest(t, id, "nohup sh -c 'sleep 2; "+assumedGuestStop+"' >/dev/null 2>&1 &", time.Minute)
				} else {
					wardyn(t, "run", "kill", id)
				}
				deadline := time.Now().Add(3 * time.Minute)
				for !get(t, id).terminal() && time.Now().Before(deadline) {
					time.Sleep(2 * time.Second)
				}
				if !get(t, id).terminal() {
					t.Fatalf("%s stop %d: run %s did not end", path, i+1, id)
				}
				waitCounted(t, 0, 2*time.Minute)
				noLeak(t)
			}
		}
	})
	t.Run("phase3_other_login_cannot_remove", func(t *testing.T) {
		b := os.Getenv("WARDYN_MACVM_USER_B")
		need(t, b, "WARDYN_MACVM_USER_B (the second macOS user, made with sysadminctl -addUser)")
		start(t, nil)
		var mine string
		for _, f := range ledger(t) {
			if !f.Tomb && f.UID == os.Getuid() {
				mine = f.Name
			}
		}
		path := admission.DefaultDir + "/" + mine
		if r := sh(t, time.Minute, "sudo", "-n", "-u", b, "rm", "-f", path); r.code != 0 && strings.Contains(r.err, "password") {
			needPerson(t, "phase 3 (sudo to the second user needs its password, which stays with the person)")
			doStep(t, "As "+b+", run: rm -f '"+path+"'")
		}
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("the second user removed the first user's ledger entry: %v", err)
		}
	})
	endAll(t)
	t.Run("phases4to6_two_logins", func(t *testing.T) { twoLogins(t) })
}

// endAll ends every live run this suite started and waits for the ledger to empty.
func endAll(t *testing.T) {
	t.Helper()
	out := wardyn(t, "run", "list", "--json").out
	var rs []run
	_ = json.Unmarshal([]byte(out), &rs)
	for _, r := range rs {
		if r.MachineKind == "macos_vm" && !r.terminal() {
			end(t, r.ID)
		}
	}
	waitCounted(t, 0, 3*time.Minute)
}

func twoLogins(t *testing.T) {
	if !cfg.twoLogins {
		t.Skip("phases 4-6 are manual: run in the two-login slot with WARDYN_MACVM_TWO_LOGINS=1 and WARDYN_MACVM_PROMPTS=1")
	}
	needPerson(t, "the two-login slot (the second GUI login)")
	b := os.Getenv("WARDYN_MACVM_USER_B")
	need(t, b, "WARDYN_MACVM_USER_B")
	ub, err := user.Lookup(b)
	if err != nil {
		t.Fatal(err)
	}
	uidB, _ := strconv.Atoi(ub.Uid)
	aTotal, aPer, bDocker, bLimit := 8, 4, 5, 4
	switch m := cfg.baseMinMemGiB; {
	case m <= 4:
	case m <= 5:
		doStep(t, "Restart login A's Docker at 10 GiB (the org stack restarts with it).")
		aTotal, aPer, bDocker, bLimit = 10, 5, 5, 5
	default:
		t.Skipf("NOT RUN: the base needs %d GiB per guest, so two logins do not fit this host (runbook P12 sizing); the ledger unit tests are the only proof and the note says two logins are untested", m)
	}
	setLimits(t, aTotal, aPer)
	doStep(t, fmt.Sprintf("Log in as %s in its own GUI session; start its Docker at %d GiB; run `wardyn-runnerd setup --total-memory %d --per-run-memory %d` with macOS VMs on, register and mark it like the first, and wait until its own prepare is ready.", b, bDocker, bLimit, bLimit))
	t.Run("phase4", func(t *testing.T) {
		preconditions(t, "phase4-a", aTotal, aPer)
		start(t, nil)
		idB := ask(t, "As "+b+", start one macOS VM run on its runner. Paste its run id:")
		if v := wait(t, idB, 5*time.Minute); v.State != "RUNNING" {
			t.Fatalf("login B's run is %s: %s", v.State, v.FailureHint)
		}
		preconditions(t, "phase4-third", aTotal, aPer)
		r := askRefused(t, nil)
		t.Logf("RECORD phase4 third_from_A_refused_by=%s reason=%q", r.By, r.Reason)
		if strings.HasPrefix(r.By, "none") {
			t.Errorf("a 3rd from login A was admitted (run %s)", r.RunID)
		}
		record(t, "phase4_third_from_B", ask(t, "As "+b+", ask for one more run: paste the refusal it shows (or 'admitted'):"))
		listing := sh(t, time.Minute, "ls", "-l", admission.DefaultDir).out
		keep(t, "ledger-ls.txt", []byte(listing))
		for _, f := range ledger(t) {
			if !f.Tomb && (f.Mode.Perm() != 0o644 || f.UID != os.Getuid() && f.UID != uidB) {
				t.Errorf("entry %s is mode %v owned by %d; want 0644 owned by its login", f.Name, f.Mode.Perm(), f.UID)
			}
		}
	})
	t.Run("phase5_logout", func(t *testing.T) {
		doStep(t, "Log "+b+" out (Apple menu > Log Out) while its VM runs.")
		deadline := time.Now().Add(5 * time.Minute)
		for procs(t, "com.apple.Virtualization.VirtualMachine", uidB)+procs(t, admission.HelperProcess, uidB) > 0 {
			if time.Now().After(deadline) {
				t.Fatalf("login B's VM processes are still there 5 minutes after logout")
			}
			time.Sleep(2 * time.Second)
		}
		var entryB string
		deadline = time.Now().Add(2 * admission.OfferRecompute)
		for entryB == "" {
			for _, f := range ledger(t) {
				if f.Tomb && f.UID == os.Getuid() {
					entryB = strings.TrimSuffix(f.Name, ".tombstone")
				}
			}
			if time.Now().After(deadline) {
				t.Fatal("login A's runner wrote no tombstone for login B's entry")
			}
			time.Sleep(time.Second)
		}
		preconditions(t, "phase5-second", aTotal, aPer)
		start(t, nil) // without a restart
		if _, err := os.Stat(admission.DefaultDir + "/" + entryB); err != nil {
			t.Errorf("login B's entry is gone, so its disk reservation is not held: %v", err)
		}
		if !confirm(t, "Does login A's runner drawer show disk held for another person's run?") {
			t.Error("the drawer does not show the held disk")
		}
	})
	t.Run("phase6_return", func(t *testing.T) {
		doStep(t, "Log "+b+" back in, and start its Docker and runner.")
		deadline := time.Now().Add(5 * time.Minute)
		for {
			left := 0
			for _, f := range ledger(t) {
				if f.UID == uidB || f.Tomb {
					left++
				}
			}
			if left == 0 {
				break
			}
			if time.Now().After(deadline) {
				keepJSON(t, "ledger-left.json", ledger(t))
				t.Fatal("login B's entry or the tombstone is still there 5 minutes after its return")
			}
			time.Sleep(2 * time.Second)
		}
		if !confirm(t, "As "+b+", is its runs/ dir under ~/Library/Application Support/Wardyn/macos-vm.noindex empty?") {
			t.Error("login B's runner did not remove its run dir")
		}
	})
}
