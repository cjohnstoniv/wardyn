// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"sync/atomic"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/maskmanifest"
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

// buildMaskManifests is the run masking manifests over the secret store's
// subject keys, and arms scope with what they hold. wardynd refuses to serve
// without them: a run it cannot prove masked is a door that would pass
// credentials through.
func buildMaskManifests(pool *pgxpool.Pool, secrets secretstore.Store, reg *secretmask.Registry, scope *maskScope) (*maskmanifest.Manifests, error) {
	keys := subjectKeysOf(secrets)
	if keys == nil {
		return nil, errors.New("refusing to start: the secret store has no per-subject keys, which the run masking manifests are sealed under")
	}
	m := maskmanifest.New(pool, keys, reg)
	scope.arm(m.Held)
	return m, nil
}
