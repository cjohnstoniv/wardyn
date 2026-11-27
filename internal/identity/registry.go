// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package identity

import (
	"context"
	"crypto/ecdsa"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/component"
)

// Deps are the platform primitives an identity.Provider constructor may use.
// An impl ignores fields it does not need and reads its own config from the
// env in its constructor.
type Deps struct {
	SigningKey  *ecdsa.PrivateKey // embedded signs with this; nil => generated
	TrustDomain string
	Revocations RevocationStore // kill-switch denylist (pg-backed in production)
	Audit       audit.Recorder
	// Bindings stores each run token's delegation chain by jti, for a provider whose token cannot carry it
	// (spire); embedded ignores it.
	Bindings TokenBindingStore
	// BootCtx bounds a constructor's waits; nil means context.Background(). embedded ignores it.
	BootCtx context.Context
}

// Constructor builds a Provider from Deps.
type Constructor func(Deps) (Provider, error)

var reg = component.NewRegistry[Constructor]("embedded")

// Register adds an identity-provider implementation; call it from an init().
func Register(name string, c Constructor) { reg.Register(name, c) }

// Names returns the registered provider names (for /healthz and error messages).
func Names() []string { return reg.Names() }

// New constructs the identity provider selected by name (empty => default).
func New(name string, d Deps) (Provider, error) {
	ctor, _, err := reg.Resolve(name)
	if err != nil {
		return nil, err
	}
	return ctor(d)
}
