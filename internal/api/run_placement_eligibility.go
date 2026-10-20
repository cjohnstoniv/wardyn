// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// requestPlacement is the placement a request names; an empty one is the
// organisation's own executor today (remotePlacement), until D-117's resolver
// fills it from the eligible set.
func requestPlacement(req createRunRequest) types.Placement {
	if remotePlacement(req.Placement) {
		return types.PlacementRemote
	}
	return req.Placement
}

// requestRunnerID is the runner a request names, or the zero UUID when it names
// none or names one that is not a UUID (which no registered runner can match).
func requestRunnerID(req createRunRequest) uuid.UUID {
	id, _ := uuid.Parse(req.RunnerID)
	return id
}

// deniedPlacements is the placement list the caller's governance profile
// denies: absent (nil) for a member with no assigned profile, and for a caller
// whose runs stand outside governance. A deny list, so absent means permitted.
func (s *Server) deniedPlacements(ctx context.Context, ceiling governanceCeiling) []types.Placement {
	if ceiling.Profile == nil || s.runUngoverned(ctx) {
		return nil
	}
	return ceiling.Limits.DenyPlacements
}

// placementDeniedRefusal is the deny_placements door: 403 placement_denied
// naming the profile, with the authz.denied row a governance door writes. The
// ONE check: the fold step and every launcher that creates a run for the acting
// member (source scan) ask it with the placement they put the run on.
func (s *Server) placementDeniedRefusal(ctx context.Context, p types.Placement, ceiling governanceCeiling) *runRefusal {
	if !slices.Contains(s.deniedPlacements(ctx, ceiling), p) {
		return nil
	}
	d := authz.Deny(authz.ReasonGovernanceProfile, "runs.placement", placementDeniedMsg(p, ceiling.Profile.Name)).WithPolicy(s.ceilingPolicy(ctx, ceiling))
	f := runError(placement.ReasonPlacementDenied.Status(), string(placement.ReasonPlacementDenied), d.Sentence)
	f.decision = &d
	return f
}

// placementDeniedError carries the refusal out of a launcher that has no
// request to answer, so its handler writes the same 403 and audit row the doors do.
type placementDeniedError struct{ refusal *runRefusal }

func (e *placementDeniedError) Error() string { return e.refusal.body.Error }

// placementDeniedMsg is the door's member sentence (MEMBER.DENIED_PLACEMENT_*), composed
// once so the 403 body and the audit row carry the same bytes.
func placementDeniedMsg(p types.Placement, profile string) string {
	if p == types.PlacementLocal {
		return fmt.Sprintf("placement local is not allowed by your governance profile %q — launch with placement remote, or leave placement empty", profile)
	}
	return fmt.Sprintf("placement remote is not allowed by your governance profile %q — launch on your own runner with placement local", profile)
}

// localPlacementRefusal is the create-time answer for a local request: the
// admitted plan's own refusal, else nil for a pooled run, which create binds to
// one of its eligible runners (runPlacement.apply), with the delivery policy and
// trust class the plan was judged at. The plan is judged at each of trusts (the
// eligible runners' classes, candidateTrusts), highest first, and the first that
// admits it is the run's. Dispatch classifies the actual spec again
// (classifyLocalDispatch) for what late authoring adds. An unpooled local
// request has no runner to go to and is refused.
func (s *Server) localPlacementRefusal(r *http.Request, req createRunRequest, pooled bool, trusts []types.RunnerTrustClass,
	spec types.RunPolicySpec, ceiling governanceCeiling, comps runComponents,
	drives []types.DriveMount, workspaces []types.Workspace, baseline composer.Baseline,
) (localJudgement, *runRefusal) {
	if req.Placement != placement.Local {
		return localJudgement{}, nil
	}
	p, refusal := s.localAdmittedPlan(r, req, spec, ceiling, comps, drives, workspaces)
	if refusal != nil {
		return localJudgement{}, refusal
	}
	if err := s.localPlanFacts(r.Context(), &p, baseline, nil); err != nil {
		return localJudgement{}, runServerError("read the local placement facts", err)
	}
	if trusts == nil {
		trusts = placement.TrustClassesByRank()
	}
	trust, ref := judgeAtTrust(p, trusts)
	if ref != nil {
		return localJudgement{}, s.localEligibilityRefusal(r, ceiling, ref)
	}
	if pooled {
		return localJudgement{policy: p.Delivery, trust: trust, floor: p.ConfinementFloor}, nil
	}
	return localJudgement{}, runError(placement.ReasonPlacementUnavailable.Status(), string(placement.ReasonPlacementUnavailable), localUnpooledMsg)
}

// remoteInsteadMsg follows a local refusal of a run its pool put on the person's own runner while a
// remote-hosted runner in the pool could also take it: the pool never moves the run there by itself.
func remoteInsteadMsg(pool string) string {
	return "To run it on a remote-hosted runner in " + pool + " instead, launch it with placement remote."
}

// localUnpooledMsg is the refusal of a local run that no pool of the person's own places: a run reaches a
// person's own runner only through one of their own pools.
const localUnpooledMsg = "Your own runner is not available to this run: a local run goes to a pool of your own runners. Choose one, or create one in My pools and add your runner to it."

// localPlanFacts fills what every LocalEligibility door (create, dispatch and
// revive) judges a runner_resident credential on: the stored delivery policy,
// the bound runner's reported posture and stored trust class, and the run's
// blast-radius floor (Q7). Before a runner is bound (create) Posture and Trust
// stay unset (create sets Trust per candidate class); a store that keeps no
// policy leaves every class refused, and a bound runner whose trust class is
// unknown refuses.
func (s *Server) localPlanFacts(ctx context.Context, p *placement.LocalPlan, baseline composer.Baseline, runnerID *uuid.UUID) error {
	p.ConfinementFloor = composer.RequiredConfinementFloor(p.Spec.ProxyConfig.Policy, baseline)
	policy, err := s.storedDeliveryPolicy(ctx)
	if err != nil {
		return err
	}
	p.Delivery = policy
	st, ok := s.cfg.Store.(interface {
		GetRunner(context.Context, uuid.UUID) (types.Runner, error)
	})
	if !ok || runnerID == nil {
		return nil
	}
	rn, err := st.GetRunner(ctx, *runnerID)
	if err != nil {
		return err
	}
	if rn.PostureReportedAt != nil {
		posture := rn.Posture
		p.Posture = &posture
	}
	if !rn.TrustClass.Valid() {
		return fmt.Errorf("runner %s has no known trust class", rn.ID)
	}
	p.Trust = rn.TrustClass
	return nil
}

// localAdmittedPlan builds the plan a local candidate is judged on from the
// admitted create-time inputs: the policy, components, drive, mounts and the
// provider/ADO/redirect intents, with each grant's provenance resolved against
// the owner's own namespace. It selects nothing and refuses nothing about
// routing; D-117 adds per-candidate verified paths and capabilities to it.
func (s *Server) localAdmittedPlan(r *http.Request, req createRunRequest,
	spec types.RunPolicySpec, ceiling governanceCeiling, comps runComponents,
	drives []types.DriveMount, workspaces []types.Workspace,
) (placement.LocalPlan, *runRefusal) {
	if fields := localDispatchUnclassified(); len(fields) != 0 {
		return placement.LocalPlan{}, runError(placement.ReasonPlacementCredential.Status(), string(placement.ReasonPlacementCredential), fields[0]+": unclassified dispatch metadata")
	}
	var sc types.SiteConfig
	if s.cfg.Store != nil {
		var err error
		sc, err = s.cfg.Store.GetSiteConfig(r.Context())
		if err != nil {
			return placement.LocalPlan{}, runServerError("read placement dispatch configuration", err)
		}
	}
	p := placement.LocalPlan{
		Spec:    runner.SandboxSpec{Image: req.Image, Drives: drives, Mounts: buildRunMounts(spec, s.userMountPosture(workspaces)), ProxyConfig: runner.ProxyConfig{Policy: spec.Clone()}},
		Origins: map[string]placement.CredentialOrigin{}, UpstreamProxySecretRef: sc.UpstreamProxySecretRef,
		LocalSelfDefinedComponents: ceiling.Limits.LocalSelfDefinedComponents,
		TrustedOutput:              localTrustedOutputRequest(req),
	}
	for _, c := range comps.attached {
		if !c.snapshot.SelfDefined && c.snapshot.Definition.LocalMCP() {
			return placement.LocalPlan{}, runError(placement.ReasonPlacementCredential.Status(), string(placement.ReasonPlacementCredential), localMCPOrgServerMsg)
		}
		if c.snapshot.SelfDefined {
			name := c.snapshot.Name
			if name == "" {
				name = "components[" + strconv.Itoa(len(p.SelfDefinedComponents)) + "]"
			}
			p.SelfDefinedComponents = append(p.SelfDefinedComponents, name)
		}
	}
	for i, g := range spec.EligibleGrants {
		origin, ref := s.localGrantOrigin(r.Context(), localRequestOwner(r), g)
		if ref != nil {
			return placement.LocalPlan{}, ref
		}
		if origin.Class != placement.ClassOwn && mcpHeaderGrant(g, comps.mcpHosts) {
			return placement.LocalPlan{}, runError(placement.ReasonPlacementCredential.Status(), string(placement.ReasonPlacementCredential), localMCPSharedCredentialMsg)
		}
		p.Origins["ProxyConfig.Policy.EligibleGrants["+strconv.Itoa(i)+"]"] = origin
		if origin.Class == placement.ClassOwn && origin.Stored {
			p.Spec.ProxyConfig.Policy.EligibleGrants[i].OwnerOnly = true
		}
	}
	intents, upstreams, refusal := s.localCredentialIntents(r, req, spec, comps, workspaces, sc)
	if refusal != nil {
		return placement.LocalPlan{}, refusal
	}
	p.CredentialIntents, p.Spec.ProxyConfig.LLMUpstreams = intents, upstreams
	return p, nil
}

// localMCPSharedCredentialMsg refuses a local run whose MCP server carries a credential the organisation
// holds. The MCP broker resolves its server's credential itself, and on a runner that resolve rides the
// relay, which serves only a grant delivered own (refuseRelayedDelivery); via_org forwards requests
// whose header the organisation injects, and the broker's tool rules would not run there.
const localMCPSharedCredentialMsg = "components: an MCP server's credential that your organisation shares can't reach your own runner; use a secret of your own for it, or run this on a remote-hosted runner"

// localMCPOrgServerMsg refuses a local run with an MCP server that runs in the sandbox from a row the
// organisation defines: its launch command and args are the organisation's, which may hold what its
// stripped config values would, and they reach the runner unjudged in the harness's MCP configuration.
const localMCPOrgServerMsg = "components: your organisation's MCP servers that run in the sandbox can't run on your own runner; define one of your own, or run this on a remote-hosted runner"

// mcpHeaderGrant reports whether g is the header credential of one of the run's MCP servers.
func mcpHeaderGrant(g types.GrantSpec, mcpHosts []string) bool {
	if g.Kind != types.GrantAPIKey {
		return false
	}
	rule, err := injectionRuleFromScope(g.Scope)
	return err == nil && slices.ContainsFunc(mcpHosts, func(h string) bool { return hostEqual(h, rule.Host) })
}

// boundLocalPath is a local path and the runner it was registered for.
type boundLocalPath struct {
	Runner uuid.UUID
	Path   string
}

// verifiedLocalPaths is the plan's VerifiedLocalPaths: a path is verified only
// when its stored runner binding is the candidate runner and the path lies
// inside one of that runner's allowed roots. bound is keyed by the plan path
// ("SandboxSpec.Mounts[0]", "SandboxSpec.Drives[0]"). Nothing produces a binding on
// batch yet (local_dir sources and host_path drives carry no runner column), so
// every local mount still refuses placement_local_path until one does.
func verifiedLocalPaths(candidate uuid.UUID, roots []string, bound map[string]boundLocalPath) map[string]bool {
	out := map[string]bool{}
	for key, b := range bound {
		if candidate != uuid.Nil && b.Runner == candidate && withinAnyRoot(b.Path, roots) {
			out[key] = true
		}
	}
	return out
}

// withinAnyRoot reports whether path, cleaned, is a root or below one. Both must
// be absolute: a relative path is never inside anything. Symlinks are the
// runner's own device-side check at bind.
func withinAnyRoot(path string, roots []string) bool {
	path = filepath.Clean(path)
	return filepath.IsAbs(path) && slices.ContainsFunc(roots, func(root string) bool {
		root = filepath.Clean(root)
		return filepath.IsAbs(root) && (path == root || strings.HasPrefix(path, strings.TrimSuffix(root, "/")+"/"))
	})
}

// memberDriveNeverOnPool is a person's own drive on a pool that has only
// people's own runners: never placed there, permanently. Its volume or claim
// would live on that person's machine, which deleting the drive cannot reach.
// No later proof a pool's runners give about local paths (verifiedLocalPaths)
// relaxes it.
func memberDriveNeverOnPool(pool store.RunnerPoolCatalogueRow, d types.UserDrive) bool {
	return d.OwnerSubject != "" && pool.Pool.Scope == types.RunnerPoolUser && len(pool.ExecutorIDs) == 0
}

func localTrustedOutputRequest(req createRunRequest) bool {
	switch req.Task {
	case harnessLoginTask, "workspace record", "workspace verify":
		return true
	default:
		return false
	}
}

// localGrantOrigin proves the owner's actual namespace before any grant write.
// Shared component scopes remain operator material even if the owner happens
// to store a colliding name. No secret value or fallback Get is used as proof.
func (s *Server) localGrantOrigin(ctx context.Context, owner string, g types.GrantSpec) (placement.CredentialOrigin, *runRefusal) {
	origin := placement.CredentialOrigin{GrantKind: g.Kind}
	switch g.Kind {
	case types.GrantGitHubToken:
		origin.Class, origin.Delivery = placement.ClassBrokered, placement.ClassGitHubToken
		return origin, nil
	case types.GrantCloudSTS:
		origin.Class, origin.Delivery = placement.ClassBrokered, placement.ClassCloudSTS
		return origin, nil
	}
	if captured, handled, err := s.localCapturedGrantOrigin(ctx, owner, g); handled {
		if err != nil {
			return origin, runError(placement.ReasonPlacementCredential.Status(), string(placement.ReasonPlacementCredential), "eligible_grants: captured credential provenance cannot be resolved")
		}
		return captured, nil
	}
	_, name, knownHosts, covered, err := storedSecretGrantPairing(g)
	if err != nil || !covered {
		return origin, runError(placement.ReasonPlacementCredential.Status(), string(placement.ReasonPlacementCredential), fmt.Sprintf("eligible_grants: %s credential provenance cannot be resolved", g.Kind))
	}
	origin.Stored = true
	own := owner != "" && s.ownsSecretMemoized(ctx, owner, name) && (knownHosts == "" || s.ownsSecretMemoized(ctx, owner, knownHosts))
	if g.Kind == types.GrantAPIKey && apiKeyScopeShared(g.Scope) {
		own = false
	}
	origin.OwnNamespace, origin.OwnerOnly = own, own
	if own {
		origin.Class = placement.ClassOwn
	} else {
		origin.Class = placement.ClassOperator
	}
	switch g.Kind {
	case types.GrantAPIKey:
		origin.Delivery = placement.ClassAPIKey
	case types.GrantEnvSecret:
		origin.Delivery = placement.ClassEnvSecret
	case types.GrantFileSecret:
		origin.Delivery = placement.ClassFileSecret
	case types.GrantSSHKey:
		origin.Delivery = placement.ClassSSHKey
	case types.GrantGitPAT:
		origin.Delivery = placement.ClassGitPATHelper
	default:
		return origin, runError(placement.ReasonPlacementCredential.Status(), string(placement.ReasonPlacementCredential), "eligible_grants: unclassified credential kind")
	}
	return origin, nil
}

func (s *Server) localEligibilityRefusal(r *http.Request, ceiling governanceCeiling, ref *placement.Refusal) *runRefusal {
	if ref.Reason == placement.ReasonPlacementComponentSelfDefine {
		d := authz.Deny(authz.ReasonPlacementComponentSelfDefined, "runs.placement", "local_self_defined_components is not permitted by the profile").WithPolicy(s.ceilingPolicy(r.Context(), ceiling))
		f := runError(ref.Reason.Status(), string(ref.Reason), ref.Error())
		f.decision = &d
		return f
	}
	return runError(ref.Reason.Status(), string(ref.Reason), ref.Error())
}

// localGrantDelivery is how one grant of a local run is delivered: its OD-12
// class key and mode (own or via_org).
type localGrantDelivery struct {
	class string
	mode  placement.Mode
}

// ownLocalGrantSpecs verifies the entire set before the first write. Local own
// grant persistence never depends on a caller remembering to stamp OwnerOnly.
// An org-held grant is admitted only when the delivery policy the run's plan was
// judged on, at the trust class it was admitted at, delivers its class via_org
// and its kind has the via_org shape, or delivers it runner_resident and the
// table admits it (residentGrantRefusal adds the grant's own bounds); it stays
// as authored (never OwnerOnly) and is recorded with that mode.
func (s *Server) ownLocalGrantSpecs(r *http.Request, spec types.RunPolicySpec, j localJudgement) (types.RunPolicySpec, []localGrantDelivery, *runRefusal) {
	spec = spec.Clone()
	out := make([]localGrantDelivery, len(spec.EligibleGrants))
	for i, g := range spec.EligibleGrants {
		origin, ref := s.localGrantOrigin(r.Context(), localRequestOwner(r), g)
		if ref != nil {
			return types.RunPolicySpec{}, nil, ref
		}
		if placement.ViaOrgGrant(origin, j.policy, j.trust) {
			out[i] = localGrantDelivery{class: origin.Delivery, mode: placement.ModeViaOrg}
			continue
		}
		resident, rref := placement.ResidentGrant(origin, j.policy, j.trust, j.floor)
		if rref != nil {
			return types.RunPolicySpec{}, nil, runError(rref.Reason.Status(), string(rref.Reason), fmt.Sprintf("eligible_grants[%d]: %s", i, rref.Error()))
		}
		if resident {
			if detail := s.residentGrantRefusal(r.Context(), g); detail != "" {
				return types.RunPolicySpec{}, nil, runError(placement.ReasonPlacementCredential.Status(), string(placement.ReasonPlacementCredential), fmt.Sprintf("eligible_grants[%d]: %s", i, detail))
			}
			out[i] = localGrantDelivery{class: origin.Delivery, mode: placement.ModeRunnerResident}
			continue
		}
		if origin.Class != placement.ClassOwn || !origin.Stored || !origin.OwnNamespace {
			return types.RunPolicySpec{}, nil, runError(placement.ReasonPlacementCredential.Status(), string(placement.ReasonPlacementCredential), fmt.Sprintf("eligible_grants[%d]: credential is not available in its owner's namespace", i))
		}
		spec.EligibleGrants[i].OwnerOnly = true
		out[i] = localGrantDelivery{class: origin.Delivery, mode: placement.ModeOwn}
	}
	return spec, out, nil
}

func localRequestOwner(r *http.Request) string {
	_, actor := actorFromRequest(r)
	return runIdentitySubject(r.Context(), actor)
}
