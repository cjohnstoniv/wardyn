// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A revive, and an extension of a run's end, keep a run alive on authority
// its owner was given at launch. Both re-check that authority as it stands
// now, as the OWNER (never the caller, for run_revive.go's reason): the
// captured profile still exists, and the launch doors the run can still be
// read back for (its agent, its workspaces, the git provider rows of its
// repos) are still open to the owner. A revive also re-checks the model
// credential its proxy would inject, and rebuilds the deployment-wide parts
// of the proxy config from the current configuration instead of reusing the
// rendered copy.
//
// Two doors cannot be read back from the run, and are not re-checked: an
// explicit integration_id (the run row does not record it) and an explicit
// image (the row cannot tell a member-named image from the convention image
// or a workspace's base image, and the image kind WIDENS, so re-checking
// every image would refuse every member run on a deployment that never
// enforced it).

// ownerRefusal is a re-check that failed: status is the HTTP answer, reason
// the audit reason.
type ownerRefusal struct {
	status int
	reason string
	msg    string
}

// ownerCapabilityRefusal re-checks the run's launch-door capabilities for its
// owner. callerIsOwner says ctx carries the owner's own session, which
// resolves exactly as it did at launch (their email and group snapshot
// included). Otherwise the owner is known only by the sub on the run row:
// any deny row of the kind covering the value refuses, since the owner's
// email and groups cannot be ruled out, and only allow rows for that sub or
// everyone count. Nor can the owner's role be known by sub, so an owner who
// launched as an admin (exempt at the launch gate) is held to the same rows:
// under an enforced kind another admin's revive, restart or extension of
// their run needs an allow row for the owner's sub or everyone. The owner's
// own session passes.
func (s *Server) ownerCapabilityRefusal(ctx context.Context, run types.AgentRun, callerIsOwner bool, repos []string) (*ownerRefusal, error) {
	if callerIsOwner && s.isOperator(ctx) {
		return nil, nil
	}
	if !callerIsOwner && run.CreatedBy == adminTokenPrincipal {
		return nil, nil
	}
	// label is what the refusal names: a provider row by its kind only, never
	// its id or base URL, as at the create gate (capProvider403).
	type door struct{ kind, value, label string }
	var doors []door
	if run.Agent != "" {
		doors = append(doors, door{capAgent, run.Agent, run.Agent})
	}
	for _, id := range run.WorkspaceIDs {
		doors = append(doors, door{capWorkspace, id.String(), id.String()})
	}
	rows, err := s.ownerProviderRows(ctx, repos)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		doors = append(doors, door{capWorkspaceProvider, row.ID, "this deployment's " + string(row.Kind) + " provider"})
	}
	for _, d := range doors {
		var allowed bool
		if callerIsOwner {
			allowed, err = s.capSeamAllowed(ctx, d.kind, d.value)
		} else {
			allowed, err = s.capAllowedForSub(ctx, run.CreatedBy, d.kind, d.value)
		}
		if err != nil {
			return nil, err
		}
		if !allowed {
			return &ownerRefusal{status: http.StatusForbidden, reason: "capability_" + d.kind, msg: fmt.Sprintf(
				"the run's owner no longer holds the %s capability for %s; start a new run", d.kind, d.label)}, nil
		}
	}
	return nil, nil
}

// ownerProviderRows is the git provider rows repos resolve to, as the
// run-create gate resolves them (denyMemberWorkspaceProviders). None when the
// deployment configures no providers.
func (s *Server) ownerProviderRows(ctx context.Context, repos []string) ([]types.GitProvider, error) {
	if len(repos) == 0 || s.cfg.Store == nil {
		return nil, nil
	}
	sc, err := s.cfg.Store.GetSiteConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("read site config: %w", err)
	}
	if !providersConfigured(sc) {
		return nil, nil
	}
	var rows []types.GitProvider
	for _, repo := range repos {
		row := admitRepoURL(sc, repoCloneURL(repo)).Provider
		if row.ID != "" && !slices.ContainsFunc(rows, func(r types.GitProvider) bool { return r.ID == row.ID }) {
			rows = append(rows, row)
		}
	}
	return rows, nil
}

// capAllowedForSub is capSeamAllowed for an owner known only by sub. A store
// that cannot answer is an error, never an allow.
func (s *Server) capAllowedForSub(ctx context.Context, sub, kind, value string) (bool, error) {
	if s.cfg.Store == nil {
		return true, nil
	}
	grants, err := s.cfg.Store.ListCapabilityGrants(ctx)
	if err != nil {
		return false, fmt.Errorf("api: resolve capability %q: %w", kind, err)
	}
	sub = canonicalUserSubject(sub)
	allow := false
	for _, g := range grants {
		if g.Capability != kind {
			continue
		}
		if g.Effect == types.CapabilityDeny {
			if capValueOverlaps(kind, g.Value, value) {
				return false, nil
			}
			continue
		}
		if capValueMatches(kind, g.Value, value) &&
			(g.SubjectType == types.CapabilitySubjectAll || (g.SubjectType == types.CapabilitySubjectUser && g.Subject == sub)) {
			allow = true
		}
	}
	if allow {
		return true, nil
	}
	enforced, err := s.newCapBatch(ctx).enforced(ctx, kind)
	if err != nil {
		return false, err
	}
	return !enforced, nil
}

// ownerProfile is the run's captured profile, refusing when it no longer exists:
// its walls cannot be known. A run that captured none (an unassigned or
// super-admin owner) passes.
func (s *Server) ownerProfile(ctx context.Context, run types.AgentRun) (*types.GovernanceProfile, *ownerRefusal) {
	if run.GovernanceProfileID == nil {
		return nil, nil
	}
	profiles, err := s.cfg.Store.ListGovernanceProfiles(ctx)
	if err != nil {
		return nil, &ownerRefusal{status: http.StatusServiceUnavailable, reason: "profile_unreadable",
			msg: "resolve the owner's governance profile: " + err.Error()}
	}
	i := slices.IndexFunc(profiles, func(p types.GovernanceProfile) bool { return p.ID == *run.GovernanceProfileID })
	if i < 0 {
		return nil, &ownerRefusal{status: http.StatusConflict, reason: "profile_gone",
			msg: "the governance profile this run was created under no longer exists; start a new run"}
	}
	return &profiles[i], nil
}

// modelCredentialRefusal re-checks every api_key injection the revived proxy
// would carry: the secret its grant names must still exist for the owner
// (their own or the operator's, the namespaces the injection sink reads), and
// an integration that holds it must not have been disabled. Without this the
// revived proxy starts, and the run's first model call fails.
func (s *Server) modelCredentialRefusal(ctx context.Context, run types.AgentRun, cfg *proxy.Config) (*ownerRefusal, error) {
	if len(cfg.Injection) == 0 {
		return nil, nil
	}
	grants, err := s.cfg.Store.ListGrantsByRun(ctx, run.ID)
	if err != nil {
		return nil, fmt.Errorf("read the run's grants: %w", err)
	}
	byID := make(map[uuid.UUID]types.CredentialGrant, len(grants))
	for _, g := range grants {
		byID[g.ID] = g
	}
	var sc *types.SiteConfig
	for _, in := range cfg.Injection {
		g, ok := byID[in.GrantID]
		if !ok || g.Spec.Kind != types.GrantAPIKey {
			continue
		}
		var scope struct {
			SecretName string `json:"secret_name"`
		}
		if err := json.Unmarshal(g.Spec.Scope, &scope); err != nil || scope.SecretName == "" {
			continue
		}
		present, err := s.secretPresentFor(ctx, runIdentitySubject(ctx, run.CreatedBy), scope.SecretName)
		if err != nil {
			return nil, err
		}
		if !present {
			return &ownerRefusal{status: http.StatusConflict, reason: "model_credential_erased", msg: fmt.Sprintf(
				"the credential this run injects for %s (secret %s) no longer exists; start a new run", in.Host, scope.SecretName)}, nil
		}
		if sc == nil {
			got, err := s.cfg.Store.GetSiteConfig(ctx)
			if err != nil {
				return nil, fmt.Errorf("read site config: %w", err)
			}
			sc = &got
		}
		if name, off := integrationDisabledFor(*sc, scope.SecretName); off {
			return &ownerRefusal{status: http.StatusConflict, reason: "model_provider_disabled", msg: fmt.Sprintf(
				"the integration %s that supplies this run's credential for %s is disabled; start a new run", name, in.Host)}, nil
		}
	}
	return nil, nil
}

// secretPresentFor reports whether name exists in owner's namespace or the
// operator's. A store that cannot list is an error, never "present".
func (s *Server) secretPresentFor(ctx context.Context, owner, name string) (bool, error) {
	if s.cfg.Secrets == nil {
		return false, errors.New("no secret store configured")
	}
	for _, ns := range []string{owner, ""} {
		names, err := s.cfg.Secrets.For(ns).List(ctx)
		if err != nil && !errors.Is(err, secretstore.ErrNotFound) {
			return false, fmt.Errorf("list secrets: %w", err)
		}
		if slices.Contains(names, name) {
			return true, nil
		}
	}
	return false, nil
}

// integrationDisabledFor reports whether the integrations that hold secret
// are all disabled, naming one. A secret no integration holds (a grant a
// policy authored directly) is not an integration's to switch off.
func integrationDisabledFor(sc types.SiteConfig, secret string) (string, bool) {
	var name string
	for _, in := range sc.Integrations {
		if !integrationHoldsSecret(in, secret) {
			continue
		}
		if !in.Disabled {
			return "", false
		}
		name = in.Name
	}
	return name, name != ""
}

func integrationHoldsSecret(in types.Integration, secret string) bool {
	if slices.ContainsFunc(in.Secrets, func(s types.IntegrationSecret) bool { return s.SecretName == secret }) {
		return true
	}
	name, _, _, ok := in.HeaderSecret()
	return ok && name == secret
}

// refreshDeploymentConfig replaces the parts of a rendered proxy config that
// come from the deployment, not the run, with the current configuration: the
// upstream proxy, the trusted CA, the internal model gateways and the
// internal-host lift. A site config that cannot be read refuses rather than
// reuse the rendered copy. The internal-host lift only narrows: a host the
// operator added since is not lifted for a run that never had it. The
// brokered-LLM 404 detail (LLMUnavailableDetail) keeps its rendered text:
// recomputing it needs the dispatch-time LLM plan, and it changes only what
// that 404 says, not what the sandbox may reach.
func (s *Server) refreshDeploymentConfig(ctx context.Context, run types.AgentRun, cfg *proxy.Config) error {
	sc, err := s.siteConfigForDispatch(ctx)
	if err != nil {
		return fmt.Errorf("read site config: %w", err)
	}
	cfg.UpstreamProxyURL = s.resolveRunUpstreamProxy(ctx, run.ID, sc, nil)
	cfg.UpstreamProxyNoProxy = sc.UpstreamProxyNoProxy
	cfg.TrustedCAPEM = s.cfg.TrustedCAPEM
	cfg.LLMUpstreams = s.cfg.LLMGateways
	cfg.InternalHosts = slices.DeleteFunc(cfg.InternalHosts, func(h types.InternalHost) bool {
		return !slices.ContainsFunc(sc.InternalHosts, func(c types.InternalHost) bool {
			return c.HostSuffix == h.HostSuffix && slices.Equal(c.CIDRs, h.CIDRs)
		})
	})
	return nil
}

// reviveOwnerRecheck runs the re-checks above for a revive, before its claim,
// and rebuilds cfg's deployment parts. A refusal is audited as run.revive
// denied; a check that cannot be answered refuses too.
func (s *Server) reviveOwnerRecheck(ctx context.Context, run types.AgentRun, cfg *proxy.Config, actorType types.ActorType, actor string) *reviveError {
	ref, err := s.ownerCapabilityRefusal(ctx, run, actor == run.CreatedBy, runRepos(run, cfg))
	if err == nil && ref == nil {
		ref, err = s.modelCredentialRefusal(ctx, run, cfg)
	}
	if err == nil && ref == nil {
		err = s.refreshDeploymentConfig(ctx, run, cfg)
	}
	if err != nil {
		return reviveRefused(http.StatusServiceUnavailable, "re-check the run owner's authority: "+err.Error())
	}
	if ref != nil {
		s.recordAudit(ctx, s.auditEvent(&run.ID, actorType, actor, "run.revive", run.ID.String(), "denied",
			mustJSON(map[string]any{"subject": run.CreatedBy, "reason": ref.reason})))
		return reviveRefused(ref.status, ref.msg)
	}
	return nil
}

// runRepos is the run's repos: its legacy repo field and, when its rendered
// proxy config can be read, the repos of its resolved policy.
func runRepos(run types.AgentRun, cfg *proxy.Config) []string {
	var repos []string
	if run.Repo != "" {
		repos = append(repos, run.Repo)
	}
	if cfg != nil {
		repos = append(repos, repoLocatorsOf(cfg.Policy.WorkspaceRepos)...)
	}
	return repos
}

// extendRefusal re-checks the owner's authority before a run's end moves
// later or is removed: extending keeps a sandbox and its credentials alive,
// so it needs the authority a revive needs, less the proxy rebuild. The
// repos come from the run's rendered proxy config where the runner can read
// it back; a runner that cannot (no revive support) leaves the legacy repo
// field alone.
func (s *Server) extendRefusal(r *http.Request, run types.AgentRun) *ownerRefusal {
	ctx := r.Context()
	if _, ref := s.ownerProfile(ctx, run); ref != nil {
		return ref
	}
	var cfg *proxy.Config
	if rv, ok := s.cfg.Runner.(runner.ProxyReviver); ok && run.SandboxRef != "" {
		raw, err := rv.ProxyConfig(ctx, run.SandboxRef)
		switch {
		case errors.Is(err, runner.ErrReviveUnsupported):
		case err != nil:
			return &ownerRefusal{status: http.StatusBadGateway, reason: "owner_unverifiable",
				msg: "read the run's proxy config to re-check its owner's authority: " + err.Error()}
		default:
			if cfg, err = proxy.LoadConfigBytes(raw); err != nil {
				return &ownerRefusal{status: http.StatusConflict, reason: "owner_unverifiable",
					msg: "the run's proxy config does not load: " + err.Error()}
			}
		}
	}
	ref, err := s.ownerCapabilityRefusal(ctx, run, principalFromRequest(r) == run.CreatedBy, runRepos(run, cfg))
	if err != nil {
		return &ownerRefusal{status: http.StatusServiceUnavailable, reason: "owner_unverifiable",
			msg: "re-check the run owner's authority: " + err.Error()}
	}
	return ref
}
