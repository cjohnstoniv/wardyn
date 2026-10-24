// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package macvm

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// The fixed guest display: 1920 x 1080 pixels at 80 pixels per inch, so the
// guest picks a 1x mode and the RFB stream stays 1920 x 1080. (That macOS
// picks 1x at this density is checked on a real Mac.)
const (
	DisplayWidth  = 1920
	DisplayHeight = 1080
	DisplayPPI    = 80
)

// GuestMTU is the FileHandle attachment's MTU.
const GuestMTU = 1500

// MachineSpec is what a caller asks for. BuildPlan turns it into the Plan an
// engine builds, after checking it.
type MachineSpec struct {
	CPUs        int
	MemoryBytes uint64
	// MinCPUs and MinMemoryBytes are the base's minimum (from its manifest).
	// A spec below them is refused; the spec is never raised to meet them.
	MinCPUs        int
	MinMemoryBytes uint64

	HardwareModel     []byte
	MachineIdentifier []byte
	DiskPath          string
	AuxPath           string
	// BaseDir, when set, is the base image directory this machine must not
	// write: a disk or aux path inside it is refused.
	BaseDir string
	// Net is the guest side of the NIC's datagram socket pair. It is the
	// machine's only network device.
	Net *os.File
}

// NetworkKind names a network attachment. FileHandle is the only kind: no NAT
// or bridged attachment exists in this package, so no plan can ask for one.
type NetworkKind string

const NetworkFileHandle NetworkKind = "filehandle"

// NetworkDevice is one guest NIC in a plan.
type NetworkDevice struct {
	Kind NetworkKind
	File *os.File
	MTU  int
}

// StorageDevice is one guest block device.
type StorageDevice struct {
	Path     string
	ReadOnly bool
}

// Display is one guest display.
type Display struct {
	Width, Height, PPI int
}

// Plan is the device list an engine builds, one to one.
type Plan struct {
	CPUs              int
	MemoryBytes       uint64
	HardwareModel     []byte
	MachineIdentifier []byte
	AuxPath           string
	Storage           []StorageDevice
	Network           []NetworkDevice
	Displays          []Display
	// Vsock is true: one virtio socket device, for the guest agent.
	Vsock bool
	// Keyboard and pointer for the desktop.
	Keyboard, Pointer bool
}

const mib = 1 << 20

// BuildPlan checks s and returns the plan. The plan always has exactly one
// network device, a FileHandle attachment, and the fixed display.
func BuildPlan(s MachineSpec) (Plan, error) {
	if s.CPUs < 1 {
		return Plan{}, errors.New("macvm: a macOS VM needs at least 1 CPU")
	}
	if s.MinCPUs > 0 && s.CPUs < s.MinCPUs {
		return Plan{}, fmt.Errorf("macvm: %d CPUs is below this macOS base's minimum of %d", s.CPUs, s.MinCPUs)
	}
	if s.MemoryBytes == 0 || s.MemoryBytes%mib != 0 {
		return Plan{}, fmt.Errorf("macvm: memory must be a whole number of MiB, got %d bytes", s.MemoryBytes)
	}
	if s.MinMemoryBytes > 0 && s.MemoryBytes < s.MinMemoryBytes {
		return Plan{}, fmt.Errorf("macvm: %d MiB of memory is below this macOS base's minimum of %d MiB", s.MemoryBytes/mib, s.MinMemoryBytes/mib)
	}
	if len(s.HardwareModel) == 0 || len(s.MachineIdentifier) == 0 {
		return Plan{}, errors.New("macvm: a macOS VM needs a hardware model and a machine identifier")
	}
	if s.DiskPath == "" || s.AuxPath == "" {
		return Plan{}, errors.New("macvm: a macOS VM needs a disk and auxiliary storage")
	}
	if s.BaseDir != "" {
		for _, p := range []string{s.DiskPath, s.AuxPath} {
			if within(s.BaseDir, p) {
				return Plan{}, fmt.Errorf("macvm: %s is inside the base image; a VM boots only its own clone", p)
			}
		}
	}
	if s.Net == nil {
		return Plan{}, errors.New("macvm: a macOS VM needs its FileHandle network socket")
	}
	return Plan{
		CPUs:              s.CPUs,
		MemoryBytes:       s.MemoryBytes,
		HardwareModel:     s.HardwareModel,
		MachineIdentifier: s.MachineIdentifier,
		AuxPath:           s.AuxPath,
		Storage:           []StorageDevice{{Path: s.DiskPath}},
		Network:           []NetworkDevice{{Kind: NetworkFileHandle, File: s.Net, MTU: GuestMTU}},
		Displays:          []Display{{Width: DisplayWidth, Height: DisplayHeight, PPI: DisplayPPI}},
		Vsock:             true,
		Keyboard:          true,
		Pointer:           true,
	}, nil
}

// CheckPlan is what an engine calls before it builds anything: it refuses a
// plan that did not come out of BuildPlan's shape (one FileHandle NIC).
func CheckPlan(p Plan) error {
	if len(p.Network) != 1 || p.Network[0].Kind != NetworkFileHandle || p.Network[0].File == nil {
		return errors.New("macvm: a macOS VM has exactly one network device, a FileHandle attachment")
	}
	if len(p.Storage) != 1 || p.Storage[0].ReadOnly {
		return errors.New("macvm: a macOS VM has exactly one writable disk, its own clone")
	}
	return nil
}

// within reports whether p is dir or inside it, after cleaning both.
func within(dir, p string) bool {
	rel, err := filepath.Rel(filepath.Clean(dir), filepath.Clean(p))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
