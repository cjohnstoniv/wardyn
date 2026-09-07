// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Governance-profile CRUD (0.7, migration 0052): the admin-facing surface over
// governance_profiles + governance_assignments — named, assignable ceilings and
// the rows binding them to a user, a group, or everyone.
//
// Modeled on access.go / permissions.go: one read that shows the whole picture,
// small validated writes, an audit event per write. Two things are specific to
// this table and neither is optional — the monotone-⊆ bound on a profile's
// eligible grants (governance_grantbound.go), which stops a profile from
// MINTING credential eligibility the deployment never provisioned, and the ON
// DELETE RESTRICT surfaced as a 409, which stops a profile delete from silently
// widening everyone it bound.
package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// maxGovernanceProfileNameLen bounds a profile name on write. A name is a
// human handle rendered in a picker and used as the resolver's deterministic
// tie-break, not a description.
const maxGovernanceProfileNameLen = 128

// mountGovernanceRoutes registers the /governance family — SEVEN routes, all on
// the SECURITY tier.
//
// The parameter is still spelled `operatorOnly` because the six CRUD routes
// were born there, before oidc.RoleSecurityAdmin existed; routes.go now hands
// this function the `securityOps` group instead, which is the widening they
// were registered on the safe tier to wait for. The group a mount function
// receives is decided AT THE CALL SITE, never by this parameter's name —
// routes.go says so at the call, and authz_test.go's chi.Walk matrix is what
// enforces it.
//
// The seventh, POST /governance/preview, was born on this tier: a READ that
// answers "which profile would bind these claims" by running the resolver
// itself, so the console never re-implements the precedence rule to show it.
func (s *Server) mountGovernanceRoutes(operatorOnly chi.Router) {
	operatorOnly.Get("/governance", s.handleGetGovernance)
	operatorOnly.Post("/governance/profiles", s.handleCreateGovernanceProfile)
	operatorOnly.Put("/governance/profiles/{id}", s.handleUpdateGovernanceProfile)
	operatorOnly.Delete("/governance/profiles/{id}", s.handleDeleteGovernanceProfile)
	operatorOnly.Post("/governance/assignments", s.handleUpsertGovernanceAssignment)
	operatorOnly.Delete("/governance/assignments/{id}", s.handleDeleteGovernanceAssignment)
	operatorOnly.Post("/governance/preview", s.handlePreviewGovernanceProfile)
}

// ─── GET /governance ───────────────────────────────────────────────────────

// governanceResponse is GET /governance's body: every profile plus every
// assignment, in ONE call — the console's whole Governance screen, the same
// "one read shows the picture" shape GET /permissions takes. The two lists are
// separate rather than nested because an assignment's whole meaning is WHICH
// profile it points at, and nesting profiles under assignments would duplicate
// a ceiling per binding while hiding an unassigned profile entirely.
type governanceResponse struct {
	Profiles    []types.GovernanceProfile    `json:"profiles"`
	Assignments []types.GovernanceAssignment `json:"assignments"`
}

// handleGetGovernance returns the whole governance picture. operatorOnly
// (routes.go).
func (s *Server) handleGetGovernance(w http.ResponseWriter, r *http.Request) {
	profiles, err := s.cfg.Store.ListGovernanceProfiles(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list governance profiles: "+err.Error())
		return
	}
	assignments, err := s.cfg.Store.ListGovernanceAssignments(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list governance assignments: "+err.Error())
		return
	}
	// Belt-and-braces, the same reason redactPolicyForRead applies on the
	// stored-policy read path: validatePolicySpec already refuses a WRITE that
	// puts a raw llm_inspection secret VALUE in a spec, so a stored ceiling
	// should never carry one — but a read path must never re-expose it if that
	// invariant is ever broken by a migration or a direct DB edit.
	for i := range profiles {
		profiles[i].Ceiling = redactSpecForRead(profiles[i].Ceiling, true)
	}
	writeJSON(w, http.StatusOK, governanceResponse{Profiles: profiles, Assignments: assignments})
}

// ─── profile writes ────────────────────────────────────────────────────────

// governanceProfileRequest is the POST/PUT body. ID/CreatedAt/UpdatedAt/
// CreatedBy are never accepted from the wire: the id comes from the path (PUT)
// or the server (POST), and provenance is always server-assigned — the same
// rule grantWriteRequest states for capability grants.
type governanceProfileRequest struct {
	Name    string                 `json:"name"`
	Ceiling types.RunPolicySpec    `json:"ceiling"`
	Limits  types.GovernanceLimits `json:"limits"`
}

// governanceProfileResponse carries the saved profile plus any OMISSION
// warnings. Warnings are advisory by design and never a refusal: a profile is
// authored from scratch, so "the deployment default carries a denied domain you
// did not" is information an author needs, not an error — and refusing would
// make the deployment default a floor the profile could not go under, which is
// the composition semantics this feature deliberately does not have.
type governanceProfileResponse struct {
	Profile  types.GovernanceProfile `json:"profile"`
	Warnings []string                `json:"warnings,omitempty"`
}

// decodeGovernanceProfileRequest decodes, normalizes and validates a profile
// write body, returning a human-readable message the caller surfaces as 400.
//
// Three gates, in order: strict decoding (an unknown field is a typo that must
// not silently widen behaviour — the LoadPolicySpec discipline); a real name
// (it is the UNIQUE handle and the resolver's tie-break, so blank is not a
// profile); and validatePolicySpec over the ceiling, which is the SAME
// validation a stored run_policies spec gets — a governance ceiling is a
// RunPolicySpec and must never be held to a weaker standard than a policy that
// merely gets clamped against one.
func decodeGovernanceProfileRequest(w http.ResponseWriter, r *http.Request) (governanceProfileRequest, string) {
	var req governanceProfileRequest
	if msg := decodeStrictMsg(w, r, &req); msg != "" {
		return governanceProfileRequest{}, msg
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		return governanceProfileRequest{}, "name is required"
	}
	if len(req.Name) > maxGovernanceProfileNameLen || !controlCharFree(req.Name) {
		return governanceProfileRequest{}, "name is invalid"
	}
	if err := validatePolicySpec(req.Ceiling); err != nil {
		return governanceProfileRequest{}, "invalid ceiling: " + err.Error()
	}
	return req, ""
}

// writeGovernanceProfile is the shared body of POST and PUT: bound the eligible
// grants, persist, audit, and answer with the saved row plus omission warnings.
// id is the row to write (a fresh one for POST, the path's for PUT) and status
// the success code.
func (s *Server) writeGovernanceProfile(w http.ResponseWriter, r *http.Request, id uuid.UUID, status int) {
	req, msg := decodeGovernanceProfileRequest(w, r)
	if msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	// THE structural bound (governance_grantbound.go). A profile may narrow the
	// deployment's credential eligibility; it may never mint eligibility the
	// deployer never provisioned. Refused at write, and re-checked at resolve
	// time because Config.DefaultPolicy is env-borne and a redeploy that drops
	// a pairing must not leave old profiles serving it.
	if err := governanceGrantsWithinCeiling(req.Ceiling.EligibleGrants, s.cfg.DefaultPolicy.EligibleGrants); err != nil {
		writeError(w, http.StatusBadRequest, "invalid ceiling: "+err.Error())
		return
	}
	p := types.GovernanceProfile{
		ID:        id,
		Name:      req.Name,
		Ceiling:   req.Ceiling,
		Limits:    req.Limits,
		CreatedBy: principalFromRequest(r),
	}
	saved, err := s.cfg.Store.UpsertGovernanceProfile(r.Context(), p)
	if errors.Is(err, store.ErrConflict) {
		writeError(w, http.StatusConflict,
			fmt.Sprintf("a governance profile named %q already exists", req.Name))
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "write governance profile: "+err.Error())
		return
	}
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		"governance.profile.write", saved.ID.String(), "success", mustJSON(map[string]any{
			"name":                  saved.Name,
			"min_confinement_class": saved.Ceiling.MinConfinementClass,
			"allow_all_egress":      saved.Ceiling.AllowAllEgress,
			"limits":                saved.Limits,
		})))
	writeJSON(w, status, governanceProfileResponse{
		Profile:  saved,
		Warnings: governanceOmissionWarnings(saved.Ceiling, s.cfg.DefaultPolicy),
	})
}

// handleCreateGovernanceProfile mints a fresh id and writes a new profile
// (201). A name another profile already holds is a caller-fixable 409, never a
// raw driver error. operatorOnly (routes.go).
func (s *Server) handleCreateGovernanceProfile(w http.ResponseWriter, r *http.Request) {
	s.writeGovernanceProfile(w, r, uuid.New(), http.StatusCreated)
}

// handleUpdateGovernanceProfile replaces the profile at {id} (200), rename
// included — renaming has to work, because ON DELETE RESTRICT makes
// delete-and-recreate impossible for a profile that is actually assigned. A PUT
// naming an id no row holds creates it there, which is what PUT means and what
// UpsertGovernanceProfile's single statement does without an existence read.
// operatorOnly (routes.go).
func (s *Server) handleUpdateGovernanceProfile(w http.ResponseWriter, r *http.Request) {
	id, ok := parseIDParam(w, r, "id", "governance profile")
	if !ok {
		return
	}
	s.writeGovernanceProfile(w, r, id, http.StatusOK)
}

// handleDeleteGovernanceProfile removes a profile (204), or 409 when it is
// still ASSIGNED.
//
// The 409 is the whole point of the FK's ON DELETE RESTRICT: cascading the
// assignments away would move every member of this profile back to the
// deployment ceiling — a silent WIDENING, with no audit line saying so and
// nothing for an admin to notice. Refusing makes the widening a deliberate,
// separately-audited act (delete the assignments first). operatorOnly
// (routes.go).
func (s *Server) handleDeleteGovernanceProfile(w http.ResponseWriter, r *http.Request) {
	id, ok := parseIDParam(w, r, "id", "governance profile")
	if !ok {
		return
	}
	err := s.cfg.Store.DeleteGovernanceProfile(r.Context(), id)
	if notFoundIf(w, err, "governance profile") {
		return
	}
	if errors.Is(err, store.ErrConflict) {
		writeError(w, http.StatusConflict,
			"this governance profile is still assigned — delete its assignments first "+
				"(deleting it while assigned would silently widen everyone it bounds back to the deployment ceiling)")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "delete governance profile: "+err.Error())
		return
	}
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		"governance.profile.delete", id.String(), "success", nil))
	w.WriteHeader(http.StatusNoContent)
}

// ─── assignment writes ─────────────────────────────────────────────────────

// governanceAssignmentRequest is POST /governance/assignments's body. The
// natural key (subject_type, subject) is what a caller names; the row's id and
// provenance are server-assigned.
type governanceAssignmentRequest struct {
	SubjectType types.CapabilitySubjectType `json:"subject_type"`
	Subject     string                      `json:"subject"`
	ProfileID   uuid.UUID                   `json:"profile_id"`
	Priority    int                         `json:"priority"`
}

// validateGovernanceAssignment normalizes a in place and validates it, applying
// the SAME field hygiene validateCapabilityGrant applies to a capability grant
// subject — trim, length, no control characters, and for a GROUP subject the
// shared oidc.CanonicalGroupSubject the login-time snapshot itself uses —
// because these two tables are written against the identical subject vocabulary
// and are resolved through the identical capabilitySubjects call. A subject
// normalized one way here and another way there is a row that silently never
// matches.
func validateGovernanceAssignment(a *types.GovernanceAssignment) error {
	if !a.SubjectType.Valid() {
		return fmt.Errorf("subject_type: invalid %q", a.SubjectType)
	}
	if a.ProfileID == uuid.Nil {
		return fmt.Errorf("profile_id: required")
	}
	if a.SubjectType == types.CapabilitySubjectAll {
		// "all" names every signed-in human; the migration is explicit that
		// subject is '' for this type, so a caller-supplied value is dropped
		// rather than trusted — there is nothing for it to legitimately name,
		// and honouring it would create a second, unreachable 'all' row.
		a.Subject = ""
		return nil
	}
	a.Subject = strings.TrimSpace(a.Subject)
	if a.Subject == "" {
		return fmt.Errorf("subject: required for subject_type %q", a.SubjectType)
	}
	if a.SubjectType == types.CapabilitySubjectGroup {
		// The group half of that hygiene is oidc.CanonicalGroupSubject, not a
		// lowercase — see validateCapabilityGrant. A group-tier assignment is
		// the sharper case of the two: HasGroupTierAssignments counts the dead
		// row as "a group tier exists", so a subject no snapshot can carry both
		// fails to wall the member it names AND refuses every caller with an
		// unanswerable snapshot on account of an assignment that could never
		// have applied to them.
		subject, ok := oidc.CanonicalGroupSubject(a.Subject)
		if !ok {
			return fmt.Errorf("subject: must be printable ASCII — a group subject is matched against the login-time group snapshot, which carries printable ASCII only, so this value can never match anyone")
		}
		a.Subject = subject
	} else {
		// The USER half is canonicalUserSubject for the same guard-before-fold
		// reason (capabilities.go): a bare ToLower folds U+212A onto ASCII 'k'
		// and U+0130 onto 'i', so a crafted spelling of a real human's address
		// was stored as THAT human's subject — an assignment binding someone
		// else's ceiling to them.
		a.Subject = canonicalUserSubject(a.Subject)
	}
	if len(a.Subject) > maxCapabilityGrantFieldLen || !controlCharFree(a.Subject) {
		return fmt.Errorf("subject: invalid")
	}
	return nil
}

// handleUpsertGovernanceAssignment binds one subject to one profile, keyed on
// the natural (subject_type, subject): re-assigning a subject REPOINTS its
// single row rather than accumulating a second. 201 for a genuinely new
// binding, 200 when an existing one was repointed — the same created/updated
// signal handleUpsertCapabilityGrant gives, derived the same way (the store
// returns the EXISTING row's id on a conflict, never the candidate's).
// operatorOnly (routes.go).
func (s *Server) handleUpsertGovernanceAssignment(w http.ResponseWriter, r *http.Request) {
	var req governanceAssignmentRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	a := types.GovernanceAssignment{
		SubjectType: req.SubjectType,
		Subject:     req.Subject,
		ProfileID:   req.ProfileID,
		Priority:    req.Priority,
	}
	if err := validateGovernanceAssignment(&a); err != nil {
		writeError(w, http.StatusBadRequest, "invalid assignment: "+err.Error())
		return
	}
	a.ID = uuid.New()
	a.CreatedBy = principalFromRequest(r)
	saved, err := s.cfg.Store.UpsertGovernanceAssignment(r.Context(), a)
	// ErrNotFound here is the FK refusing an unknown profile_id — a 404 naming
	// the profile, not a 500, and not a silent no-op.
	if notFoundIf(w, err, "governance profile") {
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "upsert governance assignment: "+err.Error())
		return
	}
	status := http.StatusCreated
	if saved.ID != a.ID {
		status = http.StatusOK
	}
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		"governance.assignment.write", saved.ID.String(), "success", mustJSON(map[string]any{
			"subject_type": saved.SubjectType,
			"subject":      saved.Subject,
			"profile_id":   saved.ProfileID,
			"priority":     saved.Priority,
		})))
	writeJSON(w, status, saved)
}

// handleDeleteGovernanceAssignment removes one assignment by id (204), 404 when
// unknown. This is the SUPPORTED way to widen a principal back to the
// deployment ceiling, which is why it is audited on its own line rather than
// riding a profile delete. operatorOnly (routes.go).
func (s *Server) handleDeleteGovernanceAssignment(w http.ResponseWriter, r *http.Request) {
	id, ok := parseIDParam(w, r, "id", "governance assignment")
	if !ok {
		return
	}
	err := s.cfg.Store.DeleteGovernanceAssignment(r.Context(), id)
	if notFoundIf(w, err, "governance assignment") {
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "delete governance assignment: "+err.Error())
		return
	}
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		"governance.assignment.delete", id.String(), "success", nil))
	w.WriteHeader(http.StatusNoContent)
}

// ─── POST /governance/preview ──────────────────────────────────────────────

// maxGovernancePreviewClaims bounds ONE preview's claim lists. The body cap
// (decodeStrict's maxJSONBody) already bounds the request, but a 1 MiB body of
// one-character claims is ~250k array elements handed to `= ANY($1::text[])`
// and to the dedupe below — so the list gets its own ceiling, well past the few
// dozen claims a real token carries.
const maxGovernancePreviewClaims = 256

// governancePreviewRequest is POST /governance/preview's body: the SAME two
// claim lists ResolveGovernanceProfile itself takes, so this endpoint has
// nothing to derive and nothing to re-order.
//
// THE AMBIGUITY, AND HOW THIS ENDPOINT TREATS IT. The console's preview field
// is one kind-LESS claims box (the People step's own, PREVIEW.FIELD_CLAIMS) —
// a pasted line may be a sign-in subject, an email, or a group, and the wire
// cannot tell. So the console sends EVERY typed claim in BOTH lists, and this
// endpoint offers each to both tiers exactly as given. That is the same shape
// POST /access/preview already takes ({roles: lines, groups: lines}), and it is
// honest because the RESPONSE SAYS WHICH TIER MATCHED: matched_tier names the
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
}

// governancePreviewResponse names the profile that would bind a principal
// presenting those claims, and the TIER of the assignment that won.
//
// EVERY FIELD IS omitempty, and an empty object is the answer for "no
// assignment matched" — the deployment ceiling. That is the same additive/
// absent doctrine defaultPolicyResponse.GovernanceProfileName follows: an
// absent key already decodes as "no profile" in the TS mirror, while "" would
// be a value the console then has to special-case.
//
// ProfileID uses `omitzero`, not `omitempty`: uuid.UUID is an ARRAY type, and
// omitempty never elides one — the nil uuid would ship as "00000000-…" on
// exactly the answer that has no profile.
type governancePreviewResponse struct {
	ProfileID   uuid.UUID                   `json:"profile_id,omitzero"`
	ProfileName string                      `json:"profile_name,omitempty"`
	MatchedTier types.CapabilitySubjectType `json:"matched_tier,omitempty"`
}

// handlePreviewGovernanceProfile answers "which profile would bind a principal
// carrying these claims" by running THE resolver — the identical
// Store.ResolveGovernanceProfile call effectiveCeiling takes on the enforcement
// path. securityOps (routes.go).
//
// THAT SINGLE CALL IS THE WHOLE POINT. The console's first cut resolved the
// preview client-side, re-reading GET /governance and re-implementing the
// ORDER BY in TypeScript — tier, sub-over-email, priority DESC, name ASC. A
// second implementation of the precedence rule is a second implementation of
// the answer, and a preview that drifts from enforcement is worse than no
// preview: it is confidently wrong at the moment an admin is deciding whether a
// ceiling is right. So there is no ORDER BY in Go here and none in TS; the
// ranking exists once, as the indexed read in internal/store/governance.go.
//
// NOT AUDITED, for the reason handleDirectorySearch states about searches: this
// is typed into, mints nothing and changes nothing, and a row per keystroke
// would turn the append-only log into a record of every claim an admin tried.
//
// ErrNotFound is a RESULT, not a failure — the absent-row doctrine, answered as
// the empty object. Only a real store failure is a 500, and it must be: a
// preview that silently reported "no assignment matches" on a database hiccup
// would tell an admin their profile does not bind someone it does.
func (s *Server) handlePreviewGovernanceProfile(w http.ResponseWriter, r *http.Request) {
	var req governancePreviewRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	users, msg := normalizeGovernancePreviewClaims(req.UserSubjects, "user_subjects")
	if msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	groups, msg := normalizeGovernancePreviewGroups(req.Groups)
	if msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	p, tier, err := s.cfg.Store.ResolveGovernanceProfile(r.Context(), users, groups)
	if errors.Is(err, store.ErrNotFound) || (err == nil && p == nil) {
		writeJSON(w, http.StatusOK, governancePreviewResponse{})
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "resolve governance profile: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, governancePreviewResponse{
		ProfileID: p.ID, ProfileName: p.Name, MatchedTier: tier,
	})
}

// normalizeGovernancePreviewClaims lowercases, trims and de-duplicates one
// claim list, preserving ORDER — which matters, because order is the user
// tier's tie-break (array_position).
//
// USER SUBJECTS ONLY. The normalization is not a nicety: assignments are stored
// lowercased (validateGovernanceAssignment) and the enforcement path's user
// input arrives already folded — capabilitySubjects lowercases the sub and the
// email. A preview that skipped it would answer "no assignment matches" for a
// claim typed `Eng` against a row the real run matches, which is the drift this
// endpoint exists to remove.
//
// NEITHER half is a plain ToLower, and this comment used to say both were. It
// first said "sessionGroups lowercases every group" — the premise a real group
// divergence rested on — and then, once corrected, that "a plain ToLower is the
// WHOLE rule for a user subject", which was the premise the USER divergence
// rested on. It is the whole rule for an ASCII subject only: strings.ToLower is
// a UNICODE fold, so U+212A becomes ASCII 'k' and U+0130 becomes 'i', and a
// preview claim typed in either spelling would answer for a DIFFERENT human's
// row than the run resolves. Both halves therefore ask the match surface's own
// canonicalizer — users through canonicalUserSubject (capabilities.go, the same
// function capabilitySubjects and the two write boundaries use), groups through
// normalizeGovernancePreviewGroups below (oidc.CanonicalGroupSubject).
func normalizeGovernancePreviewClaims(in []string, field string) ([]string, string) {
	if len(in) > maxGovernancePreviewClaims {
		return nil, fmt.Sprintf("%s: at most %d claims", field, maxGovernancePreviewClaims)
	}
	out := make([]string, 0, len(in))
	for _, v := range in {
		if c := canonicalUserSubject(v); c != "" && !slices.Contains(out, c) {
			out = append(out, c)
		}
	}
	return out, ""
}

// normalizeGovernancePreviewGroups is the GROUP half, and it calls the snapshot's
// own normalizer rather than restating it.
//
// THE DIVERGENCE THIS CLOSES. The preview answers one question — "which profile
// would bind a principal presenting this claim" — and there has to be one
// answer. A plain ToLower with no ASCII guard FOLDS: U+212A KELVIN SIGN becomes
// ASCII 'k', so a crafted "Kubernetes-admins" normalized onto the real,
// operator-authored group "kubernetes-admins" and the endpoint reported that the
// profile BINDS it. The enforcement path refuses the same claim outright
// (CanonicalGroupSubject ok=false), drops it from the snapshot and stamps the
// snapshot truncated. Preview said yes; enforcement said no.
//
// DROPPED, not refused with a 400, because dropping is exactly what the
// enforcement path does with the same claim — a preview that 400s where a login
// silently drops would be a second, different answer rather than the same one.
// The count cap and the de-duplication stay identical to the user half.
//
// RESIDUAL, named rather than hidden: enforcement ALSO stamps the snapshot
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
		c, ok := oidc.CanonicalGroupSubject(v)
		if !ok {
			continue // no login snapshot can carry it; enforcement drops it too
		}
		if !slices.Contains(out, c) {
			out = append(out, c)
		}
	}
	return out, ""
}

// ─── the resolver ──────────────────────────────────────────────────────────

// governanceCeiling is ONE principal's resolved ceiling: the RunPolicySpec they
// are bounded by, the request-shape limits beside it, and the profile it came
// from — nil for a principal with no assignment, which is the whole
// absent-row-is-today rule expressed in one field. Every routed site keys its
// new behaviour on `Profile != nil`, never on the spec's contents, because a
// profile that happens to equal DefaultPolicy is still an admin declaring this
// principal's ceiling and an unassigned member must stay byte-for-byte today.
type governanceCeiling struct {
	Spec     types.RunPolicySpec
	Limits   types.GovernanceLimits
	Profile  *types.GovernanceProfile
	Warnings []string
}

// errGroupsSnapshotStale is the 403-class resolver failure: this caller's group
// identity cannot be answered (a pre-0.6 cookie's nil snapshot, or one the
// cookie byte cap truncated), a group-tier assignment exists that might apply
// to them, and no user-tier row settles the question. Serving them ANY ceiling
// would be a guess, and the only wrong guess is the widening one.
//
// ITS Error() TEXT IS THE FULL MESSAGE, not the bare sentinel, and that is the
// fix rather than a flourish: governance.go names ONE mapping for a resolver
// failure (writeCeilingError below) "so a resolver failure cannot answer 403 at
// one site and 500 at the next for the same cause" — but a site that takes only
// the STATUS half via ceilingErrorStatus and composes its own body from
// err.Error() bypassed that rule and shipped a 403 saying nothing but
// "groups_snapshot_stale". The remedy is one the human can actually perform, and
// the alternative is a support ticket. Attaching it to the VALUE means every
// site that prints the error carries the remedy, including sites written later,
// instead of each one having to remember the rule.
var errGroupsSnapshotStale = errors.New(groupsSnapshotStaleMsg)

// groupsSnapshotStaleMsg is what the caller reads. It names the remedy, because
// the remedy is one the human can actually perform and the alternative is a
// support ticket: sign in again (or re-mint the API token) and the snapshot is
// rebuilt from the current claims.
const groupsSnapshotStaleMsg = "groups_snapshot_stale: your group membership snapshot is missing or was truncated at sign-in, " +
	"and this deployment assigns governance profiles by group — sign in again (or re-mint your API token) so your ceiling can be resolved"

// writeCeilingError answers an effectiveCeiling failure at an HTTP site: 403
// for the stale/truncated snapshot, 500 for everything else.
//
// 500 IS THE POINT for the everything-else arm. A store failure means the
// ceiling is unknown, and the adjacent GetSiteConfig idiom — log it, carry on
// with the zero value — must NOT be copied here: carrying on means silently
// substituting the deployment ceiling for a profile that may be far narrower,
// which is a widening triggered by a database hiccup.
func writeCeilingError(w http.ResponseWriter, err error) {
	if errors.Is(err, errGroupsSnapshotStale) {
		// Identical to err.Error() now that the sentinel carries the message;
		// spelled out because THIS is the site that defines what the body is,
		// and a reader should not have to chase the sentinel to find out.
		writeError(w, http.StatusForbidden, groupsSnapshotStaleMsg)
		return
	}
	writeError(w, http.StatusInternalServerError, "resolve governance ceiling: "+err.Error())
}

// writeCeilingErrorPrefixed is writeCeilingError for a seam that has its own
// error prefix ("list secrets: ", "policy: ", …).
//
// THE STALE ARM DROPS THE PREFIX ON PURPOSE. groupsSnapshotStaleMsg exists
// because the remedy is one the human can actually perform — sign in again, or
// re-mint the API token — and the alternative is a support ticket. A seam that
// pastes its prefix onto err.Error() instead publishes the bare
// `groups_snapshot_stale` sentinel: an internal identifier naming a condition a
// member has no vocabulary for and no documented way to clear. The status was
// already shared (ceilingErrorStatus); this shares the SENTENCE, so a refusal
// cannot name the remedy at one member-reachable seam and withhold it at the
// next.
//
// Everything else keeps the seam's own prefix over the underlying error, which
// is the 500 an operator reads, not the member.
func writeCeilingErrorPrefixed(w http.ResponseWriter, prefix string, err error) {
	if errors.Is(err, errGroupsSnapshotStale) {
		writeError(w, http.StatusForbidden, groupsSnapshotStaleMsg)
		return
	}
	writeError(w, http.StatusInternalServerError, prefix+err.Error())
}

// ceilingErrorStatus is writeCeilingError's status half, for the two seams that
// hand a code back up to a caller instead of writing the response themselves.
// One mapping, so a resolver failure cannot answer 403 at one site and 500 at
// the next for the same cause.
func ceilingErrorStatus(err error) int {
	if errors.Is(err, errGroupsSnapshotStale) {
		return http.StatusForbidden
	}
	return http.StatusInternalServerError
}

// effectiveCeiling resolves the ceiling that bounds the caller on ctx.
//
// This is THE consolidation the campaign exists for: fifteen sites read
// Config.DefaultPolicy as "the ceiling", six of them meaning "this principal's
// ceiling", and those six now come through here instead. Resolution order
// mirrors capAllowed's, and each step is a decision rather than a fallback:
//
//  1. OPERATOR ⇒ DefaultPolicy, with NO store read. A super admin IS the
//     ceiling-setting authority, so there is nothing to bound them by — and the
//     short-circuit is also what keeps the ~30 nil-store test doubles in this
//     package alive. DELIBERATELY isOperator and not isSecurityOperator: a
//     security admin authors profiles and is BOUNDED by their own, which is the
//     single assumption governance_grantbound.go's monotone-⊆ bound rests on.
//  2. NO STORE ⇒ DefaultPolicy. A build with no store holds no assignments, so
//     there is nothing to resolve and the answer is 0.6's answer. (Distinct
//     from capAllowed, which errors on a nil store: that resolver is asked
//     about a deployment that HAS capability state and cannot reach it, while
//     this arm is a build with none at all — the capSeamAllowed split, same
//     reasoning.)
//  3. STORE ERROR ⇒ ERROR, and the caller 500s. NEVER fall open to
//     DefaultPolicy: a database hiccup would silently widen every walled
//     principal back to the deployment ceiling, with the run proceeding
//     normally and nothing in the audit saying which ceiling it ran under.
//  4. UNANSWERABLE GROUP SNAPSHOT ⇒ 403, but only in one shape (PF-21/PF-25/
//     PF-26 — see below).
//  5. ErrNotFound ⇒ DefaultPolicy. No assignment matched: absent row, absent
//     behaviour change, the 0042 doctrine.
//  6. ALWAYS Clone. The resolved spec is handed to callers that append to its
//     slices (unionAllowedDomains, the workspace/SCM egress unions); a shallow
//     copy shares the backing array, which is the exact race resolvePolicy's
//     own Clone comment documents — and here the shared value would be a row
//     read fresh per request rather than a process global, so the corruption
//     would be intermittent instead of merely wrong.
//
// ponytail: no cache ACROSS REQUESTS, matching capAllowed's own note — a stale
// ceiling is a security bug, not a slow page. Within ONE request it is memoized
// (see the memo below), which is a different claim: the request is the unit the
// answer must be consistent over.
//
// That memo replaced "PF-13's accepted double resolution", which defended the
// repeated reads on latency grounds — "the alternative buys latency at the cost
// of the one property that matters here, which is that no site can forget to
// ask". The premise was wrong in two ways. Latency was never the cost that
// mattered: three independent, untransacted reads per member create
// (denyMemberGovernance, resolveRunPolicy, filterMemberGrants) plus dispatch's
// fourth can return DIFFERENT ANSWERS if a security admin narrows a profile
// mid-request, and resolveRunPolicy asserts the opposite in words ("a create
// must never resolve two different ceilings for one request"). And the property
// is not lost: every site still asks — the memo just answers.
func (s *Server) effectiveCeiling(ctx context.Context) (governanceCeiling, error) {
	// ONE CEILING PER REQUEST. The memo is checked first and filled on the way
	// out, so every site in one request sees the SAME answer — which is what
	// "a create must never resolve two different ceilings for one request"
	// (resolveRunPolicy) claimed and nothing implemented. A member create alone
	// took THREE independent, uncached, untransacted reads (denyMemberGovernance
	// -> resolveRunPolicy -> filterMemberGrants) and dispatch a fourth, so a
	// security admin narrowing a profile mid-flight — the incident-response
	// action — could land a run whose egress was clamped under the PRE-narrowing
	// ceiling while its grants were filtered under the post-narrowing one.
	//
	// It preserves the property the repeated reads were defended for ("no site
	// can forget to ask"): every site still asks. It just asks the memo first.
	//
	// NOT A CACHE ACROSS REQUESTS, which is the HA blocker effectiveCeiling's own
	// note names: the memo lives on the request context, so it dies with the
	// request and the next one resolves afresh. A background caller (reconcile,
	// the boot heal) carries no memo and resolves normally.
	if memo := ceilingMemoFromContext(ctx); memo != nil {
		return memo.do(ctx, s.resolveEffectiveCeiling)
	}
	return s.resolveEffectiveCeiling(ctx)
}

// resolveEffectiveCeiling is effectiveCeiling's uncached body — the resolution
// order its doc comment describes. Split out so the memo above wraps it exactly
// once and no call site can reach the raw resolve by accident.
func (s *Server) resolveEffectiveCeiling(ctx context.Context) (governanceCeiling, error) {
	deployment := governanceCeiling{Spec: s.cfg.DefaultPolicy.Clone()}
	if s.isOperator(ctx) || s.cfg.Store == nil {
		return deployment, nil
	}

	users, groups, stale := capabilitySubjects(ctx)
	// TRUNCATED COUNTS AS STALE (PF-26). sessionGroups sorts the snapshot and
	// drops its alphabetically-last entries at the cookie byte cap, so a member
	// in enough groups holds a snapshot that is present, non-nil and INCOMPLETE
	// — and the group whose assignment walls them is exactly as likely to be
	// missing as any other. Without this the tier evaporates in silence: no
	// refusal, no audit, just the deployment ceiling.
	if stale || oidcGroupsTruncatedFromContext(ctx) {
		// The unusable half must not be MATCHED against. Passing a truncated
		// list would still let a surviving group's row win, which is not wrong
		// on its own — but it makes the refusal below depend on which groups
		// happened to fit, so the same human with the same claims could be
		// refused or served depending on alphabetical luck. Resolve on the
		// answerable identity only, and let the refusal cover the rest.
		return s.ceilingWithUnusableGroups(ctx, users, deployment)
	}

	p, _, err := s.cfg.Store.ResolveGovernanceProfile(ctx, users, groups)
	return s.ceilingFromProfile(p, err, deployment)
}

// ceilingWithUnusableGroups is effectiveCeiling's step 4: the caller's group
// identity cannot be evaluated, so resolve on their user subjects alone and
// decide whether that answer is trustworthy anyway.
//
// It is trustworthy in exactly two shapes, and the scoping is the whole point
// (a blanket 403 here would lock out every pre-0.6 cookie on every deployment,
// including the ones that have never heard of governance profiles):
//
//   - A USER-TIER row matched (PF-25). user > group > all, so an explicitly
//     named principal's ceiling is FULLY determined no matter what their groups
//     are; refusing would lock out precisely the people an admin took the
//     trouble to name. The tier comes from the resolver's own ORDER BY rather
//     than being re-derived here — a user-tier and an all-tier match are
//     otherwise indistinguishable, including when both name the same profile.
//   - NO GROUP-TIER ROW EXISTS AT ALL (PF-21). Nothing an unknown group could
//     have matched, so nothing a nil snapshot could be hiding; refusing would
//     break "no assignment ⇒ byte-for-byte today" for every pre-upgrade
//     session on every deployment that never adopted group profiles.
//
// HasGroupTierAssignments stays a SEPARATE read, deliberately: the case that
// most needs it is the one where the resolver matched NOTHING, and a zero-row
// result carries no columns to have piggybacked the answer on.
func (s *Server) ceilingWithUnusableGroups(ctx context.Context, users []string, deployment governanceCeiling) (governanceCeiling, error) {
	p, tier, err := s.cfg.Store.ResolveGovernanceProfile(ctx, users, nil)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return governanceCeiling{}, fmt.Errorf("api: resolve governance profile: %w", err)
	}
	if err == nil && tier == types.CapabilitySubjectUser {
		return s.ceilingFromProfile(p, nil, deployment)
	}
	hasGroupTier, herr := s.cfg.Store.HasGroupTierAssignments(ctx)
	if herr != nil {
		return governanceCeiling{}, fmt.Errorf("api: resolve governance profile: %w", herr)
	}
	if hasGroupTier {
		// AUDITED, at the ONE site that produces this refusal.
		//
		// docs/OPERATIONS.md's "Every denial that isn't a 404" makes
		// authz.denied the record of every member denial that is not a plain
		// foreign-resource 404, and this 403 is member-reachable from six seams
		// (GET /policies/default, POST /runs, /runs/preflight, the secrets list,
		// the profile read, the drives door) — and produced none. An operator
		// reading the denial stream saw nothing at all for a member who cannot
		// use the product.
		//
		// HERE rather than in writeCeilingError, and that placement is the fix
		// rather than an implementation detail: writeCeilingError is a free
		// function with no server and no context, and there are three of them
		// (writeCeilingError, writeCeilingErrorPrefixed, ceilingErrorStatus) —
		// auditing at the WRITE sites would mean one emit per seam and a seam
		// that hands the code upward (ceilingErrorStatus) emitting nothing.
		// This is the only place the refusal is DECIDED, so it is the only place
		// it can be recorded once.
		//
		// ONCE PER REQUEST, not once per seam, because effectiveCeiling memoizes
		// (ceilingMemo): a create that asks three times is one denial, which is
		// what an operator counting denials means.
		// Guarded on the SINK, not merely handed to recordAudit's own nil check:
		// auditEvent is evaluated as recordAudit's ARGUMENT, so a server with no
		// recorder would still build the event — and stamp it from cfg.Now,
		// which a Server assembled without New() does not have. Nothing records
		// on such a build by definition, so the cheapest correct thing is not to
		// build the row at all.
		// NOT FOR A DISPLAY READ (isDisplayRead, user_drives_resolve.go). GET
		// /me resolves the ceiling to answer user_drive_denied_by_profile, so
		// this row was being written once per console poll for a member who
		// never asked for a run — a denial count that grew with page views.
		// Every enforcement caller reaches here unmarked and still records.
		if s.cfg.Audit != nil && !isDisplayRead(ctx) {
			s.recordAudit(ctx, s.auditEvent(nil, types.ActorHuman, oidcHumanFromContext(ctx),
				"authz.denied", "governance.ceiling", "denied",
				mustJSON(map[string]any{"reason": "groups_snapshot_stale"})))
		}
		return governanceCeiling{}, errGroupsSnapshotStale
	}
	return s.ceilingFromProfile(p, err, deployment)
}

// ceilingFromProfile turns one resolver answer into a ceiling: ErrNotFound (or
// a nil profile) is the deployment's, anything else is the profile's — cloned,
// and re-intersected against the deployment's eligible grants.
//
// ORDER MATTERS AND IT IS THE WHOLE FUNCTION. The REAL-ERROR check runs FIRST,
// ahead of the nil-profile one, because a failed resolve also returns a nil
// profile: checking nil first would turn every store failure into "no
// assignment matched" and hand the caller the DEPLOYMENT ceiling — the
// fail-open this file exists to refuse, arriving through the back door of a
// convenience guard. The nil check that follows is still worth having: it makes
// a store answering (nil, "", nil) mean the absent-row doctrine rather than a
// dereference, so no caller has to have checked on its behalf.
func (s *Server) ceilingFromProfile(p *types.GovernanceProfile, err error, deployment governanceCeiling) (governanceCeiling, error) {
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return governanceCeiling{}, fmt.Errorf("api: resolve governance profile: %w", err)
	}
	if p == nil {
		return deployment, nil
	}
	spec := p.Ceiling.Clone()
	kept, warns := reintersectGovernanceGrants(spec.EligibleGrants, s.cfg.DefaultPolicy.EligibleGrants, p.Name)
	spec.EligibleGrants = kept
	return governanceCeiling{Spec: spec, Limits: p.Limits, Profile: p, Warnings: warns}, nil
}

// reintersectGovernanceGrants re-applies the monotone-⊆ bound at RESOLVE time
// (PF-22's second half), dropping any profile grant the deployment ceiling no
// longer dominates.
//
// The write-time check is not enough on its own because Config.DefaultPolicy is
// ENV-BORNE: an operator who removes a pairing from WARDYN_DEFAULT_POLICY and
// redeploys has revoked that eligibility for everyone — except that the
// profiles written while it existed are rows, and rows survive redeploys. They
// would go on serving a pairing the deployment no longer provisions, and the
// profile-authoring surface would have become a way to pin credential
// eligibility past the deployer's own revocation.
//
// DROPS WITH A WARNING, never a 500 or a refusal, and the asymmetry is
// deliberate: a write is a caller's own act and can be sent back for
// correction, but a redeploy is somebody ELSE's act arriving between a member's
// two runs. Failing their run for it would turn one operator's env edit into an
// outage for every profile that mentioned the pairing, while dropping the grant
// leaves the run exactly as governed as the operator just asked for and says so
// in the warning list resolveRunPolicy already surfaces on the 201.
func reintersectGovernanceGrants(profile, deployment []types.GrantSpec, profileName string) ([]types.GrantSpec, []string) {
	if len(profile) == 0 {
		return profile, nil
	}
	kept := profile[:0:0]
	var warns []string
	for _, g := range profile {
		if err := governanceGrantWithinCeiling(g, deployment); err != nil {
			warns = append(warns, fmt.Sprintf(
				"governance profile %q: dropped %s grant no longer within the deployment's eligible grants (%v)",
				profileName, g.Kind, err))
			continue
		}
		kept = append(kept, g)
	}
	return kept, warns
}
