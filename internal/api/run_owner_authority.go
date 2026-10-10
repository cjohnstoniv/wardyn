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
	"strings"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A revive, and an extension of a run's end, keep a run alive on authority
// its owner was given at launch. Both re-check that authority as it stands
// now, as the OWNER (never the caller, for run_revive.go's reason): the
// captured profile still exists, and the launch doors the run can still be
// read back for (its agent, its workspaces, its model provider, the stored
// policy it selected, the git provider rows of its repos) are still open to
// the owner. A revive also re-checks the model credential its proxy would
// inject, and rebuilds the deployment-wide parts of the proxy config from the
// current configuration instead of reusing the rendered copy.
//
// A run's components are read back from its run_components rows, which outlive
// the erasure of everything a person typed into them: each row still says
// whether its component was the owner's own (the custom_component feature) or
// an organisation's (that component's grant), so the door is re-checked after
// the content is gone. No rows at all is a run launched without components.
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
// included). Otherwise the owner is known only by the sub and the user type
// stamped on the run row: any deny row of the kind covering the value
// refuses, since the owner's email and groups cannot be ruled out, and only
// allow rows for that sub, that type or everyone count. Nor can the owner's
// role be known by sub, so an owner who launched as an admin (exempt at the
// launch gate) is held to the same rows: under an enforced kind another
// admin's revive, restart or extension of their run needs an allow row for
// the owner's sub, type or everyone. The owner's own session passes.
func (s *Server) ownerCapabilityRefusal(ctx context.Context, run types.AgentRun, callerIsOwner bool, repos []string) (*ownerRefusal, error) {
	// Before either exemption below: a deleted component is gone for everyone.
	comps, err := s.runComponentSnapshot(ctx, run.ID)
	if err != nil {
		return nil, err
	}
	if ref, err := s.componentGoneRefusal(ctx, comps); ref != nil || err != nil {
		return ref, err
	}
	if callerIsOwner && s.runUngoverned(ctx) {
		return nil, nil
	}
	// An operator-owned run (the admin token, local mode) has no person whose
	// capabilities could have changed. Read from the run's recorded
	// authentication, never created_by, which an IdP sub could spell alike.
	if !callerIsOwner && run.OperatorOwned {
		return nil, nil
	}
	rows, err := s.ownerProviderRows(ctx, repos)
	if err != nil {
		return nil, err
	}
	for _, d := range persistedLaunchDoors(run, rows, comps) {
		var allowed bool
		if callerIsOwner {
			allowed, err = s.capSeamAllowed(ctx, d.kind, d.value)
		} else {
			allowed, err = s.capAllowedForSub(ctx, run.CreatedBy, run.UserType, d.kind, d.value)
		}
		if err != nil {
			return nil, err
		}
		if !allowed {
			return &ownerRefusal{status: http.StatusForbidden, reason: capabilityLostReason(d.kind), msg: fmt.Sprintf(
				"the run's owner no longer holds the %s capability for %s; start a new run", d.kind, d.label)}, nil
		}
	}
	return nil, nil
}

// capabilityLostReason is ownerCapabilityRefusal's wire reason for a closed
// launch-door capability, kept as a lookup rather than string-concatenating
// "capability_"+kind (#656 slice 3): the kinds persistedLaunchDoors can
// ever produce are capabilities.go's own cap* consts, a closed set, so the
// reason is one of reasons_routes.go's own reasonOwnerCapability* literals —
// visible to TestReasonDocsMatchReasonsGo, which reads both reasons files'
// string literals, not a runtime concatenation. reasonOwnerCapabilityUnknown
// is defensive only: none of the known kinds falls through to it today.
func capabilityLostReason(kind string) string {
	switch kind {
	case capAgent:
		return reasonOwnerCapabilityAgent
	case capWorkspace:
		return reasonOwnerCapabilityWorkspace
	case capModelProvider:
		return reasonOwnerCapabilityModelProvider
	case capPolicy:
		return reasonOwnerCapabilityPolicy
	case capWorkspaceProvider:
		return reasonOwnerCapabilityWorkspaceProvider
	case capComponent, capFeature:
		// The one feature value a run records is custom_component, the door of
		// a component its owner defined: either kind is the run's permission to
		// carry a component.
		return reasonOwnerCapabilityComponent
	default:
		return reasonOwnerCapabilityUnknown
	}
}

// door is one launch capability a run was admitted through. label is what a
// refusal names: a provider row by its kind only, never its id or base URL, as
// at the create gate (capProvider403).
type door struct{ kind, value, label string }

// persistedLaunchDoors is the launch doors the run row records, rows being the
// git provider rows of its repos and comps its component snapshot. A legacy
// row with no model provider or no selected policy adds no door for it.
//
// A component's door is read off the snapshot row and nothing else: an
// organisation's component needs that component's grant, and any component the
// owner defined — inline, saved, or erased since, which leaves the row and
// clears its content — needs the custom_component feature, asked once.
func persistedLaunchDoors(run types.AgentRun, rows []types.GitProvider, comps []types.RunComponent) []door {
	var doors []door
	if run.Agent != "" {
		doors = append(doors, door{capAgent, run.Agent, run.Agent})
	}
	for _, id := range run.WorkspaceIDs {
		doors = append(doors, door{capWorkspace, id.String(), id.String()})
	}
	if run.ModelProviderID != "" {
		doors = append(doors, door{capModelProvider, run.ModelProviderID, run.ModelProviderID})
	}
	if run.PolicyID != nil {
		doors = append(doors, door{capPolicy, run.PolicyID.String(), run.PolicyID.String()})
	}
	for _, row := range rows {
		doors = append(doors, door{capWorkspaceProvider, row.ID, "this deployment's " + string(row.Kind) + " provider"})
	}
	selfDefined := false
	for _, c := range comps {
		switch {
		case c.SelfDefined:
			selfDefined = true
		case c.ComponentID != nil:
			// Named by what it is, never its id or name: the refusal says
			// nothing about an organisation's component, as at the attach door.
			doors = append(doors, door{capComponent, c.ComponentID.String(), "a component your organisation provides"})
		}
	}
	if selfDefined {
		doors = append(doors, door{capFeature, featureCustomComponent, "custom components"})
	}
	return doors
}

// runComponentSnapshot is the run's component snapshot, erased rows included.
// Empty for a run launched without components, and on a store that keeps none.
func (s *Server) runComponentSnapshot(ctx context.Context, runID uuid.UUID) ([]types.RunComponent, error) {
	st, ok := s.cfg.Store.(store.ComponentStore)
	if !ok {
		return nil, nil
	}
	comps, err := st.ListRunComponents(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("read the run's components: %w", err)
	}
	return comps, nil
}

// componentGoneRefusal refuses a run that carried an organisation's component
// which no longer exists. Deleting a component withdraws it: its grant rows
// say nothing any more, so the capability re-check alone could read a deleted
// component's door as open, and the revived proxy would carry its hosts and
// the credential the organisation provided for it again. Asked of every
// caller, the owner's own admin session included.
func (s *Server) componentGoneRefusal(ctx context.Context, comps []types.RunComponent) (*ownerRefusal, error) {
	st, ok := s.cfg.Store.(store.ComponentStore)
	if !ok {
		return nil, nil
	}
	gone := &ownerRefusal{status: http.StatusConflict, reason: reasonOwnerComponentGone,
		msg: "a component this run was launched with no longer exists; start a new run"}
	for _, c := range comps {
		if c.SelfDefined {
			continue
		}
		// An organisation's row always names its component; one that does not
		// names nothing that could still exist.
		if c.ComponentID == nil {
			return gone, nil
		}
		_, err := st.GetComponent(ctx, *c.ComponentID, "")
		if errors.Is(err, store.ErrNotFound) {
			return gone, nil
		}
		if err != nil {
			return nil, fmt.Errorf("read the run's component: %w", err)
		}
	}
	return nil, nil
}

// ownerProviderRows is the git provider rows repos resolve to, as the
// run-create gate resolves them (denyUserWorkspaceProviders). None when the
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

// capAllowedForSub is capSeamAllowed for an owner known only by sub and
// userType. A store that cannot answer is an error, never an allow. A value
// restricted by "Available to" counts as enforced and only an allow naming it
// lets the owner in (capBatch.decide's step 3).
//
// userType is the run's stamp (AgentRun.UserType), the type the owner
// resolved as at create. The owner's current type is not re-checked, just as
// the captured governance profile is not re-resolved. A stamp naming a type
// that no longer exists refuses, failing closed as callerSubjects does; the
// built-in type always exists and is not read back. An empty stamp (a run
// created before migration 0080) counts no type rows.
func (s *Server) capAllowedForSub(ctx context.Context, sub, userType, kind, value string) (bool, error) {
	if s.cfg.Store == nil {
		return true, nil
	}
	if userType != "" && userType != types.UserTypeStandard {
		_, err := s.cfg.Store.GetUserType(ctx, userType)
		if errors.Is(err, store.ErrNotFound) {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("api: resolve user type %q: %w", userType, err)
		}
	}
	grants, err := s.cfg.Store.ListCapabilityGrants(ctx)
	if err != nil {
		return false, fmt.Errorf("api: resolve capability %q: %w", kind, err)
	}
	batch := s.newCapBatch(ctx)
	restricted, err := batch.isRestricted(ctx, kind, value)
	if err != nil {
		return false, err
	}
	sub = types.CanonicalUserSubject(sub)
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
			(!restricted || strings.TrimSpace(g.Value) == strings.TrimSpace(value)) &&
			(g.SubjectType == types.CapabilitySubjectAll || (g.SubjectType == types.CapabilitySubjectUser && g.Subject == sub) ||
				(g.SubjectType == types.CapabilitySubjectUserType && g.Subject == userType)) {
			allow = true
		}
	}
	if allow {
		return true, nil
	}
	enforced, err := batch.enforced(ctx, kind)
	if err != nil {
		return false, err
	}
	return !enforced && !restricted, nil
}

// ownerProfile is the run's captured profile, composed from its chain as it stands now, refusing
// when it no longer exists (its walls cannot be known), when it cannot be read (a base included:
// never a partial chain) and when a composition nothing satisfies leaves it with no valid policy. A
// run that captured none (an unassigned or super-admin owner) passes.
func (s *Server) ownerProfile(ctx context.Context, run types.AgentRun) (*ResolvedProfile, *ownerRefusal) {
	if run.GovernanceProfileID == nil {
		return nil, nil
	}
	p, err := s.resolveProfileByID(ctx, *run.GovernanceProfileID)
	if u, ok := isOverlayUnsatisfiable(err); ok {
		return nil, &ownerRefusal{status: http.StatusForbidden, reason: reasonGovernanceOverlayUnsatisfiable, msg: u.Error()}
	}
	if errors.Is(err, store.ErrNotFound) {
		return nil, &ownerRefusal{status: http.StatusConflict, reason: reasonOwnerProfileGone,
			msg: "the governance profile this run was created under no longer exists; start a new run"}
	}
	if err != nil {
		return nil, &ownerRefusal{status: http.StatusServiceUnavailable, reason: reasonOwnerProfileUnreadable,
			msg: "resolve the owner's governance profile: " + err.Error()}
	}
	return p, nil
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
			Shared     bool   `json:"shared"`
		}
		if err := json.Unmarshal(g.Spec.Scope, &scope); err != nil || scope.SecretName == "" {
			continue
		}
		// The check looks where the sink reads the grant, and names the
		// secret only where the name is the owner's own to know. A shared
		// grant is read from the operator's namespace alone, and what the
		// organisation's secret is called is the operator's. An owner_only
		// grant is read from the owner's namespace alone, and is theirs. Any
		// other grant on a person's run may be answered by the operator's
		// row, so its refusal names the host and not the secret.
		subject := runIdentitySubject(ctx, run.CreatedBy)
		namespaces := []string{subject, ""}
		gone := fmt.Sprintf("the credential this run injects for %s no longer exists; start a new run", in.Host)
		switch {
		case scope.Shared:
			namespaces = []string{""}
			gone = fmt.Sprintf("the credential your organisation provides for %s on this run no longer exists; ask your admin, then start a new run", in.Host)
		case g.Spec.OwnerOnly:
			namespaces = []string{grantReadOwner(subject, true, run.OperatorOwned)}
			fallthrough
		case run.OperatorOwned:
			gone = fmt.Sprintf("the credential this run injects for %s (secret %s) no longer exists; start a new run", in.Host, scope.SecretName)
		}
		present, err := s.secretPresentIn(ctx, namespaces, scope.SecretName)
		if err != nil {
			return nil, err
		}
		if !present {
			return &ownerRefusal{status: http.StatusConflict, reason: reasonOwnerModelCredentialErased, msg: gone}, nil
		}
		if sc == nil {
			got, err := s.cfg.Store.GetSiteConfig(ctx)
			if err != nil {
				return nil, fmt.Errorf("read site config: %w", err)
			}
			sc = &got
		}
		if name, off := integrationDisabledFor(*sc, scope.SecretName); off {
			return &ownerRefusal{status: http.StatusConflict, reason: reasonOwnerModelProviderDisabled, msg: fmt.Sprintf(
				"the integration %s that supplies this run's credential for %s is disabled; start a new run", name, in.Host)}, nil
		}
	}
	return nil, nil
}

// modelProviderRefusal refuses a run whose model provider has since been
// deleted or turned off, and a run that carries a credential its provider
// authored (a grant whose snapshot names the provider's UID) when that
// provider was re-created under the same id (a new UID). The injection sink
// would refuse the run's first model call (provider_changed), or a run
// dispatched before 0.8.2 would come back with no model credential at all;
// this answers before a revive replaces the proxy or an extension keeps the
// run alive.
func (s *Server) modelProviderRefusal(ctx context.Context, run types.AgentRun) (*ownerRefusal, error) {
	if run.ModelProviderID == "" {
		return nil, nil
	}
	grants, err := s.cfg.Store.ListGrantsByRun(ctx, run.ID)
	if err != nil {
		return nil, fmt.Errorf("read the run's grants: %w", err)
	}
	var uids []string
	for _, g := range grants {
		var scope struct {
			Snapshot providerGrantSnapshot `json:"snapshot"`
		}
		if json.Unmarshal(g.Spec.Scope, &scope) == nil && scope.Snapshot.ProviderUID != "" {
			uids = append(uids, scope.Snapshot.ProviderUID)
		}
	}
	sc, err := s.cfg.Store.GetSiteConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("read site config: %w", err)
	}
	p, found := modelProviderByID(sc.ModelProviders, run.ModelProviderID)
	switch {
	case !found || (len(uids) > 0 && !slices.Contains(uids, p.UID)):
		return &ownerRefusal{status: http.StatusConflict, reason: reasonOwnerModelProviderGone, msg: fmt.Sprintf(
			"the model provider %s this run was launched with no longer exists; start a new run", run.ModelProviderID)}, nil
	case p.Disabled:
		return &ownerRefusal{status: http.StatusConflict, reason: reasonOwnerModelProviderDisabled, msg: fmt.Sprintf(
			"the model provider %s this run was launched with is turned off; start a new run", run.ModelProviderID)}, nil
	}
	return nil, nil
}

// secretPresentIn reports whether name exists in any of namespaces ("" is the
// operator's). A store that cannot list is an error, never "present".
func (s *Server) secretPresentIn(ctx context.Context, namespaces []string, name string) (bool, error) {
	if s.cfg.Secrets == nil {
		return false, errors.New("no secret store configured")
	}
	for _, ns := range namespaces {
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
// upstream proxy, the trusted CA and the internal-host lift. The model
// upstreams are the run's own provider's (dispatch-time), so they stay. A site config that cannot be read refuses rather than
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
	if run.Placement != "" && run.Placement != types.PlacementRemote {
		return s.refreshLocalDeploymentConfig(ctx, run, sc, cfg)
	}
	cfg.UpstreamProxyURL = s.resolveRunUpstreamProxy(ctx, run.ID, sc, nil)
	cfg.UpstreamProxyNoProxy = sc.UpstreamProxyNoProxy
	cfg.TrustedCAPEM = s.cfg.TrustedCAPEM
	cfg.InternalHosts = slices.DeleteFunc(cfg.InternalHosts, func(h types.InternalHost) bool {
		return !slices.ContainsFunc(sc.InternalHosts, func(c types.InternalHost) bool {
			return c.HostSuffix == h.HostSuffix && slices.Equal(c.CIDRs, h.CIDRs)
		})
	})
	return nil
}

// reviveOwnerRecheck runs the re-checks above for a revive, before its claim,
// strips the model credentials no provider authored
// (stripRevivedModelInjections), and rebuilds cfg's deployment parts. The
// strip comes after the refusals that do not read cfg's injections, so a
// refused revive audits no drops, and before the credential re-check, which
// must not refuse over a credential the strip removes. A refusal is audited as
// run.revive denied; a check that cannot be answered refuses too.
func (s *Server) reviveOwnerRecheck(ctx context.Context, run types.AgentRun, cfg *proxy.Config, actorType types.ActorType, actor string) *reviveError {
	if run.Placement != "" && run.Placement != types.PlacementRemote {
		err := s.refreshDeploymentConfig(ctx, run, cfg)
		var denied *placement.Refusal
		if errors.As(err, &denied) {
			s.recordAudit(ctx, s.auditEvent(&run.ID, actorType, actor, "run.revive", run.ID.String(), "denied", mustJSON(map[string]any{"reason": denied.Reason})))
			return reviveRefused(denied.Reason.Status(), string(denied.Reason), denied.Error())
		}
		return reviveRefused(http.StatusServiceUnavailable, reasonReviveOwnerAuthorityUnreadable, "re-check local placement authority")
	}
	ref, err := s.ownerCapabilityRefusal(ctx, run, actor == run.CreatedBy, runRepos(run, cfg))
	if err == nil && ref == nil {
		ref, err = s.modelProviderRefusal(ctx, run)
	}
	if err == nil && ref == nil {
		if rerr := s.stripRevivedModelInjections(ctx, run, cfg); rerr != nil {
			return rerr
		}
		s.pruneUnpairedInterception(cfg)
		ref, err = s.modelCredentialRefusal(ctx, run, cfg)
	}
	if err == nil && ref == nil {
		err = s.refreshDeploymentConfig(ctx, run, cfg)
	}
	if err != nil {
		return reviveRefused(http.StatusServiceUnavailable, reasonReviveOwnerAuthorityUnreadable, "re-check the run owner's authority: "+err.Error())
	}
	if ref != nil {
		s.recordAudit(ctx, s.auditEvent(&run.ID, actorType, actor, "run.revive", run.ID.String(), "denied",
			mustJSON(map[string]any{"subject": run.CreatedBy, "reason": ref.reason})))
		return reviveRefused(ref.status, ref.reason, ref.msg)
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
// repos come from the run's stored proxy config where there is one
// (run_proxy_config.go); a run without one leaves the legacy repo field alone.
func (s *Server) extendRefusal(r *http.Request, run types.AgentRun) *ownerRefusal {
	ctx := r.Context()
	if _, ref := s.ownerProfile(ctx, run); ref != nil {
		return ref
	}
	var cfg *proxy.Config
	raw, err := s.loadRunProxyConfig(ctx, run.ID)
	switch {
	case errors.Is(err, store.ErrNotFound), errors.Is(err, errRunProxyConfigNotKept):
	case err != nil:
		return &ownerRefusal{status: http.StatusServiceUnavailable, reason: reasonOwnerUnverifiable,
			msg: "read the run's proxy config to re-check its owner's authority: " + err.Error()}
	default:
		if cfg, err = s.loadRenderedProxyConfig(raw); err != nil {
			return &ownerRefusal{status: http.StatusConflict, reason: reasonOwnerUnverifiable,
				msg: "the run's proxy config does not load: " + err.Error()}
		}
	}
	ref, err := s.ownerCapabilityRefusal(ctx, run, principalFromRequest(r) == run.CreatedBy, runRepos(run, cfg))
	if err == nil && ref == nil {
		ref, err = s.modelProviderRefusal(ctx, run)
	}
	if err != nil {
		return &ownerRefusal{status: http.StatusServiceUnavailable, reason: reasonOwnerUnverifiable,
			msg: "re-check the run owner's authority: " + err.Error()}
	}
	return ref
}
