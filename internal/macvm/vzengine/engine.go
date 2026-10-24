// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build darwin && arm64 && cgo

package vzengine

/*
#cgo CFLAGS: -x objective-c -fobjc-arc
#cgo LDFLAGS: -framework Foundation
#import <Foundation/Foundation.h>

static int macvmThermalState(void) {
	return (int)[[NSProcessInfo processInfo] thermalState];
}
*/
import "C"

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Code-Hex/vz/v3"
	"golang.org/x/sys/unix"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
)

// MinHostMajor is the oldest macOS this engine runs on (provisioning needs 27).
const MinHostMajor = 27

// Engine is the Virtualization-framework engine.
type Engine struct{}

// New returns the engine.
func New() *Engine { return &Engine{} }

var _ macvm.Engine = (*Engine)(nil)

func (*Engine) HostCheck() error {
	v, err := unix.Sysctl("kern.osproductversion")
	if err != nil {
		return fmt.Errorf("vzengine: macOS version: %w", err)
	}
	major, _, _ := strings.Cut(v, ".")
	if n, err := strconv.Atoi(major); err != nil || n < MinHostMajor {
		return fmt.Errorf("macOS VMs need macOS %d or later on this Mac (it runs %s)", MinHostMajor, v)
	}
	return nil
}

func (*Engine) LatestRestoreImageURL(context.Context) (string, error) {
	return vz.GetLatestSupportedMacOSRestoreImageURL()
}

func (*Engine) LoadRestoreImage(path string) (macvm.RestoreImage, error) {
	img, err := vz.LoadMacOSRestoreImageFromPath(path)
	if err != nil {
		return macvm.RestoreImage{}, err
	}
	req := img.MostFeaturefulSupportedConfiguration()
	if req == nil {
		return macvm.RestoreImage{}, errors.New("this Mac supports no configuration of this restore image")
	}
	hw := req.HardwareModel()
	if hw == nil || !hw.Supported() {
		return macvm.RestoreImage{}, errors.New("this Mac does not support the restore image's hardware model")
	}
	return macvm.RestoreImage{
		Path: path, BuildVersion: img.BuildVersion(), OSVersion: img.OperatingSystemVersion().String(),
		HardwareModel: hw.DataRepresentation(), MinCPUs: int(req.MinimumSupportedCPUCount()),
		MinMemoryBytes: req.MinimumSupportedMemorySize(),
	}, nil
}

func (*Engine) NewMachineIdentifier() ([]byte, error) {
	id, err := vz.NewMacMachineIdentifier()
	if err != nil {
		return nil, err
	}
	return id.DataRepresentation(), nil
}

func (*Engine) CreateAuxStorage(path string, hardwareModel []byte) error {
	hw, err := vz.NewMacHardwareModelWithData(hardwareModel)
	if err != nil {
		return err
	}
	_, err = vz.NewMacAuxiliaryStorage(path, vz.WithCreatingMacAuxiliaryStorage(hw))
	return err
}

func (*Engine) ThermalState() macvm.ThermalState {
	// NSProcessInfoThermalState: 0 nominal, 1 fair, 2 serious, 3 critical.
	return macvm.ThermalState(C.macvmThermalState())
}

// NewMachine builds a VM from a checked plan: one FileHandle NIC, one
// writable disk (the clone), the fixed display, vsock, keyboard and trackpad.
// No NAT or bridged attachment is ever constructed.
func (*Engine) NewMachine(p macvm.Plan) (macvm.Machine, error) {
	if err := macvm.CheckPlan(p); err != nil {
		return nil, err
	}
	hw, err := vz.NewMacHardwareModelWithData(p.HardwareModel)
	if err != nil {
		return nil, fmt.Errorf("hardware model: %w", err)
	}
	id, err := vz.NewMacMachineIdentifierWithData(p.MachineIdentifier)
	if err != nil {
		return nil, fmt.Errorf("machine identifier: %w", err)
	}
	aux, err := vz.NewMacAuxiliaryStorage(p.AuxPath)
	if err != nil {
		return nil, fmt.Errorf("aux storage: %w", err)
	}
	platform, err := vz.NewMacPlatformConfiguration(
		vz.WithMacHardwareModel(hw), vz.WithMacMachineIdentifier(id), vz.WithMacAuxiliaryStorage(aux))
	if err != nil {
		return nil, err
	}
	boot, err := vz.NewMacOSBootLoader()
	if err != nil {
		return nil, err
	}
	cfg, err := vz.NewVirtualMachineConfiguration(boot, uint(p.CPUs), p.MemoryBytes)
	if err != nil {
		return nil, err
	}
	cfg.SetPlatformVirtualMachineConfiguration(platform)

	gd, err := vz.NewMacGraphicsDeviceConfiguration()
	if err != nil {
		return nil, err
	}
	var displays []*vz.MacGraphicsDisplayConfiguration
	for _, d := range p.Displays {
		dc, err := vz.NewMacGraphicsDisplayConfiguration(int64(d.Width), int64(d.Height), int64(d.PPI))
		if err != nil {
			return nil, err
		}
		displays = append(displays, dc)
	}
	gd.SetDisplays(displays...)
	cfg.SetGraphicsDevicesVirtualMachineConfiguration([]vz.GraphicsDeviceConfiguration{gd})

	var storage []vz.StorageDeviceConfiguration
	for _, s := range p.Storage {
		att, err := vz.NewDiskImageStorageDeviceAttachment(s.Path, s.ReadOnly)
		if err != nil {
			return nil, fmt.Errorf("disk %s: %w", s.Path, err)
		}
		blk, err := vz.NewVirtioBlockDeviceConfiguration(att)
		if err != nil {
			return nil, err
		}
		storage = append(storage, blk)
	}
	cfg.SetStorageDevicesVirtualMachineConfiguration(storage)

	var nics []*vz.VirtioNetworkDeviceConfiguration
	for _, n := range p.Network {
		if n.Kind != macvm.NetworkFileHandle {
			return nil, errors.New("vzengine: only FileHandle network attachments exist")
		}
		att, err := vz.NewFileHandleNetworkDeviceAttachment(n.File)
		if err != nil {
			return nil, err
		}
		if n.MTU > 0 {
			if err := att.SetMaximumTransmissionUnit(n.MTU); err != nil {
				return nil, err
			}
		}
		nic, err := vz.NewVirtioNetworkDeviceConfiguration(att)
		if err != nil {
			return nil, err
		}
		mac, err := vz.NewRandomLocallyAdministeredMACAddress()
		if err != nil {
			return nil, err
		}
		nic.SetMACAddress(mac)
		nics = append(nics, nic)
	}
	cfg.SetNetworkDevicesVirtualMachineConfiguration(nics)

	if p.Vsock {
		sock, err := vz.NewVirtioSocketDeviceConfiguration()
		if err != nil {
			return nil, err
		}
		cfg.SetSocketDevicesVirtualMachineConfiguration([]vz.SocketDeviceConfiguration{sock})
	}
	if p.Keyboard {
		kb, err := vz.NewMacKeyboardConfiguration()
		if err != nil {
			return nil, err
		}
		cfg.SetKeyboardsVirtualMachineConfiguration([]vz.KeyboardConfiguration{kb})
	}
	if p.Pointer {
		tp, err := vz.NewMacTrackpadConfiguration()
		if err != nil {
			return nil, err
		}
		cfg.SetPointingDevicesVirtualMachineConfiguration([]vz.PointingDeviceConfiguration{tp})
	}
	ent, err := vz.NewVirtioEntropyDeviceConfiguration()
	if err != nil {
		return nil, err
	}
	cfg.SetEntropyDevicesVirtualMachineConfiguration([]*vz.VirtioEntropyDeviceConfiguration{ent})

	if ok, err := cfg.Validate(); !ok || err != nil {
		return nil, fmt.Errorf("vzengine: configuration: %v", err)
	}
	vm, err := vz.NewVirtualMachine(cfg)
	if err != nil {
		return nil, err
	}
	m := &machine{vm: vm, platform: platform, done: make(chan struct{})}
	go m.watch()
	return m, nil
}

func (*Engine) Install(ctx context.Context, m macvm.Machine, ipswPath string, progress func(float64)) error {
	vm, ok := m.(*machine)
	if !ok {
		return errors.New("vzengine: not a vz machine")
	}
	inst, err := vz.NewMacOSInstaller(vm.vm, ipswPath)
	if err != nil {
		return err
	}
	errc := make(chan error, 1)
	go func() { errc <- inst.Install(ctx) }()
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case err := <-errc:
			if err == nil {
				progress(1)
			}
			return err
		case <-t.C:
			progress(inst.FractionCompleted())
		}
	}
}

// machine is one vz VM.
type machine struct {
	vm       *vz.VirtualMachine
	platform *vz.MacPlatformConfiguration
	started  bool
	mu       sync.Mutex
	done     chan struct{}
	once     sync.Once
}

// watch closes done once the VM has started and then stopped or failed.
func (m *machine) watch() {
	for s := range m.vm.StateChangedNotify() {
		switch s {
		case vz.VirtualMachineStateRunning, vz.VirtualMachineStateStarting:
			m.mu.Lock()
			m.started = true
			m.mu.Unlock()
		case vz.VirtualMachineStateStopped, vz.VirtualMachineStateError:
			m.mu.Lock()
			started := m.started
			m.mu.Unlock()
			if started || s == vz.VirtualMachineStateError {
				m.once.Do(func() { close(m.done) })
				return
			}
		}
	}
}

func (m *machine) Start(context.Context) error {
	if err := m.vm.Start(); err != nil {
		m.once.Do(func() { close(m.done) })
		return err
	}
	return nil
}

func (m *machine) RequestStop() error {
	_, err := m.vm.RequestStop()
	return err
}

func (m *machine) Stop() error {
	if !m.vm.CanStop() {
		return nil
	}
	return m.vm.Stop()
}

func (m *machine) State() macvm.MachineState {
	switch m.vm.State() {
	case vz.VirtualMachineStateStopped:
		return macvm.StateStopped
	case vz.VirtualMachineStateRunning:
		return macvm.StateRunning
	case vz.VirtualMachineStatePaused, vz.VirtualMachineStatePausing, vz.VirtualMachineStateResuming:
		return macvm.StatePaused
	case vz.VirtualMachineStateStarting, vz.VirtualMachineStateRestoring:
		return macvm.StateStarting
	case vz.VirtualMachineStateStopping, vz.VirtualMachineStateSaving:
		return macvm.StateStopping
	}
	return macvm.StateError
}

func (m *machine) Done() <-chan struct{} { return m.done }

func (m *machine) Listen(port uint32) (net.Listener, error) {
	socks := m.vm.SocketDevices()
	if len(socks) == 0 {
		return nil, errors.New("vzengine: this VM has no vsock device")
	}
	return socks[0].Listen(port)
}

// MachineIdentifier reads the identifier back from the platform configuration
// the VM was built from. That is the helper's own object (vz exposes no
// running VM's configuration), so it matches the configured one by
// construction and proves nothing about what the guest was given.
func (m *machine) MachineIdentifier() ([]byte, error) {
	id := m.platform.MachineIdentifier()
	if id == nil {
		return nil, errors.New("vzengine: no machine identifier")
	}
	return id.DataRepresentation(), nil
}
