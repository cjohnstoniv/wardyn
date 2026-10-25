// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package macvm

import (
	"context"
	"errors"
	"os"
)

// Provisioner runs the one provisioning first boot of a freshly installed
// macOS base: the boot that hands the guest an admin account through Apple's
// guest-provisioning start option (macOS 27 on host and guest). macOS reads
// those options only on the first boot after restore, so a base gets exactly
// one call to Boot. Every later boot of the base or its clones goes through
// the regular engine.
//
// The darwin implementation looks the macOS 27 API up at run time, so the
// helper still builds against the macOS 26 SDK; on an older host Supported
// and Boot fail closed with an error that names the missing class or
// selector. Every other build gets a stub whose methods return an
// "unsupported" error.
type Provisioner interface {
	// Supported returns nil when this host can run the provisioning boot.
	// Otherwise the error names what is missing.
	Supported() error

	// Boot starts the installed base once with guest provisioning on:
	// account username/password (also the full name), automatic login, and
	// Remote Login. disk and aux are the base's disk image and auxiliary
	// storage; hardwareModel and machineID are the data representations of
	// its VZMacHardwareModel and VZMacMachineIdentifier. nic is one end of a
	// connected datagram socket that becomes the guest's only network
	// interface; Boot duplicates it, so the caller may close its copy.
	//
	// Boot returns once the VM has started. done delivers exactly one value
	// when the VM stops: nil when the guest shut itself down, or the
	// framework's error. stop asks the guest to shut down, force-stops the VM
	// when ctx ends first, and returns the value done delivered; it may be
	// called more than once and after done has fired.
	Boot(disk, aux string, hardwareModel, machineID []byte, cpus uint, memoryBytes uint64, nic *os.File, username, password string) (done <-chan error, stop func(ctx context.Context) error, err error)
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

// ErrProvisionUnsupported is what a build without the Virtualization framework answers.
var ErrProvisionUnsupported = errors.New("macvm provision: guest provisioning needs a cgo build for macOS on Apple silicon")

type provisionStub struct{}

func (provisionStub) Supported() error { return ErrProvisionUnsupported }

func (provisionStub) Boot(string, string, []byte, []byte, uint, uint64, *os.File, string, string) (<-chan error, func(context.Context) error, error) {
	return nil, nil, ErrProvisionUnsupported
}
