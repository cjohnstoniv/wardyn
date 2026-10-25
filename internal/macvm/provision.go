// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package macvm

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

// Provisioner runs the one first boot of a freshly installed macOS base. On
// a host with Apple's guest-provisioning start option (macOS 27 on host and
// guest) that boot hands the guest an admin account; macOS reads those
// options only on the first boot after restore, so a base gets exactly one
// call to Boot. On a host without it (macOS 26) the base boots plain and its
// setup is finished by hand. Every later boot of the base or its clones goes
// through the regular engine.
//
// The darwin implementation looks the macOS 27 API up at run time, so the
// helper still builds against the macOS 26 SDK; on an older host Supported
// fails with ErrNoProvisioning, and Boot with a username fails closed. Every
// other build gets a stub whose methods return an "unsupported" error.
type Provisioner interface {
	// Supported returns nil when this host can run the provisioning boot.
	// Otherwise the error names what is missing: it wraps ErrNoProvisioning
	// when only guest provisioning is, and ErrNoVirtualization when the
	// framework cannot run a VM at all.
	Supported() error

	// ScreenReady is nil when Boot's Screen can open the guest's window on
	// this Mac (FirstBooter.ScreenReady).
	ScreenReady() error

	// Boot starts the installed base once. With a username it turns guest
	// provisioning on: account username/password (also the full name),
	// automatic login, and Remote Login. With username "" the base boots as
	// installed, for its setup by hand. disk and aux are the base's disk
	// image and auxiliary storage; hardwareModel and machineID are the data
	// representations of its VZMacHardwareModel and VZMacMachineIdentifier.
	// nic is one end of a connected datagram socket that becomes the guest's
	// only network interface; Boot duplicates it, so the caller may close its
	// copy.
	//
	// Boot returns once the VM has started. Done delivers exactly one value
	// when the VM stops: nil when the guest shut itself down, or the
	// framework's error. Stop asks the guest to shut down, force-stops the VM
	// when ctx ends first, and returns the value Done delivered; it may be
	// called more than once and after Done has fired. Screen opens the VM's
	// window on this Mac (ServeScreens).
	Boot(disk, aux string, hardwareModel, machineID []byte, cpus uint, memoryBytes uint64, nic *os.File, username, password string) (FirstBoot, error)
}

// NewProvisioner returns this platform's Provisioner.
func NewProvisioner() Provisioner {
	if provisionPlatform != nil {
		return provisionPlatform()
	}
	return provisionStub{}
}

// provisionPlatform is set by the darwin cgo build (provision_darwin.go).
var provisionPlatform func() Provisioner

// ErrNoVirtualization is what a Mac's provisioning check wraps when the Virtualization framework reports
// no virtualization support (VZVirtualMachine.isSupported is false). Its text is the name the check gives.
var ErrNoVirtualization = errors.New("virtualization support")

// ErrNoProvisioning is what a Mac's provisioning check wraps when the Virtualization framework runs VMs
// but has no guest provisioning (macOS 26): prepare then boots the base plain and asks for its setup by
// hand at once.
var ErrNoProvisioning = errors.New("guest provisioning")

// ErrProvisionUnsupported is what a build without the Virtualization framework answers.
var ErrProvisionUnsupported = errors.New("macvm provision: guest provisioning needs a cgo build for macOS on Apple silicon")

type provisionStub struct{}

func (provisionStub) Supported() error { return ErrProvisionUnsupported }

func (provisionStub) ScreenReady() error { return ErrProvisionUnsupported }

func (provisionStub) Boot(string, string, []byte, []byte, uint, uint64, *os.File, string, string) (FirstBoot, error) {
	return FirstBoot{}, ErrProvisionUnsupported
}

// provisionVMLimitMark starts the text of a provisioning boot's start error that was the framework's VM
// limit refusal: the C side sees the NSError's domain and code, the Go side only this text.
const provisionVMLimitMark = "vm-limit: "

// classifyProvisionStart wraps ErrVMLimit around a start error that carries provisionVMLimitMark, so the
// provisioning boot is told apart as a run's or a stage-2 boot's start is.
func classifyProvisionStart(err error) error {
	if err != nil && strings.Contains(err.Error(), provisionVMLimitMark) {
		return fmt.Errorf("%w: %w", ErrVMLimit, err)
	}
	return err
}
