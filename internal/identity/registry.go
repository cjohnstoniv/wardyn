// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package identity

import (
	"crypto/ecdsa"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/component"
)

// Deps are the platform primitives an identity.Provider constructor may use.
// Heterogeneous seams keep their own typed Deps; an impl ignores fields it does
// not need (e.g. a future SPIRE provider ignores SigningKey, reads Options).
type Deps struct {
	SigningKey  *ecdsa.PrivateKey // embedded signs with this; nil => generated
	TrustDomain string
	Revocations RevocationStore // kill-switch denylist (pg-backed in production)
	Audit       audit.Recorder
	Options     map[string]string // impl-specific config, from WARDYN_IDENTITY_*
}

// Constructor builds a Provider from Deps.
type Constructor func(Deps) (Provider, error)

var reg = component.NewRegistry[Constructor]("embedded")

// Register adds an identity-provider implementation; call it from an init().
func Register(name string, c Constructor) { reg.Register(name, c) }

// Names lists the registered provider names (sorted). Default returns the default.
func Names() []string { return reg.Names() }
func Default() string { return reg.Default() }

// New constructs the identity provider selected by name (empty => default).
func New(name string, d Deps) (Provider, error) {
	ctor, _, err := reg.Resolve(name)
	if err != nil {
		return nil, err
	}
	return ctor(d)
}
