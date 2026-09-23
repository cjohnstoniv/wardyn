// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// capabilitySubjectUserType is the subject type the type editor's "What this
// type gets" screen (user-types-design.md rev 4 §2.6, UT-7a) asks Explain
// about. It is NOT yet one of capability_grants.subject_type's CHECKed values
// (migration 0042: user, group, all) — UT-3 (#610) widens that CHECK and
// teaches capabilitySubjects and ListCapabilityGrantsFor to carry it, at which
// point explainPrincipal passes the type through too. Until then no grant row
// can name a user type, so Explain answers from the `all` rows alone: correct,
// not merely harmless.
const capabilitySubjectUserType types.CapabilitySubjectType = "user_type"

// validExplainSubjectType reports whether Explain can be asked about this
// subject type. Wider than validCapabilityGrant's write-boundary check
// (types.CapabilitySubjectType.Valid) on purpose: Explain only READS, so
// asking about a user type a moment before UT-3 lands is answered, not
// refused.
func validExplainSubjectType(t types.CapabilitySubjectType) bool {
	switch t {
	case types.CapabilitySubjectUser, types.CapabilitySubjectGroup, capabilitySubjectUserType:
		return true
	default:
		return false
	}
}

// capExplainState is one of the five states "What this type gets" renders per
// row (design §2.6). Each is an ANSWER, not only a label: everyone and
// this_type are exactly the cells capBatch.decide allows, and the other three
// exactly the cells it refuses (TestCapExplainAgreesWithResolver). Which of
// each pair a cell gets is read off the row that decided it:
//
//   - blocked: an overlapping deny (decide step 2) — the wall warning. Shown on
//     an unenforced widening kind too, where step 4 refuses first: the row
//     still walls the subject the moment the kind is enforced.
//   - this_type: a matching allow (step 5), including one written for `all`.
//   - everyone: no row decides it and the narrowing kind is unenforced (step 6).
//   - admins_only: a widening kind refused without a deny — its switch is off
//     (step 4, which also makes an allow row on it inert until UT-10) or
//     nothing allows the value (step 7).
//   - not_available: a narrowing kind refused without a deny — it is enforced
//     and nothing allows the value (step 7). The switch is "restrict every
//     value of this kind" (design §2.6), so this is the same state UT-10's
//     per-value restriction bit will produce for one value.
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
// no specific row names — and any other value is one resource this subject
// (or every signed-in human) holds an explicit grant for, exactly the
// granularity the "Available to" control writes at (design §2.6).
type capExplainRow struct {
	Kind  string          `json:"kind"`
	Value string          `json:"value"`
	State capExplainState `json:"state"`
}

// explainPrincipal canonicalizes subject the way validateCapabilityGrant
// canonicalizes a stored row's subject, and returns the synthetic caller
// Explain answers for: the identities capabilitySubjects would publish for a
// person who is exactly this subject and nothing else. So "Alice@Corp.com "
// finds the rows written for alice@corp.com, and a group no session could
// carry is refused rather than answered "everyone".
//
// A user's groups are NOT resolved (there is no directory lookup): a user
// subject is asked about with no groups, so a group deny that walls the real
// person does not show on their grid. A user_type carries neither half yet
// (capabilitySubjectUserType).
func explainPrincipal(subjectType types.CapabilitySubjectType, raw string) (subject string, users, groups []string, err error) {
	subject = strings.TrimSpace(raw)
	if subject == "" {
		return "", nil, nil, fmt.Errorf("subject is required")
	}
	switch subjectType {
	case types.CapabilitySubjectUser:
		subject = canonicalUserSubject(subject)
		users = []string{subject}
	case types.CapabilitySubjectGroup:
		g, ok := oidc.CanonicalGroupSubject(subject)
		if !ok {
			return "", nil, nil, fmt.Errorf("subject: a group subject must be printable ASCII — it is matched against the login-time group snapshot, which carries printable ASCII only")
		}
		subject, groups = g, []string{g}
	}
	return subject, users, groups, nil
}

// capExplain is the Explain grid (K4, design §4.2) for the synthetic principal
// (users, groups): a member — never the operator exemption, never a stale
// snapshot, since a named subject's rows are all in hand — whose rows are the
// ones ListCapabilityGrantsFor selects for every live caller. Every cell is
// capBatch.decide's own answer for that principal, so the grid cannot say
// allowed where the resolver refuses; capExplainCell only picks which of the
// five states names the row that decided it.
//
// Per kind: the default row, then one row per specific value a grant names,
// sorted. The default is decided over the kind's "*" rows alone. For an exact
// kind that is the same as asking about "*"; for an egress host set it is not
// — "*" as a WANT overlaps every host deny (capValueOverlaps), which is the
// right answer for a member who types "*" and the wrong one for "any other
// host".
func (s *Server) capExplain(ctx context.Context, users, groups, kinds []string) ([]capExplainRow, error) {
	grants, err := s.cfg.Store.ListCapabilityGrantsFor(ctx, users, groups)
	if err != nil {
		return nil, fmt.Errorf("api: explain capabilities: %w", err)
	}
	enf, err := s.cfg.Store.GetCapabilityEnforcement(ctx)
	if err != nil {
		return nil, fmt.Errorf("api: explain capabilities: %w", err)
	}
	var wild []types.CapabilityGrant
	for _, g := range grants {
		if strings.TrimSpace(g.Value) == capWildcard {
			wild = append(wild, g)
		}
	}
	full, dflt := explainBatch(s, grants, enf), explainBatch(s, wild, enf)

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

		var values []string
		for _, g := range full.byKind[kind] {
			if strings.TrimSpace(g.Value) != capWildcard && !slices.Contains(values, g.Value) {
				values = append(values, g.Value)
			}
		}
		sort.Strings(values)
		for _, v := range values {
			if state, err = full.explainCell(ctx, kind, k.direction, v); err != nil {
				return nil, err
			}
			rows = append(rows, capExplainRow{Kind: kind, Value: v, State: state})
		}
	}
	return rows, nil
}

// explainBatch is a capBatch already holding everything decide reads — the
// rows indexed by kind and the switch map — so a cell never reaches the store.
// users/groups stay nil: they only select rows, and grants already is that.
func explainBatch(s *Server, grants []types.CapabilityGrant, enf map[string]bool) *capBatch {
	b := &capBatch{s: s, byKind: map[string][]types.CapabilityGrant{}, enf: enf, enfLoaded: true}
	for _, g := range grants {
		b.byKind[g.Capability] = append(b.byKind[g.Capability], g)
	}
	return b
}

// explainCell is one cell: decide's answer, labelled by scan's rows. A deny
// never coexists with allowed (step 2 or 4 refuses), and a widening kind is
// only ever allowed through an allow (step 5).
func (b *capBatch) explainCell(ctx context.Context, kind string, dir capDirection, value string) (capExplainState, error) {
	allowed, err := b.decide(ctx, kind, dir, value)
	if err != nil {
		return "", err
	}
	deny, allow, err := b.scan(ctx, kind, value)
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
	case dir == capWidening:
		return capExplainAdminsOnly, nil
	default:
		return capExplainNotAvailable, nil
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
// (routes.go, mountPermissionRoutes) — the same admin tier as the rest of
// /permissions; this is a read over that same table, at a different subject
// than the caller's own.
func (s *Server) handleExplainCapabilities(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	subjectType := types.CapabilitySubjectType(q.Get("subject_type"))
	if !validExplainSubjectType(subjectType) {
		writeError(w, http.StatusBadRequest, "subject_type must be one of: user, group, user_type")
		return
	}
	subject, users, groups, err := explainPrincipal(subjectType, q.Get("subject"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
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

	rows, err := s.capExplain(r.Context(), users, groups, kinds)
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
