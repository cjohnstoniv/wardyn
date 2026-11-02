// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package macvm

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"golang.org/x/sys/unix"

	"github.com/cjohnstoniv/wardyn/internal/vm/guestagent"
)

// DriveOpenTarget is where the throwaway guest mounts the drive. An agent wrote that disk, so only the
// guest's kernel ever parses it: the Mac gets its files as a stream, over the agent socket.
const DriveOpenTarget = guestagent.MacAgentHome + "/drive"

// DriveOpenReady is the one line `wardyn-macvm drive-open` prints, as JSON on stdout, once the guest has
// the drive mounted. Its Exec sessions on AgentSock run as the guest's agent user.
type DriveOpenReady struct {
	AgentSock string `json:"agent_sock"`
	Target    string `json:"target"`
}

// DriveOpenOptions configures DriveOpen.
type DriveOpenOptions struct {
	Engine Engine
	Layout Layout
	Slot   int
	// Object is the drive's object name (Layout.DriveImage). Label is its volume's name, which only the
	// control plane's drive id gives and which a run's guest formatted it with; the guest finds the
	// volume by it, and a wrong one fails the Init without touching the drive.
	Object, Label string
	CPUs          int
	MemoryMiB     uint64
	// RunnerID, Entry and LedgerDir name the admission entry runnerd took, which Claim claims.
	RunnerID, Entry, LedgerDir string
	// SockRoot overrides the socket root, admission.DefaultSockDir (tests).
	SockRoot string
	Claim    func(RunConfig) error
	Leaked   func(RunConfig) error
	// HoldIdleSleep is RunOptions.HoldIdleSleep.
	HoldIdleSleep func(pid int) (stop func())
	SockFstat     func(fd int, name string) (unix.Stat_t, error)
	Log           *slog.Logger
	// Ready is called once the guest has the drive mounted.
	Ready func(DriveOpenReady)
}

// driveBoot is what makes a helper's boot a look at a drive and not a run. Only DriveOpen sets it, and
// it is no field of run.json, so `wardyn-macvm run` can never boot this way.
type driveBoot struct {
	init  guestagent.Init
	ready func(DriveOpenReady)
	// failed holds why the guest did not open the drive, which stopped the VM.
	failed chan error
}

// driveGuestWait bounds the guest's boot and its Init, which replays the drive's journal.
const driveGuestWait = 10 * time.Minute

// driveEntryRe is a ledger entry's id as the ledger mints it. It names a directory DriveOpen removes.
var driveEntryRe = regexp.MustCompile(`^[0-9a-f]{1,64}$`)

// driveOpenRoot holds the throwaway guests' clones. It is apart from the runs directory because runnerd
// treats what it finds there as its runs.
func (l Layout) driveOpenRoot() string { return filepath.Join(l.Root, "drive-open") }

// DriveOpen boots a throwaway clone of the ready base with one existing disk drive attached, for as
// long as ctx lives: `wardyn-macvm drive-open`. The boot is a run's, so it has no tools port and no
// desktop; it reaches no network, and its Init holds the drive and nothing else: no environment, secret,
// file or CA, and nothing ever starts a harness. The drive's lock is held for the boot's life. The clone
// goes when it ends; it returns nil only after a requested stop, ctx's error when ctx ends.
func DriveOpen(ctx context.Context, o DriveOpenOptions) error {
	img, err := o.Layout.DriveImage(o.Object)
	if err != nil {
		return err
	}
	if o.Label == "" || !driveEntryRe.MatchString(o.Entry) {
		return errors.New("macvm: opening a disk drive needs its volume's name and an admission entry")
	}
	// Never made here: a new image would be a blank drive the guest formats.
	fi, err := os.Lstat(img)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("macvm: this Mac has no disk drive %s", o.Object)
	}
	if err == nil && !fi.Mode().IsRegular() {
		err = errors.New("not a file")
	}
	if err == nil {
		err = checkOwner(fi)
	}
	if err != nil {
		return fmt.Errorf("macvm: disk drive %s: %w", o.Object, err)
	}
	key := JobSockKey(o.Entry)
	o.sweepClones(key)
	dir := filepath.Join(o.Layout.driveOpenRoot(), key)
	id := DiskID(0)
	if err := WriteRunConfig(dir, RunConfig{RunID: key, BaseDir: o.Layout.BaseDir(o.Slot), CPUs: o.CPUs, MemoryMiB: o.MemoryMiB, Identity: IdentityFresh,
		RunnerID: o.RunnerID, LedgerEntry: o.Entry, LedgerDir: o.LedgerDir, SockRoot: o.SockRoot,
		Disks: []Disk{{ID: id, Path: img, SizeBytes: fi.Size()}}}); err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	boot := &driveBoot{ready: o.Ready, failed: make(chan error, 1), init: guestagent.Init{SignInsOff: true,
		Disks: []guestagent.Disk{{ID: id, Label: o.Label, Target: DriveOpenTarget, SizeBytes: fi.Size()}}}}
	err = Run(ctx, RunOptions{Engine: o.Engine, RunDir: dir, Log: o.Log, Claim: o.Claim, Leaked: o.Leaked, HoldIdleSleep: o.HoldIdleSleep, SockFstat: o.SockFstat, drive: boot})
	select {
	case failed := <-boot.failed:
		return failed
	default:
		return err
	}
}

// sweepClones removes the clones of helpers that died before they could: past sockSweepGrace, with
// nothing answering on their ctl.sock.
func (o DriveOpenOptions) sweepClones(own string) {
	entries, err := os.ReadDir(o.Layout.driveOpenRoot())
	if err != nil {
		return
	}
	for _, e := range entries {
		fi, err := e.Info()
		if err != nil || e.Name() == own || time.Since(fi.ModTime()) < sockSweepGrace {
			continue
		}
		c, err := net.DialTimeout("unix", filepath.Join(SockDir(o.SockRoot, e.Name()), ControlSock), time.Second)
		if err == nil {
			_ = c.Close()
			continue
		}
		_ = os.RemoveAll(filepath.Join(o.Layout.driveOpenRoot(), e.Name()))
	}
}

// noReachNet attaches the VM's network to one end of a socket pair that carries nothing, before ctl.sock
// exists, so the guest reaches nothing and net_fd, which only takes a first network, can never add one.
// The other end is returned: closing it would end the boot as a lost network.
func (r *runner) noReachNet() (*os.File, error) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		return nil, fmt.Errorf("macvm: socketpair: %w", err)
	}
	unix.CloseOnExec(fds[0])
	unix.CloseOnExec(fds[1])
	r.netCh <- os.NewFile(uintptr(fds[0]), "no-reach-net")
	r.st.Network = NetAttached
	return os.NewFile(uintptr(fds[1]), "no-reach-net-peer"), nil
}

// initDriveGuest, on DriveOpen's boot, waits for the running guest's agent and hands it the drive, then
// calls ready. When that fails it stops the VM and leaves the reason in failed. The func returned ends the wait.
func (r *runner) initDriveGuest(ctx context.Context) (stop func()) {
	if r.o.drive == nil {
		return func() {}
	}
	wait, stop := context.WithTimeout(ctx, driveGuestWait)
	sock := filepath.Join(r.sockDir, AgentSock)
	go func() {
		var nd net.Dialer
		c := guestagent.NewClient(func(ctx context.Context) (net.Conn, error) { return nd.DialContext(ctx, "unix", sock) })
		defer c.Close()
		err := c.WaitReady(wait, 20*time.Second, func() bool { return r.status().State != StateRunning })
		if err == nil {
			_, err = c.Init(wait, r.o.drive.init)
		}
		switch {
		case err == nil:
			r.o.drive.ready(DriveOpenReady{AgentSock: sock, Target: DriveOpenTarget})
		case !errors.Is(wait.Err(), context.Canceled) && !errors.Is(err, guestagent.ErrStopped):
			r.o.drive.failed <- fmt.Errorf("macvm: the guest did not open the drive: %w", err)
			select {
			case r.stopReq <- stopRequest{graceful: true, timeout: DefaultStopTimeout}:
			default: // a stop is already on its way
			}
		}
	}()
	return stop
}
