// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package main

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
	"github.com/cjohnstoniv/wardyn/internal/macvm/admission"
	"github.com/cjohnstoniv/wardyn/internal/macvm/macvmtest"
)

// prepNets is the Docker side of the jobs' stage-2 networks: one per job at most, built by start (the
// driver's StartMacPrepNet) and gone by its undo or by remove (RemoveMacPrepNet). Building one that is
// up, or taking one down that is gone, is an error: no second net_fd, no double undo.
type prepNets struct {
	t                      *testing.T
	mu                     sync.Mutex
	up                     map[string]bool
	built, undone, removed int
}

func (p *prepNets) down(id, how string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.up[id] {
		p.t.Errorf("%s the stage-2 network of %s, which is not up", how, id)
	}
	delete(p.up, id)
}

func (p *prepNets) start(_ context.Context, id string) (*os.File, func(), error) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		return nil, nil, err
	}
	_ = unix.Close(fds[1])
	p.mu.Lock()
	if p.up[id] {
		p.t.Errorf("built the stage-2 network of %s while it is up", id)
	}
	if p.up == nil {
		p.up = map[string]bool{}
	}
	p.up[id] = true
	p.built++
	p.mu.Unlock()
	return os.NewFile(uintptr(fds[0]), "relay"), func() {
		p.mu.Lock()
		p.undone++
		p.mu.Unlock()
		p.down(id, "undid")
	}, nil
}

func (p *prepNets) remove(_ context.Context, id string) error {
	p.mu.Lock()
	p.removed++
	p.mu.Unlock()
	p.down(id, "removed")
	return nil
}

func (p *prepNets) counts() (built, undone, removed int, up bool, n int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, u := range p.up {
		up = up || u
	}
	return p.built, p.undone, p.removed, up, len(p.up)
}

// use points m at nets, with the job sockets under sockRoot.
func (p *prepNets) use(m *macOSVMs, sockRoot string) {
	m.sockRoot, m.sockFstat = sockRoot, macvmtest.SockFstat
	m.prepNet, m.prepRemove = p.start, p.remove
}

const stage2HelperPID = 4343

// liveStage2Job is a tools update whose helper (pid stage2HelperPID) lives and whose stage-2 boot (the
// helper's real Stage2BootConfig, on a fake engine) listens on the job's ctl.sock until the test ends.
// attached is closed once the boot has its network.
func liveStage2Job(t *testing.T, r *macLedgerRig, m *macOSVMs) (e admission.Entry, attached <-chan error) {
	t.Helper()
	r.host.setProcs(admission.Proc{PID: os.Getpid(), UID: 501, Start: 3, Name: "wardyn-runnerd"},
		admission.Proc{PID: stage2HelperPID, UID: 501, Start: 9, Name: admission.HelperProcess})
	prevPoll := jobSockPoll
	jobSockPoll = 10 * time.Millisecond
	t.Cleanup(func() { jobSockPoll = prevPoll })
	e, err := m.ledger.Admit(admission.Request{Kind: admission.KindToolsUpdate, GuestCPUs: 4, GuestMemory: 8 << 30,
		DiskReserve: admission.RunDiskReservation(macvm.BaseDiskBytes), OwnerPID: os.Getpid(), OwnerStart: 3})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.ledger.ClaimHelper(e.ID, stage2HelperPID, 9); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	dir := t.TempDir()
	for _, f := range []string{macvm.DiskFile, macvm.AuxFile} {
		_ = os.WriteFile(filepath.Join(dir, f), nil, 0o600)
	}
	spec := macvm.MachineSpec{CPUs: 4, MemoryBytes: 8 << 30, MinCPUs: 2, MinMemoryBytes: 4 << 30, HardwareModel: []byte("hw"),
		MachineIdentifier: []byte("id"), DiskPath: filepath.Join(dir, macvm.DiskFile), AuxPath: filepath.Join(dir, macvm.AuxFile)}
	boot := macvm.Stage2BootConfig{Engine: &macvmtest.Engine{}, SockRoot: m.sockRoot, SockKey: macvm.JobSockKey(e.ID), SockFstat: macvmtest.SockFstat,
		NetWait: 10 * time.Second, GuestWait: 10 * time.Second}
	ch := make(chan error, 1)
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		_, stop, err := boot.ToolsBoot()(ctx, spec)
		ch <- err
		if stop != nil {
			<-ctx.Done()
			_ = stop(context.Background())
		}
	}()
	t.Cleanup(func() { cancel(); <-stopped })
	return e, ch
}

func awaitAttached(t *testing.T, attached <-chan error) {
	t.Helper()
	select {
	case err := <-attached:
		if err != nil {
			t.Fatalf("the stage-2 boot: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the stage-2 boot never got its network")
	}
}

// serving starts m's job handling under a ctx of its own, as serve's start does (with its sweep left out).
func serving(m *macOSVMs) context.CancelFunc {
	ctx, cancel := context.WithCancel(context.Background())
	m.mu.Lock()
	m.ctx = ctx
	m.mu.Unlock()
	return cancel
}

// handled waits until m runs no job handler.
func handled(t *testing.T, m *macOSVMs) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		m.mu.Lock()
		n := len(m.nets)
		m.mu.Unlock()
		if n == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("a job handler outlived its runnerd's stop")
		}
	}
}

// helperExits ends the job's helper and waits for its stage-2 network to go.
func helperExits(t *testing.T, r *macLedgerRig, m *macOSVMs, nets *prepNets) {
	t.Helper()
	r.host.setProcs(admission.Proc{PID: os.Getpid(), UID: 501, Start: 3, Name: "wardyn-runnerd"})
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if _, _, _, up, _ := nets.counts(); !up {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the stage-2 network outlived the job's helper")
		}
	}
	m.recompute()
	m.recompute()
	time.Sleep(100 * time.Millisecond)
}

// R3 M2 + L1: a graceful stop or restart of runnerd mid stage 2 leaves the job its network (the helper
// outlives runnerd), and the runnerd after it adopts that network: it builds none and never sends the
// helper a second net_fd, then removes it once, when the helper has gone.
func TestMacOSVMStopLeavesTheJobItsNetworkAndARestartAdoptsIt(t *testing.T) {
	r := newMacLedgerRig(t)
	nets := &prepNets{t: t}
	first := r.open(t)
	nets.use(first, macvmtest.SockRoot(t))
	_, attached := liveStage2Job(t, r, first)
	stop := serving(first)
	first.recompute()
	awaitAttached(t, attached)
	stop() // launchctl kickstart -k, or the package's upgrade
	handled(t, first)
	if built, undone, removed, up, _ := nets.counts(); built != 1 || undone != 0 || removed != 0 || !up {
		t.Fatalf("after a graceful stop: built %d, undone %d, removed %d, up %v; want the job's network left up", built, undone, removed, up)
	}

	next := r.open(t)
	nets.use(next, first.sockRoot)
	defer serving(next)()
	next.recompute()
	next.recompute()
	time.Sleep(200 * time.Millisecond)
	if built, _, removed, up, _ := nets.counts(); built != 1 || removed != 0 || !up {
		t.Fatalf("after the restart: built %d, removed %d, up %v; want the network adopted, none built or sent", built, removed, up)
	}
	helperExits(t, r, next, nets)
	if built, undone, removed, _, n := nets.counts(); built != 1 || undone != 0 || removed != 1 || n != 0 {
		t.Fatalf("after the helper exits: built %d, undone %d, removed %d (%d up); want one removal", built, undone, removed, n)
	}
}

// The overseer's pin: runnerd crashes mid stage 2, after its hand-off, so no handler and no undo of its
// run; the next runnerd's start sweep keeps the ledgered job's network, its recompute adopts it, and it
// is removed exactly once when the helper has gone, never undone as well.
func TestMacOSVMCrashMidStage2RestartAdoptsAndNeverDoubleUndoes(t *testing.T) {
	r := newMacLedgerRig(t)
	nets := &prepNets{t: t}
	dead := r.open(t)
	nets.use(dead, macvmtest.SockRoot(t))
	e, attached := liveStage2Job(t, r, dead)
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if _, err := os.Lstat(macvm.JobControlSock(dead.sockRoot, e.ID)); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the stage-2 boot never listened on the job's ctl.sock")
		}
	}
	attach, _, err := nets.start(context.Background(), e.ID) // the dead runnerd's handNet, cut short after the hand-off
	if err != nil {
		t.Fatal(err)
	}
	if err := dead.sendJobNet(context.Background(), e.ID, attach); err != nil {
		t.Fatal(err)
	}
	_ = attach.Close()
	awaitAttached(t, attached)

	next := r.open(t)
	nets.use(next, dead.sockRoot)
	var kept []string
	next.prepSweep = func(_ context.Context, keep []string) (int, error) {
		kept = keep
		return 0, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	next.start(ctx, func() {})
	next.recompute()
	next.recompute()
	time.Sleep(200 * time.Millisecond)
	if built, _, removed, up, _ := nets.counts(); len(kept) != 1 || kept[0] != e.ID || built != 1 || removed != 0 || !up {
		t.Fatalf("after the restart: kept %v, built %d, removed %d, up %v; want the job's network kept and adopted", kept, built, removed, up)
	}
	helperExits(t, r, next, nets)
	if built, undone, removed, _, n := nets.counts(); built != 1 || undone != 0 || removed != 1 || n != 0 {
		t.Fatalf("after the helper exits: built %d, undone %d, removed %d (%d up); want one removal, no undo", built, undone, removed, n)
	}
}

// R3 L2: an entry that names this runner but whose file another login owns is not this runner's: the
// entries directory is shared, so anyone can write one.
func TestMacOSVMEntryOfAnotherLoginIsNotMine(t *testing.T) {
	r := newMacLedgerRig(t)
	r.host.setProcs(admission.Proc{PID: os.Getpid(), UID: 501, Start: 3, Name: "wardyn-runnerd"},
		admission.Proc{PID: stage2HelperPID, UID: 502, Start: 9, Name: admission.HelperProcess})
	m := r.open(t)
	e, err := m.ledger.Admit(admission.Request{Kind: admission.KindPrepare, GuestCPUs: 4, GuestMemory: 8 << 30,
		DiskReserve: admission.PrepareDiskReservation(macvm.BaseDiskBytes), OwnerPID: os.Getpid(), OwnerStart: 3})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.ledger.ClaimHelper(e.ID, stage2HelperPID, 9); err != nil {
		t.Fatal(err)
	}
	if got := m.own(admission.KindPrepare); len(got) != 1 || !m.preparing() {
		t.Fatalf("this login's own entry: %+v", got)
	}
	r.own("e-"+e.ID+".json", 502)
	if got := m.own(admission.KindPrepare); len(got) != 0 || m.preparing() {
		t.Fatalf("an entry another login owns read as this runner's: %+v", got)
	}
}

// R3 L3: a per-run limit below the base's minimum offers none, and the caps carry the owner's text for
// it (MV-R47, MV-R48 in mocks/MACVM/strings.md), naming the flag to raise.
func TestMacOSVMBelowMinimumRefusesWithTheCanonText(t *testing.T) {
	r := newMacLedgerRig(t)
	m, _ := openWithBudget(t, r)
	for _, tc := range []struct {
		cpuMillis, memMiB int64
		want              string
	}{
		{4000, 2048, "Per-run memory on this runner is 2 GB; a macOS VM needs at least 4 GB. Its owner raises it to 4 GB or more in Your account → My runner, or with `wardyn runner set runner-a --per-run-memory 4`."},
		{4000, 3584, "Per-run memory on this runner is 3.5 GB; a macOS VM needs at least 4 GB. Its owner raises it to 4 GB or more in Your account → My runner, or with `wardyn runner set runner-a --per-run-memory 4`."},
		{2000, 8192, "Per-run CPU on this runner is 2 CPUs; a macOS VM needs at least 4. Its owner raises it to 4 or more in Your account → My runner, or with `wardyn runner set runner-a --per-run-cpu 4`."},
		{4000, 8192, ""},
	} {
		m.setPerRunLimits(tc.cpuMillis, tc.memMiB)
		m.recompute()
		if c := m.current(); c.Refusal != tc.want || (tc.want != "" && c.Max != 0) {
			t.Errorf("limits (%d, %d): max %d, refusal %q; want %q", tc.cpuMillis, tc.memMiB, c.Max, c.Refusal, tc.want)
		}
	}
}
