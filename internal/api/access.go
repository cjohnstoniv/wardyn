// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Access / role-mapping CRUD (migration 0051): the console's
// Getting Started -> People editor over the store half of
// internal/auth/oidc's RoleMappingSource. Modeled on permissions.go's shape —
// one read that shows the whole picture, small validated writes, an audit
// event per write — but with two guards permissions.go has no analogue for:
// this table decides who derives admin at all, so a bad write here can either
// silently widen access (a collision with the operator allowlist) or lock the
// acting admin out of their own deployment (see the posture-flip and lockout
// guards below).
package api

import (
	"cmp"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// accessDeniedRole is the JSON spelling of "no role at all" — deriveRole's
// ok=false outcome. Never a real oidc role constant (RoleAdmin/RoleUser),
// deliberately, so a client can tell "this caller derives no role" apart from
// any string the role map could actually produce.
const accessDeniedRole = "denied"

// accessEmailKeyRefused is EMAIL_KEY_REFUSED — the frozen string the
// adjudication (docs/design/people-access-prompt.md) creates, rendered
// verbatim on a POST /access/mappings refused for carrying an email-shaped
// value with Config.AllowEmailMappings unset. Byte-for-byte per the
// adjudication; do not reword without updating that doc too.
const accessEmailKeyRefused = "Email mappings are disabled on this install. Map an App Role or group instead, or opt in with WARDYN_OIDC_ALLOW_EMAIL_MAPPINGS in your chart."

// accessStaleSnapshot is the lockout guard's OTHER refusal: a caller
// whose login-time claim snapshot (oidcGroupsFromContext) cannot even
// reproduce the admin role they demonstrably hold right now — because their
// admin-granting group fell off the 2048-byte groups-snapshot truncation
// (sessionGroups), or because their session cookie predates snapshots
// entirely (Groups nil) — gets THIS message, never the lockout one: the
// snapshot is too stale to answer "would this write remove your admin
// access" at all, so refusing on ITS say-so would be a false lockout, not a
// caught one.
const accessStaleSnapshot = "your sign-in is too old to verify this change — sign in again before changing role mappings"

// accessStaleSnapshotToken is the SAME refusal for the API-TOKEN lane, and it
// exists because oidcGroupsFromContext resolves to the token's STAMPED
// snapshot for a wdn_-token caller, never a claim freshly re-derived from the
// IdP on this request — so a demonstrably-admin token can still fail
// roleBefore against a snapshot that has not caught up yet.
//
// This guard fires on ANY caller whose stamped claim snapshot cannot reproduce
// the admin role they hold — and apiTokenAuth installs exactly such a snapshot:
// api_tokens.groups is stamped on MINT and on every OnLogin
// (store.RefreshAPITokenIdentity) and read verbatim on every request, and a
// NULL groups_truncated (a pre-0.7 token) reads as truncated. So a
// wdn_-token admin is refused every POST/DELETE /access/mappings until the
// stamp catches up: the token owner's own NEXT sign-in re-stamps role AND
// the group snapshot together on every unrevoked token they hold, this one
// included, so retrying the write after that sign-in succeeds. The residual
// is a token whose owner never signs in again — for them, re-minting from the
// console (Account → API tokens) is still the only way to force a fresh stamp.
//
// The guard already distinguishes lanes once (the admin-token/local-mode
// exemption above), so this is the same distinction applied to the sentence
// rather than to the decision: the refusal is unchanged, only the remedy is the
// caller's own.
const accessStaleSnapshotToken = "your API token's sign-in snapshot is too old to verify this change — the token owner's next sign-in re-stamps it; sign in again, then retry, or re-mint the token from the console (Account → API tokens) if you cannot sign in again"

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

// GET /access

type accessMappingView struct {
	ID    string `json:"id,omitempty"`
	Value string `json:"value"`
	// Role is the tier; UserType the row's type, set only when Role is the
	// user tier (a chart value naming a type id arrives here split the same
	// way sign-in splits it).
	Role        string    `json:"role"`
	UserType    string    `json:"user_type,omitempty"`
	Source      string    `json:"source"` // "chart" | "console"
	Shadowed    bool      `json:"shadowed"`
	ShadowCause string    `json:"shadow_cause"` // "" | "chart" | "operator_allowlist"
	CreatedAt   time.Time `json:"created_at,omitzero"`
	CreatedBy   string    `json:"created_by,omitempty"`
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
	// OperatorEmails is the ADDRESSES themselves — api.Config.OperatorEmails,
	// the same list fed into oidc.Config.LegacyAdminEmails at boot — so the
	// console's Defaults block can render the addresses, not just the bool
	// above (which stays for the guard-note logic that only needs presence).
	OperatorEmails []string `json:"operator_emails"`
	// AllowEmailMappings mirrors Config.AllowEmailMappings —
	// the console derives email-ness of a row from "@" in its value itself
	// (no new per-row field), and uses this alongside that to render the
	// §7.2 warn badge / the opt-in state, matching what a write would accept.
	AllowEmailMappings bool `json:"allow_email_mappings"`
	// EmailDomainsConfigured reports whether WARDYN_OIDC_EMAIL_DOMAINS
	// is set (oidc.Authenticator.HasEmailDomains) — the EMAIL_KEY badge copy
	// depends on this, and the response otherwise cannot express it.
	EmailDomainsConfigured bool `json:"email_domains_configured"`
	// Provider is a human-facing IdP name derived SERVER-SIDE from the OIDC
	// issuer URL (e.g. "Microsoft Entra ID") so the console's SSO chip names
	// WHERE sign-in comes from, not just THAT it is SSO; it falls back to the
	// issuer's host when the issuer isn't a recognized provider. The raw
	// issuer URL is deliberately NOT on the wire — the console never rendered
	// it, and deriving the name here keeps one implementation of that mapping.
	Provider string        `json:"provider"`
	Posture  accessPosture `json:"posture"`
	// UserTypes is every user type, so the page can name the type on each
	// row, in the default role and in a preview without a second call.
	UserTypes []types.UserType `json:"user_types"`
}

// ssoProviderName derives a human-facing IdP name from an OIDC issuer URL,
// falling back to the issuer's host so an unrecognized provider still names
// itself rather than showing a bare "SSO".
func ssoProviderName(issuer string) string {
	u, err := url.Parse(issuer)
	if err != nil || u.Host == "" {
		return ""
	}
	host := strings.ToLower(u.Host)
	switch {
	case strings.Contains(host, "login.microsoftonline.com") || strings.Contains(host, "sts.windows.net"):
		return "Microsoft Entra ID"
	case strings.Contains(host, "accounts.google.com"):
		return "Google"
	case strings.Contains(host, "okta.com") || strings.Contains(host, "oktapreview.com"):
		return "Okta"
	case strings.Contains(host, "auth0.com"):
		return "Auth0"
	default:
		return u.Host
	}
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
//
// Both are mapping targets (accessTarget): arm 1's user is on the built-in
// type, so a default role naming a custom type is a change too — unmatched
// people would move from Standard user to that type. after is validated
// against userTypes (DefaultRoleOutcome) so it never claims a target a real
// sign-in would refuse with user_type_unknown — the same check
// accessUnmatchedOutcome already applies for the write-side guards.
func accessRolePosture(a *oidc.Authenticator, userTypes []types.UserType) (before, after string, changes bool) {
	before = oidc.RoleAdmin
	if a.HasOperatorEmails() {
		before = oidc.RoleUser
	}
	after = accessDeniedRole
	if role, userType, ok := a.DefaultRoleOutcome(userTypes); ok {
		after = accessTarget(role, userType)
	}
	return before, after, before != after
}

// accessTarget spells a tier and type the way a role-map value does: the
// tier, except a user on a custom type, which is that type's id. "user" stays
// the built-in type's spelling, so a deployment with no custom types reads
// exactly as before.
func accessTarget(role, userType string) string {
	if role == oidc.RoleUser && userType != "" && userType != types.UserTypeStandard {
		return userType
	}
	return role
}

// accessMappingsView merges chart rows (ChartRoleMap) and console rows into
// GET /access's one table, computing shadowed/shadow_cause with the SAME two
// rules mergeRoleMaps applies at login (chart collision, then the operator
// allowlist) — mergeRoleMaps itself is unexported, so this replicates its
// rule ORDER using the two accessors oidc exports for exactly this purpose
// (ChartRoleMap, IsOperatorEmail), through the one accessCollisionCause the
// write path refuses on: read and write must never disagree about which
// source shadows a value. A chart row is never shadowed — the chart always
// wins a collision, by construction. Sorted by value then source so the
// response is deterministic across calls with the same underlying state.
func accessMappingsView(chart map[string]string, rows []types.RoleMapping, a *oidc.Authenticator) []accessMappingView {
	out := make([]accessMappingView, 0, len(chart)+len(rows))
	for value, target := range chart {
		mv := accessMappingView{Value: value, Role: target, Source: "chart"}
		if role, userType, ok := oidc.SplitMappingTarget(target); ok {
			mv.Role, mv.UserType = role, userType
		}
		out = append(out, mv)
	}
	for _, m := range rows {
		mv := accessMappingView{
			ID: m.ID.String(), Value: m.Value, Role: m.Role, UserType: m.UserType, Source: "console",
			CreatedBy: m.CreatedBy, CreatedAt: m.CreatedAt,
		}
		if m.Role == oidc.RoleUser && m.UserType == "" {
			mv.UserType = types.UserTypeStandard
		}
		if cause := accessCollisionCause(m.Value, chart, a); cause != "" {
			mv.Shadowed, mv.ShadowCause = true, cause
		}
		out = append(out, mv)
	}
	slices.SortFunc(out, func(a, b accessMappingView) int {
		return cmp.Or(strings.Compare(a.Value, b.Value), strings.Compare(a.Source, b.Source))
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
		writeServerError(w, r, "list role mappings", err)
		return
	}
	userTypes, err := s.cfg.Store.ListUserTypes(r.Context())
	if err != nil {
		writeServerError(w, r, "list user types", err)
		return
	}
	if userTypes == nil {
		userTypes = []types.UserType{}
	}
	chart := s.cfg.OIDC.ChartRoleMap()
	before, after, changes := accessRolePosture(s.cfg.OIDC, userTypes)
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
		Provider:               ssoProviderName(s.cfg.OIDC.Issuer()),
		Posture: accessPosture{
			// MapEmpty is the REAL merged-map emptiness (chart + rows,
			// with a shadowed row contributing nothing) — not a raw row
			// count, which diverges whenever a stored row collides with the
			// chart or the operator allowlist.
			MapEmpty: s.cfg.OIDC.MergedMapEmpty(toOIDCRoleMappings(rows)),
			Before:   before, After: after, Changes: changes,
		},
		UserTypes: userTypes,
	})
}

// toOIDCRoleMappings converts store rows to the oidc package's own
// RoleMapping shape — the input every derivation accessor
// (PreviewRoleAgainst, MergedMapEmpty) takes, so a handler with a fresh
// []types.RoleMapping read never has to hand-roll the per-field copy.
func toOIDCRoleMappings(rows []types.RoleMapping) []oidc.RoleMapping {
	out := make([]oidc.RoleMapping, len(rows))
	for i, m := range rows {
		out[i] = oidc.RoleMapping{Value: m.Value, Role: m.Role, UserType: m.UserType}
	}
	return out
}

// accessUnmatchedOutcome is the ONE derivation both write-guards and the GET
// posture display use to ask "what role does an UNMATCHED signed-in human
// get against rows": PreviewRoleAgainst with no roles/groups/email,
// mapped to accessDeniedRole on a refusal and spelled as a mapping target
// (accessTarget) otherwise. Routing every caller through
// mergeRoleMaps+deriveRole this way — rather than a hand-mirrored arm
// computation keyed on raw row counts — means it can never diverge from what
// a real login would decide, including when a stored row is shadowed.
func (s *Server) accessUnmatchedOutcome(rows []oidc.RoleMapping, userTypes []types.UserType) string {
	d := s.cfg.OIDC.PreviewRoleAgainst(rows, userTypes, nil, nil, "")
	if !d.OK() {
		return accessDeniedRole
	}
	return accessTarget(d.Role, d.UserType)
}

// shared: canonicalization, candidate-map construction, guards

// canonicalRoleMapValue trims+lowers value and validates it against EXACTLY
// the contract mergeRoleMaps enforces on a console row (RoleMapping's own doc
// comment): non-empty, ASCII (matching is ASCII-only — a non-ASCII value can
// never match a claim, see oidc.ASCIIOnly), and free of control characters
// (the same hygiene validateCapabilityGrant applies to its own Value field —
// an empty/control-char value stored raw would be either a dead key or,
// worse for whitespace, one that matches ANY empty/whitespace claim).
//
// The ASCII guard runs before the fold, mirroring the chart-side twin
// oidc.ParseRoleMap (its ASCIIOnly refusal precedes its own ToLower) and
// oidc's deriveRole lookup loop. strings.ToLower folds KELVIN SIGN U+212A to
// 'k' and U+0130 to 'i', so guarding the LOWERED value accepts a value the
// operator did not type and stores a DIFFERENT, ASCII one under it — the
// console write surface silently disagreeing with the boot-time parser about
// what "ASCII" means, and binding a role to a group nobody named.
func canonicalRoleMapValue(value string) (string, error) {
	raw := strings.TrimSpace(value)
	if raw == "" {
		return "", fmt.Errorf("value: required")
	}
	if !oidc.ASCIIOnly(raw) {
		return "", fmt.Errorf("value: must be ASCII — matching is ASCII-only, a non-ASCII value can never match a claim")
	}
	v := strings.ToLower(raw)
	if len(v) > maxCapabilityGrantFieldLen || !controlCharFree(v) {
		return "", fmt.Errorf("value: invalid")
	}
	return v, nil
}

// accessCollisionBody is POST /access/mappings' collision 400: a
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

// accessCandidateRows builds the role-mapping set as it would be after a
// proposed write — replace/add value's row (a zero write means: this is a
// DELETE, drop the row instead), leaving every other existing row untouched —
// so the lockout guard can hand it to PreviewRoleAgainst and ask "would the
// acting admin still derive admin AFTER this write" without a second store
// round trip and without reimplementing the upsert-or-delete semantics twice.
func accessCandidateRows(existing []types.RoleMapping, id string, write oidc.RoleMapping) []oidc.RoleMapping {
	out := make([]oidc.RoleMapping, 0, len(existing)+1)
	replaced := false
	for _, m := range existing {
		if (id != "" && m.ID.String() == id) || (id == "" && m.Value == write.Value) {
			if write.Role != "" {
				out = append(out, write)
				replaced = true
			}
			continue // delete: drop this row from the candidate set
		}
		out = append(out, oidc.RoleMapping{Value: m.Value, Role: m.Role, UserType: m.UserType})
	}
	if write.Role != "" && !replaced {
		out = append(out, write)
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
// The snapshot itself can be too stale to trust — an admin whose
// admin-granting group fell off the 2048-byte groups-snapshot truncation
// (sessionGroups), or whose cookie predates snapshots entirely (Groups nil),
// would derive NON-admin against the snapshot regardless of what the write
// does, which would trip the lockout message on every write as a false
// positive if left unchecked. Checking roleBefore — the SAME snapshot run against
// the EXISTING rows, before this write — first avoids that: only when roleBefore is
// genuinely admin does a roleAfter that comes out non-admin mean the WRITE
// caused the demotion (the real lockout); when roleBefore already isn't
// admin, the snapshot cannot reproduce the admin access the caller
// demonstrably holds (they got past requireOperator to reach this handler at
// all), so it gets the distinct accessStaleSnapshot refusal instead — never
// silently allowed, since a snapshot too stale to verify a NO-OP write is
// too stale to verify a real demotion either.
//
// A user type decides nothing here: a tie or a missing type never refuses an
// admin-tier sign-in (deriveRole puts it on the built-in type), so it trips
// neither side of this guard.
func (s *Server) accessLockoutErr(r *http.Request, existing []types.RoleMapping, candidate []oidc.RoleMapping, userTypes []types.UserType) error {
	sub := oidcHumanFromContext(r.Context())
	if sub == "" {
		return nil
	}
	groups, email := oidcGroupsFromContext(r.Context()), oidcEmailFromContext(r.Context())
	before := s.cfg.OIDC.PreviewRoleAgainst(toOIDCRoleMappings(existing), userTypes, nil, groups, email)
	if !before.OK() || before.Role != oidc.RoleAdmin {
		// Same refusal, the caller's own REMEDY. Both lanes now clear this the
		// same way — the owner's own next sign-in re-stamps role AND the group
		// snapshot together (store.RefreshAPITokenIdentity, fired from
		// OnLogin) — but the token lane's caller cannot sign in FROM this
		// request: it is the token's owner, not this API call, who has to go
		// sign in, after which retrying the write succeeds. See
		// accessStaleSnapshotToken. The residual for either lane is the same:
		// an owner who never signs in again keeps whatever snapshot they had.
		if apiTokenIDFromContext(r.Context()) != uuid.Nil {
			return errors.New(accessStaleSnapshotToken)
		}
		return errors.New(accessStaleSnapshot)
	}
	after := s.cfg.OIDC.PreviewRoleAgainst(candidate, userTypes, nil, groups, email)
	if !after.OK() || after.Role != oidc.RoleAdmin {
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

// POST /access/mappings

type roleMappingWriteRequest struct {
	Value string `json:"value"`
	Role  string `json:"role"`
	// UserType names the row's type when Role is the user tier; empty means
	// the built-in type. Refused on the two admin tiers.
	UserType                string `json:"user_type,omitempty"`
	AcknowledgeAccessChange bool   `json:"acknowledge_access_change,omitempty"`
}

// accessMappingTarget is validMappingTarget at the console's write boundary:
// the row a POST may write, or the 400 sentence. A user row always names its
// type (the built-in one when none is given), and the type must exist in
// userTypes — a row naming no type would refuse every sign-in it decides
// (user_type_unknown), so it is refused here, before it is saved. A type id is
// never a tier: a reserved word is refused even though no user_types row can
// carry one.
func accessMappingTarget(req roleMappingWriteRequest, value string, userTypes []types.UserType) (oidc.RoleMapping, string) {
	if !oidc.ValidRole(req.Role) {
		return oidc.RoleMapping{}, fmt.Sprintf("The role %q isn't valid (want %q, %q or %q).", req.Role, oidc.RoleAdmin, oidc.RoleSecurityAdmin, oidc.RoleUser)
	}
	if req.Role != oidc.RoleUser {
		if req.UserType != "" {
			return oidc.RoleMapping{}, "Only the user role takes a user type."
		}
		return oidc.RoleMapping{Value: value, Role: req.Role}, ""
	}
	userType := cmp.Or(req.UserType, types.UserTypeStandard)
	if oidc.UserTypeIDReserved(userType) || !oidc.UserTypeIDWellFormed(userType) {
		return oidc.RoleMapping{}, fmt.Sprintf("The user type %q isn't a valid id.", userType)
	}
	// The built-in type always exists (seeded, never deletable), as it does
	// for sign-in.
	if userType != types.UserTypeStandard && !slices.ContainsFunc(userTypes, func(t types.UserType) bool { return t.ID == userType }) {
		return oidc.RoleMapping{}, accessUnknownUserType(userType)
	}
	return oidc.RoleMapping{Value: value, Role: oidc.RoleUser, UserType: userType}, ""
}

func accessUnknownUserType(id string) string {
	return fmt.Sprintf("The user type %q doesn't exist. Create it under User types first.", id)
}

// handleUpsertRoleMapping creates or re-adds one console row, keyed on the
// natural UNIQUE (value) — a conflict flips the existing row's role in place
// (store.UpsertRoleMapping), same 201-new/200-updated status split
// handleUpsertCapabilityGrant uses. Four gates run, in order, before the
// store is ever touched: shape/canonicalization, the chart/operator
// collision, the email-mapping opt-in, and the posture-flip guard —
// which fires whenever this write actually moves the unmatched-human
// outcome (accessUnmatchedOutcome over existing vs. candidate rows),
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
	userTypes, err := s.cfg.Store.ListUserTypes(r.Context())
	if err != nil {
		writeServerError(w, r, "list user types", err)
		return
	}
	write, msg := accessMappingTarget(req, value, userTypes)
	if msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	chart := s.cfg.OIDC.ChartRoleMap()
	if cause := accessCollisionCause(value, chart, s.cfg.OIDC); cause != "" {
		writeAccessCollision(w, value, cause)
		return
	}
	// Adjudication (docs/design/people-access-prompt.md): an email-shaped
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
		writeServerError(w, r, "list role mappings", err)
		return
	}
	candidate := accessCandidateRows(existing, "", write)

	// Posture-flip guard: fires iff this write actually moves the
	// unmatched-human outcome — derived from the REAL merged map via
	// accessUnmatchedOutcome (existing rows vs. candidate rows), not a raw
	// row-count precondition, which would miss a flip whenever a stored
	// row was shadowed (see accessUnmatchedOutcome's doc).
	if !req.AcknowledgeAccessChange {
		before := s.accessUnmatchedOutcome(toOIDCRoleMappings(existing), userTypes)
		after := s.accessUnmatchedOutcome(candidate, userTypes)
		if before != after {
			writeAccessPostureFlip(w, before, after)
			return
		}
	}

	if lerr := s.accessLockoutErr(r, existing, candidate, userTypes); lerr != nil {
		writeError(w, http.StatusBadRequest, lerr.Error())
		return
	}

	// A fresh candidate id: UpsertRoleMapping returns the EXISTING row's id on
	// a natural-key (value) conflict, never this one — comparing the two is
	// how the handler tells created from updated, the same trick
	// handleUpsertCapabilityGrant uses (that table's natural key includes a
	// caller-supplied id too, but the comparison works identically here).
	m := types.RoleMapping{ID: uuid.New(), Value: value, Role: write.Role, UserType: write.UserType, CreatedBy: principalFromRequest(r)}
	saved, err := s.cfg.Store.UpsertRoleMapping(r.Context(), m)
	if errors.Is(err, store.ErrNotFound) {
		// The type was removed between the list above and this write: the
		// foreign key refused the row.
		writeError(w, http.StatusBadRequest, accessUnknownUserType(write.UserType))
		return
	}
	if err != nil {
		writeServerError(w, r, "upsert role mapping", err)
		return
	}
	action, status := "access.role_mapping.write", http.StatusCreated
	if saved.ID != m.ID {
		status = http.StatusOK
	}
	// A demotion made here is effective here. A role mapping decides the role a
	// LOGIN derives; an outstanding wdn_ token carries a role stamped at MINT
	// and read verbatim on every request until its owner's OWN next login
	// re-stamps it (store.RefreshAPITokenRoles) — a real bound, but on their
	// schedule rather than the operator's, and one that never arrives for
	// someone who has left. So if this write takes a tier away from the value,
	// the affected principals' tokens are revoked now rather than announced —
	// scoped to snapshots that actually lose a tier, so a member's CI
	// credential naming the same group keeps working. The count is taken FIRST
	// and is the informational half — how many live frozen snapshots named this
	// value before the edit acted — so the audit row carries both numbers:
	// what was outstanding, and what this write actually revoked.
	stale := s.noteStaleRoleSnapshots(r.Context(), saved.Value, "upsert")
	revoked := s.revokeDemotedRoleSnapshots(r, saved.Value, toOIDCRoleMappings(existing), candidate, userTypes)
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		action, saved.ID.String(), "success", mustJSON(map[string]any{
			"value": saved.Value, "role": saved.Role, "user_type": saved.UserType,
			"stale_token_snapshots": stale, "tokens_revoked": revoked,
		})))
	// Embedded, so the response is a strict SUPERSET of the RoleMapping every
	// existing client already decodes — the console, pkg/client and the CLI keep
	// working byte for byte, and a client that wants the signal reads one more
	// key. omitempty: a deployment with no outstanding tokens sees no new field
	// at all.
	writeJSON(w, status, struct {
		types.RoleMapping
		StaleTokenSnapshots int `json:"stale_token_snapshots,omitempty"`
		TokensRevoked       int `json:"tokens_revoked,omitempty"`
	}{RoleMapping: saved, StaleTokenSnapshots: stale, TokensRevoked: revoked})
}

// DELETE /access/mappings/{id}

// handleDeleteRoleMapping removes one console row by id. The lockout guard
// runs against the candidate set with this row removed; the REVERSE
// posture-flip guard fires whenever removing this row actually moves
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
		writeServerError(w, r, "list role mappings", err)
		return
	}
	userTypes, err := s.cfg.Store.ListUserTypes(r.Context())
	if err != nil {
		writeServerError(w, r, "list user types", err)
		return
	}
	// The matched row, kept for the audit event below — once deleted,
	// the store can no longer say which value/role this id ever named.
	var matched types.RoleMapping
	for _, m := range existing {
		if m.ID == id {
			matched = m
			break
		}
	}
	candidate := accessCandidateRows(existing, id.String(), oidc.RoleMapping{})

	// acknowledge_access_change is a query param (this route's own
	// natural-key delete has no body) — ParseBool over a bare == "true"
	// so "1"/"TRUE"/"T" also work, err (including absent) => false.
	acknowledge, _ := strconv.ParseBool(r.URL.Query().Get("acknowledge_access_change"))
	if !acknowledge {
		before := s.accessUnmatchedOutcome(toOIDCRoleMappings(existing), userTypes)
		after := s.accessUnmatchedOutcome(candidate, userTypes)
		if before != after {
			writeAccessPostureFlip(w, before, after)
			return
		}
	}

	if lerr := s.accessLockoutErr(r, existing, candidate, userTypes); lerr != nil {
		writeError(w, http.StatusBadRequest, lerr.Error())
		return
	}

	if err := s.cfg.Store.DeleteRoleMapping(r.Context(), id); err != nil {
		if notFoundIf(w, err, "role mapping") {
			return
		}
		writeServerError(w, r, "delete role mapping", err)
		return
	}
	// The delete side of the same act, and the sharper one: removing a mapping
	// is how an admin takes a role AWAY. Same revoke, same scoping — and the
	// candidate set is the real post-delete row set, so a value the CHART still
	// grants (or a second console row still names) is not a demotion and
	// revokes nothing. The response is 204 with no body, so both numbers ride
	// the audit row and the WARN line rather than the wire — a body here would
	// change this route's status shape for every existing client.
	staleDeleted := s.noteStaleRoleSnapshots(r.Context(), matched.Value, "delete")
	revokedDeleted := s.revokeDemotedRoleSnapshots(r, matched.Value, toOIDCRoleMappings(existing), candidate, userTypes)
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		"access.role_mapping.delete", id.String(), "success", mustJSON(map[string]any{
			"value": matched.Value, "role": matched.Role, "user_type": matched.UserType,
			"stale_token_snapshots": staleDeleted, "tokens_revoked": revokedDeleted,
		})))
	w.WriteHeader(http.StatusNoContent)
}

// POST /access/preview

type accessPreviewRequest struct {
	Roles      []string `json:"roles,omitempty"`
	Groups     []string `json:"groups,omitempty"`
	Email      string   `json:"email,omitempty"`
	UseSession bool     `json:"use_session,omitempty"`
}

type accessPreviewResponse struct {
	Role string `json:"role"`
	// UserType is the type the sign-in would carry ("" when refused).
	UserType string       `json:"user_type,omitempty"`
	OK       bool         `json:"ok"`
	Matched  []oidc.Match `json:"matched"`
	// Denial is why a refused sign-in is refused: no_role,
	// user_type_ambiguous (Tied names the custom types at the top
	// priority) or user_type_unknown (Unknown names the missing ids). An
	// admin sign-in is never refused over a type: it carries Tied or Unknown
	// with no Denial and lands on the built-in type.
	Denial  string   `json:"denial,omitempty"`
	Tied    []string `json:"tied,omitempty"`
	Unknown []string `json:"unknown,omitempty"`
	Error   string   `json:"error,omitempty"`
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

	d, err := s.cfg.OIDC.PreviewRole(r.Context(), roles, groups, email)
	if err != nil {
		writeJSON(w, http.StatusOK, accessPreviewResponse{Matched: []oidc.Match{}, Error: "role_check_unavailable"})
		return
	}
	// A nil slice marshals to JSON null, but the field is typed
	// AccessPreviewMatch[] on the wire and the console reads its .length — so
	// a no-match preview must still send [], never null.
	matched := d.Matches
	if matched == nil {
		matched = []oidc.Match{}
	}
	writeJSON(w, http.StatusOK, accessPreviewResponse{
		Role: d.Role, UserType: d.UserType, OK: d.OK(), Matched: matched,
		Denial: d.Denial, Tied: d.Tied, Unknown: d.Unknown,
	})
}
