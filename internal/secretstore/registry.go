// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package secretstore

import (
	"filippo.io/age"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/component"
)

// Deps are the platform primitives a secretstore.Store constructor may use. The
// age identity is parsed (and the demo-key guard applied) by the control plane
// before construction; an alternate (OpenBao/Vault/KMS) reads its address/role
// from Options and ignores the pg-specific fields.
type Deps struct {
	Pool        *pgxpool.Pool
	AgeIdentity age.Identity
	Options     map[string]string // impl-specific config, from WARDYN_SECRET_STORE_*
}

// Constructor builds a Store from Deps.
type Constructor func(Deps) (Store, error)

var reg = component.NewRegistry[Constructor]("pg")

// Register adds a secret-store implementation; call it from an init().
func Register(name string, c Constructor) { reg.Register(name, c) }

// Names returns the registered store names (for /healthz and error messages).
func Names() []string { return reg.Names() }

// New constructs the secret store selected by name (empty => default).
func New(name string, d Deps) (Store, error) {
	ctor, _, err := reg.Resolve(name)
	if err != nil {
		return nil, err
	}
	return ctor(d)
}
