// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package secretstore

import (
	"time"

	"filippo.io/age"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/component"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/kek"
)

// Deps are the platform primitives a secretstore.Store constructor may use.
// The age identity is parsed (demo-key guard applied) by the control plane
// before construction; nil in store mode with no WARDYN_AGE_KEY set.
// PlatformIdentity is WARDYN_PLATFORM_KEY_FILE's, or nil: set, boot keys wrap
// under their own KEK rather than one the age key derives (design §2.13 c).
// External is the configured external store client, or nil: store mode
// writes to it, and every mode reads the pointer rows it names (design §2.2).
type Deps struct {
	Pool             *pgxpool.Pool
	AgeIdentity      age.Identity
	PlatformIdentity age.Identity
	// KEK is the configured key service (Vault Transit), or nil. With
	// KEKWrites (WARDYN_KEK=transit) it also wraps every data key the store
	// writes; read-only, it lets an install move back to the local key
	// (design §2.3).
	KEK       kek.KEK
	KEKWrites bool
	External  External
	// ExternalTimeout is WARDYN_SECRET_STORE_TIMEOUT, the bound on each call
	// to External (0: its 5s default); a store-mode write is bounded at 6x it.
	ExternalTimeout time.Duration
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
