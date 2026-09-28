// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package substrate

import (
	"github.com/cjohnstoniv/wardyn/internal/component"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Deps are the platform primitives a Substrate constructor may use. Heterogeneous seams
// keep their own typed Deps; an impl ignores fields it doesn't need and reads its own
// impl-specific config from the env in its constructor.
type Deps struct {
	// ProxyImage is the wardyn-proxy sidecar image the substrate launches beside each agent (the sole egress path).
	ProxyImage string
	// DriveProbeImage is the OCI image the docker substrate's host_path drive-readability
	// probe runs in; empty means the substrate's own pinned default (a non-OCI substrate ignores it).
	DriveProbeImage string
	// ConfinementRuntimes are the operator's fail-closed per-class runtime pins (WARDYN_CONFINEMENT_MAP); nil = built-in defaults.
	ConfinementRuntimes map[types.ConfinementClass]string
	// UserDriveHostRoots is WARDYN_USER_DRIVE_HOST_ROOTS, parsed once at boot and passed here
	// so every substrate agrees on one host_path ceiling instead of re-reading the env itself.
	// nil (default) refuses every host_path drive; Kubernetes (no host-path backend) ignores it.
	UserDriveHostRoots []string
	// Record is whether a substrate should wrap Exec's argv with wardyn-rec (PTY recording),
	// set by cmd/wardynd from the resolved recording-store selection (RecordEnabled) rather
	// than hardcoded; each substrate must carry it through to Config.Record verbatim, never defaulting it to true.
	Record bool
}

// RecordEnabled maps a recording-store selection (WARDYN_RECORDING_STORE: "pg", "fs", or
// "off") to Deps.Record: every selection records except "off". cmd/wardynd and each
// substrate's register_test.go both call this rather than hardcoding a bool.
func RecordEnabled(store string) bool { return store != "off" }

// Constructor builds a Substrate from Deps.
type Constructor func(Deps) (Substrate, error)

var reg = component.NewRegistry[Constructor]("docker")

// Register adds a confinement-substrate implementation, called from an init(). The
// OCI/Docker substrate registers only under `-tags docker`, so a tagless build fails closed at Resolve.
func Register(name string, c Constructor) { reg.Register(name, c) }

// New constructs the substrate selected by name (empty => default).
func New(name string, d Deps) (Substrate, error) {
	ctor, _, err := reg.Resolve(name)
	if err != nil {
		return nil, err
	}
	return ctor(d)
}

// Names returns the registered substrate names (for /healthz and error messages).
func Names() []string { return reg.Names() }
