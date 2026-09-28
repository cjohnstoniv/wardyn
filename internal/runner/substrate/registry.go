// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package substrate

import (
	"github.com/cjohnstoniv/wardyn/internal/component"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Deps are the platform primitives a Substrate constructor may use. Heterogeneous
// seams keep their own typed Deps; an impl ignores fields it does not need (a
// non-OCI VMM ignores ProxyImage-as-OCI-ref semantics) and reads its own
// impl-specific config from the env in its constructor — see the docker impl's
// WARDYN_RECORDING_MOUNT / WARDYN_INTERNAL_NETWORK reads in register.go.
type Deps struct {
	// ProxyImage is the wardyn-proxy sidecar image the substrate launches beside
	// each agent (the sole egress path — L0).
	ProxyImage string
	// DriveProbeImage is the OCI image the docker substrate's host_path
	// drive-readability probe (#165) runs in. Empty = the substrate's own
	// pinned default; a non-OCI substrate with no such probe ignores it.
	DriveProbeImage string
	// ConfinementRuntimes are the operator's fail-closed per-class runtime pins
	// (WARDYN_CONFINEMENT_MAP); nil = the substrate's built-in defaults.
	ConfinementRuntimes map[types.ConfinementClass]string
	// UserDriveHostRoots is the deployment's WARDYN_USER_DRIVE_HOST_ROOTS
	// ceiling over host_path user drives, parsed at boot
	// (runner.ParseUserDriveHostRoots) and passed here rather than re-read from
	// the env by each substrate: the flag has an env pair, so a substrate
	// reading os.Getenv would silently ignore an operator who set the FLAG, and
	// a ceiling one half of the process disagrees about is not a ceiling.
	//
	// nil (the default) refuses every host_path drive. A substrate with no
	// host-path drive backend at all (Kubernetes) ignores it.
	UserDriveHostRoots []string
	// Record is whether a substrate's driver should wrap Exec's argv with
	// wardyn-rec (PTY session recording): cmd/wardynd's buildRunnerFromFlags
	// sets this from the resolved recording-store selection
	// (WARDYN_RECORDING_STORE via RecordEnabled) rather than a substrate
	// hardcoding it — see #1113 (an install with recording off still got a
	// brokered:recording deny row because register.go ignored this and
	// always wrapped). Each substrate's register.go must carry this through
	// to its own Config.Record verbatim, never re-defaulting it to true.
	Record bool
}

// RecordEnabled maps a recording-store selection (WARDYN_RECORDING_STORE:
// "pg", "fs", or "off") to Deps.Record: every selection records except "off",
// which recording.New treats as no store at all (its "disabled" contract).
// cmd/wardynd calls this to build Deps.Record; a substrate's register_test.go
// calls it directly so its Config-building tests derive Record from a store
// name instead of a literal bool.
func RecordEnabled(store string) bool { return store != "off" }

// Constructor builds a Substrate from Deps.
type Constructor func(Deps) (Substrate, error)

var reg = component.NewRegistry[Constructor]("docker")

// Register adds a confinement-substrate implementation; call it from an init().
// The OCI/Docker substrate registers itself only under `-tags docker`, so a
// tagless control plane fails closed at Resolve ("not registered") rather than
// carrying target-specific code (the parity rule).
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
