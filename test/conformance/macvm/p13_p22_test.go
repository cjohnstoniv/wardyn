// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build macvm_live

package macvmlive

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
	"github.com/cjohnstoniv/wardyn/internal/macvm/admission"
)

func alive(pid int) bool { return pid > 0 && syscall.Kill(pid, 0) == nil }

// entryFor is the ledger entry of run id, if any.
func entryFor(t *testing.T, id string) (ledgerFile, bool) {
	t.Helper()
	for _, f := range ledger(t) {
		if !f.Tomb && f.Entry.RunID == id {
			return f, true
		}
	}
	return ledgerFile{}, false
}

// stillWorks is P13 and P14's "as before": the same helper serves the run, the org still has it
// RUNNING, exec works and a flow through the proxy recovers.
func stillWorks(t *testing.T, id string, helperPID int) {
	t.Helper()
	if !alive(helperPID) {
		t.Fatalf("helper %d of run %s did not survive", helperPID, id)
	}
	if got := helperStatus(t, id).HelperPID; got != helperPID {
		t.Fatalf("ctl.sock is served by helper %d, not the one that was running (%d)", got, helperPID)
	}
	if v := get(t, id); v.State != "RUNNING" {
		t.Fatalf("run %s is %s: runnerd did not adopt it (%s)", id, v.State, v.FailureHint)
	}
	mustGuest(t, id, "true")
	if cfg.allowedURL != "" {
		deadline := time.Now().Add(2 * time.Minute)
		for guest(t, id, "curl -sS -o /dev/null --max-time 20 '"+cfg.allowedURL+"'", time.Minute).code != 0 {
			if time.Now().After(deadline) {
				t.Fatal("no flow through the proxy recovered within 2 minutes")
			}
			time.Sleep(5 * time.Second)
		}
	}
	if cfg.prompts && !confirm(t, fmt.Sprintf("Open run %s's desktop: does it still work?", id)) {
		t.Error("the desktop does not work after the restart")
	}
}

// TestP13_RunnerRestart: runnerd restarts with VMs live; the helpers survive and are adopted.
func TestP13_RunnerRestart(t *testing.T) {
	live(t)
	id := start(t, spec{})
	pid := helperStatus(t, id).HelperPID
	mustOK(t, sh(t, 2*time.Minute, "launchctl", "kickstart", "-k", fmt.Sprintf("gui/%d/%s", os.Getuid(), assumedRunnerdLabel)), "kickstart runnerd")
	time.Sleep(10 * time.Second)
	stillWorks(t, id, pid)
}

// inode is a file's inode, for P14's "the file was replaced".
func inode(t *testing.T, path string) uint64 {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Sys().(*syscall.Stat_t).Ino
}

// TestP14_PkgUpgrade: the same pkg re-versioned, installed with VMs live. The install is the
// person's: it takes the admin password, which never reaches the suite.
func TestP14_PkgUpgrade(t *testing.T) {
	live(t)
	need(t, cfg.upgradePkg, "WARDYN_MACVM_UPGRADE_PKG (the same pkg, re-versioned)")
	needPerson(t, "the automated suite's run (the install needs the admin password)")
	bin, err := exec.LookPath(env("WARDYN_MACVM_HELPER", "wardyn-macvm"))
	if err != nil {
		t.Fatal(err)
	}
	id := start(t, spec{})
	pid := helperStatus(t, id).HelperPID
	before, since := inode(t, bin), time.Now()
	doStep(t, "Run: sudo installer -pkg '"+cfg.upgradePkg+"' -target /")
	if inode(t, bin) == before {
		t.Errorf("%s kept inode %d: the installer rewrote it in place", bin, before)
	}
	stillWorks(t, id, pid)
	crash := sh(t, 5*time.Minute, "log", "show", "--start", since.Format("2006-01-02 15:04:05"), "--predicate", `eventMessage CONTAINS "Code Signature Invalid"`)
	keep(t, "codesign-log.txt", []byte(crash.out))
	reports, _ := filepath.Glob(filepath.Join(os.Getenv("HOME"), "Library/Logs/DiagnosticReports", "wardyn-macvm*"))
	for _, r := range reports {
		if fi, err := os.Stat(r); err == nil && fi.ModTime().After(since) {
			t.Errorf("a helper crashed after the upgrade: %s", r)
		}
	}
	if strings.Contains(crash.out, "wardyn-macvm") {
		t.Error("\"Code Signature Invalid\" names the helper after the upgrade")
	}
	t.Run("protocol_mismatch_and_tools_update", func(t *testing.T) {
		notRun(t, "a mismatched control protocol ending a run, and an older base guest agent starting a tools update, need builds made for them")
	})
}

// TestP15_HostSleep is reserved for the MacBook field test.
func TestP15_HostSleep(t *testing.T) {
	notRun(t, "reserved for the MacBook field test; not run on the rental (a Mac mini has no lid or battery)")
}

// TestP16_WedgedGuest: SIGSTOP the guest agent; the run ends within 60 s plus the stop path, and
// its ledger entry is released only after the helper and its VM process are gone.
func TestP16_WedgedGuest(t *testing.T) {
	live(t)
	block(t)
	id := start(t, spec{})
	pid := helperStatus(t, id).HelperPID
	keep(t, "vz-procs.txt", []byte(sh(t, 30*time.Second, "sh", "-c", "ps -axo pid=,uid=,comm= | grep -i virtualization").out))
	r := guest(t, id, "pid=$(pgrep -x "+assumedGuestAgent+") || exit 3; kill -0 $pid 2>/dev/null || exit 4; nohup sh -c \"sleep 3; kill -STOP $pid\" >/dev/null 2>&1 &", time.Minute)
	switch r.code {
	case 0:
	case 3:
		t.Fatalf("no %s process in the guest", assumedGuestAgent)
	case 4:
		notRun(t, "exec runs as the agent user, which may not signal the guest agent (kill -0 refused), and the base has no sudoers rule")
	default:
		t.Fatalf("the exec failed: exit %d %s", r.code, r.err)
	}
	t0 := time.Now()
	gone := func() bool {
		return !alive(pid) && procs(t, "com.apple.Virtualization.VirtualMachine", os.Getuid()) == 0
	}
	// Watch until the run is terminal AND the helper and VM process are gone: the entry must hold
	// until then, however long the stop takes.
	for !get(t, id).terminal() || !gone() {
		if _, ok := entryFor(t, id); !ok && !gone() {
			t.Fatal("the ledger entry was released while the helper or its Virtualization process was still alive")
		}
		if time.Since(t0) > 5*time.Minute {
			t.Fatal("the wedged run, or its helper and VM process, did not end within 5 minutes")
		}
		time.Sleep(500 * time.Millisecond)
	}
	record(t, "wedged_to_gone", time.Since(t0))
	record(t, "end_reason", get(t, id).FailureHint)
	if d := time.Since(t0); d > 60*time.Second+45*time.Second {
		t.Errorf("the run ended %s after the agent stopped; budget 60s plus requestStop, stop and kill", d)
	}
	waitCounted(t, 0, 2*time.Minute)
}

// sampler records the volume's free space every second until stopped.
type sampler struct {
	mu   sync.Mutex
	min  int64
	rows []string
	stop chan struct{}
	done chan struct{}
}

func sample(path string) *sampler {
	s := &sampler{min: 1 << 62, stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(s.done)
		for {
			var st syscall.Statfs_t
			if syscall.Statfs(path, &st) == nil {
				f := int64(st.Bavail) * int64(st.Bsize)
				s.mu.Lock()
				s.min = min(s.min, f)
				s.rows = append(s.rows, fmt.Sprintf("%s,%d", time.Now().UTC().Format(time.RFC3339), f))
				s.mu.Unlock()
			}
			select {
			case <-s.stop:
				return
			case <-time.After(time.Second):
			}
		}
	}()
	return s
}

func (s *sampler) end(t *testing.T, name string) int64 {
	t.Helper()
	close(s.stop)
	<-s.done
	keep(t, name, []byte("time,free_bytes\n"+strings.Join(s.rows, "\n")+"\n"))
	return s.min
}

// filler takes n bytes of host disk inside Wardyn's own .noindex dir (so Spotlight and Time
// Machine skip it), removed when owner ends; the removal is registered before anything is written.
func filler(t, owner *testing.T, name string, n int64) {
	t.Helper()
	if n <= 0 {
		return
	}
	p := filepath.Join(layout(t).Root, "macvm-live-filler-"+name)
	owner.Cleanup(func() { _ = os.Remove(p) })
	mib := strconv.FormatInt(n/(1<<20), 10)
	mustOK(t, sh(t, 60*time.Minute, "dd", "if=/dev/zero", "of="+p, "bs=1m", "count="+mib), "fill "+name)
}

// TestP17_DiskBound: (a) a guest filling its disk keeps host free above the floor; (b) a second
// run is refused for disk; (c) another process pushing free below 5 GiB ends the newest run;
// (d) retained snapshot versions. (a), (b) and (d)'s refusal block; (c) and (d)'s measurements
// are limits.
func TestP17_DiskBound(t *testing.T) {
	live(t)
	vol := layout(t).Root
	reserve := int64(cfg.runDiskGiB)*gib + admission.DiskExtraBytes
	parent := t
	var id string
	t.Run("a_floor", func(t *testing.T) {
		block(t)
		filler(t, parent, "a", freeBytes(t, vol)-(reserve+admission.DiskFloorBytes+gib))
		id = start(t, spec{owner: parent})
		s := sample(vol)
		r := guest(t, id, "dd if=/dev/zero of=$HOME/p17-fill bs=1m; sync; df -k /", 2*time.Hour)
		low := s.end(t, "a-free.csv")
		keep(t, "a-guest.txt", []byte(r.out+r.err))
		if !strings.Contains(r.out+r.err, "No space left on device") {
			notRun(t, "the guest's disk did not fill (no \"No space left on device\" from dd), so the floor was not pressed")
		}
		record(t, "a_min_free_gib", float64(low)/gib)
		if low < admission.DiskFloorBytes {
			t.Errorf("host free fell to %d bytes, below the 5 GiB floor", low)
		}
	})
	t.Run("b_second_refused", func(t *testing.T) {
		block(t)
		if id == "" {
			notRun(t, "(a) did not start the first run")
		}
		f := preconditions(t, "p17b", cfg.baseMinMemGiB)
		r := askRefused(t, spec{})
		if r.By == "placement" && f.Disk >= 1 {
			t.Logf("RECORD b refused_by=%s reason=%q", r.By, r.Reason)
			notRun(t, "placement's refusal cannot be put down to disk: the preconditions say one more run's disk fits")
		}
		if r.By == "placement" {
			mustRefuse(t, r, "placement", msg["noRoom"])
		} else {
			mustRefuse(t, r, "runner", msg["ledgerDisk"])
		}
	})
	t.Run("c_newest_ends", func(t *testing.T) {
		if id == "" {
			notRun(t, "(a) did not start the first run")
		}
		t0 := time.Now()
		filler(t, parent, "c", freeBytes(t, vol)-4*gib)
		for !get(t, id).terminal() && time.Since(t0) < 5*time.Minute {
			time.Sleep(time.Second)
		}
		record(t, "c_ended_after", time.Since(t0))
		record(t, "c_end_reason", get(t, id).FailureHint)
		if !get(t, id).terminal() || time.Since(t0) > 30*time.Second+45*time.Second {
			t.Errorf("LIMIT (c): the newest run did not end within 30 s plus the stop time (best effort)")
		}
	})
	if id != "" {
		end(t, id)
	}
	for _, n := range []string{"a", "c"} {
		_ = os.Remove(filepath.Join(vol, "macvm-live-filler-"+n))
	}
	endAll(t)
	t.Run("d_snapshot_versions", func(t *testing.T) {
		block(t)
		id := start(t, spec{})
		s := sample(vol)
		defer s.end(t, "d-free.csv")
		mustOK(t, guest(t, id, "dd if=/dev/urandom of=$HOME/p17-random bs=1m count=10240 && sync", time.Hour), "write 10 GiB")
		date := takeSnapshot(t)
		deleted := false
		defer func() {
			if !deleted {
				deleteSnapshot(t, date)
			}
		}()
		record(t, "snapshot", date)
		filler(t, t, "d", freeBytes(t, vol)-(reserve+admission.DiskFloorBytes+5*gib))
		f0 := freeBytes(t, vol)
		mustOK(t, guest(t, id, "dd if=/dev/urandom of=$HOME/p17-random bs=1m count=10240 conv=notrunc && sync", time.Hour), "rewrite 10 GiB")
		f1 := freeBytes(t, vol)
		record(t, "d_rewrite_extra_gib", float64(f0-f1)/gib)
		if f1 >= reserve+admission.DiskFloorBytes {
			notRun(t, "the rewrite left %d bytes free, so the snapshot held no extra space and the refusal cannot be witnessed", f1)
		}
		f := preconditions(t, "p17d", cfg.baseMinMemGiB)
		r := askRefused(t, spec{})
		if r.By == "placement" && f.Disk >= 1 {
			t.Logf("RECORD d refused_by=%s reason=%q", r.By, r.Reason)
			notRun(t, "placement's refusal cannot be put down to disk: the preconditions say one more run's disk fits")
		}
		if r.By == "placement" {
			mustRefuse(t, r, "placement", msg["noRoom"])
		} else {
			mustRefuse(t, r, "runner", msg["ledgerDisk"])
		}
		end(t, id)
		f2 := freeBytes(t, vol)
		record(t, "d_held_after_end_gib", float64(f0-f2)/gib)
		deleteSnapshot(t, date)
		deleted = true
		time.Sleep(time.Minute)
		record(t, "d_back_after_delete_gib", float64(freeBytes(t, vol)-f2)/gib)
		keep(t, "d-df.txt", []byte(sh(t, time.Minute, "df", "-k", vol).out))
	})
}

// caffeinate is the helper's idle-sleep holder (macvm.IdleSleepCommand): its pid if one waits on
// helperPID.
func caffeinate(t *testing.T, helperPID int) int {
	t.Helper()
	want := strings.Join(macvm.IdleSleepCommand(helperPID), " ")
	for _, l := range strings.Split(sh(t, 30*time.Second, "ps", "-axo", "pid=,args=").out, "\n") {
		f := strings.Fields(l)
		if len(f) > 1 && strings.Join(f[1:], " ") == want {
			pid, _ := strconv.Atoi(f[0])
			return pid
		}
	}
	return 0
}

// asserted reports whether pmset lists a PreventUserIdleSystemSleep assertion held by pid.
func asserted(t *testing.T, pid int) bool {
	t.Helper()
	out := sh(t, time.Minute, "pmset", "-g", "assertions").out
	keep(t, fmt.Sprintf("assertions-%d.txt", time.Now().UnixNano()), []byte(out))
	re := regexp.MustCompile(`pid ` + strconv.Itoa(pid) + `\(caffeinate\):.*PreventUserIdleSystemSleep`)
	return pid > 0 && re.MatchString(out)
}

// TestP18_ThermalAndPower: the helper holds an idle-sleep assertion while a VM runs, and only then.
func TestP18_ThermalAndPower(t *testing.T) {
	live(t)
	id := start(t, spec{})
	hp := helperStatus(t, id).HelperPID
	cp := caffeinate(t, hp)
	if cp == 0 || !asserted(t, cp) {
		t.Errorf("no idle-sleep assertion from %q while a VM runs (caffeinate pid %d)", strings.Join(macvm.IdleSleepCommand(hp), " "), cp)
	}
	record(t, "thermal", runnerdStatus(t)["thermal"])
	end(t, id)
	waitCounted(t, 0, 2*time.Minute)
	if caffeinate(t, hp) != 0 || cp != 0 && asserted(t, cp) {
		t.Error("the helper's idle-sleep assertion is still held after the VM stopped")
	}
	t.Run("drawer", func(t *testing.T) {
		if !cfg.prompts {
			notRun(t, "the drawer's thermal state is a person's reading (WARDYN_MACVM_PROMPTS=1)")
		}
		record(t, "drawer_thermal", ask(t, "What thermal state does the runner drawer show?"))
	})
}

// TestP19_TeardownHygiene: the orphan sweep after kill -9 of a helper, then nothing left. run.sh
// runs it last, after P12's logout and return.
func TestP19_TeardownHygiene(t *testing.T) {
	live(t)
	block(t)
	t.Run("kill9_helper", func(t *testing.T) {
		id := start(t, spec{})
		pid := helperStatus(t, id).HelperPID
		if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(5 * time.Minute)
		for {
			_, entry := entryFor(t, id)
			_, err := os.Stat(runDir(t, id))
			if get(t, id).terminal() && !entry && os.IsNotExist(err) {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("5 minutes after kill -9 of helper %d: run ended=%v, entry=%v, run dir=%v", pid, get(t, id).terminal(), entry, err)
			}
			time.Sleep(2 * time.Second)
		}
		record(t, "end_reason", get(t, id).FailureHint)
	})
	t.Run("nothing_left", func(t *testing.T) {
		if n := procs(t, admission.HelperProcess, -1); n > 0 {
			t.Errorf("%d helper processes remain", n)
		}
		des, _ := os.ReadDir(layout(t).RunsDir())
		for _, d := range des {
			t.Errorf("run dir remains: %s", d.Name())
		}
		if out := strings.TrimSpace(mustOK(t, sh(t, time.Minute, "docker", "ps", "-aq", "--filter", "name=wardyn-vmnet-"), "docker ps")); out != "" {
			t.Errorf("vmnet containers remain: %s", out)
		}
		for _, f := range ledger(t) {
			t.Errorf("ledger file remains: %s (uid %d)", f.Name, f.UID)
		}
		// Earlier-boot junk the design says is never cleared is listed, not failed.
		junk, _ := filepath.Glob(filepath.Join(layout(t).Root, "*"))
		keepJSON(t, "root-listing.json", junk)
	})
}

// TestP20_ManagedGate: each refusal of the managed gate, as an admin and as a member. The posture
// file is root-owned and the trust change back is four-eyes, so a person does those steps.
func TestP20_ManagedGate(t *testing.T) {
	live(t)
	block(t)
	needPerson(t, "after setup (posture, four-eyes and a member login are a person's steps)")
	t.Run("posture_missing", func(t *testing.T) {
		doStep(t, "Move /Library/Application Support/Wardyn/posture.json aside (sudo mv ... posture.json.off), and wait 30 s for the runner to report.")
		r := askRefused(t, spec{})
		member := ask(t, "As a member, ask for a macOS VM run on this runner now: paste the refusal the member sees:")
		doStep(t, "Move posture.json back.")
		mustRefuse(t, r, "placement", msg["posture"])
		if !msg["posture"].MatchString(member) {
			t.Errorf("the member's refusal is not the posture message: %q", member)
		}
	})
	t.Run("personal", func(t *testing.T) {
		mustOK(t, wardyn(t, "runner", "trust", runnerID(t), "personal"), "runner trust personal")
		r := askRefused(t, spec{})
		member := ask(t, "As a member, ask for a macOS VM run on this runner now: paste the refusal the member sees:")
		doStep(t, "Mark the runner managed_device again through four-eyes with the second test admin.")
		mustRefuse(t, r, "placement", msg["managedOp"])
		if !msg["managedMem"].MatchString(member) {
			t.Errorf("the member's refusal does not name the role and the next step: %q", member)
		}
		if tc := runnerRow(t).TrustClass; tc != "managed_device" {
			t.Errorf("the runner is %q after the four-eyes step, not managed_device", tc)
		}
	})
	t.Run("evidence_wording", func(t *testing.T) {
		id := start(t, spec{})
		if !confirm(t, fmt.Sprintf("Does run %s's page show its evidence with the \"admin-asserted\" wording?", id)) {
			t.Error("the run page's evidence lacks the admin-asserted wording")
		}
	})
}

// TestP21_GuestClock: skew sampled every 5 minutes for an hour, against the host's midpoint.
func TestP21_GuestClock(t *testing.T) {
	live(t)
	id := start(t, spec{})
	var worst float64
	var rows []string
	for t0 := time.Now(); ; {
		before := time.Now()
		out := strings.TrimSpace(mustGuest(t, id, `perl -MTime::HiRes=time -e 'printf "%.3f\n", time'`))
		after := time.Now()
		g, err := strconv.ParseFloat(out, 64)
		if err != nil {
			t.Fatalf("guest time %q: %v", out, err)
		}
		skew := g - float64(before.UnixNano()+after.UnixNano())/2e9
		// The exec's round trip bounds what can be known about the skew.
		rtt := after.Sub(before).Seconds()
		rows = append(rows, fmt.Sprintf("%s,%.3f,%.3f", before.UTC().Format(time.RFC3339), skew, rtt))
		worst = max(worst, max(skew, -skew)-rtt/2)
		if time.Since(t0)+cfg.clockEvery > cfg.clockFor+time.Second {
			break
		}
		time.Sleep(cfg.clockEvery)
	}
	keep(t, "skew.csv", []byte("time,skew_s,exec_rtt_s\n"+strings.Join(rows, "\n")+"\n"))
	record(t, "worst_skew_s", fmt.Sprintf("%.3f", worst))
	if worst > 2 {
		t.Errorf("LIMIT: the guest clock was %.3f s off, beyond 2 s", worst)
	}
}

// TestP22_DistinctIdentities: two concurrent guests per strategy. Only distinct configured
// identifiers qualify two concurrent VMs, so travel (one identifier per base) is capped at one VM
// per slot base. P4's tracking check is kept as evidence, never as qualification.
func TestP22_DistinctIdentities(t *testing.T) {
	live(t)
	var p4 p4Result
	if b, err := os.ReadFile(p4Path()); err == nil {
		_ = json.Unmarshal(b, &p4)
		t.Logf("RECORD p4_tracking_check_holds=%v (%s); evidence only", p4.Tracking, p4.Why)
	} else {
		t.Logf("RECORD p4_result=missing (%v)", err)
	}
	verdict := map[macvm.IdentityStrategy]bool{}
	for _, s := range []macvm.IdentityStrategy{macvm.IdentityFresh, macvm.IdentityTravel} {
		t.Run(string(s), func(t *testing.T) {
			if booted, ok := p4.Booted[s]; !ok {
				notRun(t, "P4 has no %s series (%s)", s, p4Path())
			} else if !booted {
				notRun(t, "%s did not boot in P4", s)
			}
			if s == macvm.IdentityTravel && cfg.slotBases < 2 {
				notRun(t, "travel is capped at one VM per slot base and this Mac has one (WARDYN_MACVM_SLOT_BASES): it cannot qualify for two")
			}
			setIdentity(t, s)
			var ids [2]string
			for i := range ids {
				ids[i] = start(t, spec{})
			}
			var hashes [2]string
			var guests [2]guestIdentity
			for i, id := range ids {
				st := helperStatus(t, id)
				checkIdentity(t, st, s)
				hashes[i], guests[i] = st.MachineIDSHA256, readGuestIdentity(t, id)
				t.Logf("RECORD run=%s identity=%s machine_id_sha256=%s guest_serial=%s guest_uuid=%s", id, st.Identity, hashes[i], guests[i].Serial, guests[i].UUID)
				keepHelperLog(t, id)
			}
			ok, why := qualifies(hashes, guests)
			verdict[s] = ok
			t.Logf("RECORD %s qualifies=%v (%s)", s, ok, why)
		})
	}
	t.Run("two_concurrent", func(t *testing.T) {
		block(t)
		switch {
		case verdict[macvm.IdentityFresh]:
			t.Log("RECORD decision=keep fresh (distinct configured identifiers by construction)")
		case verdict[macvm.IdentityTravel]:
			t.Log("RECORD decision=keep travel on per-slot bases")
		case len(verdict) == 0:
			notRun(t, "no strategy was tried for two concurrent VMs")
		default:
			t.Error("neither strategy qualifies for two concurrent VMs: run with a host cap of 1; the per-slot-base fallback restores 2 on a second window")
		}
	})
}
