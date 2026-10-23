// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"regexp"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The secret keep. A unit admin reads a profile at the member tier: each eligible grant's scope
// without its secret refs and llm_inspection.workspace_secret_names as a count (redactSpecForUser,
// redactOverlayForUser). The GET -> edit -> PUT round trip sends that back, so a write served at the
// member tier has each such grant and the count turned back into what the stored row holds there,
// as keepMounts does for a withheld host path. A security or super admin reads them whole, and their
// writes are stored as sent.

// secretNamesSentinelRE is the count a member-tier read gives for workspace_secret_names.
var secretNamesSentinelRE = regexp.MustCompile(`^<\d+ name\(s\) redacted>$`)

// keepSecretRefs is req with the stored row's hidden secret refs and names put back, for a write to
// profile id served at the member tier; any other tier's req, and a create's, is returned unchanged.
// A names count that does not stand for the stored names is refused.
func (s *Server) keepSecretRefs(ctx context.Context, id uuid.UUID, req governanceUnitRequest, all []types.GovernanceUnit) (governanceUnitRequest, error) {
	cur, ok := unitsByID(all)[id]
	if !ok || s.specReadTierFor(ctx) != specReadMember {
		return req, nil
	}
	var err error
	req.Ceiling.EligibleGrants = keepGrantSecretRefs(req.Ceiling.EligibleGrants, cur.Ceiling.EligibleGrants)
	if req.Ceiling.LLMInspection, err = keepSecretNames("ceiling", req.Ceiling.LLMInspection, cur.Ceiling.LLMInspection); err != nil {
		return req, err
	}
	o := req.comp.overlay
	if o == nil {
		return req, nil
	}
	var stored types.CeilingOverlay
	if cur.Overlay != nil {
		stored = *cur.Overlay
	}
	cp := *o
	if o.EligibleGrants != nil {
		var keep []types.GrantSpec
		if stored.EligibleGrants != nil {
			keep = *stored.EligibleGrants
		}
		grants := keepGrantSecretRefs(*o.EligibleGrants, keep)
		cp.EligibleGrants = &grants
	}
	if cp.LLMInspection, err = keepSecretNames("overlay", o.LLMInspection, stored.LLMInspection); err != nil {
		return req, err
	}
	if reflect.DeepEqual(cp, *o) {
		return req, nil
	}
	// The raw overlay is what a held write stores and replays, so it carries what was put back too.
	var fields map[string]json.RawMessage
	if json.Unmarshal(req.Overlay, &fields) != nil {
		return req, nil
	}
	for k := range fields {
		switch {
		case strings.EqualFold(k, "eligible_grants"):
			fields[k] = mustJSON(cp.EligibleGrants)
		case strings.EqualFold(k, "llm_inspection"):
			fields[k] = mustJSON(cp.LLMInspection)
		}
	}
	req.comp.overlay, req.Overlay = &cp, mustJSON(fields)
	return req, nil
}

// keepGrantSecretRefs is in with each grant that names no secret and reads as a stored grant that
// does reads at the member tier replaced by that stored grant: the n-th such grant of one reading
// takes the n-th stored grant of it, so grants that read alike keep their stored order. A grant that
// names a secret of its own, or reads as no stored grant, is kept as sent.
func keepGrantSecretRefs(in, stored []types.GrantSpec) []types.GrantSpec {
	if len(in) == 0 {
		return in
	}
	hidden := map[string][]types.GrantSpec{}
	for _, g := range stored {
		if scopeNamesSecret(g.Scope) {
			k := grantReadKey(g)
			hidden[k] = append(hidden[k], g)
		}
	}
	out := slices.Clone(in)
	for i, g := range in {
		if scopeNamesSecret(g.Scope) {
			continue
		}
		if k := grantReadKey(g); k != "" && len(hidden[k]) > 0 {
			out[i], hidden[k] = hidden[k][0], hidden[k][1:]
		}
	}
	return out
}

// scopeNamesSecret reports whether a grant scope carries a key memberSecretScopeKeys hides.
func scopeNamesSecret(scope json.RawMessage) bool {
	var fields map[string]json.RawMessage
	if json.Unmarshal(scope, &fields) != nil {
		return false
	}
	for k := range fields {
		if slices.ContainsFunc(memberSecretScopeKeys, func(m string) bool { return strings.EqualFold(k, m) }) {
			return true
		}
	}
	return false
}

// grantReadKey is g as a member-tier read shows it, its scope's keys in one order; empty when the
// scope is not an object.
func grantReadKey(g types.GrantSpec) string {
	var scope map[string]any
	if json.Unmarshal(redactScopeSecretRefs(g.Scope), &scope) != nil || scope == nil {
		return ""
	}
	g.Scope = mustJSON(scope)
	return string(mustJSON(g))
}

// keepSecretNames is in with the names count a member-tier read gives for stored turned back into the
// stored names. A count that does not stand for stored is refused: storing it would replace the
// names with the redaction.
func keepSecretNames(field string, in, stored *types.LLMInspectionSpec) (*types.LLMInspectionSpec, error) {
	if !sentSecretNamesCount(in) {
		return in, nil
	}
	if stored != nil && len(stored.WorkspaceSecretNames) > 0 {
		read := redactSpecForUser(types.RunPolicySpec{LLMInspection: stored}).LLMInspection
		if slices.Equal(read.WorkspaceSecretNames, in.WorkspaceSecretNames) {
			cp := *in
			cp.WorkspaceSecretNames = slices.Clone(stored.WorkspaceSecretNames)
			return &cp, nil
		}
	}
	return nil, writeRefusal(http.StatusUnprocessableEntity, reasonGovernanceUnitRequestInvalid,
		"The %s.llm_inspection.workspace_secret_names sent back, %q, does not stand for the names this profile stores. "+
			"Send the names themselves, or the field exactly as you read it.", field, in.WorkspaceSecretNames[0])
}

// sentSecretNamesCount reports whether li's workspace_secret_names is the count a member-tier read gives.
func sentSecretNamesCount(li *types.LLMInspectionSpec) bool {
	return li != nil && len(li.WorkspaceSecretNames) == 1 && secretNamesSentinelRE.MatchString(li.WorkspaceSecretNames[0])
}

// withoutKeptSecrets is spec without the grants that name no secret and without a names count, for
// the decode check that runs before keepSecretRefs puts the stored ones back (buildGovernanceUnit
// judges the result in full).
func withoutKeptSecrets(spec types.RunPolicySpec) types.RunPolicySpec {
	spec.EligibleGrants = slices.DeleteFunc(slices.Clone(spec.EligibleGrants),
		func(g types.GrantSpec) bool { return !scopeNamesSecret(g.Scope) })
	if sentSecretNamesCount(spec.LLMInspection) {
		li := *spec.LLMInspection
		li.WorkspaceSecretNames = nil
		spec.LLMInspection = &li
	}
	return spec
}
