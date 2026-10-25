// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package macvm

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
)

// Engine is the boundary between the helper and the hypervisor. vzengine
// implements it with Code-Hex/vz on darwin/arm64; macvmtest.FakeEngine
// implements it for every test above this line.
type Engine interface {
	// HostCheck says why this host cannot run macOS VMs, or nil when it can
	// (Apple silicon, a supported macOS, Virtualization available).
	HostCheck() error
	// LatestRestoreImageURL is the URL of the newest restore image this host
	// supports, without downloading it.
	LatestRestoreImageURL(ctx context.Context) (string, error)
	// LoadRestoreImage opens a downloaded restore image and fails unless the
	// host has a supported configuration for it.
	LoadRestoreImage(path string) (RestoreImage, error)
	// NewMachineIdentifier returns a fresh opaque machine identifier.
	NewMachineIdentifier() ([]byte, error)
	// CreateAuxStorage creates the auxiliary storage file for a hardware model.
	CreateAuxStorage(path string, hardwareModel []byte) error
	// NewMachine builds (but does not start) a VM from a validated plan.
	NewMachine(p Plan) (Machine, error)
	// Install runs Apple's installer on a stopped machine. progress gets the
	// fraction completed (0..1). The installer must never be paused or stopped
	// part way: Apple calls that undefined behaviour, so a cancelled install
	// leaves a disk Prepare discards.
	Install(ctx context.Context, m Machine, ipswPath string, progress func(float64)) error
	// ThermalState is the host's current thermal state.
	ThermalState() ThermalState
}

// Machine is one VM built from a Plan.
type Machine interface {
	Start(ctx context.Context) error
	// RequestStop asks the guest to shut down; it returns once the request is sent.
	RequestStop() error
	// Stop ends the VM at once.
	Stop() error
	State() MachineState
	// Done is closed once the VM has stopped for good (or failed to start).
	Done() <-chan struct{}
	// Listen accepts the guest's vsock connections to port on the host side.
	Listen(port uint32) (net.Listener, error)
	// MachineIdentifier reads back the machine identifier the VM was built
	// with (its data representation), for the per-VM identity log.
	MachineIdentifier() ([]byte, error)
}

// FirstBooter runs the one first boot of a freshly installed base: with
// Apple's guest-provisioning start option (an admin account, automatic login,
// Remote Login) where the host has it, else plain, for the base's setup by
// hand. It is the one call above the engine that Code-Hex/vz does not have;
// the MACVM-PROVISION lane's macvm.Provisioner (from NewProvisioner) has
// exactly this method set, so it plugs in unchanged, and an upstream vz start
// option (D3(c)) would sit behind the same two methods.
type FirstBooter interface {
	// Supported is nil when this host can run the provisioning boot. A host
	// whose framework has no guest provisioning (macOS 26) wraps
	// ErrNoProvisioning, and Boot still starts the base plain.
	Supported() error
	// Boot starts the base once and returns once it has started: with
	// provisioning on for username/password, or plain when username is "".
	Boot(disk, aux string, hardwareModel, machineID []byte, cpus uint, memoryBytes uint64, nic *os.File, username, password string) (FirstBoot, error)
}

// FirstBoot is one started first boot.
type FirstBoot struct {
	// Done delivers one value when the VM stops.
	Done <-chan error
	// Stop asks for a shutdown, force-stops when ctx ends, and returns what
	// Done delivered.
	Stop func(ctx context.Context) error
	// Screen shows the VM's screen in a window on this Mac, for its setup by
	// hand, until closeWindow is called. Nil where no window can open.
	Screen func() (closeWindow func(), err error)
}

// MinHostMajor is the oldest macOS a Mac can run macOS VMs on. It lives here, in no build tag, so the
// runner reads it too; vzengine's HostCheck enforces it through CheckHostVersion.
const MinHostMajor = 26

// ProvisionHostMajor is the oldest macOS with Apple's guest provisioning, named in words only: the helper
// looks the API up at run time, and a host without it sets up its base by hand.
const ProvisionHostMajor = 27

// CheckHostVersion says why a Mac running macOS version v (kern.osproductversion) cannot run macOS VMs,
// or nil when its major is MinHostMajor or later.
func CheckHostVersion(v string) error {
	major, _, _ := strings.Cut(v, ".")
	if n, err := strconv.Atoi(major); err != nil || n < MinHostMajor {
		return fmt.Errorf("macOS VMs need macOS %d or later on this Mac (it runs %s)", MinHostMajor, v)
	}
	return nil
}

// VZErrorVMLimitExceeded is the Virtualization framework's code 6 in VZErrorDomain
// (VZErrorVirtualMachineLimitExceeded): it refuses to start another macOS VM.
const VZErrorVMLimitExceeded = 6

// IsVMLimit says an NSError's domain and code are the framework's VM limit refusal.
func IsVMLimit(domain string, code int) bool {
	return domain == "VZErrorDomain" && code == VZErrorVMLimitExceeded
}

// ErrVMLimit is what an engine's Start wraps when the framework refused with its VM limit. The helper
// matches it by type, never by the framework's text.
var ErrVMLimit = errors.New("macvm: Apple's framework refused to start another macOS VM (VZErrorVirtualMachineLimitExceeded)")

// RestoreImage is what LoadRestoreImage reads from a restore image.
type RestoreImage struct {
	Path          string
	BuildVersion  string // for example "26A434"
	OSVersion     string // for example "27.0.1"
	HardwareModel []byte // opaque data representation
	// MinCPUs and MinMemoryBytes are the image's minimum guest size, from its
	// most featureful supported configuration.
	MinCPUs        int
	MinMemoryBytes uint64
}

// MachineState is a VM's state as the helper reports it.
type MachineState string

const (
	StateStopped  MachineState = "stopped"
	StateStarting MachineState = "starting"
	StateRunning  MachineState = "running"
	StatePaused   MachineState = "paused"
	StateStopping MachineState = "stopping"
	StateError    MachineState = "error"
)

// ThermalState mirrors ProcessInfo.ThermalState.
type ThermalState int

const (
	ThermalNominal ThermalState = iota
	ThermalFair
	ThermalSerious
	ThermalCritical
)

func (t ThermalState) String() string {
	switch t {
	case ThermalNominal:
		return "nominal"
	case ThermalFair:
		return "fair"
	case ThermalSerious:
		return "serious"
	case ThermalCritical:
		return "critical"
	}
	return "unknown"
}

// RefusesNewVM is the thermal gate: no new VM starts while the Mac is hot.
// A running VM is not stopped by it.
func (t ThermalState) RefusesNewVM() bool { return t >= ThermalSerious }

// ErrThermal is returned when the thermal gate refuses a start.
var ErrThermal = errors.New("macvm: this Mac is too hot to start a macOS VM; try again when it has cooled down")

// thermalGate refuses a new VM at serious or critical.
func thermalGate(e Engine) error {
	if e.ThermalState().RefusesNewVM() {
		return ErrThermal
	}
	return nil
}

// Vsock ports the guest agent dials (the guest initiates; the host listens).
const (
	AgentPort   uint32 = 1024
	DesktopPort uint32 = 1025
)
