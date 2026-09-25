// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// the closed kind set
//
// Nine kinds, and this slice is the ONLY place the set is written down —
// migration 0042 deliberately puts no CHECK on capability_grants.capability, so
// a tenth kind is a constant here plus its enforcement call site, with no DDL.
// The console's own list (ui/src/app/lib/permissions-copy.ts CAPABILITY_KINDS)
// mirrors these ids and must not drift.
//
// Eight of the nine NARROW what a member may already do; capImage WIDENS (a
// member cannot name a custom image at all today). Both directions resolve
// through the one resolver below (capBatch.decide) — the difference is the
// kind's row in capKinds.
const (
	// capEgressHost bounds the egress hosts a member may author on an inline
	// policy, and the hosts a member may approve an egress request for. Values
	// are a bare host or a "*.suffix" wildcard, matched by entryCoversAny — the
	// one host matcher this package already has.
	capEgressHost = "egress_host"
	// capSecret bounds which secret names a member may reference from an inline
	// policy, and which ones they can see listed. Exact names.
	capSecret = "secret"
	// capWorkspace bounds which onboarded workspaces a member may launch a run
	// against. Values are workspace uuids (as strings).
	capWorkspace = "workspace"
	// capImage WIDENS: it names the exact image refs a member may launch
	// directly, a power members do not have at all without a grant. Note that
	// devcontainer_repo is NOT a capability and stays unconditionally
	// admin-only — it executes attacker-authored build configuration, which is
	// not a thing to hand out one row at a time.
	capImage = "image"
	// capAgent NARROWS: it bounds which agent/harness a member may launch —
	// req.Agent, the member's own free-text choice, gated at denyMemberRequest.
	// Values are the exact `--agent` string plus `*`.
	//
	// DELIBERATELY narrowing rather than widening, and the direction is decided
	// by capBatch.decide's step 4 below: a WIDENING kind refuses on
	// !enforced, which would refuse every member run on every deployment that
	// has not enforced this kind — i.e. all of them on upgrade day. Launching an
	// agent is something every member could already do, so it stays allowed
	// until an admin enforces it.
	//
	// DELIBERATELY not constrained to the harness catalog either, here or at the
	// grant write boundary: harnessByID documents a WARDYN_AGENT_IMAGES-only
	// custom agent as supported, so a catalog check would make an operator's own
	// entry unwriteable.
	capAgent = "agent"
	// capIntegration NARROWS: it bounds which AI-provider integration a member
	// may name on a run — req.IntegrationID, and NOTHING ELSE.
	//
	// Tier 1 only. resolveRunIntegration has three tiers, and the other
	// two — a workspace's own LLMCred pin (tier 2) and the operator's
	// DefaultFor:agent_runs site default (tier 3) — are OPERATOR-authored. Gating
	// them would contradict the doctrine rendered on the very screen this kind
	// appears on ("a capability bounds what a member chose, never what an admin
	// pre-authorized", permissions-copy.ts PERM.DOCTRINE) and would let one `all`
	// deny row strip the site's model access deployment-wide. So the gate lives
	// at denyMemberRequest, on the one member-authored input, and never inside
	// resolveRunIntegration — which operator callers reach too.
	capIntegration = "integration"
	// capWorkspaceProvider NARROWS: it bounds which git provider row a member's
	// work may come from — the row workspace_providers.go's providerFor resolves
	// a repository's derived clone URL to. Values are the provider row's own id
	// (the lowercase-ASCII slug an integration id is written in), plus `*`.
	//
	// Narrowing, on the same rule capAgent's comment states: a member could
	// already launch a run against ANY onboarded repository, so the unenforced
	// default stays ALLOWED and an upgraded 0.7.1 deployment is unchanged. A
	// DENY row still bites immediately, before anyone enforces the kind — which
	// is what the other six get too, and is the on-ramp the deny-above-the-
	// switch precedence exists for.
	//
	// It gates the member's OWN choice of where work comes from, and nothing
	// else. Whether a repository is admissible AT ALL is a separate, admin-level
	// question (admitRepoURL's verdict, an operator's 422) that binds operators
	// too; this kind only decides whether THIS member may bring work from a
	// provider row the deployment carries.
	//
	// SIX doors, because a repository reaches a clone by six member-reachable
	// paths: POST /runs over the resolved spec and over the legacy repo field,
	// workspace create and edit, and the two server-side clones (scan, build).
	// A gate on fewer is a gate a member walks around by editing the workspace.
	//
	// It names the provider ROW, not the repository: a per-repo capability would
	// be an ACL this feature does not have (admission is URL-prefix), and the
	// row is the unit an admin actually writes down.
	capWorkspaceProvider = "workspace_provider"
	// capModelProvider NARROWS: it bounds which model provider a person's run
	// may use (SiteConfig.ModelProviders, by id, plus `*`) — the one a request
	// names, a workspace pins, or an agent's default reaches them by. Narrowing
	// on capAgent's rule: every member could already reach every provider.
	//
	// Unlike capIntegration it gates the workspace PIN too. Every model
	// credential is the person's own, so a pin is no longer an admin handing a
	// member access they could not otherwise get; a pin naming an ungranted
	// provider is refused, never exempt (enforceRunModelProvider).
	capModelProvider = "model_provider"
	// capFeature NARROWS: it bounds whether a person may MINT a personal
	// credential at all. Two values, a closed set (featureValues), plus `*`:
	// featureSSHKey gates POST /me/ssh-keys and featureAPIToken gates POST
	// /me/tokens, one check at each mint door (the token door also keeps
	// member mode's 409; the SSH door stores a capped key instead, #564).
	//
	// Narrowing, on capAgent's rule: every signed-in person could already add a
	// key and mint a token, so the unenforced default stays ALLOWED and an
	// upgraded deployment is unchanged. A DENY row bites at once, which is how
	// one user type is turned off ("SSH keys: Blocked" for a Portfolio manager).
	//
	// Mint only. A key or token that already exists keeps working until it is
	// removed or revoked; the kind decides what may be ADDED, never re-checks
	// what is there.
	capFeature = "feature"
)

// The closed value set of capFeature. canonicalGrantValue refuses any other
// value, so a misspelt row can never sit in the table protecting nothing.
const (
	featureSSHKey   = "ssh_key"
	featureAPIToken = "api_token"
)

var featureValues = []string{featureSSHKey, featureAPIToken}

// capabilityKinds is the closed set, in the order the admin surface shows them.
var capabilityKinds = []string{capEgressHost, capSecret, capWorkspace, capImage, capAgent, capIntegration, capWorkspaceProvider, capModelProvider, capFeature}

// capKindsVersion numbers the kind table, and GET /me/capabilities returns it so
// a client holding a copy of the set (the console's CAPABILITY_KINDS) can tell
// its copy is stale. Monotonic: a change to capKinds — a kind added, or a row's
// direction changed — bumps it by one and it never goes down.
// TestCapKindsVersionPinsTheTable fails on a table change that forgets to.
const capKindsVersion = 3

// validCapabilityKind reports whether kind is one of the nine. The API write
// boundary uses it in place of the CHECK the schema deliberately does not have.
func validCapabilityKind(kind string) bool { return slices.Contains(capabilityKinds, kind) }

// capWildcard matches every value of its kind. Spelled the same for all nine so
// an admin does not have to learn a per-kind syntax for "all of them".
const capWildcard = "*"

// resolution

// canonicalUserSubject canonicalizes ONE human identity — an IdP `sub` or an
// email claim — into the exact string a `user` subject row is matched by. It is
// the user half of oidc.CanonicalGroupSubject, and it is used by BOTH sides of
// the match: the caller's own subject list (capabilitySubjects, just below) and
// every surface that writes a `user` subject (validateCapabilityGrant,
// validateGovernanceAssignment, the governance preview's claim normalizer).
// One function, so what a caller can BE is exactly what an admin can WRITE.
//
// The ASCII guard runs on the raw value, before the fold, and the order is the
// security property — the same ordering CanonicalGroupSubject, ParseRoleMap and
// deriveRole already use, for the same reason. strings.ToLower does UNICODE
// case mapping: KELVIN SIGN U+212A folds to ASCII 'k' and U+0130 folds to ASCII
// 'i'. Folding first therefore let a crafted email claim "Kim@Korp.com"
// resolve to "kim@korp.com" — the exact string another human's capability
// grants, governance assignment and drive allocation are written against, since
// every one of those columns is matched by `subject = ANY($1::text[])` exact
// equality. A plain ToLower is the whole rule only for an ASCII subject — not
// for one that isn't.
//
// A NON-ASCII identity is kept VERBATIM rather than dropped, and that is where
// this differs from the group rule — deliberately. A group subject is matched
// against a STORED login-time snapshot that can only carry printable ASCII, so
// a non-ASCII group name is a row nobody can ever match and the write boundary
// refuses it. A user subject is the caller's OWN identity, recomputed per
// request from claims the IdP chooses: dropping it would silently discard a
// DENY written against a human whose directory hands out non-ASCII subjects,
// and refusing it at the write boundary would make that human ungovernable.
// Verbatim is safe in the only direction that matters — a string that is never
// folded can only ever equal itself, so it inherits nobody's grants — and
// because both sides call this function, a subject that can be written is
// exactly a subject that can be matched.
//
// Trimmed on both arms: an untrimmed sub arm would let a claim with a trailing
// space resolve to a subject no write boundary (which trims) could ever produce.
func canonicalUserSubject(s string) string {
	s = strings.TrimSpace(s)
	if !oidc.ASCIIOnly(s) {
		return s
	}
	return strings.ToLower(s)
}

// capabilitySubjects returns the grant subjects that describe the caller on
// ctx: their user identities and their group snapshot.
//
// users carries BOTH the lowercased OIDC sub and the email, because an admin
// writing a grant knows one or the other and should not have to guess which one
// this IdP made authoritative. Matching either is a deliberate widening of who
// a `user` row hits — and it is safe in the direction that matters, since a
// DENY written against either identity also hits.
//
// groups is nil for a pre-0.6 cookie (the snapshot predates the field) and
// empty when the IdP sent nothing usable. stale reports that the group half is
// UNANSWERABLE, which is two shapes and not one: the nil snapshot above, and a
// snapshot that is present but PARTIAL — sessionGroups dropped entries at the
// cookie byte cap, or the IdP never sent the claim at all (an Entra groups
// overage), or the row is a pre-0.7 API token whose completeness was never
// recorded (Server.apiTokenAuth reads a NULL marker as truncated). Either way
// the caller's group grants cannot be evaluated in full until they sign in
// again or re-mint. Callers surface that; they must not silently treat it as
// "no groups" — that is the reading that lets a group DENY evaporate.
//
// The returned groups stay the PARTIAL list rather than being blanked: a row
// that matches one of them is a real match, and for the deny direction seeing
// more is strictly safer. It is the rows that are MISSING that stale is for
// (capScan's unresolvable-deny check).
func capabilitySubjects(ctx context.Context) (users, groups []string, stale bool) {
	if sub := canonicalUserSubject(oidcHumanFromContext(ctx)); sub != "" {
		users = append(users, sub)
	}
	if email := canonicalUserSubject(oidcEmailFromContext(ctx)); email != "" && !slices.Contains(users, email) {
		users = append(users, email)
	}
	groups = oidcGroupsFromContext(ctx)
	return users, groups, groups == nil || oidcGroupsTruncatedFromContext(ctx)
}

// the kind table

// capDirection is which way a kind moves a member's power, and so what its
// enforcement switch means while it is OFF. One rule — "an upgrade with no
// configuration changes nothing" — lands on opposite defaults, because the two
// directions start from opposite postures.
type capDirection int

const (
	// capNarrowing bounds something a member could already do: an unenforced
	// kind ALLOWS, because 0.5 already let a member do it.
	capNarrowing capDirection = iota
	// capWidening hands out something a member could not do: the switch ON is a
	// PRECONDITION on any allow, because 0.5 already refused it. The console's
	// own copy states this contract for image (permissions-copy.ts KIND.image).
	capWidening
)

// capKind is one kind's row: DATA the one resolver reads, rather than a rule
// each wrapper re-derived from a comment.
type capKind struct {
	direction capDirection
	// hostSet: values are host sets ("host", "host:port", "*.suffix") compared
	// by the one host matcher, and a deny overlaps in EITHER direction
	// (capValueOverlaps). Every other kind is an exact, case-sensitive id.
	hostSet bool
	// restrictable: an "Available to" list may restrict one value of this kind
	// (user-types design 2.6: the admin-configured resources a person is
	// offered). Read by capBatch.decide's step 3 and the availability write.
	restrictable bool
	// gatesAdminPins: the kind also bounds a value an ADMIN pinned, not only the
	// member's own choice. False for every kind but capModelProvider — "a
	// capability bounds what a member chose, never what an admin pre-authorized"
	// (capIntegration); true for capModelProvider, whose workspace pin
	// enforceRunModelProvider checks.
	gatesAdminPins bool
	// reason is the authz.denied reason a refusal of this kind carries. The
	// widening kind's refusal is the BYOI one: image is refused as a member
	// bringing their own image, whichever door asked.
	reason authz.Reason
}

// capKinds is the table, keyed by exactly the names in capabilityKinds
// (TestCapKindTableIsTheClosedSet).
var capKinds = map[string]capKind{
	capEgressHost:        {direction: capNarrowing, hostSet: true, reason: authz.ReasonCapabilityEgressHost},
	capSecret:            {direction: capNarrowing, reason: authz.ReasonCapabilitySecret},
	capWorkspace:         {direction: capNarrowing, restrictable: true, reason: authz.ReasonCapabilityWorkspace},
	capImage:             {direction: capWidening, restrictable: true, reason: authz.ReasonBYOIMember},
	capAgent:             {direction: capNarrowing, restrictable: true, reason: authz.ReasonCapabilityAgent},
	capIntegration:       {direction: capNarrowing, restrictable: true, reason: authz.ReasonCapabilityIntegration},
	capWorkspaceProvider: {direction: capNarrowing, restrictable: true, reason: authz.ReasonCapabilityWorkspaceProvider},
	capModelProvider:     {direction: capNarrowing, restrictable: true, gatesAdminPins: true, reason: authz.ReasonCapabilityModelProvider},
	capFeature:           {direction: capNarrowing, restrictable: true, reason: authz.ReasonCapabilityFeature},
}

// the wrappers
//
// Every capability question in this package is answered by capBatch.decide,
// the ONE grant resolver. The functions below are one-value doors onto it; each
// reaches it through capBatchFor, so a resolution that installed withCapBatch
// shares one snapshot across every question it asks.

// capAllowed answers "may this caller use `kind` at `value`" with the kind's
// own direction from capKinds, in capBatch.decide's seven-step order.
//
// Deny sits ABOVE the enforcement switch on purpose: it makes deny rows the
// adoption on-ramp. An admin can blacklist one host for one contractor without
// flipping the whole deployment fail-closed, which is the only way this feature
// gets used before anyone trusts it. There is no user-over-group precedence,
// because "Bob's user allow overrode the group deny" is a breach report.
//
// Errors are never allowed to read as permission. A store failure returns
// (false, err) so the caller answers 500 rather than deciding either way; a nil
// Store (test wiring only — wardynd always wires PG) is that same error here,
// NOT a silent allow. capSeamAllowed is the door that answers a store-less
// build instead of erroring.
//
// ponytail: no cache across requests. A new grant takes effect on the very next
// request. A process-local cache is the HA blocker OPERATIONS already names for
// other state, and a stale permission cache is a security bug rather than a
// slow page — add one only behind a shared invalidation channel.
//
// DELIBERATELY isOperator (capBatch.decide's step 1) — this is the INVARIANT
// that makes handing /permissions to a security admin safe at all: no
// capability kind, present or future, can ever widen the admin tier. A security
// admin is capability-BOUNDED exactly like a member (they may self-grant
// through /permissions, audited, and still reach nothing this exemption would
// give them). See the three-tier doctrine on internal/auth/oidc's
// RoleSecurityAdmin. A pinning test asserts it.
func (s *Server) capAllowed(ctx context.Context, kind, value string) (bool, error) {
	if s.cfg.Store == nil && validCapabilityKind(kind) && !s.isOperator(ctx) {
		return false, fmt.Errorf("api: capability %q cannot be resolved: no store configured", kind)
	}
	return s.capSeamAllowed(ctx, kind, value)
}

// capGranted asks the WIDENING question of `kind`, whatever its row says: does
// this caller hold an explicit grant AND is the kind enforced? `image` is the
// only widening kind and the only production caller; the direction is fixed
// here rather than read from capKinds because this door has always answered
// that question for any kind it was handed, and the widening answer is the
// narrower of the two. A build with no store holds no rows, so it refuses —
// which is again 0.5.
func (s *Server) capGranted(ctx context.Context, kind, value string) (bool, error) {
	if !validCapabilityKind(kind) {
		return false, fmt.Errorf("api: unknown capability kind %q", kind)
	}
	return s.capBatchFor(ctx).decide(ctx, kind, capWidening, value)
}

// capSeamAllowed is the resolver at an enforcement seam: the kind's own
// direction, and the one case a seam has that capAllowed refuses to guess at —
// a deployment with no Store cannot hold a grant OR an enforcement row, so a
// NARROWING kind behaves exactly as 0.5 did (allowed) and a WIDENING kind is
// refused (capBatch.decide's nil-Store rule).
//
// capAllowed keeps erroring on a nil Store, because a RESOLVER that answered
// "allowed" for a question it could not resolve is the failure mode this file
// is built against. The resolver is asked about a deployment that has
// capability state and cannot reach it, while a seam is running in a build that
// has none at all (this package's own harnesses; wardynd always wires PG).
func (s *Server) capSeamAllowed(ctx context.Context, kind, value string) (bool, error) {
	return s.capBatchFor(ctx).allowed(ctx, kind, value)
}

// capScan reports whether a DENY overlaps and/or an ALLOW covers value — the
// grant half of the resolver (capBatch.scan), stale-snapshot refusal included.
func (s *Server) capScan(ctx context.Context, kind, value string) (deny, allow bool, err error) {
	return s.capBatchFor(ctx).scan(ctx, kind, value)
}

// capUnresolvableGroupDeny reports whether ANY group-subject DENY row of this
// kind could cover value. It is asked only when capabilitySubjects says the
// caller's group snapshot is unanswerable, and it is what keeps that refusal
// SCOPED — the same scoping ceilingWithUnusableGroups gets from
// HasGroupTierAssignments (governance.go). A blanket refusal on every stale
// snapshot would deny every pre-0.6 cookie and every pre-0.7 API token on every
// deployment, including the overwhelming majority that hold no group deny rows
// at all, and "an upgrade with no configuration changes nothing" is the rule
// this whole file is built on.
//
// ALLOW rows deliberately need no equivalent: a group allow the scan cannot see
// costs the caller access (a narrowing kind falls through to the enforcement
// switch, a widening one refuses outright), which is the fail-CLOSED direction
// already.
//
// The rows are selected in SQL, not scanned in Go, and the reason is not
// tidiness. This runs on the path taken by every caller whose group snapshot is
// unanswerable — which, by the fail-closed reading of a NULL groups_truncated
// column, is EVERY API token minted before 0.7, on every request it makes.
// Asking ListCapabilityGrants (the whole table) there meant the cost of an
// authorization check scaled with the size of the grant table: measured at
// 68 ms per call against 20k grants versus 0.35 ms for the indexed sibling.
// That is an availability surface — a caller holding one pre-0.7 token can
// force an unbounded read per checked value — and it grows precisely as a
// deployment adopts the feature.
//
// ListGroupDenyGrants applies EXACTLY the predicate the old loop applied
// (group + deny + this kind) in the query instead, leaving only the
// value-overlap test in Go, where the one host/wildcard matcher lives.
// TestCapUnresolvableGroupDenyMatchesFullScan pins the path against a full scan
// of the old shape over a generated matrix, because a FASTER fail-closed check
// that stops firing is a breach, not a regression. Within one resolution the
// read is memoized per kind (capBatch.unresolvableGroupDeny).
func (s *Server) capUnresolvableGroupDeny(ctx context.Context, kind, value string) (bool, error) {
	return s.newCapBatch(ctx).unresolvableGroupDeny(ctx, kind, value)
}

// capValueMatches reports whether a grant written for grantValue covers want.
//
// egress_host reuses entryCoversAny (artifact_redirect.go), the SAME matcher
// the egress substitution drop uses — so "*.pythonhosted.org" and "pypi.org:443"
// behave here exactly as they do everywhere else egress hosts are compared.
// Deliberately not a second host matcher: two matchers that disagree is how a
// deny gets bypassed by a port suffix.
//
// Every other kind is an exact, case-sensitive compare. A secret name, a
// workspace uuid, an image ref, an agent id, an integration id and a git
// provider row id are all identifiers where a near-miss must not match; only
// egress hosts have a defensible subdomain semantics (capKind.hostSet).
func capValueMatches(kind, grantValue, want string) bool {
	grantValue = strings.TrimSpace(grantValue)
	if grantValue == capWildcard {
		return true
	}
	if capKinds[kind].hostSet {
		return entryCoversAny(grantValue, map[string]bool{egressEntryHost(want): true})
	}
	return grantValue == strings.TrimSpace(want)
}

// capValueOverlaps is the DENY question, and it is deliberately not
// capValueMatches: an allow has to COVER the want, but a deny only has to
// OVERLAP it. For every exact kind the two are the same question, but an
// egress host is a SET — "*.example.com" is every host under it — and a want
// that is itself a wildcard can contain a denied host without being covered by
// it. Asking only "does the deny cover the want" let a member whose allowlist
// said "*.example.com" keep an entry that includes the denied
// "secret.example.com": the deny row protected nothing, which is the one thing
// this file promises it always does ("deny beats everything").
//
// So a deny bites when the sets intersect in EITHER direction — deny covers
// want, or want covers deny. Both directions go through capValueMatches, so
// there is still exactly one host matcher.
func capValueOverlaps(kind, grantValue, want string) bool {
	if capValueMatches(kind, grantValue, want) {
		return true
	}
	// Only host-set kinds have set-valued entries; every other kind is an exact
	// identifier, where "want covers deny" is the same compare reversed.
	return capKinds[kind].hostSet && capValueMatches(kind, want, grantValue)
}

// the one resolver

// capBatch is THE capability-grant resolver. It resolves the caller's grants,
// the enforcement map and (only when needed) the unresolvable-group-deny rows
// at most ONCE, then answers every value in process through decide.
//
// A run's egress allowlist is NOT a handful: it is spec.AllowedDomains, taken
// verbatim from the request body. Measured, against a real store.PG over
// loopback with an empty grants table, before the batch existed: ~505µs per
// entry, linear, so one member request carrying the most entries that fit under
// maxJSONBody (52,425 x "api.anthropic.com") spent 104,850 sequential round
// trips and 27.0s resolving — 157,275 and 45.3s when the caller's group
// snapshot is unanswerable. POST /runs/preflight is on the member router group
// and persists nothing, so that is repeatable for free. After: 2 round trips
// (3 stale), flat in N.
//
// Not a cache, deliberately, and that distinction is the whole reason this is
// safe: a batch lives for ONE resolution (a ctx memo installed by withCapBatch,
// or a one-shot batch per call without one) and is discarded, so a grant
// revoked between requests still binds on the next one.
//
// Lazy, so a resolution with nothing to check performs no reads at all, and in
// the order the per-value wrappers always read (decide's step comments).
type capBatch struct {
	s        *Server
	operator bool
	noStore  bool

	// subj is the caller's subjects (callerSubjects), read with the grants —
	// never before, so a question no grant can settle performs no read and an
	// unknown user type fails exactly the questions that consult the rows.
	subj callerSubjects

	// byKind indexes the caller's grants by capability, built ONCE — so a spec
	// asking about N egress hosts does not pay O(N x every grant the caller
	// holds) inside one handler, on a path any authenticated member reaches
	// (POST /runs/preflight). nil until read.
	byKind map[string][]types.CapabilityGrant

	enfLoaded bool
	enf       map[string]bool

	// restricted is capability_restrictions (kind -> restricted values), read
	// at most once, and only when a restrictable kind's answer depends on it.
	restrictLoaded bool
	restricted     map[string]map[string]bool

	// groupDeny memoizes ListGroupDenyGrants PER KIND — the narrow read, not the
	// full table. A map because the store call is keyed by capability and a
	// resolution can ask about more than one (egress_host, then secret).
	groupDeny map[string][]types.CapabilityGrant
}

// newCapBatch snapshots the store-free inputs decide needs before it ever
// reads: the operator bit and the nil-Store bit.
func (s *Server) newCapBatch(ctx context.Context) *capBatch {
	return &capBatch{s: s, noStore: s.cfg.Store == nil, operator: s.isOperator(ctx)}
}

// capBatchKey carries one resolution's batch: the ownedSecretMemo pattern, a
// pointer holder installed once at the top of a resolution and shared by every
// door further down without re-threading a parameter.
type capBatchKey struct{}

type capBatchMemo struct{ b *capBatch }

// withCapBatch installs the batch memo for a resolution. Idempotent: a nested
// call keeps the outer memo, so the whole resolution shares one snapshot.
// Single-goroutine by construction, on ownedSecretMemo's terms.
func withCapBatch(ctx context.Context) context.Context {
	if _, ok := ctx.Value(capBatchKey{}).(*capBatchMemo); ok {
		return ctx
	}
	return context.WithValue(ctx, capBatchKey{}, &capBatchMemo{})
}

// capBatchFor returns the resolution's batch, built on first use. Without a
// memo in ctx it is a fresh one-shot batch: exactly the per-call reads the
// one-value wrappers made before they shared one.
func (s *Server) capBatchFor(ctx context.Context) *capBatch {
	m, ok := ctx.Value(capBatchKey{}).(*capBatchMemo)
	if !ok {
		return s.newCapBatch(ctx)
	}
	if m.b == nil {
		m.b = s.newCapBatch(ctx)
	}
	return m.b
}

// allowed answers one value in the direction capKinds gives its kind.
func (b *capBatch) allowed(ctx context.Context, kind, value string) (bool, error) {
	k, ok := capKinds[kind]
	if !ok {
		return false, fmt.Errorf("api: unknown capability kind %q", kind)
	}
	return b.decide(ctx, kind, k.direction, value)
}

// decide is the rule order, the ONE place it is written — union of every
// subject's rows, deny-wins:
//
//  1. Admin, admin token and local mode are EXEMPT: a capability bounds a
//     member; the admin tier is the one writing the grants.
//  2. An overlapping DENY ⇒ Deny.
//  3. A RESTRICTED value ("Available to: Only...") makes the kind count as
//     enforced for that value, and only an allow naming the value itself
//     lets a caller in — a wildcard allow does not list anyone.
//  4. Widening && !enforced ⇒ Deny.
//  5. A matching ALLOW ⇒ Allow.
//  6. Narrowing && !enforced ⇒ Allow. An absent enforcement row is a
//     freshly-upgraded deployment, which must behave as it did before.
//  7. Otherwise ⇒ Deny.
//
// A build with no Store holds no rows and no switch: a widening kind is
// refused and a narrowing kind allowed — the 0.5 answer for each.
//
// Reads are lazy and in the order the per-value wrappers always made them: a
// narrowing kind reads grants (then, on a stale snapshot, the group-deny rows)
// and the switch only when no row settled it; a widening kind reads its switch
// FIRST and never reads grants while it is off. Steps 2 and 4 both answer Deny,
// so reading 4's input first changes which reads happen, never the answer — and
// it keeps a grants-read failure on an unenforced widening kind a 403, not a
// 500.
func (b *capBatch) decide(ctx context.Context, kind string, dir capDirection, value string) (bool, error) {
	if b.operator { // 1
		return true, nil
	}
	if b.noStore {
		return dir == capNarrowing, nil
	}
	r := &capRead{b: b, kind: kind, value: value}
	ok := r.steps(ctx, dir == capWidening)
	if r.err != nil {
		return false, r.err // never an answer the store could not back
	}
	return ok, nil
}

// capRead is one decide call's lazy view of the batch: each input is read on
// first use, and the first error stops every later read.
type capRead struct {
	b           *capBatch
	kind, value string
	err         error
	scanned     bool
	deny, allow bool
}

// steps is decide's 2-7. Any error lands on r.err and decide discards the answer.
func (r *capRead) steps(ctx context.Context, widening bool) bool {
	// 2. An overlapping deny. A widening kind whose switch is off skips the read:
	// step 4 answers Deny whatever this one would.
	if !(widening && !r.enforced(ctx)) && r.denied(ctx) {
		return false
	}
	// 3. A restricted value makes the kind count as enforced. It lives in
	// capRead.enforced, so steps 2, 4 and 6 all see it, and step 5 asks
	// capRead.granted for an allow naming the value itself.

	// 4. Widening && !enforced.
	if widening && !r.enforced(ctx) {
		return false
	}
	// 5. A matching allow.
	if r.granted(ctx) {
		return true
	}
	// 6. Narrowing && !enforced.
	if !widening && !r.enforced(ctx) {
		return true
	}
	// 7. Otherwise.
	return false
}

// enforced reports whether the kind's switch is on, or (step 3) the value is
// restricted. An absent row is off; the restriction is read only when the
// switch alone does not already answer.
func (r *capRead) enforced(ctx context.Context) bool {
	if r.err != nil {
		return false
	}
	var on bool
	on, r.err = r.b.enforced(ctx, r.kind)
	if r.err == nil && !on {
		on, r.err = r.b.isRestricted(ctx, r.kind, r.value)
	}
	return on && r.err == nil
}

func (r *capRead) denied(ctx context.Context) bool { r.scan(ctx); return r.deny }

// granted is step 5. On a restricted value only an allow naming the value
// counts: "Only..." lists who gets it, and a wildcard allow written for the
// whole kind lists nobody in particular. The restriction is read only when
// the allow that matched is a wildcard.
func (r *capRead) granted(ctx context.Context) bool {
	r.scan(ctx)
	if !r.allow || r.err != nil || r.b.namedAllow(r.kind, r.value) {
		return r.allow && r.err == nil
	}
	restricted, err := r.b.isRestricted(ctx, r.kind, r.value)
	if err != nil {
		r.err = err
		return false
	}
	return !restricted
}

func (r *capRead) scan(ctx context.Context) {
	if r.scanned || r.err != nil {
		return
	}
	r.scanned = true
	r.deny, r.allow, r.err = r.b.scan(ctx, r.kind, r.value)
}

// enforced reads the switch map once per batch.
func (b *capBatch) enforced(ctx context.Context, kind string) (bool, error) {
	if !b.enfLoaded {
		enf, err := b.s.cfg.Store.GetCapabilityEnforcement(ctx)
		if err != nil {
			return false, fmt.Errorf("api: read capability enforcement: %w", err)
		}
		b.enf, b.enfLoaded = enf, true
	}
	return b.enf[kind], nil
}

// isRestricted reads capability_restrictions once per batch and reports
// whether value is restricted. A kind that cannot be restricted never reads.
func (b *capBatch) isRestricted(ctx context.Context, kind, value string) (bool, error) {
	if !capKinds[kind].restrictable {
		return false, nil
	}
	if !b.restrictLoaded {
		rs, err := b.s.cfg.Store.ListCapabilityRestrictions(ctx)
		if err != nil {
			return false, fmt.Errorf("api: read capability restrictions: %w", err)
		}
		b.restricted, b.restrictLoaded = rs, true
	}
	return b.restricted[kind][strings.TrimSpace(value)], nil
}

// namedAllow reports whether the caller holds an allow naming value itself,
// not a wildcard. Asked only after scan loaded the grants; restrictable kinds
// are exact ids, so the compare is capValueMatches' exact arm.
func (b *capBatch) namedAllow(kind, value string) bool {
	for _, g := range b.byKind[kind] {
		if g.Effect == types.CapabilityAllow && strings.TrimSpace(g.Value) == strings.TrimSpace(value) {
			return true
		}
	}
	return false
}

// scan walks the caller's own grants of one kind and reports whether a DENY
// overlaps value and/or an ALLOW covers it — the single place a stored row is
// compared against a request.
//
// An unanswerable group snapshot is not "no group rows". capabilitySubjects'
// stale bit says the group list the grants were read for is missing or
// partial, so a group DENY the caller actually holds may simply not be in them
// — and every door reads a clean scan as permission. effectiveCeiling
// (governance.go) already refuses on exactly this input. So when the snapshot
// is unanswerable and a group deny row COULD cover this value, the scan reports
// the deny it cannot rule out — UNCONDITIONALLY on allow: gating it on !allow
// would let a value the caller holds a user-tier allow for slip past a group
// deny nobody could evaluate.
func (b *capBatch) scan(ctx context.Context, kind, value string) (deny, allow bool, err error) {
	if b.byKind == nil {
		// An unknown user type fails the read, never resolving it without the
		// type (callerSubjects).
		subj, err := b.s.callerSubjects(ctx)
		if err != nil {
			return false, false, err
		}
		grants, err := b.s.cfg.Store.ListCapabilityGrantsFor(ctx, subj.users, subj.groups, subj.userType)
		if err != nil {
			return false, false, fmt.Errorf("api: resolve capability %q: %w", kind, err)
		}
		b.subj = subj
		b.byKind = make(map[string][]types.CapabilityGrant, len(capabilityKinds))
		for _, g := range grants {
			b.byKind[g.Capability] = append(b.byKind[g.Capability], g)
		}
	}
	// The kind's OWN rows, not every row the caller holds.
	for _, g := range b.byKind[kind] {
		b.s.capRowsScanned.Add(1)
		if g.Effect == types.CapabilityDeny {
			if capValueOverlaps(kind, g.Value, value) {
				return true, false, nil // deny is final
			}
			continue
		}
		if capValueMatches(kind, g.Value, value) {
			allow = true
		}
	}
	if b.subj.stale {
		unresolved, err := b.unresolvableGroupDeny(ctx, kind, value)
		if err != nil {
			return false, false, err
		}
		if unresolved {
			// Logged, not audited: the door that asked writes its own
			// authz.denied with the reason it knows, and this line is what
			// tells an operator the refusal was about COMPLETENESS rather than
			// a row naming this human. Value is left out — a secret name or a
			// workspace id is the door's to log, not the resolver's.
			slog.Warn("api: capability refused because the caller's group snapshot is unanswerable and a group deny grant of this kind exists",
				"capability", kind, "principal", oidcHumanFromContext(ctx))
			return true, false, nil
		}
	}
	return false, allow, nil
}

// unresolvableGroupDeny is capUnresolvableGroupDeny's read, memoized per kind.
// Loaded only on the stale path, so an answerable caller never pays for it.
func (b *capBatch) unresolvableGroupDeny(ctx context.Context, kind, value string) (bool, error) {
	if b.groupDeny == nil {
		b.groupDeny = map[string][]types.CapabilityGrant{}
	}
	grants, loaded := b.groupDeny[kind]
	if !loaded {
		var err error
		// ListGroupDenyGrants filters subject_type='group' AND effect='deny' AND
		// capability=$1 in SQL, so nothing is filtered again here and the match
		// is capValueOverlaps alone — one shared matcher.
		if grants, err = b.s.cfg.Store.ListGroupDenyGrants(ctx, kind); err != nil {
			return false, fmt.Errorf("api: resolve capability %q: %w", kind, err)
		}
		b.groupDeny[kind] = grants
	}
	for _, g := range grants {
		if capValueOverlaps(kind, g.Value, value) {
			return true, nil
		}
	}
	return false, nil
}

// the owned-secret seam

// ownedSecretMemoKey carries one request's owned-secret memo. A pointer holder
// rather than the value, so a memo installed once at the top of a resolution is
// shared by every site further down without re-threading a parameter through
// three signatures two other packages already call.
type ownedSecretMemoKey struct{}

// ownedSecretMemo is one request's answer to "which secrets does this principal
// own", resolved at most once PER OWNER.
//
// It exists for the same reason capBatch does, on the list capBatch did not
// cover. capBatch made the egress loop flat in len(allowed_domains) and
// maxAllowedDomainsPerSpec capped that list — but the member pipeline's OTHER
// caller-sized list, spec.eligible_grants, has no count cap at all and bought an
// unmemoized For(owner).List per grant at THREE sites in one request:
// filterMemberGrants' 6c own-key arm, narrowMemberInlinePolicy's ownership
// exemption (twice per grant, secret_ref and known_hosts_ref) and
// validateInlineSecretRefs' unknown-name arm. Measured on this tree with a
// counting store double: 3N+1 owner-scoped reads for N grants, N chosen entirely
// by the request body under maxJSONBody, on POST /runs/preflight — a route that
// persists nothing and is repeatable for free. After: one read per owner per
// request, flat in N.
//
// Not a cache, on the same terms capBatch states: it lives for ONE resolution
// and is discarded with the request, so a secret created or deleted between
// requests is seen by the next one.
//
// A store ERROR is memoized as "owns nothing", which is what ownsSecret already
// answers for a failed read — fail closed, and identical within the request
// whether it is asked once or a thousand times. Re-reading per value on the
// error path would leave exactly the amplification this closes, on the path a
// struggling store is least able to absorb.
type ownedSecretMemo struct {
	byOwner map[string]map[string]bool
}

// withOwnedSecretMemo installs the memo for a resolution. Idempotent: a nested
// call keeps the outer memo, so the whole request shares one answer.
func withOwnedSecretMemo(ctx context.Context) context.Context {
	if _, ok := ctx.Value(ownedSecretMemoKey{}).(*ownedSecretMemo); ok {
		return ctx
	}
	return context.WithValue(ctx, ownedSecretMemoKey{}, &ownedSecretMemo{byOwner: map[string]map[string]bool{}})
}

// ownsSecretMemoized is ownsSecret through the request's memo. Without a memo in
// ctx it IS ownsSecret, byte for byte — every caller outside the member run
// pipeline (policies.go, workspace_refs.go, compose_setup.go) keeps today's
// behaviour with no signature change.
//
// Single-goroutine by construction: the member pipeline is one sequential
// resolution inside one handler, and the memo is installed per resolution rather
// than per process, so there is no second writer to guard against. If a caller
// ever fans out over one memo, this needs a mutex — say so here rather than
// discovering it as a race.
func (s *Server) ownsSecretMemoized(ctx context.Context, owner, name string) bool {
	memo, ok := ctx.Value(ownedSecretMemoKey{}).(*ownedSecretMemo)
	if !ok || owner == "" || name == "" || s.cfg.Secrets == nil {
		return s.ownsSecret(ctx, owner, name)
	}
	set, loaded := memo.byOwner[owner]
	if !loaded {
		set = map[string]bool{}
		// Names only, own rows only — the same read ownsSecret makes, and the
		// same reason: an ownership claim must be provable, never merely
		// unrefuted.
		if names, err := s.cfg.Secrets.For(owner).List(ctx); err == nil {
			for _, n := range names {
				set[n] = true
			}
		}
		memo.byOwner[owner] = set
	}
	return set[name]
}
