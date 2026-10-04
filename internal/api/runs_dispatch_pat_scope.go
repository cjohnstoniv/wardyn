// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A narrowed git_pat grant narrows the RUN, and only the PAT broker can enforce
// it (the proxy's /wardyn/git/ route, pat_scope.go there). Dispatch therefore
// carries each winning grant's scope to the proxy, and refuses the run when the
// narrowing could not be enforced: a narrowing that cannot bind would read as a
// boundary and be none.

// patNarrowingEnv is what the three refusals read beyond the grants themselves.
// Dispatch and Review fill it from the same sources, so they answer alike.
type patNarrowingEnv struct {
	brokerOn bool // WARDYN_GIT_PAT_BROKER is not off
	// brokered is whether the run has a GitHub-brokered forge (its github_token
	// grant seeds the broker map): its git_pat grant for that forge is withheld.
	brokered bool
	site     types.SiteConfig
	// bbsAPI is whether the Bitbucket Server API door is on (WARDYN_GIT_PAT_API_BITBUCKET_SERVER).
	bbsAPI bool
	// adoHosts are the hosts the run's Azure DevOps gate covers, which
	// serveADOGit handles before the broker's scope checks.
	adoHosts []string
}

// patNarrowingRefusal returns the reason and a sentence (it opens on the grant,
// so the caller adds its own lead) for the first git_pat
// grant in grants whose narrowing the run could not enforce, or "" when none.
// grants is the run's eligible grants, as specs.
func patNarrowingRefusal(grants []types.GrantSpec, env patNarrowingEnv) (reason, detail string) {
	for _, g := range grants {
		if g.Kind != types.GrantGitPAT {
			continue
		}
		sc, err := types.DecodeGitPATScope(g.Scope)
		if err != nil {
			continue
		}
		switch {
		case sc.Narrowed() && !env.brokerOn:
			return reasonGitPATNarrowingNeedsBroker, fmt.Sprintf(
				"The git_pat grant for %s sets repos, access or api, and that narrowing is enforced only by the PAT broker, which is off (WARDYN_GIT_PAT_BROKER). Without it the PAT is resident in the sandbox and nothing narrows it.", sc.Host)
		case sc.API && sc.Forge == types.PATForgeBitbucketServer && !env.bbsAPI:
			return reasonGitPATAPIForgeDisabled, fmt.Sprintf(
				"The git_pat grant for %s sets api for bitbucket_server, and that forge's API door is off on this deployment (WARDYN_GIT_PAT_API_BITBUCKET_SERVER).", sc.Host)
		case sc.Narrowed() && patSSHKeyFor(grants, sc.Host):
			return reasonGitPATNarrowingSSHConflict, fmt.Sprintf(
				"The git_pat grant for %s sets repos, access or api, and the run also holds an ssh_key for the same forge. SSH is a second push path the broker cannot see, so the narrowing would not bind.", sc.Host)
		case sc.SetsAnyAxis() && patNarrowingHostUnsupported(env, sc.Host):
			return reasonGitPATNarrowingUnsupportedHost, fmt.Sprintf(
				"The git_pat grant for %s sets repos, access, api or forge, and that host is served by another lane (Azure DevOps, or a GitHub-brokered forge) that does not read those fields.", sc.Host)
		}
	}
	return "", ""
}

// patSSHKeyFor reports whether grants hold an ssh_key for the same forge as
// host. Both fold through sshOver443Endpoint, so github.com and
// ssh.github.com are one forge.
func patSSHKeyFor(grants []types.GrantSpec, host string) bool {
	want, ok := sshOver443Endpoint(host)
	if !ok {
		return false
	}
	return slices.ContainsFunc(grants, func(g types.GrantSpec) bool {
		if g.Kind != types.GrantSSHKey {
			return false
		}
		sshHost, _, _, _, err := sshKeyScopeFields(g.Scope)
		if err != nil {
			return false
		}
		got, ok := sshOver443Endpoint(sshHost)
		return ok && got == want
	})
}

// patNarrowingHostUnsupported reports whether host is served by a lane that
// ignores the narrowing axes: an Azure DevOps host or one the run's Azure
// DevOps gate covers (handlePATBroker hands it to serveADOGit first), or a
// brokered forge's host (its PAT is withheld).
func patNarrowingHostUnsupported(env patNarrowingEnv, host string) bool {
	return adoGrantHost(env.site, host) ||
		slices.ContainsFunc(env.adoHosts, func(h string) bool { return hostEqual(h, host) }) ||
		(env.brokered && brokeredForgeHost(host))
}

// patAPIDoor reports whether any git_pat grant among rows sets api: true. Its
// forge host is then terminated by the proxy, which needs the per-run MITM CA.
func patAPIDoor(rows []types.CredentialGrant) bool {
	return slices.ContainsFunc(rows, func(r types.CredentialGrant) bool {
		if r.Spec.Kind != types.GrantGitPAT {
			return false
		}
		sc, err := types.DecodeGitPATScope(r.Spec.Scope)
		return err == nil && sc.API
	})
}

// patGrantSpecs is the specs of a run's stored grants.
func patGrantSpecs(rows []types.CredentialGrant) []types.GrantSpec {
	out := make([]types.GrantSpec, len(rows))
	for i, r := range rows {
		out[i] = r.Spec
	}
	return out
}

// patGrantIDsOf is every git_pat grant id among rows (brokeredPATGrantIDs'
// set), nil when there is none.
func patGrantIDsOf(rows []types.CredentialGrant) []uuid.UUID {
	var ids []uuid.UUID
	for _, g := range rows {
		if g.Spec.Kind == types.GrantGitPAT {
			ids = append(ids, g.ID)
		}
	}
	return ids
}

// errPATScopeRowMissing is a winning grant id with no stored row.
var errPATScopeRowMissing = errors.New("git_pat grant has no stored row to read its scope from")

// scopePATGrants adds each grant's narrowing to the broker allowlist from the
// run's stored grant rows. Only a narrowed grant carries any, so an unnarrowed
// grant renders as it always did. A winning id with no row is an error: its
// scope is unknown, and an unknown scope must not read as an unnarrowed one.
func scopePATGrants(grants map[string]proxy.PATGrant, rows []types.CredentialGrant) error {
	for host, g := range grants {
		i := slices.IndexFunc(rows, func(r types.CredentialGrant) bool {
			return r.ID == g.GrantID && r.Spec.Kind == types.GrantGitPAT
		})
		if i < 0 {
			return fmt.Errorf("%w: host %q, grant %s", errPATScopeRowMissing, host, g.GrantID)
		}
		sc, err := types.DecodeGitPATScope(rows[i].Spec.Scope)
		if err != nil {
			return fmt.Errorf("git_pat grant %s scope: %w", g.GrantID, err)
		}
		if !sc.Narrowed() {
			continue
		}
		g.Repos, g.API = sc.Repos, sc.API
		if sc.Access == types.PATAccessRead {
			g.Access = sc.Access
		}
		if sc.Forge != types.PATForgeGeneric {
			g.Forge = sc.Forge
		}
		grants[host] = g
	}
	return nil
}

// enforceablePATNarrowing is false, with the run already failed, when a
// narrowed git_pat grant could not be enforced (patNarrowingRefusal). rows is the
// one ListGrantsByRun read dispatch makes.
func (s *Server) enforceablePATNarrowing(ctx context.Context, run types.AgentRun, p dispatchParams,
	rows []types.CredentialGrant, site types.SiteConfig, adoRun adoEntraRun, adoInject bool,
) bool {
	env := patNarrowingEnvOf(p.PATBroker, p.GitGrants, site, adoRun, adoInject)
	if reason, detail := patNarrowingRefusal(patGrantSpecs(rows), env); reason != "" {
		return s.refusePATDispatch(ctx, run, reason, "This run was not launched: "+detail)
	}
	return true
}

// patNarrowingEnvOf is the env dispatch and revive both fill: the broker
// switch, the run's brokered-forge map, the site config and the run's Azure
// DevOps lane as resolveADOEntraRun decided it.
func patNarrowingEnvOf(brokerOn bool, gitGrants map[string]uuid.UUID, site types.SiteConfig, adoRun adoEntraRun, adoOn bool) patNarrowingEnv {
	env := patNarrowingEnv{brokerOn: brokerOn, brokered: len(gitGrants) > 0, site: site, bbsAPI: patAPIBitbucketOn()}
	if adoOn {
		env.adoHosts = adoRun.laneHosts()
	}
	return env
}

// scopedPATGrants is the proxy's git_pat allowlist for the run, each narrowed
// grant carrying its scope from the stored row, or ok=false with the run already
// failed because a winning grant's scope could not be read. It reads p after the
// ceiling re-assertion has dropped a denied host's lane, so a dropped lane stays
// dropped.
func (s *Server) scopedPATGrants(ctx context.Context, run types.AgentRun, p dispatchParams,
	rows []types.CredentialGrant,
) (map[string]proxy.PATGrant, bool) {
	grants := patBrokerGrants(p.GitPATGrants, p.PATBroker)
	if err := scopePATGrants(grants, rows); err != nil {
		return nil, s.refusePATDispatch(ctx, run, "",
			"This run was not launched: a git_pat grant's scope could not be read, so what it is narrowed to is unknown: "+err.Error())
	}
	return grants, true
}

// refusePATDispatch fails the run with the sentence and audits it on run.create,
// the shape every dispatch-time lane refusal takes. reason is "" for a refusal
// that has no wire reason. It returns false so a caller can
// `return …, s.refusePATDispatch(…)`.
func (s *Server) refusePATDispatch(ctx context.Context, run types.AgentRun, reason, detail string) bool {
	s.failAndRevoke(ctx, run.ID, types.RunStarting, detail)
	data := map[string]any{"error": "git_pat narrowing: " + cmp.Or(reason, "scope unreadable"), "detail": detail}
	if reason != "" {
		data["reason"] = reason
	}
	s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.create",
		run.ID.String(), "failure", mustJSON(data)))
	return false
}

// patNarrowingAtDoor is dispatch's refusal asked at Review (POST /runs/preflight),
// from the spec's grants and the site snapshot the autonomy gate read, so a
// person hears it before the run rather than as a FAILED badge. "" when none.
// Whether the run is brokered is read off its github_token grant, which is
// how a brokered run comes to have a broker map at all.
func (s *Server) patNarrowingAtDoor(r *http.Request, spec types.RunPolicySpec, site types.SiteConfig) (reason, detail string) {
	env := patNarrowingEnv{brokerOn: !s.cfg.DisableGitPATBroker, site: site, bbsAPI: patAPIBitbucketOn()}
	env.brokered = slices.ContainsFunc(spec.EligibleGrants, func(g types.GrantSpec) bool { return g.Kind == types.GrantGitHubToken })
	if lane, on := resolveADOEntraRun(site, repoLocatorsOf(spec.WorkspaceRepos),
		runIdentitySubject(r.Context(), principalFromRequest(r))); on {
		env.adoHosts = lane.laneHosts()
	}
	return patNarrowingRefusal(spec.EligibleGrants, env)
}
