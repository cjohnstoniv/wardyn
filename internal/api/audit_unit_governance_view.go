// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"encoding/json"
	"regexp"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The governance view is how a unit admin is served the governance rows about their units: the unit,
// membership, unit-admin, deciders, envelope and request rows that carry no run (store.unitGovernanceActions
// lists them, and only the feed reads them: a unit admin has no export). Like the run view it is an
// allow list. It also shows less than the row holds: a security admin appears as a kind, never by name, and
// so does a group decider, because who they are is not a unit admin's to read.

// unitGovernanceDataKeys is every data key the governance view serves. TestAuditUnitGovernanceAllowListCensus
// checks each against docs/AUDIT-ACTIONS.md and against the keys that must never be served.
var unitGovernanceDataKeys = []string{
	"unit_id", "from_unit_id", "parent_id", "units", "subject_type", "subject", "priority", "home",
	"unit_scope", "changed", "added", "removed", "duration", "expires_at", "expired_at", "change_id",
	"withdrawn", "image", "grant_id",
}

// unitGovernanceSubject is the shape of a subject, or of a group name in added and removed: a person's
// address, a group or a user type. A line break or a control character is not in it.
var unitGovernanceSubject = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:@+/ -]{0,319}$`)

// unitGovernanceImage is an image reference by digest, with no credentials in it.
var unitGovernanceImage = regexp.MustCompile(`^[^@\s]+@sha256:[0-9a-f]{64}$`)

// The kinds a unit admin reads in place of a name they may not see.
const (
	unitGovernanceActorSecurity  = "security"
	unitGovernanceActorUnitAdmin = "unit_admin"
	unitGovernanceActorGroup     = "group"
	unitGovernanceGroupScope     = "group:"
)

// unitGovernanceOwnIDTarget reports an action whose target is the row's own membership or grant id: a
// membership row, an elevation approval or expiry, an image approval.
func unitGovernanceOwnIDTarget(action string) bool {
	switch action {
	case "governance.elevation.approve", "governance.elevation.expire", "governance.image_request.approve":
		return true
	}
	return strings.HasPrefix(action, "governance.membership.")
}

// unitGovernanceRequest is whether action is a person's own request, whose author the reader may see.
func unitGovernanceRequest(action string) bool {
	return action == "governance.elevation.request" || action == "governance.image_request.request"
}

// unitGovernanceWithdrawn is whether the row is a person withdrawing their own request: the writer marks a
// withdrawal and gives it no unit_scope, so without this it would read as a security admin's.
func unitGovernanceWithdrawn(data map[string]json.RawMessage) bool {
	var b bool
	raw, ok := data["withdrawn"]
	return ok && json.Unmarshal(raw, &b) == nil && b
}

// unitGovernanceView is ev as a unit admin whose units are in reads it: its id and envelope, a target and an
// actor only as the rules below allow, and its allow-listed data. The source address, the forwarding device
// and the chain hashes are left out, as the run view leaves them.
func unitGovernanceView(ev types.AuditEvent, in map[uuid.UUID]bool) types.AuditEvent {
	var data map[string]json.RawMessage
	if len(ev.Data) > 0 && json.Unmarshal(ev.Data, &data) != nil {
		data = nil
	}
	return types.AuditEvent{ID: ev.ID, Time: ev.Time, ActorType: ev.ActorType, Actor: unitGovernanceActor(ev, data, in),
		Action: ev.Action, Target: unitGovernanceTarget(ev, data, in), Outcome: ev.Outcome, Data: unitGovernanceData(data, in)}
}

// unitGovernanceActor is who the row says acted, for a reader whose units are in:
//   - the person, for a unit admin acting under a grant in the reader's scope, for a person's own request
//     or their withdrawal of it, and for the system;
//   - "group" for a decision by a group decider, never the person's name;
//   - "unit_admin" for an admin of a unit above the reader;
//   - "security" when the scope is security or the row has none (a security admin's write);
//   - "" for a scope the row holds empty (a re-read found the grant gone) or in a shape no writer uses.
func unitGovernanceActor(ev types.AuditEvent, data map[string]json.RawMessage, in map[uuid.UUID]bool) string {
	if ev.ActorType == types.ActorSystem || unitGovernanceRequest(ev.Action) || unitGovernanceWithdrawn(data) {
		return ev.Actor
	}
	raw, present := data["unit_scope"]
	if !present {
		return unitGovernanceActorSecurity
	}
	var scope string
	if json.Unmarshal(raw, &scope) != nil {
		return ""
	}
	switch id, err := uuid.Parse(scope); {
	case scope == "":
		return ""
	case scope == unitGovernanceActorSecurity:
		return unitGovernanceActorSecurity
	case strings.HasPrefix(scope, unitGovernanceGroupScope):
		return unitGovernanceActorGroup
	case err != nil:
		return ""
	case in[id]:
		return ev.Actor
	}
	return unitGovernanceActorUnitAdmin
}

// unitGovernanceTarget is the row's target when it is a unit in scope, or on the families that name one the
// row's own membership or grant id (a uuid) for a row whose unit_id is in scope; otherwise empty.
func unitGovernanceTarget(ev types.AuditEvent, data map[string]json.RawMessage, in map[uuid.UUID]bool) string {
	id, err := uuid.Parse(ev.Target)
	if err != nil {
		return ""
	}
	if in[id] {
		return ev.Target
	}
	if unit, ok := unitGovernanceUUID(data["unit_id"]); ok && in[unit] && unitGovernanceOwnIDTarget(ev.Action) {
		return ev.Target
	}
	return ""
}

// unitGovernanceUUID is raw as a uuid, when it is a JSON string holding one.
func unitGovernanceUUID(raw json.RawMessage) (uuid.UUID, bool) {
	var s string
	if len(raw) == 0 || json.Unmarshal(raw, &s) != nil {
		return uuid.Nil, false
	}
	id, err := uuid.Parse(s)
	return id, err == nil
}

// unitGovernanceData is data's allow-listed keys under the value rules of unitGovernanceValue. Nothing served
// leaves nil.
func unitGovernanceData(data map[string]json.RawMessage, in map[uuid.UUID]bool) json.RawMessage {
	kept := map[string]any{}
	for _, k := range unitGovernanceDataKeys {
		if raw, ok := data[k]; ok {
			if v, keep := unitGovernanceValue(k, raw, in); keep {
				kept[k] = v
			}
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

// unitGovernanceValue is the value served under key and whether any is. The ids that name a unit are kept
// only for a unit in scope; units keeps the in-scope subset; the scope is served as security, an id, "group"
// or empty; subjects and groups and an image need their own shape; the rest follow the run view's codes,
// numbers and booleans. An object is never served.
func unitGovernanceValue(key string, raw json.RawMessage, in map[uuid.UUID]bool) (any, bool) {
	switch key {
	case "unit_id", "from_unit_id", "parent_id":
		id, ok := unitGovernanceUUID(raw)
		return id.String(), ok && in[id]
	case "grant_id":
		id, ok := unitGovernanceUUID(raw)
		return id.String(), ok
	case "unit_scope":
		return unitGovernanceScopeValue(raw, in)
	case "units":
		return unitGovernanceUnits(raw, in)
	case "subject", "added", "removed":
		return unitGovernanceStrings(raw, unitGovernanceSubject)
	case "image":
		return unitGovernanceStrings(raw, unitGovernanceImage)
	}
	if !unitViewValue(key, raw) {
		return nil, false
	}
	return json.RawMessage(bytes.TrimSpace(raw)), true
}

// unitGovernanceScopeValue is data.unit_scope as served: security, a unit in scope, "group" for a group
// decider (the group's name is dropped), and "" for a scope the row holds empty.
func unitGovernanceScopeValue(raw json.RawMessage, in map[uuid.UUID]bool) (any, bool) {
	var scope string
	if json.Unmarshal(raw, &scope) != nil {
		return nil, false
	}
	if id, err := uuid.Parse(scope); err == nil {
		return scope, in[id]
	}
	switch {
	case scope == "" || scope == unitGovernanceActorSecurity:
		return scope, true
	case strings.HasPrefix(scope, unitGovernanceGroupScope):
		return unitGovernanceActorGroup, true
	}
	return nil, false
}

// unitGovernanceUnits is the units of a list of ids that are in scope, nothing when none is.
func unitGovernanceUnits(raw json.RawMessage, in map[uuid.UUID]bool) (any, bool) {
	var ids []string
	if json.Unmarshal(raw, &ids) != nil {
		return nil, false
	}
	kept := []string{}
	for _, s := range ids {
		if id, err := uuid.Parse(s); err == nil && in[id] {
			kept = append(kept, s)
		}
	}
	return kept, len(kept) > 0
}

// unitGovernanceStrings is raw, a string or a list of strings, when every item matches shape.
func unitGovernanceStrings(raw json.RawMessage, shape *regexp.Regexp) (any, bool) {
	var one string
	if json.Unmarshal(raw, &one) == nil {
		return one, shape.MatchString(one)
	}
	var many []string
	if json.Unmarshal(raw, &many) != nil {
		return nil, false
	}
	if slices.ContainsFunc(many, func(s string) bool { return !shape.MatchString(s) }) {
		return nil, false
	}
	return many, true
}
