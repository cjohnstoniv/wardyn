// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The unit view is how a unit admin is served the rows of another person's run in their subtrees. The
// run's owner may read things a unit admin must not: their private personal walls, that a restriction
// binds them and by which name, an organisation's shared-secret names, and free-text errors that carry
// paths and names. So the view is an allow list, not a list of keys to remove: a data key some later row
// adds stays hidden from unit admins until it is added to unitViewDataKeys.

// unitViewDataKeys is every data key the unit view serves. TestAuditUnitViewAllowListCensus checks each
// against docs/AUDIT-ACTIONS.md and against the keys that must never be served.
var unitViewDataKeys = []string{
	"reason", "kind", "host", "hosts", "port", "state", "count", "bytes", "approval_id", "expires_at",
	"provider", "mode", "cause", "agent", "repo", "confinement_class", "exit_code",
}

// unitViewTargetFamilies are the action families whose target the unit view serves: a run, a host, an
// approval. Every other row's target (a secret, a person, a policy) is served empty.
var unitViewTargetFamilies = []string{"run.", "egress.", "approval."}

// unitView is ev as the unit view serves it: its id and envelope, its target for the families above,
// and its allow-listed data. The source address, the forwarding device and the chain hashes are left
// out; the hashes could not be checked against the projected row anyway.
func unitView(ev types.AuditEvent) types.AuditEvent {
	out := types.AuditEvent{ID: ev.ID, Time: ev.Time, RunID: ev.RunID, ActorType: ev.ActorType, Actor: ev.Actor,
		Action: ev.Action, Outcome: ev.Outcome, Data: unitViewData(ev.Data)}
	if slices.ContainsFunc(unitViewTargetFamilies, func(f string) bool { return strings.HasPrefix(ev.Action, f) }) {
		out.Target = ev.Target
	}
	return out
}

// unitViewData is raw's allow-listed keys. A value that is an object, or a list holding one, is left out
// too: what it nests is not on the list. Data that is not an object serves nothing.
func unitViewData(raw json.RawMessage) json.RawMessage {
	var in map[string]json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &in) != nil {
		return nil
	}
	kept := map[string]json.RawMessage{}
	for k, v := range in {
		if slices.Contains(unitViewDataKeys, k) && flatJSON(v) {
			kept[k] = v
		}
	}
	if len(kept) == 0 {
		return nil
	}
	out, err := json.Marshal(kept)
	if err != nil {
		return nil
	}
	return out
}

// flatJSON reports a JSON scalar, or a list of scalars.
func flatJSON(v json.RawMessage) bool {
	v = bytes.TrimSpace(v)
	if len(v) == 0 || v[0] == '{' {
		return false
	}
	if v[0] != '[' {
		return true
	}
	var items []json.RawMessage
	if json.Unmarshal(v, &items) != nil {
		return false
	}
	for _, it := range items {
		if it = bytes.TrimSpace(it); len(it) > 0 && (it[0] == '{' || it[0] == '[') {
			return false
		}
	}
	return true
}

// unitAuditRows is how rows leave for a unit admin: sealed fields opened, the member view's host and
// node-record stripping, then the unit view, except for a row of a run the caller created, which keeps
// the member view auditRowsFor gives its owner (its shared-secret references checked against that run's
// grants). Run owners are read once per run for the request; a store that cannot list runs by id serves
// every row in the unit view.
func (s *Server) unitAuditRows(r *http.Request) func([]types.AuditEvent, error) ([]types.AuditEvent, error) {
	ctx, me := r.Context(), principalFromRequest(r)
	own := map[uuid.UUID]*sharedRefs{} // runs read so far; nil for a run that is not the caller's
	return func(evs []types.AuditEvent, err error) ([]types.AuditEvent, error) {
		if evs, err = s.unsealed(ctx, evs, err); err != nil {
			return nil, err
		}
		if err := s.learnRunOwners(ctx, evs, me, own); err != nil {
			return nil, err
		}
		evs = withoutHostDetail(withoutNodeRecordSource(evs))
		out := make([]types.AuditEvent, len(evs))
		for i, ev := range evs {
			var refs *sharedRefs
			if ev.RunID != nil {
				refs = own[*ev.RunID]
			}
			if refs == nil {
				out[i] = unitView(ev)
				continue
			}
			mine, err := refs.events(evs[i : i+1])
			if err != nil {
				return nil, err
			}
			out[i] = mine[0]
		}
		return out, nil
	}
}

// learnRunOwners adds to own each run of evs it has not read yet: a member view for a run me created,
// nil for any other.
func (s *Server) learnRunOwners(ctx context.Context, evs []types.AuditEvent, me string, own map[uuid.UUID]*sharedRefs) error {
	unread := map[uuid.UUID]bool{}
	for _, ev := range evs {
		if ev.RunID == nil {
			continue
		}
		if _, seen := own[*ev.RunID]; !seen {
			unread[*ev.RunID] = true
		}
	}
	reader, ok := s.cfg.Store.(store.RunSweepReader)
	if len(unread) == 0 || !ok {
		return nil
	}
	runs, err := reader.GetRunsByID(ctx, slices.Collect(maps.Keys(unread)))
	if err != nil {
		return err
	}
	for id := range unread {
		own[id] = nil
	}
	for _, run := range runs {
		if run.CreatedBy == me {
			id := run.ID
			own[id] = &sharedRefs{grants: func() ([]types.CredentialGrant, error) { return s.cfg.Store.ListGrantsByRun(ctx, id) }}
		}
	}
	return nil
}
