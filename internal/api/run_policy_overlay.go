// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// A request's policy_overlay is a document's partial policy: it lays the keys it
// names onto the caller's default policy (their ceiling, or the deployment
// default), and a key it leaves out keeps the default's value. A request's
// inline_policy replaces the default instead, so the two never ride together,
// and neither does a policy_id: the overlay always applies onto the default.

// policyOverlayInvalidMsg and policyOverlayConflictMsg are the sentences of the
// two overlay refusals. Their TypeScript twins are POLICY_OVERLAY_INVALID and
// POLICY_OVERLAY_CONFLICT in ui/src/app/lib/run-mode-refusals.ts.
func policyOverlayInvalidMsg(path, detail string) string {
	return fmt.Sprintf("policy_overlay is not usable: %s %s.", path, detail)
}

func policyOverlayConflictMsg() string {
	return "policy_overlay changes your organisation's default policy, so it cannot be combined with policy_id or inline_policy."
}

var policySpecType = reflect.TypeFor[types.RunPolicySpec]()

// policyOverlayRefusal checks a request's policy_overlay on its own, before any
// gate reads it: never beside policy_id or inline_policy, and a non-empty object
// whose every key is a carried policy key with a strictly read value, no excluded
// nested field and no redaction placeholder. The template decoder's registry walk
// is the check, so a document and a request are held to the same rules.
func policyOverlayRefusal(req createRunRequest) *runRefusal {
	if len(req.PolicyOverlay) == 0 {
		return nil
	}
	if req.PolicyID != nil || req.InlinePolicy != nil {
		return runError(http.StatusBadRequest, reasonPolicyOverlayConflict, policyOverlayConflictMsg())
	}
	var d templateDecoder
	d.walkValue(policySpecType, req.PolicyOverlay, "policy_overlay")
	if len(d.diags) == 0 {
		return nil
	}
	first := d.diags[0]
	return runError(http.StatusBadRequest, reasonPolicyOverlayInvalid, policyOverlayInvalidMsg(first.Path, policyOverlayDetail(first)))
}

// policyOverlayDetail says what is wrong at a diagnostic's path, without the
// template sentences, which speak of a template.
func policyOverlayDetail(d client.TemplateDiagnostic) string {
	switch d.Reason {
	case reasonTemplateFieldInvalid:
		return strings.TrimSuffix(strings.TrimPrefix(d.Message, d.Path+" "), ".")
	case reasonTemplateSecretRefused:
		return "holds what looks like a secret: an overlay names secrets, never their values"
	case reasonTemplateFieldUnavailable:
		return "is not available on this server yet"
	}
	return "is not a policy field"
}

// policyOverlayKeys is the keys an overlay names, sorted; none for no overlay.
func policyOverlayKeys(overlay json.RawMessage) []string {
	var present map[string]json.RawMessage
	if json.Unmarshal(overlay, &present) != nil {
		return nil
	}
	return sortedKeys(present)
}

// requestPolicyRules is the policy a request authors, as the run-mode checks read
// it: its inline_policy, or what its policy_overlay names, with the field it is
// spoken of at. A request with neither has none.
func requestPolicyRules(req createRunRequest) (*types.RunPolicySpec, string) {
	if req.InlinePolicy != nil {
		return req.InlinePolicy, "inline_policy"
	}
	var spec types.RunPolicySpec
	if len(req.PolicyOverlay) > 0 && json.Unmarshal(req.PolicyOverlay, &spec) == nil {
		return &spec, "policy_overlay"
	}
	return nil, ""
}

// applyPolicyOverlay lays the request's policy_overlay onto base, the caller's
// resolved default. For a governed caller only the overlay's keys are bounded as
// theirs: the member pipeline reads the merged spec, so a grant is judged beside
// the hosts it will have, and only the keys the overlay names are taken from its
// answer. Keys the overlay leaves out keep the default's treatment, so the
// operator's mounts and grant pairings, which clamping the default as a whole
// would drop, survive. The warnings and audited drops are the overlay's own: a
// warning the default alone draws, and a drop in a key the overlay leaves out,
// describe nothing the run loses.
func (s *Server) applyPolicyOverlay(ctx context.Context, r *http.Request, base types.RunPolicySpec, req *createRunRequest, ceiling governanceCeiling, dryRun bool, src *policySourceRecord) (types.RunPolicySpec, []string, *runRefusal) {
	if len(req.PolicyOverlay) == 0 {
		return base, nil, nil
	}
	invalid := func(err error) *runRefusal {
		return runError(http.StatusBadRequest, specRefusalReason(err, reasonPolicyOverlayInvalid), "invalid policy_overlay: "+err.Error())
	}
	keys := policyOverlayKeys(req.PolicyOverlay)
	merged, err := templatePolicyOverlay(base, req.PolicyOverlay)
	if err != nil {
		return base, nil, invalid(err)
	}
	// Count-capped before the per-entry narrowing below (see the inline arm).
	if slices.Contains(keys, "allowed_domains") {
		if err := validateAllowedDomainsCount(merged.AllowedDomains); err != nil {
			return base, nil, invalid(err)
		}
	}
	spec, warns := merged, []string(nil)
	// The keys the overlay laid on are the person's; what the bound below then
	// narrowed in them is the ceiling's.
	src.knobs.setBy(ceiling.ownProvenance(), base, merged)
	if !s.runUngoverned(ctx) {
		bounded, mergedWarns, drops, refusal := s.boundRunUserSpecDrops(ctx, r, merged, ceiling, "invalid policy_overlay: ")
		if refusal != nil {
			return base, nil, refusal
		}
		_, baseWarns, _, _ := s.boundRunUserSpecDrops(ctx, r, base, ceiling, "invalid policy: ")
		spec = withPolicyKeys(base, bounded, keys)
		src.knobs.setBy(ceiling.provenance(), merged, spec)
		warns = slices.DeleteFunc(mergedWarns, func(w string) bool { return slices.Contains(baseWarns, w) })
		if !dryRun {
			s.auditPolicyDrops(ctx, r, "runs.policy_overlay", slices.DeleteFunc(drops, func(d capDrop) bool { return !slices.Contains(keys, capDropField(d)) }))
		}
	}
	if err := validatePolicySpec(spec); err != nil {
		return base, nil, invalid(err)
	}
	overlaid := redactSpecForUser(auditablePolicy(merged))
	src.Overlay, src.overlaid = keys, &overlaid
	return spec, warns, nil
}

// recordPolicyApplyAudits writes the policy apply rows the fold staged, right after the run's row is
// inserted: they exist exactly when a run does, and a launch refused anywhere earlier leaves none.
func (s *Server) recordPolicyApplyAudits(ctx context.Context, src policySourceRecord) {
	for _, ev := range src.applyAudits {
		s.recordAudit(ctx, ev)
	}
}

// auditPolicyOverlay builds a launch's overlay row, as policy.inline.apply is: staged with the other
// apply rows and written with the run's creation. Keys only, never values:
// the run id is not minted yet, and run.create carries the same keys bound to it.
func (s *Server) auditPolicyOverlay(r *http.Request, keys []string) []types.AuditEvent {
	if len(keys) == 0 {
		return nil
	}
	return []types.AuditEvent{s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		"policy.overlay.apply", "", "success", mustJSON(map[string]any{"keys": keys}))}
}

// capDropField is the policy key a member-pipeline drop took its entry from.
func capDropField(d capDrop) string {
	if d.reason == authz.ReasonCapabilityEgressHost {
		return "allowed_domains"
	}
	return "eligible_grants"
}

// withPolicyKeys is base with the fields keys names taken from from, an empty or
// zero value included: an overlay key the clamp emptied stays emptied.
func withPolicyKeys(base, from types.RunPolicySpec, keys []string) types.RunPolicySpec {
	out := base.Clone()
	dst, src := reflect.ValueOf(&out).Elem(), reflect.ValueOf(from)
	for _, key := range keys {
		if f, ok := templateStructField(policySpecType, key); ok {
			dst.FieldByIndex(f.Index).Set(src.FieldByIndex(f.Index))
		}
	}
	return out
}
