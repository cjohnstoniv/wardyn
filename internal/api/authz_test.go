// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/recording"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// ─── item 9: the chi.Walk-enumerated authorization matrix ────────────────────
//
// The router's ACTUAL routes are discovered at runtime via chi.Walk — never
// hand-listed. routeMatrix below classifies each one; TestAuthzMatrix fails
// on any discovered route missing a classification (and on any classified
// route the router no longer registers, so the table cannot go stale either).
// Per the brief: "six routes were missed by hand-listing across two drafts —
// the walk-enumeration is the point; hand-list nothing."
//
// Coverage boundary: the SSH gateway (docs/SSH.md, internal/api/sshgateway.go)
// runs its OWN separate listener with its own authorization (sshAuth) —
// chi.Walk only ever sees wardynd's HTTP router, so it CANNOT discover or
// exercise SSH connections at all. A green TestAuthzMatrix says nothing
// about SSH authorization; sshgateway_test.go is that surface's own pin.

// routeClass is the authorization tier a route sits behind.
type routeClass string

const (
	// classAdmin: only the admin role (or the admin token / local mode
	// ceiling) reaches the handler; a member is refused with 403.
	classAdmin routeClass = "admin"
	// classMember: any authenticated caller (admin or member) reaches the
	// handler; unauthenticated is refused with 401. Some member-class routes
	// SCOPE their response to the caller's own data internally (GET /runs,
	// GET /approvals, GET /audit, GET /setup/status) rather than refusing —
	// that scoping is exercised by each feature's own dedicated tests
	// (runs_policy_test.go-adjacent, approvals, audit, setup), not re-proven
	// here; this matrix's job is the coarse admit/refuse boundary.
	classMember routeClass = "member"
	// classOwner: owner-or-admin. The route names a specific run or approval
	// in its path; an admin (any entity) or the entity's own creator reaches
	// the handler, a foreign member is refused with the BYTE-IDENTICAL 404 a
	// missing entity gets (no existence oracle), and unauthenticated is
	// refused with 401.
	classOwner routeClass = "owner"
	// classAnonymous: no credential required at all (health/UI/OIDC bootstrap).
	classAnonymous routeClass = "anonymous"
	// classInternal: gated by a run-scoped or ground-truth-scoped bearer
	// token (internalAuth / internalAuthGroundtruth) — NEITHER the admin
	// token NOR an SSO session satisfies it.
	classInternal routeClass = "internal"
)

// routeEntity names which seeded fixture a classOwner route's path id(s) are
// substituted with. Irrelevant for every other class.
type routeEntity string

const (
	entityRun      routeEntity = "run"
	entityApproval routeEntity = "approval"
)

type classifiedRoute struct {
	class  routeClass
	entity routeEntity
}

// routeMatrix is keyed exactly as chi.Walk reports a route: "METHOD /pattern".
// Built from the ground-truth dump of a maximally-configured server (every
// conditional route mounted: OIDC, Secrets, RecordingStore all wired) — see
// the method comment on TestAuthzMatrix for how to regenerate it if this ever
// needs re-verifying against the live router.
var routeMatrix = map[string]classifiedRoute{
	// ── anonymous ──
	"GET /":              {class: classAnonymous},
	"GET /healthz":       {class: classAnonymous},
	"GET /readyz":        {class: classAnonymous},
	"GET /auth/login":    {class: classAnonymous},
	"GET /auth/callback": {class: classAnonymous},
	"GET /auth/logout":   {class: classAnonymous},

	// ── admin ──
	"GET /metrics": {class: classAdmin},
	// The admin twins of /me/tokens: the deployment-wide inventory names other
	// humans, and revoke-any is the remediation path for a token whose owner was
	// demoted or has left (migration 0045's stamp ceiling).
	"GET /api/v1/tokens":                                        {class: classAdmin},
	"DELETE /api/v1/tokens/{id}":                                {class: classAdmin},
	"POST /api/v1/setup/harness-login":                          {class: classAdmin},
	"PUT /api/v1/setup/harness-credential/{provider}":           {class: classAdmin},
	"DELETE /api/v1/setup/harness-credential/{provider}":        {class: classAdmin},
	"POST /api/v1/policies":                                     {class: classAdmin},
	"PUT /api/v1/policies/{id}":                                 {class: classAdmin},
	"DELETE /api/v1/policies/{id}":                              {class: classAdmin},
	"POST /api/v1/sources":                                      {class: classAdmin},
	"POST /api/v1/sources/{id}/scan":                            {class: classAdmin},
	"DELETE /api/v1/sources/{id}":                               {class: classAdmin},
	"POST /api/v1/base-images":                                  {class: classAdmin},
	"DELETE /api/v1/base-images/{id}":                           {class: classAdmin},
	"POST /api/v1/workspaces":                                   {class: classAdmin},
	"PUT /api/v1/workspaces/{id}":                               {class: classAdmin},
	"DELETE /api/v1/workspaces/{id}":                            {class: classAdmin},
	"POST /api/v1/workspaces/{id}/scan":                         {class: classAdmin},
	"POST /api/v1/workspaces/{id}/build":                        {class: classAdmin},
	"PUT /api/v1/workspaces/{id}/approved-egress":               {class: classAdmin},
	"PUT /api/v1/workspaces/{id}/denied-egress":                 {class: classAdmin},
	"PUT /api/v1/workspaces/{id}/llm-cred":                      {class: classAdmin},
	"PUT /api/v1/workspaces/{id}/requirements":                  {class: classAdmin},
	"POST /api/v1/workspaces/{id}/record":                       {class: classAdmin},
	"POST /api/v1/workspaces/{id}/record/{task}/promote-egress": {class: classAdmin},
	"POST /api/v1/workspaces/{id}/env-as-code/write":            {class: classAdmin},
	"PUT /api/v1/secrets/{name}":                                {class: classAdmin},
	"DELETE /api/v1/secrets/{name}":                             {class: classAdmin},
	"PUT /api/v1/site-config":                                   {class: classAdmin},
	"POST /api/v1/site-config/test-proxy":                       {class: classAdmin},
	"POST /api/v1/site-config/test-redirect":                    {class: classAdmin},
	"PUT /api/v1/integrations/{id}":                             {class: classAdmin},
	"DELETE /api/v1/integrations/{id}":                          {class: classAdmin},
	"GET /api/v1/permissions":                                   {class: classAdmin},
	"POST /api/v1/permissions/grants":                           {class: classAdmin},
	"DELETE /api/v1/permissions/grants/{id}":                    {class: classAdmin},
	"PUT /api/v1/permissions/enforcement":                       {class: classAdmin},
	"POST /api/v1/sessions/revoke":                              {class: classAdmin},

	// ── member (any authenticated human/token; internally scoped where the
	// handler itself narrows the response — see the classMember doc) ──
	"GET /api/v1/approvals":    {class: classMember},
	"GET /api/v1/audit":        {class: classMember},
	"GET /api/v1/audit/export": {class: classMember},
	"GET /api/v1/base-images":  {class: classMember},
	"GET /api/v1/integrations": {class: classMember},
	"GET /api/v1/me":           {class: classMember},
	// /me/ssh-keys (SSH lane, C2): classMember, NOT classOwner — this is a
	// self-service registry scoped to the caller's OWN principal AT THE
	// STORE (sshkeys.go's package doc), same shape as GET/POST /secrets
	// above; a foreign fingerprint on the DELETE path is store-level
	// principal-scoped so it already answers store.ErrNotFound (404)
	// without needing an owner/foreign id pair here.
	"GET /api/v1/me/ssh-keys": {class: classMember},
	// /me/tokens is the same self-service shape as /me/ssh-keys above:
	// classMember, principal-scoped AT THE STORE, so DELETE /me/tokens/{id}
	// answers a foreign id with store.ErrNotFound (404) without needing an
	// owner/foreign pair here. The token a member mints carries their own
	// stamped role, so minting one crosses no tier — see apiTokenAuth.
	"GET /api/v1/me/tokens":         {class: classMember},
	"POST /api/v1/me/tokens":        {class: classMember},
	"DELETE /api/v1/me/tokens/{id}": {class: classMember},
	// /me/capabilities is the member-safe twin of GET /permissions above: it
	// answers only for the caller's OWN subjects (ListCapabilityGrantsFor), so
	// it sits on r like every other /me/* read, not operatorOnly.
	"GET /api/v1/me/capabilities": {class: classMember},
	// /me/run-layout is the same shape as /me/ssh-keys above: classMember, not
	// classOwner. It names no entity in its path — the STORE scopes it to the
	// caller's own principal, so there is no foreign row to 404 on.
	"GET /api/v1/me/run-layout":                   {class: classMember},
	"PUT /api/v1/me/run-layout":                   {class: classMember},
	"GET /api/v1/policies":                        {class: classMember},
	"GET /api/v1/policies/default":                {class: classMember},
	"GET /api/v1/policies/{id}":                   {class: classMember},
	"GET /api/v1/runs":                            {class: classMember},
	"GET /api/v1/secrets":                         {class: classMember},
	"GET /api/v1/setup/status":                    {class: classMember},
	"GET /api/v1/site-config":                     {class: classMember},
	"GET /api/v1/sources":                         {class: classMember},
	"GET /api/v1/sources/{id}":                    {class: classMember},
	"GET /api/v1/workspaces":                      {class: classMember},
	"GET /api/v1/workspaces/{id}":                 {class: classMember},
	"GET /api/v1/workspaces/{id}/build":           {class: classMember},
	"GET /api/v1/workspaces/{id}/env-as-code":     {class: classMember},
	"GET /api/v1/workspaces/{id}/observed-egress": {class: classMember},
	"POST /api/v1/auth/logout":                    {class: classMember},
	"POST /api/v1/me/ssh-keys":                    {class: classMember},
	"POST /api/v1/runs":                           {class: classMember},
	"POST /api/v1/runs/preflight":                 {class: classMember},
	"DELETE /api/v1/me/ssh-keys/{fingerprint}":    {class: classMember},

	// ── owner-or-admin ──
	"GET /api/v1/runs/{id}":                   {class: classOwner, entity: entityRun},
	"GET /api/v1/runs/{id}/grants":            {class: classOwner, entity: entityRun},
	"GET /api/v1/runs/{id}/recording/{runID}": {class: classOwner, entity: entityRun},
	"POST /api/v1/runs/{id}/attach-ticket":    {class: classOwner, entity: entityRun},
	"POST /api/v1/runs/{id}/kill":             {class: classOwner, entity: entityRun},
	"POST /api/v1/runs/{id}/profile":          {class: classOwner, entity: entityRun},
	// The run cockpit's live evidence reads. classOwner, same gate as GET
	// /runs/{id} above: each names a run in its path and each exposes something
	// about a LIVE sandbox — the workspace's diff, its resource usage, and who
	// is holding its PTY. A foreign member gets the byte-identical 404.
	"GET /api/v1/runs/{id}/files":         {class: classOwner, entity: entityRun},
	"GET /api/v1/runs/{id}/resources":     {class: classOwner, entity: entityRun},
	"GET /api/v1/runs/{id}/attach-holder": {class: classOwner, entity: entityRun},
	// Take-over ends another human's live terminal session. Still classOwner
	// (an owner may reclaim their own run's PTY, and the act is audited as
	// session.takeover) — NOT classMember, which would let anyone displace
	// anyone.
	"POST /api/v1/runs/{id}/attach/takeover": {class: classOwner, entity: entityRun},
	"POST /api/v1/approvals/{id}/approve":    {class: classOwner, entity: entityApproval},
	"POST /api/v1/approvals/{id}/deny":       {class: classOwner, entity: entityApproval},

	// GET /runs/{id}/attach (the interactive PTY WebSocket) is a SPECIAL case:
	// its ticket-LESS fallback lane (ticketOrHumanAuth) is plain admin-only
	// (requireOperator) — a member without a minted ticket cannot attach at
	// all, own run or not — so THIS matrix (which never presents a
	// ?ticket=) correctly classifies it admin. The ticket-BEARING lane's
	// owner-or-admin behavior (a member may attach their OWN run via a
	// ticket they minted) is a property of handleAttachWS's ticket-role
	// re-check, covered by TestAttachWS_TicketRoleAuthorization instead —
	// chi.Walk reports only ONE route here regardless, so both properties
	// need pinning, just not both from this table.
	"GET /api/v1/runs/{id}/attach": {class: classAdmin},

	// ── internal (run-token / ground-truth-token bearer only) ──
	"GET /api/v1/internal/approvals/{id}":       {class: classInternal},
	"GET /api/v1/internal/injection/{grantID}":  {class: classInternal},
	"POST /api/v1/internal/approvals":           {class: classInternal},
	"POST /api/v1/internal/credentials/mint":    {class: classInternal},
	"POST /api/v1/internal/decisions":           {class: classInternal},
	"POST /api/v1/internal/groundtruth":         {class: classInternal},
	"POST /api/v1/internal/token/renew":         {class: classInternal},
	"PUT /api/v1/internal/recordings/{runID}":   {class: classInternal},
	"PUT /api/v1/internal/scan-results/{runID}": {class: classInternal},
	"PUT /api/v1/internal/sso-token/{runID}":    {class: classInternal},
}

// routeParamRe matches a chi path parameter segment like "{id}" or "{runID}".
var routeParamRe = regexp.MustCompile(`\{[a-zA-Z0-9]+\}`)

// buildPath substitutes EVERY path parameter in pattern with id. Every route
// in routeMatrix needs at most one MEANINGFUL entity id (routes with two
// differently-named params, e.g. .../runs/{id}/recording/{runID}, need the
// SAME value in both — Handler's own outer/inner equality check depends on
// it), so a single blanket replace covers every case.
func buildPath(pattern, id string) string {
	return routeParamRe.ReplaceAllString(pattern, id)
}

// bodyFor returns the request body a generic (non-owner) matrix probe sends:
// none for a body-less method, an empty JSON object otherwise. The handler
// underneath is expected to fail on SHAPE (missing required fields) for a
// generic body — that failure is a 4xx/5xx the matrix tolerates (see
// assertNotBlocked); only the AUTHORIZATION status classes are pinned here.
func bodyFor(method string) string {
	if method == http.MethodGet || method == http.MethodDelete {
		return ""
	}
	return "{}"
}

// assertNotBlocked fails t if code is 401 or 403 — the two statuses that mean
// "authorization refused this caller", as opposed to any 2xx/4xx/5xx a
// handler's own downstream logic might reach once authorization let it
// through (which this matrix deliberately does not attempt to fully
// simulate — that is each feature's own test file's job).
func assertNotBlocked(t *testing.T, who string, w *httptest.ResponseRecorder) {
	t.Helper()
	if w.Code == http.StatusUnauthorized || w.Code == http.StatusForbidden {
		t.Errorf("%s: status = %d, want NOT 401/403; body=%s", who, w.Code, w.Body.String())
	}
}

// fakeAuthzSessionRevocations is a no-op oidc.SessionRevocations double that
// exists only to make POST /api/v1/sessions/revoke visible to chi.Walk: the
// route is mounted conditionally (see routes.go) on cfg.SessionRevocations
// != nil, same as Secrets/RecordingStore above — the matrix's "every
// conditional route mounted" doctrine requires it wired here too.
type fakeAuthzSessionRevocations struct{}

func (fakeAuthzSessionRevocations) IsSessionRevoked(context.Context, string, time.Time) (bool, error) {
	return false, nil
}
func (fakeAuthzSessionRevocations) RevokeSub(context.Context, string) error { return nil }
func (fakeAuthzSessionRevocations) RevokeAll(context.Context) error         { return nil }

func TestAuthzMatrix(t *testing.T) {
	ast := newAuthzStore()
	aap := newAuthzApprovals(ast)
	h := newHarness(t)
	cfg := baseTestConfig(h, ast)
	cfg.OIDC = &oidc.Authenticator{}
	cfg.Secrets = getErrStore{getErr: secretstore.ErrNotFound}
	cfg.Approvals = aap
	rs, err := recording.NewFSStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg.RecordingStore = rs
	cfg.SessionRevocations = fakeAuthzSessionRevocations{}
	srv := New(cfg)

	const memberSub = "sub-member"
	const otherSub = "sub-other-member"
	adminSess := ssoSession(t, "sub-admin", "admin@corp.example", oidc.RoleAdmin)
	memberSess := ssoSession(t, memberSub, "member@corp.example", oidc.RoleMember)

	// seedRun creates a RUNNING run owned by createdBy, with a matching cast
	// pre-saved in the recording store (so GET .../recording/{runID} can
	// distinguish "denied" from "no cast exists yet" for whichever id a
	// given sub-case uses).
	seedRun := func(createdBy string) uuid.UUID {
		id := uuid.New()
		ast.mu.Lock()
		ast.runs[id] = types.AgentRun{ID: id, CreatedBy: createdBy, State: types.RunRunning, Agent: "claude-code"}
		ast.mu.Unlock()
		_ = rs.SaveCast(context.Background(), id.String(), strings.NewReader(`{"version":2}`+"\n"))
		return id
	}
	// seedApproval creates a PENDING approval on a fresh run owned by createdBy.
	seedApproval := func(createdBy string) uuid.UUID {
		runID := seedRun(createdBy)
		return aap.seed(runID)
	}

	// ── discover every ACTUAL route via chi.Walk; classify or fail ──
	discovered := map[string]bool{}
	if err := chi.Walk(srv.router, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		discovered[method+" "+route] = true
		return nil
	}); err != nil {
		t.Fatalf("chi.Walk: %v", err)
	}
	for key := range discovered {
		if _, ok := routeMatrix[key]; !ok {
			t.Errorf("UNCLASSIFIED route %q — add it to routeMatrix (admin/member/owner/anonymous/internal)", key)
		}
	}
	for key := range routeMatrix {
		if !discovered[key] {
			t.Errorf("STALE routeMatrix entry %q — the router no longer registers this route; remove it", key)
		}
	}

	// ── execute the table ──
	for key, rc := range routeMatrix {
		key, rc := key, rc
		method, pattern, ok := strings.Cut(key, " ")
		if !ok {
			t.Fatalf("malformed routeMatrix key %q", key)
		}
		t.Run(key, func(t *testing.T) {
			body := bodyFor(method)
			switch rc.class {
			case classAnonymous:
				// Auth-independent by definition: the only meaningful signal is
				// that NO credential still works. (An authed caller getting the
				// same non-401 treatment follows trivially — these routes never
				// consult identity at all — so re-asserting it per credential
				// would be redundant, not additional signal.)
				p := buildPath(pattern, "x1")
				w := doSSO(t, srv, method, p, nil, body)
				if w.Code == http.StatusUnauthorized {
					t.Errorf("anonymous route 401'd with no credential: %d body=%s", w.Code, w.Body.String())
				}

			case classInternal:
				p := buildPath(pattern, uuid.New().String())
				if w := do(t, srv, method, p, "", body); w.Code != http.StatusUnauthorized {
					t.Errorf("no token: status = %d, want 401; body=%s", w.Code, w.Body.String())
				}
				if w := do(t, srv, method, p, adminToken, body); w.Code != http.StatusUnauthorized {
					t.Errorf("admin token (wrong audience): status = %d, want 401; body=%s", w.Code, w.Body.String())
				}
				// L2: an SSO session — even an admin's — is a different auth mode
				// entirely (internalAuth/internalAuthGroundtruth accept ONLY a
				// run-scoped or ground-truth-scoped bearer token), not merely "the
				// wrong audience" the admin-bearer case above already covers.
				if w := doSSO(t, srv, method, p, adminSess, body); w.Code != http.StatusUnauthorized {
					t.Errorf("admin SSO session (wrong auth mode entirely): status = %d, want 401; body=%s", w.Code, w.Body.String())
				}

			case classAdmin:
				p := buildPath(pattern, "x1")
				assertNotBlocked(t, "admin", doSSO(t, srv, method, p, adminSess, body))
				if w := doSSO(t, srv, method, p, memberSess, body); w.Code != http.StatusForbidden {
					t.Errorf("member: status = %d, want 403; body=%s", w.Code, w.Body.String())
				}
				if w := doSSO(t, srv, method, p, nil, body); w.Code != http.StatusUnauthorized {
					t.Errorf("unauthenticated: status = %d, want 401; body=%s", w.Code, w.Body.String())
				}

			case classMember:
				p := buildPath(pattern, "x1")
				assertNotBlocked(t, "admin", doSSO(t, srv, method, p, adminSess, body))
				assertNotBlocked(t, "member", doSSO(t, srv, method, p, memberSess, body))
				if w := doSSO(t, srv, method, p, nil, body); w.Code != http.StatusUnauthorized {
					t.Errorf("unauthenticated: status = %d, want 401; body=%s", w.Code, w.Body.String())
				}

			case classOwner:
				var ownedID, foreignID uuid.UUID
				switch rc.entity {
				case entityRun:
					ownedID, foreignID = seedRun(memberSub), seedRun(otherSub)
				case entityApproval:
					ownedID, foreignID = seedApproval(memberSub), seedApproval(otherSub)
				default:
					t.Fatalf("classOwner route %q has no entity set", key)
				}
				// L3: the non-owner probe below gets its OWN untouched foreign
				// approval — foreignID itself is DECIDED by the admin-bypass probe
				// right below (a state-mutating call against an approval's FSM),
				// and re-probing an already-decided approval risks a 409
				// (approval.ErrAlreadyDecided) coincidentally shadowing the 404
				// this assertion exists to pin, rather than that 404 being pinned
				// by test design. A run has no such decide-FSM (kill doesn't touch
				// CreatedBy), so it reuses foreignID directly.
				nonOwnerForeignID := foreignID
				if rc.entity == entityApproval {
					nonOwnerForeignID = seedApproval(otherSub)
				}
				pOwned := buildPath(pattern, ownedID.String())
				pForeign := buildPath(pattern, foreignID.String())
				pNonOwnerForeign := buildPath(pattern, nonOwnerForeignID.String())

				// Admin reaches even a FOREIGN entity — proves the bypass, not
				// merely "admin can read its own".
				if w := doSSO(t, srv, method, pForeign, adminSess, body); w.Code == http.StatusUnauthorized || w.Code == http.StatusForbidden || w.Code == http.StatusNotFound {
					t.Errorf("admin on a FOREIGN entity: status = %d, want none of 401/403/404; body=%s", w.Code, w.Body.String())
				}
				// The owner reaches their own.
				if w := doSSO(t, srv, method, pOwned, memberSess, body); w.Code == http.StatusUnauthorized || w.Code == http.StatusForbidden || w.Code == http.StatusNotFound {
					t.Errorf("owning member: status = %d, want none of 401/403/404; body=%s", w.Code, w.Body.String())
				}
				// A non-owner gets the byte-identical 404 a missing entity would
				// (no existence oracle) — never 403.
				if w := doSSO(t, srv, method, pNonOwnerForeign, memberSess, body); w.Code != http.StatusNotFound {
					t.Errorf("non-owning member: status = %d, want 404 (no existence oracle); body=%s", w.Code, w.Body.String())
				}
				if w := doSSO(t, srv, method, pOwned, nil, body); w.Code != http.StatusUnauthorized {
					t.Errorf("unauthenticated: status = %d, want 401; body=%s", w.Code, w.Body.String())
				}

			default:
				t.Fatalf("route %q has no recognized class %q", key, rc.class)
			}
		})
	}
}

// TestDecide_MemberKindRestriction is the HIGH-1 review fix's dedicated
// coverage: TestAuthzMatrix's classOwner case only ever seeds an
// egress_domain approval (aap.seed), so it proves ownership scoping but never
// exercises decide()'s per-Kind restriction. A member who owns the run may
// decide an egress_domain approval on it (unchanged from item 3) but NOT a
// credential or tool_call approval on that SAME owned run — those stay
// admin-only regardless of ownership (self-approving either would self-mint a
// real credential / reopen the clamped ceiling under the member's own
// authority). Both get the byte-identical "approval not found" 404 a foreign
// approval would (no existence/kind oracle), never 403.
func TestDecide_MemberKindRestriction(t *testing.T) {
	ast := newAuthzStore()
	aap := newAuthzApprovals(ast)
	h := newHarness(t)
	cfg := baseTestConfig(h, ast)
	cfg.OIDC = &oidc.Authenticator{}
	cfg.Approvals = aap
	srv := New(cfg)

	const memberSub = "sub-member-kind"
	member := ssoSession(t, memberSub, "member-kind@corp.example", oidc.RoleMember)

	runID := uuid.New()
	ast.mu.Lock()
	ast.runs[runID] = types.AgentRun{ID: runID, CreatedBy: memberSub, State: types.RunRunning}
	ast.mu.Unlock()

	seed := func(kind types.ApprovalKind) uuid.UUID {
		id := uuid.New()
		aap.mu.Lock()
		aap.byID[id] = types.ApprovalRequest{ID: id, RunID: runID, Kind: kind, State: types.ApprovalPending, RequestedAt: time.Now().UTC()}
		aap.mu.Unlock()
		return id
	}

	for _, kind := range []types.ApprovalKind{types.ApprovalCredential, types.ApprovalToolCall} {
		for _, path := range []string{"/approve", "/deny"} {
			id := seed(kind)
			w := doSSO(t, srv, http.MethodPost, "/api/v1/approvals/"+id.String()+path, member, "")
			if w.Code != http.StatusNotFound {
				t.Errorf("owning member deciding a %s approval via %s: status = %d, want 404 (admin-only regardless of ownership); body=%s",
					kind, path, w.Code, w.Body.String())
			}
		}
	}

	// Contrast case: the SAME owning member CAN decide an egress_domain
	// approval on the SAME run — the restriction is kind-specific, not a
	// blanket "members can never decide their own approvals".
	egressID := seed(types.ApprovalEgressDomain)
	if w := doSSO(t, srv, http.MethodPost, "/api/v1/approvals/"+egressID.String()+"/approve", member, ""); w.Code != http.StatusOK {
		t.Errorf("owning member deciding their own egress_domain approval: status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
}

// ─── in-memory store.Store fake ───────────────────────────────────────────
//
// Full coverage (compile-time asserted against store.Store) rather than an
// embedded-nil partial fake: this matrix issues real requests against
// admin/member-open routes too, and a nil-embedded Store would panic
// (Recoverer-caught 500) on the first untouched method — masking exactly the
// kind of "did auth even run" signal this test exists to catch. Every method
// beyond CreateRun/GetRun/ListRuns*/UpdateRunStateIf* is a minimal, honest
// stub (empty list / ErrNotFound / no-op) — the matrix's job is the
// authorization boundary, not full functional fidelity per route.
type authzStore struct {
	mu   sync.Mutex
	runs map[uuid.UUID]types.AgentRun
	// workspaces is real rather than a hard-wired ErrNotFound because `always`
	// — the one decision scope that writes durable config — is otherwise
	// unreachable: decide()'s rule 7 loads the workspace and the write-back
	// updates it, so a stub can only ever exercise always's REJECT paths and a
	// green build would never notice the durable half regressing. Seeded ids
	// only; an unseeded id still answers ErrNotFound, so every route the matrix
	// above walks reads exactly as it did before.
	workspaces map[uuid.UUID]types.Workspace
	tickets    map[string]store.AttachTicket
}

func newAuthzStore() *authzStore {
	return &authzStore{
		runs:       map[uuid.UUID]types.AgentRun{},
		workspaces: map[uuid.UUID]types.Workspace{},
		tickets:    map[string]store.AttachTicket{},
	}
}

var _ store.Store = (*authzStore)(nil)
var _ store.RunsByCreatorPager = (*authzStore)(nil)

func (s *authzStore) Ping(_ context.Context) error { return nil }

func (s *authzStore) CreateRun(_ context.Context, r types.AgentRun) (types.AgentRun, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.ID == uuid.Nil {
		r.ID = uuid.New()
	}
	s.runs[r.ID] = r
	return r, nil
}

func (s *authzStore) GetRun(_ context.Context, id uuid.UUID) (types.AgentRun, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.runs[id]
	if !ok {
		return types.AgentRun{}, store.ErrNotFound
	}
	return r, nil
}

func (s *authzStore) ListRuns(context.Context) ([]types.AgentRun, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]types.AgentRun, 0, len(s.runs))
	for _, r := range s.runs {
		out = append(out, r)
	}
	return out, nil
}

func (s *authzStore) ListRunsPageByCreator(_ context.Context, createdBy string, _ store.Page) ([]types.AgentRun, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []types.AgentRun{}
	for _, r := range s.runs {
		if r.CreatedBy == createdBy {
			out = append(out, r)
		}
	}
	return out, nil
}

func (s *authzStore) UpdateRunStateIf(_ context.Context, id uuid.UUID, from, to types.RunState) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.runs[id]
	if !ok || r.State != from {
		return false, nil
	}
	r.State = to
	s.runs[id] = r
	return true, nil
}

func (s *authzStore) UpdateRunStateIfIdle(ctx context.Context, id uuid.UUID, from, to types.RunState, _ time.Time) (bool, error) {
	return s.UpdateRunStateIf(ctx, id, from, to)
}

func (s *authzStore) mutateRun(id uuid.UUID, fn func(*types.AgentRun)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.runs[id]
	if !ok {
		return store.ErrNotFound
	}
	fn(&r)
	s.runs[id] = r
	return nil
}

func (s *authzStore) SetSandboxRef(_ context.Context, id uuid.UUID, ref string) error {
	return s.mutateRun(id, func(r *types.AgentRun) { r.SandboxRef = ref })
}
func (s *authzStore) SetRunImage(_ context.Context, id uuid.UUID, image string) error {
	return s.mutateRun(id, func(r *types.AgentRun) { r.Image = image })
}
func (s *authzStore) SetRunAgentExecID(_ context.Context, id uuid.UUID, execID string) error {
	return s.mutateRun(id, func(r *types.AgentRun) { r.AgentExecID = execID })
}
func (s *authzStore) SetRunFailureHint(_ context.Context, id uuid.UUID, hint string) error {
	return s.mutateRun(id, func(r *types.AgentRun) { r.FailureHint = hint })
}
func (s *authzStore) TouchRun(context.Context, uuid.UUID) error { return nil }

func (s *authzStore) CreatePolicy(_ context.Context, p types.RunPolicy) (types.RunPolicy, error) {
	if p.ID == uuid.Nil {
		p.ID = uuid.New()
	}
	return p, nil
}
func (s *authzStore) GetPolicy(context.Context, uuid.UUID) (types.RunPolicy, error) {
	return types.RunPolicy{}, store.ErrNotFound
}
func (s *authzStore) ListPolicies(context.Context) ([]types.RunPolicy, error) { return nil, nil }
func (s *authzStore) UpdatePolicy(context.Context, uuid.UUID, string, types.RunPolicySpec) (types.RunPolicy, error) {
	return types.RunPolicy{}, store.ErrNotFound
}
func (s *authzStore) DeletePolicy(context.Context, uuid.UUID) error { return nil }

func (s *authzStore) CreateWorkspace(_ context.Context, ws types.Workspace) (types.Workspace, error) {
	if ws.ID == uuid.Nil {
		ws.ID = uuid.New()
	}
	return ws, nil
}
func (s *authzStore) GetWorkspace(_ context.Context, id uuid.UUID) (types.Workspace, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ws, ok := s.workspaces[id]
	if !ok {
		return types.Workspace{}, store.ErrNotFound
	}
	return ws, nil
}
func (s *authzStore) ListWorkspaces(context.Context) ([]types.Workspace, error) { return nil, nil }
func (s *authzStore) UpdateWorkspace(context.Context, uuid.UUID, types.Workspace) (types.Workspace, error) {
	return types.Workspace{}, store.ErrNotFound
}
func (s *authzStore) SetWorkspaceApprovedEgress(_ context.Context, id uuid.UUID, domains []string) (types.Workspace, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ws, ok := s.workspaces[id]
	if !ok {
		return types.Workspace{}, store.ErrNotFound
	}
	ws.ApprovedEgress = domains
	s.workspaces[id] = ws
	return ws, nil
}

// AddWorkspaceEgressDecision mirrors the SEMANTICS of the PG statement backing
// it, not merely its signature. The cross-list removal is the half worth
// mirroring: deny beats allow everywhere the proxy evaluates policy, so a host
// left on both lists makes one direction a silent no-op — a fake that only
// appended would let exactly that regression through green. Dedupe and the
// "an already-listed host always passes the cap" rule are here for the same
// reason: an idempotent re-decide must not be reported as "cap reached".
func (s *authzStore) AddWorkspaceEgressDecision(_ context.Context, id uuid.UUID, host string, allow bool, maxApproved int) (types.Workspace, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ws, ok := s.workspaces[id]
	if !ok {
		return types.Workspace{}, store.ErrNotFound
	}
	add, remove := &ws.ApprovedEgress, &ws.DeniedEgress
	if !allow {
		add, remove = remove, add
	}
	if !slices.Contains(*add, host) {
		if len(*add) >= maxApproved {
			return types.Workspace{}, store.ErrConflict
		}
		*add = append(*add, host)
	}
	*remove = slices.DeleteFunc(*remove, func(h string) bool { return h == host })
	s.workspaces[id] = ws
	return ws, nil
}
func (s *authzStore) SetWorkspaceDeniedEgress(context.Context, uuid.UUID, []string) (types.Workspace, error) {
	return types.Workspace{}, store.ErrNotFound
}
func (s *authzStore) SetWorkspaceLLMCred(context.Context, uuid.UUID, *types.WorkspaceLLMCred) (types.Workspace, error) {
	return types.Workspace{}, store.ErrNotFound
}
func (s *authzStore) SetWorkspaceRequirements(context.Context, uuid.UUID, map[string]types.WorkspaceRequirement) (types.Workspace, error) {
	return types.Workspace{}, store.ErrNotFound
}
func (s *authzStore) SetWorkspaceRecordResult(context.Context, uuid.UUID, string, json.RawMessage, string) (types.Workspace, bool, error) {
	return types.Workspace{}, false, store.ErrNotFound
}
func (s *authzStore) ClaimWorkspaceActiveRun(context.Context, uuid.UUID, uuid.UUID, *uuid.UUID) (types.Workspace, bool, error) {
	return types.Workspace{}, false, store.ErrNotFound
}
func (s *authzStore) ClearWorkspaceActiveRun(context.Context, uuid.UUID, uuid.UUID) (bool, error) {
	return false, nil
}
func (s *authzStore) SetWorkspaceBuiltImage(context.Context, uuid.UUID, string, string) (types.Workspace, error) {
	return types.Workspace{}, store.ErrNotFound
}
func (s *authzStore) SetWorkspaceImportState(context.Context, uuid.UUID, types.WorkspaceStatus, *uuid.UUID, *uuid.UUID) (types.Workspace, bool, error) {
	return types.Workspace{}, false, store.ErrNotFound
}
func (s *authzStore) SetWorkspaceScanResult(context.Context, uuid.UUID, json.RawMessage, uuid.UUID) (types.Workspace, bool, error) {
	return types.Workspace{}, false, store.ErrNotFound
}
func (s *authzStore) MergeWorkspaceRequirements(context.Context, uuid.UUID, map[string]types.WorkspaceRequirement) (types.Workspace, error) {
	return types.Workspace{}, store.ErrNotFound
}
func (s *authzStore) DeleteWorkspace(context.Context, uuid.UUID) error { return nil }

func (s *authzStore) UpsertSource(_ context.Context, src types.Source) (types.Source, error) {
	if src.ID == uuid.Nil {
		src.ID = uuid.New()
	}
	return src, nil
}
func (s *authzStore) GetSource(context.Context, uuid.UUID) (types.Source, error) {
	return types.Source{}, store.ErrNotFound
}
func (s *authzStore) GetSourcesByIDs(context.Context, []uuid.UUID) (map[uuid.UUID]types.Source, error) {
	return nil, nil
}
func (s *authzStore) GetBaseImagesByIDs(context.Context, []uuid.UUID) (map[uuid.UUID]types.BaseImageEntry, error) {
	return nil, nil
}
func (s *authzStore) ListSources(context.Context) ([]types.Source, error) { return nil, nil }
func (s *authzStore) UpdateSourceConfig(context.Context, uuid.UUID, string, map[string]types.WorkspaceRequirement) (types.Source, error) {
	return types.Source{}, store.ErrNotFound
}
func (s *authzStore) WorkspacesAttaching(context.Context, uuid.UUID) ([]string, error) {
	return nil, nil
}
func (s *authzStore) DeleteSource(context.Context, uuid.UUID, bool) error              { return nil }
func (s *authzStore) ClaimSourceActiveRun(context.Context, uuid.UUID, uuid.UUID) error { return nil }
func (s *authzStore) ClearSourceActiveRun(context.Context, uuid.UUID, uuid.UUID) error { return nil }
func (s *authzStore) SetSourceScanResult(context.Context, uuid.UUID, []byte, types.WorkspaceStatus, uuid.UUID, map[string]types.WorkspaceRequirement) (types.Source, error) {
	return types.Source{}, store.ErrNotFound
}
func (s *authzStore) SetSourceScanResultUnfenced(context.Context, uuid.UUID, []byte, types.WorkspaceStatus, map[string]types.WorkspaceRequirement) (types.Source, error) {
	return types.Source{}, store.ErrNotFound
}

func (s *authzStore) UpsertBaseImage(_ context.Context, b types.BaseImageEntry) (types.BaseImageEntry, error) {
	if b.ID == uuid.Nil {
		b.ID = uuid.New()
	}
	return b, nil
}
func (s *authzStore) UpdateBaseImageName(_ context.Context, id uuid.UUID, name string) (types.BaseImageEntry, error) {
	return types.BaseImageEntry{ID: id, Name: name}, nil
}
func (s *authzStore) GetBaseImage(context.Context, uuid.UUID) (types.BaseImageEntry, error) {
	return types.BaseImageEntry{}, store.ErrNotFound
}
func (s *authzStore) ListBaseImages(context.Context) ([]types.BaseImageEntry, error) { return nil, nil }
func (s *authzStore) WorkspacesUsingBaseImage(context.Context, uuid.UUID) ([]string, error) {
	return nil, nil
}
func (s *authzStore) DeleteBaseImage(context.Context, uuid.UUID, bool) error { return nil }

func (s *authzStore) CreateGrant(_ context.Context, g types.CredentialGrant) (types.CredentialGrant, error) {
	if g.ID == uuid.Nil {
		g.ID = uuid.New()
	}
	return g, nil
}
func (s *authzStore) ListGrantsByRun(context.Context, uuid.UUID) ([]types.CredentialGrant, error) {
	return nil, nil
}

// Approval CRUD on Store itself is never exercised by the API layer directly
// (it always goes through Config.Approvals — authzApprovals below); these
// exist only to satisfy store.Store.
func (s *authzStore) CreateApproval(_ context.Context, a types.ApprovalRequest) (types.ApprovalRequest, error) {
	return a, nil
}
func (s *authzStore) GetApproval(context.Context, uuid.UUID) (types.ApprovalRequest, error) {
	return types.ApprovalRequest{}, store.ErrNotFound
}
func (s *authzStore) ListApprovals(context.Context, types.ApprovalState) ([]types.ApprovalRequest, error) {
	return nil, nil
}
func (s *authzStore) DecideApproval(context.Context, uuid.UUID, types.ApprovalDecision) (types.ApprovalRequest, error) {
	return types.ApprovalRequest{}, store.ErrNotFound
}

func (s *authzStore) QueryAuditEvents(context.Context, uuid.UUID, int) ([]types.AuditEvent, error) {
	return nil, nil
}
func (s *authzStore) QueryRecentAuditEvents(context.Context, int) ([]types.AuditEvent, error) {
	return nil, nil
}
func (s *authzStore) LatestAuditEventByAction(context.Context, string) (types.AuditEvent, error) {
	return types.AuditEvent{}, store.ErrNotFound
}

func (s *authzStore) GetSiteConfig(context.Context) (types.SiteConfig, error) {
	return types.SiteConfig{}, nil
}
func (s *authzStore) PutSiteConfig(_ context.Context, cfg types.SiteConfig) (types.SiteConfig, error) {
	return cfg, nil
}

func (s *authzStore) PutRef(context.Context, string, string) error { return nil }
func (s *authzStore) GetRef(context.Context, string) (string, bool, error) {
	return "", false, nil
}
func (s *authzStore) DeleteRef(context.Context, string) error { return nil }

func (s *authzStore) MintAttachTicket(_ context.Context, token string, t store.AttachTicket, _, _ time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tickets[token] = t
	return nil
}
func (s *authzStore) ConsumeAttachTicket(_ context.Context, token string, _ time.Time) (store.AttachTicket, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tickets[token]
	if ok {
		delete(s.tickets, token)
	}
	return t, ok, nil
}

// SSH gateway key registry (rebase compile trap, per the reviewer's own
// note): store.Store gained these four methods on the SSH lane
// (0033_ssh_public_keys.sql). The matrix's GET/POST /me/ssh-keys and DELETE
// /me/ssh-keys/{fingerprint} routes are classMember — item 2's coarse
// admit/refuse boundary, not full functional fidelity (see routeClass's own
// doc comment) — so honest stubs are enough; never exercised beyond "does the
// handler reach the store at all".
func (s *authzStore) AddSSHKey(_ context.Context, k types.SSHPublicKey) (types.SSHPublicKey, error) {
	return k, nil
}
func (s *authzStore) ListSSHKeysByPrincipal(context.Context, string) ([]types.SSHPublicKey, error) {
	return nil, nil
}
func (s *authzStore) GetSSHKeyByFingerprint(context.Context, string) (types.SSHPublicKey, error) {
	return types.SSHPublicKey{}, store.ErrNotFound
}
func (s *authzStore) DeleteSSHKey(context.Context, string, string) error { return nil }

// ─── per-user api tokens (migration 0045) ─────────────────────────────────
//
// Honest empty state, same rationale as the SSH stubs above: this matrix pins
// the coarse admit/refuse boundary of the five token routes, not the feature.
// GetAPITokenByRaw returning ErrNotFound is what makes every bearer in this file
// take the pre-existing admin path — the matrix presents session cookies and the
// admin token, never a `wdn_` bearer, so the token auth branch must be inert
// here. apitokens_test.go is that branch's own pin.
func (s *authzStore) CreateAPIToken(_ context.Context, t types.APIToken, _ string) (types.APIToken, error) {
	return t, nil
}
func (s *authzStore) GetAPITokenByRaw(context.Context, string) (types.APIToken, error) {
	return types.APIToken{}, store.ErrNotFound
}
func (s *authzStore) TouchAPIToken(context.Context, uuid.UUID, time.Time) error { return nil }
func (s *authzStore) ListAPITokensByPrincipal(context.Context, string) ([]types.APIToken, error) {
	return nil, nil
}
func (s *authzStore) ListAPITokens(context.Context) ([]types.APIToken, error) { return nil, nil }
func (s *authzStore) RevokeAPIToken(context.Context, uuid.UUID, string, time.Time) (types.APIToken, error) {
	return types.APIToken{}, store.ErrNotFound
}

// ─── capability grants (migration 0042) ───────────────────────────────────
//
// authzStore is the one NON-embedding store.Store double in the tree (see this
// type's doc comment on why it implements every method rather than embedding),
// so widening Store lands here as six compile errors until they are stubbed.
//
// The stubs are honest EMPTY state, not permissive shortcuts: no grants and no
// enforcement rows is exactly a freshly-upgraded 0.5 deployment, so every route
// this matrix walks resolves precisely as it did before 0042 existed. That is
// the state the back-compat proof wants under the authorization matrix; the
// resolver's own allow/deny/precedence matrix lives in capabilities_test.go
// with a store double that can actually hold rows.
func (s *authzStore) UpsertCapabilityGrant(_ context.Context, g types.CapabilityGrant) (types.CapabilityGrant, error) {
	return g, nil
}
func (s *authzStore) DeleteCapabilityGrant(context.Context, uuid.UUID) error {
	return store.ErrNotFound
}
func (s *authzStore) ListCapabilityGrants(context.Context) ([]types.CapabilityGrant, error) {
	return nil, nil
}
func (s *authzStore) ListCapabilityGrantsFor(context.Context, []string, []string) ([]types.CapabilityGrant, error) {
	return nil, nil
}
func (s *authzStore) GetCapabilityEnforcement(context.Context) (map[string]bool, error) {
	return map[string]bool{}, nil
}
func (s *authzStore) PutCapabilityEnforcement(_ context.Context, enabled map[string]bool) (map[string]bool, error) {
	return enabled, nil
}

// ─── in-memory ApprovalService fake, ownership-aware ──────────────────────

// authzApprovals is a minimal approval FSM backed by an in-memory map, PLUS
// store.ApprovalsByRunCreatorPager (item 2) — it consults ast (the SAME
// authzStore backing the server's Config.Store) to resolve an approval's
// run's owner, exactly as wardynd's production approvalService would need to
// once wired with a matching delegation method (see pagination.go's
// production-wiring note).
type authzApprovals struct {
	mu    sync.Mutex
	store *authzStore
	byID  map[uuid.UUID]types.ApprovalRequest
}

func newAuthzApprovals(st *authzStore) *authzApprovals {
	return &authzApprovals{store: st, byID: map[uuid.UUID]types.ApprovalRequest{}}
}

var _ ApprovalService = (*authzApprovals)(nil)
var _ store.ApprovalsByRunCreatorPager = (*authzApprovals)(nil)

func (a *authzApprovals) seed(runID uuid.UUID) uuid.UUID {
	a.mu.Lock()
	defer a.mu.Unlock()
	id := uuid.New()
	a.byID[id] = types.ApprovalRequest{
		ID: id, RunID: runID, Kind: types.ApprovalEgressDomain,
		RequestedScope: json.RawMessage(`{"host":"example.com"}`),
		State:          types.ApprovalPending, RequestedAt: time.Now().UTC(),
	}
	return id
}

func (a *authzApprovals) Request(_ context.Context, req types.ApprovalRequest) (types.ApprovalRequest, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if req.ID == uuid.Nil {
		req.ID = uuid.New()
	}
	req.State = types.ApprovalPending
	a.byID[req.ID] = req
	return req, nil
}

func (a *authzApprovals) Decide(_ context.Context, id uuid.UUID, _ types.ActorType, decision types.ApprovalDecision) (types.ApprovalRequest, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	ap, ok := a.byID[id]
	if !ok {
		return types.ApprovalRequest{}, store.ErrNotFound
	}
	if ap.State != types.ApprovalPending {
		return types.ApprovalRequest{}, store.ErrAlreadyDecided
	}
	ap.State = decision.State
	ap.DecidedBy, ap.Reason = decision.DecidedBy, decision.Reason
	ap.DecisionScope, ap.DecisionExpiresAt = decision.Scope, decision.ExpiresAt
	a.byID[id] = ap
	return ap, nil
}

func (a *authzApprovals) Get(_ context.Context, id uuid.UUID) (types.ApprovalRequest, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	ap, ok := a.byID[id]
	if !ok {
		return types.ApprovalRequest{}, store.ErrNotFound
	}
	return ap, nil
}

func (a *authzApprovals) List(_ context.Context, state types.ApprovalState) ([]types.ApprovalRequest, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := []types.ApprovalRequest{}
	for _, ap := range a.byID {
		if state == "" || ap.State == state {
			out = append(out, ap)
		}
	}
	return out, nil
}

// ListApprovalsPageByRunCreator: item 2's optional scoped-list interface.
func (a *authzApprovals) ListApprovalsPageByRunCreator(ctx context.Context, createdBy string, stateFilter types.ApprovalState, _ store.Page) ([]types.ApprovalRequest, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := []types.ApprovalRequest{}
	for _, ap := range a.byID {
		if stateFilter != "" && ap.State != stateFilter {
			continue
		}
		run, err := a.store.GetRun(ctx, ap.RunID)
		if err != nil || run.CreatedBy != createdBy {
			continue
		}
		out = append(out, ap)
	}
	return out, nil
}
