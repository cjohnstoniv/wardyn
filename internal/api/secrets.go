// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// secretNameRE constrains secret names to a safe, predictable identifier set.
var secretNameRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9._-]{0,126}[a-z0-9])?$`)

// secretsMaxPerOwner bounds how many secrets one namespace may hold — the same
// runaway-add guard apiTokenMaxPerPrincipal and sshMaxKeysPerPrincipal are, and
// answered the same way (422, "remove one first", no audit event).
//
// 100 rather than those two's 20, because this namespace is DEPLOYMENT
// INVENTORY, not one human's credentials: the operator's "" namespace holds
// every provider key, every git/SSH credential and every integration-derived
// row (the integration fold alone derives ~15), so a 20-cap would refuse a
// live install the moment it upgraded.
const secretsMaxPerOwner = 100

// reservedSecretNames are platform-internal keys that must not be overwritten
// or deleted through the API (they would brick identity/session handling), and
// that the injection sink refuses to resolve as a stored value. Managed-harness
// OAuth blobs are deliberately NOT listed: reservedSecret seals every name
// harnessCredSecretName generates by PATTERN below (reservedSecret's own doc and
// harnessCredSecretName in harnesscred.go carry that rationale), so there is no
// list to keep in sync.
var reservedSecretNames = map[string]bool{
	"wardyn-signing-key":    true,
	"wardyn-session-key":    true,
	"wardyn-ssh-host-key":   true,
	"wardyn-ui-session-key": true,
}

// reservedSecret reports whether name is a platform-internal / managed-credential
// key that the generic secrets API must not touch and the injection sink must not
// resolve as a raw value. It covers the static reservedSecretNames set PLUS the
// managed-harness token-blob PATTERN (wardyn-harness-<provider>-oauth) — so a
// FUTURE provider row in agentHarnessLogin (e.g. codex) is sealed automatically,
// closing the "add a provider, forget to reserve its blob" landmine even though
// its name is generated dynamically by harnessCredSecretName. Use this at every
// reserved-name guard instead of a bare map lookup.
func reservedSecret(name string) bool {
	if reservedSecretNames[name] {
		return true
	}
	return strings.HasPrefix(name, "wardyn-harness-") && strings.HasSuffix(name, "-oauth")
}

// ReservedPlatformSecret reports whether name is one of this package's
// platform-internal reserved keys — the base set both the generic-secrets-API
// guard and every credential sink build on, so a true here means refused at all
// of them.
//
// Exported for ONE caller: cmd/wardynd's TestPlatformSecretsAreReservedEverywhere,
// the only place that can see both this set and the daemon's own platform-key
// constants (a `package main` cannot be imported, so the test cannot live here).
func ReservedPlatformSecret(name string) bool { return reservedSecret(name) }

// sinkReservedSecret is the reserved-name guard at the credential SINKS — the
// api_key injection resolver (handleInternalInjection), the git_pat/ssh_key
// broker mints, and the policy write-time checks that mirror them. It is
// reservedSecret() PLUS the three RESIDENT AWS SigV4 credential names that
// resolveBedrockAuth reads DIRECTLY from the store to sign Bedrock requests
// (aws-access-key-id / aws-secret-access-key / aws-session-token). Those never
// flow through a grant on the legitimate Bedrock path, so an api_key/git_pat/
// ssh_key grant naming one is only ever an attempt to exfiltrate the operator's
// long-lived AWS secret key to an allowlisted host (as a Bearer header or git
// password) — reject it at every sink. bedrock-api-key is deliberately EXCLUDED:
// the Bedrock BEARER path authors a host-pinned api_key grant that legitimately
// resolves it through the injection sink (see runs.go), so reserving it here
// would break that path.
//
// This is NOT the guard for the broker's git_pat/ssh_key mint paths
// (mintGitPAT/mintSSHKey). Those return a secret's raw VALUE into the sandbox
// (unlike api_key, whose value never leaves the broker), so they need a
// STRICTLY WIDER guard — internal/broker.reservedBrokerSecretNames — that also
// refuses github-app-key/github-app-id and bedrock-api-key (W12-B-1). Those two
// pairs are operator-PROVIDED credentials the generic secrets API must stay able
// to Put, which is exactly why they are sealed on the broker side only. The
// broker cannot import this package, so the two lists are related but
// deliberately not identical; do not "fix" that by widening sinkReservedSecret
// itself — the api_key path is fine with the narrower set. What the two sets
// MUST agree on is the daemon-GENERATED platform keys (cmd/wardynd's
// loadOrCreateSecret names, wardyn-ssh-host-key among them): nobody authors
// those, so neither guard has a reason to let one through — see
// TestPlatformSecretsAreReservedEverywhere (cmd/wardynd).
func sinkReservedSecret(name string) bool {
	return reservedSecret(name) ||
		name == bedrockAccessKeyIDSecret ||
		name == bedrockSecretAccessKeySecret ||
		name == bedrockSessionTokenSecret
}

// secretsAPIReserved is the reserved-name guard for the GENERIC secrets API
// (Put/Delete/List). It is reservedSecret() PLUS the two Anthropic OAuth
// injection SENTINELS (types.SubscriptionOAuthSecret / types.ManagedOAuthSecret).
// A sentinel is name-privileged — an api_key grant carrying it resolves at the
// injection sink to a LIVE OAuth token via oauthProviderForSentinel, IGNORING any
// stored value — so letting an operator Put a value under that name (silently
// shadowed) or listing it is confusing at best and hides that the name is
// credential-privileged at worst. Reserved from the generic API ONLY, never at
// the sinks: a subscription/managed policy legitimately names the sentinel in an
// api_key grant, which validateInlineSecretRefs and the injection sink allow via
// the provider switch (oauthProviderForSentinel), which runs AFTER this guard.
func secretsAPIReserved(name string) bool {
	return reservedSecret(name) || name == types.SubscriptionOAuthSecret || name == types.ManagedOAuthSecret
}

type putSecretRequest struct {
	Value string `json:"value"`
}

// writableSecretName validates a secret name is well-formed and not a
// reserved platform-internal key, writing the appropriate 400/403 and
// returning ok=false on failure. Shared by Put and Delete.
func (s *Server) writableSecretName(w http.ResponseWriter, name, owner string) bool {
	if !secretNameRE.MatchString(name) {
		writeError(w, http.StatusBadRequest, "invalid secret name (lowercase alphanumerics, '.', '_', '-')")
		return false
	}
	if secretsAPIReserved(name) {
		writeError(w, http.StatusForbidden, "secret name is reserved for platform internals")
		return false
	}
	// The Bedrock/SigV4 credential material is ALWAYS resolved from the
	// operator namespace (runs_bedrock.go's setupBedrock reads present[...]
	// off For("")) — a member row under one of these four names would read as
	// "Bedrock is configured" in setup while dispatch never actually uses it,
	// a confusing dead end rather than a working BYOK path. sinkReservedSecret
	// deliberately excludes bedrock-api-key (the operator's legitimate write
	// path); that exclusion does not extend to a non-operator namespace. Shared
	// by Put and Delete so both paths carry it.
	if owner != "" && (sinkReservedSecret(name) || name == bedrockAPIKeySecret) {
		writeError(w, http.StatusForbidden, "secret name is reserved for platform internals")
		return false
	}
	return true
}

// handlePutSecret stores (or overwrites) a named secret in the caller's own
// namespace (0.7, migration 0050: "" for an operator, else their own
// principal — see secretOwnerFromRequest). The value is write-only: no API
// path ever returns it. Every write is an audit event.
func (s *Server) handlePutSecret(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	// ?owner= is honoured here exactly as on DELETE/GET: an admin's cross-write
	// lands in the NAMED member's namespace. Silently ignoring it would put the
	// value in the operator namespace — the Get fallback for EVERY member's
	// runs — which is the one blast radius a per-principal write must not have.
	owner, ok := s.secretOwnerParam(w, r)
	if !ok {
		return
	}
	if !s.writableSecretName(w, name, owner) {
		return
	}
	var body putSecretRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxJSONBody)).Decode(&body); err != nil || body.Value == "" {
		writeError(w, http.StatusBadRequest, `body must be {"value":"<non-empty secret>"}`)
		return
	}
	// Fail-CLOSED: reject a secret the masking/scanning layers would SILENTLY drop
	// (secretmask.Add/NewMasker and contentscan.filterCorpus ignore values shorter
	// than secretmask.MinLen). Accepting it would falsely imply it gets masked and
	// scanned; the operator must learn immediately instead. Reserved system keys
	// (signing/session) never reach here — they are set internally, not via this
	// user-facing Put, and are already rejected above. secretGitHubAppID is the
	// one EXEMPTION: a numeric GitHub App ID is a public identifier, not maskable
	// credential material, so masking it would be meaningless and refusing it
	// would break GitHub App setup via the wizard/CLI.
	if name != secretGitHubAppID && len(body.Value) < secretmask.MinLen {
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("secret too short: must be at least %d bytes to be masked and scanned", secretmask.MinLen))
		return
	}
	if !s.admitSecretCount(w, r, owner, name) {
		return
	}
	if err := s.cfg.Secrets.For(owner).Put(r.Context(), name, []byte(body.Value)); err != nil {
		writeError(w, http.StatusInternalServerError, "store secret: "+err.Error())
		return
	}
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		"secret.write", name, "success", secretOwnerAuditData(owner)))
	w.WriteHeader(http.StatusNoContent)
}

// admitSecretCount is the secretsMaxPerOwner check, sited immediately before
// the Put so nothing between them can change the count.
//
// AN OVERWRITE IS NEVER REFUSED, and that is the whole reason this is a
// function rather than a `len(names) >= max` line. `Put` is both "add" and
// "rotate": an operator sitting exactly at the cap must still be able to
// replace an expiring key, and a cap that refused that would turn a soft guard
// into an outage on the one day it matters most. Only a NEW name is capped.
//
// A List failure is fail-OPEN (admit): this bounds accidental growth, and
// refusing every write because the count could not be taken would make a
// transient store hiccup look like a permission problem. The Put below reports
// a real store failure on its own.
func (s *Server) admitSecretCount(w http.ResponseWriter, r *http.Request, owner, name string) bool {
	names, err := s.cfg.Secrets.For(owner).List(r.Context())
	if err != nil || len(names) < secretsMaxPerOwner || slices.Contains(names, name) {
		return true
	}
	writeError(w, http.StatusUnprocessableEntity,
		fmt.Sprintf("too many stored secrets (max %d) — delete one first", secretsMaxPerOwner))
	return false
}

// handleDeleteSecret removes a named secret from the caller's own namespace,
// or (admin-only) another principal's via ?owner=. Idempotent and scoped:
// another member's row is structurally unreachable (secretOwnerParam refuses
// ?owner= for a non-operator, and For(owner) never resolves a different
// owner's row), so deleting one 204s exactly like deleting a never-set name —
// no existence oracle. Audited.
func (s *Server) handleDeleteSecret(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	owner, ok := s.secretOwnerParam(w, r)
	if !ok {
		return
	}
	if !s.writableSecretName(w, name, owner) {
		return
	}
	if err := s.cfg.Secrets.For(owner).Delete(r.Context(), name); err != nil {
		writeError(w, http.StatusInternalServerError, "delete secret: "+err.Error())
		return
	}
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		"secret.delete", name, "success", secretOwnerAuditData(owner)))
	w.WriteHeader(http.StatusNoContent)
}

// secretOwnerParam resolves the secret-store namespace PUT, DELETE and the
// LIST endpoint read/write: the caller's own (secretOwnerFromRequest) by
// default, or ?owner=<principal> when the caller is an operator asking about
// a specific member's rows. A non-operator naming ?owner= at all gets a
// CONSTANT 403 — refused before any lookup, so it never varies with whether
// the named principal exists — the same posture handleReassignWorkspace's
// admin-only gate uses for the analogous workspace-ownership query.
func (s *Server) secretOwnerParam(w http.ResponseWriter, r *http.Request) (owner string, ok bool) {
	q := r.URL.Query().Get("owner")
	if q == "" {
		return s.secretOwnerFromRequest(r), true
	}
	if !s.isOperator(r.Context()) {
		writeError(w, http.StatusForbidden, "?owner= is admin-only")
		return "", false
	}
	return q, true
}

// secretOwnerAuditData is the secret.write/secret.delete audit Data: nil for
// an operator-namespace write (byte-identical to pre-0050), or
// {"secret_owner": owner} otherwise — a member's own PUT/DELETE and an
// admin's ?owner= cross-write alike. Unlike workspace_owner.go's
// auditWorkspaceDataFor (which stamps only when the actor and the owner
// differ), this stamps on EVERY non-"" owner: which namespace a secret write
// landed in is the whole point of the marker, including a member's own
// ordinary write.
func secretOwnerAuditData(owner string) json.RawMessage {
	if owner == "" {
		return nil
	}
	return mustJSON(map[string]any{"secret_owner": owner})
}

// handleListSecrets returns {"names": [...], "mine": [...]} — never values.
// Reserved platform-internal keys (reservedSecretNames: wardyn-signing-key,
// wardyn-session-key) are EXCLUDED from both: they back identity/session
// handling and are not user-managed, so surfacing their names is an
// unnecessary leak (and they are already non-writable/non-deletable via the
// API).
//
// `mine` is always the queried namespace's own rows (the caller's, or one
// member's via admin ?owner=) — operator ⇒ mine == names.
//
// `names` keeps its PRE-0.7 meaning for an admin (this endpoint's three
// existing UI callers are unchanged): the operator namespace, or one
// member's own rows with ?owner=. For a MEMBER it narrows to the
// operator-owned names an eligible grant in the operator's ceiling actually
// PAIRS with (memberVisibleOperatorSecretNames), closing a name-enumeration
// gap the flat pre-0.7 namespace had — capSeamAllowed(capSecret, n) alone
// passed everything through whenever that capability was unenforced (the
// default), so a member could list every operator secret's name regardless
// of any grant. Narrowed HERE and not in listUserSecretNames, which also
// feeds the setup checklist and presentSecretNames: those compute whether the
// DEPLOYMENT is provisioned, and a member's own grants must not make an
// operator's secret read as missing.
func (s *Server) handleListSecrets(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	owner, ok := s.secretOwnerParam(w, r)
	if !ok {
		return
	}
	mine, err := reservedFilteredSecretNames(ctx, s.cfg.Secrets.For(owner))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list secrets: "+err.Error())
		return
	}
	names := mine
	if !s.isOperator(ctx) {
		names, err = s.memberVisibleOperatorSecretNames(ctx)
		if err != nil {
			// ceilingErrorStatus so an unanswerable group snapshot is the same
			// 403 every other routed site gives, not a 500 that reads as an
			// outage; a plain store failure still 500s.
			writeError(w, ceilingErrorStatus(err), "list secrets: "+err.Error())
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"names": names, "mine": mine})
}

// memberVisibleOperatorSecretNames is handleListSecrets' member-facing
// `names`: the reserved-filtered OPERATOR secret names an eligible grant in
// THIS CALLER'S ceiling actually pairs
// with a host — storedSecretGrantPairing is the same extraction
// filterMemberGrants uses to decide whether a MEMBER's own inline grant is
// eligible-listed — narrowed further by the existing capSeamAllowed(capSecret,
// …) gate once an operator enforces it. Ceiling-pairing is unconditional
// (closes the name-enumeration gap regardless of enforcement); the capability
// gate on top only ever narrows more.
//
// The caller's ceiling, not Config.DefaultPolicy's (effectiveCeiling): this
// list is precisely "which operator secrets may I ask for", and a governance
// profile that narrows a member's eligible grants has to narrow the menu with
// it — otherwise the console offers names their own run would then drop, which
// reads as a bug and teaches members to ignore the list. It uses the SAME
// grant list filterMemberGrants enforces, so what is shown and what is
// accepted cannot drift.
func (s *Server) memberVisibleOperatorSecretNames(ctx context.Context) ([]string, error) {
	ceiling, cerr := s.effectiveCeiling(ctx)
	if cerr != nil {
		return nil, cerr
	}
	all, err := reservedFilteredSecretNames(ctx, s.cfg.Secrets.For(""))
	if err != nil {
		return nil, err
	}
	paired := map[string]bool{}
	for _, g := range ceiling.Spec.EligibleGrants {
		_, secretRef, knownHostsRef, covered, derr := storedSecretGrantPairing(g)
		if !covered || derr != nil {
			continue
		}
		if secretRef != "" {
			paired[secretRef] = true
		}
		if knownHostsRef != "" {
			paired[knownHostsRef] = true
		}
	}
	// The same batch narrowMemberInlinePolicy uses. N here is OPERATOR-controlled
	// (the deployment's stored secret names, intersected with the ceiling's
	// paired grants) rather than caller-controlled, so this was hygiene and not
	// the availability defect capBatch was written for — but it is the identical
	// "2N round trips over a list" shape, and leaving one copy standing is how
	// the next reader concludes the pattern is fine.
	cap := s.newCapBatch(ctx)
	kept := all[:0:0]
	for _, n := range all {
		if !paired[n] {
			continue
		}
		ok, cerr := cap.allowed(ctx, capSecret, n)
		if cerr != nil {
			return nil, cerr
		}
		if ok {
			kept = append(kept, n)
		}
	}
	return kept, nil
}

// listUserSecretNames returns the OPERATOR namespace's present secret NAMES
// (never values) with the reserved platform-internal keys (wardyn-signing-key,
// wardyn-session-key) excluded. Shared by handleSetupStatus and
// presentSecretNames — both compute whether the DEPLOYMENT is provisioned, an
// operator-wide question, so this deliberately stays on For("") rather than
// taking a caller's own namespace (handleListSecrets' `mine` is that read).
func (s *Server) listUserSecretNames(ctx context.Context) ([]string, error) {
	return reservedFilteredSecretNames(ctx, s.cfg.Secrets.For(""))
}

// reservedFilteredSecretNames lists store's names with the platform-reserved
// keys excluded — the one loop listUserSecretNames (operator namespace) and
// handleListSecrets' `mine` (any namespace) both need.
func reservedFilteredSecretNames(ctx context.Context, store secretstore.Store) ([]string, error) {
	all, err := store.List(ctx)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(all))
	for _, n := range all {
		if secretsAPIReserved(n) {
			continue
		}
		names = append(names, n)
	}
	return names, nil
}

// presentSecretNames is listUserSecretNames as a name→present lookup: the ONE
// map every secret-aware verdict is computed from (compose, preflight, the
// record lane's api-key fallback), so the checklist's present/missing verdicts
// can never disagree with the launch-time secret gate. Best-effort by design —
// no secret store or a list error yields an empty (never nil) map, i.e. "no
// secrets present", which fails those verdicts CLOSED. Operator namespace
// only, via listUserSecretNames — these are deployment-provisioning verdicts,
// not a per-caller view.
func (s *Server) presentSecretNames(ctx context.Context) map[string]bool {
	present := map[string]bool{}
	if s.cfg.Secrets == nil {
		return present
	}
	names, err := s.listUserSecretNames(ctx)
	if err != nil {
		return present
	}
	for _, n := range names {
		present[n] = true
	}
	return present
}

// presentSecretNamesFor widens presentSecretNames to a caller's own namespace:
// the operator names UNION owner's own reserved-filtered names (For(owner).List
// — own rows only, never another member's). owner == "" collapses to exactly
// presentSecretNames (the byte-identical-for-operators case). This is what lets
// a member's own anthropic-api-key synthesise their anthropic_api_key row
// (effectiveIntegrations) and satisfy a model-access verdict with no operator
// row at all.
func (s *Server) presentSecretNamesFor(ctx context.Context, owner string) map[string]bool {
	present := s.presentSecretNames(ctx)
	if owner == "" || s.cfg.Secrets == nil {
		return present
	}
	names, err := reservedFilteredSecretNames(ctx, s.cfg.Secrets.For(owner))
	if err != nil {
		return present
	}
	for _, n := range names {
		present[n] = true
	}
	return present
}

// ownsSecret reports whether owner holds their OWN secret named name — a
// names-only For(owner).List (own rows only, never the operator fallback Get
// would give), so a member's ownership claim is provable, never merely
// unrefuted. owner == "" (an operator) never "owns" anything by this path —
// operator material is governed by the ceiling, not ownership.
//
// ponytail: one List call per invocation, no per-request cache — a member's
// inline policy carries a handful of grants at most, so this never runs in a
// loop large enough to matter; add a cache if a caller ever iterates hundreds.
func (s *Server) ownsSecret(ctx context.Context, owner, name string) bool {
	if owner == "" || name == "" || s.cfg.Secrets == nil {
		return false
	}
	names, err := s.cfg.Secrets.For(owner).List(ctx)
	if err != nil {
		return false
	}
	return slices.Contains(names, name)
}

// formatInjectionValue applies an injection rule's Format ("%s"-style) to the
// secret. An empty format means the raw secret value.
func formatInjectionValue(format string, secret []byte) string {
	if format == "" {
		return string(secret)
	}
	return fmt.Sprintf(format, string(secret))
}
