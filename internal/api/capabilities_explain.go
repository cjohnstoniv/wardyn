// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// capExplainState is one of the five states "What this type gets" renders per
// row (user-types design §2.6). Each is an ANSWER, not only a label: everyone
// and this_type are exactly the cells capBatch.decide allows, and the other
// three exactly the cells it refuses (TestCapExplainAgreesWithResolver). Which
// of each pair a cell gets is read off the row that decided it:
//
//   - blocked: an overlapping deny (decide step 2) — the wall warning. Shown on
//     an unenforced widening kind too, where step 4 refuses first: the row
//     still walls the subject the moment the kind is enforced.
//   - this_type: a matching allow (step 5), including one written for `all`.
//   - everyone: no row decides it and the narrowing kind is unenforced (step 6).
//   - not_available: refused without a deny because the value is restricted
//     ("Available to: Only...", step 3) and no allow naming it lists this
//     subject — on any kind, image included — or because a narrowing kind is
//     enforced and nothing allows the value (step 7).
//   - admins_only: a widening kind refused without a deny on an unrestricted
//     value — its switch is off (step 4) or nothing allows the value (step 7).
type capExplainState string

const (
	capExplainEveryone     capExplainState = "everyone"
	capExplainThisType     capExplainState = "this_type"
	capExplainBlocked      capExplainState = "blocked"
	capExplainAdminsOnly   capExplainState = "admins_only"
	capExplainNotAvailable capExplainState = "not_available"
)

// capExplainRow is one line of the grid: kind K's answer at value V. The
// wildcard "*" (capWildcard) is the family's default — the answer for a value
// no specific row names. Any other value is one resource this subject (or
// every signed-in human) holds an explicit grant for, or one an admin has
// restricted to a list; Restricted says which values carry that bit.
type capExplainRow struct {
	Kind       string          `json:"kind"`
	Value      string          `json:"value"`
	State      capExplainState `json:"state"`
	Restricted bool            `json:"restricted,omitempty"`
}

// explainPrincipal canonicalizes subject the way validateCapabilityGrant
// canonicalizes a stored row's subject, and returns the synthetic caller
// Explain answers for: the subjects callerSubjects would publish for a person
// who is exactly this subject and nothing else. So "Alice@Corp.com " finds the
// rows written for alice@corp.com, and a group no session could carry is
// refused rather than answered "everyone".
//
// Nothing about the subject is looked up: a user subject carries no groups and
// no user type (there is no directory, and a person's type is stamped per
// session), so a group or type deny that walls the real person does not show
// on their grid. A user_type subject is its id, verbatim, as it is written on
// a grant row.
func explainPrincipal(subjectType types.CapabilitySubjectType, raw string) (string, callerSubjects, error) {
	var c callerSubjects
	subject := strings.TrimSpace(raw)
	switch {
	case subjectType != types.CapabilitySubjectUser && subjectType != types.CapabilitySubjectGroup && subjectType != types.CapabilitySubjectUserType:
		return "", c, fmt.Errorf("subject_type must be one of: user, group, user_type")
	case subject == "":
		return "", c, fmt.Errorf("subject is required")
	}
	switch subjectType {
	case types.CapabilitySubjectUser:
		subject = canonicalUserSubject(subject)
		c.users = []string{subject}
	case types.CapabilitySubjectGroup:
		g, ok := oidc.CanonicalGroupSubject(subject)
		if !ok {
			return "", c, fmt.Errorf("subject: a group subject must be printable ASCII — it is matched against the login-time group snapshot, which carries printable ASCII only")
		}
		subject, c.groups = g, []string{g}
	default:
		if !oidc.UserTypeIDWellFormed(subject) {
			return "", c, fmt.Errorf("subject: %q is not a user type id", subject)
		}
		c.userType = subject
	}
	return subject, c, nil
}

// capExplain is the Explain grid (K4, authz design §4.2) for the synthetic
// principal subj: a caller who is never the operator exemption and never a
// stale snapshot, since a named subject's rows are all in hand, and whose rows
// are the ones ListCapabilityGrantsFor selects for every live caller. Every
// cell is capBatch.decide's own answer for that principal, so the grid cannot
// say allowed where the resolver refuses; explainCell only picks which of the
// five states names the row that decided it.
//
// Per kind: the default row, then one row per specific value a grant names or
// a restriction covers, sorted. The default is decided over the kind's "*"
// rows alone. For an exact kind that is the same as asking about "*"; for an
// egress host set it is not — "*" as a WANT overlaps every host deny
// (capValueOverlaps), which is the right answer for a member who types "*"
// and the wrong one for "any other host".
func (s *Server) capExplain(ctx context.Context, subj callerSubjects, kinds []string) ([]capExplainRow, error) {
	grants, err := s.cfg.Store.ListCapabilityGrantsFor(ctx, subj.users, subj.groups, subj.userType)
	if err != nil {
		return nil, fmt.Errorf("api: explain capabilities: %w", err)
	}
	enf, err := s.cfg.Store.GetCapabilityEnforcement(ctx)
	if err != nil {
		return nil, fmt.Errorf("api: explain capabilities: %w", err)
	}
	restricted, err := s.cfg.Store.ListCapabilityRestrictions(ctx)
	if err != nil {
		return nil, fmt.Errorf("api: explain capabilities: %w", err)
	}
	var wild []types.CapabilityGrant
	for _, g := range grants {
		if strings.TrimSpace(g.Value) == capWildcard {
			wild = append(wild, g)
		}
	}
	full := explainBatch(s, subj, grants, enf, restricted)
	dflt := explainBatch(s, subj, wild, enf, restricted)

	var rows []capExplainRow
	for _, kind := range kinds {
		k, ok := capKinds[kind]
		if !ok {
			continue
		}
		state, err := dflt.explainCell(ctx, kind, k.direction, capWildcard)
		if err != nil {
			return nil, err
		}
		rows = append(rows, capExplainRow{Kind: kind, Value: capWildcard, State: state})

		values := map[string]bool{}
		for _, g := range full.byKind[kind] {
			values[strings.TrimSpace(g.Value)] = true
		}
		for v, on := range restricted[kind] {
			if on && k.restrictable {
				values[v] = true
			}
		}
		delete(values, capWildcard)
		for _, v := range slices.Sorted(maps.Keys(values)) {
			if state, err = full.explainCell(ctx, kind, k.direction, v); err != nil {
				return nil, err
			}
			rows = append(rows, capExplainRow{Kind: kind, Value: v, State: state, Restricted: k.restrictable && restricted[kind][v]})
		}
	}
	return rows, nil
}

// explainBatch is a capBatch already holding everything decide reads — the
// rows indexed by kind, the switch map and the restrictions — so a cell never
// reaches the store. byKind is non-nil even when empty: a nil one would make
// scan resolve the ADMIN asking, not the subject.
func explainBatch(s *Server, subj callerSubjects, grants []types.CapabilityGrant, enf map[string]bool, restricted map[string]map[string]bool) *capBatch {
	b := &capBatch{s: s, subj: subj, byKind: map[string][]types.CapabilityGrant{},
		enf: enf, enfLoaded: true, restricted: restricted, restrictLoaded: true}
	for _, g := range grants {
		b.byKind[g.Capability] = append(b.byKind[g.Capability], g)
	}
	return b
}

// explainCell is one cell: decide's answer, labelled by scan's rows and the
// value's restriction. A deny never coexists with allowed (step 2 or 4
// refuses), and a widening kind is only ever allowed through an allow (step 5).
func (b *capBatch) explainCell(ctx context.Context, kind string, dir capDirection, value string) (capExplainState, error) {
	allowed, err := b.decide(ctx, kind, dir, value)
	if err != nil {
		return "", err
	}
	deny, allow, err := b.scan(ctx, kind, value)
	if err != nil {
		return "", err
	}
	restricted, err := b.isRestricted(ctx, kind, value)
	if err != nil {
		return "", err
	}
	switch {
	case deny:
		return capExplainBlocked, nil
	case allowed && allow:
		return capExplainThisType, nil
	case allowed:
		return capExplainEveryone, nil
	case restricted, dir == capNarrowing:
		return capExplainNotAvailable, nil
	default:
		return capExplainAdminsOnly, nil
	}
}

// explainResponse is GET /permissions/explain's body.
type explainResponse struct {
	SubjectType  types.CapabilitySubjectType `json:"subject_type"`
	Subject      string                      `json:"subject"`
	KindsVersion int                         `json:"kinds_version"`
	Rows         []capExplainRow             `json:"rows"`
}

// handleExplainCapabilities answers GET
// /permissions/explain?subject_type=&subject=&kinds=: the Explain grid (K4)
// for one named subject across the kinds asked for (each once), defaulting to
// every kind in the admin surface's own order (capabilityKinds). securityOps
// (mountPermissionRoutes) — the same tier as the rest of /permissions; this is
// a read over that same table, at a different subject than the caller's own.
// A user type that does not exist is refused as the grant write refuses it.
func (s *Server) handleExplainCapabilities(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	subjectType := types.CapabilitySubjectType(q.Get("subject_type"))
	subject, subj, err := explainPrincipal(subjectType, q.Get("subject"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !s.userTypeSubjectExists(w, r, subjectType, subject) {
		return
	}

	kinds := capabilityKinds
	if raw := q.Get("kinds"); raw != "" {
		kinds = nil
		for _, k := range strings.Split(raw, ",") {
			if !validCapabilityKind(k) {
				writeError(w, http.StatusBadRequest, fmt.Sprintf("unknown capability kind %q", k))
				return
			}
			if !slices.Contains(kinds, k) {
				kinds = append(kinds, k)
			}
		}
	}

	rows, err := s.capExplain(r.Context(), subj, kinds)
	if err != nil {
		writeServerError(w, r, "explain capabilities", err)
		return
	}
	writeJSON(w, http.StatusOK, explainResponse{
		SubjectType:  subjectType,
		Subject:      subject,
		KindsVersion: capKindsVersion,
		Rows:         rows,
	})
}
