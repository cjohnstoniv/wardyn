// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"regexp"
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
// adds stays hidden from unit admins until it is added to unitViewDataKeys. Under an allowed key, a
// sentence is left out too (unitViewCode): several rows write an error's text under reason.

// unitViewDataKeys is every data key the unit view serves. TestAuditUnitViewAllowListCensus checks each
// against docs/AUDIT-ACTIONS.md and against the keys that must never be served.
var unitViewDataKeys = []string{
	"reason", "kind", "host", "hosts", "port", "state", "count", "bytes", "approval_id", "expires_at",
	"provider", "mode", "cause", "agent", "repo", "confinement_class", "exit_code", "decision",
}

// unitViewTargetFamilies are the action families whose target the unit view serves: a run, a host, an
// approval. Every other row's target (a secret, a person, a policy) is served empty.
var unitViewTargetFamilies = []string{"run.", "egress.", "approval."}

// unitViewServesTarget reports an action, or an action family, whose target the unit view serves.
func unitViewServesTarget(action string) bool {
	return slices.ContainsFunc(unitViewTargetFamilies, func(f string) bool { return strings.HasPrefix(action, f) })
}

// unitViewCode is the shape of a string the unit view serves under an allowed key: a code, a host, a time
// or an id, with no space in it. Anything else, such as an error's text or a note a person wrote, is left
// out, because what it says is not on the list. A repository also keeps '/' and '@' (owner/name, or a
// clone address with its credentials already redacted).
var (
	unitViewCode = regexp.MustCompile(`^[A-Za-z0-9*\[][A-Za-z0-9_.:+*\[\]-]{0,127}$`)
	unitViewRepo = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:@/+-]{0,255}$`)
)

// unitView is ev as the unit view serves it: its id and envelope, its target for the families above,
// and its allow-listed data. The source address, the forwarding device and the chain hashes are left
// out; the hashes could not be checked against the projected row anyway.
func unitView(ev types.AuditEvent) types.AuditEvent {
	out := types.AuditEvent{ID: ev.ID, Time: ev.Time, RunID: ev.RunID, ActorType: ev.ActorType, Actor: ev.Actor,
		Action: ev.Action, Outcome: ev.Outcome, Data: unitViewData(ev.Data)}
	if unitViewServesTarget(ev.Action) {
		out.Target = ev.Target
	}
	return out
}

// unitViewData is raw's allow-listed keys, each with a value unitViewValue serves. Data that is not an
// object serves nothing.
func unitViewData(raw json.RawMessage) json.RawMessage {
	var in map[string]json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &in) != nil {
		return nil
	}
	kept := map[string]json.RawMessage{}
	for k, v := range in {
		if slices.Contains(unitViewDataKeys, k) && unitViewValue(k, v) {
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

// unitViewValue reports a value the unit view serves under key: a number, a boolean, null, a string of
// the unitViewCode shape, or a list of those. An object is left out (what it nests is not on the list),
// and so is a list holding one item that is not served.
func unitViewValue(key string, v json.RawMessage) bool {
	if v = bytes.TrimSpace(v); len(v) > 0 && v[0] == '[' {
		var items []json.RawMessage
		if json.Unmarshal(v, &items) != nil {
			return false
		}
		return !slices.ContainsFunc(items, func(it json.RawMessage) bool { return !unitViewScalar(key, it) })
	}
	return unitViewScalar(key, v)
}

// unitViewScalar is unitViewValue for one value that is not a list.
func unitViewScalar(key string, v json.RawMessage) bool {
	if v = bytes.TrimSpace(v); len(v) == 0 || v[0] == '{' || v[0] == '[' {
		return false
	}
	if v[0] != '"' {
		return true
	}
	var str string
	if json.Unmarshal(v, &str) != nil {
		return false
	}
	if key == "repo" {
		return unitViewRepo.MatchString(str)
	}
	return unitViewCode.MatchString(str)
}

// unitAuditRows is how rows leave for a unit admin: sealed fields opened, the member view's node-record
// and host stripping, then the unit view, except for a row of a run the caller created, which keeps the
// member view auditRowsFor gives its owner, in its order: its shared-secret references checked against
// that run's grants, then the same stripping. Run owners are read once per run for the request; a store
// that cannot list runs by id serves every row in the unit view. A row with no run is a governance row the
// feed read for scope's units, and is served through the governance view (audit_unit_governance_view.go).
func (s *Server) unitAuditRows(r *http.Request, scope *unitAuditScope) func([]types.AuditEvent, error) ([]types.AuditEvent, error) {
	ctx, me := r.Context(), principalFromRequest(r)
	in := map[uuid.UUID]bool{}
	for _, u := range scope.units {
		in[u] = true
	}
	own := map[uuid.UUID]*sharedRefs{} // runs read so far; nil for a run that is not the caller's
	return func(evs []types.AuditEvent, err error) ([]types.AuditEvent, error) {
		if evs, err = s.unsealed(ctx, evs, err); err != nil {
			return nil, err
		}
		if err := s.learnRunOwners(ctx, evs, me, own); err != nil {
			return nil, err
		}
		out := make([]types.AuditEvent, len(evs))
		for i, ev := range evs {
			if ev.RunID == nil {
				out[i] = unitGovernanceView(ev, in)
				continue
			}
			refs := own[*ev.RunID]
			row := evs[i : i+1]
			if refs != nil {
				if row, err = refs.events(row); err != nil {
					return nil, err
				}
			}
			row = withoutHostDetail(withoutNodeRecordSource(row))
			out[i] = row[0]
			if refs == nil {
				out[i] = unitView(row[0])
			}
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
