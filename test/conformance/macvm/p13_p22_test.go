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

// stillWorks is P13 and P14's "as before": the run is live, exec works and a flow through the
// proxy recovers.
func stillWorks(t *testing.T, id string, helperPID int) {
	t.Helper()
	if !alive(helperPID) {
		t.Fatalf("helper %d of run %s did not survive", helperPID, id)
	}
	deadline := time.Now().Add(2 * time.Minute)
	for helperStatus(t, id).HelperPID != helperPID {
		if time.Now().After(deadline) {
			t.Fatal("the helper answering ctl.sock is not the one that was running")
		}
		time.Sleep(2 * time.Second)
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
	id := start(t, nil)
	pid := helperStatus(t, id).HelperPID
	mustOK(t, sh(t, 2*time.Minute, "launchctl", "kickstart", "-k", fmt.Sprintf("gui/%d/%s", os.Getuid(), assumedRunnerdLabel)), "kickstart runnerd")
	time.Sleep(10 * time.Second)
	stillWorks(t, id, pid)
}

// inode is the helper file's inode, for P14's "the file was replaced".
func inode(t *testing.T, path string) uint64 {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Sys().(*syscall.Stat_t).Ino
}

// TestP14_PkgUpgrade: the same pkg re-versioned, installed with VMs live. The install is
// the person's: it takes the admin password, which never reaches the suite.
func TestP14_PkgUpgrade(t *testing.T) {
	live(t)
	need(t, cfg.upgradePkg, "WARDYN_MACVM_UPGRADE_PKG (the same pkg, re-versioned)")
	needPerson(t, "the automated suite's run (the install needs the admin password)")
	bin, err := exec.LookPath(cfg.helper)
	if err != nil {
		t.Fatal(err)
	}
	id := start(t, nil)
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
	t.Log("RECORD manual-only: a mismatched control protocol ending a run, and an older base guest agent starting a tools update, need builds made for them")
}

// TestP15_HostSleep is reserved for the MacBook field test.
func TestP15_HostSleep(t *testing.T) {
	t.Skip("reserved for the MacBook field test; not run on the rental (a Mac mini has no lid or battery)")
}

// TestP16_WedgedGuest: SIGSTOP the guest agent; the run ends within 60 s plus the stop path, and
// its ledger entry is released only after the VM is verifiably gone.
func TestP16_WedgedGuest(t *testing.T) {
	live(t)
	id := start(t, nil)
	pid := helperStatus(t, id).HelperPID
	keep(t, "vz-procs.txt", []byte(sh(t, 30*time.Second, "sh", "-c", "ps -axo pid=,uid=,comm= | grep -i virtualization").out))
	mustGuest(t, id, "sudo -n sh -c 'nohup sh -c \"sleep 3; kill -STOP $(pgrep -x "+assumedGuestAgent+")\" >/dev/null 2>&1 &'")
	t0 := time.Now()
	for !get(t, id).terminal() {
		if _, ok := entryFor(t, id); !ok && (alive(pid) || procs(t, "com.apple.Virtualization.VirtualMachine", os.Getuid()) > 0) {
			t.Fatal("the ledger entry was released while the helper or its Virtualization process was still alive")
		}
		if time.Since(t0) > 60*time.Second+3*time.Minute {
			t.Fatal("the wedged run did not end")
		}
		time.Sleep(500 * time.Millisecond)
	}
	record(t, "wedged_to_ended", time.Since(t0))
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

func sample(t *testing.T, path string) *sampler {
	t.Helper()
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

// filler takes n bytes of host disk with mkfile, removed at the end of the test.
func filler(t *testing.T, name string, n int64) {
	t.Helper()
	if n <= 0 {
		return
	}
	p := filepath.Join(filepath.Dir(layout(t).Root), "macvm-live-"+name)
	mustOK(t, sh(t, 30*time.Minute, "mkfile", "-n", strconv.FormatInt(n/(1<<20), 10)+"m", p), "mkfile "+name)
	// -n makes a sparse file; writing it makes the space real.
	mustOK(t, sh(t, 60*time.Minute, "dd", "if=/dev/zero", "of="+p, "bs=1m", "count="+strconv.FormatInt(n/(1<<20), 10), "conv=notrunc"), "fill "+name)
	t.Cleanup(func() { _ = os.Remove(p) })
}

// TestP17_DiskBound: (a) a guest filling its disk keeps host free above the floor; (b) a second
// run is refused for disk; (c) another process pushing free below 5 GiB ends the newest run;
// (d) retained snapshot versions.
func TestP17_DiskBound(t *testing.T) {
	live(t)
	vol := layout(t).Root
	reserve := int64(cfg.runDiskGiB)*gib + admission.DiskExtraBytes
	t.Run("a_b_c", func(t *testing.T) {
		filler(t, "a", freeBytes(t, vol)-(reserve+admission.DiskFloorBytes+gib))
		id := start(t, nil)
		s := sample(t, vol)
		guest(t, id, "dd if=/dev/zero of=$HOME/p17-fill bs=1m; sync", 2*time.Hour)
		low := s.end(t, "a-free.csv")
		record(t, "a_min_free_gib", float64(low)/gib)
		if low < admission.DiskFloorBytes {
			t.Errorf("(a) host free fell to %d bytes, below the 5 GiB floor", low)
		}
		r := askRefused(t, nil)
		t.Logf("RECORD b refused_by=%s reason=%q", r.By, r.Reason)
		if strings.HasPrefix(r.By, "none") || !msg["disk"].MatchString(r.Reason) {
			t.Errorf("(b) the second run was not refused with the disk message: %+v", r)
		}
		t0 := time.Now()
		filler(t, "c", freeBytes(t, vol)-4*gib)
		for !get(t, id).terminal() && time.Since(t0) < 5*time.Minute {
			time.Sleep(time.Second)
		}
		record(t, "c_ended_after", time.Since(t0))
		record(t, "c_end_reason", get(t, id).FailureHint)
		if !get(t, id).terminal() || time.Since(t0) > 30*time.Second+45*time.Second {
			t.Errorf("LIMIT (c): the newest run did not end within 30 s plus the stop time (best effort)")
		}
	})
	endAll(t)
	t.Run("d_snapshot_versions", func(t *testing.T) {
		id := start(t, nil)
		s := sample(t, vol)
		mustOK(t, guest(t, id, "dd if=/dev/urandom of=$HOME/p17-random bs=1m count=10240 && sync", time.Hour), "write 10 GiB")
		snap := mustOK(t, sh(t, 5*time.Minute, "tmutil", "localsnapshot"), "tmutil localsnapshot")
		date := ""
		if f := strings.Fields(snap); len(f) > 0 {
			date = f[len(f)-1]
		}
		record(t, "snapshot", strings.TrimSpace(snap))
		filler(t, "d", freeBytes(t, vol)-(reserve+admission.DiskFloorBytes+5*gib))
		f0 := freeBytes(t, vol)
		mustOK(t, guest(t, id, "dd if=/dev/urandom of=$HOME/p17-random bs=1m count=10240 conv=notrunc && sync", time.Hour), "rewrite 10 GiB")
		f1 := freeBytes(t, vol)
		record(t, "d_rewrite_extra_gib", float64(f0-f1)/gib)
		if f1 >= reserve+admission.DiskFloorBytes {
			t.Errorf("(d) NOT RUN: the rewrite left %d bytes free, so the snapshot held no extra space and the refusal cannot be witnessed", f1)
		} else {
			r := askRefused(t, nil)
			t.Logf("RECORD d refused_by=%s reason=%q", r.By, r.Reason)
			if strings.HasPrefix(r.By, "none") {
				t.Errorf("(d) the second run was admitted although live free space no longer fits its reservation")
			}
		}
		end(t, id)
		f2 := freeBytes(t, vol)
		record(t, "d_held_after_end_gib", float64(f0-f2)/gib)
		del := sh(t, 10*time.Minute, "tmutil", "deletelocalsnapshots", date)
		if del.code != 0 && cfg.prompts {
			doStep(t, "Run: sudo tmutil deletelocalsnapshots "+date)
		} else if del.code != 0 {
			t.Errorf("the window's snapshot %s is still there: delete it by hand (sudo tmutil deletelocalsnapshots %s)", date, date)
		}
		time.Sleep(time.Minute)
		record(t, "d_back_after_delete_gib", float64(freeBytes(t, vol)-f2)/gib)
		s.end(t, "d-free.csv")
		keep(t, "d-df.txt", []byte(sh(t, time.Minute, "df", "-k", vol).out))
	})
}

// TestP18_ThermalAndPower: the helper holds an idle-sleep assertion while a VM runs, and only then.
func TestP18_ThermalAndPower(t *testing.T) {
	live(t)
	held := func() bool {
		out := sh(t, time.Minute, "pmset", "-g", "assertions").out
		keep(t, fmt.Sprintf("assertions-%d.txt", time.Now().Unix()), []byte(out))
		for _, l := range strings.Split(out, "\n") {
			if strings.Contains(l, "wardyn-macvm") && strings.Contains(l, "IdleSleep") {
				return true
			}
		}
		return false
	}
	id := start(t, nil)
	if !held() {
		t.Error("no idle-sleep assertion from the helper while a VM runs")
	}
	st := runnerdStatus(t)
	record(t, "thermal", st["thermal"])
	end(t, id)
	waitCounted(t, 0, 2*time.Minute)
	if held() {
		t.Error("the helper's idle-sleep assertion is still held after the VM stopped")
	}
	if cfg.prompts {
		record(t, "drawer_thermal", ask(t, "What thermal state does the runner drawer show?"))
	}
}

// TestP19_TeardownHygiene: the orphan sweep after kill -9 of a helper, then nothing left. Run it
// last, after P12's logout and return.
func TestP19_TeardownHygiene(t *testing.T) {
	live(t)
	t.Run("kill9_helper", func(t *testing.T) {
		id := start(t, nil)
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
		if out := strings.TrimSpace(sh(t, time.Minute, "docker", "ps", "-aq", "--filter", "name=wardyn-vmnet-").out); out != "" {
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
	needPerson(t, "after setup (posture, four-eyes and a member login are a person's steps)")
	refused := func(t *testing.T, re string) {
		r := askRefused(t, nil)
		t.Logf("RECORD refused_by=%s reason=%q", r.By, r.Reason)
		if strings.HasPrefix(r.By, "none") || !msg[re].MatchString(r.Reason) {
			t.Errorf("not refused with the %s message: %+v", re, r)
		}
	}
	t.Run("posture_missing", func(t *testing.T) {
		doStep(t, "Move /Library/Application Support/Wardyn/posture.json aside (sudo mv ... posture.json.off), and wait 30 s for the runner to report.")
		refused(t, "posture")
		doStep(t, "Move posture.json back.")
	})
	t.Run("personal", func(t *testing.T) {
		mustOK(t, wardyn(t, "runner", "trust", cfg.runner, "personal"), "runner trust personal")
		refused(t, "managed")
		doStep(t, "Mark the runner managed_device again through four-eyes with the second test admin.")
	})
	t.Run("member", func(t *testing.T) {
		record(t, "member_posture", ask(t, "As a member, with posture.json moved aside: paste the refusal the member sees (it should name the role and the next step):"))
		record(t, "member_personal", ask(t, "As a member, with the runner set to personal: paste the refusal the member sees:"))
	})
	t.Run("evidence_wording", func(t *testing.T) {
		id := start(t, nil)
		if !confirm(t, fmt.Sprintf("Does run %s's page show its evidence with the \"admin-asserted\" wording?", id)) {
			t.Error("the run page's evidence lacks the admin-asserted wording")
		}
	})
}

// TestP21_GuestClock: skew sampled every 5 minutes for an hour, against the host's midpoint.
func TestP21_GuestClock(t *testing.T) {
	live(t)
	id := start(t, nil)
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
		mid := float64(before.UnixNano()+after.UnixNano()) / 2e9
		skew := g - mid
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

// TestP22_DistinctIdentities: two concurrent guests per strategy, judged by one rule: only
// distinct configured identifiers qualify two concurrent VMs, so travel (one identifier per base) is
// capped at one VM per slot base. P4's tracking check is kept as evidence, never as qualification.
func TestP22_DistinctIdentities(t *testing.T) {
	live(t)
	var p4 p4Result
	if b, err := os.ReadFile(p4Path()); err == nil {
		_ = json.Unmarshal(b, &p4)
		t.Logf("RECORD p4_tracking_check_holds=%v (%s); evidence only", p4.Tracking, p4.Why)
	} else {
		t.Logf("RECORD p4_result=missing (%v): both strategies are tried", err)
	}
	verdict := map[macvm.IdentityStrategy]bool{}
	for _, s := range []macvm.IdentityStrategy{macvm.IdentityFresh, macvm.IdentityTravel} {
		t.Run(string(s), func(t *testing.T) {
			if p4.Booted != nil && !p4.Booted[s] {
				t.Skipf("NOT RUN: %s did not boot in P4", s)
			}
			if s == macvm.IdentityTravel && cfg.slotBases < 2 {
				t.Skip("NOT RUN: travel is capped at one VM per slot base and this Mac has one (WARDYN_MACVM_SLOT_BASES); it does not qualify for two")
			}
			setIdentity(t, s)
			var ids [2]string
			for i := range ids {
				ids[i] = start(t, nil)
			}
			var hashes [2]string
			var guests [2]guestIdentity
			for i, id := range ids {
				st := helperStatus(t, id)
				checkIdentity(t, st, s)
				hashes[i], guests[i] = st.MachineIDSHA256, readGuestIdentity(t, id)
				t.Logf("RECORD run=%s strategy=%s machine_id_sha256=%s guest_serial=%s guest_uuid=%s", id, s, hashes[i], guests[i].Serial, guests[i].UUID)
				keepHelperLog(t, id)
			}
			ok, why := qualifies(hashes, guests)
			verdict[s] = ok
			t.Logf("RECORD %s qualifies=%v (%s)", s, ok, why)
			for _, id := range ids {
				end(t, id)
			}
		})
	}
	switch {
	case verdict[macvm.IdentityFresh]:
		t.Log("RECORD decision=keep fresh (distinct configured identifiers by construction)")
	case verdict[macvm.IdentityTravel]:
		t.Log("RECORD decision=keep travel on per-slot bases")
	default:
		t.Error("DECIDE: neither strategy qualifies for two concurrent VMs: run with a host cap of 1; the per-slot-base fallback restores 2 on a second window")
	}
}
