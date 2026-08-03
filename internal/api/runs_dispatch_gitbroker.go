// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"log/slog"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The dispatch-time half of "a brokered forge is single-lane": which forges the
// git broker serves, which egress entries belong to them, and what a brokered run
// therefore loses — the managed host names and the forge's SSH endpoint
// (confineGitBrokerEgress), plus the ssh_key / git_pat CREDENTIALS themselves
// (dropBrokeredGrants), audited so neither withholding is silent. Split out of
// runs_dispatch.go for the file-size gate; behaviour is unchanged and the
// policy-write half still lives in policy.go (validateGrantLaneExclusivity),
// reading the SAME gitBrokerForges list.

// gitBrokerForges are the forges Option C's git-broker serves. Distinct from
// gitBrokerManagedHosts on purpose: that list is the set of host NAMES the
// /wardyn/gh/ route actually re-originates to, and ssh.github.com is not one of
// them — the broker speaks smart-HTTP, never SSH. Keep it meaning exactly that.
// (Both lists now feed promoteSkipHosts, so that consumer no longer distinguishes
// them; the route it names is what does.) This list is the input to
// gitBrokerSSHEndpoints, and to the policy-write refusal
// (validateGrantLaneExclusivity, policy.go), so both halves of the single-lane
// rule read from ONE place. The broker is github.com-only in v1.
var gitBrokerForges = []string{"github.com"}

// gitBrokerSSHEndpoints returns the SSH-over-443 endpoints of the brokered forges
// ("ssh.github.com:443"), via sshOver443Endpoint so the forge→endpoint mapping
// lives in exactly one place.
func gitBrokerSSHEndpoints() []string {
	out := make([]string, 0, len(gitBrokerForges))
	for _, f := range gitBrokerForges {
		if ep, ok := sshOver443Endpoint(f); ok {
			out = append(out, ep)
		}
	}
	return out
}

// gitBrokerSSHHosts is gitBrokerSSHEndpoints with the ":443" stripped
// ("ssh.github.com"). Every consumer wants the BARE host — the deny must cover
// every port (confineGitBrokerEgress), and the promotion skip map keys on a bare
// host (promoteSkipHosts, record.go) — so strip it once, here.
func gitBrokerSSHHosts() []string {
	out := gitBrokerSSHEndpoints()
	for i, ep := range out {
		out[i] = egressEntryHost(ep)
	}
	return out
}

// confineGitBrokerEgress makes the git-broker route the ONLY route to a brokered
// forge: it strips every gitBrokerManagedHosts entry AND each brokered forge's
// SSH-over-443 endpoint from the allowlist, denies all of them outright, and
// returns what it removed (nil = nothing to do).
//
// SCOPE, exactly. Two sets, deliberately separate (see gitBrokerForges above).
// The SSH deny added is the BARE host, not the ":443" endpoint, so it covers
// every port — a ":443"-only deny would leave ssh.github.com:22 reachable under
// allow_all_egress once the subtraction has removed the allowlist entry.
//
// WHY THE SSH LANE IS NOW CLOSED. This REVERSES an earlier decision of this same
// campaign, on the owner's call; do not restore it. The old rule left
// ssh.github.com:443 allowed on a brokered run holding an ssh_key grant
// (unionRunEgress adds it at create time), reasoning that an ssh_key is an
// operator-supplied, operator-bounded credential and that denying its endpoint
// would break a lane the operator asked for without binding it, since SSH is
// opaque to the receive-pack branch-namespace parser either way. The result was a
// brokered run with github.com:443 denied and ssh.github.com:443 ALLOWED — an
// uninspectable second push route around the one confinement the broker exists to
// enforce, and the single finding that pinned the security review at a ceiling.
//
// What answers the old objection is that the operator is no longer silently
// deprived of the capability: validatePolicySpec (validateGrantLaneExclusivity,
// policy.go) REFUSES a policy declaring both a github_token and an ssh_key grant
// for the same forge, and names the choice — brokered and branch-confined, or
// operator-supplied and unbound, not both for one forge. This function is the
// fail-closed half for anything already stored, and it keys on ACTUAL
// brokered-ness (a non-empty broker map), which policy-write cannot know.
//
// git_pat needs no confinement BEYOND the four managed hosts (they already name
// every GitHub host git dials over HTTPS), but the CREDENTIAL is withheld the same
// way the ssh_key is — dropBrokeredGrants. This corrects an earlier claim here
// that a git_pat on a brokered run was "already dead twice over" and needed
// nothing: the second death it counted was wardyn-git-helper refusing on
// isGitHubHost, and that only binds a caller that asks git for the credential. An
// agent that POSTs the mint route directly never meets the helper, and
// isBrokeredGitGrant (the proxy's mint refusal) matches github_token grant ids
// only — so the PAT minted. The one real barrier was the name-keyed deny below,
// which does not bind a raw-IP CONNECT under allow_all_egress (see "WHAT 'ONLY
// ROUTE' MEANS" below). SSH additionally has no credential-helper chokepoint at
// all — git's seam is HTTP-only — which is why agent-run writes the key file
// itself; that difference is about where a refusal could sit, not about whether
// the credential should be resident.
//
// WHY BOTH. The subtraction keeps the effective allowlist honest — it stops
// claiming a host the run is not meant to dial. The deny is the load-bearing half:
// deny beats allow AND beats allow_all_egress (proxy.Policy.evalHost), so an
// allow-all policy cannot leave the direct route open. Without it the confinement
// the broker route enforces — the per-repo allowlist and the push
// branch-namespace pkt-line parser — was reachable only by shipped-policy
// accident: no git host is ever TLS-MITM'd (LLM + operator artifact hosts only),
// so a direct github.com:443 CONNECT is an opaque tunnel no parser can read, and
// wardyn-git-helper prints a live minted token to stdout inside the sandbox with
// the insteadOf rewrite living in the agent-writable global gitconfig.
//
// The broker's own re-origination to github.com is unaffected: handleGitBroker
// dials through the proxy transport after vetURL (the SSRF/private-IP guard),
// never through the policy evaluator.
//
// WHAT "ONLY ROUTE" MEANS, EXACTLY — the same caveat the four HTTPS denies carry
// in docs/POLICIES.md, and it applies verbatim to the SSH deny: these are
// NAME-based denies, and a name-based deny does not bind an IP LITERAL. Under
// allow_all_egress a CONNECT straight to 140.82.114.4:22 is still allowed
// (measured). So this is the only CONVENIENT route, not the only conceivable
// one; it is what git itself will use, since the clone/push URL carries a name.
// Nothing here creates or worsens that — it predates the SSH deny and binds the
// HTTPS denies identically — but do not restate "the only route" without it.
//
// No-op without git grants — a run with no brokered repo has no broker route, so
// its GitHub egress is the operator's ordinary policy choice.
func confineGitBrokerEgress(policy *types.RunPolicySpec, gitGrants map[string]uuid.UUID) []string {
	if len(gitGrants) == 0 {
		return nil
	}
	sshHosts := gitBrokerSSHHosts()                             // bare, so the deny covers every port
	kept, dropped := policy.AllowedDomains[:0:0], []string(nil) // :0:0 — never alias the caller's array
	for _, d := range policy.AllowedDomains {
		if gitBrokerManaged(d) || slices.Contains(sshHosts, egressEntryHost(d)) {
			dropped = append(dropped, d)
			continue
		}
		kept = append(kept, d)
	}
	policy.AllowedDomains = kept
	// Keyed on the RAW entry, NOT egressEntryHost(d) — do not "tidy" this now that
	// the helper exists. A policy that already denies "github.com:443" must still
	// get the stronger bare-host deny added: normalizing the key would suppress it
	// as a duplicate and leave github.com:22 reachable under allow_all_egress. The
	// whole suite passes with that one-token edit except TestConfineGitBrokerEgress'
	// pre-existing-:443-deny case, which exists to catch exactly it.
	denied := map[string]bool{}
	for _, d := range policy.DeniedDomains {
		denied[strings.ToLower(strings.TrimSpace(d))] = true
	}
	add := []string(nil)
	for _, h := range append(append([]string(nil), gitBrokerManagedHosts...), sshHosts...) {
		if !denied[h] {
			add = append(add, h)
		}
	}
	if len(add) > 0 {
		policy.DeniedDomains = append(append([]string(nil), policy.DeniedDomains...), add...)
		dropped = append(dropped, add...)
	}
	return dropped
}

// gitBrokerManaged reports whether ONE egress-allowlist entry names a host the
// git-broker manages — an exact host, a "*." wildcard entry the broker's own
// wildcard covers, or either carrying a ":port" qualifier. Normalization mirrors
// proxy.CompilePolicy (lowercase, trailing dot stripped) so what is subtracted is
// exactly what the proxy would otherwise have allowed.
func gitBrokerManaged(entry string) bool {
	h := egressEntryHost(entry)
	for _, m := range gitBrokerManagedHosts {
		if suffix, wild := strings.CutPrefix(m, "*"); wild {
			if strings.HasSuffix(h, suffix) {
				return true
			}
			continue
		}
		if h == m {
			return true
		}
	}
	return false
}

// egressEntryHost normalizes ONE egress-list entry to its bare host: lowercased,
// ":port" qualifier and trailing dot stripped — the same normalization
// proxy.classifyDomain applies to the WELL-FORMED entries ValidDomainEntry lets
// through, which is every entry that can reach here.
//
// It is NOT a general re-implementation of classifyDomain, and a new caller must
// not assume it. classifyDomain splits with net.SplitHostPort and honours only
// ports 1..65535; this splits at the first ":". They diverge on "github.com:0"
// (classifyDomain keeps the whole string as a host that matches nothing; this
// yields "github.com") and on a bracketed IPv6 literal ("[::1]:443" -> "[").
// Neither is reachable today — the first is rejected at policy write by
// proxy.ValidDomainEntry, the second is an address, not one of the broker's host
// NAMES, and is caught by the proxy's private-IP guard — which is why this stays
// a five-line local helper instead of a second copy of classifyDomain that could
// drift from the real matcher. Needing either case means calling the proxy.
func egressEntryHost(entry string) string {
	h := strings.ToLower(strings.TrimSpace(entry))
	if host, _, ok := strings.Cut(h, ":"); ok { // "github.com:443" -> "github.com"
		h = host
	}
	return strings.TrimSuffix(h, ".")
}

// auditBrokeredGrantDrop warns + records the audit event for one kind's withheld
// hosts, and is a no-op when nothing was withheld. Both drops route through here
// so neither can ever become the silent one: the operator asked for a credential
// and is not getting it, so the run says which kind, which hosts, and why.
func (s *Server) auditBrokeredGrantDrop(ctx context.Context, runID uuid.UUID, kind, action string, hosts []string, note string) {
	if len(hosts) == 0 {
		return
	}
	slog.WarnContext(ctx, "wardynd: brokered run — withholding "+kind+" grant(s) for the brokered forge; a brokered forge is single-lane, push through the git broker",
		slog.String("run_id", runID.String()), slog.Any("hosts", hosts))
	s.recordAudit(ctx, s.auditEvent(&runID, types.ActorSystem, "wardynd", action,
		runID.String(), "failure", mustJSON(map[string]any{"dropped_hosts": hosts, "note": note})))
}

// dropBrokeredGrants withholds the {host: grant_id} entries whose host belongs
// to a BROKERED forge, returning the survivors (never the caller's map, which
// other phases still read) and the hosts dropped, sorted; nil when there is
// nothing to drop. brokeredHost is the per-kind same-forge test
// (brokeredForgeSSHHost for ssh_key, brokeredForgeHost for git_pat).
//
// WHY THE CREDENTIAL AND NOT JUST THE LANE. confineGitBrokerEgress closes the
// NETWORK path; on its own that leaves the KEY (or the PAT). A pre-existing
// stored policy can still hold both grants — validateGrantLaneExclusivity
// refuses new WRITES, and resolveRunPolicy deliberately does not re-validate what
// is already stored — so without this, a brokered run still exported
// WARDYN_SSH_GRANTS, agent-run's provision_ssh_grants still minted the key and
// wrote it 0400, and the proxy's mint route still served it (isBrokeredGitGrant
// matches github_token grant ids only). The result was a GitHub private key
// resident in the sandbox that cannot reach its forge, stays re-mintable for the
// whole run (docs/POLICIES.md: "the agent process can re-mint the same private
// key for itself at any point in the run"), and is exfiltratable through any
// other egress the run is allowed. A capability that cannot be used is not a
// capability, it is only a liability.
//
// GIT_PAT IS THE SAME LIABILITY, and used to be exempt on a justification that
// did not hold. The old reasoning counted two deaths: wardyn-git-helper refuses
// on isGitHubHost before the PAT fallback whenever WARDYN_GIT_BROKER_REPOS is
// set, and github.com is one of confineGitBrokerEgress's denies. The first is not
// a barrier against the threat this exists for — the helper is a convenience for
// git, and nothing makes an agent go through it; the mint route takes a POST with
// a grant id, and isBrokeredGitGrant only matches github_token grant ids, so the
// PAT minted straight out. That leaves ONE barrier, the name-keyed deny, which
// this file already documents as not binding a raw-IP CONNECT under
// allow_all_egress (confineGitBrokerEgress, "WHAT 'ONLY ROUTE' MEANS"). And a
// GitHub git_pat is typically a USER PAT: broader than the repo-scoped
// installation token it sits beside, and bound by no branch namespace.
//
// AT DISPATCH, NOT CREATE, for the same reason confineGitBrokerEgress is here:
// "brokered" is a RUN-time fact (augmentGitBrokerGrants seeds the broker map from
// the run's declared clone set, not just the grant scope), so create cannot know
// it. The two halves therefore key on the SAME map and cannot disagree.
//
// A non-brokered forge's credential (dev.azure.com, gitlab.com) is untouched, and
// so is EVERY grant on a non-brokered run.
func dropBrokeredGrants(grants map[string]string, gitGrants map[string]uuid.UUID, brokeredHost func(string) bool) (map[string]string, []string) {
	if len(grants) == 0 || len(gitGrants) == 0 {
		return grants, nil
	}
	kept, dropped := map[string]string{}, []string(nil)
	for host, grantID := range grants {
		if brokeredHost(host) {
			dropped = append(dropped, host)
			continue
		}
		kept[host] = grantID
	}
	slices.Sort(dropped)
	return kept, dropped
}

// brokeredForgeSSHHost is the ssh_key same-forge test: sshOver443Endpoint folds
// "github.com" and "ssh.github.com" onto one endpoint, compared against
// gitBrokerSSHEndpoints.
func brokeredForgeSSHHost(host string) bool {
	ep, ok := sshOver443Endpoint(host)
	return ok && slices.Contains(gitBrokerSSHEndpoints(), ep)
}

// brokeredForgeHost is the git_pat/HTTPS same-forge test: the host IS a brokered
// forge or a subdomain of one. Derived from gitBrokerForges so both halves of the
// single-lane rule still read from ONE list, and deliberately the same shape as
// wardyn-git-helper's isGitHubHost ("github.com" or "*.github.com") — the helper's
// refusal and this withholding must cover the same hosts or one of them is a
// credential path the other thinks is closed.
func brokeredForgeHost(host string) bool {
	h := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
	for _, f := range gitBrokerForges {
		if h == f || strings.HasSuffix(h, "."+f) {
			return true
		}
	}
	return false
}
