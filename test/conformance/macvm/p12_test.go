// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build macvm_live

package macvmlive

import (
	"encoding/json"
	"fmt"
	"os"
	"os/user"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/macvm/admission"
)

// setLimits changes the window runner's totals with no run live on it.
func setLimits(t *testing.T, total, perRun int) {
	t.Helper()
	mustOK(t, wardyn(t, "runner", "set", runnerID(t), "--total-memory", strconv.Itoa(total), "--per-run-memory", strconv.Itoa(perRun)),
		fmt.Sprintf("runner set %d / %d", total, perRun))
}

// fits is how many more runs of the runner's per-run size each independent limit admits, recorded
// before a step so the refusal that fires can be attributed.
type fits struct {
	Licence                                            int // the licence cap less the ledger's counted entries
	RunnerRuns, RunnerCPU, RunnerMemory, RunnerDisk    int // the runner's runs at once and its totals left
	HostMemory                                         int // the host memory budget left
	Disk                                               int // live free space less the floor and the reservations held
	HeldDiskBytes, HostBudgetBytes, FreeBytes, Counted int64
}

// others reports whether any limit other than the licence cap would refuse one more run.
func (f fits) others() string {
	var by []string
	for name, n := range map[string]int{"runner runs": f.RunnerRuns, "runner CPU": f.RunnerCPU, "runner memory": f.RunnerMemory,
		"runner disk": f.RunnerDisk, "host memory": f.HostMemory, "disk": f.Disk} {
		if n < 1 {
			by = append(by, name)
		}
	}
	return strings.Join(by, ", ")
}

func left(avail, per int64) int {
	if per <= 0 {
		return 1 << 20 // the runner sets no per-run amount: this limit never refuses
	}
	return int(avail / per)
}

// preconditions records every independent resource and run-count precondition before a step: the
// licence count, the runner's runs at once, CPU, memory and disk, the host memory budget, and the
// live free disk with the reservations already held. perRunGiB is the per-run memory just set.
func preconditions(t *testing.T, step string, perRunGiB int) fits {
	t.Helper()
	fs := ledger(t)
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
	f := fits{Licence: admission.LicenceCap - counted(fs), Counted: int64(counted(fs)), HeldDiskBytes: heldDisk(fs)}
	f.HostBudgetBytes = int64(memsize) - int64(dockerSum) - int64(admission.MacOSReserveBytes) - int64(reserved)
	f.HostMemory = int(max(f.HostBudgetBytes, 0) / (int64(perRunGiB)*gib + int64(admission.DefaultOverheadBytes)))
	f.FreeBytes = freeBytes(t, layout(t).Root)
	f.Disk = int(max(f.FreeBytes-admission.DiskFloorBytes-f.HeldDiskBytes, 0) / (int64(cfg.runDiskGiB)*gib + admission.DiskExtraBytes))
	rv := runnerRow(t)
	f.RunnerRuns, f.RunnerCPU, f.RunnerMemory, f.RunnerDisk = 1<<20, 1<<20, 1<<20, 1<<20
	if rv.Limits != nil {
		av, per := rv.Usage.Available, rv.Limits.PerRun
		f.RunnerRuns = av.Runs
		f.RunnerCPU, f.RunnerMemory, f.RunnerDisk = left(av.CPUMillis, per.CPUMillis), left(av.MemoryMiB, per.MemoryMiB), left(av.DiskMiB, per.DiskMiB)
	}
	keepJSON(t, "pre-"+step+".json", map[string]any{"fits": f, "runner": rv, "hw_memsize": memsize, "docker_bytes_by_uid": docker, "runnerd_status": runnerdStatus(t)})
	t.Logf("RECORD %s more runs fit: licence=%d runner_runs=%d runner_cpu=%d runner_memory=%d runner_disk=%d host_memory=%d disk=%d (held disk %d bytes)",
		step, f.Licence, f.RunnerRuns, f.RunnerCPU, f.RunnerMemory, f.RunnerDisk, f.HostMemory, f.Disk, f.HeldDiskBytes)
	return f
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

// endAll ends every live macOS VM run and waits for the ledger to empty.
func endAll(t *testing.T) {
	t.Helper()
	var rs []run
	if err := json.Unmarshal([]byte(mustOK(t, wardyn(t, "run", "list", "--json"), "run list")), &rs); err != nil {
		t.Fatalf("run list JSON: %v", err)
	}
	for _, r := range rs {
		if r.MachineKind == "macos_vm" && !r.terminal() {
			end(t, r.ID)
		}
	}
	waitCounted(t, 0, 3*time.Minute)
}

// TestP12_CapSlotsAndLogins: the cap, `max`=1, slot release, the second login's removal attempt
// (phases 1-3, automated) and two logins with a logout and return (phases 4-6, a person's).
// Admission is never loosened to make a phase fit; a witness that cannot be isolated is NOT_RUN.
// The ledger's own admission refusal is covered by the ledger's unit tests, not by this suite.
func TestP12_CapSlotsAndLogins(t *testing.T) {
	live(t)
	block(t)
	if n := counted(ledger(t)); n != 0 {
		t.Fatalf("P12 starts with no VM on this Mac; the ledger counts %d", n)
	}
	t.Run("phase1_cap", func(t *testing.T) {
		g := 4
		if cfg.baseMinMemGiB > 4 {
			g = 6 // the fallback: the runner's total then refuses too, so the witness is NOT_RUN
		}
		setLimits(t, 12, g)
		for i := range 2 {
			preconditions(t, fmt.Sprintf("phase1-start%d", i+1), g)
			start(t, spec{})
		}
		f := preconditions(t, "phase1-third", g)
		r := askRefused(t, spec{})
		t.Logf("RECORD phase1 third_refused_by=%s reason=%q", r.By, r.Reason)
		if r.By == "none" {
			t.Fatalf("the 3rd macOS VM was not refused (run %s is %s)", r.RunID, r.State)
		}
		t.Run("licence_witness", func(t *testing.T) {
			if o := f.others(); o != "" {
				notRun(t, "the licence cap is not the only limit at the 3rd ask (%s would also refuse); the refusal is recorded above", o)
			}
			if f.Licence != 0 {
				t.Fatalf("the ledger counts %d entries before the 3rd ask, not 2", f.Counted)
			}
			// Placement refuses from the ledger's offer: the runner's max is its own live runs plus
			// the ledger's headroom, which the licence cap alone has brought to zero.
			mustRefuse(t, r, "placement", msg["licence"])
		})
		noLeak(t)
	})
	endAll(t)
	t.Run("phase2_max1", func(t *testing.T) {
		setLimits(t, 12, 7)
		preconditions(t, "phase2-start", 7)
		start(t, spec{})
		f := preconditions(t, "phase2-second", 7)
		r := askRefused(t, spec{})
		if f.Licence < 1 || f.HostMemory < 1 || f.Disk < 1 || f.RunnerMemory >= 1 {
			t.Logf("RECORD phase2 refused_by=%s reason=%q", r.By, r.Reason)
			notRun(t, "the runner's memory total is not the only limit at the 2nd ask (licence=%d host_memory=%d disk=%d runner_memory=%d)", f.Licence, f.HostMemory, f.Disk, f.RunnerMemory)
		}
		mustRefuse(t, r, "placement", regexp.MustCompile(msg["noRoom"].String()+`|[Mm]emory`))
		t.Logf("RECORD phase2 ledger_cap=%d (unchanged)", admission.LicenceCap)
		noLeak(t)
	})
	endAll(t)
	t.Run("phase3_stop_loops", func(t *testing.T) {
		setLimits(t, 12, 6)
		for _, path := range []string{"helper_graceful", "org_kill"} {
			t.Run(path, func(t *testing.T) {
				for i := range cfg.stopLoops {
					preconditions(t, fmt.Sprintf("phase3-%s-%d", path, i+1), 6)
					id := start(t, spec{})
					if path == "helper_graceful" {
						if err := helperStop(t, id); err != nil {
							t.Fatalf("loop %d: the helper's stop failed: %v", i+1, err)
						}
					} else {
						mustOK(t, wardyn(t, "run", "kill", id), "run kill")
					}
					deadline := time.Now().Add(3 * time.Minute)
					for !get(t, id).terminal() {
						if time.Now().After(deadline) {
							t.Fatalf("loop %d: run %s did not end", i+1, id)
						}
						time.Sleep(2 * time.Second)
					}
					waitCounted(t, 0, 2*time.Minute)
					noLeak(t)
				}
			})
		}
		t.Run("stop_from_guest", func(t *testing.T) {
			notRun(t, "no guest-side shutdown path: exec runs as the agent user with no sudoers rule and the guest agent has no shutdown op; the helper's graceful stop asks the guest instead")
		})
	})
	t.Run("phase3_other_login_cannot_remove", func(t *testing.T) { otherLoginCannotRemove(t) })
	endAll(t)
	t.Run("phases4to6_two_logins", func(t *testing.T) { twoLogins(t) })
}

var denied = regexp.MustCompile(`Permission denied|Operation not permitted`)

// otherLoginCannotRemove has the second macOS user try to delete the first user's ledger entry.
// Only an attempt that really ran as that user and was refused by the system passes.
func otherLoginCannotRemove(t *testing.T) {
	b := os.Getenv("WARDYN_MACVM_USER_B")
	need(t, b, "WARDYN_MACVM_USER_B (the second macOS user, made with sysadminctl -addUser)")
	start(t, spec{})
	var mine string
	for _, f := range ledger(t) {
		if !f.Tomb && f.UID == os.Getuid() {
			mine = f.Name
		}
	}
	if mine == "" {
		t.Fatal("this login's run holds no ledger entry")
	}
	path := admission.DefaultDir + "/" + mine
	who := sh(t, time.Minute, "sudo", "-n", "-u", b, "id", "-un")
	switch {
	case who.code == 0 && strings.TrimSpace(who.out) == b:
		r := sh(t, time.Minute, "sudo", "-n", "-u", b, "rm", path)
		record(t, "removal_attempt", clean(r.err))
		if r.code == 0 || !denied.MatchString(r.err) {
			t.Errorf("the removal as %s did not fail with a permission refusal: exit %d %q", b, r.code, r.err)
		}
	case cfg.prompts:
		doStep(t, "As "+b+" (its own login or `su "+b+"`), run: rm '"+path+"'")
		got := ask(t, "Paste exactly what rm printed:")
		if !denied.MatchString(got) {
			t.Errorf("the removal as %s was not refused with a permission error: %q", b, got)
		}
	default:
		notRun(t, "sudo cannot run as %s without a password (%s) and no person is at the Mac", b, clean(who.err))
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the second user removed the first user's ledger entry: %v", err)
	}
}

func twoLogins(t *testing.T) {
	if !cfg.twoLogins {
		notRun(t, "phases 4-6 are manual: run in the two-login slot with WARDYN_MACVM_TWO_LOGINS=1 and WARDYN_MACVM_PROMPTS=1")
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
		notRun(t, "the base needs %d GiB per guest, so two logins do not fit this host; the ledger unit tests are the only proof and the note says two logins are untested", m)
	}
	setLimits(t, aTotal, aPer)
	doStep(t, fmt.Sprintf("Log in as %s in its own GUI session; start its Docker at %d GiB; run `wardyn-runnerd setup --total-memory %d --per-run-memory %d` with macOS VMs on, register and mark it like the first, and wait until its own prepare is ready.", b, bDocker, bLimit, bLimit))
	// A's first run lives across phases 4 and 5, so it is owned by this test, not by a phase.
	var firstA, entryB string
	parent := t
	t.Run("phase4", func(t *testing.T) {
		preconditions(t, "phase4-a", aPer)
		firstA = start(t, spec{owner: parent})
		idB := ask(t, "As "+b+", start one macOS VM run on its runner. Paste its run id:")
		if v := wait(t, idB, 5*time.Minute); v.State != "RUNNING" {
			t.Fatalf("login B's run is %s: %s", v.State, v.FailureHint)
		}
		preconditions(t, "phase4-third", aPer)
		r := askRefused(t, spec{})
		t.Logf("RECORD phase4 third_from_A_refused_by=%s reason=%q", r.By, r.Reason)
		if r.By == "none" {
			t.Errorf("a 3rd from login A was admitted (run %s)", r.RunID)
		}
		if !confirm(t, "As "+b+", ask for one more run: was it refused?") {
			t.Error("a 3rd from login B was admitted")
		}
		record(t, "phase4_third_from_B", ask(t, "Paste the refusal login B saw:"))
		keep(t, "ledger-ls.txt", []byte(sh(t, time.Minute, "ls", "-l", admission.DefaultDir).out))
		for _, f := range ledger(t) {
			if !f.Tomb && (f.Mode.Perm() != 0o644 || f.UID != os.Getuid() && f.UID != uidB) {
				t.Errorf("entry %s is mode %v owned by %d; want 0644 owned by its login", f.Name, f.Mode.Perm(), f.UID)
			}
		}
	})
	t.Run("phase5_logout", func(t *testing.T) {
		if firstA == "" {
			notRun(t, "phase 4 did not start login A's run")
		}
		doStep(t, "Log "+b+" out (Apple menu > Log Out) while its VM runs.")
		deadline := time.Now().Add(5 * time.Minute)
		for procs(t, "com.apple.Virtualization.VirtualMachine", uidB)+procs(t, admission.HelperProcess, uidB) > 0 {
			if time.Now().After(deadline) {
				t.Fatalf("login B's VM processes are still there 5 minutes after logout")
			}
			time.Sleep(2 * time.Second)
		}
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
		if v := get(t, firstA); v.State != "RUNNING" {
			t.Fatalf("login A's first run %s is %s when A asks for its 2nd", firstA, v.State)
		}
		preconditions(t, "phase5-second", aPer)
		start(t, spec{owner: parent}) // without a restart
		if v := get(t, firstA); v.State != "RUNNING" {
			t.Errorf("login A's first run %s is %s after its 2nd started", firstA, v.State)
		}
		if _, err := os.Stat(admission.DefaultDir + "/" + entryB); err != nil {
			t.Errorf("login B's entry is gone, so its disk reservation is not held: %v", err)
		}
		if !confirm(t, "Does login A's runner drawer show disk held for another person's run?") {
			t.Error("the drawer does not show the held disk")
		}
	})
	t.Run("phase6_return", func(t *testing.T) {
		if entryB == "" {
			notRun(t, "phase 5 did not tombstone login B's entry")
		}
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
