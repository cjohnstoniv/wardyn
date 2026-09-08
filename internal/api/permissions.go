// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Permissioning CRUD (0.6 pillar 2, stage A-C): the admin-facing surface over
// capability_grants + capability_enforcement (migration 0042) and the
// member-safe read of a caller's own effective set. The resolver these routes
// feed lives in capabilities.go; this file is only validation + persistence +
// audit for the four writes plus the two reads.
package api

import (
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"unicode"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// maxCapabilityGrantFieldLen bounds subject/value on write. Generous for a
// group name, an email, a host, or an image ref; small enough that a grant
// table stays a table, not a blob store.
const maxCapabilityGrantFieldLen = 512

// permissionsResponse is GET /permissions's body: the whole grant table plus
// the per-kind enforcement switches, in one call — the admin Permissions
// screen's entire data need (plan: "one call").
type permissionsResponse struct {
	Grants      []grantView     `json:"grants"`
	Enforcement map[string]bool `json:"enforcement"`
}

// grantView is a stored grant plus ONE derived bit: whether its value is a
// value the resolver can never be asked about, so the Permissions screen stops
// rendering a rule that protects nothing as if it were active.
//
// A strict SUPERSET of types.CapabilityGrant — every field an existing client
// decodes is still there and `inert` is omitempty, so the console, pkg/client
// and the CLI keep working byte for byte and a client that wants the signal
// reads one more key.
//
// WHY IT EXISTS. capability_grants shipped in v0.6.0 and the per-kind value
// rule (canonicalGrantValue) is a WRITE-boundary rule, so every non-canonical
// row written before it — an uppercase workspace uuid, an uppercase secret
// name, a free-text value where a uuid was meant — survives the upgrade
// unchanged and keeps rendering as an active DENY that has never once fired.
// The upgrade cannot safely rewrite them (a normalizing migration would make an
// inert ALLOW start granting, unreviewed, at boot), and it must not silently
// drop them either. So it SAYS SO, at the surface where an operator is looking
// at the row, and re-saving it through POST /permissions/grants canonicalizes
// it or refuses it by name.
type grantView struct {
	types.CapabilityGrant
	Inert bool `json:"inert,omitempty"`
}

// markInertGrants derives grantView.Inert by asking the write boundary's own
// function what it would store for each row: a row is inert exactly when
// canonicalGrantValue refuses its value or would have stored something else.
// One rule, so the marker cannot drift from the behaviour it describes.
//
// WARNs once per call listing the offenders, because the operator who most
// needs to know may be reading logs rather than the Permissions screen — and
// because the console does not render this field yet.
func (s *Server) markInertGrants(r *http.Request, grants []types.CapabilityGrant) []grantView {
	out := make([]grantView, len(grants))
	var inert []string
	for i, g := range grants {
		out[i] = grantView{CapabilityGrant: g}
		if canonical, err := canonicalGrantValue(g.Capability, g.Value); err != nil || canonical != g.Value {
			out[i].Inert = true
			inert = append(inert, g.Capability+"="+g.Value)
		}
	}
	if len(inert) > 0 {
		slog.WarnContext(r.Context(), "api: capability grants stored before the per-kind value rule can never match anything",
			"grants", len(grants), "inert", len(inert), "values", strings.Join(inert, ", "),
			"remedy", "re-save each row through POST /api/v1/permissions/grants — the write boundary canonicalizes it into the form the resolver compares, or refuses it by name")
	}
	return out
}

// handleGetPermissions returns every capability grant (admin audience — the
// FULL table, unlike GET /me/capabilities below) plus the enforcement switch
// map. operatorOnly (routes.go).
func (s *Server) handleGetPermissions(w http.ResponseWriter, r *http.Request) {
	grants, err := s.cfg.Store.ListCapabilityGrants(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list capability grants: "+err.Error())
		return
	}
	enf, err := s.cfg.Store.GetCapabilityEnforcement(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "get capability enforcement: "+err.Error())
		return
	}
	// The ETag covers ONLY the enforcement map, not Grants: it exists so a
	// caller can round-trip it as PUT /permissions/enforcement's If-Match
	// (etag.go) — that endpoint's own whole-document replace, not this one.
	w.Header().Set("ETag", computeETag(enf))
	writeJSON(w, http.StatusOK, permissionsResponse{Grants: s.markInertGrants(r, grants), Enforcement: enf})
}

// grantWriteRequest is POST /permissions/grants's body. ID/CreatedAt/CreatedBy
// are never accepted from the wire — the natural key (subject_type, subject,
// capability, value) is what a caller names; the row's identity and
// provenance are always server-assigned.
type grantWriteRequest struct {
	SubjectType types.CapabilitySubjectType `json:"subject_type"`
	Subject     string                      `json:"subject"`
	Capability  string                      `json:"capability"`
	Value       string                      `json:"value"`
	Effect      types.CapabilityEffect      `json:"effect"`
}

// controlCharFree reports whether s has no control characters or DEL. Applied
// to every grant field on write: a subject/value round-trips into an admin
// screen and (for egress_host once D-wiring lands) an egress comparison, so
// this is the same cheap hygiene validateSiteConfig applies to URL/host
// fields — not a full per-kind shape validator, which the plan leaves to the
// closed kind set's own resolver-side matching (capValueMatches).
//
// unicode.IsControl covers C0 and DEL exactly as the hand-rolled loop did,
// and ALSO the C1 range U+0080–U+009F — strictly tighter at every call site,
// never looser. That is the stance repoFieldSafe already takes on C1
// (repoclone_test.go rejects a NEL), so this brings the two into line.
func controlCharFree(s string) bool {
	return !strings.ContainsFunc(s, unicode.IsControl)
}

// validateCapabilityGrant normalizes g in place (trims, lowercases the
// subject to match how capabilitySubjects/sessionGroups normalize a caller's
// own identity) and validates it against the closed kind set + the subject
// shape migration 0042 documents. Fail closed: an unknown kind, an invalid
// subject_type/effect, or an empty value/subject is rejected outright rather
// than stored inert.
func validateCapabilityGrant(g *types.CapabilityGrant) error {
	if !g.SubjectType.Valid() {
		return fmt.Errorf("subject_type: invalid %q", g.SubjectType)
	}
	if !validCapabilityKind(g.Capability) {
		return fmt.Errorf("capability: unknown kind %q", g.Capability)
	}
	if !g.Effect.Valid() {
		return fmt.Errorf("effect: invalid %q", g.Effect)
	}
	g.Value = strings.TrimSpace(g.Value)
	if g.Value == "" {
		return fmt.Errorf("value: required")
	}
	if len(g.Value) > maxCapabilityGrantFieldLen || !controlCharFree(g.Value) {
		return fmt.Errorf("value: invalid")
	}
	// THE VALUE, canonicalized per kind by the ONE function the read side also
	// asks (canonicalGrantValue below): a value that can never match anything
	// the resolver will be asked about is refused or folded here, never stored
	// 201-Created to render as an active rule that protects nothing.
	v, verr := canonicalGrantValue(g.Capability, g.Value)
	if verr != nil {
		return verr
	}
	g.Value = v
	if g.SubjectType == types.CapabilitySubjectAll {
		// "all" names every signed-in human; the migration is explicit that
		// subject is '' for this type, so a caller-supplied value is dropped
		// rather than trusted (there is nothing for it to legitimately name).
		g.Subject = ""
		return nil
	}
	// user/group: canonicalized the SAME way the caller's own identities are
	// (capabilitySubjects, sessionGroups) — a grant written "Alice@Corp.com"
	// must still hit the folded sub/email the resolver compares against, and
	// each half asks the MATCH surface's own function rather than restating it.
	g.Subject = strings.TrimSpace(g.Subject)
	if g.Subject == "" {
		return fmt.Errorf("subject: required for subject_type %q", g.SubjectType)
	}
	if g.SubjectType == types.CapabilitySubjectGroup {
		// A GROUP subject is matched by exact equality against the login-time
		// snapshot, and that snapshot is strictly narrower than "lowercase it":
		// sessionGroups can only carry printable ASCII, checked BEFORE the fold.
		// So the write surface asks the match surface itself — one function,
		// oidc.CanonicalGroupSubject — rather than a second, looser spelling of
		// the same rule. Without it a subject no session can ever produce is
		// stored 201-Created and rendered on the Permissions screen as active
		// while it matches nobody: a DENY that protects nothing (the failure
		// the egress_host arm above added ValidDomainEntry to close, on the
		// VALUE half of the identical record), and a group tier that
		// HasGroupTierAssignments still counts as present.
		subject, ok := oidc.CanonicalGroupSubject(g.Subject)
		if !ok {
			return fmt.Errorf("subject: must be printable ASCII — a group subject is matched against the login-time group snapshot, which carries printable ASCII only, so this value can never match anyone")
		}
		g.Subject = subject
	} else {
		// A USER subject gets canonicalUserSubject, not a bare ToLower. The
		// fold is UNICODE: U+212A folds to ASCII 'k' and U+0130 to ASCII 'i',
		// so a plain lowercase stored an admin's "Kim@Korp.com" (crafted K's)
		// as "kim@korp.com" — binding a DENY, or a governance profile, to a
		// real human the author never named, and to the exact string that
		// human's own claims resolve to. Same function as the read side, so
		// what a caller can BE is what this can store.
		g.Subject = canonicalUserSubject(g.Subject)
	}
	if len(g.Subject) > maxCapabilityGrantFieldLen || !controlCharFree(g.Subject) {
		return fmt.Errorf("subject: invalid")
	}
	return nil
}

// canonicalGrantValue is the ONE per-kind rule for a capability grant VALUE: it
// returns the exact string the resolver will be asked to match, or an error
// when the given value can never match anything at all.
//
// TWO CALLERS, WHICH IS THE POINT. validateCapabilityGrant applies it at the
// write boundary, and handleGetPermissions asks it about a STORED row to mark
// the rows an older Wardyn accepted before this rule existed (grantView.Inert).
// A row is inert exactly when this function refuses it or would have stored
// something else — so the marker cannot drift from the rule, and a new kind
// gets both behaviours from one place.
//
// PER KIND, and each arm states what the resolver is actually asked about:
//
//   - egress_host: the proxy's own entry grammar (proxy.ValidDomainEntry), the
//     same check every allowed_domains ingest runs. Without it "*example.com"
//     stored fine and then covered "evilexample.com" (entryCoversAny is a bare
//     suffix test), and a mid-label or URL-shaped value stored as a row that can
//     never match — a deny that protects nothing.
//
//   - workspace: uuid.Parse, stored as .String(). The resolver compares
//     capValueMatches' `grantValue == want` against uuid.UUID.String(), which is
//     always canonical lowercase-hyphenated, so all four alternative spellings
//     uuid.Parse accepts were stored 201-Created, rendered as an active DENY,
//     and matched nothing. CANONICALIZED rather than refused, because an admin
//     who pastes a braced id from a tool means the workspace — and folding also
//     collapses five spellings onto one natural key instead of five rows each
//     claiming to govern the same workspace. A value uuid.Parse cannot read at
//     all IS refused: it can never name a workspace.
//
//   - secret and integration: LOWERCASED, then held to the grammar their own
//     rows are held to (secretNameRE, integrationRefRE — both lowercase-only).
//     This arm's comment used to say the opposite ("lowercasing a secret name
//     here would stop it matching the row secrets.go stores"), and the premise
//     was inverted: secrets.go cannot store an uppercase name at all, so an
//     uppercase capSecret DENY was byte-for-byte the same inert row the
//     workspace arm exists to prevent, and folding can only ever make the grant
//     match the row the author meant. The ASCII guard runs BEFORE the fold, the
//     same order canonicalUserSubject and oidc.CanonicalGroupSubject use: a
//     non-ASCII value can never name one of these rows, and folding first would
//     let U+212A land on an ASCII name the author never typed.
//
//   - agent and image: STORED VERBATIM, and that is a decision rather than an
//     omission. An agent id is not held to a closed catalog at the run boundary
//     (a BYOA run names its own), and an image ref's tag may legitimately carry
//     uppercase, so neither vocabulary is provably narrower than what is typed
//     — folding them would be this function inventing a canonical form the
//     resolver does not use.
//
// The "*" wildcard is this table's own spelling for "every value of this kind"
// (capValueMatches short-circuits on it), not a value of any kind, so it is
// exempt everywhere.
func canonicalGrantValue(capability, value string) (string, error) {
	v := strings.TrimSpace(value)
	if v == "" {
		return "", fmt.Errorf("value: required")
	}
	if len(v) > maxCapabilityGrantFieldLen || !controlCharFree(v) {
		return "", fmt.Errorf("value: invalid")
	}
	if v == capWildcard {
		return v, nil
	}
	switch capability {
	case capEgressHost:
		if err := proxy.ValidDomainEntry(v); err != nil {
			return "", fmt.Errorf("value: %w", err)
		}
	case capWorkspace:
		id, err := uuid.Parse(v)
		if err != nil {
			return "", fmt.Errorf("value: %q is not a workspace id — a workspace capability names a workspace by uuid, and the resolver compares it exactly, so a value it cannot read can never match anything", v)
		}
		return id.String(), nil
	case capSecret, capIntegration:
		grammar, what := secretNameRE, "secret name"
		if capability == capIntegration {
			grammar, what = integrationRefRE, "integration id"
		}
		if !oidc.ASCIIOnly(v) {
			return "", fmt.Errorf("value: %q is not a %s — one is written in lowercase ASCII, so this value can never match a stored row", v, what)
		}
		lowered := strings.ToLower(v)
		if !grammar.MatchString(lowered) {
			return "", fmt.Errorf("value: %q is not a %s — the resolver compares it exactly against a row whose own name rule this value cannot satisfy, so it can never match anything", v, what)
		}
		return lowered, nil
	}
	return v, nil
}

// handleUpsertCapabilityGrant creates or re-grants one row, keyed on the
// natural (subject_type, subject, capability, value). A conflict FLIPS the
// existing row's effect in place (store.UpsertCapabilityGrant) rather than
// leaving two contradictory rows — the response status tells the caller which
// happened: 201 for a genuinely new row, 200 when an existing one was
// updated (the console's DUPLICATE copy). operatorOnly (routes.go).
func (s *Server) handleUpsertCapabilityGrant(w http.ResponseWriter, r *http.Request) {
	var req grantWriteRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	g := types.CapabilityGrant{
		SubjectType: req.SubjectType,
		Subject:     req.Subject,
		Capability:  req.Capability,
		Value:       req.Value,
		Effect:      req.Effect,
	}
	if err := validateCapabilityGrant(&g); err != nil {
		writeError(w, http.StatusBadRequest, "invalid grant: "+err.Error())
		return
	}
	// A fresh candidate id: UpsertCapabilityGrant returns the EXISTING row's id
	// on a natural-key conflict, never this one — comparing the two is how the
	// handler tells created from updated without a separate existence read.
	g.ID = uuid.New()
	g.CreatedBy = principalFromRequest(r)
	saved, err := s.cfg.Store.UpsertCapabilityGrant(r.Context(), g)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "upsert capability grant: "+err.Error())
		return
	}
	action, status := "capability.grant.created", http.StatusCreated
	if saved.ID != g.ID {
		action, status = "capability.grant.updated", http.StatusOK
	}
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		action, saved.ID.String(), "success", mustJSON(map[string]any{
			"subject_type": saved.SubjectType,
			"subject":      saved.Subject,
			"capability":   saved.Capability,
			"value":        saved.Value,
			"effect":       saved.Effect,
		})))
	writeJSON(w, status, saved)
}

// handleDeleteCapabilityGrant removes one grant by id. operatorOnly
// (routes.go) — there is no owning principal to scope this to, so a 404 on an
// unknown id is the whole story (no existence oracle to protect: an admin
// already sees the full table via GET /permissions).
func (s *Server) handleDeleteCapabilityGrant(w http.ResponseWriter, r *http.Request) {
	id, ok := parseIDParam(w, r, "id", "capability grant")
	if !ok {
		return
	}
	if err := s.cfg.Store.DeleteCapabilityGrant(r.Context(), id); err != nil {
		if notFoundIf(w, err, "capability grant") {
			return
		}
		writeError(w, http.StatusInternalServerError, "delete capability grant: "+err.Error())
		return
	}
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		"capability.grant.deleted", id.String(), "success", nil))
	w.WriteHeader(http.StatusNoContent)
}

// handlePutCapabilityEnforcement replaces the WHOLE per-kind switch map — the
// body is exactly the JSON shape GetCapabilityEnforcement returns, and
// PutCapabilityEnforcement's own doc explains why omitting a key is a real
// "turn it off": absent == not enforced everywhere else this state is read.
// Every key must be one of the four known kinds — validated here rather than
// left inert, matching the closed-kind write-boundary rule the grant handler
// above already applies. operatorOnly (routes.go); its own table, never
// SiteConfig, so a stale client round-tripping an older document can never
// silently disable this (see migration 0042's comment).
//
// If-Match (etag.go) is optional optimistic concurrency on top of this
// whole-map replace: an absent header behaves exactly as before, a present
// one that no longer matches GET /permissions's current Enforcement ETag is
// refused with 412 before the write reaches the store. capEnforcementMu
// (server.go) makes the check-then-write atomic against a second overlapping
// PUT on this process.
func (s *Server) handlePutCapabilityEnforcement(w http.ResponseWriter, r *http.Request) {
	var body map[string]bool
	if !decodeStrict(w, r, &body) {
		return
	}
	for kind := range body {
		if !validCapabilityKind(kind) {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("unknown capability kind %q", kind))
			return
		}
	}
	s.capEnforcementMu.Lock()
	defer s.capEnforcementMu.Unlock()
	existing, err := s.cfg.Store.GetCapabilityEnforcement(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "get existing capability enforcement: "+err.Error())
		return
	}
	if !ifMatchSatisfied(r, computeETag(existing)) {
		writeError(w, http.StatusPreconditionFailed,
			"If-Match does not match the current capability enforcement map — GET /permissions again and retry")
		return
	}
	saved, err := s.cfg.Store.PutCapabilityEnforcement(r.Context(), body)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "put capability enforcement: "+err.Error())
		return
	}
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		"capability.enforcement.write", "capability_enforcement", "success", mustJSON(saved)))
	w.Header().Set("ETag", computeETag(saved))
	writeJSON(w, http.StatusOK, saved)
}

// meCapabilitiesResponse is GET /me/capabilities's body: what the CALLER
// personally holds, never the full admin table (that stays behind
// GET /permissions). Grants is exactly ListCapabilityGrantsFor(users, groups)
// — the same subject resolution capAllowed itself uses — so the console can
// answer "am I granted X" the identical way the server would, without a
// dedicated per-value probe endpoint.
type meCapabilitiesResponse struct {
	Grants              []types.CapabilityGrant `json:"grants"`
	Enforcement         map[string]bool         `json:"enforcement"`
	SessionGroups       []string                `json:"session_groups"`
	GroupsSnapshotStale bool                    `json:"groups_snapshot_stale"`
}

// handleMeCapabilities is the member-safe twin of GET /permissions: it sits on
// the plain /me block (classMember, not operatorOnly — see routes.go), and
// answers only for the caller's OWN subjects. GroupsSnapshotStale surfaces the
// nil-vs-empty distinction capabilitySubjects documents: a pre-0.6 cookie or a
// session with no group claim at all must read as "can't tell yet", not as
// silently holding no group grants.
func (s *Server) handleMeCapabilities(w http.ResponseWriter, r *http.Request) {
	users, groups, stale := capabilitySubjects(r.Context())
	grants, err := s.cfg.Store.ListCapabilityGrantsFor(r.Context(), users, groups)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list capability grants: "+err.Error())
		return
	}
	enf, err := s.cfg.Store.GetCapabilityEnforcement(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "get capability enforcement: "+err.Error())
		return
	}
	// CreatedBy names the ADMIN who wrote the row (principal or email). A member
	// needs to know WHAT they hold, never which colleague signed it — and the
	// field is `omitempty`, so blanking it drops it from the body rather than
	// shipping an empty string. GET /permissions (operatorOnly) still carries it.
	for i := range grants {
		grants[i].CreatedBy = ""
	}
	writeJSON(w, http.StatusOK, meCapabilitiesResponse{
		Grants:              grants,
		Enforcement:         enf,
		SessionGroups:       groups,
		GroupsSnapshotStale: stale,
	})
}
