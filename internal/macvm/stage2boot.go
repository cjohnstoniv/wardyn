// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package macvm

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/vm/guestagent"
	"golang.org/x/sys/unix"
)

// Stage2BootConfig configures the stage-2 boot of prepare and of a tools
// update: the base (or its staging copy) on vz with the run-style network
// runnerd hands over, and tools mode on.
type Stage2BootConfig struct {
	Engine Engine
	// SockRoot is the socket root ("" is admission.DefaultSockDir), and
	// SockKey this job's socket directory under u<uid>: JobSockKey of the
	// ledger entry runnerd took. runnerd connects to its ctl.sock there and
	// sends the NIC's descriptor (net_fd), as for a run.
	SockRoot  string
	SockKey   string
	SockFstat func(fd int, name string) (unix.Stat_t, error)
	// NetWait bounds the wait for runnerd's net_fd (0: 2 minutes);
	// GuestWait the wait for each guest agent session (0: 5 minutes).
	NetWait   time.Duration
	GuestWait time.Duration
	// Leaked, if set, runs when the VM's start is refused with ErrVMLimit: the helper records the framework's
	// slot the ledger cannot account for (RunOptions.Leaked). Its failure is logged.
	Leaked func() error
	Log    *slog.Logger
}

// JobSockKey is the socket directory key of a prepare's or a tools update's
// stage-2 boot: the first 12 hex digits of its ledger entry's id.
func JobSockKey(entryID string) string { return entryID[:min(len(entryID), RunKeyLen)] }

// stage2Ports are a stage-2 boot's vsock listeners: a run's, plus tools.
var stage2Ports = []uint32{AgentPort, DesktopPort, ToolsPort}

// ToolsBoot returns the stage-2 boot this configuration describes. It shares no folder: only a run's
// boot does.
func (c Stage2BootConfig) ToolsBoot() ToolsBoot {
	return func(ctx context.Context, spec MachineSpec) (ToolsGuest, func(context.Context) error, error) {
		if len(spec.Shares) > 0 || len(spec.Disks) > 0 {
			return nil, nil, errSharesOutsideRun
		}
		return c.boot(ctx, spec)
	}
}

// ErrStage2Stopped is a stage-2 boot that runnerd stopped through its ctl.sock.
var ErrStage2Stopped = errors.New("macvm: the stage-2 boot was stopped through its ctl.sock")

// stage2VM is one stage-2 boot: its ctl.sock answers hello, status, stop and
// net_fd like a run's (no desktop_password: a stage-2 boot has no run
// credential). stop ends the boot at any point and replies once the VM is
// stopped and the boot's sockets are gone.
type stage2VM struct {
	events  eventHub
	netCh   chan *os.File
	mu      sync.Mutex
	st      Status
	stopReq chan struct{} // closed by ctl.sock stop
	down    chan struct{} // closed once the boot is undone
	reqOnce sync.Once
	dnOnce  sync.Once
}

func (v *stage2VM) status() Status {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.st
}
func (v *stage2VM) setState(s MachineState, detail string) {
	v.mu.Lock()
	v.st.State, v.st.Error = s, detail
	v.mu.Unlock()
}
func (v *stage2VM) finish() { v.dnOnce.Do(func() { close(v.down) }) }
func (v *stage2VM) stop(bool, time.Duration) error {
	v.reqOnce.Do(func() { close(v.stopReq) })
	<-v.down
	if st := v.status(); st.State != StateStopped {
		return fmt.Errorf("macvm: the stage-2 VM is %s after stop", st.State)
	}
	return nil
}
func (v *stage2VM) netFD(f *os.File) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.st.Network != NetWaiting {
		return errAlreadyAttached
	}
	select {
	case v.netCh <- f:
		v.st.Network = NetAttached
		return nil
	default:
		return errAlreadyAttached
	}
}
func (v *stage2VM) subscribe() (<-chan Event, func()) { return v.events.subscribe() }
func (v *stage2VM) desktopPassword() (string, error)  { return "", ErrNoDesktopPassword }

func (c Stage2BootConfig) boot(ctx context.Context, spec MachineSpec) (ToolsGuest, func(context.Context) error, error) {
	if c.Engine == nil || c.SockKey == "" {
		return nil, nil, errors.New("macvm: a stage-2 boot needs an engine and a socket key")
	}
	if c.Log == nil {
		c.Log = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	var undo []func()
	fail := func(err error) (ToolsGuest, func(context.Context) error, error) {
		for i := len(undo) - 1; i >= 0; i-- {
			undo[i]()
		}
		return nil, nil, err
	}
	sc := SockConfig(c.SockRoot)
	sc.Fstat = c.SockFstat
	if err := checkSocketPaths(filepath.Join(sc.UserDir(), c.SockKey)); err != nil {
		return nil, nil, err
	}
	v := &stage2VM{netCh: make(chan *os.File, 1), stopReq: make(chan struct{}), down: make(chan struct{}),
		st: Status{Proto: ControlProto, State: StateStopped, HelperPID: os.Getpid(), Network: NetWaiting, CPUs: spec.CPUs, MemoryMiB: spec.MemoryBytes / mib}}
	undo = append(undo, v.finish)
	dir, err := sc.MakeRunDir(c.SockKey)
	if err != nil {
		return fail(err)
	}
	undo = append(undo, func() { _ = os.RemoveAll(dir) }, v.events.close)
	ctl, err := listenUnix(filepath.Join(dir, ControlSock))
	if err != nil {
		return fail(err)
	}
	undo = append(undo, func() { _ = ctl.Close() })
	go serveControl(ctl, v)
	attach, err := waitNetFD(ctx, v, cmpDuration(c.NetWait, 2*time.Minute))
	if err != nil {
		return fail(err)
	}
	attachConn, err := net.FileConn(attach)
	_ = attach.Close()
	if err != nil {
		return fail(fmt.Errorf("macvm: network descriptor: %w", err))
	}
	undo = append(undo, func() { _ = attachConn.Close() })
	guestNet, hostNet, err := NewNetPair()
	if err != nil {
		return fail(err)
	}
	undo = append(undo, func() { _ = guestNet.Close(); _ = hostNet.Close() })
	go func() { _ = (&Bridge{Guest: hostNet, Attach: attachConn, Stderr: logWriter{c.Log}}).Run() }()
	if err := thermalGate(c.Engine); err != nil {
		return fail(err)
	}
	spec.Net = guestNet
	plan, err := BuildPlan(spec)
	if err != nil {
		return fail(err)
	}
	var (
		m     Machine
		pairs map[uint32]*pairer
	)
	// The first boot's VM, just stopped, may hold the aux storage a moment
	// longer: each attempt builds the VM and its listeners afresh and undoes
	// them all when it fails.
	err = retryAuxLocked(ctx, c.Log, func() error {
		var attempt []func()
		failed := func(err error) error {
			for i := len(attempt) - 1; i >= 0; i-- {
				attempt[i]()
			}
			return err
		}
		var err error
		if m, err = c.Engine.NewMachine(plan); err != nil {
			return fmt.Errorf("macvm: build the stage-2 VM: %w", err)
		}
		pairs = map[uint32]*pairer{}
		for _, port := range stage2Ports {
			l, err := m.Listen(port)
			if err != nil {
				return failed(fmt.Errorf("macvm: vsock %d: %w", port, err))
			}
			attempt = append(attempt, func() { _ = l.Close() })
			var open []byte
			if port == DesktopPort {
				open = []byte{DesktopOpenByte}
			}
			p := newPairer(open)
			attempt = append(attempt, p.close)
			pairs[port] = p
			go p.acceptGuest(l)
		}
		if err := m.Start(ctx); err != nil {
			return failed(fmt.Errorf("macvm: start the stage-2 VM: %w", err))
		}
		undo = append(undo, attempt...)
		return nil
	})
	if err != nil {
		reportVMLimit(err, c.Leaked, c.Log)
		return fail(err)
	}
	undo = append(undo, func() { _ = m.Stop() })
	v.mu.Lock()
	v.st.Listeners = append(v.st.Listeners, stage2Ports...)
	v.st.State, v.st.StartedAt = StateRunning, time.Now().UTC()
	v.mu.Unlock()
	wait := cmpDuration(c.GuestWait, 5*time.Minute)
	life, kill := context.WithCancelCause(context.Background())
	g := &toolsGuest{
		tools:   guestagent.NewClient(pairDial(pairs[ToolsPort], wait)),
		control: guestagent.NewClient(pairDial(pairs[AgentPort], wait)),
		desktop: pairDial(pairs[DesktopPort], wait),
		tmp:     os.TempDir(),
		life:    life,
	}
	var (
		once sync.Once
		serr error
	)
	shutdown := func(sctx context.Context, cause error) error {
		once.Do(func() {
			v.setState(StateStopping, "")
			kill(cause)
			g.close()
			if serr = stopMachine(sctx, m); serr != nil {
				v.setState(StateError, serr.Error())
			} else {
				v.setState(StateStopped, "")
			}
			for i := len(undo) - 1; i >= 0; i-- {
				undo[i]()
			}
		})
		return serr
	}
	go func() {
		select {
		case <-v.stopReq:
			sctx, cancel := context.WithTimeout(context.Background(), DefaultStopTimeout)
			defer cancel()
			_ = shutdown(sctx, ErrStage2Stopped)
		case <-life.Done():
		}
	}()
	stop := func(sctx context.Context) error {
		err := shutdown(sctx, errors.New("macvm: the stage-2 boot ended"))
		if errors.Is(context.Cause(life), ErrStage2Stopped) {
			return errors.Join(ErrStage2Stopped, err)
		}
		return err
	}
	return g, stop, nil
}

func cmpDuration(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}

// waitNetFD waits for runnerd's net_fd on the stage-2 boot's ctl.sock.
func waitNetFD(ctx context.Context, v *stage2VM, wait time.Duration) (*os.File, error) {
	select {
	case f := <-v.netCh:
		return f, nil
	case <-v.stopReq:
		return nil, errors.New("macvm: the stage-2 boot was stopped before its network arrived")
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(wait):
		return nil, errors.New("macvm: runnerd never sent the stage-2 boot's network descriptor")
	}
}

// stopMachine asks the guest to shut down and waits until sctx ends, then
// stops it hard.
func stopMachine(sctx context.Context, m Machine) error {
	if m.State() == StateStopped {
		return nil
	}
	if err := m.RequestStop(); err == nil {
		select {
		case <-m.Done():
			return nil
		case <-sctx.Done():
		}
	}
	if err := m.Stop(); err != nil {
		return err
	}
	select {
	case <-m.Done():
		return nil
	case <-time.After(hardStopWait):
		return errors.New("macvm: the stage-2 VM did not stop")
	}
}

// pairDial dials a vsock port's idle guest connection, waiting at most wait.
func pairDial(p *pairer, wait time.Duration) func(ctx context.Context) (net.Conn, error) {
	return func(ctx context.Context) (net.Conn, error) {
		ctx, cancel := context.WithTimeout(ctx, wait)
		defer cancel()
		return p.take(ctx)
	}
}

// toolsGuest is ToolsGuest over the guest agent's tools-mode session (1026)
// and its control session (1024, which answers the tools-port dial and runs
// the launcher's self-test as the agent user), and the control check's RFB
// client over the desktop path (1025).
type toolsGuest struct {
	tools, control *guestagent.Client
	desktop        func(ctx context.Context) (net.Conn, error)
	tmp            string          // where a reader that cannot be read twice is spooled
	life           context.Context // ends when the boot is stopped
}

// bound is ctx, also ended when the boot is stopped.
func (g *toolsGuest) bound(ctx context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancelCause(ctx)
	unhook := context.AfterFunc(g.life, func() { cancel(context.Cause(g.life)) })
	return ctx, func() { unhook(); cancel(nil) }
}

var _ ToolsGuest = (*toolsGuest)(nil)
var _ DesktopProbe = (*toolsGuest)(nil)

func (g *toolsGuest) close() { g.tools.Close(); g.control.Close() }

// Push sends r with the SHA-256 the wire needs: hashed in place for a
// seekable reader, else spooled to a temp file first.
func (g *toolsGuest) Push(ctx context.Context, dest string, mode fs.FileMode, r io.Reader, size int64) error {
	ctx, done := g.bound(ctx)
	defer done()
	rs, ok := r.(io.ReadSeeker)
	if !ok {
		f, err := os.CreateTemp(g.tmp, ".wardyn-push-*")
		if err != nil {
			return err
		}
		defer os.Remove(f.Name())
		defer f.Close()
		if _, err := io.Copy(f, io.LimitReader(r, size+1)); err != nil {
			return err
		}
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return err
		}
		rs = f
	}
	start, err := rs.Seek(0, io.SeekCurrent)
	if err != nil {
		return err
	}
	h := sha256.New()
	if n, err := io.Copy(h, io.LimitReader(rs, size+1)); err != nil {
		return err
	} else if n != size {
		return fmt.Errorf("macvm: push %s: %d bytes, want %d", dest, n, size)
	}
	if _, err := rs.Seek(start, io.SeekStart); err != nil {
		return err
	}
	return g.tools.ToolsPush(ctx, dest, mode, io.LimitReader(rs, size), size, hex.EncodeToString(h.Sum(nil)))
}

func (g *toolsGuest) Run(ctx context.Context, argv []string) (string, int, error) {
	ctx, done := g.bound(ctx)
	defer done()
	return g.tools.ToolsRun(ctx, argv)
}

func (g *toolsGuest) DesktopPassword(ctx context.Context, pw []byte) (bool, error) {
	ctx, done := g.bound(ctx)
	defer done()
	return g.tools.ToolsDesktopPassword(ctx, string(pw))
}

func (g *toolsGuest) Pointer(ctx context.Context) (int, int, error) {
	ctx, done := g.bound(ctx)
	defer done()
	p, err := g.tools.ToolsPointer(ctx)
	return p.X, p.Y, err
}

func (g *toolsGuest) ToolsDial(ctx context.Context) (string, error) {
	ctx, done := g.bound(ctx)
	defer done()
	d, err := g.control.ToolsDial(ctx)
	return d.Class, err
}

// AgentExec runs argv over the control session, as the agent user through the
// run's own exec path (the guest needs no Init for it), and returns its stdout
// and stderr together.
func (g *toolsGuest) AgentExec(ctx context.Context, argv, env []string) (string, int, error) {
	ctx, done := g.bound(ctx)
	defer done()
	s, err := g.control.Exec(ctx, guestagent.ExecRequest{Argv: argv, Env: env})
	if err != nil {
		return "", 0, err
	}
	defer s.Close()
	_ = s.Stdin.Close()
	var mu sync.Mutex
	var out bytes.Buffer
	var wg sync.WaitGroup
	for _, r := range []io.Reader{s.Stdout, s.Stderr} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			buf := make([]byte, 4096)
			for {
				n, err := r.Read(buf)
				mu.Lock()
				out.Write(buf[:n])
				mu.Unlock()
				if err != nil {
					return
				}
			}
		}()
	}
	code, err := s.Wait()
	wg.Wait()
	return out.String(), code, err
}

// MovePointer logs in to the guest's Screen Sharing with VncAuth over the
// desktop path and sends one PointerEvent to (x, y), no buttons.
func (g *toolsGuest) MovePointer(ctx context.Context, pw []byte, x, y int) error {
	ctx, done := g.bound(ctx)
	defer done()
	c, err := g.desktop(ctx)
	if err != nil {
		return err
	}
	defer c.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = c.SetDeadline(dl)
	}
	if _, err := rfbLogin(bufio.NewReader(c), c, string(pw)); err != nil {
		return err
	}
	ev := []byte{5, 0, 0, 0, 0, 0}
	binary.BigEndian.PutUint16(ev[2:], uint16(x))
	binary.BigEndian.PutUint16(ev[4:], uint16(y))
	_, err = c.Write(ev)
	return err
}
