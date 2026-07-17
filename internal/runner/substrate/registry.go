// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package substrate

import (
	"github.com/cjohnstoniv/wardyn/internal/component"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Deps are the platform primitives a Substrate constructor may use. Heterogeneous
// seams keep their own typed Deps; an impl ignores fields it does not need (a
// non-OCI VMM ignores ProxyImage-as-OCI-ref semantics, reads Options).
type Deps struct {
	// ProxyImage is the wardyn-proxy sidecar image the substrate launches beside
	// each agent (the sole egress path — L0).
	ProxyImage string
	// ConfinementRuntimes are the operator's fail-closed per-class runtime pins
	// (WARDYN_CONFINEMENT_MAP); nil = the substrate's built-in defaults.
	ConfinementRuntimes map[types.ConfinementClass]string
	// Options is impl-specific config, from WARDYN_RUNNER_* env.
	Options map[string]string
}

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
