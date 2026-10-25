// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package macvm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// RunOptions configures one helper `run`.
type RunOptions struct {
	Engine Engine
	RunDir string
	Log    *slog.Logger
	// Desktop serves desktop.sock; nil leaves it unserved.
	Desktop DesktopShim
	// DesktopPassword is the run's VNC password, served on ctl.sock (OpDesktopPassword) and wiped
	// when Run returns; nil is a run with no desktop.
	DesktopPassword *DesktopSecret
	// NetWait bounds the wait for runnerd's net_fd before the VM starts.
	NetWait time.Duration
	// HoldIdleSleep, if set, is started once the VM runs and its stop func
	// called when the VM ends (the idle-sleep assertion).
	HoldIdleSleep func(pid int) (stop func())
	// Listening, if set, is called once ctl.sock accepts connections.
	Listening func()
	// Claim, if set, runs before the VM is built: the helper records itself
	// in its admission entry (admission.Ledger.ClaimHelper) and does not
	// start when that fails.
	Claim func(RunConfig) error
	// Leaked, if set, runs when the VM's start was refused with ErrVMLimit: the helper records the
	// framework's slot the ledger cannot account for. Its failure is logged; the run still fails with the
	// start error.
	Leaked func(RunConfig) error
	// SockFstat replaces fstat(2) in the socket directory checks (tests).
	SockFstat func(fd int, name string) (unix.Stat_t, error)
	// extraPorts are vsock ports this boot registers beyond a run's (tests
	// of the listener check).
	extraPorts []uint32
}

// DefaultStopTimeout is the graceful stop allowance when none is given.
const DefaultStopTimeout = 30 * time.Second

// ErrNetworkLost ends a run whose relay attach stream dropped.
var ErrNetworkLost = errors.New("macvm: the macOS VM lost its network")

// hardStopWait bounds the wait for a VM to end after a hard stop.
const hardStopWait = 15 * time.Second

type runner struct {
	o       RunOptions
	cfg     RunConfig
	id      Identity
	events  eventHub
	sockDir string

	mu      sync.Mutex
	st      Status
	machine Machine
	hostNet *os.File
	netCh   chan *os.File

	stopReq  chan stopRequest
	stopOnce sync.Once
	stopped  chan struct{}
}

type stopRequest struct {
	graceful bool
	timeout  time.Duration
}

// Run is `wardyn-macvm run --dir <run-dir>`: it clones the base, builds the
// VM, serves ctl.sock, waits for the network descriptor, starts the VM and
// serves the guest agent until the VM stops. It returns nil after a requested
// stop, and the cause otherwise.
func Run(ctx context.Context, o RunOptions) error {
	if o.Log == nil {
		o.Log = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	if o.NetWait == 0 {
		o.NetWait = 2 * time.Minute
	}
	if o.DesktopPassword != nil {
		defer o.DesktopPassword.Wipe()
	}
	cfg, err := ReadRunConfig(o.RunDir)
	if err != nil {
		return err
	}
	sockDir, err := makeRunSockDir(o, cfg)
	if err != nil {
		return err
	}
	defer os.RemoveAll(sockDir)
	r := &runner{
		o: o, cfg: cfg, sockDir: sockDir,
		netCh:   make(chan *os.File, 1),
		stopReq: make(chan stopRequest, 1),
		stopped: make(chan struct{}),
		st: Status{
			Proto: ControlProto, RunID: cfg.RunID, State: StateStopped, HelperPID: os.Getpid(),
			Identity: cfg.Identity, CPUs: cfg.CPUs, MemoryMiB: cfg.MemoryMiB, Network: NetWaiting,
			Display: Display{Width: DisplayWidth, Height: DisplayHeight, PPI: DisplayPPI},
		},
	}
	defer r.events.close()
	return r.run(ctx)
}

func (r *runner) run(ctx context.Context) error {
	if err := thermalGate(r.o.Engine); err != nil {
		return err
	}
	man, id, err := cloneReadyBase(r.o.Engine, r.cfg.BaseDir, r.o.RunDir, r.cfg.Identity)
	if err != nil {
		return err
	}
	r.id = id
	r.mu.Lock()
	r.st.MachineID, r.st.MachineIDSHA256 = r.id.Encoded(), IDHash(r.id.MachineIdentifier)
	r.mu.Unlock()
	r.o.Log.Info("macvm: identity configured", "run_id", r.cfg.RunID, "identity", string(r.id.Strategy),
		"machine_id_sha256", IDHash(r.id.MachineIdentifier))

	guestNet, hostNet, err := NewNetPair()
	if err != nil {
		return err
	}
	defer guestNet.Close()
	r.hostNet = hostNet
	defer hostNet.Close()
	plan, err := BuildPlan(MachineSpec{
		CPUs: r.cfg.CPUs, MemoryBytes: r.cfg.MemoryMiB * mib,
		MinCPUs: man.MinCPUs, MinMemoryBytes: man.MinMemoryMiB * mib,
		HardwareModel: man.HardwareModel, MachineIdentifier: r.id.MachineIdentifier,
		DiskPath: filepath.Join(r.o.RunDir, DiskFile), AuxPath: filepath.Join(r.o.RunDir, AuxFile),
		BaseDir: r.cfg.BaseDir, Net: guestNet,
	})
	if err != nil {
		return err
	}
	if r.o.Claim != nil {
		if err := r.o.Claim(r.cfg); err != nil {
			return fmt.Errorf("macvm: admission: %w", err)
		}
	}
	m, err := r.o.Engine.NewMachine(plan)
	if err != nil {
		return fmt.Errorf("macvm: build VM: %w", err)
	}

	ctl, err := listenUnix(filepath.Join(r.sockDir, ControlSock))
	if err != nil {
		return err
	}
	defer ctl.Close()
	go serveControl(ctl, r)
	if r.o.Listening != nil {
		r.o.Listening()
	}

	// The VM starts only once its network is attached.
	var attach *os.File
	select {
	case attach = <-r.netCh:
	case req := <-r.stopReq:
		r.finish(nil, req)
		return nil
	case <-ctx.Done():
		r.finish(nil, stopRequest{})
		return ctx.Err()
	case <-time.After(r.o.NetWait):
		r.finish(nil, stopRequest{})
		return errors.New("macvm: runnerd never sent the network descriptor")
	}
	netErr := make(chan error, 1)
	attachConn, err := net.FileConn(attach)
	_ = attach.Close()
	if err != nil {
		r.finish(nil, stopRequest{})
		return fmt.Errorf("macvm: network descriptor: %w", err)
	}
	defer attachConn.Close()
	go func() {
		netErr <- (&Bridge{Guest: hostNet, Attach: attachConn, Stderr: logWriter{r.o.Log}}).Run()
	}()

	if err := thermalGate(r.o.Engine); err != nil {
		r.finish(nil, stopRequest{})
		return err
	}
	r.setState(StateStarting, "")
	if err := m.Start(ctx); err != nil {
		if errors.Is(err, ErrVMLimit) && r.o.Leaked != nil {
			if lerr := r.o.Leaked(r.cfg); lerr != nil {
				r.o.Log.Error("macvm: recording a leaked slot failed", "run_id", r.cfg.RunID, "err", lerr.Error())
			}
		}
		r.setState(StateError, err.Error())
		r.finish(nil, stopRequest{})
		return fmt.Errorf("macvm: start VM: %w", err)
	}
	readBack := ""
	if id, err := m.MachineIdentifier(); err == nil {
		readBack = IDHash(id)
	} else {
		r.o.Log.Warn("macvm: identity read-back failed", "run_id", r.cfg.RunID, "err", err.Error())
	}
	r.o.Log.Info("macvm: identity read back", "run_id", r.cfg.RunID, "identity", string(r.id.Strategy),
		"machine_id_sha256", IDHash(r.id.MachineIdentifier), "readback_sha256", readBack)
	r.mu.Lock()
	r.machine = m
	r.st.ReadBackSHA256 = readBack
	r.mu.Unlock()
	// Running only once the listener set is checked and agent.sock serves.
	stopServing, err := r.serveGuest(m)
	if err != nil {
		r.setState(StateError, err.Error())
		r.finish(m, stopRequest{})
		return err
	}
	defer stopServing()
	r.setState(StateRunning, "")
	if r.o.HoldIdleSleep != nil {
		defer r.o.HoldIdleSleep(os.Getpid())()
	}

	select {
	case <-m.Done():
		r.finish(m, stopRequest{})
		return errors.New("macvm: the macOS VM stopped")
	case req := <-r.stopReq:
		r.finish(m, req)
		return nil
	case err := <-netErr:
		r.mu.Lock()
		r.st.Network = NetLost
		r.mu.Unlock()
		r.events.publish(Event{Type: EventNetwork, Detail: NetLost})
		r.o.Log.Error("macvm: network lost", "err", err.Error())
		r.finish(m, stopRequest{})
		return ErrNetworkLost
	case <-ctx.Done():
		r.finish(m, stopRequest{graceful: true, timeout: DefaultStopTimeout})
		return ctx.Err()
	}
}

// serveGuest listens for the guest agent's vsock connections and serves
// agent.sock (and desktop.sock when a shim is set). Every vsock listener is
// registered first, and the set is checked before agent.sock exists, so
// runnerd can send no Init to a VM listening on anything but a run's ports.
func (r *runner) serveGuest(m Machine) (func(), error) {
	var closers []io.Closer
	cleanup := func() {
		for _, c := range closers {
			_ = c.Close()
		}
	}
	type guestPort struct {
		port  uint32
		sock  string
		open  []byte
		serve func(host, guest net.Conn)
	}
	ports := []guestPort{{port: AgentPort, sock: AgentSock, serve: splice}}
	if r.o.Desktop != nil {
		ports = append(ports, guestPort{port: DesktopPort, sock: DesktopSock, open: []byte{DesktopOpenByte}, serve: r.o.Desktop.ServeDesktop})
	}
	for _, port := range r.o.extraPorts {
		ports = append(ports, guestPort{port: port, sock: "tools.sock", serve: splice})
	}
	listeners := make([]net.Listener, len(ports))
	for i, gp := range ports {
		gl, err := m.Listen(gp.port)
		if err != nil {
			cleanup()
			return nil, fmt.Errorf("macvm: vsock %d: %w", gp.port, err)
		}
		closers = append(closers, gl)
		listeners[i] = gl
		r.mu.Lock()
		r.st.Listeners = append(r.st.Listeners, gp.port)
		r.mu.Unlock()
	}
	if err := checkRunListeners(r.status().Listeners); err != nil {
		cleanup()
		return nil, err
	}
	for i, gp := range ports {
		p := newPairer(gp.open)
		if gp.port == AgentPort {
			p.connected = func() { r.events.publish(Event{Type: EventGuestAgent, Detail: "connected"}) }
		}
		go p.acceptGuest(listeners[i])
		hl, err := listenUnix(filepath.Join(r.sockDir, gp.sock))
		if err != nil {
			cleanup()
			return nil, err
		}
		closers = append(closers, hl, closerFunc(p.close))
		go servePaired(hl, p, gp.serve)
	}
	return cleanup, nil
}

// finish stops the VM once (gracefully first when asked) and marks the run
// stopped. A nil machine never started.
func (r *runner) finish(m Machine, req stopRequest) {
	r.stopOnce.Do(func() {
		defer close(r.stopped)
		if m == nil {
			r.setState(StateStopped, "")
			return
		}
		if m.State() != StateStopped {
			r.setState(StateStopping, "")
			if req.graceful {
				if req.timeout <= 0 {
					req.timeout = DefaultStopTimeout
				}
				if err := m.RequestStop(); err == nil {
					select {
					case <-m.Done():
					case <-time.After(req.timeout):
					}
				}
			}
			select {
			case <-m.Done():
			default:
				if err := m.Stop(); err != nil {
					r.o.Log.Error("macvm: hard stop", "err", err.Error())
				}
				select {
				case <-m.Done():
				case <-time.After(hardStopWait):
					r.setState(StateError, "the VM did not stop")
					return
				}
			}
		}
		r.setState(StateStopped, "")
	})
}

func (r *runner) setState(s MachineState, detail string) {
	r.mu.Lock()
	r.st.State = s
	if s == StateRunning && r.st.StartedAt.IsZero() {
		r.st.StartedAt = time.Now().UTC()
	}
	if detail != "" {
		r.st.Error = detail
	}
	r.mu.Unlock()
	r.events.publish(Event{Type: EventState, State: s, Detail: detail})
	r.o.Log.Info("macvm: state", "state", string(s), "detail", detail)
}

// controlHandler.

func (r *runner) status() Status {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.st
}

func (r *runner) stop(graceful bool, timeout time.Duration) error {
	select {
	case r.stopReq <- stopRequest{graceful: graceful, timeout: timeout}:
	default: // a stop is already on its way
	}
	<-r.stopped
	if st := r.status(); st.State != StateStopped {
		return fmt.Errorf("macvm: VM is %s after stop", st.State)
	}
	return nil
}

func (r *runner) desktopPassword() (string, error) {
	if r.o.DesktopPassword == nil {
		return "", ErrNoDesktopPassword
	}
	return r.o.DesktopPassword.Credential(context.Background())
}

func (r *runner) netFD(f *os.File) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.st.Network != NetWaiting {
		return errAlreadyAttached
	}
	select {
	case r.netCh <- f:
		r.st.Network = NetAttached
	default:
		return errAlreadyAttached
	}
	go r.events.publish(Event{Type: EventNetwork, Detail: NetAttached})
	return nil
}

func (r *runner) subscribe() (<-chan Event, func()) { return r.events.subscribe() }

// listenUnix listens on path (mode 0600), replacing a socket a previous
// helper left behind.
func listenUnix(path string) (*net.UnixListener, error) {
	if fi, err := os.Lstat(path); err == nil && fi.Mode()&os.ModeSocket != 0 {
		_ = os.Remove(path)
	}
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = l.Close()
		return nil, err
	}
	return l, nil
}

type closerFunc func()

func (f closerFunc) Close() error { f(); return nil }

// logWriter puts the relay's stderr into the helper log.
type logWriter struct{ l *slog.Logger }

func (w logWriter) Write(p []byte) (int, error) {
	w.l.Info("macvm: relay", "stderr", string(p))
	return len(p), nil
}
