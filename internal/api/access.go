// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Access / role-mapping CRUD (Phase 2 lane A, migration 0051): the console's
// Getting Started -> People editor over the store half of
// internal/auth/oidc's RoleMappingSource. Modeled on permissions.go's shape —
// one read that shows the whole picture, small validated writes, an audit
// event per write — but with two guards permissions.go has no analogue for:
// this table decides who derives ADMIN AT ALL, so a bad write here can either
// silently widen access (a collision with the operator allowlist) or lock the
// acting admin out of their own deployment (see the posture-flip and lockout
// guards below).
package api

import (
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// accessDeniedRole is the JSON spelling of "no role at all" — deriveRole's
// ok=false outcome. Never a real oidc role constant (RoleAdmin/RoleMember),
// deliberately, so a client can tell "this caller derives no role" apart from
// any string the role map could actually produce.
const accessDeniedRole = "denied"

// accessEmailKeyRefused is EMAIL_KEY_REFUSED — the frozen string the Q7
// adjudication (docs/design/people-access-prompt.md) creates, rendered
// verbatim on a POST /access/mappings refused for carrying an email-shaped
// value with Config.AllowEmailMappings unset. Byte-for-byte per the
// adjudication; do not reword without updating that doc too.
const accessEmailKeyRefused = "Email mappings are disabled on this install. Map an App Role or group instead, or opt in with WARDYN_OIDC_ALLOW_EMAIL_MAPPINGS in your chart."

// accessStaleSnapshot (A-1) is the lockout guard's OTHER refusal: a caller
// whose login-time claim snapshot (oidcGroupsFromContext) cannot even
// reproduce the admin role they demonstrably hold right now — because their
// admin-granting group fell off the 2048-byte groups-snapshot truncation
// (sessionGroups), or because their session cookie predates snapshots
// entirely (Groups nil) — gets THIS message, never the lockout one: the
// snapshot is too stale to answer "would this write remove your admin
// access" at all, so refusing on ITS say-so would be a false lockout, not a
// caught one.
const accessStaleSnapshot = "your sign-in is too old to verify this change — sign in again before changing role mappings"

// mountAccessRoutes registers the People-step access surface. ALL FOUR routes
// are operatorOnly, including the two reads: unlike /permissions (a member
// gets a scoped read via GET /me/capabilities), there is no member-safe view
// of role mappings — the console's People step is an admin surface end to
// end, so there is nothing to carve a member-scoped twin out of.
func (s *Server) mountAccessRoutes(operatorOnly chi.Router) {
	operatorOnly.Get("/access", s.handleGetAccess)
	operatorOnly.Post("/access/mappings", s.handleUpsertRoleMapping)
	operatorOnly.Delete("/access/mappings/{id}", s.handleDeleteRoleMapping)
	operatorOnly.Post("/access/preview", s.handlePreviewRole)
}

// requireOIDC writes the 503 idiom injection.go's broker-not-configured uses
// (writeError + "<noun> not configured") and reports whether the caller
// should return immediately. Every /access route needs this FIRST: with no
// OIDC configured there is no role derivation to manage, and every accessor
// below (ChartRoleMap, PreviewRole, ...) is a method on s.cfg.OIDC.
func (s *Server) requireOIDC(w http.ResponseWriter) bool {
	if s.cfg.OIDC != nil {
		return true
	}
	writeError(w, http.StatusServiceUnavailable, "SSO is not configured")
	return false
}

// ─── GET /access ───────────────────────────────────────────────────────────

type accessMappingView struct {
	ID          string     `json:"id,omitempty"`
	Value       string     `json:"value"`
	Role        string     `json:"role"`
	Source      string     `json:"source"` // "chart" | "console"
	Shadowed    bool       `json:"shadowed"`
	ShadowCause string     `json:"shadow_cause"` // "" | "chart" | "operator_allowlist"
	CreatedAt   *time.Time `json:"created_at,omitempty"`
	CreatedBy   string     `json:"created_by,omitempty"`
}

type accessPosture struct {
	MapEmpty bool   `json:"map_empty"`
	Before   string `json:"before"`
	After    string `json:"after"`
	Changes  bool   `json:"changes"`
}

type accessResponse struct {
	Mappings              []accessMappingView `json:"mappings"`
	DefaultRole           string              `json:"default_role"`
	OperatorEmailsPresent bool                `json:"operator_emails_present"`
	// OperatorEmails (A-3) is the ADDRESSES themselves — api.Config.OperatorEmails,
	// the same list fed into oidc.Config.LegacyAdminEmails at boot — so the
	// console's Defaults block can render the addresses, not just the bool
	// above (which stays for the guard-note logic that only needs presence).
	OperatorEmails []string `json:"operator_emails"`
	// AllowEmailMappings mirrors Config.AllowEmailMappings (Q7 adjudication) —
	// the console derives email-ness of a row from "@" in its value itself
	// (no new per-row field), and uses this alongside that to render the
	// §7.2 warn badge / the opt-in state, matching what a write would accept.
	AllowEmailMappings bool `json:"allow_email_mappings"`
	// EmailDomainsConfigured (A-4) reports whether WARDYN_OIDC_EMAIL_DOMAINS
	// is set (oidc.Authenticator.HasEmailDomains) — the EMAIL_KEY badge copy
	// depends on this, and the response otherwise cannot express it.
	EmailDomainsConfigured bool          `json:"email_domains_configured"`
	Posture                accessPosture `json:"posture"`
}

// accessRolePosture computes the arm-1-vs-arm-2 outcome deriveRole's own
// precedence doc lays out, from ONLY the two exported accessors that decide
// it: before is what an UNMATCHED signed-in human gets when the role map is
// EMPTY (arm 1 — HasOperatorEmails' allowlist split, or admin-for-everyone
// with neither configured), after is what one gets once the role map is
// NON-EMPTY and nothing in it matched (arm 2 — DefaultRole, or accessDeniedRole
// when unset). changes names the one setting that is worth a UI callout:
// adding the FIRST mapping (or removing the LAST) can silently move every
// unmatched human from one of these outcomes to the other.
func accessRolePosture(a *oidc.Authenticator) (before, after string, changes bool) {
	before = oidc.RoleAdmin
	if a.HasOperatorEmails() {
		before = oidc.RoleMember
	}
	after = accessDeniedRole
	if dr := a.DefaultRole(); dr != "" {
		after = dr
	}
	return before, after, before != after
}

// accessMappingsView merges chart rows (ChartRoleMap) and console rows into
// GET /access's one table, computing shadowed/shadow_cause with the SAME two
// rules mergeRoleMaps applies at login (chart collision, then the operator
// allowlist) — mergeRoleMaps itself is unexported, so this replicates its
// rule ORDER using the two accessors oidc exports for exactly this purpose
// (ChartRoleMap, IsOperatorEmail). A chart row is never shadowed — the chart
// always wins a collision, by construction. Sorted by value then source so
// the response is deterministic across calls with the same underlying state.
func accessMappingsView(chart map[string]string, rows []types.RoleMapping, a *oidc.Authenticator) []accessMappingView {
	out := make([]accessMappingView, 0, len(chart)+len(rows))
	for value, role := range chart {
		out = append(out, accessMappingView{Value: value, Role: role, Source: "chart"})
	}
	for _, m := range rows {
		mv := accessMappingView{
			ID: m.ID.String(), Value: m.Value, Role: m.Role, Source: "console",
			CreatedBy: m.CreatedBy,
		}
		if !m.CreatedAt.IsZero() {
			createdAt := m.CreatedAt
			mv.CreatedAt = &createdAt
		}
		switch {
		case chart[m.Value] != "":
			mv.Shadowed, mv.ShadowCause = true, "chart"
		case a.IsOperatorEmail(m.Value):
			mv.Shadowed, mv.ShadowCause = true, "operator_allowlist"
		}
		out = append(out, mv)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Value != out[j].Value {
			return out[i].Value < out[j].Value
		}
		return out[i].Source < out[j].Source
	})
	return out
}

// handleGetAccess returns the console's whole People-step data need in one
// call: every chart + console role mapping (with collision/shadow provenance
// a human can act on), the boot-time DefaultRole/operator-allowlist posture,
// and the same before/after/changes pair the write guards below evaluate —
// so the UI can show the SAME warning the guards would enforce, before an
// admin ever attempts the write that trips them.
func (s *Server) handleGetAccess(w http.ResponseWriter, r *http.Request) {
	if !s.requireOIDC(w) {
		return
	}
	rows, err := s.cfg.Store.ListRoleMappings(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list role mappings: "+err.Error())
		return
	}
	chart := s.cfg.OIDC.ChartRoleMap()
	before, after, changes := accessRolePosture(s.cfg.OIDC)
	// A nil slice marshals to JSON null, but the field is typed string[] on the
	// wire and the console reads its .length — so an install with no operator
	// emails must still send [], never null.
	operatorEmails := s.cfg.OperatorEmails
	if operatorEmails == nil {
		operatorEmails = []string{}
	}
	writeJSON(w, http.StatusOK, accessResponse{
		Mappings:               accessMappingsView(chart, rows, s.cfg.OIDC),
		DefaultRole:            s.cfg.OIDC.DefaultRole(),
		OperatorEmailsPresent:  s.cfg.OIDC.HasOperatorEmails(),
		OperatorEmails:         operatorEmails,
		AllowEmailMappings:     s.cfg.AllowEmailMappings,
		EmailDomainsConfigured: s.cfg.OIDC.HasEmailDomains(),
		Posture: accessPosture{
			// MapEmpty (A-5) is the REAL merged-map emptiness (chart + rows,
			// with a shadowed row contributing nothing) — not a raw row
			// count, which diverges whenever a stored row collides with the
			// chart or the operator allowlist.
			MapEmpty: s.cfg.OIDC.MergedMapEmpty(toOIDCRoleMappings(rows)),
			Before:   before, After: after, Changes: changes,
		},
	})
}

// toOIDCRoleMappings converts store rows to the oidc package's own
// RoleMapping shape — the input every derivation accessor
// (PreviewRoleAgainst, MergedMapEmpty) takes, so a handler with a fresh
// []types.RoleMapping read never has to hand-roll the per-field copy.
func toOIDCRoleMappings(rows []types.RoleMapping) []oidc.RoleMapping {
	out := make([]oidc.RoleMapping, len(rows))
	for i, m := range rows {
		out[i] = oidc.RoleMapping{Value: m.Value, Role: m.Role}
	}
	return out
}

// accessUnmatchedOutcome is the ONE derivation both write-guards and the GET
// posture display use to ask "what role does an UNMATCHED signed-in human
// get against rows" (A-5): PreviewRoleAgainst with no roles/groups/email,
// mapped to accessDeniedRole on ok=false. Routing every caller through
// mergeRoleMaps+deriveRole this way — rather than a hand-mirrored arm
// computation keyed on raw row counts — means it can never diverge from what
// a real login would decide, including when a stored row is shadowed.
func (s *Server) accessUnmatchedOutcome(rows []oidc.RoleMapping) string {
	role, ok := s.cfg.OIDC.PreviewRoleAgainst(rows, nil, nil, "")
	if !ok {
		return accessDeniedRole
	}
	return role
}

// ─── shared: canonicalization, candidate-map construction, guards ─────────

// isASCII reports whether s contains no byte above ASCII — the same
// byte-level test internal/auth/oidc's asciiOnly applies (a multi-byte UTF-8
// rune's bytes are all >= 0x80, so this agrees with a rune-level check).
func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] > 0x7f {
			return false
		}
	}
	return true
}

// canonicalRoleMapValue trims+lowers value and validates it against EXACTLY
// the contract mergeRoleMaps enforces on a console row (RoleMapping's own doc
// comment): non-empty, ASCII (matching is ASCII-only — a non-ASCII value can
// never match a claim, see oidc's asciiOnly), and free of control characters
// (the same hygiene validateCapabilityGrant applies to its own Value field —
// an empty/control-char value stored raw would be either a dead key or,
// worse for whitespace, one that matches ANY empty/whitespace claim).
func canonicalRoleMapValue(value string) (string, error) {
	v := strings.ToLower(strings.TrimSpace(value))
	if v == "" {
		return "", fmt.Errorf("value: required")
	}
	if len(v) > maxCapabilityGrantFieldLen || !controlCharFree(v) {
		return "", fmt.Errorf("value: invalid")
	}
	if !isASCII(v) {
		return "", fmt.Errorf("value: must be ASCII — matching is ASCII-only, a non-ASCII value can never match a claim")
	}
	return v, nil
}

// accessCollisionBody (A-2) is POST /access/mappings' collision 400: a
// candidate value that already resolves through boot-time config always
// wins the same collision at login (mergeRoleMaps), so the write is refused
// — structured, keyed on Cause, reusing the exact "chart" | "operator_allowlist"
// vocabulary accessMappingView.ShadowCause already freezes for GET /access,
// rather than two hand-frozen prose strings a client would have to
// substring-match to tell apart.
type accessCollisionBody struct {
	Error string `json:"error"`
	Cause string `json:"cause"` // "chart" | "operator_allowlist"
	Value string `json:"value"`
}

// accessCollisionCause reports WHICH boot-time source (if any) a candidate
// value already collides with — "" means no collision. Chart is checked
// first: a value can collide with both (a chart row happens to also be an
// operator email), and the chart is the more specific, more actionable
// source to name.
func accessCollisionCause(value string, chart map[string]string, a *oidc.Authenticator) string {
	if chart[value] != "" {
		return "chart"
	}
	if a.IsOperatorEmail(value) {
		return "operator_allowlist"
	}
	return ""
}

func writeAccessCollision(w http.ResponseWriter, value, cause string) {
	source := "WARDYN_OIDC_ROLE_MAP"
	if cause == "operator_allowlist" {
		source = "WARDYN_OIDC_OPERATOR_EMAILS"
	}
	writeJSON(w, http.StatusBadRequest, accessCollisionBody{
		Error: fmt.Sprintf("value %q is already set by your chart config (%s) and cannot be overridden here", value, source),
		Cause: cause,
		Value: value,
	})
}

// accessCandidateRows builds the role-mapping set AS IT WOULD BE after a
// proposed write — replace/add valueRole (role == "" means: this write is a
// DELETE, drop the row instead), leaving every other existing row untouched —
// so the lockout guard can hand it to PreviewRoleAgainst and ask "would the
// acting admin still derive admin AFTER this write" without a second store
// round trip and without reimplementing the upsert-or-delete semantics twice.
func accessCandidateRows(existing []types.RoleMapping, id, value, role string) []oidc.RoleMapping {
	out := make([]oidc.RoleMapping, 0, len(existing)+1)
	replaced := false
	for _, m := range existing {
		if (id != "" && m.ID.String() == id) || (id == "" && m.Value == value) {
			if role != "" {
				out = append(out, oidc.RoleMapping{Value: value, Role: role})
				replaced = true
			}
			continue // delete: drop this row from the candidate set
		}
		out = append(out, oidc.RoleMapping{Value: m.Value, Role: m.Role})
	}
	if role != "" && !replaced {
		out = append(out, oidc.RoleMapping{Value: value, Role: role})
	}
	return out
}

// accessLockoutErr is POST/DELETE /access/mappings' LOCKOUT GUARD: an
// SSO-human caller (never an admin-token/local-mode caller — see below) whose
// own candidate-map derivation would no longer come out admin is refused,
// checked against the SAME login-time claim snapshot every other capability
// grant in this deployment already trusts (oidcGroupsFromContext) — there is
// no fresher claim set to check against short of forcing a re-login.
//
// Admin-token and local-mode callers are EXEMPT (oidcHumanFromContext ==
// "") — that is deliberate, not an oversight: they carry no per-human role to
// demote (see isOperator's own doc), and refusing them here would remove the
// only remaining path to UNDO a bad mapping once every SSO admin has already
// locked themselves out. That exemption IS the recovery path.
//
// A-1: the snapshot itself can be too stale to trust — an admin whose
// admin-granting group fell off the 2048-byte groups-snapshot truncation
// (sessionGroups), or whose cookie predates snapshots entirely (Groups nil),
// would derive NON-admin against the snapshot regardless of what the write
// does, which used to trip the lockout message on every write (false
// positive). Fixed by checking roleBefore — the SAME snapshot run against
// the EXISTING rows, before this write — first: only when roleBefore is
// genuinely admin does a roleAfter that comes out non-admin mean the WRITE
// caused the demotion (the real lockout); when roleBefore already isn't
// admin, the snapshot cannot reproduce the admin access the caller
// demonstrably holds (they got past requireOperator to reach this handler at
// all), so it gets the distinct accessStaleSnapshot refusal instead — never
// silently allowed, since a snapshot too stale to verify a NO-OP write is
// too stale to verify a real demotion either.
func (s *Server) accessLockoutErr(r *http.Request, existing []types.RoleMapping, candidate []oidc.RoleMapping) error {
	sub := oidcHumanFromContext(r.Context())
	if sub == "" {
		return nil
	}
	groups, email := oidcGroupsFromContext(r.Context()), oidcEmailFromContext(r.Context())
	roleBefore, ok := s.cfg.OIDC.PreviewRoleAgainst(toOIDCRoleMappings(existing), nil, groups, email)
	if !ok || roleBefore != oidc.RoleAdmin {
		return errors.New(accessStaleSnapshot)
	}
	roleAfter, ok := s.cfg.OIDC.PreviewRoleAgainst(candidate, nil, groups, email)
	if !ok || roleAfter != oidc.RoleAdmin {
		return fmt.Errorf("this change would remove your own admin access (checked against your last sign-in)")
	}
	return nil
}

// accessPostureFlipBody is the structured 400 body both posture-flip guards
// write when the caller has not passed acknowledge_access_change — carries
// the same before/after the GET /access response already shows, so the
// console can render the identical warning inline instead of a bare message.
type accessPostureFlipBody struct {
	Error                   string `json:"error"`
	RequiredAcknowledgement bool   `json:"required_acknowledgement"`
	Before                  string `json:"before"`
	After                   string `json:"after"`
}

func writeAccessPostureFlip(w http.ResponseWriter, before, after string) {
	writeJSON(w, http.StatusBadRequest, accessPostureFlipBody{
		Error:                   fmt.Sprintf("this change moves every unmatched signed-in human from %q to %q — pass acknowledge_access_change=true to confirm", before, after),
		RequiredAcknowledgement: true,
		Before:                  before, After: after,
	})
}

// ─── POST /access/mappings ─────────────────────────────────────────────────

type roleMappingWriteRequest struct {
	Value                   string `json:"value"`
	Role                    string `json:"role"`
	AcknowledgeAccessChange bool   `json:"acknowledge_access_change,omitempty"`
}

// handleUpsertRoleMapping creates or re-adds one console row, keyed on the
// natural UNIQUE (value) — a conflict flips the existing row's role in place
// (store.UpsertRoleMapping), same 201-new/200-updated status split
// handleUpsertCapabilityGrant uses. Four gates run, in order, before the
// store is ever touched: shape/canonicalization, the chart/operator
// collision, the Q7 email-mapping opt-in, and the posture-flip guard —
// which fires whenever this write actually moves the unmatched-human
// outcome (accessUnmatchedOutcome over existing vs. candidate rows, A-5),
// not merely on a first-console-row precondition. The lockout guard runs
// last, against the write's actual candidate outcome.
func (s *Server) handleUpsertRoleMapping(w http.ResponseWriter, r *http.Request) {
	if !s.requireOIDC(w) {
		return
	}
	var req roleMappingWriteRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	value, err := canonicalRoleMapValue(req.Value)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !oidc.ValidRole(req.Role) {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("role: invalid %q (want %q or %q)", req.Role, oidc.RoleAdmin, oidc.RoleMember))
		return
	}
	chart := s.cfg.OIDC.ChartRoleMap()
	if cause := accessCollisionCause(value, chart, s.cfg.OIDC); cause != "" {
		writeAccessCollision(w, value, cause)
		return
	}
	// Q7 adjudication (docs/design/people-access-prompt.md): an email-shaped
	// CONSOLE value is refused unless the org opted in
	// (WARDYN_OIDC_ALLOW_EMAIL_MAPPINGS) — an SSO/Entra deployment's default
	// posture steers an admin to an App Role or group key instead. env
	// WARDYN_OIDC_ROLE_MAP's own email-keyed entries are UNAFFECTED (legacy,
	// boot-warned separately — see buildOptionalFeatures). Checked AFTER the
	// collision check on purpose: an email value that already collides with
	// the chart/operator allowlist (e.g. an operator's own address) gets that
	// more specific, more actionable refusal — not a generic "email mappings
	// are off" that would be true but beside the point.
	if strings.Contains(value, "@") && !s.cfg.AllowEmailMappings {
		writeError(w, http.StatusBadRequest, accessEmailKeyRefused)
		return
	}

	existing, err := s.cfg.Store.ListRoleMappings(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list role mappings: "+err.Error())
		return
	}
	candidate := accessCandidateRows(existing, "", value, req.Role)

	// POSTURE-FLIP GUARD (A-5): fires iff this write actually moves the
	// unmatched-human outcome — derived from the REAL merged map via
	// accessUnmatchedOutcome (existing rows vs. candidate rows), not a raw
	// row-count precondition, which used to miss a flip whenever a stored
	// row was shadowed (see accessUnmatchedOutcome's doc).
	if !req.AcknowledgeAccessChange {
		before := s.accessUnmatchedOutcome(toOIDCRoleMappings(existing))
		after := s.accessUnmatchedOutcome(candidate)
		if before != after {
			writeAccessPostureFlip(w, before, after)
			return
		}
	}

	if lerr := s.accessLockoutErr(r, existing, candidate); lerr != nil {
		writeError(w, http.StatusBadRequest, lerr.Error())
		return
	}

	// A fresh candidate id: UpsertRoleMapping returns the EXISTING row's id on
	// a natural-key (value) conflict, never this one — comparing the two is
	// how the handler tells created from updated, the same trick
	// handleUpsertCapabilityGrant uses (that table's natural key includes a
	// caller-supplied id too, but the comparison works identically here).
	m := types.RoleMapping{ID: uuid.New(), Value: value, Role: req.Role, CreatedBy: principalFromRequest(r)}
	saved, err := s.cfg.Store.UpsertRoleMapping(r.Context(), m)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "upsert role mapping: "+err.Error())
		return
	}
	action, status := "access.role_mapping.write", http.StatusCreated
	if saved.ID != m.ID {
		status = http.StatusOK
	}
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		action, saved.ID.String(), "success", mustJSON(map[string]any{
			"value": saved.Value, "role": saved.Role,
		})))
	writeJSON(w, status, saved)
}

// ─── DELETE /access/mappings/{id} ──────────────────────────────────────────

// handleDeleteRoleMapping removes one console row by id. The lockout guard
// runs against the candidate set with this row removed; the REVERSE
// posture-flip guard (A-5) fires whenever removing this row actually moves
// the unmatched-human outcome — derived from the real merged map, the same
// accessUnmatchedOutcome the add-side guard uses, not a hand-mirrored arm
// computation keyed on chart/row counts.
func (s *Server) handleDeleteRoleMapping(w http.ResponseWriter, r *http.Request) {
	if !s.requireOIDC(w) {
		return
	}
	id, ok := parseIDParam(w, r, "id", "role mapping")
	if !ok {
		return
	}
	existing, err := s.cfg.Store.ListRoleMappings(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list role mappings: "+err.Error())
		return
	}
	// The matched row, kept for the audit event below (A-6) — once deleted,
	// the store can no longer say which value/role this id ever named.
	var matched types.RoleMapping
	for _, m := range existing {
		if m.ID == id {
			matched = m
			break
		}
	}
	candidate := accessCandidateRows(existing, id.String(), "", "")

	// acknowledge_access_change is a query param (this route's own
	// natural-key delete has no body) — ParseBool over a bare == "true"
	// (A-10) so "1"/"TRUE"/"T" also work, err (including absent) => false.
	acknowledge, _ := strconv.ParseBool(r.URL.Query().Get("acknowledge_access_change"))
	if !acknowledge {
		before := s.accessUnmatchedOutcome(toOIDCRoleMappings(existing))
		after := s.accessUnmatchedOutcome(candidate)
		if before != after {
			writeAccessPostureFlip(w, before, after)
			return
		}
	}

	if lerr := s.accessLockoutErr(r, existing, candidate); lerr != nil {
		writeError(w, http.StatusBadRequest, lerr.Error())
		return
	}

	if err := s.cfg.Store.DeleteRoleMapping(r.Context(), id); err != nil {
		if notFoundIf(w, err, "role mapping") {
			return
		}
		writeError(w, http.StatusInternalServerError, "delete role mapping: "+err.Error())
		return
	}
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		"access.role_mapping.delete", id.String(), "success", mustJSON(map[string]any{
			"value": matched.Value, "role": matched.Role,
		})))
	w.WriteHeader(http.StatusNoContent)
}

// ─── POST /access/preview ──────────────────────────────────────────────────

type accessPreviewRequest struct {
	Roles      []string `json:"roles,omitempty"`
	Groups     []string `json:"groups,omitempty"`
	Email      string   `json:"email,omitempty"`
	UseSession bool     `json:"use_session,omitempty"`
}

type accessPreviewMatch struct {
	Value  string `json:"value"`
	Role   string `json:"role"`
	Source string `json:"source"`
}

type accessPreviewResponse struct {
	Role    string               `json:"role"`
	OK      bool                 `json:"ok"`
	Matched []accessPreviewMatch `json:"matched"`
	Error   string               `json:"error,omitempty"`
}

// handlePreviewRole runs the SAME derivation a real login would (PreviewRole,
// a real Config.RoleMappings read) against either explicit claims or the
// caller's own last-sign-in snapshot (use_session) — the console's "who would
// this row make an admin" preview, without waiting for that person to sign
// in. A store read failure is a PREVIEW OUTCOME (error field, 200), not a
// 500: "couldn't check" is exactly the thing this endpoint exists to let an
// admin see and retry, not an API failure.
func (s *Server) handlePreviewRole(w http.ResponseWriter, r *http.Request) {
	if !s.requireOIDC(w) {
		return
	}
	var req accessPreviewRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	roles, groups, email := req.Roles, req.Groups, req.Email
	if req.UseSession {
		if oidcHumanFromContext(r.Context()) == "" {
			writeError(w, http.StatusBadRequest, "no session claims to preview")
			return
		}
		roles, groups, email = nil, oidcGroupsFromContext(r.Context()), oidcEmailFromContext(r.Context())
	}

	role, matched, ok, err := s.cfg.OIDC.PreviewRole(r.Context(), roles, groups, email)
	if err != nil {
		writeJSON(w, http.StatusOK, accessPreviewResponse{Matched: []accessPreviewMatch{}, Error: "role_check_unavailable"})
		return
	}
	out := make([]accessPreviewMatch, len(matched))
	for i, m := range matched {
		out[i] = accessPreviewMatch{Value: m.Value, Role: m.Role, Source: string(m.Source)}
	}
	writeJSON(w, http.StatusOK, accessPreviewResponse{Role: role, OK: ok, Matched: out})
}
