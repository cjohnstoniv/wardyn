// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Per-resource availability ("Available to", user-types design 2.6): the
// restricted bit on one value of one restrictable kind. Who is listed is the
// allow rows in capability_grants, written through POST /permissions/grants;
// this file only turns the bit on and off. capBatch.decide's step 3 reads it.
package api

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// availabilityView is GET and PUT /permissions/availability/{kind}/{value}'s
// body: whether the value is restricted, and the allow rows naming it (the
// "Only..." list). Wildcard allows are left out: they list nobody in
// particular, and on a restricted value they let nobody in (decide's step 5).
type availabilityView struct {
	Kind       string                  `json:"kind"`
	Value      string                  `json:"value"`
	Restricted bool                    `json:"restricted"`
	AllowedBy  []types.CapabilityGrant `json:"allowed_by"`
}

// availabilityWriteRequest is PUT's body.
type availabilityWriteRequest struct {
	Restricted *bool `json:"restricted"`
}

// availabilityOnlyEmptyMsg is the empty "Only..." refusal: restricting a value
// nobody is listed for would make it available to nobody by accident.
const availabilityOnlyEmptyMsg = "Add at least one person, group or user type before choosing Only, or nobody could use this."

// availabilityTarget parses and canonicalizes {kind} and the value (the rest
// of the path, since an image ref carries slashes). Writes the 400 and
// returns ok=false on a kind that cannot be restricted or a value that could
// never be asked about.
func availabilityTarget(w http.ResponseWriter, r *http.Request) (kind, value string, ok bool) {
	kind = chi.URLParam(r, "kind")
	if !capKinds[kind].restrictable {
		writeErrorReason(w, http.StatusBadRequest, reasonAvailabilityKindNotRestrictable, fmt.Sprintf("%q can't be restricted to a list; only the resources people are offered can be.", kind))
		return "", "", false
	}
	// chi hands the wildcard over still escaped, so an encoded ref would be
	// stored as its escapes and an encoded %2e%2e would slip past the dot-segment
	// rule; decode it first so both are judged as the ref they spell.
	raw, err := url.PathUnescape(chi.URLParam(r, "*"))
	if err != nil {
		writeErrorReason(w, http.StatusBadRequest, reasonAvailabilityTargetInvalid, "Invalid availability target: value: not a valid path escape")
		return "", "", false
	}
	value, err = canonicalGrantValue(kind, raw)
	if err == nil && value == capWildcard {
		err = fmt.Errorf("value: name one resource, not every one")
	}
	if err != nil {
		writeErrorReason(w, http.StatusBadRequest, reasonAvailabilityTargetInvalid, "Invalid availability target: "+err.Error())
		return "", "", false
	}
	return kind, value, true
}

// availabilityOf reads the value's restricted bit and its named allow rows.
func (s *Server) availabilityOf(r *http.Request, kind, value string) (availabilityView, error) {
	v := availabilityView{Kind: kind, Value: value, AllowedBy: []types.CapabilityGrant{}}
	restricted, err := s.cfg.Store.ListCapabilityRestrictions(r.Context())
	if err != nil {
		return v, err
	}
	v.Restricted = restricted[kind][value]
	grants, err := s.cfg.Store.ListCapabilityGrants(r.Context())
	if err != nil {
		return v, err
	}
	for _, g := range grants {
		if g.Capability == kind && g.Effect == types.CapabilityAllow && strings.TrimSpace(g.Value) == value {
			v.AllowedBy = append(v.AllowedBy, g)
		}
	}
	return v, nil
}

// handleGetAvailability answers one value's availability. securityOps.
func (s *Server) handleGetAvailability(w http.ResponseWriter, r *http.Request) {
	kind, value, ok := availabilityTarget(w, r)
	if !ok {
		return
	}
	v, err := s.availabilityOf(r, kind, value)
	if err != nil {
		writeServerError(w, r, "read capability availability", err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// writeAvailability writes one value's restriction bit. Lifting a component's is the one case with
// its own seam, which refuses (ErrNotFound) an id that is no org component, so a deleted component's
// id stays closed; every other write is the generic one.
func (s *Server) writeAvailability(r *http.Request, kind, value string, restricted bool) error {
	if lifter, ok := s.cfg.Store.(store.ComponentRestrictionLifter); ok && kind == capComponent && !restricted {
		id, err := uuid.Parse(value)
		if err != nil {
			return err
		}
		return lifter.LiftComponentRestriction(r.Context(), id, principalFromRequest(r))
	}
	return s.cfg.Store.SetCapabilityRestriction(r.Context(), kind, value, restricted, principalFromRequest(r))
}

// handlePutAvailability turns one value's restriction on or off. securityOps:
// availability is a grant fact, even when the console draws the control inside
// an admin-only editor. Turning it on with nobody listed is refused (400).
func (s *Server) handlePutAvailability(w http.ResponseWriter, r *http.Request) {
	kind, value, ok := availabilityTarget(w, r)
	if !ok {
		return
	}
	var req availabilityWriteRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	if req.Restricted == nil {
		writeErrorReason(w, http.StatusBadRequest, reasonAvailabilityRestrictedRequired, "Invalid availability: restricted is required.")
		return
	}
	v, err := s.availabilityOf(r, kind, value)
	if err != nil {
		writeServerError(w, r, "read capability availability", err)
		return
	}
	if *req.Restricted && len(v.AllowedBy) == 0 {
		writeErrorReason(w, http.StatusBadRequest, reasonAvailabilityOnlyEmpty, availabilityOnlyEmptyMsg)
		return
	}
	mode := govDirect
	if *req.Restricted != v.Restricted {
		// Covered whenever the stored bit changes, in either direction: lifting a restriction admits
		// every person with no grant write, and restricting one adds no narrowing exemption beyond that.
		if mode, ok = s.governanceWriteMode(w, r); !ok {
			return
		}
		if mode == govQueue {
			s.holdAvailability(w, r, kind, value, *req.Restricted)
			return
		}
	}
	// With the switch on, an unchanged PUT never reaches the store (the admin token's break-glass
	// excepted): the bit was read outside any transaction, so the only effect the write could have is
	// to undo a restriction approved in between.
	if *req.Restricted != v.Restricted || !envEnabled(envGovernanceSecondHuman) || isAdminTokenCaller(r) {
		err := s.writeAvailability(r, kind, value, *req.Restricted)
		if notFoundIf(w, err, "component", reasonComponentNotFound) {
			return
		}
		if err != nil {
			writeServerError(w, r, "write capability availability", err)
			return
		}
	}
	v.Restricted = *req.Restricted
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		"capability.availability.write", kind, "success", mustJSON(availabilityAuditData(kind, value, v.Restricted))))
	if mode == govBypass {
		s.recordGovernanceBypass(r, govKindAvailability, kind+"/"+value, "success", nil)
	}
	writeJSON(w, http.StatusOK, v)
}

// errUngrantedWorkspaceRepo is narrowUserInlinePolicy's signal (inline_policy.go)
// that a workspace_repos entry named an onboarded workspace the caller does
// not hold capWorkspace for (#1259) — a REFUSAL, not a capDrop, because
// req.workspace_id already refuses the identical workspace with a named 403
// (denyUserRequest) and this is the same door reached a second way. Distinct
// from the plain errors that function otherwise returns (which boundUserSpec
// 500s as a store failure) so the caller can tell the two apart and answer
// 403 instead.
type errUngrantedWorkspaceRepo struct {
	repo, wsID string
}

func (e *errUngrantedWorkspaceRepo) Error() string {
	return fmt.Sprintf("workspace %s (repo %q) is not granted to you", e.wsID, e.repo)
}
