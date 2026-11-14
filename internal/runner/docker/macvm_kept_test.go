// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker && unix

package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sys/unix"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
	"github.com/cjohnstoniv/wardyn/internal/macvm/admission"
	"github.com/cjohnstoniv/wardyn/internal/macvm/macvmtest"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/vm/guestagent"
)

const keptOwnerHash = "owner-hash"

// keptGuest is the kept rig's guest: it counts stop scrubs and answers a boot scrub in Init, as the
// macOS guest agent does.
type keptGuest struct {
	mu     sync.Mutex
	scrubs int
	spawns int
	cancel context.CancelFunc
}

// newKeptRig is newMacRig with kept machines on (110 GB) and a guest that scrubs.
func newKeptRig(t *testing.T) (*macRig, *keptGuest) {
	t.Helper()
	r := newMacRig(t)
	r.c.LimitBytes = 110 << 30
	r.c.lockWait = 100 * time.Millisecond // a held lock is refused at once; TestKeptStopWaitsForABriefHolder waits
	g := &keptGuest{}
	r.c.spawn = func(dir string) error {
		// wardyn-macvm makes each run's desktop password and wipes it when the run ends, so every helper
		// here gets its own: a machine's second session must not be handed the first one's wiped secret.
		secret, err := macvm.NewDesktopSecret()
		if err != nil {
			return err
		}
		ctx, cancel := context.WithCancel(context.Background())
		g.mu.Lock()
		g.spawns++
		g.cancel = cancel
		g.mu.Unlock()
		t.Cleanup(cancel) // after newMacRig's: it runs first, so the rig's wait finds these helpers ended
		keptHelper(ctx, r, g, dir, secret)
		return nil
	}
	r.c.signal = func(int, unix.Signal) error {
		g.mu.Lock()
		defer g.mu.Unlock()
		if g.cancel != nil {
			g.cancel()
		}
		return nil
	}
	return r, g
}

// keptHelper is macRig.helper with the guest's scrub (POST /v1/scrub, and the boot scrub in Init's answer),
// serving secret as this run's desktop password.
func keptHelper(ctx context.Context, r *macRig, g *keptGuest, dir string, secret *macvm.DesktopSecret) {
	r.runs.Add(2)
	r.host.set(admission.Proc{PID: macHelperPID, UID: 501, Start: 7, Name: admission.HelperProcess})
	go func() {
		defer r.runs.Done()
		defer r.host.gone(macHelperPID)
		_ = macvm.Run(ctx, macvm.RunOptions{Engine: r.eng, RunDir: dir, NetWait: 10 * time.Second, Desktop: r.desktop, DesktopPassword: secret, SockFstat: macvmtest.SockFstat, Claim: func(cfg macvm.RunConfig) error {
			_, err := r.c.Ledger.ClaimHelper(cfg.LedgerEntry, macHelperPID, 7)
			return err
		}})
	}()
	go func() {
		defer r.runs.Done()
		mux := http.NewServeMux()
		mux.HandleFunc("GET /v1/ready", func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(guestagent.Ready{Version: "test"})
		})
		mux.HandleFunc("POST /v1/init", func(w http.ResponseWriter, req *http.Request) {
			var in guestagent.Init
			_ = json.NewDecoder(req.Body).Decode(&in)
			r.mu.Lock()
			r.init = &in
			res := guestagent.InitResult{Scrub: &guestagent.ScrubReport{When: guestagent.ScrubAtBoot, Outcome: guestagent.ScrubDone, Removed: map[string]int{"managed_settings": 1}}}
			if in.DesktopPassword != "" || in.SignInsOff {
				set := r.setPassword && !in.SignInsOff
				res.DesktopPasswordSet = &set
			}
			r.mu.Unlock()
			_ = json.NewEncoder(w).Encode(res)
		})
		mux.HandleFunc("POST /v1/scrub", func(w http.ResponseWriter, _ *http.Request) {
			g.mu.Lock()
			g.scrubs++
			g.mu.Unlock()
			_ = json.NewEncoder(w).Encode(guestagent.ScrubReport{When: guestagent.ScrubAtStop, Outcome: guestagent.ScrubDone,
				Removed: map[string]int{"managed_settings": 2}, CAs: []string{"ab12"}})
		})
		mux.HandleFunc("GET /v1/dial/tools", func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(guestagent.ToolsDial{Class: guestagent.DialRefused, Error: "connection refused", Dials: 1})
		})
		_ = guestagent.ServeVsock(ctx, func(context.Context) (net.Conn, error) {
			ms := r.eng.Machines()
			if len(ms) == 0 {
				return nil, net.ErrClosed
			}
			return ms[len(ms)-1].DialAgent(macvm.AgentPort)
		}, mux)
	}()
}

func (g *keptGuest) counts() (scrubs, spawns int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.scrubs, g.spawns
}

// keptSpec is an interactive macOS VM run that is a session of kept machine id.
func keptSpec(id uuid.UUID, mode string) runner.SandboxSpec {
	s := macSpec()
	s.RunID = uuid.New()
	s.Interactive = true
	s.KeptMachine = &runner.KeptMachineSpec{ID: id, Mode: mode, OwnerHash: keptOwnerHash, IdleDays: 30}
	return s
}

// keptMachine makes and stops a new kept machine, and answers its folder.
func keptMachine(t *testing.T, r *macRig, id uuid.UUID) string {
	t.Helper()
	sb, err := r.d.CreateSandbox(context.Background(), keptSpec(id, macvm.KeptNew))
	if err != nil {
		t.Fatal(err)
	}
	if err := r.d.StopSandbox(context.Background(), sb.Ref); err != nil {
		t.Fatal(err)
	}
	return r.c.Layout.KeptMachineDir(id.String())
}

func readKept(t *testing.T, dir string, id uuid.UUID) macvm.KeptRecord {
	t.Helper()
	rec, err := macvm.ReadKept(dir, id.String())
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

// A kept machine's session refuses, before anything exists, every secret env but the MCP routes and every
// file delivered in the component secret directory, naming each by key or path and never by value; the
// same spec without a kept machine is not refused for them.
func TestKeptSpecRefusesSecretEnvAndSecretFiles(t *testing.T) {
	r, _ := newKeptRig(t)
	for name, tc := range map[string]struct {
		mut  func(*runner.SandboxSpec)
		want string
	}{
		"secret env": {func(s *runner.SandboxSpec) { s.SecretEnv = map[string]string{"GH_TOKEN": "value-1"} }, "secret env GH_TOKEN"},
		"secret file": {func(s *runner.SandboxSpec) {
			s.ManagedFiles = []runner.ManagedFile{{Path: runner.ComponentSecretDir + "/db", Mode: runner.ComponentSecretFileMode, AgentOwned: true, Content: []byte("value-2")}}
		}, "secret file " + runner.ComponentSecretDir + "/db"},
		"both, with the routes": {func(s *runner.SandboxSpec) {
			s.SecretEnv = map[string]string{keptMCPSecretEnv: "value-3", "B_KEY": "value-4", "A_KEY": "value-5"}
		}, "secret env A_KEY, secret env B_KEY"},
	} {
		spec := keptSpec(uuid.New(), macvm.KeptResume)
		tc.mut(&spec)
		_, err := r.d.refuseMacVMSpec(spec)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: refusal %v, want it to name %q", name, err, tc.want)
			continue
		}
		if strings.Contains(err.Error(), "value-") || strings.Contains(err.Error(), keptMCPSecretEnv) {
			t.Errorf("%s: refusal %q names a value or the MCP routes", name, err)
		}
		spec.KeptMachine = nil
		if err := refuseKeptSecrets(spec); err != nil {
			t.Errorf("%s: a run with no kept machine was refused: %v", name, err)
		}
	}
	spec := keptSpec(uuid.New(), macvm.KeptNew)
	spec.SecretEnv = map[string]string{keptMCPSecretEnv: "routes"}
	spec.ManagedFiles = []runner.ManagedFile{{Path: "/etc/wardyn/managed-settings.json", Content: []byte("{}")}}
	if err := refuseKeptSecrets(spec); err != nil {
		t.Fatalf("the MCP routes and a managed setting were refused: %v", err)
	}
}

// A Mac whose limit is zero keeps no machines, and only an interactive run keeps or resumes one.
func TestKeptMachineRefusesOffMacAndBackgroundRun(t *testing.T) {
	r, g := newKeptRig(t)
	spec := keptSpec(uuid.New(), macvm.KeptNew)
	spec.Interactive = false
	if _, err := r.d.CreateSandbox(context.Background(), spec); err == nil || err.Error() != keptSaidOnlyLive {
		t.Fatalf("a background run: %v", err)
	}
	r.c.LimitBytes = 0
	if _, err := r.d.CreateSandbox(context.Background(), keptSpec(uuid.New(), macvm.KeptNew)); err == nil || err.Error() != keptSaidOff {
		t.Fatalf("a Mac that keeps none: %v", err)
	}
	if _, spawns := g.counts(); spawns != 0 {
		t.Fatalf("%d helpers started", spawns)
	}
}

// A new kept machine is cloned into the kept folder; a graceful stop asks the guest to scrub, deletes the
// run folder only, records the stop as scrubbed with a new expiry, and reports the disk kept with the
// scrub. Its boot scrub rides on the create's sandbox.
func TestKeptMachineStopScrubsAndKeeps(t *testing.T) {
	r, g := newKeptRig(t)
	id := uuid.New()
	spec := keptSpec(id, macvm.KeptNew)
	sb, err := r.d.CreateSandbox(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if sb.KeptScrub == nil || sb.KeptScrub.When != guestagent.ScrubAtBoot || sb.KeptScrub.Outcome != guestagent.ScrubDone {
		t.Fatalf("boot scrub %+v", sb.KeptScrub)
	}
	cfg, err := macvm.ReadRunConfig(r.c.runDir(spec.RunID))
	if err != nil || cfg.Kept == nil || cfg.Kept.ID != id.String() || cfg.Kept.Mode != macvm.KeptNew || cfg.Kept.RunnerID != "runner-a" || cfg.Kept.OwnerHash != keptOwnerHash {
		t.Fatalf("run.json kept block %+v %v", cfg.Kept, err)
	}
	dir := r.c.Layout.KeptMachineDir(id.String())
	if rec := readKept(t, dir, id); rec.Scrubbed {
		t.Fatal("a booted machine reads scrubbed")
	}
	before := time.Now().UTC()
	if err := r.d.StopSandbox(context.Background(), sb.Ref); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(r.c.runDir(spec.RunID)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the run folder outlived the stop: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, macvm.DiskFile)); err != nil {
		t.Fatalf("the kept disk went with the run: %v", err)
	}
	rec := readKept(t, dir, id)
	if !rec.Scrubbed || rec.StopKind != macvm.KeptStopGraceful || rec.LastStop.Before(before) || rec.ExpiresAt.Before(before.AddDate(0, 0, 29)) {
		t.Fatalf("record after a graceful stop %+v", rec)
	}
	if scrubs, _ := g.counts(); scrubs != 1 {
		t.Fatalf("%d stop scrubs, want 1", scrubs)
	}
	rep, ok := r.d.MacOSVMDisk(sb.Ref)
	if !ok || rep.State != runner.MacOSVMDiskKept || rep.Reason != runner.MacOSVMDiskKeptMachine || rep.Scrub == nil ||
		rep.Scrub.When != guestagent.ScrubAtStop || rep.Scrub.Outcome != guestagent.ScrubDone || len(rep.Scrub.CAs) != 1 || rep.Scrub.CAs[0] != "ab12" {
		t.Fatalf("disk report %+v", rep)
	}
}

// A kill keeps the machine as a hard stop: no stop scrub, recorded unscrubbed, reported not_run.
func TestKeptMachineKillKeepsTheMachine(t *testing.T) {
	r, g := newKeptRig(t)
	id := uuid.New()
	sb, err := r.d.CreateSandbox(context.Background(), keptSpec(id, macvm.KeptNew))
	if err != nil {
		t.Fatal(err)
	}
	if err := r.d.KillSandbox(context.Background(), sb.Ref); err != nil {
		t.Fatal(err)
	}
	dir := r.c.Layout.KeptMachineDir(id.String())
	if rec := readKept(t, dir, id); rec.Scrubbed || rec.StopKind != macvm.KeptStopHard {
		t.Fatalf("record after a kill %+v", rec)
	}
	if scrubs, _ := g.counts(); scrubs != 0 {
		t.Fatalf("a kill ran %d stop scrubs", scrubs)
	}
	if rep, ok := r.d.MacOSVMDisk(sb.Ref); !ok || rep.Reason != runner.MacOSVMDiskKeptMachine || rep.Scrub == nil || rep.Scrub.Outcome != keptScrubNotRun {
		t.Fatalf("disk report %+v", rep)
	}
}

// A resume boots the machine's own disk in its kept folder, not a new clone, and a stop keeps it again.
func TestKeptMachineResumeBootsTheKeptDisk(t *testing.T) {
	r, _ := newKeptRig(t)
	id := uuid.New()
	dir := keptMachine(t, r, id)
	if err := os.WriteFile(filepath.Join(dir, macvm.DiskFile), []byte("what the person made"), 0o600); err != nil {
		t.Fatal(err)
	}
	spec := keptSpec(id, macvm.KeptResume)
	sb, err := r.d.CreateSandbox(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	ms := r.eng.Machines()
	if p := ms[len(ms)-1].Plan; p.Storage[0].Path != filepath.Join(dir, macvm.DiskFile) {
		t.Fatalf("the resume boots %s", p.Storage[0].Path)
	}
	if err := r.d.StopSandbox(context.Background(), sb.Ref); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(dir, macvm.DiskFile)); err != nil || string(b) != "what the person made" {
		t.Fatalf("the kept disk after a resume: %q %v", b, err)
	}
	if rec := readKept(t, dir, id); !rec.Scrubbed {
		t.Fatalf("record after the resume's stop %+v", rec)
	}
}

// A fresh start reports the base its reset cloned, as the machine's record says after this boot; a record
// this boot did not write reports nothing.
func TestKeptFreshStartReportsTheClonesOwnBase(t *testing.T) {
	r, _ := newKeptRig(t)
	id := uuid.New()
	dir := keptMachine(t, r, id)
	spec := keptSpec(id, macvm.KeptFresh)
	sb, err := r.d.CreateSandbox(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.d.StopSandbox(context.Background(), sb.Ref) }()
	rec := readKept(t, dir, id)
	if sb.KeptBase == nil || sb.KeptBase.Name != rec.Base || !sb.KeptBase.PreparedAt.Equal(rec.BasePreparedAt) || rec.BasePreparedAt.IsZero() {
		t.Fatalf("reported %+v, record %+v", sb.KeptBase, rec)
	}
	other := keptSpec(id, macvm.KeptResume)
	if got := r.c.keptBootBase(other); got != nil {
		t.Fatalf("a record another boot wrote was reported: %+v", got)
	}
}

// A resume by another person, or of a machine another registration of this Mac made, is refused as no
// such machine before any helper starts, and the machine's record is left byte for byte.
func TestKeptResumeRefusesAnotherOwnersMachine(t *testing.T) {
	r, g := newKeptRig(t)
	id := uuid.New()
	dir := keptMachine(t, r, id)
	before, _ := os.ReadFile(filepath.Join(dir, macvm.KeptFile))
	_, spawned := g.counts()
	want := fmt.Sprintf("You have no kept machine %s.", id)
	spec := keptSpec(id, macvm.KeptResume)
	spec.KeptMachine.OwnerHash = "someone-else"
	if _, err := r.d.CreateSandbox(context.Background(), spec); err == nil || err.Error() != want {
		t.Fatalf("another person's resume: %v", err)
	}
	spec = keptSpec(id, macvm.KeptFresh)
	spec.KeptMachine.OwnerHash = "someone-else"
	if _, err := r.d.CreateSandbox(context.Background(), spec); err == nil || err.Error() != want {
		t.Fatalf("another person's reset: %v", err)
	}
	if after, _ := os.ReadFile(filepath.Join(dir, macvm.KeptFile)); !bytes.Equal(before, after) {
		t.Fatalf("the record changed:\n%s\n%s", before, after)
	}
	rec := readKept(t, dir, id)
	rec.RunnerID = "runner-b"
	if err := macvm.WriteKept(dir, rec); err != nil {
		t.Fatal(err)
	}
	if _, err := r.d.CreateSandbox(context.Background(), keptSpec(id, macvm.KeptResume)); err == nil || err.Error() != want {
		t.Fatalf("another registration's machine: %v", err)
	}
	if _, spawns := g.counts(); spawns != spawned {
		t.Fatalf("a refused resume started %d helpers", spawns-spawned)
	}
	// The helper's own refusal, when it is the one that sees it, reads the same.
	if err := r.c.keptStartErr(spec, fmt.Errorf("docker: the macOS VM did not start: %s", macvm.ErrKeptOwner)); err == nil || err.Error() != want {
		t.Fatalf("the helper's owner refusal: %v", err)
	}
}

// A machine whose lock another boot holds is refused before its helper starts, and the helper's own lock
// refusal reads the same.
func TestKeptResumeRefusesALockedMachine(t *testing.T) {
	r, g := newKeptRig(t)
	id := uuid.New()
	dir := keptMachine(t, r, id)
	unlock, err := macvm.LockKept(dir)
	if err != nil {
		t.Fatal(err)
	}
	_, spawned := g.counts()
	spec := keptSpec(id, macvm.KeptResume)
	if _, err := r.d.CreateSandbox(context.Background(), spec); err == nil || err.Error() != keptSaidLocked {
		t.Fatalf("a locked machine: %v", err)
	}
	unlock()
	if _, spawns := g.counts(); spawns != spawned {
		t.Fatal("a refused resume started a helper")
	}
	if err := r.c.keptStartErr(spec, fmt.Errorf("docker: the macOS VM did not start: %s", macvm.ErrKeptLocked)); err == nil || err.Error() != keptSaidLocked {
		t.Fatalf("the helper's lock refusal: %v", err)
	}
}

// A new machine that would take the Mac past its kept limit (each machine at its worst case, 51 GB for
// a 50 GB disk) is refused with the numbers before any clone.
func TestKeptLimitRefusesBeforeTheClone(t *testing.T) {
	r, g := newKeptRig(t)
	keptMachine(t, r, uuid.New())
	r.c.LimitBytes = 60 << 30
	_, spawned := g.counts()
	id := uuid.New()
	want := "This Mac keeps machines up to 60 GB, and another would need up to 51 GB more. Delete one with `wardyn machines delete`, or raise the limit on that Mac."
	if _, err := r.d.CreateSandbox(context.Background(), keptSpec(id, macvm.KeptNew)); err == nil || err.Error() != want {
		t.Fatalf("over the limit: %v", err)
	}
	if _, err := os.Lstat(r.c.Layout.KeptMachineDir(id.String())); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a refused machine has a folder: %v", err)
	}
	if _, spawns := g.counts(); spawns != spawned {
		t.Fatal("a refused machine started a helper")
	}
	r.c.LimitBytes = 110 << 30
	if _, err := r.d.CreateSandbox(context.Background(), keptSpec(id, macvm.KeptNew)); err != nil {
		t.Fatalf("within the limit: %v", err)
	}
}

// The low-disk check ends a kept machine's run as a hard stop; its teardown keeps the machine.
func TestLowDiskEndKeepsTheMachine(t *testing.T) {
	r, _ := newKeptRig(t)
	id := uuid.New()
	spec := keptSpec(id, macvm.KeptNew)
	if _, err := r.d.CreateSandbox(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	if run, err := r.d.EndNewestMacVM(context.Background(), 1<<30); run != spec.RunID.String() || err != nil {
		t.Fatalf("ended %q, %v", run, err)
	}
	if !r.d.teardownMacVM(context.Background(), spec.RunID) {
		t.Fatal("the run was not torn down")
	}
	dir := r.c.Layout.KeptMachineDir(id.String())
	if _, err := os.Stat(filepath.Join(dir, macvm.DiskFile)); err != nil {
		t.Fatalf("the low-disk end deleted the machine: %v", err)
	}
	if rec := readKept(t, dir, id); rec.Scrubbed || rec.StopKind != macvm.KeptStopHard {
		t.Fatalf("record after the low-disk end %+v", rec)
	}
}

// A new machine whose create fails is dropped; a resume whose create fails keeps its machine.
func TestKeptNewFailureDropsTheMachine(t *testing.T) {
	r, _ := newKeptRig(t)
	kept := uuid.New()
	dir := keptMachine(t, r, kept)
	r.setPassword = false // the guest sets no desktop password: the create fails after the boot
	id := uuid.New()
	if _, err := r.d.CreateSandbox(context.Background(), keptSpec(id, macvm.KeptNew)); err == nil {
		t.Fatal("the create did not fail")
	}
	if _, err := os.Lstat(r.c.Layout.KeptMachineDir(id.String())); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a failed new machine was kept: %v", err)
	}
	if _, err := r.d.CreateSandbox(context.Background(), keptSpec(kept, macvm.KeptResume)); err == nil {
		t.Fatal("the resume did not fail")
	}
	if _, err := os.Stat(filepath.Join(dir, macvm.DiskFile)); err != nil {
		t.Fatalf("a failed resume deleted the machine: %v", err)
	}
}

// kept_machine's delete removes a stopped machine and its lock, answers already_absent for one this Mac
// does not hold (another id that shares the folder key included, which stays), and refuses a booted one.
func TestKeptMachineDeleteOp(t *testing.T) {
	r, _ := newKeptRig(t)
	ctx := context.Background()
	if out, err := r.d.DeleteKeptMachine(ctx, uuid.New(), "owner"); err != nil || out != runnerwire.KeptMachineAlreadyAbsent {
		t.Fatalf("no such machine: %q %v", out, err)
	}
	id := uuid.New()
	dir := keptMachine(t, r, id)
	twin := uuid.MustParse(id.String()[:24] + "000000000000")
	if twin == id {
		twin = uuid.MustParse(id.String()[:24] + "111111111111")
	}
	if out, err := r.d.DeleteKeptMachine(ctx, twin, "admin"); err != nil || out != runnerwire.KeptMachineAlreadyAbsent {
		t.Fatalf("an id sharing the key: %q %v", out, err)
	}
	if out, err := r.d.DeleteKeptMachine(ctx, id, "owner"); err != nil || out != runnerwire.KeptMachineDeleted {
		t.Fatalf("delete: %q %v", out, err)
	}
	for _, p := range []string{dir, dir + ".lock"} {
		if _, err := os.Lstat(p); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("%s outlived the delete: %v", p, err)
		}
	}
	if out, err := r.d.DeleteKeptMachine(ctx, id, "owner"); err != nil || out != runnerwire.KeptMachineAlreadyAbsent {
		t.Fatalf("a repeat delete: %q %v", out, err)
	}
	booted := uuid.New()
	sb, err := r.d.CreateSandbox(ctx, keptSpec(booted, macvm.KeptNew))
	if err != nil {
		t.Fatal(err)
	}
	_, derr := r.d.DeleteKeptMachine(ctx, booted, "owner")
	if derr == nil {
		t.Fatal("a booted machine was deleted")
	}
	var we *runnerwire.Error
	if !errors.As(derr, &we) || we.Code != runnerwire.CodeRefused {
		t.Fatalf("a booted machine's refusal is a refused reply, since nothing was removed: %v", derr)
	}
	if _, err := os.Stat(filepath.Join(r.c.Layout.KeptMachineDir(booted.String()), macvm.DiskFile)); err != nil {
		t.Fatalf("a refused delete removed the disk: %v", err)
	}
	_ = r.d.StopSandbox(ctx, sb.Ref)
}

// The inventory names each machine's run while one boots it, and none once it is stopped; it counts
// every machine at its worst case against the limit.
func TestKeptMachineInventoryNamesItsRun(t *testing.T) {
	r, _ := newKeptRig(t)
	if k, err := r.d.KeptMachines(); err != nil || k == nil || len(k.Machines) != 0 || k.LimitBytes != 110<<30 {
		t.Fatalf("an empty Mac: %+v %v", k, err)
	}
	id := uuid.New()
	spec := keptSpec(id, macvm.KeptNew)
	sb, err := r.d.CreateSandbox(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	k, err := r.d.KeptMachines()
	if err != nil || len(k.Machines) != 1 {
		t.Fatalf("inventory %+v %v", k, err)
	}
	if m := k.Machines[0]; m.ID != id.String() || !m.InUse || m.RunID != spec.RunID.String() || k.WorstBytes != 51<<30 {
		t.Fatalf("a booted machine %+v (worst %d)", m, k.WorstBytes)
	}
	if err := r.d.StopSandbox(context.Background(), sb.Ref); err != nil {
		t.Fatal(err)
	}
	k, err = r.d.KeptMachines()
	if err != nil || len(k.Machines) != 1 {
		t.Fatalf("inventory %+v %v", k, err)
	}
	if m := k.Machines[0]; m.InUse || m.RunID != "" || !m.Scrubbed || m.ExpiresAt.IsZero() {
		t.Fatalf("a stopped machine %+v", m)
	}
	r.c.LimitBytes = 0
	if k, err := r.d.KeptMachines(); k != nil || err != nil {
		t.Fatalf("a Mac that keeps none reports %+v %v", k, err)
	}
}

// writeKeptFolder writes a kept machine's folder with only its record, as a sweep or erase sees it.
func writeKeptFolder(t *testing.T, r *macRig, rec macvm.KeptRecord) string {
	t.Helper()
	dir := r.c.Layout.KeptMachineDir(rec.ID)
	if err := macvm.WriteKept(dir, rec); err != nil {
		t.Fatal(err)
	}
	return dir
}

// The sweep deletes, with no network, an expired machine and one another registration of this Mac made;
// a current machine stays, and so does an expired one a boot holds. An expired machine's old generation
// that an update moved aside goes with it; a current machine's stays.
func TestKeptMachineSweepDeletesExpiredAndForeign(t *testing.T) {
	r, _ := newKeptRig(t)
	past, future := time.Now().Add(-time.Hour), time.Now().Add(time.Hour)
	expired := writeKeptFolder(t, r, macvm.KeptRecord{ID: uuid.NewString(), OwnerHash: keptOwnerHash, RunnerID: "runner-a", ExpiresAt: past})
	foreign := writeKeptFolder(t, r, macvm.KeptRecord{ID: uuid.NewString(), OwnerHash: keptOwnerHash, RunnerID: "runner-b", ExpiresAt: future})
	current := writeKeptFolder(t, r, macvm.KeptRecord{ID: uuid.NewString(), OwnerHash: keptOwnerHash, RunnerID: "runner-a", ExpiresAt: future})
	held := writeKeptFolder(t, r, macvm.KeptRecord{ID: uuid.NewString(), OwnerHash: keptOwnerHash, RunnerID: "runner-a", ExpiresAt: past})
	unlock, err := macvm.LockKept(held)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	for _, dir := range []string{expired, current} {
		if err := os.MkdirAll(dir+".update.old-"+uuid.NewString(), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.d.SweepKeptMachines(); err != nil {
		t.Fatal(err)
	}
	for dir, want := range map[string]bool{expired: false, foreign: false, current: true, held: true} {
		if _, err := os.Lstat(dir); (err == nil) != want {
			t.Errorf("%s present=%v after the sweep, want %v", dir, err == nil, want)
		}
	}
	for dir, want := range map[string]int{expired: 0, current: 1} {
		if old, _ := filepath.Glob(dir + ".update.old-*"); len(old) != want {
			t.Errorf("%s old generations after the sweep %v, want %d", dir, old, want)
		}
	}
}

// Erasing a revoked runner's kept machines tears down the run booted from one and deletes every folder
// in the kept folder, recorded or not.
func TestKeptMachineEraseAllTearsDownAndDeletes(t *testing.T) {
	r, _ := newKeptRig(t)
	spec := keptSpec(uuid.New(), macvm.KeptNew)
	if _, err := r.d.CreateSandbox(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	keptStaging(t, writeKeptFolder(t, r, macvm.KeptRecord{ID: uuid.NewString(), OwnerHash: keptOwnerHash, RunnerID: "runner-a"}))
	if err := os.MkdirAll(filepath.Join(r.c.Layout.KeptDir(), "debris"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := r.d.EraseKeptMachines(context.Background()); err != nil {
		t.Fatal(err)
	}
	des, err := os.ReadDir(r.c.Layout.KeptDir())
	if err != nil || len(des) != 0 {
		t.Fatalf("the kept folder after an erase: %v %v", des, err)
	}
	if _, err := os.Stat(r.c.runDir(spec.RunID)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the booted run's folder outlived the erase: %v", err)
	}
}

// keptTools is a fake tools updater that does what the real one does with the machine's lock: it takes it
// itself, refused while another holds it. It records, per call, whether the lock was free.
type keptTools struct {
	dirs []string
	free []bool
	err  error
}

func (k *keptTools) UpdateKeptTools(_ context.Context, dir string) error {
	unlock, err := macvm.LockKept(dir)
	k.dirs, k.free = append(k.dirs, dir), append(k.free, err == nil)
	if err != nil {
		return err
	}
	defer unlock()
	return k.err
}

// A resume runs the tools update on the machine's own folder before its boot, with the machine's lock free
// for the update to take; the boot then holds the lock. A failed update does not start the machine, and
// one whose lock another holds is tried again within the lock wait, then reads as running. A new machine
// is not updated.
func TestKeptResumeUpdatesToolsBeforeTheBootLock(t *testing.T) {
	r, g := newKeptRig(t)
	// The locked arm must try again inside lockWait; 100 ms can pass in the first try on a loaded runner.
	// The fake answers locked until tools.err changes, so that arm takes the whole second, no more.
	r.c.lockWait = time.Second
	tools := &keptTools{}
	r.c.Tools = tools
	id := uuid.New()
	dir := keptMachine(t, r, id)
	if len(tools.dirs) != 0 {
		t.Fatalf("a new machine was updated: %v", tools.dirs)
	}
	tools.err = errors.New("the update failed")
	_, spawned := g.counts()
	if _, err := r.d.CreateSandbox(context.Background(), keptSpec(id, macvm.KeptResume)); err == nil || err.Error() != keptSaidTools {
		t.Fatalf("a failed update: %v", err)
	}
	failed := len(tools.dirs)
	tools.err = macvm.ErrKeptLocked
	if _, err := r.d.CreateSandbox(context.Background(), keptSpec(id, macvm.KeptResume)); err == nil || err.Error() != keptSaidLocked {
		t.Fatalf("an update that found the machine locked: %v", err)
	}
	locked := len(tools.dirs)
	if _, spawns := g.counts(); spawns != spawned {
		t.Fatal("a machine whose update failed was started")
	}
	tools.err = nil
	sb, err := r.d.CreateSandbox(context.Background(), keptSpec(id, macvm.KeptResume))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.d.StopSandbox(context.Background(), sb.Ref) }()
	if failed != 1 || locked < failed+2 || len(tools.dirs) != locked+1 ||
		slices.ContainsFunc(tools.dirs, func(d string) bool { return d != dir }) || slices.Contains(tools.free, false) {
		t.Fatalf("updates %v (failed %d, locked %d), lock free %v", tools.dirs, failed, locked, tools.free)
	}
	if locked, err := macvm.KeptLocked(dir); err != nil || !locked {
		t.Fatalf("the boot after the update does not hold the machine's lock: %v %v", locked, err)
	}
}

// keptStaging writes a tools update's staging beside a machine's folder, as an update leaves it: a copy of
// the folder's record, and the staging's lock file.
func keptStaging(t *testing.T, dir string) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, macvm.KeptFile))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir+keptUpdateSuffix, 0o700); err != nil {
		t.Fatal(err)
	}
	for path, body := range map[string][]byte{filepath.Join(dir+keptUpdateSuffix, macvm.KeptFile): b, dir + keptUpdateSuffix + ".lock": nil} {
		if err := os.WriteFile(path, body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// A tools update's staging, <key>.update and its lock, is never a machine: the inventory does not list it,
// the sweep never deletes it while its machine's lock is held, even for a machine it deletes otherwise, and
// removes it, stale, only holding that lock. A deleted machine takes its staging with it.
func TestKeptUpdateStagingIsNotAMachine(t *testing.T) {
	r, _ := newKeptRig(t)
	future := time.Now().Add(time.Hour)
	foreign := writeKeptFolder(t, r, macvm.KeptRecord{ID: uuid.NewString(), OwnerHash: keptOwnerHash, RunnerID: "runner-b", ExpiresAt: future})
	current := writeKeptFolder(t, r, macvm.KeptRecord{ID: uuid.NewString(), OwnerHash: keptOwnerHash, RunnerID: "runner-a", ExpiresAt: future})
	keptStaging(t, foreign)
	keptStaging(t, current)
	k, err := r.d.KeptMachines()
	if err != nil || len(k.Machines) != 2 || k.WorstBytes != 2*(51<<30) {
		t.Fatalf("inventory with update staging %+v %v", k, err)
	}
	// Both machines held: nothing goes, the foreign one and either staging included.
	unlockF, err := macvm.LockKept(foreign)
	if err != nil {
		t.Fatal(err)
	}
	unlockC, err := macvm.LockKept(current)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.d.SweepKeptMachines(); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{foreign, foreign + keptUpdateSuffix, foreign + keptUpdateSuffix + ".lock", current + keptUpdateSuffix} {
		if !keptExists(p) {
			t.Fatalf("the sweep deleted %s while its machine was held", p)
		}
	}
	unlockF()
	unlockC()
	// Free: the foreign machine goes with its staging; the current one stays and loses its stale staging.
	if err := r.d.SweepKeptMachines(); err != nil {
		t.Fatal(err)
	}
	for p, want := range map[string]bool{foreign: false, foreign + keptUpdateSuffix: false, foreign + keptUpdateSuffix + ".lock": false, foreign + ".lock": false,
		current: true, current + keptUpdateSuffix: false, current + keptUpdateSuffix + ".lock": false} {
		if keptExists(p) != want {
			t.Errorf("%s present=%v after the sweep, want %v", p, !want, want)
		}
	}
	// The delete op takes a machine's staging too.
	id := uuid.MustParse(readKeptID(t, current))
	keptStaging(t, current)
	if out, err := r.d.DeleteKeptMachine(context.Background(), id, "owner"); err != nil || out != runnerwire.KeptMachineDeleted {
		t.Fatalf("delete: %q %v", out, err)
	}
	if keptExists(current+keptUpdateSuffix) || keptExists(current+keptUpdateSuffix+".lock") {
		t.Fatal("a deleted machine left its update staging")
	}
}

func readKeptID(t *testing.T, dir string) string {
	t.Helper()
	var rec macvm.KeptRecord
	b, err := os.ReadFile(filepath.Join(dir, macvm.KeptFile))
	if err != nil || json.Unmarshal(b, &rec) != nil {
		t.Fatalf("record in %s: %v", dir, err)
	}
	return rec.ID
}

// A spec with no idle days, or a nonsense count, still makes a machine with an expiry this Mac enforces:
// the site default of 30 days, in run.json and in the machine's record, and again at its stop.
func TestKeptMachineIdleDaysDefaultToThirty(t *testing.T) {
	r, _ := newKeptRig(t)
	for _, days := range []int{0, -1} {
		id := uuid.New()
		spec := keptSpec(id, macvm.KeptNew)
		spec.KeptMachine.IdleDays = days
		before := time.Now().UTC()
		sb, err := r.d.CreateSandbox(context.Background(), spec)
		if err != nil {
			t.Fatal(err)
		}
		if cfg, err := macvm.ReadRunConfig(r.c.runDir(spec.RunID)); err != nil || cfg.Kept == nil || cfg.Kept.IdleDays != 30 {
			t.Fatalf("idle days %d: run.json kept block %+v %v", days, cfg.Kept, err)
		}
		dir := r.c.Layout.KeptMachineDir(id.String())
		if rec := readKept(t, dir, id); rec.ExpiresAt.Before(before.AddDate(0, 0, 30)) || rec.ExpiresAt.After(time.Now().UTC().AddDate(0, 0, 30)) {
			t.Fatalf("idle days %d: a new machine expires at %v", days, rec.ExpiresAt)
		}
		if err := r.d.StopSandbox(context.Background(), sb.Ref); err != nil {
			t.Fatal(err)
		}
		if rec := readKept(t, dir, id); rec.ExpiresAt.Before(rec.LastStop.AddDate(0, 0, 30)) {
			t.Fatalf("idle days %d: a stopped machine expires at %v, stopped %v", days, rec.ExpiresAt, rec.LastStop)
		}
	}
	if keptSaidExpired(keptIdleDays(0), true).Error() != "Your machine wasn't used for 30 days, so it was deleted. Start a new one." {
		t.Fatal("the expiry sentence does not name the default")
	}
}

// The helper's recorded refusal code is read first and worded; with no code, an older helper's error text
// is matched; a code wins over the text.
func TestKeptStartErrReadsTheHelpersCodeFirst(t *testing.T) {
	r, _ := newKeptRig(t)
	id := uuid.New()
	spec := keptSpec(id, macvm.KeptResume)
	dir, err := r.c.makeRunDir(spec.RunID)
	if err != nil {
		t.Fatal(err)
	}
	code := func(c string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, macvm.KeptRefusalFile), []byte(`{"kept_refusal":"`+c+`"}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	notFound := fmt.Sprintf("You have no kept machine %s.", id)
	plain := errors.New("docker: the macOS VM helper did not start: the helper exited before it started the VM")
	for c, want := range map[string]string{macvm.KeptRefusalLocked: keptSaidLocked, macvm.KeptRefusalOwner: notFound,
		macvm.KeptRefusalIDMismatch: notFound, macvm.KeptRefusalIncomplete: keptSaidIncomplete} {
		code(c)
		if err := r.c.keptStartErr(spec, plain); err == nil || err.Error() != want {
			t.Errorf("code %s: %v, want %q", c, err, want)
		}
	}
	code(macvm.KeptRefusalIncomplete)
	if err := r.c.keptStartErr(spec, fmt.Errorf("x: %w", macvm.ErrKeptLocked)); err == nil || err.Error() != keptSaidIncomplete {
		t.Fatalf("the code did not win over the text: %v", err)
	}
	if err := os.Remove(filepath.Join(dir, macvm.KeptRefusalFile)); err != nil {
		t.Fatal(err)
	}
	if err := r.c.keptStartErr(spec, fmt.Errorf("x: %w", macvm.ErrKeptIDMismatch)); err == nil || err.Error() != notFound {
		t.Fatalf("an older helper's text: %v", err)
	}
	if err := r.c.keptStartErr(spec, plain); err == nil || err.Error() != plain.Error() {
		t.Fatalf("an error that is no refusal was reworded: %v", err)
	}
}

// A helper that refuses a kept machine's boot ends the create at once with the refusal's sentence, not at
// the control socket's two-minute wait; the machine stays.
func TestKeptCreateStopsWaitingForARefusingHelper(t *testing.T) {
	r, _ := newKeptRig(t)
	id := uuid.New()
	dir := keptMachine(t, r, id)
	if err := os.Remove(filepath.Join(dir, macvm.AuxFile)); err != nil { // the helper finds the machine incomplete
		t.Fatal(err)
	}
	start := time.Now()
	if _, err := r.d.CreateSandbox(context.Background(), keptSpec(id, macvm.KeptResume)); err == nil || err.Error() != keptSaidIncomplete {
		t.Fatalf("an incomplete machine: %v", err)
	}
	if d := time.Since(start); d > 30*time.Second {
		t.Fatalf("the create waited %s for a helper that had refused", d)
	}
	if _, err := os.Stat(filepath.Join(dir, macvm.DiskFile)); err != nil {
		t.Fatalf("the refused machine was deleted: %v", err)
	}
}

// A helper this runnerd started that exits before its control socket ends the wait at once.
func TestMacVMHelperWaitEndsWhenTheHelperExits(t *testing.T) {
	r, _ := newKeptRig(t)
	runID := uuid.New()
	dir, err := r.c.makeRunDir(runID)
	if err != nil {
		t.Fatal(err)
	}
	exits := &sync.Map{}
	gone := make(chan struct{})
	close(gone)
	exits.Store(dir, gone)
	r.c.helperExits = exits
	start := time.Now()
	if err := r.c.waitMacVMHelper(context.Background(), dir, runID); err == nil || !strings.Contains(err.Error(), "exited") {
		t.Fatalf("wait: %v", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("the wait took %s", d)
	}
	if _, ok := exits.Load(dir); ok {
		t.Fatal("the wait left the run dir's exit channel behind")
	}
}

// keptAt sets the rig's kept clock to at.
func keptAt(r *macRig, at time.Time) { r.c.keptNow = func() time.Time { return at } }

// keptRunning writes a record whose session is run, and run's folder when live, as a helper that has
// exited before its stop was recorded (live) or a session cut short (not live) leaves it.
func keptRunning(t *testing.T, r *macRig, run uuid.UUID, live bool, expires time.Time) (string, uuid.UUID) {
	t.Helper()
	id := uuid.New()
	dir := writeKeptFolder(t, r, macvm.KeptRecord{ID: id.String(), OwnerHash: keptOwnerHash, RunnerID: "runner-a",
		ActiveRun: run.String(), ExpiresAt: expires})
	if live {
		if err := os.MkdirAll(r.c.runDir(run), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return dir, id
}

// A machine resumed before its expiry and stopped after it is refreshed by its stop, and no sweep deletes
// it; a stop that is not the session's own changes nothing.
func TestKeptMachineUsedPastItsExpiryIsKept(t *testing.T) {
	r, _ := newKeptRig(t)
	id := uuid.New()
	dir := keptMachine(t, r, id)
	spec := keptSpec(id, macvm.KeptResume)
	sb, err := r.d.CreateSandbox(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if rec := readKept(t, dir, id); rec.ActiveRun != spec.RunID.String() {
		t.Fatalf("the resumed machine's session is %q, want %s", rec.ActiveRun, spec.RunID)
	}
	later := time.Now().UTC().AddDate(0, 0, 45)
	keptAt(r, later)
	if err := r.d.StopSandbox(context.Background(), sb.Ref); err != nil {
		t.Fatal(err)
	}
	rec := readKept(t, dir, id)
	if rec.ActiveRun != "" || !rec.LastStop.Equal(later) || !rec.ExpiresAt.Equal(later.AddDate(0, 0, 30)) {
		t.Fatalf("the stop after the old expiry recorded %+v", rec)
	}
	if err := r.d.SweepKeptMachines(); err != nil || !keptExists(filepath.Join(dir, macvm.DiskFile)) {
		t.Fatalf("the sweep after the stop: %v, disk there %v", err, keptExists(filepath.Join(dir, macvm.DiskFile)))
	}
	if r.c.keptStopped(&macvm.KeptRun{ID: id.String(), OwnerHash: keptOwnerHash, RunnerID: "runner-a"}, uuid.New(), nil); readKept(t, dir, id) != rec {
		t.Fatal("another run's stop changed the record")
	}
}

// Between a helper's exit and its stop's record, the run's folder is still there: a sweep and a resume
// leave the machine, expired or not, and the session's own stop then records it.
func TestKeptSweepWaitsForTheSessionsStop(t *testing.T) {
	r, _ := newKeptRig(t)
	run := uuid.New()
	dir, id := keptRunning(t, r, run, true, time.Now().Add(-time.Hour))
	if err := r.d.SweepKeptMachines(); err != nil {
		t.Fatal(err)
	}
	if rec := readKept(t, dir, id); rec.ActiveRun != run.String() {
		t.Fatalf("the sweep changed a held machine: %+v", rec)
	}
	if _, err := r.d.CreateSandbox(context.Background(), keptSpec(id, macvm.KeptResume)); err == nil || err.Error() != keptSaidLocked {
		t.Fatalf("a resume of a machine whose stop is not recorded: %v", err)
	}
	now := time.Now().UTC()
	keptAt(r, now)
	r.c.keptStopped(&macvm.KeptRun{ID: id.String(), OwnerHash: keptOwnerHash, RunnerID: "runner-a"}, run, nil)
	if rec := readKept(t, dir, id); rec.ActiveRun != "" || rec.StopKind != macvm.KeptStopHard || !rec.ExpiresAt.Equal(now.AddDate(0, 0, 30)) {
		t.Fatalf("the session's stop recorded %+v", rec)
	}
}

// A stop reports the expiry it wrote to the record, from the idle days the run was launched with, so the
// organisation records the same deadline whatever its setting is by then; a stop that is not the session's
// own writes and reports none.
func TestKeptStopReportsTheExpiryItWrote(t *testing.T) {
	r, _ := newKeptRig(t)
	now := time.Now().UTC()
	keptAt(r, now)
	for _, days := range []int{1, 30, 90} {
		run := uuid.New()
		dir, id := keptRunning(t, r, run, true, now.Add(time.Hour))
		k := &macvm.KeptRun{ID: id.String(), OwnerHash: keptOwnerHash, RunnerID: "runner-a", IdleDays: days}
		if _, got := r.c.keptStopped(k, uuid.New(), nil); !got.IsZero() {
			t.Fatalf("%d days: a stop of another run reported %s", days, got)
		}
		_, got := r.c.keptStopped(k, run, nil)
		if rec := readKept(t, dir, id); !got.Equal(rec.ExpiresAt) || !got.Equal(now.AddDate(0, 0, days)) {
			t.Fatalf("%d days: reported %s, record %s, want %s", days, got, rec.ExpiresAt, now.AddDate(0, 0, days))
		}
	}
	d := r.d
	run := uuid.New()
	d.keptMachineDisk(run, nil, now.AddDate(0, 0, 3))
	if rep, ok := d.macDisks.Load(run); !ok || !rep.(runner.MacOSVMDisk).KeptExpiresAt.Equal(now.AddDate(0, 0, 3)) {
		t.Fatalf("the disk report lost the expiry: %v", rep)
	}
}

// A stop that cannot lock the record writes nothing and reports the expiry as pending, not as no word; the
// Mac's own recovery then gives the machine the idle days its run was launched with, which its inventory
// carries to the organisation.
func TestKeptStopThatCannotLockReportsPending(t *testing.T) {
	r, _ := newKeptRig(t)
	now := time.Now().UTC()
	keptAt(r, now)
	run, id := uuid.New(), uuid.New()
	dir := writeKeptFolder(t, r, macvm.KeptRecord{ID: id.String(), OwnerHash: keptOwnerHash, RunnerID: "runner-a",
		ActiveRun: run.String(), IdleDays: 1, ExpiresAt: now.Add(time.Hour)})
	if err := os.MkdirAll(r.c.runDir(run), 0o700); err != nil {
		t.Fatal(err)
	}
	r.c.lockWait = 50 * time.Millisecond
	unlock, err := macvm.LockKept(dir)
	if err != nil {
		t.Fatal(err)
	}
	scrub, expiry := r.c.keptStopped(&macvm.KeptRun{ID: id.String(), OwnerHash: keptOwnerHash, RunnerID: "runner-a", IdleDays: 1}, run, nil)
	unlock()
	r.d.keptMachineDisk(run, scrub, expiry)
	v, _ := r.d.macDisks.Load(run)
	rep, ok := v.(runner.MacOSVMDisk)
	if !ok || !rep.KeptExpiresAt.IsZero() || !rep.KeptExpiryPending {
		t.Fatalf("the report of a stop that wrote nothing: %+v, %v", rep, ok)
	}
	if err := os.RemoveAll(r.c.runDir(run)); err != nil {
		t.Fatal(err)
	}
	// The run folder is gone and the record still names the session: not yet a machine to keep or expire.
	if k, err := r.d.KeptMachines(); err != nil || len(k.Machines) != 1 || !k.Machines[0].InUse || k.Machines[0].RunID != run.String() {
		t.Fatalf("the inventory of an unrecovered session: %+v, %v", k, err)
	}
	if err := r.d.SweepKeptMachines(); err != nil {
		t.Fatal(err)
	}
	if rec := readKept(t, dir, id); rec.ActiveRun != "" || !rec.ExpiresAt.Equal(now.AddDate(0, 0, 1)) {
		t.Fatalf("the recovered machine %+v, want the launched 1 day", rec)
	}
	if k, err := r.d.KeptMachines(); err != nil || k.Machines[0].InUse || !k.Machines[0].ExpiresAt.Equal(now.AddDate(0, 0, 1)) {
		t.Fatalf("the inventory after recovery: %+v, %v", k, err)
	}
}

// A session cut short (its run folder gone, its stop never recorded) is ended by the sweep and its machine
// kept, expired or not; an expired machine no session holds is still deleted.
func TestKeptSweepRecoversAnInterruptedSession(t *testing.T) {
	r, _ := newKeptRig(t)
	now := time.Now().UTC()
	keptAt(r, now)
	cut, cutID := keptRunning(t, r, uuid.New(), false, now.Add(-time.Hour))
	idle := writeKeptFolder(t, r, macvm.KeptRecord{ID: uuid.NewString(), OwnerHash: keptOwnerHash, RunnerID: "runner-a", ExpiresAt: now.Add(-time.Hour)})
	if err := r.d.SweepKeptMachines(); err != nil {
		t.Fatal(err)
	}
	if rec := readKept(t, cut, cutID); rec.ActiveRun != "" || !rec.ExpiresAt.Equal(now.AddDate(0, 0, 30)) {
		t.Fatalf("the interrupted session's machine %+v", rec)
	}
	if keptExists(idle) {
		t.Fatal("an expired machine no session holds was kept")
	}
	// A resume recovers the same way, and starts.
	id := uuid.New()
	dir := keptMachine(t, r, id)
	rec := readKept(t, dir, id)
	rec.ActiveRun, rec.ExpiresAt = uuid.NewString(), time.Now().Add(-time.Hour)
	if err := macvm.WriteKept(dir, rec); err != nil {
		t.Fatal(err)
	}
	if _, err := r.d.CreateSandbox(context.Background(), keptSpec(id, macvm.KeptResume)); err != nil {
		t.Fatalf("a resume after an interrupted session: %v", err)
	}
}

// An expired machine is said deleted only once it is gone; one its lock still holds will be deleted.
func TestKeptExpiryIsSaidDeletedOnlyWhenGone(t *testing.T) {
	r, _ := newKeptRig(t)
	id := uuid.New()
	dir := keptMachine(t, r, id)
	keptAt(r, time.Now().UTC().AddDate(0, 0, 31))
	spec := keptSpec(id, macvm.KeptResume)
	said := fmt.Errorf("docker: the macOS VM did not start: %s", macvm.ErrKeptExpired)
	unlock, err := macvm.LockKept(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.c.keptStartErr(spec, said); err == nil || err.Error() != "Your machine wasn't used for 30 days, so it expired and will be deleted." {
		t.Fatalf("an expired machine still locked: %v", err)
	}
	unlock()
	if !keptExists(dir) {
		t.Fatal("a locked machine was deleted")
	}
	if _, err := r.d.CreateSandbox(context.Background(), spec); err == nil || err.Error() != "Your machine wasn't used for 30 days, so it was deleted. Start a new one." {
		t.Fatalf("an expired machine: %v", err)
	}
	if keptExists(dir) {
		t.Fatal("the expired machine is still there")
	}
}

// keptBase sets the size of the disk the rig's base clones.
func keptBase(t *testing.T, r *macRig, diskBytes int64) {
	t.Helper()
	keptManifest(t, keptBaseDir(t, r), func(m *macvm.Manifest) { m.DiskBytes = diskBytes })
}

func keptBaseDir(t *testing.T, r *macRig) string {
	t.Helper()
	base, err := r.c.Layout.NamedBaseDir("")
	if err != nil {
		t.Fatal(err)
	}
	return base
}

// keptManifest changes the manifest in dir, a base's or a kept machine's copy.
func keptManifest(t *testing.T, dir string, change func(*macvm.Manifest)) {
	t.Helper()
	man, err := macvm.ReadManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	change(&man)
	b, _ := json.Marshal(man)
	if err := os.WriteFile(filepath.Join(dir, macvm.ManifestFile), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// Two new machines that each fit but not together cannot both reserve: the second start counts the
// first's reservation before its record exists. A dropped reservation frees its room.
func TestKeptReservationRefusesTheSecondStart(t *testing.T) {
	r, _ := newKeptRig(t)
	r.c.LimitBytes = 60 << 30
	man, err := r.d.refuseMacVMSpec(macSpec())
	if err != nil {
		t.Fatal(err)
	}
	first, err := r.c.reserveKept(keptSpec(uuid.New(), macvm.KeptNew), &man, &macvm.KeptRun{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.c.reserveKept(keptSpec(uuid.New(), macvm.KeptNew), &man, &macvm.KeptRun{}); err == nil || !strings.HasPrefix(err.Error(), "This Mac keeps machines up to 60 GB") {
		t.Fatalf("the second start: %v", err)
	}
	first()
	release, err := r.c.reserveKept(keptSpec(uuid.New(), macvm.KeptNew), &man, &macvm.KeptRun{})
	if err != nil {
		t.Fatalf("after the first dropped its reservation: %v", err)
	}
	release()
}

// A reset onto a larger base reserves the difference and is refused when that passes the limit; the
// machine is left as it was.
func TestKeptResetOntoALargerBaseIsRefused(t *testing.T) {
	r, g := newKeptRig(t)
	id := uuid.New()
	dir := keptMachine(t, r, id)
	r.c.LimitBytes = 60 << 30
	keptBase(t, r, 80<<30)
	_, spawned := g.counts()
	if _, err := r.d.CreateSandbox(context.Background(), keptSpec(id, macvm.KeptFresh)); err == nil || !strings.HasPrefix(err.Error(), "This Mac keeps machines up to 60 GB") {
		t.Fatalf("a reset onto a larger base: %v", err)
	}
	if _, spawns := g.counts(); spawns != spawned || !keptExists(filepath.Join(dir, macvm.DiskFile)) {
		t.Fatal("a refused reset started a helper or changed the machine")
	}
}

// A base replaced between the reservation and the helper's clone is refused by the helper, which clones
// only the base the reservation pinned: a new machine is not made, and a reset leaves the old disk as it was.
func TestKeptBaseSwappedBeforeTheCloneIsRefused(t *testing.T) {
	r, _ := newKeptRig(t)
	kept := uuid.New()
	keptDir := keptMachine(t, r, kept)
	before := readKept(t, keptDir, kept)
	disk, err := macvm.ReadManifest(keptDir)
	if err != nil {
		t.Fatal(err)
	}
	spawn := r.c.spawn
	r.c.spawn = func(dir string) error {
		keptBase(t, r, 80<<30) // after the reservation, before the clone
		return spawn(dir)
	}
	id := uuid.New()
	if _, err := r.d.CreateSandbox(context.Background(), keptSpec(id, macvm.KeptNew)); err == nil || err.Error() != keptSaidBase {
		t.Fatalf("a new machine on a swapped base: %v", err)
	}
	dir := r.c.Layout.KeptMachineDir(id.String())
	if keptExists(dir) || keptExists(dir+keptReserveSuffix) {
		t.Fatal("the refused machine or its reservation was kept")
	}
	keptBase(t, r, disk.DiskBytes)
	if _, err := r.d.CreateSandbox(context.Background(), keptSpec(kept, macvm.KeptFresh)); err == nil || err.Error() != keptSaidBase {
		t.Fatalf("a reset on a swapped base: %v", err)
	}
	if man, err := macvm.ReadManifest(keptDir); err != nil || !reflect.DeepEqual(man, disk) || !keptExists(filepath.Join(keptDir, macvm.DiskFile)) {
		t.Fatalf("the refused reset changed the old disk: %+v, %v", man, err)
	}
	if after := readKept(t, keptDir, kept); after.BasePreparedAt != before.BasePreparedAt || after.CreatedAt != before.CreatedAt {
		t.Fatalf("the refused reset changed the record: %+v", after)
	}
}

// A reservation whose create is gone is dropped by the sweep; inventory never lists one.
func TestKeptSweepDropsAStaleReservation(t *testing.T) {
	r, _ := newKeptRig(t)
	keptMachine(t, r, uuid.New())
	man, err := r.d.refuseMacVMSpec(macSpec())
	if err != nil {
		t.Fatal(err)
	}
	id := uuid.New()
	if _, err := r.c.reserveKept(keptSpec(id, macvm.KeptNew), &man, &macvm.KeptRun{}); err != nil {
		t.Fatal(err)
	}
	res := r.c.Layout.KeptMachineDir(id.String()) + keptReserveSuffix
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(res, old, old); err != nil {
		t.Fatal(err)
	}
	if k, err := r.d.KeptMachines(); err != nil || k == nil || len(k.Machines) != 1 {
		t.Fatalf("inventory with a reservation: %+v, %v", k, err)
	}
	if err := r.d.SweepKeptMachines(); err != nil || keptExists(res) {
		t.Fatalf("the stale reservation after the sweep: %v, there %v", err, keptExists(res))
	}
}

// A resume is checked against the machine's own manifest, not the Mac's current base: it starts when the
// base now needs more than the run has, and when there is no base; a machine that itself needs more is
// still refused. A reset still needs the base.
func TestKeptResumeIsCheckedAgainstItsOwnDisk(t *testing.T) {
	r, _ := newKeptRig(t)
	ctx := context.Background()
	id := uuid.New()
	dir := keptMachine(t, r, id)
	base := keptBaseDir(t, r)
	keptManifest(t, base, func(m *macvm.Manifest) { m.MinCPUs = 8 })
	sb, err := r.d.CreateSandbox(ctx, keptSpec(id, macvm.KeptResume))
	if err != nil {
		t.Fatalf("a resume after the base needs more CPUs: %v", err)
	}
	if err := r.d.StopSandbox(ctx, sb.Ref); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(base, macvm.ManifestFile)); err != nil {
		t.Fatal(err)
	}
	sb, err = r.d.CreateSandbox(ctx, keptSpec(id, macvm.KeptResume))
	if err != nil {
		t.Fatalf("a resume with no base on the Mac: %v", err)
	}
	if err := r.d.StopSandbox(ctx, sb.Ref); err != nil {
		t.Fatal(err)
	}
	if _, err := r.d.CreateSandbox(ctx, keptSpec(id, macvm.KeptFresh)); err == nil {
		t.Fatal("a reset with no base started")
	}
	if !keptExists(filepath.Join(dir, macvm.DiskFile)) {
		t.Fatal("a refused reset changed the machine")
	}
	keptManifest(t, dir, func(m *macvm.Manifest) { m.MinCPUs = 8 })
	if _, err := r.d.CreateSandbox(ctx, keptSpec(id, macvm.KeptResume)); err == nil || err.Error() != "Per-run CPUs on this runner are 4; a macOS VM needs at least 8." {
		t.Fatalf("a machine that needs more CPUs than the run has: %v", err)
	}
}

// An interrupted session's machine gets back the idle allowance it was given, not the default.
func TestKeptRecoveryKeepsTheMachinesIdleDays(t *testing.T) {
	r, _ := newKeptRig(t)
	now := time.Now().UTC()
	keptAt(r, now)
	for _, days := range []int{1, 90} {
		id := uuid.New()
		dir := writeKeptFolder(t, r, macvm.KeptRecord{ID: id.String(), OwnerHash: keptOwnerHash, RunnerID: "runner-a",
			ActiveRun: uuid.NewString(), IdleDays: days, ExpiresAt: now.Add(-time.Hour)})
		if err := r.d.SweepKeptMachines(); err != nil {
			t.Fatal(err)
		}
		if rec := readKept(t, dir, id); rec.ActiveRun != "" || !rec.ExpiresAt.Equal(now.AddDate(0, 0, days)) {
			t.Fatalf("idle days %d: the recovered machine %+v", days, rec)
		}
	}
}

// A start's record published between another start's listing of the kept folder and its reads never
// hides that start: its key counts its reservation while it has one, and counts it once.
func TestKeptReservationCountsARecordPublishedMeanwhile(t *testing.T) {
	r, _ := newKeptRig(t)
	man, err := r.d.refuseMacVMSpec(macSpec())
	if err != nil {
		t.Fatal(err)
	}
	a := uuid.New()
	if _, err := r.c.reserveKept(keptSpec(a, macvm.KeptNew), &man, &macvm.KeptRun{}); err != nil {
		t.Fatal(err)
	}
	r.c.keptListed = func() { // A's helper publishes its record as B reads
		r.c.keptListed = nil
		writeKeptFolder(t, r, macvm.KeptRecord{ID: a.String(), OwnerHash: keptOwnerHash, RunnerID: "runner-a"})
	}
	r.c.LimitBytes = 60 << 30
	if _, err := r.c.reserveKept(keptSpec(uuid.New(), macvm.KeptNew), &man, &macvm.KeptRun{}); err == nil || !strings.HasPrefix(err.Error(), "This Mac keeps machines up to 60 GB") {
		t.Fatalf("B beside A's published record: %v", err)
	}
	if !keptExists(r.c.Layout.KeptMachineDir(a.String())) {
		t.Fatal("the barrier did not publish A's record")
	}
	r.c.LimitBytes = 110 << 30
	if _, err := r.c.reserveKept(keptSpec(uuid.New(), macvm.KeptNew), &man, &macvm.KeptRun{}); err != nil {
		t.Fatalf("A's key counted twice: %v", err)
	}
}

// A clone that no longer fits the Mac's total when its helper has made it is refused: a new machine is
// dropped before its reservation goes, and a reset's replaced disk stays counted, by its record, once the
// reservation is gone.
func TestKeptConfirmRefusesACloneOverTheMacTotal(t *testing.T) {
	r, _ := newKeptRig(t)
	ctx := context.Background()
	id := uuid.New()
	dir := keptMachine(t, r, id)
	spawn := r.c.spawn
	r.c.spawn = func(run string) error {
		r.c.LimitBytes = 10 << 30 // the Mac's room shrinks after the reservation, before the confirm
		return spawn(run)
	}
	for _, mode := range []string{macvm.KeptNew, macvm.KeptFresh} {
		r.c.LimitBytes = 110 << 30
		kid := id
		if mode == macvm.KeptNew {
			kid = uuid.New()
		}
		if _, err := r.d.CreateSandbox(ctx, keptSpec(kid, mode)); err == nil || !strings.HasPrefix(err.Error(), "This Mac keeps machines up to 10 GB") {
			t.Fatalf("%s: a clone over the Mac's total: %v", mode, err)
		}
		kdir := r.c.Layout.KeptMachineDir(kid.String())
		if keptExists(kdir + keptReserveSuffix) {
			t.Fatalf("%s: the reservation outlived the create", mode)
		}
		if mode == macvm.KeptNew && keptExists(kdir) {
			t.Fatal("the refused new machine was kept")
		}
	}
	held, err := r.c.keptMacBytes("")
	if err != nil || held < keptWorstOf(dir) || !keptExists(filepath.Join(dir, macvm.DiskFile)) {
		t.Fatalf("the reset disk is not counted: %d, %v", held, err)
	}
}

// A stop's record waits for a lock held only for a moment rather than dropping the write; one never
// recorded is ended by the sweep as a hard stop once the run folder is gone.
func TestKeptStopWaitsForABriefHolder(t *testing.T) {
	r, _ := newKeptRig(t)
	run := uuid.New()
	r.c.lockWait = 10 * time.Second
	dir, id := keptRunning(t, r, run, true, time.Now().Add(time.Hour))
	unlock, err := macvm.LockKept(dir)
	if err != nil {
		t.Fatal(err)
	}
	go func() { time.Sleep(200 * time.Millisecond); unlock() }()
	r.c.keptStopped(&macvm.KeptRun{ID: id.String(), OwnerHash: keptOwnerHash, RunnerID: "runner-a"}, run, nil)
	if rec := readKept(t, dir, id); rec.ActiveRun != "" || rec.StopKind != macvm.KeptStopHard {
		t.Fatalf("the stop behind a brief holder: %+v", rec)
	}
	cut, cutID := keptRunning(t, r, uuid.New(), false, time.Now().Add(time.Hour))
	if err := r.d.SweepKeptMachines(); err != nil {
		t.Fatal(err)
	}
	if rec := readKept(t, cut, cutID); rec.ActiveRun != "" || rec.StopKind != macvm.KeptStopHard || rec.LastStop.IsZero() || rec.Scrubbed {
		t.Fatalf("a stop never recorded, after the sweep: %+v", rec)
	}
}

// A folder a create left with no record (its helper stopped between the clone and the record) is removed
// by the sweep once it is old, unless a create still reserves it or holds its lock. An old generation an
// update moved aside beside it stays: it is the person's disk when the update's new copy had no record.
func TestKeptSweepRemovesAFolderWithNoRecord(t *testing.T) {
	r, _ := newKeptRig(t)
	old := time.Now().Add(-time.Hour)
	folder := func() string {
		dir := r.c.Layout.KeptMachineDir(uuid.NewString())
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, macvm.DiskFile), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(dir, old, old); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	debris, reserved, held := folder(), folder(), folder()
	aside := debris + ".update.old-" + uuid.NewString()
	if err := os.MkdirAll(aside, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(reserved+keptReserveSuffix, []byte(`{"bytes":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	unlock, err := macvm.LockKept(held)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	fresh := r.c.Layout.KeptMachineDir(uuid.NewString())
	if err := os.MkdirAll(fresh, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := r.d.SweepKeptMachines(); err != nil {
		t.Fatal(err)
	}
	for dir, want := range map[string]bool{debris: false, reserved: true, held: true, fresh: true, aside: true} {
		if keptExists(dir) != want {
			t.Errorf("%s present=%v after the sweep, want %v", dir, keptExists(dir), want)
		}
	}
}

// Erasure waits for a tools update holding a machine's lock, and reports a machine whose lock is still
// held as not erased.
func TestKeptEraseWaitsForAToolsUpdate(t *testing.T) {
	r, _ := newKeptRig(t)
	ctx := context.Background()
	dir := keptMachine(t, r, uuid.New())
	unlock, err := macvm.LockKept(dir) // a tools update in it
	if err != nil {
		t.Fatal(err)
	}
	r.c.eraseWait = 300 * time.Millisecond
	if err := r.d.EraseKeptMachines(ctx); err == nil || !strings.Contains(err.Error(), "was not erased") || !keptExists(dir) {
		t.Fatalf("an erase beside a held machine: %v, present %v", err, keptExists(dir))
	}
	r.c.eraseWait = 10 * time.Second
	go func() { time.Sleep(200 * time.Millisecond); unlock() }()
	if err := r.d.EraseKeptMachines(ctx); err != nil || keptExists(dir) {
		t.Fatalf("an erase after the update ended: %v, present %v", err, keptExists(dir))
	}
}

// The name a kept machine is made with reaches its record, survives its stop, and a resume that names it
// otherwise renames it.
func TestKeptMachineNameRoundTrips(t *testing.T) {
	r, _ := newKeptRig(t)
	ctx := context.Background()
	id := uuid.New()
	dir := r.c.Layout.KeptMachineDir(id.String())
	for _, name := range []string{"build box", "release box"} {
		mode := macvm.KeptResume
		if !keptExists(dir) {
			mode = macvm.KeptNew
		}
		spec := keptSpec(id, mode)
		spec.KeptMachine.Name = name
		sb, err := r.d.CreateSandbox(ctx, spec)
		if err != nil {
			t.Fatal(err)
		}
		if err := r.d.StopSandbox(ctx, sb.Ref); err != nil {
			t.Fatal(err)
		}
		if rec := readKept(t, dir, id); rec.Name != name || rec.ActiveRun != "" {
			t.Fatalf("%s %q: the stopped machine's record %+v", mode, name, rec)
		}
	}
}
