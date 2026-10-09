// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"encoding/json"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// What an organisation's shared secret is called is the operator's. A grant
// whose scope is marked `shared` reads the operator's row and only it
// (injectionGrantRead), so the name beside that mark is never the run owner's
// own — and a run's owner reads their run's audit rows and its grant list.
//
// The rule is applied where a person below the security tier is served, and
// to every row, because that is the one place that covers a row already
// written and a row some later code writes: dispatch's run.policy.resolve and
// the broker's credential.mint keep the grant's scope whole, as the security
// tier and the sinks read it, and an append-only row cannot be corrected
// afterwards.

// sharedRefs removes shared-secret references from what one reader is served.
// Two shapes name one:
//
//   - a JSON object marked `"shared": true` — a grant scope, wherever a row
//     puts it (data.scope, eligible_grants[].scope);
//   - a JSON object that names a grant by `grant_id` beside the secret it
//     reads (run.injection.drop), which carries no mark of its own. Whether
//     that grant is shared is answered by the run's own grant list.
//
// The secret-naming keys (memberSecretScopeKeys) are dropped from either; the
// host, the mark and the grant id stay.
type sharedRefs struct {
	// grants reads the run's grant list. It is called at most once, and only
	// when a row names a grant beside a secret; nil answers no such row, which
	// is right for a grant's own scope, which carries its own mark.
	grants func() ([]types.CredentialGrant, error)
	shared map[string]bool // ids of the run's shared grants; nil until read
}

func (sr *sharedRefs) sharedGrant(id string) (bool, error) {
	if sr.grants == nil {
		return false, nil
	}
	if sr.shared == nil {
		grants, err := sr.grants()
		if err != nil {
			return false, err
		}
		sr.shared = map[string]bool{}
		for _, g := range grants {
			if apiKeyScopeShared(g.Spec.Scope) {
				sr.shared[g.ID.String()] = true
			}
		}
	}
	return sr.shared[id], nil
}

// strip returns raw without the references. raw is returned as it came, byte
// for byte, when it holds none. A value that does not decode is withheld
// whole: it cannot be shown to carry no name.
func (sr *sharedRefs) strip(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 {
		return raw, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber() // a number is written back as it was read
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, nil
	}
	changed, err := sr.walk(v)
	if err != nil {
		return nil, err
	}
	if !changed {
		return raw, nil
	}
	return json.Marshal(v)
}

// walk edits a decoded JSON value in place and reports whether it removed
// anything.
func (sr *sharedRefs) walk(v any) (changed bool, err error) {
	var children []any
	switch x := v.(type) {
	case map[string]any:
		if changed, err = sr.stripObject(x); err != nil {
			return false, err
		}
		for _, child := range x {
			children = append(children, child)
		}
	case []any:
		children = x
	}
	for _, child := range children {
		c, err := sr.walk(child)
		if err != nil {
			return false, err
		}
		changed = changed || c
	}
	return changed, nil
}

func (sr *sharedRefs) stripObject(obj map[string]any) (changed bool, err error) {
	var named []string
	for _, k := range memberSecretScopeKeys {
		if _, ok := obj[k]; ok {
			named = append(named, k)
		}
	}
	if len(named) == 0 {
		return false, nil
	}
	shared, _ := obj["shared"].(bool)
	if id, ok := obj["grant_id"].(string); ok && !shared {
		if shared, err = sr.sharedGrant(id); err != nil {
			return false, err
		}
	}
	if !shared {
		return false, nil
	}
	for _, k := range named {
		delete(obj, k)
	}
	return true, nil
}

// events is evs as the reader is served them. evs is not edited.
func (sr *sharedRefs) events(evs []types.AuditEvent) ([]types.AuditEvent, error) {
	if len(evs) == 0 {
		return evs, nil // an empty read is answered as the store answered it
	}
	out := make([]types.AuditEvent, len(evs))
	for i, ev := range evs {
		data, err := sr.strip(ev.Data)
		if err != nil {
			return nil, err
		}
		ev.Data = data
		out[i] = ev
	}
	return out, nil
}

// withoutSharedSecretRefs is strip for a value that carries its own mark: a
// grant's scope.
func withoutSharedSecretRefs(raw json.RawMessage) json.RawMessage {
	out, _ := (&sharedRefs{}).strip(raw) // no grant list is read, so nothing fails
	return out
}
