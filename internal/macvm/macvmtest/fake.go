// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package macvmtest is a fake macvm.Engine (and FirstBooter) for tests on any
// OS. Its machines change state as asked and accept "guest" vsock
// connections that a test dials with DialGuest.
package macvmtest

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
)

// Engine is a fake macvm.Engine. Set fields before use; read counters with
// the accessor methods.
type Engine struct {
	mu sync.Mutex

	HostErr error
	URL     string
	// Image is what LoadRestoreImage returns (Path is filled in).
	Image macvm.RestoreImage
	// LoadFailures makes that many LoadRestoreImage calls fail first.
	LoadFailures int
	Thermal      macvm.ThermalState
	// InstallFunc, if set, runs as the installer; otherwise it reports 0.5
	// and 1 and returns.
	InstallFunc func(ctx context.Context, m *Machine, ipsw string, progress func(float64)) error
	// StartErr makes Machine.Start fail: every start, or with StartFailures
	// set, only the first that many.
	StartErr      error
	StartFailures int
	// IgnoreRequestStop makes machines ignore a graceful stop.
	IgnoreRequestStop bool
	// LockAux makes each machine flock its aux storage for the life of the
	// process, as Code-Hex/vz's installer VM does: nothing releases it.
	LockAux bool

	installs, loads, ids, startFails, urlCalls int
	machines                                   []*Machine
}

var _ macvm.Engine = (*Engine)(nil)

func (e *Engine) HostCheck() error { return e.HostErr }

func (e *Engine) LatestRestoreImageURL(context.Context) (string, error) {
	e.mu.Lock()
	e.urlCalls++
	e.mu.Unlock()
	if e.URL == "" {
		return "", errors.New("fake: no restore image URL")
	}
	return e.URL, nil
}

func (e *Engine) LoadRestoreImage(path string) (macvm.RestoreImage, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.loads++
	if e.LoadFailures > 0 {
		e.LoadFailures--
		return macvm.RestoreImage{}, errors.New("fake: unsupported restore image")
	}
	if _, err := os.Stat(path); err != nil {
		return macvm.RestoreImage{}, err
	}
	img := e.Image
	img.Path = path
	if img.HardwareModel == nil {
		img.HardwareModel = []byte("fake-hw-model")
	}
	return img, nil
}

func (e *Engine) NewMachineIdentifier() ([]byte, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.ids++
	return []byte(fmt.Sprintf("fake-machine-id-%d", e.ids)), nil
}

func (e *Engine) CreateAuxStorage(path string, hw []byte) error {
	return os.WriteFile(path, append([]byte("aux:"), hw...), 0o600)
}

func (e *Engine) NewMachine(p macvm.Plan) (macvm.Machine, error) {
	if err := macvm.CheckPlan(p); err != nil {
		return nil, err
	}
	m := &Machine{Plan: p, e: e, state: macvm.StateStopped, done: make(chan struct{}), listeners: map[uint32]*listener{}}
	if e.LockAux {
		f, err := lockAux(p.AuxPath)
		if err != nil {
			return nil, err
		}
		m.aux = f
	}
	e.mu.Lock()
	e.machines = append(e.machines, m)
	e.mu.Unlock()
	return m, nil
}

func (e *Engine) Install(ctx context.Context, m macvm.Machine, ipsw string, progress func(float64)) error {
	e.mu.Lock()
	e.installs++
	f := e.InstallFunc
	e.mu.Unlock()
	fm := m.(*Machine)
	if f != nil {
		return f(ctx, fm, ipsw, progress)
	}
	progress(0.5)
	progress(1)
	return nil
}

func (e *Engine) ThermalState() macvm.ThermalState {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.Thermal
}

// SetThermal changes the thermal state.
func (e *Engine) SetThermal(t macvm.ThermalState) {
	e.mu.Lock()
	e.Thermal = t
	e.mu.Unlock()
}

// URLCalls is how many times LatestRestoreImageURL ran.
func (e *Engine) URLCalls() int { e.mu.Lock(); defer e.mu.Unlock(); return e.urlCalls }

// Installs is how many times Install ran.
func (e *Engine) Installs() int { e.mu.Lock(); defer e.mu.Unlock(); return e.installs }

// Loads is how many times LoadRestoreImage ran.
func (e *Engine) Loads() int { e.mu.Lock(); defer e.mu.Unlock(); return e.loads }

// Machines are the machines built so far.
func (e *Engine) Machines() []*Machine {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]*Machine(nil), e.machines...)
}

func (e *Engine) startErr() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.StartFailures > 0 {
		if e.startFails >= e.StartFailures {
			return nil
		}
		e.startFails++
	}
	return e.StartErr
}

// Machine is a fake VM.
type Machine struct {
	Plan macvm.Plan

	e         *Engine
	mu        sync.Mutex
	state     macvm.MachineState
	done      chan struct{}
	doneOnce  sync.Once
	listeners map[uint32]*listener
	readBack  []byte
	aux       *os.File // LockAux's lock, never closed
}

func (m *Machine) Start(context.Context) error {
	if err := m.e.startErr(); err != nil {
		m.end(macvm.StateError)
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state != macvm.StateStopped {
		return fmt.Errorf("fake: start in state %s", m.state)
	}
	m.state = macvm.StateRunning
	return nil
}

func (m *Machine) RequestStop() error {
	if m.e.IgnoreRequestStop {
		return nil
	}
	m.end(macvm.StateStopped)
	return nil
}

func (m *Machine) Stop() error { m.end(macvm.StateStopped); return nil }

// Crash ends the VM as if it died.
func (m *Machine) Crash() { m.end(macvm.StateError) }

func (m *Machine) end(s macvm.MachineState) {
	m.mu.Lock()
	m.state = s
	ls := m.listeners
	m.mu.Unlock()
	for _, l := range ls {
		_ = l.Close()
	}
	m.doneOnce.Do(func() { close(m.done) })
}

func (m *Machine) State() macvm.MachineState {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state
}

func (m *Machine) Done() <-chan struct{} { return m.done }

// MachineIdentifier returns the read-back set by SetReadBackID, else the
// plan's identifier.
func (m *Machine) MachineIdentifier() ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.readBack != nil {
		return m.readBack, nil
	}
	return m.Plan.MachineIdentifier, nil
}

// SetReadBackID makes MachineIdentifier read back id, as if the framework
// had replaced the configured identifier.
func (m *Machine) SetReadBackID(id []byte) {
	m.mu.Lock()
	m.readBack = id
	m.mu.Unlock()
}

func (m *Machine) Listen(port uint32) (net.Listener, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.listeners[port]; ok {
		return nil, fmt.Errorf("fake: port %d already listening", port)
	}
	l := &listener{port: port, ch: make(chan net.Conn), closed: make(chan struct{})}
	m.listeners[port] = l
	return l, nil
}

// DialGuest connects to port as the guest would, once the host listens.
func (m *Machine) DialGuest(port uint32) (net.Conn, error) {
	m.mu.Lock()
	l := m.listeners[port]
	m.mu.Unlock()
	if l == nil {
		return nil, fmt.Errorf("fake: nothing listens on port %d", port)
	}
	host, guest := net.Pipe()
	select {
	case l.ch <- host:
		return guest, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

// Listening reports whether the host listens on port.
func (m *Machine) Listening(port uint32) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.listeners[port] != nil
}

type listener struct {
	port   uint32
	ch     chan net.Conn
	once   sync.Once
	closed chan struct{}
}

func (l *listener) Accept() (net.Conn, error) {
	select {
	case c := <-l.ch:
		return c, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *listener) Close() error { l.once.Do(func() { close(l.closed) }); return nil }

func (l *listener) Addr() net.Addr { return vsockAddr(l.port) }

type vsockAddr uint32

func (vsockAddr) Network() string  { return "vsock" }
func (a vsockAddr) String() string { return fmt.Sprintf("2:%d", uint32(a)) }

// FirstBoot is a fake macvm.FirstBooter. Boot records its arguments and
// returns a VM that runs until stop is called (or Shutdown, as if the guest
// shut itself down), and whose Screen counts the windows it opens and closes.
type FirstBoot struct {
	Unsupported error
	BootErr     error
	// ScreenErr makes Screen fail.
	ScreenErr error
	// NoScreen is ScreenReady's answer, and Screen's first, as on a Mac with no desktop session.
	NoScreen error
	// LockAux makes Boot flock the aux storage first and fail as the framework
	// does while another VM, in any process, still holds it (Engine.LockAux).
	LockAux bool
	// AuxLocked, if set, says the aux storage is still locked; Boot then fails
	// the same way.
	AuxLocked func() bool

	mu             sync.Mutex
	boots          int
	last           BootArgs
	done           chan error
	stopOnce       sync.Once
	opened, closed int
}

// BootArgs are the arguments of the last Boot.
type BootArgs struct {
	Disk, Aux                string
	HardwareModel, MachineID []byte
	CPUs                     uint
	MemoryBytes              uint64
	Username, Password       string
}

var _ macvm.FirstBooter = (*FirstBoot)(nil)

// ErrAuxLocked is the framework's refusal of a VM whose aux storage another VM
// locks, as the first boot reports it.
var ErrAuxLocked = errors.New("macvm provision: start: Invalid virtual machine configuration. Failed to lock auxiliary storage")

func (f *FirstBoot) Supported() error { return f.Unsupported }

func (f *FirstBoot) ScreenReady() error { return f.NoScreen }

func (f *FirstBoot) Boot(disk, aux string, hw, mid []byte, cpus uint, mem uint64, nic *os.File, user, pass string) (macvm.FirstBoot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.boots++
	if f.BootErr != nil {
		return macvm.FirstBoot{}, f.BootErr
	}
	if f.AuxLocked != nil && f.AuxLocked() {
		return macvm.FirstBoot{}, ErrAuxLocked
	}
	if f.LockAux {
		l, err := lockAux(aux)
		if err != nil {
			return macvm.FirstBoot{}, err
		}
		_ = l.Close()
	}
	if nic == nil {
		return macvm.FirstBoot{}, errors.New("fake: no nic")
	}
	f.last = BootArgs{disk, aux, hw, mid, cpus, mem, user, pass}
	f.done = make(chan error, 1)
	f.stopOnce = sync.Once{}
	stop := func(context.Context) error { f.Shutdown(nil); return nil }
	return macvm.FirstBoot{Done: f.done, Stop: stop, Screen: f.screen}, nil
}

func (f *FirstBoot) screen() (func(), error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := cmp.Or(f.NoScreen, f.ScreenErr); err != nil {
		return nil, err
	}
	f.opened++
	return func() { f.mu.Lock(); f.closed++; f.mu.Unlock() }, nil
}

// Windows is how many windows Screen opened, and how many of them were closed.
func (f *FirstBoot) Windows() (opened, closed int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.opened, f.closed
}

// Shutdown ends the provisioned VM with err (nil: the guest shut down).
func (f *FirstBoot) Shutdown(err error) {
	f.mu.Lock()
	d := f.done
	once := &f.stopOnce
	f.mu.Unlock()
	if d != nil {
		once.Do(func() { d <- err })
	}
}

// Boots is how many provisioning boots ran.
func (f *FirstBoot) Boots() int { f.mu.Lock(); defer f.mu.Unlock(); return f.boots }

// Last is the last Boot's arguments.
func (f *FirstBoot) Last() BootArgs { f.mu.Lock(); defer f.mu.Unlock(); return f.last }
