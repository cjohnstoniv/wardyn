// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// capabilitySubjectUserType is the subject type the type editor's "What this
// type gets" screen (user-types-design.md rev 4 §2.6, UT-7a) asks Explain
// about. It is NOT yet one of capability_grants.subject_type's CHECKed values
// (migration 0042: user, group, all) — UT-3 (#610) widens that CHECK and
// teaches capabilitySubjects to carry it. Until then no grant row can name a
// user type, so Explain answers every kind's own default for one: correct,
// not merely harmless, and nothing here changes when UT-3 lands.
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
// row (design §2.6): *Everyone* (no row, unrestricted), *This type* (an allow
// row), *Blocked* (a deny row, the wall warning), *Admins only* (a widening
// kind with no allow), and *Not available* (restricted, this subject not
// listed).
//
// capExplainNotAvailable is carried here but never produced by capExplain
// yet: it needs the per-value "restricted" bit (capability_restrictions,
// UT-10 / #612), which has no migration on this tree — capBatch.decide's own
// step 3 is still a reserved comment for the same reason. It starts firing
// the moment that table lands; nothing here has to change.
type capExplainState string

const (
	capExplainEveryone     capExplainState = "everyone"
	capExplainThisType     capExplainState = "this_type"
	capExplainBlocked      capExplainState = "blocked"
	capExplainAdminsOnly   capExplainState = "admins_only"
	capExplainNotAvailable capExplainState = "not_available"
)

// capExplainRow is one line of the grid: kind K's answer at value V. The
// wildcard "*" (capWildcard) is the family's own default — no row names this
// subject or "all" specifically — and any other value is one resource this
// subject (or every signed-in human) holds an explicit grant for, exactly the
// granularity the "Available to" control writes at (design §2.6).
type capExplainRow struct {
	Kind  string          `json:"kind"`
	Value string          `json:"value"`
	State capExplainState `json:"state"`
}

// capExplain is the Explain grid (K4, design §4.2): for each of kinds, every
// row that names subject (subjectType, subject) or names "all", resolved by
// the same deny-wins rule capBatch.decide applies for a live caller — minus
// its step 3 (UT-10's restriction bit is not on this tree) and minus the
// per-kind enforcement switch, which the console already renders as its own
// control and is not one of the five per-row states.
//
// Pure and error-free: grants is already the full table in memory (the same
// read GET /permissions makes via ListCapabilityGrants), so this never
// touches the store.
func capExplain(grants []types.CapabilityGrant, subjectType types.CapabilitySubjectType, subject string, kinds []string) []capExplainRow {
	var rows []capExplainRow
	for _, kind := range kinds {
		k, ok := capKinds[kind]
		if !ok {
			continue
		}
		rows = append(rows, capExplainKind(grants, subjectType, subject, kind, k.direction)...)
	}
	return rows
}

// capExplainKind is capExplain for one kind. byValue groups this subject's
// (and "all"'s) own rows by the exact Value a grant names — no overlap
// expansion, because the grid renders exactly the resources an admin wrote a
// rule about, at the granularity they wrote it. Deny wins across the two
// scopes for the SAME value, matching capBatch.scan's own no-subject-
// precedence rule: capability_grants' natural key already forbids two rows at
// one value for one subject, so only a subject/"all" pair can ever disagree.
func capExplainKind(grants []types.CapabilityGrant, subjectType types.CapabilitySubjectType, subject, kind string, dir capDirection) []capExplainRow {
	byValue := map[string]types.CapabilityEffect{}
	for _, g := range grants {
		if g.Capability != kind {
			continue
		}
		if g.SubjectType != types.CapabilitySubjectAll && !(g.SubjectType == subjectType && g.Subject == subject) {
			continue
		}
		if byValue[g.Value] != types.CapabilityDeny {
			byValue[g.Value] = g.Effect
		}
	}

	def := capExplainEveryone
	if dir == capWidening {
		def = capExplainAdminsOnly
	}
	if len(byValue) == 0 {
		return []capExplainRow{{Kind: kind, Value: capWildcard, State: def}}
	}

	values := make([]string, 0, len(byValue))
	for v := range byValue {
		values = append(values, v)
	}
	sort.Strings(values)

	rows := make([]capExplainRow, 0, len(values)+1)
	if _, ok := byValue[capWildcard]; !ok {
		rows = append(rows, capExplainRow{Kind: kind, Value: capWildcard, State: def})
	}
	for _, v := range values {
		state := capExplainThisType
		if byValue[v] == types.CapabilityDeny {
			state = capExplainBlocked
		}
		rows = append(rows, capExplainRow{Kind: kind, Value: v, State: state})
	}
	return rows
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
// for one named subject across the kinds asked for, defaulting to every kind
// in the admin surface's own order (capabilityKinds). securityOps
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
	subject := q.Get("subject")
	if subject == "" {
		writeError(w, http.StatusBadRequest, "subject is required")
		return
	}

	kinds := capabilityKinds
	if raw := q.Get("kinds"); raw != "" {
		kinds = strings.Split(raw, ",")
		for _, k := range kinds {
			if !validCapabilityKind(k) {
				writeError(w, http.StatusBadRequest, fmt.Sprintf("unknown capability kind %q", k))
				return
			}
		}
	}

	grants, err := s.cfg.Store.ListCapabilityGrants(r.Context())
	if err != nil {
		writeServerError(w, r, "list capability grants", err)
		return
	}
	writeJSON(w, http.StatusOK, explainResponse{
		SubjectType:  subjectType,
		Subject:      subject,
		KindsVersion: capKindsVersion,
		Rows:         capExplain(grants, subjectType, subject, kinds),
	})
}
