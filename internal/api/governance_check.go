// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// POST /governance/check: which unit binds a principal carrying some claims.
package api

import (
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// POST /governance/check

// maxGovernancePreviewClaims bounds ONE preview's claim lists. The body cap
// (decodeStrict's maxJSONBody) already bounds the request, but a 1 MiB body of
// one-character claims is ~250k array elements handed to `= ANY($1::text[])`
// and to the dedupe below — so the list gets its own ceiling, well past the few
// dozen claims a real token carries.
const maxGovernancePreviewClaims = 256

// governancePreviewRequest is POST /governance/check's body: the SAME two
// claim lists ListMembershipsFor itself takes, so this endpoint has
// nothing to derive and nothing to re-order.
//
// The ambiguity, and how this endpoint treats it. The console's preview field
// is one kind-LESS claims box (the People step's own, PREVIEW.FIELD_CLAIMS) —
// a pasted line may be a sign-in subject, an email, or a group, and the wire
// cannot tell. So the console sends EVERY typed claim in BOTH lists, and this
// endpoint offers each to both tiers exactly as given. That is the same shape
// POST /access/preview already takes ({roles: lines, groups: lines}), and it is
// honest because the response says which tier matched: matched_tier names the
// row that won, which is what the console renders ("matched by a group
// assignment"). An answer that could only have come from a group assignment
// says so, and one that could only have come from a user assignment says that.
//
// What this endpoint deliberately does NOT do is inspect the SHAPE of a claim.
// There is no "@ makes it an email" rule here, because that would be a second
// opinion about identity living beside the resolver's. Within the user tier the
// resolver ranks by array_position — the caller's own ordering IS the
// precedence — so the console sends user_subjects in the order capabilitySubjects
// would build them (sign-in subject before email), and the ranking stays the
// SQL's.
type governancePreviewRequest struct {
	UserSubjects []string `json:"user_subjects,omitempty"`
	Groups       []string `json:"groups,omitempty"`
	// UserType is the previewed person's type id; "" previews no type tier.
	UserType string `json:"user_type,omitempty"`
}

// governancePreviewResponse names the profile that would bind a principal
// presenting those claims, and the TIER of the assignment that won.
//
// Every field is omitempty, and an empty object is the answer for "no
// assignment matched" — the deployment ceiling. That is the same additive/
// absent doctrine defaultPolicyResponse.GovernanceUnitName follows: an
// absent key already decodes as "no profile" in the TS mirror, while "" would
// be a value the console then has to special-case.
//
// ProfileID uses `omitzero`, not `omitempty`: uuid.UUID is an ARRAY type, and
// omitempty never elides one — the nil uuid would ship as "00000000-…" on
// exactly the answer that has no profile.
type governancePreviewResponse struct {
	UnitID      uuid.UUID                   `json:"unit_id,omitzero"`
	UnitName    string                      `json:"unit_name,omitempty"`
	MatchedTier types.CapabilitySubjectType `json:"matched_tier,omitempty"`
}

// handleGovernanceCheck answers "which unit would bind a principal
// carrying these claims, with no unit asked for" by running THE selection — the identical
// selectContext call effectiveCeiling makes on the enforcement path (the home context).
// securityOps (routes.go).
//
// That single call is the whole point: the ranking order — tier,
// sub-over-email, priority DESC, name ASC — must not be re-implemented
// anywhere else. A
// second implementation of the precedence rule is a second implementation of
// the answer, and a preview that drifts from enforcement is worse than no
// preview: it is confidently wrong at the moment an admin is deciding whether a
// ceiling is right. So there is no ORDER BY in Go here and none in TS; the
// ranking exists once, as the indexed read in internal/store/governance.go.
//
// Not audited, for the reason handleDirectorySearch states about searches: this
// is typed into, mints nothing and changes nothing, and a row per keystroke
// would turn the append-only log into a record of every claim an admin tried.
//
// ErrNotFound is a RESULT, not a failure — the absent-row doctrine, answered as
// the empty object. Only a real store failure is a 500, and it must be: a
// preview that silently reported "no assignment matches" on a database hiccup
// would tell an admin their profile does not bind someone it does.
func (s *Server) handleGovernanceCheck(w http.ResponseWriter, r *http.Request) {
	var req governancePreviewRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	users, msg := normalizeGovernancePreviewClaims(req.UserSubjects, "user_subjects")
	if msg != "" {
		writeErrorReason(w, http.StatusBadRequest, reasonGovernancePreviewClaimsInvalid, msg)
		return
	}
	groups, msg := normalizeGovernancePreviewGroups(req.Groups)
	if msg != "" {
		writeErrorReason(w, http.StatusBadRequest, reasonGovernancePreviewClaimsInvalid, msg)
		return
	}
	pick, err := s.selectContext(r.Context(), contextQuery{users: users, groups: groups, userType: strings.TrimSpace(req.UserType)}, contextChoice{})
	if err != nil {
		writeServerError(w, r, "resolve governance profile", err)
		return
	}
	if pick.isDefault() || pick.row == nil {
		writeJSON(w, http.StatusOK, governancePreviewResponse{})
		return
	}
	writeJSON(w, http.StatusOK, governancePreviewResponse{
		UnitID: pick.unit.ID, UnitName: pick.unit.Name, MatchedTier: pick.row.Membership.SubjectType,
	})
}

// normalizeGovernancePreviewClaims lowercases, trims and de-duplicates one
// claim list, preserving ORDER — which matters, because order is the user
// tier's tie-break (array_position).
//
// User subjects only. The normalization is not a nicety: assignments are stored
// lowercased (validateGovernanceMembership) and the enforcement path's user
// input arrives already folded — capabilitySubjects lowercases the sub and the
// email. A preview that skipped it would answer "no assignment matches" for a
// claim typed `Eng` against a row the real run matches, which is the drift this
// endpoint exists to remove.
//
// Neither half is a plain ToLower. It is the whole rule for an ASCII subject only: strings.ToLower is
// a UNICODE fold, so U+212A becomes ASCII 'k' and U+0130 becomes 'i', and a
// preview claim typed in either spelling would answer for a DIFFERENT human's
// row than the run resolves. Both halves therefore ask the match surface's own
// canonicalizer — users through types.CanonicalUserSubject (the same function
// capabilitySubjects and the two write boundaries use), groups through
// normalizeGovernancePreviewGroups below (types.CanonicalGroupSubject).
func normalizeGovernancePreviewClaims(in []string, field string) ([]string, string) {
	if len(in) > maxGovernancePreviewClaims {
		return nil, fmt.Sprintf("%s: at most %d claims", field, maxGovernancePreviewClaims)
	}
	out := make([]string, 0, len(in))
	for _, v := range in {
		if c := types.CanonicalUserSubject(v); c != "" && !slices.Contains(out, c) {
			out = append(out, c)
		}
	}
	return out, ""
}

// normalizeGovernancePreviewGroups is the GROUP half, and it calls the snapshot's
// own normalizer rather than restating it.
//
// The divergence this closes. The preview answers one question — "which profile
// would bind a principal presenting this claim" — and there has to be one
// answer. A plain ToLower with no ASCII guard FOLDS: U+212A KELVIN SIGN becomes
// ASCII 'k', so a crafted "Kubernetes-admins" normalized onto the real,
// operator-authored group "kubernetes-admins" and the endpoint reported that the
// profile BINDS it. The enforcement path refuses the same claim outright
// (CanonicalGroupSubject ok=false), drops it from the snapshot and stamps the
// snapshot truncated. Preview said yes; enforcement said no.
//
// Dropped, not refused with a 400, because dropping is exactly what the
// enforcement path does with the same claim — a preview that 400s where a login
// silently drops would be a second, different answer rather than the same one.
// The count cap and the de-duplication stay identical to the user half.
//
// Residual, named rather than hidden: enforcement ALSO stamps the snapshot
// truncated when it drops a group, and a truncated snapshot makes
// effectiveCeiling answer errGroupsSnapshotStale (403) for that member whenever
// a group-tier assignment exists. The preview has no field for "and this claim
// would make your snapshot incomplete", so it reports the dropped claim as
// simply unmatched. That is strictly closer to enforcement than the fold was,
// and the gap is filed rather than invented as a new response field here.
func normalizeGovernancePreviewGroups(in []string) ([]string, string) {
	if len(in) > maxGovernancePreviewClaims {
		return nil, fmt.Sprintf("groups: at most %d claims", maxGovernancePreviewClaims)
	}
	out := make([]string, 0, len(in))
	for _, v := range in {
		c, ok := types.CanonicalGroupSubject(v)
		if !ok {
			continue // no login snapshot can carry it; enforcement drops it too
		}
		if !slices.Contains(out, c) {
			out = append(out, c)
		}
	}
	return out, ""
}
