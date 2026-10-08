// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// DRAFT (M2 canon pending)
const (
	// inlineSecretStoreMissingRefusal is the 422 for a spec whose grants need a
	// stored secret on a deployment with no secret store wired. Reachable from
	// four doors (create, preflight, policy create, policy update).
	//
	// Article-free on purpose: the grant KIND is interpolated, and "a %s" reads
	// as "a api_key". The kind is the first grant that needs a store, which is
	// the one the author has to look at first.
	inlineSecretStoreMissingRefusal = "the %s grant requires a secret store, but none is configured"
)

// validateInlineSecretRefs fails a policy spec closed when any of its api_key
// OR git_pat eligible grants names a secret that does not actually exist, is a
// reserved platform-internal key, or when no secret store is configured at all.
// Despite the name (kept for the inline call site it was written for — the
// stored/default branch now also calls it, same check either way) it takes
// a plain types.RunPolicySpec, not anything inline-specific.
// Names come from Secrets.List; create/preflight also grade the ADO own-token
// credential, while policy preview deliberately defers that readiness. The
// returned status code is 422 (Unprocessable Entity) for every failure so the
// create call fails closed: an inline policy whose grant references a
// missing/forbidden secret would otherwise brick the run (a proxy-injection
// api_key at startup, or a git_pat clone at task time).
//
// Other grant kinds are skipped here — the structural validity of a grant scope
// is the job of validatePolicySpec / the broker; this check is solely about
// secret existence.
// filterUserGrants drops a MEMBER's inline stored-secret grant (api_key /
// git_pat / ssh_key) whose {host, secret} pairing the operator did not
// eligible-list. composer.Clamp bounds egress, confinement, TTL and grant
// KINDS, but the SCOPE of a non-github grant — which stored credential is
// injected on which host — is member-authored and left unclamped; without this
// a member could pair ANY non-reserved stored secret with ANY allowlisted host
// (a secret-exfil primitive, amplified by scan-seeded egress). DROPPED, not
// rejected: a run's legitimate model-access grant is re-added admin-side at
// launch (handleCreateRun) AFTER this returns — the run's model provider, or
// applyWorkspaceRequirements folding a workspace requirement's secret — so a
// member's own copy of one is redundant, and dropping an unmatched grant
// removes a genuine exfil pairing while the real grant still arrives. (A member
// run whose model access relies on a raw operator secret with NO integration
// and NO workspace requirement gets no grant re-added — fail-closed, no exfil;
// operators provision member model access via an integration.) Rejecting
// instead would 403 the composer's own model-access grant under a wildcard
// api_key ceiling, and an integration key's name is not the provider convention
// so it cannot be exempt-matched at this layer anyway. Called for MEMBERS only —
// an operator is the ceiling authority and stays unclamped. A sentinel LLM
// api_key grant is kept: it references no operator stored secret and
// validateInlineSecretRefs host-pins it to the provider. An UNDECODABLE scope —
// or, since the pairing switch closed, an UNKNOWN KIND — is a malformed request
// → error (fail closed), never a silent drop. env_secret is admin-only and is
// dropped for a member regardless of the ceiling; see the block below.
//
// The eligible-grant list it compares against is the CALLER's ceiling
// (effectiveCeiling), not Config.DefaultPolicy: a governance profile narrows
// which credential pairings its members may reuse, and reading the deployment
// list here would have left that narrowing unenforced at the one seam where a
// pairing actually becomes an injected credential. Resolved INSIDE rather than
// threaded in from resolveRunPolicy — ponytail: it buys a signature no caller,
// present or future, can pass the wrong ceiling to. effectiveCeiling memoizes
// per request (governance.go), so asking here is free AND cannot disagree with
// the caller's own clamp.
func (s *Server) filterUserGrants(ctx context.Context, owner string, allowedDomains []string, grants []types.GrantSpec) (kept []types.GrantSpec, warns []string, code int, err error) {
	resolved, cerr := s.effectiveCeiling(ctx)
	if cerr != nil {
		// Fail CLOSED, and never by silently substituting the deployment list:
		// an unknown ceiling cannot decide whether a pairing is eligible, and
		// guessing here is the one direction that leaks a credential.
		return nil, nil, ceilingErrorStatus(cerr), cerr
	}
	ceiling := resolved.Spec.EligibleGrants
	for _, g := range grants {
		host, secretRef, _, covered, derr := storedSecretGrantPairing(g)
		if !covered {
			kept = append(kept, g) // github_token (scope-intersected by the clamp), cloud_sts, …
			continue
		}
		if derr != nil {
			return nil, nil, http.StatusUnprocessableEntity, fmt.Errorf("%s grant scope invalid: %w", g.Kind, derr)
		}
		// bedrock-api-key is never member-authored, whoever owns the row — ahead
		// of the own-key arm, which would otherwise admit it to any model-provider
		// host under any header. Its one author is dispatch, whose grant records
		// the namespace the key is read from (resolveBedrockBearerInjection); a
		// member's own key reaches their runs that way under a per_user row.
		if secretRef == bedrockAPIKeySecret {
			warns = append(warns, fmt.Sprintf(
				"dropped %s grant naming secret %q: the Bedrock API key reaches a run only through the grant Wardyn authors at launch",
				g.Kind, secretRef))
			continue
		}
		if g.Kind == types.GrantAPIKey {
			// 6c own-key arm: a member's OWN api_key secret, paired with a
			// model-provider host (isModelProviderHost — the gateway counts
			// too) that the run's own already-clamped egress allows, is
			// admitted with NO operator eligible-grant pairing at all — this
			// is what lets a member's own model key be used with no hand-
			// authored inline grant naming an operator secret. host must be
			// an EXACT allowedDomains entry (the load-bearing half — Clamp
			// has already intersected egress to the ceiling; the suffix
			// match alone would admit evilanthropic.com), and ownership is
			// proved by a names-only For(owner).List, never a value read.
			if s.isModelProviderHost(host) && domainAllowedExact(allowedDomains, host) && s.ownsSecretMemoized(ctx, owner, secretRef) {
				kept = append(kept, g)
				continue
			}
		}
		// env_secret is ADMIN-ONLY by default, ahead of the pairing check — a
		// member does not get one even for a pairing the operator DID list.
		// Defence in depth only: resolveRunPolicy already ran
		// dropAdminOnlyEnvSecretGrants over the same spec on EVERY member path,
		// so by the time a grant reaches here there is nothing left for this arm
		// to drop. It stays because filterUserGrants is the grant gate and a
		// gate that trusts its caller to have already applied half its rule is
		// one refactor away from applying none of it.
		if residentSecretKind(g.Kind) && userEnvSecretIsAdminOnly() {
			warns = append(warns, envSecretAdminOnlyWarning(g.Kind, secretRef))
			continue
		}
		if !storedSecretPairingInCeiling(g, ceiling) {
			warns = append(warns, fmt.Sprintf(
				"dropped %s grant pairing secret %q with host %q: not in the operator's eligible grants (the run's own model access is provisioned by the platform)",
				g.Kind, secretRef, host))
			continue
		}
		kept = append(kept, g)
	}
	return kept, warns, 0, nil
}

// storedSecretGrantPairing returns the (host, secretRef, knownHostsRef) a
// stored-secret grant pairs and whether the kind is one enforceMemberGrantScope
// covers. knownHostsRef is populated only for ssh_key (empty — and compared as
// such — for every other kind; see storedSecretPairingInCeiling). An undecodable
// scope returns covered=true WITH the error (fail closed — an unreadable
// stored-secret grant is rejected, never skipped).
//
// The switch is CLOSED — every types.GrantKind is named, and the default arm
// REFUSES rather than falling through: a grant kind added to types.GrantKind
// and wired to a stored secret — env_secret is exactly that — must be named
// here before it can be member-authorable and clamped/checked against
// capSecret. The kind set is small and closed; a compiler-visible list plus a
// refusing default makes forgetting fail shut instead of open.
// (TestStoredSecretGrantPairing_UnknownKindIsRefused is the regression.)
func storedSecretGrantPairing(g types.GrantSpec) (host, secretRef, knownHostsRef string, covered bool, err error) {
	switch g.Kind {
	case types.GrantAPIKey:
		r, e := injectionRuleFromScope(g.Scope)
		return r.Host, r.SecretName, "", true, e
	case types.GrantGitPAT:
		sc, e := types.DecodeGitPATScope(g.Scope)
		return sc.Host, sc.SecretName, "", true, e
	case types.GrantSSHKey:
		h, kr, _, khr, e := sshKeyScopeFields(g.Scope)
		return h, kr, khr, true, e
	case types.GrantEnvSecret:
		// An env_secret has no host — it is delivered TO the sandbox, not to a
		// destination — so the env var NAME takes the host slot. That makes the
		// ceiling comparison an exact (name, secret) pairing, the same shape
		// git_pat gets, rather than letting a member reuse an operator-blessed
		// secret under a variable name the operator never wrote. hostEqual's
		// lowercasing is harmless here: envSecretScopeFields admits upper case
		// only, so two names that compare equal ARE equal.
		n, sn, e := envSecretScopeFields(g.Scope)
		return n, sn, "", true, e
	case types.GrantFileSecret:
		// The FILE name takes the host slot, for env_secret's reason: the
		// ceiling comparison is then an exact (file, secret) pairing.
		f, sn, e := fileSecretScopeFields(g.Scope)
		return f, sn, "", true, e
	case types.GrantGitHubToken, types.GrantCloudSTS:
		// Genuinely name no stored secret: github_token mints an App
		// installation token (scope-intersected by composer.Clamp), cloud_sts is
		// hard-refused by the embedded IdP. Not covered, and safe to keep.
		return "", "", "", false, nil
	default:
		return "", "", "", true, fmt.Errorf("unknown grant kind %q", g.Kind)
	}
}

// storedSecretPairingInCeiling reports whether some operator eligible-grant of
// the same kind pairs the SAME host with the SAME secret (host case-insensitive)
// AND, for ssh_key, the SAME known_hosts_secret_ref (empty included), AND, for
// api_key, the SAME header and format (header case-insensitive, both defaulted
// the way injectionRuleFromScope defaults them) — an exact-pairing match, so a
// member may only reuse a pairing the operator explicitly listed, never invent
// one.
//
// header/format joined the match too: without them a member grant that kept
// the operator's blessed (host, secret) pairing but moved the secret under an
// arbitrary header matched the ceiling and was kept, and the proxy writes that
// header verbatim onto the forwarded request while relaying the upstream
// response to the sandbox verbatim — so an upstream that echoes the offending
// header hands the operator's key back to the sandbox.
//
// known_hosts_secret_ref is part of this match: without it, a member could
// pair an operator-approved (host, key_secret_ref) pairing with ANY
// known_hosts_secret_ref of their own choosing — including one naming a stored
// secret with no relation to SSH host keys — and mintSSHKey (broker.go) would
// return that secret's raw value as Minted.KnownHosts, an rbac-bypass escaping
// this gate entirely. Requiring an exact match (including the common
// empty==empty case, where the operator named no known_hosts_secret_ref at all)
// closes that: a member can only obtain known_hosts material the operator's OWN
// ceiling grant already named for that exact pairing.
//
// An operator ceiling grant with a wildcard (empty/undecodable) scope carries no
// pairing and matches nothing here, so it authorizes the kind for the
// composer's own (sentinel, host-pinned) grants without empowering a member to
// pick the secret and host.
//
// FORWARDS to composer.PairingInCeiling, which is THE comparator: two
// implementations of one rule drift; one implementation with two callers
// cannot. The pairing DECODE here (storedSecretGrantPairing) stays, because
// it answers a different question: whether a grant is well-formed enough to
// DELIVER, which fails a malformed scope closed rather than merely declining
// to match it.
func storedSecretPairingInCeiling(g types.GrantSpec, ceiling []types.GrantSpec) bool {
	return composer.PairingInCeiling(g, ceiling)
}

// neededSecret is one secret name a spec references AND the grant kind that
// references it. The kind is the whole point: without it, a refusal on this
// path cannot say which grant is the problem — git_pat and ssh_key grants
// would be named "api_key" too. Reachable from four doors.
// ownerOnlyMissingRefusal names the remedy: the row is stored by that person,
// signed in as themselves; an admin's own writes land in the operator
// namespace, so an admin stores theirs from the user view.
const ownerOnlyMissingRefusal = "%s grant for secret %q is owner_only, and the run's owner has no secret of that name of their own " +
	"(an operator secret of that name is never used for it). Store it via the secrets API signed in as that person; " +
	"an admin stores their own from the user view"

type neededSecret struct {
	name      string
	kind      types.GrantKind
	ownerOnly bool
	host      string // a git_pat grant's host, for the Azure DevOps owner_only rule
}

// secretRefsOf is validateInlineSecretRefs' shape half: the secret names a
// spec's api_key, git_pat and ssh_key grants reference, refusing a scope that
// does not decode, a reserved name, or a misdirected LLM-auth sentinel. It
// reads no store. A stored policy is checked with this alone: its grants
// resolve in each run owner's namespace, and run-create checks that (#1123).
func (s *Server) secretRefsOf(spec types.RunPolicySpec) ([]neededSecret, error) {
	// Collect the secret names referenced by api_key, git_pat AND ssh_key
	// grants (all three resolve a stored secret by name — api_key proxy-side,
	// git_pat via the git helper, ssh_key as the resident key + optional
	// known_hosts), each carrying the kind that referenced it.
	var needed []neededSecret
	for _, g := range spec.EligibleGrants {
		switch g.Kind {
		case types.GrantAPIKey:
			rule, derr := injectionRuleFromScope(g.Scope)
			if derr != nil {
				// An undecodable api_key scope cannot reference a resolvable secret;
				// fail closed rather than silently skipping it.
				return nil, fmt.Errorf("api_key grant scope invalid: %w", derr)
			}
			if sinkReservedSecret(rule.SecretName) {
				return nil, fmt.Errorf("api_key grant references reserved secret name %q", rule.SecretName)
			}
			needed = append(needed, neededSecret{rule.SecretName, types.GrantAPIKey, g.OwnerOnly, ""})
		case types.GrantGitPAT:
			pat, derr := types.DecodeGitPATScope(g.Scope)
			if derr != nil {
				return nil, fmt.Errorf("git_pat grant scope invalid: %w", derr)
			}
			patHost, secretName := pat.Host, pat.SecretName
			// nameSinkReservedSecret, not sinkReservedSecret (#1048): this kind
			// returns the raw value into the sandbox, so it needs the wider guard
			// that also refuses a wardyn-provider-*-key name. api_key above stays
			// on the narrower guard — the provider arm legitimately names a -key.
			if nameSinkReservedSecret(secretName) {
				return nil, fmt.Errorf("git_pat grant references reserved secret name %q", secretName)
			}
			needed = append(needed, neededSecret{secretName, types.GrantGitPAT, g.OwnerOnly, patHost})
		case types.GrantSSHKey:
			_, keyRef, _, khRef, derr := sshKeyScopeFields(g.Scope)
			if derr != nil {
				return nil, fmt.Errorf("ssh_key grant scope invalid: %w", derr)
			}
			// nameSinkReservedSecret, not sinkReservedSecret (#1048) — same
			// reasoning as git_pat above.
			if nameSinkReservedSecret(keyRef) || nameSinkReservedSecret(khRef) {
				return nil, errors.New("ssh_key grant references a reserved secret name")
			}
			needed = append(needed, neededSecret{keyRef, types.GrantSSHKey, g.OwnerOnly, ""})
			if khRef != "" {
				needed = append(needed, neededSecret{khRef, types.GrantSSHKey, g.OwnerOnly, ""})
			}
		case types.GrantFileSecret:
			// A resident value, so the wider guard, as for git_pat. Collected,
			// unlike env_secret, so a missing secret is a 422 at create rather
			// than a file that silently never arrives.
			_, secretName, derr := fileSecretScopeFields(g.Scope)
			if derr != nil {
				return nil, fmt.Errorf("file_secret grant scope invalid: %w", derr)
			}
			if nameSinkReservedSecret(secretName) {
				return nil, fmt.Errorf("file_secret grant references reserved secret name %q", secretName)
			}
			needed = append(needed, neededSecret{secretName, types.GrantFileSecret, g.OwnerOnly, ""})
		default:
			continue
		}
	}
	return needed, nil
}

// validateInlineSecretRefs is secretRefsOf plus existence, for a spec about to
// run: owner is the caller's secret namespace (secretOwnerFromRequest) and
// subject the run identity's (runIdentitySubject), the namespace an owner_only
// grant is read from at mint. With no reference there is nothing to check and
// no secret store is required.
func (s *Server) validateInlineSecretRefs(ctx context.Context, owner, subject string, spec types.RunPolicySpec) (int, error) {
	return s.validateRunSecretRefs(ctx, owner, subject, spec, true)
}

func (s *Server) validateRunSecretRefs(ctx context.Context, owner, subject string, spec types.RunPolicySpec, credentials bool) (int, error) {
	needed, err := s.secretRefsOf(spec)
	if err != nil {
		return http.StatusUnprocessableEntity, err
	}
	if len(needed) == 0 {
		return 0, nil
	}

	// At least one grant needs a secret: a secret store MUST be wired or the
	// injection can never resolve (fail closed). The message names the KIND that
	// actually needs it, not whichever kind this check was written for first.
	if s.cfg.Secrets == nil {
		return http.StatusUnprocessableEntity, fmt.Errorf(inlineSecretStoreMissingRefusal, needed[0].kind)
	}

	// Names only — never Get a value here.
	have, err := s.cfg.Secrets.List(ctx)
	if err != nil {
		return http.StatusUnprocessableEntity, fmt.Errorf("list secrets: %w", err)
	}
	known := make(map[string]bool, len(have))
	for _, n := range have {
		known[n] = true
	}
	// A git token for an Azure DevOps host is owner_only whatever the policy
	// said (persistRunGrants forces it, #1429), so it is checked as one: a
	// person with no token of their own is refused here, with the reason,
	// rather than started into a clone that fails inside the sandbox (adoGitGrants
	// drops the grant where the row takes each person's own token instead).
	needed, code, err := s.adoGitGrants(ctx, subject, needed, spec, credentials)
	if err != nil {
		return code, err
	}
	for _, n := range needed {
		// A person's owner_only grant never reads the operator namespace (#1106):
		// only the row the mint reads. An operator-owned run's own row is the
		// operator's, which the check below finds.
		if n.ownerOnly && !operatorOwnedRequest(ctx) {
			if !s.ownsSecretMemoized(ctx, subject, n.name) {
				return http.StatusUnprocessableEntity, fmt.Errorf(ownerOnlyMissingRefusal, n.kind, n.name)
			}
			continue
		}
		// A name in owner's OWN namespace (For(owner).List — own rows only) is
		// accepted too — this is what lets a member's inline_policy name their
		// own model key with no operator row of that name at all (6c). Never
		// widens: an operator-only name still 422s below.
		if !known[n.name] && !s.ownsSecretMemoized(ctx, owner, n.name) {
			return http.StatusUnprocessableEntity, fmt.Errorf(
				"%s grant references unknown secret %q (set it first via the secrets API)", n.kind, n.name)
		}
	}
	return 0, nil
}
