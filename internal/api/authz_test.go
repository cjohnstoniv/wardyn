// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/approval"
	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/recording"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// item 9: the chi.Walk-enumerated authorization matrix
//
// The router's ACTUAL routes are discovered at runtime via chi.Walk — never
// hand-listed. routeMatrix below classifies each one; TestAuthzMatrix fails
// on any discovered route missing a classification (and on any classified
// route the router no longer registers, so the table cannot go stale either).
// Per the brief: "six routes were missed by hand-listing across two drafts —
// the walk-enumeration is the point; hand-list nothing."
//
// Coverage boundary ONE: the SSH gateway (docs/SSH.md, internal/api/sshgateway.go)
// runs its OWN separate listener with its own authorization (sshAuth) —
// chi.Walk only ever sees wardynd's HTTP router, so it CANNOT discover or
// exercise SSH connections at all. A green TestAuthzMatrix says nothing
// about SSH authorization; sshgateway_test.go is that surface's own pin.
//
// Coverage boundary TWO: the UI-sandbox gateway (docs/UI-SANDBOXES.md,
// internal/api/uigateway.go). UIGatewayHandler is deliberately NOT mounted on
// this router — its routes must exist only on the second origin, and
// Server.Handler() 404s them (pinned by TestUIGateway_ConsoleOriginHasNoRelayRoutes).
// The consequence is the same as SSH's: chi.Walk cannot see it, so a green
// TestAuthzMatrix says NOTHING about relay authorization. That surface's own
// pins are the ticket/session tests in uigateway_test.go —
// EnterRejectsBadTickets, EnterRejectsNonOwnerTicket, EnterRequiresDeclaredApp,
// EnterRequiresRunningRun, RelayRequiresAValidSessionForThisRun.
//
// Both boundaries are named here because the failure mode is a READER's: a
// green matrix reads as "every route is classified", and without this it would
// be read that way for two surfaces it never touches.

// routeClass is the authorization tier a route sits behind.
type routeClass string

const (
	// classAdmin: only the SUPER admin role (or the admin token / local mode
	// ceiling) reaches the handler; a member AND a security_admin are both
	// refused with 403. Since 0.7 that second refusal is half the class's
	// meaning — see classSecurity.
	classAdmin routeClass = "admin"
	// classSecurity: the 0.7 SECOND admin tier (§B) — admin OR security_admin
	// reaches the handler (routes.go's securityOps group,
	// requireSecurityOperator), a member is refused with the BYTE-IDENTICAL
	// 403 classAdmin writes. The two admin classes are NOT a ladder: every
	// classAdmin route refuses a security_admin, so the split is only real if
	// BOTH directions are probed, which TestSecurityAdminRouteTier does over
	// this same table.
	//
	// 22 routes carry it: §B's 14 SEC of the 40 gated routes (sessions revoke ·
	// workspace approved-/denied-egress + record + promote-egress ·
	// site-config's two probes · permissions x4 · the two admin token twins ·
	// audit chain verify) plus /governance's 7 and §I's directory search, all
	// new in 0.7 and outside that count. Everything else gated stays classAdmin
	// — /policies writes and /access included, deliberately.
	classSecurity routeClass = "security"
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
	// classDevice: gated by deviceAuth alone — a `wdd_` device bearer whose
	// device IS the path's {id}. It authenticates a daemon, never a person, so
	// every human credential — the admin's included — is refused with 401, and
	// a live device token on another device's id gets 404, never 403.
	classDevice routeClass = "device"
)

// routeEntity names which seeded fixture a classOwner route's path id(s) are
// substituted with. Irrelevant for every other class.
type routeEntity string

const (
	entityRun      routeEntity = "run"
	entityApproval routeEntity = "approval"
	// entityWorkspace is the 0048 ownership noun: a MEMBER-OWNED workspace
	// (owned_by = the member's principal). An operator-owned workspace is a
	// different case entirely — still admin-only to mutate — and is pinned by
	// workspace_owner_test.go rather than by this matrix.
	entityWorkspace routeEntity = "workspace"
)

// ownerTier names WHICH ADMIN TIER an owner-scoped route's admin bypass belongs
// to. Required whenever class == classOwner, the same way entity already is.
//
// Why the table needed a third dimension (F155). routeMatrix is what routes.go
// calls authoritative, and classAdmin/classSecurity carry the tier so that a
// route wired to the wrong predicate reddens TestSecurityAdminRouteTier.
// classOwner carried none, so all 16 owner-scoped routes were SKIPPED by that
// test — and the 0.7 invariant they most need is exactly the one it enforces:
// the tiers DO NOT NEST, so "may an admin bypass ownership here" has two
// different answers depending on whether the bypass is inspect-or-stop
// (ownsRunOrAdmin, isSecurityOperator) or something the super admin reserves (a
// live PTY, a recording replay, a workspace write). Executed on the unfixed
// tree: a NEW owner-scoped route wired to the WIDE predicate and classified
// {classOwner, entityRun} passed both matrix tests while answering 200 to a
// security_admin on a foreign run.
type ownerTier string

const (
	// tierSuper: the admin bypass on this route is the SUPER admin's alone. A
	// security_admin reading a FOREIGN entity gets the byte-identical 404 a
	// non-owner gets — no existence oracle, and no ladder.
	tierSuper ownerTier = "super"
	// tierSecurity: the bypass extends to the security tier, deliberately —
	// inspect-or-stop is that tier's warrant (helpers.go's ownsRunOrAdmin).
	tierSecurity ownerTier = "security"
)

type classifiedRoute struct {
	class  routeClass
	entity routeEntity
	// ownerTier is required for classOwner and meaningless elsewhere; the
	// classOwner arm of TestAuthzMatrix fatals on a route that omits it, the
	// same way it already does for a missing entity.
	ownerTier ownerTier
	// body overrides bodyFor's generic "{}" for routes whose AUTHORIZATION —
	// not merely their shape validation — depends on what is in the request.
	// The generic body is normally the right probe precisely because a handler
	// is expected to fail on shape AFTER authorization ran; a route whose
	// predicate reads a request field is the exception, and probing it with
	// "{}" asserts nothing about the tier it is classified as.
	body string
}

// routeMatrix is keyed exactly as chi.Walk reports a route: "METHOD /pattern".
// Built from the ground-truth dump of a maximally-configured server (every
// conditional route mounted: OIDC, Secrets, RecordingStore all wired) — see
// the method comment on TestAuthzMatrix for how to regenerate it if this ever
// needs re-verifying against the live router.
var routeMatrix = map[string]classifiedRoute{
	// anonymous
	"GET /": {class: classAnonymous},
	// The console SPA, registered INSTEAD of "GET /" when a UIDir is set —
	// which the shipped image does (ENV WARDYN_UI_DIR=/srv/ui). Anonymous by
	// necessity: the console shell has to load before anyone can sign in. It is
	// mounted on the TOP-LEVEL router, outside every auth group and after the
	// whole /api/v1 tree, so it can shadow nothing; mountUI's own withinDir
	// check is what keeps it from serving outside the directory.
	"GET /*":             {class: classAnonymous},
	"GET /healthz":       {class: classAnonymous},
	"GET /readyz":        {class: classAnonymous},
	"GET /auth/login":    {class: classAnonymous},
	"GET /auth/callback": {class: classAnonymous},
	// Hybrid enrolment: a laptop's first boot holds no credential yet, only the
	// single-use enrolment token in its BODY, so the route is anonymous by
	// necessity and rate-limited per TCP peer instead (handleDeviceEnrol).
	"POST /api/v1/devices/enrol": {class: classAnonymous},

	// admin (SUPER only: a security_admin is refused here too)
	"GET /metrics":                                       {class: classAdmin},
	"POST /api/v1/setup/onboarding-complete":             {class: classAdmin},
	"PUT /api/v1/setup/harness-credential/{provider}":    {class: classAdmin},
	"DELETE /api/v1/setup/harness-credential/{provider}": {class: classAdmin},
	// The stored-policy WRITES stay SUPER even though /governance's profile
	// authoring is classSecurity (§B, decided): a stored run_policy is
	// selectable CONTENT, so a SEC write path here would re-open the credential
	// mint through a side door — author a policy pairing an operator secret
	// with attacker egress, then simply select it. The policy READS are
	// classMember below (redacted for anyone outside the security tier).
	"POST /api/v1/policies":        {class: classAdmin},
	"PUT /api/v1/policies/{id}":    {class: classAdmin},
	"DELETE /api/v1/policies/{id}": {class: classAdmin},
	// Library sources + base images: supply-chain admission, SUPER.
	"POST /api/v1/sources":            {class: classAdmin},
	"POST /api/v1/sources/{id}/scan":  {class: classAdmin},
	"DELETE /api/v1/sources/{id}":     {class: classAdmin},
	"POST /api/v1/base-images":        {class: classAdmin},
	"DELETE /api/v1/base-images/{id}": {class: classAdmin},
	// The workspace routes that BIND CREDENTIAL MATERIAL or WRITE THE HOST.
	// Their egress-decision siblings (approved-/denied-egress, promote-egress)
	// are classSecurity below — same handler file, same scopedWorkspaceWrite
	// helper, different tier, decided at the router.
	"PUT /api/v1/workspaces/{id}/llm-cred":     {class: classAdmin},
	"PUT /api/v1/workspaces/{id}/requirements": {class: classAdmin},
	// THE OPERATOR-TOPOLOGY READS. Their WRITES were already classAdmin and the
	// reads were classMember — a split made by VERB rather than by what the
	// document carries. GET /site-config returns the same whole document the
	// PUT above is classAdmin for "integration credential refs included": the
	// upstream-proxy secret ref, every integrations[].secrets[].secret_name,
	// and the internal proxy / SCM / artifact hostnames. A Source row carries
	// Locator (the host filesystem path of a local_dir source) and requirement
	// keys spelled `secret:<name>` / `egress:<host>`; a BaseImageEntry carries
	// the internal registry ref and the bootstrap URLs in Steps. No secret
	// VALUES — those are write-only — so this is a target list rather than a
	// key, handed to the whole member tier by routes the console called on page
	// load. Nothing member-facing consumes them (the console has no client
	// method for /sources or /base-images at all, and both /site-config callers
	// already tolerate a null), which is why the fix is one router line each
	// rather than three response projections.
	"GET /api/v1/site-config":  {class: classAdmin},
	"GET /api/v1/sources":      {class: classAdmin},
	"GET /api/v1/sources/{id}": {class: classAdmin},
	"GET /api/v1/base-images":  {class: classAdmin},
	// RECORD, by that same criterion. It reads like an egress-decision sibling
	// (it is how the hosts promote-egress promotes get observed) and was
	// classified with them, but it LAUNCHES an interactive sandbox rather than
	// writing a list: open egress by default, the workspace's local_dir
	// bind-mounted read-write, the clone credential minted, the workspace's
	// required secrets folded into proxy injections, the operator's LLM
	// credential attached — and the run stamped CreatedBy = the caller, which
	// walked straight through handleAttachTicket's strict foreign-run guard and
	// yielded a PTY in a sandbox holding another member's files. Pinned by
	// TestRecordWorkspaceIsSuperAdminOnly (security_admin_test.go).
	"POST /api/v1/workspaces/{id}/record": {class: classAdmin},
	// Offboarding (O6): admin-only, and gated by requireOperator rather than in
	// the handler precisely so the member refusal is a CONSTANT 403 that never
	// varies with whether the named workspace exists.
	"POST /api/v1/workspaces/{id}/reassign":          {class: classAdmin},
	"POST /api/v1/workspaces/{id}/env-as-code/write": {class: classAdmin},
	// The PUT replaces the WHOLE site-config document, integration credential
	// refs included — so it stays SUPER while its two non-mutating probes go
	// classSecurity. A SEC-writable per-field subset needs per-field authz
	// (phase 2), not a second gate on the same full-document write.
	"PUT /api/v1/site-config": {class: classAdmin},
	// Workspace providers (0.7.2) — the git-provider policy and storage
	// ceilings, stored as a sub-object of the same site-config singleton. BOTH
	// verbs are SUPER for the sibling GET's reason: a provider's base URLs name
	// the org's forge hosts and org paths, which is corporate topology, and the
	// member tier is served the provider KIND in a refusal instead.
	"GET /api/v1/workspace-providers": {class: classAdmin},
	"PUT /api/v1/workspace-providers": {class: classAdmin},
	// The agent roster (0.7.2) — which agents this deployment offers, the lane
	// each reaches its model on, and (under per_user) the org's AWS access portal
	// URL. SUPER for the sibling block's reason: it names the org's model-provider
	// choices and its IdP. The member tier is served a DIFFERENT, narrower
	// document — SetupStatus.harnesses' enabled/mechanism/credential_source —
	// which carries no start URL.
	"GET /api/v1/agent-providers": {class: classAdmin},
	"PUT /api/v1/agent-providers": {class: classAdmin},
	// Model providers (0.8): gateway addresses, the AWS access portal and
	// account pins. SUPER for the agent roster's reason; the member tier is
	// served SetupStatus.model_providers instead, which carries none of them.
	"GET /api/v1/model-providers":      {class: classAdmin},
	"PUT /api/v1/model-providers":      {class: classAdmin},
	"PUT /api/v1/integrations/{id}":    {class: classAdmin},
	"DELETE /api/v1/integrations/{id}": {class: classAdmin},
	// Access / role mappings (migration 0051, Phase 2 lane A): the console's
	// Getting Started -> People editor over the store half of
	// internal/auth/oidc's RoleMappingSource. SUPER-only, including BOTH reads,
	// and the contrast with /permissions (classSecurity below) is the point: a
	// security admin who could write role mappings would map themselves to
	// admin, and the GET leaks the operator-email target list. This bounds who
	// derives admin AT ALL, a bigger blast radius than any capability.
	"GET /api/v1/access":                  {class: classAdmin},
	"POST /api/v1/access/mappings":        {class: classAdmin},
	"DELETE /api/v1/access/mappings/{id}": {class: classAdmin},
	"POST /api/v1/access/preview":         {class: classAdmin},
	// Operator-triggered sandbox sweep. SUPER for HOST reach and blast radius —
	// it drives the runner (Status + StopSandbox) plus the credential revoke
	// cascade across every run in the deployment from one call, and the host is
	// one of the three axes securityOps never gets.
	//
	// It is not justified by reach into runs: the sweep skips every non-terminal
	// run (it reaps the sandbox of runs that have already ended), and the
	// security tier can stop a foreign run, on purpose — ownsRunOrAdmin is
	// isSecurityOperator, so kill admits it on any run. See
	// TestSecurityAdminCanStopAForeignRun below and routes.go's own note.
	"POST /api/v1/admin/sandboxes/sweep":  {class: classAdmin},
	"GET /api/v1/admin/runs/proxy-window": {class: classAdmin},
	"POST /api/v1/admin/runs/restart":     {class: classAdmin},
	// Minting a device enrolment token creates a credential, so it is SUPER;
	// the inventory and the revoke are the inventory-then-revoke pair /tokens
	// already puts on the security tier (classSecurity below).
	"POST /api/v1/admin/devices/enrolment-tokens": {class: classAdmin},

	// security (0.7 §B: admin or security_admin; a member still 403s)
	// The admin twins of /me/tokens: the deployment-wide inventory names other
	// humans, and revoke-any is the remediation path for a token whose owner was
	// demoted or has left (migration 0045's stamp ceiling). Incident response,
	// and neither hands the caller reach — the inventory returns metadata, the
	// DELETE only subtracts.
	"GET /api/v1/tokens":         {class: classSecurity},
	"DELETE /api/v1/tokens/{id}": {class: classSecurity},
	// The device twins of the two token routes above: see an enrolled laptop,
	// cut it off. Neither hands the caller reach — the inventory carries no
	// credential material and the revoke only subtracts.
	"GET /api/v1/admin/devices":         {class: classSecurity},
	"DELETE /api/v1/admin/devices/{id}": {class: classSecurity},
	// The same pair for enrolment tokens not yet redeemed: the list carries
	// neither the token nor its hash, and the revoke only subtracts.
	"GET /api/v1/admin/devices/enrolment-tokens":         {class: classSecurity},
	"DELETE /api/v1/admin/devices/enrolment-tokens/{id}": {class: classSecurity},
	// Offboarding (CS-5): erase every credential one person holds. It only
	// subtracts, and names no value back — the same shape as the revokes above.
	"DELETE /api/v1/people/{principal}/credentials": {class: classSecurity},
	// The workspace EGRESS-DECISION lane. Deciding which hosts a workspace's
	// runs may reach is the same authority as deciding an egress approval, and
	// promote-egress is literally its bulk form.
	// record itself is classAdmin above: it LAUNCHES the sandbox whose
	// observations promote-egress promotes, and launching is not deciding.
	"PUT /api/v1/workspaces/{id}/approved-egress":               {class: classSecurity},
	"PUT /api/v1/workspaces/{id}/denied-egress":                 {class: classSecurity},
	"POST /api/v1/workspaces/{id}/record/{task}/promote-egress": {class: classSecurity},
	// Non-mutating: they launch a throwaway probe sandbox and answer "does the
	// baseline this deployment already declares actually work" — evidence, not
	// configuration. The PUT they probe stays classAdmin above.
	"POST /api/v1/site-config/test-proxy":    {class: classSecurity},
	"POST /api/v1/site-config/test-redirect": {class: classSecurity},
	// The org allow/denylist primitive. Delegable ONLY because of the
	// no-capability-reaches-admin invariant (capAllowed/capGranted
	// short-circuit on isOperator alone) — pinned by
	// TestCapabilityGrantsNeverReachTheAdminTier, without which handing this
	// tier the grant table would be a self-promotion primitive.
	"GET /api/v1/permissions":                {class: classSecurity},
	"POST /api/v1/permissions/grants":        {class: classSecurity},
	"DELETE /api/v1/permissions/grants/{id}": {class: classSecurity},
	"PUT /api/v1/permissions/enforcement":    {class: classSecurity},
	// "Available to" (#612): the restricted bit is a grant fact, on the same
	// tier as the grant rows that list who gets the value.
	"GET /api/v1/permissions/availability/{kind}/*": {class: classSecurity},
	"PUT /api/v1/permissions/availability/{kind}/*": {class: classSecurity},
	// Cutting a compromised human's live sessions: the time-critical half of
	// incident response, and a revocation only ever SUBTRACTS reach.
	"POST /api/v1/sessions/revoke": {class: classSecurity},
	// The tamper-evidence verdict over the audit hash chain — the evidence this
	// tier's whole job rests on (§F promises them "verify the audit chain" in
	// words). Gated at all, unlike the two paginated /audit reads below, because
	// the verdict counts every row in the deployment: whole-fleet audit VOLUME
	// is the same disclosure that keeps /metrics admin-gated.
	"GET /api/v1/audit/chain/verify": {class: classSecurity},

	// Governance profiles (migration 0052) — classSecurity: profile authoring
	// IS the security-admin duty (§A/§B reconciliation). Six of these registered
	// on operatorOnly when they shipped, before the tier existed, and this is
	// the widening they were waiting for. There is deliberately no member-safe
	// read: a member learns their OWN effective ceiling from GET
	// /policies/default, not from the whole assignment table. Authoring is not
	// self-exemption — effectiveCeiling short-circuits on isOperator, so a
	// security admin's own runs stay bound by whichever profile applies to them.
	//
	// The seventh, POST /governance/preview, was BORN on this tier. It is a
	// READ that answers "which profile would bind these claims" by running
	// Store.ResolveGovernanceProfile — the enforcement path's own call — and it
	// is classSecurity for the reason the whole family is: the answer discloses
	// how the org's ceilings are assigned. A member is refused here and reads
	// their own ceiling from /policies/default instead, exactly as above.
	"GET /api/v1/governance":                     {class: classSecurity},
	"POST /api/v1/governance/profiles":           {class: classSecurity},
	"PUT /api/v1/governance/profiles/{id}":       {class: classSecurity},
	"DELETE /api/v1/governance/profiles/{id}":    {class: classSecurity},
	"POST /api/v1/governance/assignments":        {class: classSecurity},
	"DELETE /api/v1/governance/assignments/{id}": {class: classSecurity},
	"POST /api/v1/governance/preview":            {class: classSecurity},
	// User types (migration 0071_user_types): defining a type is the security
	// tier's duty, like a profile. Who IS a type is the /access routes'
	// (classAdmin). A member reads their own type from /me, never this list.
	"GET /api/v1/user-types":         {class: classSecurity},
	"POST /api/v1/user-types":        {class: classSecurity},
	"PUT /api/v1/user-types/{id}":    {class: classSecurity},
	"DELETE /api/v1/user-types/{id}": {class: classSecurity},
	// User drives (migration 0054) — SPLIT (issue #168, 0.8). The four routes
	// that NAME A HOST PATH (host_root) or a cluster storage class — creating,
	// listing, updating, and removing the drive itself — stay classAdmin, and
	// the contrast with the seven /governance rows directly above is the tier
	// line in one pair: "never the host" is precisely what separates SUPER
	// from securityOps. The other three — granting an allocation, revoking
	// one, and previewing whose drive resolves — are classSecurity: none of
	// the three names a host path, and a security admin's authority over
	// drives was already the DenyUserDrive door in the profile editor, which
	// they reach through /governance. A member is refused on all seven and
	// learns about their OWN drive from /me.user_drive instead — the same
	// "your own answer, never the whole table" split the governance rows draw
	// against /policies/default.
	"GET /api/v1/drives":                {class: classAdmin},
	"POST /api/v1/drives":               {class: classAdmin},
	"PUT /api/v1/drives/{id}":           {class: classAdmin},
	"DELETE /api/v1/drives/{id}":        {class: classAdmin},
	"POST /api/v1/drives/grants":        {class: classSecurity},
	"DELETE /api/v1/drives/grants/{id}": {class: classSecurity},
	"POST /api/v1/drives/preview":       {class: classSecurity},
	// Directory autocomplete (§I) — classSecurity, and the contrast with the
	// four classAdmin /access routes above is the whole tier argument in one
	// pair: those decide who DERIVES admin, this one only READS the directory
	// so the security admin authoring an assignment can pick the group instead
	// of hand-typing its object GUID. Gated at all (not on r) because it
	// discloses org structure — names, emails, group membership — to whoever
	// can call it. Registered unconditionally, so an unwired connector answers
	// 503 here rather than disappearing from this walk.
	"GET /api/v1/access/directory/search": {class: classSecurity},

	// member (any authenticated human/token; internally scoped where the
	// handler itself narrows the response — see the classMember doc)
	"GET /api/v1/approvals":    {class: classMember},
	"GET /api/v1/audit":        {class: classMember},
	"GET /api/v1/audit/export": {class: classMember},
	"GET /api/v1/integrations": {class: classMember},
	"GET /api/v1/me":           {class: classMember},
	// /me/ssh-keys (SSH lane, C2): classMember, NOT classOwner — this is a
	// self-service registry scoped to the caller's OWN principal AT THE
	// STORE (sshkeys.go's package doc), same shape as GET/POST /secrets
	// above; a foreign fingerprint on the DELETE path is store-level
	// principal-scoped so it already answers store.ErrNotFound (404)
	// without needing an owner/foreign id pair here.
	"GET /api/v1/me/ssh-keys": {class: classMember},
	// This caller's own Azure DevOps access state (scmaccess.go, #386): the
	// same self-service shape as /me/ssh-keys above — scoped entirely to the
	// caller's own OIDC subject (computeSCMAccessRows), so a member reading only
	// their own answer discloses nothing about anyone else.
	"GET /api/v1/me/scm-access": {class: classMember},
	// The per-user Azure DevOps sign-in (ado_entra.go): classMember, and for
	// the same reason as /me/ssh-keys above — a member signs in FOR
	// THEMSELVES. Both doors refuse a caller with no identity provider
	// subject, the capture is bound to that subject fail-closed, and the
	// credential is written under that principal's own namespace, so neither
	// route can reach anyone else's credential whatever tier the caller holds.
	"GET /api/v1/scm/azure-devops/signin":   {class: classMember},
	"GET /api/v1/scm/azure-devops/callback": {class: classMember},
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
	// The container LOGIN launch (0.7.2). classMember is AUTHENTICATION only
	// here: the real predicate is inside the handler (authorizeHarnessLogin) —
	// an operator always passes, anyone else needs an enabled `per_user` roster
	// row for the provider AND capAgent on that row's agent. It sits here rather
	// than on classAdmin because under such a row the credential this captures is
	// the CALLER'S OWN, and an admin-only door leaves a member with no route to
	// model access at all. The token PASTE and DISCONNECT above stay SUPER: those
	// write the deployment's shared credential.
	//
	// The body is NOT the generic "{}": handleHarnessLogin defaults an empty
	// provider to "anthropic", for which no per_user row can exist (per_user is
	// bedrock_sso-only), so the generic probe would 403 every member on a route
	// that admits them — asserting the opposite of this row. newAuthzMatrixServer
	// seeds the matching enabled row; the 403-WITHOUT-a-row case is
	// TestHarnessLogin_MemberRefusedWithoutPerUserRow, not a matrix arm.
	"POST /api/v1/setup/harness-login": {class: classMember, body: `{"provider":"aws"}`},
	// /me/run-layout is the same shape as /me/ssh-keys above: classMember, not
	// classOwner. It names no entity in its path — the STORE scopes it to the
	// caller's own principal, so there is no foreign row to 404 on.
	"GET /api/v1/me/run-layout":    {class: classMember},
	"PUT /api/v1/me/run-layout":    {class: classMember},
	"GET /api/v1/policies":         {class: classMember},
	"GET /api/v1/policies/default": {class: classMember},
	"GET /api/v1/policies/{id}":    {class: classMember},
	"GET /api/v1/runs":             {class: classMember},
	// PUT/DELETE moved here from classAdmin in 0.7 (migration `0050`,
	// per-principal secrets): self-service, scoped to the caller's OWN row at
	// the STORE (secretOwnerFromRequest) — same shape as /me/ssh-keys and
	// /me/tokens above. The own-row/other-owner/admin-?owner= scoping this
	// coarse matrix does not probe is secrets_test.go's job.
	"PUT /api/v1/secrets/{name}":    {class: classMember},
	"DELETE /api/v1/secrets/{name}": {class: classMember},
	"GET /api/v1/secrets":           {class: classMember},
	"GET /api/v1/setup/status":      {class: classMember},

	// Each person's own model-provider credential (0.8): the same self-service
	// shape, written into the caller's own namespace only. Who may reach a
	// given provider is model_provider_credentials_test.go's job.
	"PUT /api/v1/model-providers/{id}/credential":    {class: classMember},
	"DELETE /api/v1/model-providers/{id}/credential": {class: classMember},
	// Each person's own sign-in for a provider (MP-13): who may sign in to a
	// given provider is provider_signin_test.go's job.
	"POST /api/v1/model-providers/{id}/sign-in": {class: classMember},
	"PUT /api/v1/model-providers/{id}/sign-in":  {class: classMember},

	// The workspace READS stay member-class: an operator-owned workspace — every
	// pre-0048 row — is readable by any authenticated caller exactly as before.
	// What 0048 adds is that another MEMBER's owned row 404s, which is the same
	// "scoped internally, proven by the feature's own tests" arrangement GET
	// /runs and GET /approvals already have here (workspace_owner_test.go).
	"GET /api/v1/workspaces":                      {class: classMember},
	"GET /api/v1/workspaces/{id}":                 {class: classMember},
	"GET /api/v1/workspaces/{id}/build":           {class: classMember},
	"GET /api/v1/workspaces/{id}/observed-egress": {class: classMember},
	// Creating a workspace is the member ON-RAMP (the created row is
	// owner-stamped from the session) — there is no {id} to own yet.
	"POST /api/v1/workspaces":  {class: classMember},
	"POST /api/v1/auth/logout": {class: classMember},
	"POST /api/v1/me/ssh-keys": {class: classMember},
	// "View as member" (0.7.4, P2). classMember and NOT operatorOnly, on
	// purpose: toggling OFF has to be reachable from inside the mode, where the
	// caller's effective role IS member. Toggling ON from a real member session
	// is a no-op 200, so the member class costs nothing; the no-human lane
	// (admin token / local mode / no IdP) is refused inside the handler, which
	// is a 400 rather than a tier.
	//
	// No `body` override: the generic "{}" bodyFor sends decodes to an empty
	// View (off) and the handler answers 200, which is what classMember's
	// assertNotBlocked probe needs.
	"POST /api/v1/me/view":                     {class: classMember},
	"POST /api/v1/policies/grade":              {class: classMember},
	"POST /api/v1/runs":                        {class: classMember},
	"POST /api/v1/runs/preflight":              {class: classMember},
	"DELETE /api/v1/me/ssh-keys/{fingerprint}": {class: classMember},

	// owner-or-admin
	// The workspace CRUD/scan/build tier (0048): a member acts on the workspaces
	// THEY own, a foreign owned one is the byte-identical 404, and an admin
	// reaches every one. The 403 an operator-owned row still returns to a member
	// is NOT exercised here (this probe only ever seeds member-owned fixtures) —
	// TestWorkspaceOwnership_OperatorOwnedStaysAdminOnly pins it.
	// GET env-as-code is owner-or-super, NOT a member read, and it is the
	// emitted CONTENT that decides it: the files carry the internal registry
	// coordinate the workspace reads blank, the site-config artifact redirects
	// /site-config is admin-only for, and the operator's setup commands. Its
	// write twin below has always been operatorOnly (F287).
	"GET /api/v1/workspaces/{id}/env-as-code": {class: classOwner, entity: entityWorkspace, ownerTier: tierSuper},
	"PUT /api/v1/workspaces/{id}":             {class: classOwner, entity: entityWorkspace, ownerTier: tierSuper},
	"DELETE /api/v1/workspaces/{id}":          {class: classOwner, entity: entityWorkspace, ownerTier: tierSuper},
	"POST /api/v1/workspaces/{id}/scan":       {class: classOwner, entity: entityWorkspace, ownerTier: tierSuper},
	"POST /api/v1/workspaces/{id}/build":      {class: classOwner, entity: entityWorkspace, ownerTier: tierSuper},
	"GET /api/v1/runs/{id}":                   {class: classOwner, entity: entityRun, ownerTier: tierSecurity},
	"GET /api/v1/runs/{id}/grants":            {class: classOwner, entity: entityRun, ownerTier: tierSecurity},
	// Moving a run's end keeps a sandbox and its credentials alive: a write,
	// so not the security tier's inspect-or-stop.
	"PATCH /api/v1/runs/{id}":                 {class: classOwner, entity: entityRun, ownerTier: tierSuper},
	"GET /api/v1/runs/{id}/recording/{runID}": {class: classOwner, entity: entityRun, ownerTier: tierSuper},
	"POST /api/v1/runs/{id}/attach-ticket":    {class: classOwner, entity: entityRun, ownerTier: tierSuper},
	"POST /api/v1/runs/{id}/kill":             {class: classOwner, entity: entityRun, ownerTier: tierSecurity},
	// Thawing a paused run keeps its sandbox busy: a write, so not the
	// security tier's inspect-or-stop.
	"POST /api/v1/runs/{id}/resume": {class: classOwner, entity: entityRun, ownerTier: tierSuper},
	// Revive gives a run egress again: a write, like PATCH above.
	"POST /api/v1/runs/{id}/revive":  {class: classOwner, entity: entityRun, ownerTier: tierSuper},
	"POST /api/v1/runs/{id}/profile": {class: classOwner, entity: entityRun, ownerTier: tierSecurity},
	// The run cockpit's live evidence reads. classOwner, same gate as GET
	// /runs/{id} above: each names a run in its path and each exposes something
	// about a LIVE sandbox — the workspace's diff, its resource usage, and who
	// is holding its PTY. A foreign member gets the byte-identical 404.
	"GET /api/v1/runs/{id}/files":         {class: classOwner, entity: entityRun, ownerTier: tierSecurity},
	"GET /api/v1/runs/{id}/resources":     {class: classOwner, entity: entityRun, ownerTier: tierSecurity},
	"GET /api/v1/runs/{id}/attach-holder": {class: classOwner, entity: entityRun, ownerTier: tierSecurity},
	// Take-over ends another human's live terminal session. Still classOwner
	// (an owner may reclaim their own run's PTY, and the act is audited as
	// session.takeover) — NOT classMember, which would let anyone displace
	// anyone.
	"POST /api/v1/runs/{id}/attach/takeover": {class: classOwner, entity: entityRun, ownerTier: tierSuper},
	"POST /api/v1/approvals/{id}/approve":    {class: classOwner, entity: entityApproval, ownerTier: tierSecurity},
	"POST /api/v1/approvals/{id}/deny":       {class: classOwner, entity: entityApproval, ownerTier: tierSecurity},
	"GET /api/v1/approvals/{id}/paths":       {class: classOwner, entity: entityApproval, ownerTier: tierSecurity},

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
	//
	// classAdmin and explicitly NOT classSecurity (§B): an interactive shell in
	// a sandbox its holder does not own is the exact reach the security tier is
	// defined not to have — the case that killed the tier-ladder design. The
	// SUPER classification is load-bearing here, not incidental, which is why
	// TestSecurityAdminRouteTier 403s a security_admin on this route.
	"GET /api/v1/runs/{id}/attach": {class: classAdmin},

	// internal (run-token / ground-truth-token bearer only)
	"GET /api/v1/internal/approvals/{id}":         {class: classInternal},
	"GET /api/v1/internal/injection/{grantID}":    {class: classInternal},
	"POST /api/v1/internal/approvals":             {class: classInternal},
	"POST /api/v1/internal/approvals/{id}/expire": {class: classInternal},
	"POST /api/v1/internal/credentials/mint":      {class: classInternal},
	"POST /api/v1/internal/decisions":             {class: classInternal},
	"POST /api/v1/internal/activity":              {class: classInternal},
	"POST /api/v1/internal/groundtruth":           {class: classInternal},
	"POST /api/v1/internal/token/renew":           {class: classInternal},
	"PUT /api/v1/internal/recordings/{runID}":     {class: classInternal},
	"PUT /api/v1/internal/scan-results/{runID}":   {class: classInternal},
	"PUT /api/v1/internal/sso-token/{runID}":      {class: classInternal},

	// device (a `wdd_` device bearer on its own {id} only)
	"POST /api/v1/devices/{id}/audit":     {class: classDevice},
	"POST /api/v1/devices/{id}/heartbeat": {class: classDevice},
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
// A route may override it (classifiedRoute.body) when its AUTHORIZATION reads a
// request field — the generic body then probes the wrong decision entirely.
func bodyFor(method string, rc classifiedRoute) string {
	if method == http.MethodGet || method == http.MethodDelete {
		return ""
	}
	if rc.body != "" {
		return rc.body
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

func (fakeAuthzSessionRevocations) IsSessionRevoked(context.Context, string, string, time.Time) (bool, error) {
	return false, nil
}
func (fakeAuthzSessionRevocations) RevokeSub(context.Context, string) error { return nil }
func (fakeAuthzSessionRevocations) RevokeAll(context.Context) error         { return nil }

// newAuthzMatrixServer builds the MAXIMALLY-CONFIGURED server both matrix
// tests walk — and, since 0.7.2, the ROSTER-ENFORCED one: it seeds an agent
// roster (authzMatrixSiteConfig), so every consumer of this fixture now runs
// with agent-provider enforcement on rather than in legacy open mode. That is
// deliberate (POST /setup/harness-login's tier is per-request and unprovable
// without it) and it is why the store's PutSiteConfig must not persist — every conditional route mounted (OIDC, Secrets, RecordingStore,
// SessionRevocations) — so chi.Walk sees the whole table and the "every
// conditional route mounted" doctrine holds for TestSecurityAdminRouteTier
// too. Shared rather than duplicated: a second copy of this config is exactly
// where a conditional route silently goes unmounted and therefore unprobed.
// authzMatrixSiteConfig is the roster the matrix walks under: ONE enabled
// per_user bedrock_sso row for claude-code. It exists because POST
// /setup/harness-login's tier is per-request — an operator always reaches it, a
// member reaches it only when the org declared that each person signs in
// themselves — so without this row the matrix would probe the refusal and pass
// while asserting the opposite of that route's classification.
func authzMatrixSiteConfig() types.SiteConfig {
	return types.SiteConfig{AgentProviders: &types.AgentProviders{Agents: []types.AgentProvider{{
		ID:               "claude-code",
		Mechanism:        types.AgentMechanismBedrockSSO,
		CredentialSource: types.CredentialSourcePerUser,
		SSOStartURL:      "https://matrix-org.awsapps.com/start",
	}}}}
}

// shape, when given, adjusts the config before New — the deployment-shape knobs
// (AdminToken, SSOOnly, MemberMode) TestSSOShapeRoleMatrix walks this same
// router under.
func newAuthzMatrixServer(t *testing.T, shape ...func(*Config)) (*Server, *authzStore, *authzApprovals, *recording.FSStore) {
	t.Helper()
	ast := newAuthzStore()
	aap := newAuthzApprovals(ast)
	cfg := baseTestConfig(newHarness(t), ast)
	cfg.OIDC = &oidc.Authenticator{}
	cfg.Secrets = getErrStore{getErr: secretstore.ErrNotFound}
	cfg.Approvals = aap
	rs, err := recording.NewFSStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg.RecordingStore = rs
	cfg.SessionRevocations = fakeAuthzSessionRevocations{}
	ast.siteCfg = authzMatrixSiteConfig()
	for _, f := range shape {
		f(&cfg)
	}
	return New(cfg), ast, aap, rs
}

// newAuthzMatrixServerWithUI is newAuthzMatrixServer built THE WAY THE SHIPPED
// IMAGE IS: deploy/compose/Dockerfile.wardynd sets ENV WARDYN_UI_DIR=/srv/ui and
// ships the console there, so a UIDir is the default in production and the Helm
// chart inherits it from the image. mountUI (ui.go) registers the SPA catch-all
// `GET /*` in that configuration and the bare `GET /` only WITHOUT one — so the
// two configurations register DIFFERENT root routes, and a matrix built from
// only one of them proves route-completeness for a router the product does not
// ship.
func newAuthzMatrixServerWithUI(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<!doctype html>"), 0o600); err != nil {
		t.Fatal(err)
	}
	ast := newAuthzStore()
	cfg := baseTestConfig(newHarness(t), ast)
	cfg.OIDC = &oidc.Authenticator{}
	cfg.Secrets = getErrStore{getErr: secretstore.ErrNotFound}
	cfg.Approvals = newAuthzApprovals(ast)
	fs, err := recording.NewFSStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg.RecordingStore = fs
	cfg.SessionRevocations = fakeAuthzSessionRevocations{}
	ast.siteCfg = authzMatrixSiteConfig()
	cfg.UIDir = dir
	return New(cfg)
}

func TestAuthzMatrix(t *testing.T) {
	srv, ast, aap, rs := newAuthzMatrixServer(t)
	uiSrv := newAuthzMatrixServerWithUI(t)

	const memberSub = "sub-member"
	const otherSub = "sub-other-member"
	adminSess := ssoSession(t, "sub-admin", "admin@corp.example", oidc.RoleAdmin)
	memberSess := ssoSession(t, memberSub, "member@corp.example", oidc.RoleUser)

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
	// seedWorkspace creates a MEMBER-OWNED workspace (0048). ownedBy is the
	// member principal; the composition floor (one ephemeral source) keeps the
	// fixture free of any host path, so these probes exercise ownership alone.
	seedWorkspace := func(ownedBy string) uuid.UUID {
		id := uuid.New()
		ast.mu.Lock()
		ast.workspaces[id] = types.Workspace{
			ID: id, Name: "ws-" + ownedBy, OwnedBy: ownedBy, Status: types.WorkspaceScanned,
			Sources: []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeEphemeral, Target: "/home/agent/work"}},
		}
		ast.mu.Unlock()
		return id
	}

	// A live enrolled device: classDevice's positive control, and the {id}
	// every human credential is refused on, so a refusal there can never be
	// read as "no such device".
	matrixDeviceID := uuid.New()
	const matrixDeviceToken = deviceTokenPrefix + "matrix-device"
	if _, err := ast.CreateDevice(context.Background(), types.Device{ID: matrixDeviceID, Name: "matrix-laptop"}, matrixDeviceToken); err != nil {
		t.Fatalf("seed device: %v", err)
	}

	// discover every actual route via chi.Walk; classify or fail
	//
	// Both shipped configurations are walked, and the union is what must be
	// classified. UIDir decides the root route — `GET /*` with a console,
	// `GET /` without — so walking one server alone asserts completeness for a
	// router half the deployments do not run. The shipped image sets
	// WARDYN_UI_DIR, so the UI-less walk on its own is the configuration NOBODY
	// ships; the union covers the headless API-only install too.
	discovered := map[string]bool{}
	onlyWithoutUI := map[string]bool{}
	walk := func(name string, router chi.Router, into map[string]bool) {
		if err := chi.Walk(router, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
			discovered[method+" "+route] = true
			if into != nil {
				into[method+" "+route] = true
			}
			return nil
		}); err != nil {
			t.Fatalf("chi.Walk(%s): %v", name, err)
		}
	}
	walk("no UIDir", srv.router, onlyWithoutUI)
	walk("UIDir set (the shipped image)", uiSrv.router, nil)
	// The two configurations must genuinely differ, or this walked the same
	// router twice and the union proves nothing more than one walk did.
	if !discovered["GET /*"] || !discovered["GET /"] {
		t.Errorf("the two walks produced no UI-conditional split (GET /* present=%v, GET / present=%v) — "+
			"mountUI's two arms are the reason this test walks twice",
			discovered["GET /*"], discovered["GET /"])
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

	// execute the table
	for key, rc := range routeMatrix {
		key, rc := key, rc
		method, pattern, ok := strings.Cut(key, " ")
		if !ok {
			t.Fatalf("malformed routeMatrix key %q", key)
		}
		t.Run(key, func(t *testing.T) {
			body := bodyFor(method, rc)
			// A route the UI-less server does not register (today: the SPA
			// catch-all) is probed against the server that DOES register it.
			srv := srv
			if !onlyWithoutUI[key] {
				srv = uiSrv
			}
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
				p := buildPath(pattern, "x1") // CF: OLD probe
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

			case classDevice:
				own := buildPath(pattern, matrixDeviceID.String())
				// The control first: the device's own token on its own id is
				// admitted. Without it, every 401 below could be the routes being
				// broken rather than the credential being refused.
				if w := do(t, srv, method, own, matrixDeviceToken, body); w.Code == http.StatusUnauthorized || w.Code == http.StatusForbidden || w.Code == http.StatusNotFound {
					t.Errorf("device token on its own id: status = %d, want admitted; body=%s", w.Code, w.Body.String())
				}
				// Every HUMAN credential is refused — the admin's included: a
				// device is not a tier above or below the human ones, it is a
				// different kind of caller.
				for who, w := range map[string]*httptest.ResponseRecorder{
					"admin session":  doSSO(t, srv, method, own, adminSess, body),
					"member session": doSSO(t, srv, method, own, memberSess, body),
					"admin token":    do(t, srv, method, own, adminToken, body),
					"no credential":  doSSO(t, srv, method, own, nil, body),
				} {
					if w.Code != http.StatusUnauthorized {
						t.Errorf("%s: status = %d, want 401; body=%s", who, w.Code, w.Body.String())
					}
				}
				// A live device token on ANOTHER device's id: 404, never 403.
				if w := do(t, srv, method, buildPath(pattern, uuid.NewString()), matrixDeviceToken, body); w.Code != http.StatusNotFound {
					t.Errorf("device token on a foreign id: status = %d, want 404 (no existence oracle); body=%s", w.Code, w.Body.String())
				}

			// classAdmin and classSecurity are the SAME probe here — admin
			// passes, a member 403s, anonymous 401s. What separates them is the
			// security_admin axis, which this table cannot express in one
			// credential per class; TestSecurityAdminRouteTier walks this same
			// map and probes exactly that axis in BOTH directions.
			case classAdmin, classSecurity:
				p := buildPath(pattern, "x1")
				assertNotBlocked(t, "admin", doSSO(t, srv, method, p, adminSess, body))
				if w := doSSO(t, srv, method, p, memberSess, body); w.Code != http.StatusForbidden {
					t.Errorf("member: status = %d, want 403; body=%s", w.Code, w.Body.String())
				}
				if w := doSSO(t, srv, method, p, nil, body); w.Code != http.StatusUnauthorized {
					t.Errorf("unauthenticated: status = %d, want 401; body=%s", w.Code, w.Body.String())
				}

			case classMember:
				// A REAL uuid, not "x1". parseIDParam rejects a non-uuid with a
				// 400 BEFORE the handler's authorization runs, and
				// assertNotBlocked passes on any non-401/403 — so on every
				// {id}-bearing member route the old probe asserted nothing at
				// all about admitting members. Executed: three of them could
				// answer 403 "requires admin role" to every non-operator with
				// this whole package green. A syntactically valid id reaches
				// authorization; the 404 that usually follows is the handler
				// answering on its own merits, which is what this matrix
				// tolerates by design.
				p := buildPath(pattern, uuid.New().String())
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
				case entityWorkspace:
					ownedID, foreignID = seedWorkspace(memberSub), seedWorkspace(otherSub)
				default:
					t.Fatalf("classOwner route %q has no entity set", key)
				}
				// REQUIRED, the same way entity is: a new owner-scoped route
				// that does not state which admin tier may bypass ownership on
				// it cannot be classified, because the answer is not derivable
				// from the class (F155). Fatal here rather than defaulted, so
				// the omission is a failure and not a silent tierSuper.
				if rc.ownerTier != tierSuper && rc.ownerTier != tierSecurity {
					t.Fatalf("classOwner route %q has no ownerTier set (want tierSuper or tierSecurity) — "+
						"the tiers do not nest, so 'may an admin bypass ownership here' has two answers", key)
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

	// ── the query-param id row class (authz_query_id_test.go) ──
	//
	// The path probes above never put an id in the QUERY string, so a member
	// naming someone else's run in `?run_id=` on a classMember route was never
	// asked. Each row seeds an owned and a foreign entity and probes both.
	for key, row := range queryIDMatrix {
		if row.foreign == 0 {
			continue // pinnedBy carries it; TestQueryParamIDsAreClassified holds it to that
		}
		route, param, _ := strings.Cut(key, "?")
		method, pattern, _ := strings.Cut(route, " ")
		t.Run(key, func(t *testing.T) {
			var own, foreign string
			var leaks []string
			switch row.entity {
			case entityRun:
				ownRun, foreignRun := seedRun(memberSub), seedRun(otherSub)
				aap.seed(ownRun)
				leaks = []string{foreignRun.String(), aap.seed(foreignRun).String()}
				own, foreign = ownRun.String(), foreignRun.String()
			case entityPrincipal:
				own, foreign = memberSub, otherSub
				leaks = []string{otherSub}
			default:
				t.Fatalf("query-param row %q has no seedable entity %q", key, row.entity)
			}
			at := func(id string) string { return buildPath(pattern, "x1") + "?" + param + "=" + id }
			body := bodyFor(method, routeMatrix[route])

			w := doSSO(t, srv, method, at(foreign), memberSess, body)
			if w.Code != row.foreign {
				t.Errorf("member naming a FOREIGN %s: status = %d, want %d; body=%s", param, w.Code, row.foreign, w.Body.String())
			}
			for _, leak := range leaks {
				if strings.Contains(w.Body.String(), leak) {
					t.Errorf("member naming a FOREIGN %s got a body carrying %s: %s", param, leak, w.Body.String())
				}
			}
			if row.ownAdmitted {
				if w := doSSO(t, srv, method, at(own), memberSess, body); w.Code == http.StatusUnauthorized || w.Code == http.StatusForbidden || w.Code == http.StatusNotFound {
					t.Errorf("member naming their OWN %s: status = %d, want admitted; body=%s", param, w.Code, w.Body.String())
				}
			}
			assertNotBlocked(t, "admin naming a FOREIGN "+param, doSSO(t, srv, method, at(foreign), adminSess, body))
			if w := doSSO(t, srv, method, at(own), nil, body); w.Code != http.StatusUnauthorized {
				t.Errorf("unauthenticated: status = %d, want 401; body=%s", w.Code, w.Body.String())
			}
		})
	}
}

// TestSecurityAdminRouteTier is the security-admin twin of
// TestRequireOperator_OperatorPassesEveryGatedRoute (rbac_test.go), and the
// ONE test that makes §B's 26 SUPER / 14 SEC split enforceable rather than
// merely written down. It walks routeMatrix — the SAME table TestAuthzMatrix
// proves exhaustive against chi.Walk, so a new gated route cannot join the
// router without landing in one of these two arms — and probes the axis
// TestAuthzMatrix structurally cannot: what a SECURITY_ADMIN session gets.
//
// Both directions are asserted, and both matter:
//
//   - classSecurity: a security_admin must NOT be blocked. Dropping one SEC
//     re-registration in routes.go (registering it on operatorOnly again)
//     turns that route's probe into a 403 and reddens this test — the
//     counterfactual the split is worth having.
//   - classAdmin: a security_admin MUST get 403. Without this half the tier
//     could silently be widened into a ladder (security_admin ⊆ admin), which
//     is precisely the shape §B refuses — a ladder stamps `admin` on their SSH
//     key and hands them a shell in every developer's sandbox.
//
// A MEMBER is refused by both classes, asserted here too so the file reads as
// one statement about the tier rather than two half-statements: the security
// tier is a THIRD value, not "member with extras".
//
// Same probe doctrine as TestAuthzMatrix: a non-401/403 status is a pass (the
// handler ran and answered on its own merits — a 4xx for the deliberately
// bogus "x1" path id or the empty body is expected and irrelevant here).
//
// And that doctrine is where this test stops. "x1" is not a UUID, so on every
// {id}-bearing route parseIDParam 400s before the handler's own authorization
// runs: what is pinned here is the ROUTER GATE, never what the tier can then
// reach. A route sitting on the right tier is not evidence that the handler
// behind it respects ownership — which is exactly how a route can be gated
// perfectly consistently onto the wrong tier with nothing to notice.
// TestSecurityAdminOnForeignWorkspace (security_admin_workspace_test.go) is the
// compensating arm for the workspace-scoped members of this set: it seeds a
// real, foreign, member-owned workspace and asserts what each handler does with
// it, deriving the route set from this same table so a re-tiering changes its
// coverage without an edit.
func TestSecurityAdminRouteTier(t *testing.T) {
	srv, _, _, _ := newAuthzMatrixServer(t)
	secSess := ssoSession(t, secAdminSub, secAdminMail, oidc.RoleSecurityAdmin)
	memberSess := ssoSession(t, "sub-member-tier", "member-tier@corp.example", oidc.RoleUser)

	// F155: the owner-scoped half, probed against a REAL, SEEDED, FOREIGN
	// entity rather than the "x1" placeholder the gated-route loop uses.
	//
	// The placeholder is why this could not simply be folded into the loop
	// below: on an {id}-bearing route parseIDParam 400s before the handler's
	// own authorization runs, so a bogus id proves nothing about ownership —
	// and ownership is the WHOLE question for classOwner. These routes have no
	// router-level tier gate at all; the tier lives in the handler's predicate
	// (helpers.go's ownsRunOrAdmin vs ownsRunOrSuperAdmin vs
	// ownsWorkspaceOrAdmin), which is exactly why nothing was checking it.
	t.Run("owner-scoped routes", func(t *testing.T) {
		srv, ast, aap, rs := newAuthzMatrixServer(t)
		const foreignSub = "sub-foreign-owner"
		seedRun := func() uuid.UUID {
			id := uuid.New()
			ast.mu.Lock()
			ast.runs[id] = types.AgentRun{ID: id, CreatedBy: foreignSub, State: types.RunRunning, Agent: "claude-code"}
			ast.mu.Unlock()
			// A real cast, so GET .../recording/{runID} answers about ACCESS
			// rather than about a missing file.
			_ = rs.SaveCast(context.Background(), id.String(), strings.NewReader(`{"version":2}`+"\n"))
			return id
		}
		seedWorkspace := func() uuid.UUID {
			id := uuid.New()
			ast.mu.Lock()
			ast.workspaces[id] = types.Workspace{
				ID: id, Name: "ws-foreign", OwnedBy: foreignSub, Status: types.WorkspaceScanned,
				Sources: []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeEphemeral, Target: "/home/agent/work"}},
			}
			ast.mu.Unlock()
			return id
		}
		sec := ssoSession(t, secAdminSub, secAdminMail, oidc.RoleSecurityAdmin)
		var probed int
		for key, rc := range routeMatrix {
			if rc.class != classOwner {
				continue
			}
			method, pattern, ok := strings.Cut(key, " ")
			if !ok {
				t.Fatalf("malformed routeMatrix key %q", key)
			}
			t.Run(key, func(t *testing.T) {
				var foreignID uuid.UUID
				switch rc.entity {
				case entityRun:
					foreignID = seedRun()
				case entityApproval:
					foreignID = aap.seed(seedRun())
				case entityWorkspace:
					foreignID = seedWorkspace()
				default:
					t.Fatalf("classOwner route %q has no entity set", key)
				}
				w := doSSO(t, srv, method, buildPath(pattern, foreignID.String()), sec, bodyFor(method, rc))
				switch rc.ownerTier {
				case tierSuper:
					// The byte-identical 404 a non-owner gets: no existence
					// oracle, and no ladder — a security admin does not reach a
					// live PTY, a recording replay or a workspace write on
					// someone else's entity.
					if w.Code != http.StatusNotFound {
						t.Errorf("security_admin on a FOREIGN entity, tierSuper route: status = %d, want 404 "+
							"(the tiers do not nest); body=%s", w.Code, w.Body.String())
					}
				case tierSecurity:
					// Inspect-or-stop IS this tier's warrant, so the bypass must
					// work — a route silently narrowed to super would strand
					// incident response, which is the other direction of the
					// same drift.
					if w.Code == http.StatusNotFound || w.Code == http.StatusForbidden || w.Code == http.StatusUnauthorized {
						t.Errorf("security_admin on a FOREIGN entity, tierSecurity route: status = %d, want the "+
							"handler's own answer — inspect-or-stop is this tier's warrant; body=%s", w.Code, w.Body.String())
					}
				default:
					t.Fatalf("classOwner route %q has no ownerTier set", key)
				}
			})
			probed++
		}
		// Every owner-scoped route is probed, not a subset: the count is what
		// catches a route that silently leaves classOwner.
		// 17 since F287 moved GET /workspaces/{id}/env-as-code here from
		// classMember (its emitted files are the operator's authored
		// environment, and its write twin was already operatorOnly); 18 since
		// #569 added PATCH /runs/{id}; 19 since #575 added POST
		// /runs/{id}/revive; 20 since #1066 added GET /approvals/{id}/paths;
		// 21 since #572 added POST /runs/{id}/resume.
		if probed != 20 {
			t.Errorf("probed %d classOwner routes, want 20 — a route that left classOwner takes its tier "+
				"assertion with it", probed)
		}
	})

	var sec, super int
	for key, rc := range routeMatrix {
		key, rc := key, rc
		if rc.class != classAdmin && rc.class != classSecurity {
			continue
		}
		method, pattern, ok := strings.Cut(key, " ")
		if !ok {
			t.Fatalf("malformed routeMatrix key %q", key)
		}
		t.Run(key, func(t *testing.T) {
			body := bodyFor(method, rc)
			p := buildPath(pattern, "x1")
			if rc.class == classSecurity {
				assertNotBlocked(t, "security_admin", doSSO(t, srv, method, p, secSess, body))
			} else if w := doSSO(t, srv, method, p, secSess, body); w.Code != http.StatusForbidden {
				t.Errorf("security_admin on a SUPER route: status = %d, want 403 (the tiers do not nest); body=%s",
					w.Code, w.Body.String())
			}
			// Both classes refuse a member, with the byte-identical body — a
			// member must never learn WHICH admin tier a route sits on.
			w := doSSO(t, srv, method, p, memberSess, body)
			if w.Code != http.StatusForbidden {
				t.Errorf("member on a %s route: status = %d, want 403; body=%s", rc.class, w.Code, w.Body.String())
			} else if !strings.Contains(w.Body.String(), "requires admin role") {
				t.Errorf("member 403 body = %q, want the tier-agnostic wording", w.Body.String())
			}
		})
		if rc.class == classSecurity {
			sec++
		} else {
			super++
		}
	}

	// The split itself, pinned as a number: §B decided 14 SEC of the 40 gated
	// routes, plus /governance's 7 and §I's directory search (all new in 0.7,
	// outside that count) = 22, and 26 SUPER — plus /drives' 7, also new in 0.7
	// and born SUPER, = 33. R1 then moved ONE route across:
	// POST /workspaces/{id}/record, which §B put in the workspace
	// egress-decision lane but which LAUNCHES a credentialed, host-mounting,
	// open-egress sandbox and stamps the caller as its owner rather than
	// deciding anything. R1 also moved FOUR reads OUT of classMember and into
	// SUPER — GET /site-config, /sources, /sources/{id} and /base-images, which
	// returned operator topology and credential refs to the whole member tier —
	// so 21 SEC / 38 SUPER. 0.7.2 then added the two /workspace-providers verbs,
	// born SUPER for the same topology reason as those four reads, = 40 SUPER,
	// and the two /agent-providers verbs beside them for the same reason again,
	// = 42 SUPER. 0.7.2 then moved ONE route OUT: POST /setup/harness-login, the
	// container LOGIN launch, which under a `per_user` agent row captures the
	// CALLER'S OWN model credential — an admin-only door there leaves a member
	// with no route to model access at all, so the tier moved and the predicate
	// went inside the handler. Its two sibling credential verbs (the token paste
	// and the disconnect) did NOT move: they write the deployment's shared
	// credential. = 41 SUPER. Issue #168 (0.8) then moved THREE /drives routes
	// OUT of SUPER and into SEC — POST /drives/grants, DELETE
	// /drives/grants/{id}, POST /drives/preview — because none of the three
	// names a host path, unlike the four /drives routes that stayed = 24 SEC /
	// 38 SUPER. 0.8's hybrid enrolment then added three: minting a device
	// enrolment token creates a credential, so it is born SUPER (= 39), while
	// the device inventory and revoke are the /tokens pair's twins and land on
	// the security tier (= 26 SEC). 0.8's user types then added four /user-types
	// routes on the security tier, a profile's peers (= 30 SEC), #506 added
	// the device pair's twins for enrolment tokens not yet redeemed, list and
	// revoke (= 32 SEC), CS-5 added the credential erase, which only
	// subtracts (= 33 SEC), and #612 the GET/PUT /permissions/availability pair
	// beside the grant rows (= 35 SEC). 0.8's model providers add GET/PUT
	// /model-providers, SUPER for the agent roster's reason (= 41). #575 then
	// added the standing-runs pair (GET /admin/runs/proxy-window, POST
	// /admin/runs/restart), born SUPER because a restart replaces proxies on runs
	// the caller does not own (= 43 SUPER). A route silently reclassified in the
	// table above would still pass every probe — it would just be enforcing the
	// WRONG tier, exactly the drift the per-route loop cannot see.
	if sec != 35 || super != 43 {
		t.Errorf("tier split = %d security / %d admin, want 35 / 43 (§B's 14 SEC + governance's 7 + §I's directory search + the device inventory and revoke + the enrolment-token list and revoke + the 4 /user-types routes + the credential erase + the 2 /permissions/availability routes, MINUS record, PLUS #168's 3 moved /drives routes; and 26 SUPER + /drives' 7 + record + the four operator-topology reads + 0.7.2's GET/PUT /workspace-providers and GET/PUT /agent-providers + the device enrolment-token mint + 0.8's GET/PUT /model-providers + #575's standing-runs pair, MINUS the reclassified POST /setup/harness-login, MINUS #168's 3 moved /drives routes)", sec, super)
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
	member := ssoSession(t, memberSub, "member-kind@corp.example", oidc.RoleUser)

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

// in-memory store.Store fake
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
	// siteCfg is real rather than a hard-wired zero because 0.7.2 put a ROUTE'S
	// TIER behind it: POST /setup/harness-login admits a member only when the
	// agent roster declares a per_user credential source, so a zero site config
	// would make the matrix's member arm assert the refusal instead of the
	// admission. Seeded by newAuthzMatrixServer; every other consumer reads the
	// same empty document it read before.
	siteCfg types.SiteConfig
	// The hybrid device capability (store.DeviceStore), so the matrix walks
	// the device routes with it PRESENT — a device-route 401 is then deviceAuth
	// refusing the credential, not the capability being absent
	// (devices_test.go).
	*fakeDeviceStore
}

func newAuthzStore() *authzStore {
	return &authzStore{
		runs:            map[uuid.UUID]types.AgentRun{},
		workspaces:      map[uuid.UUID]types.Workspace{},
		tickets:         map[string]store.AttachTicket{},
		fakeDeviceStore: newFakeDeviceStore(),
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

// CountActiveRunsBy is real rather than a 0 stub so the governance quota reads
// the same rows every other creator-scoped answer here does.
func (s *authzStore) CountActiveRunsBy(_ context.Context, createdBy string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, r := range s.runs {
		if r.CreatedBy == createdBy && !r.State.IsTerminal() {
			n++
		}
	}
	return n, nil
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
func (s *authzStore) SetRunDiskMiB(_ context.Context, id uuid.UUID, mib int) error {
	return s.mutateRun(id, func(r *types.AgentRun) { r.DiskMiB = mib })
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
func (s *authzStore) UpdateWorkspace(context.Context, uuid.UUID, types.Workspace, bool) (types.Workspace, error) {
	return types.Workspace{}, store.ErrNotFound
}

// SetWorkspaceApprovedEgress / SetWorkspaceDeniedEgress mirror the PG
// statements' SEMANTICS, EgressEditedAt included. That stamp is not bookkeeping:
// it is the whole reason the documented undo (this PUT) survives a restart, so a
// fake that replaced the list without it would keep every test green while
// ReconcileWorkspaceEgressDecisions quietly resurrected removed hosts — the
// exact defect the reconcile probes pin.
func (s *authzStore) SetWorkspaceApprovedEgress(_ context.Context, id uuid.UUID, domains []string) (types.Workspace, error) {
	return s.setWorkspaceEgressList(id, domains, true)
}

func (s *authzStore) SetWorkspaceDeniedEgress(_ context.Context, id uuid.UUID, domains []string) (types.Workspace, error) {
	return s.setWorkspaceEgressList(id, domains, false)
}

func (s *authzStore) setWorkspaceEgressList(id uuid.UUID, domains []string, approved bool) (types.Workspace, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ws, ok := s.workspaces[id]
	if !ok {
		return types.Workspace{}, store.ErrNotFound
	}
	if approved {
		ws.ApprovedEgress = domains
	} else {
		ws.DeniedEgress = domains
	}
	at := time.Now().UTC()
	ws.EgressEditedAt = &at
	s.workspaces[id] = ws
	return ws, nil
}

// AddWorkspaceEgressDecision mirrors the SEMANTICS of the PG statement backing
// it, not merely its signature. The cross-list removal is the half worth
// mirroring: deny beats allow everywhere the proxy evaluates policy, so a host
// left on both lists makes one direction a silent no-op — a fake that only
// appended would let exactly that fault through green. Dedupe and the
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
func (s *authzStore) SetWorkspaceLLMCred(context.Context, uuid.UUID, *types.WorkspaceLLMCred) (types.Workspace, error) {
	return types.Workspace{}, store.ErrNotFound
}

// SetWorkspaceOwner is REAL (not a stub) so the reassign tests can assert the
// column actually moved and that a re-read shows the row operator-owned.
func (s *authzStore) SetWorkspaceOwner(_ context.Context, id uuid.UUID, owner string) (types.Workspace, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ws, ok := s.workspaces[id]
	if !ok {
		return types.Workspace{}, store.ErrNotFound
	}
	ws.OwnedBy = owner
	s.workspaces[id] = ws
	return ws, nil
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
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.siteCfg, nil
}

// PutSiteConfig ECHOES without persisting, and that is deliberate: the matrix
// probes every write route with a generic body, so a persisting double would
// let `PUT /site-config` (or `PUT /agent-providers`) blank the seeded roster
// mid-walk and make a LATER route's classification depend on map iteration
// order. Nothing in the matrix reads back what a probe wrote.
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
func (s *authzStore) RefreshSSHKeyRoles(context.Context, string, string, time.Time) error {
	return nil
}
func (s *authzStore) RefreshAPITokenIdentity(context.Context, string, string, string, []string, bool) error {
	return nil
}

// per-user api tokens (migration 0045)
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

// capability grants (migration 0042)
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
func (s *authzStore) ListGroupDenyGrants(context.Context, string) ([]types.CapabilityGrant, error) {
	return nil, nil
}
func (s *authzStore) ListCapabilityGrantsFor(context.Context, []string, []string, string) ([]types.CapabilityGrant, error) {
	return nil, nil
}
func (s *authzStore) GetCapabilityEnforcement(context.Context) (map[string]bool, error) {
	return map[string]bool{}, nil
}
func (s *authzStore) PutCapabilityEnforcement(_ context.Context, enabled map[string]bool) (map[string]bool, error) {
	return enabled, nil
}

func (s *authzStore) ListCapabilityRestrictions(context.Context) (map[string]map[string]bool, error) {
	return map[string]map[string]bool{}, nil
}

func (s *authzStore) SetCapabilityRestriction(context.Context, string, string, bool, string) error {
	return nil
}

// role mappings (migration 0051, Phase 2 lane A)
//
// Same honest-empty-state posture as the capability grant stubs above: no
// rows is exactly a freshly-upgraded deployment with nothing configured on
// the People step, so /access's routes resolve without panicking and the
// matrix's admin/member/unauthenticated boundary is what gets exercised, not
// a hand-rolled fixture.
func (s *authzStore) UpsertRoleMapping(_ context.Context, m types.RoleMapping) (types.RoleMapping, error) {
	return m, nil
}
func (s *authzStore) DeleteRoleMapping(context.Context, uuid.UUID) error {
	return store.ErrNotFound
}
func (s *authzStore) ListRoleMappings(context.Context) ([]types.RoleMapping, error) {
	return nil, nil
}

// user types (migration 0071_user_types)
//
// The same honest empty state: the matrix exercises the tier on /user-types,
// not the rows behind it.
func (s *authzStore) ListUserTypes(context.Context) ([]types.UserType, error) { return nil, nil }
func (s *authzStore) GetUserType(context.Context, string) (types.UserType, error) {
	return types.UserType{}, store.ErrNotFound
}
func (s *authzStore) CreateUserType(_ context.Context, t types.UserType) (types.UserType, error) {
	return t, nil
}
func (s *authzStore) UpdateUserType(context.Context, types.UserType) (types.UserType, error) {
	return types.UserType{}, store.ErrNotFound
}
func (s *authzStore) UserTypeReferences(context.Context, string) (int, error)  { return 0, nil }
func (s *authzStore) UserTypeTokenStamps(context.Context, string) (int, error) { return 0, nil }
func (s *authzStore) DeleteUserType(context.Context, string) error             { return store.ErrNotFound }

// governance profiles (migration 0052)
//
// Same honest-empty-state posture as the two stub blocks above, and here it is
// also the exact state the matrix wants: NO profile and NO assignment is the
// deployment that has not adopted governance profiles, which by the
// absent-row doctrine behaves byte-for-byte as it did before this feature
// existed. So every route this matrix walks resolves as it always has, and the
// only thing under test on the /governance rows is the authorization boundary.
// ResolveGovernanceProfile returns ErrNotFound for the same reason — "no
// assignment matched", which the resolver reads as the deployment ceiling.
// The precedence matrix itself is a store-level test against a real Postgres
// (internal/store/governance_pg_test.go), where rows can actually exist.
func (s *authzStore) UpsertGovernanceProfile(_ context.Context, p types.GovernanceProfile) (types.GovernanceProfile, error) {
	return p, nil
}
func (s *authzStore) DeleteGovernanceProfile(context.Context, uuid.UUID) error {
	return store.ErrNotFound
}
func (s *authzStore) ListGovernanceProfiles(context.Context) ([]types.GovernanceProfile, error) {
	return nil, nil
}
func (s *authzStore) UpsertGovernanceAssignment(_ context.Context, a types.GovernanceAssignment) (types.GovernanceAssignment, error) {
	return a, nil
}
func (s *authzStore) DeleteGovernanceAssignment(context.Context, uuid.UUID) error {
	return store.ErrNotFound
}
func (s *authzStore) ListGovernanceAssignments(context.Context) ([]types.GovernanceAssignment, error) {
	return nil, nil
}
func (s *authzStore) ResolveGovernanceProfile(context.Context, []string, []string, string) (*types.GovernanceProfile, types.CapabilitySubjectType, error) {
	return nil, "", store.ErrNotFound
}
func (s *authzStore) HasGroupTierAssignments(context.Context) (bool, error) {
	return false, nil
}

// user drives (migration 0054)
//
// Same honest-empty-state posture as the governance block above, and here it is
// also the exact state the matrix wants: NO drive and NO grant is the
// deployment that has not adopted user drives, which by the absent-row doctrine
// mounts nothing — byte for byte the behaviour before the feature existed. So
// every route this matrix walks resolves as it always has, and the only thing
// under test on a /drives row is the authorization boundary. The precedence
// table itself is a store-level test against a real Postgres
// (internal/store/user_drives_test.go), where rows can actually exist.
func (s *authzStore) UpsertUserDrive(_ context.Context, d types.UserDrive, _ bool) (types.UserDrive, error) {
	return d, nil
}
func (s *authzStore) GetUserDrive(context.Context, uuid.UUID) (types.UserDrive, error) {
	return types.UserDrive{}, store.ErrNotFound
}
func (s *authzStore) DeleteUserDrive(context.Context, uuid.UUID) error {
	return store.ErrNotFound
}
func (s *authzStore) ListUserDrives(context.Context) ([]types.UserDriveListItem, error) {
	return nil, nil
}
func (s *authzStore) UpsertUserDriveGrant(_ context.Context, g types.UserDriveGrant, _ bool) (types.UserDriveGrant, error) {
	return g, nil
}
func (s *authzStore) DeleteUserDriveGrant(context.Context, uuid.UUID) (types.UserDriveGrant, error) {
	return types.UserDriveGrant{}, store.ErrNotFound
}
func (s *authzStore) ListUserDriveGrants(context.Context) ([]types.UserDriveGrant, error) {
	return nil, nil
}
func (s *authzStore) ResolveUserDrive(context.Context, []string, []string, string) (
	*types.UserDrive, *types.UserDriveGrant, types.CapabilitySubjectType, error) {
	return nil, nil, "", store.ErrNotFound
}
func (s *authzStore) HasGroupTierDriveGrants(context.Context) (bool, error) {
	return false, nil
}

// in-memory ApprovalService fake, ownership-aware

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

func (a *authzApprovals) CancelForRun(_ context.Context, runID uuid.UUID, reason string) (map[string]int, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	byKind := map[string]int{}
	for id, ap := range a.byID {
		if ap.RunID != runID || ap.State != types.ApprovalPending {
			continue
		}
		ap.State = types.ApprovalCancelled
		ap.DecidedBy, ap.Reason = "system", reason
		a.byID[id] = ap
		byKind[approval.TallyKey(ap)]++
	}
	return byKind, nil
}

func (a *authzApprovals) ExpireOne(_ context.Context, id uuid.UUID, _, _ string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	ap, ok := a.byID[id]
	if !ok || ap.State != types.ApprovalPending {
		return nil
	}
	ap.State = types.ApprovalExpired
	ap.DecidedBy = "system"
	a.byID[id] = ap
	return nil
}

func (a *authzApprovals) CountForRun(_ context.Context, runID uuid.UUID) (int, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := 0
	for _, ap := range a.byID {
		if ap.RunID == runID {
			n++
		}
	}
	return n, nil
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
