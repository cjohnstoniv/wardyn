// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/maskmanifest"
	"github.com/cjohnstoniv/wardyn/internal/maskstore"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/subjectkey"
)

// maskScopeValue is the mask_scope an audit row of an uncovered run carries:
// only the process-wide corpus masked it (docs/AUDIT-ACTIONS.md).
const maskScopeValue = "globals_only"

// maskScope tells the masking recorder whether this process holds a run's
// complete masking manifest. The recorder chain is built before the secret
// store, whose subject keys the manifests need, so the answer is armed late;
// until then no row is labelled. A nil *maskScope labels nothing.
type maskScope struct {
	held atomic.Pointer[func(uuid.UUID) bool]
}

func (m *maskScope) arm(held func(uuid.UUID) bool) {
	if m != nil {
		m.held.Store(&held)
	}
}

// uncovered reports whether runID's manifest is not held complete here.
func (m *maskScope) uncovered(runID uuid.UUID) bool {
	if m == nil {
		return false
	}
	held := m.held.Load()
	return held != nil && !(*held)(runID)
}

// withMaskScope is data with "mask_scope":"globals_only" added when data is
// empty or a JSON object that does not carry the key. Any other shape is left
// as it is: the label never makes a row unreadable.
func withMaskScope(data json.RawMessage) json.RawMessage {
	label := []byte(`"mask_scope":"` + maskScopeValue + `"`)
	t := bytes.TrimSpace(data)
	switch {
	case bytes.Contains(t, []byte(`"mask_scope"`)):
		return data
	case len(t) == 0 || bytes.Equal(t, []byte("null")):
		return append(append([]byte{'{'}, label...), '}')
	case t[0] == '{' && json.Valid(t):
		if len(bytes.TrimSpace(t[1:len(t)-1])) == 0 {
			return append(append([]byte{'{'}, label...), '}')
		}
		return append(append(append([]byte{'{'}, label...), ','), t[1:]...)
	}
	return data
}

// subjectKeysOf finds the per-subject key manager of the secret store, through
// the audit wrapper that does not forward it.
func subjectKeysOf(s secretstore.Store) *subjectkey.Manager {
	for s != nil {
		if p, ok := s.(interface{ SubjectKeys() *subjectkey.Manager }); ok {
			return p.SubjectKeys()
		}
		u, ok := s.(interface{ Unwrap() secretstore.Store })
		if !ok {
			return nil
		}
		s = u.Unwrap()
	}
	return nil
}

// principalKeyRootOf is the secret store's PrincipalKeyRootReady, through the
// audit wrapper that does not forward it; nil for a store without one.
func principalKeyRootOf(s secretstore.Store) error {
	for s != nil {
		if p, ok := s.(interface{ PrincipalKeyRootReady() error }); ok {
			return p.PrincipalKeyRootReady()
		}
		u, ok := s.(interface{ Unwrap() secretstore.Store })
		if !ok {
			return nil
		}
		s = u.Unwrap()
	}
	return nil
}

// buildMaskManifests is the run masking manifests over the secret store's
// subject keys, and arms scope with what they hold. wardynd refuses to serve
// without them: a run it cannot prove masked is a door that would pass
// credentials through.
//
// It also makes the Postgres masking registry reg's only Backend, whichever
// number of replicas is running, and reads the committed corpus once before it
// returns: a registry that starts empty would mask nothing until its first read.
// The caller starts the store's background reads (Store.Start) on the process
// context.
func buildMaskManifests(ctx context.Context, pool *pgxpool.Pool, secrets secretstore.Store, reg *secretmask.Registry, scope *maskScope) (*maskmanifest.Manifests, *maskstore.Store, error) {
	keys := subjectKeysOf(secrets)
	if keys == nil {
		return nil, nil, errors.New("refusing to start: the secret store has no per-subject keys, which the run masking manifests are sealed under")
	}
	if err := principalKeyRootOf(secrets); err != nil {
		return nil, nil, fmt.Errorf("refusing to start: the per-subject keys the run masking manifests are sealed under have no key to wrap under: %w. Set WARDYN_AGE_KEY (wardynd -gen-age-key) or a WARDYN_KEK key service (transit or azurekv); an external secret store alone does not wrap them", err)
	}
	m := maskmanifest.New(pool, keys, reg)
	scope.arm(m.Held)
	store := maskstore.New(pool, keys, reg)
	if err := store.Fresh(ctx, time.Now()); err != nil {
		return nil, nil, fmt.Errorf("refusing to start: the shared masking registry could not be read: %w", err)
	}
	return m, store, nil
}
