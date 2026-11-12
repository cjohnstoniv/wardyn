// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/ghscope"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The source-control capability cap: the Azure DevOps and GitHub capabilities the Wardyn boundary in
// hard_limits, and a restriction a run captured, let that run hold. It is an allowlist and fails closed:
// a capability a later release adds is outside every cap until someone lists it. Absent is no cap.
//
// A cap is not an overlay knob. A list meet only narrows a list a run names, and a run on a provider's
// default profile names none, so the cap is met with the provider row's ceiling and with the lists a
// dispatch assembles instead, at the doors below.

// validateVCSCaps refuses a cap that lists nothing, or a capability that cannot be granted. The list is
// stored as written, pr included.
func validateVCSCaps(ado *[]adoscope.Capability, gh *[]ghscope.Capability) error {
	if ado != nil {
		if err := validateCapList("azure_devops_capabilities", *ado, adoscope.GrantableCapabilityList()); err != nil {
			return err
		}
	}
	if gh != nil {
		return validateCapList("github_capabilities", *gh, ghscope.GrantableCapabilityList())
	}
	return nil
}

func validateCapList[C interface {
	~string
	Grantable() bool
}](field string, caps []C, grantable string,
) error {
	if len(caps) == 0 {
		return fmt.Errorf("%s: an empty cap allows no capability; omit the field for no cap", field)
	}
	for i, c := range caps {
		if !c.Grantable() {
			return fmt.Errorf("%s[%d]: %q is not a grantable capability; want one of: %s", field, i, string(c), grantable)
		}
	}
	return nil
}

// capOf is the capabilities every layer that names a list lets a run hold (pr expanded), in the first
// naming layer's order, or nil when no layer names one.
func capOf[C comparable](layers []sealLayer, of func(sealLayer) *[]C, expandCaps func([]C) []C) *[]C {
	out, named := []C{}, false
	for _, l := range layers {
		p := of(l)
		if p == nil {
			continue
		}
		list := expandCaps(*p)
		if !named {
			out, named = slices.Clone(list), true
			continue
		}
		out = slices.DeleteFunc(out, func(c C) bool { return !slices.Contains(list, c) })
	}
	if !named {
		return nil
	}
	return &out
}

// adoCapOf is the Azure DevOps cap the run's outer layers set: the capabilities every layer that names a
// list allows (pr expanded), or nil when no layer names one.
func adoCapOf(layers []sealLayer) *[]adoscope.Capability {
	return capOf(layers, func(l sealLayer) *[]adoscope.Capability { return l.adoCap }, adoscope.ExpandPR)
}

func githubCapOf(layers []sealLayer) *[]ghscope.Capability {
	return capOf(layers, func(l sealLayer) *[]ghscope.Capability { return l.githubCap }, ghscope.ExpandPR)
}

// withinADOCap is caps less every capability cap leaves out, in caps' order (pr expanded); caps itself when
// cap is nil.
func withinADOCap(caps []adoscope.Capability, cap *[]adoscope.Capability) []adoscope.Capability {
	if cap == nil {
		return caps
	}
	var out []adoscope.Capability
	for _, c := range adoscope.ExpandPR(caps) {
		if adoscope.Has(*cap, c) {
			out = append(out, c)
		}
	}
	return out
}

func withinGitHubCap(caps []ghscope.Capability, cap *[]ghscope.Capability) []ghscope.Capability {
	if cap == nil {
		return caps
	}
	var out []ghscope.Capability
	for _, c := range ghscope.ExpandPR(caps) {
		if ghscope.Has(*cap, c) {
			out = append(out, c)
		}
	}
	return out
}

// effectiveADOCeiling is the provider row's ceiling (ADOEntraConfig.Ceiling, pr expanded) less every
// capability cap leaves out: what a run on that row may hold. With a nil cap it is the row's ceiling.
func effectiveADOCeiling(row *types.ADOEntraConfig, cap *[]adoscope.Capability) []adoscope.Capability {
	return withinADOCap(row.Ceiling(), cap)
}

// capLacks reports that cap is set and does not list c (cap is a capOf result, so pr is already expanded).
func capLacks[C comparable](cap *[]C, c C) bool {
	return cap != nil && !slices.Contains(*cap, c)
}

// runADOCap reads the run and its outer layers now; an error is the run's row unread. A boundary or a
// restriction that cannot be read is an error here, so a resolve is retried rather than answered from a guess;
// a dispatch reads the same layer as a cap that lists nothing (adoCapOf), never no cap.
func (s *Server) runADOCap(ctx context.Context, runID uuid.UUID) (*[]adoscope.Capability, error) {
	if s.cfg.Store == nil {
		return nil, errors.New("api: no store to read the run from")
	}
	run, err := s.cfg.Store.GetRun(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("api: read the run for its Azure DevOps cap: %w", err)
	}
	layers := s.outerLayers(ctx, run)
	if slices.ContainsFunc(layers, func(l sealLayer) bool { return l.unread }) {
		return nil, errors.New("api: the run's boundary or restrictions could not be read")
	}
	return adoCapOf(layers), nil
}

const (
	adoCapAtResolveRefusal   = "Wardyn stopped this run's Azure DevOps access: the Wardyn boundary or a restriction no longer allows %s. Start a new run."
	adoCapRunUnreadableBody  = "This run, the Wardyn boundary or its restrictions could not be read, so its Azure DevOps access cannot be checked; try again"
	adoCapLaneNothingDetail  = "Can't launch: the Wardyn boundary or a restriction allows none of this run's Azure DevOps capabilities. Ask a security admin to change the cap."
	adoCapLanePolicyDetail   = "Can't launch with this policy. It asks for %s on Azure DevOps, which the Wardyn boundary or a restriction does not allow. Ask a security admin to change the cap."
	githubCapLaneNothingBody = "The Wardyn boundary or a restriction allows none of the GitHub CLI's capabilities, so the GitHub CLI would hold none. " +
		"Launch without the GitHub CLI, or ask a security admin to change the cap."
)

// adoShortLabels names capabilities by their console names, quoted, for a sentence.
func adoShortLabels(caps []adoscope.Capability) string {
	names := make([]string, len(caps))
	for i, c := range caps {
		names[i] = "“" + adoscope.ShortLabel(c) + "”"
	}
	if n := len(names); n > 1 {
		return strings.Join(names[:n-1], ", ") + " and " + names[n-1]
	}
	return strings.Join(names, "")
}

// capADOLane bounds the run's Azure DevOps lane by the cap its outer layers set, after the policy, the
// walls and the standing grants have each had their say: an operator's pick and a member's governance
// standing are not bounded by the row's profile, so only a cap applied last binds them. A list the run's
// policy names that goes past the cap refuses, as one past the ceiling does; a default profile, the walls'
// meet or a standing grant is cut to the cap, and the policy records the bounded list. A cap that leaves
// the lane nothing refuses.
func (s *Server) capADOLane(ctx context.Context, run types.AgentRun, ado adoEntraRun, policy *types.RunPolicySpec) (adoEntraRun, *wallsCapabilityRefusal) {
	cap := adoCapOf(s.outerLayers(ctx, run))
	if cap == nil {
		return ado, nil
	}
	refuse := func(detail string) (adoEntraRun, *wallsCapabilityRefusal) {
		return ado, &wallsCapabilityRefusal{status: http.StatusForbidden, reason: reasonADOCapabilitiesCapped, detail: detail}
	}
	ado.ceiling = withinADOCap(ado.ceiling, cap)
	kept := withinADOCap(ado.caps, cap)
	outside := adoCapsOutside(adoscope.ExpandPR(ado.caps), *cap)
	if ado.capsFromPolicy && len(outside) > 0 {
		return refuse(fmt.Sprintf(adoCapLanePolicyDetail, adoShortLabels(outside)))
	}
	if len(kept) == 0 {
		return refuse(adoCapLaneNothingDetail)
	}
	if len(outside) > 0 {
		ado.caps, policy.AzureDevOpsCapabilities = kept, slices.Clone(kept)
	}
	return ado, nil
}

// adoCapAtResolve is a credential resolve's check against the cap: 503 run_unreadable when the run cannot
// be read, 403 capability_capped when a capability the grant's snapshot holds is outside the cap, else
// status 0 and the cap, for the ask that may follow. A cap that leaves a held capability out ends the run's
// Azure DevOps access at its next resolve, as a narrowed provider ceiling does.
func (s *Server) adoCapAtResolve(ctx context.Context, runID uuid.UUID, sn adoEntraScopeSnapshot) (cap *[]adoscope.Capability, status int, reason, body string) {
	cap, err := s.runADOCap(ctx, runID)
	if err != nil {
		slog.ErrorContext(ctx, "wardynd: read the run for its Azure DevOps cap; the resolve is refused",
			slog.String("run_id", runID.String()), slog.Any("err", err))
		return nil, http.StatusServiceUnavailable, reasonRunUnreadable, adoCapRunUnreadableBody
	}
	if cap == nil {
		return nil, 0, "", ""
	}
	if outside := adoCapsOutside(adoscope.ExpandPR(sn.Capabilities), *cap); len(outside) > 0 {
		return cap, http.StatusForbidden, reasonCapabilityCapped, fmt.Sprintf(adoCapAtResolveRefusal, adoShortLabels(outside))
	}
	return cap, 0, "", ""
}

// cappedGitHubCLICapabilities is the GitHub CLI's capabilities under the cap the run's outer layers set. A
// cap that leaves the CLI none refuses the launch. With no cap caps is returned as it is.
func (s *Server) cappedGitHubCLICapabilities(ctx context.Context, run types.AgentRun, caps []ghscope.Capability) ([]ghscope.Capability, *runRefusal) {
	cap := githubCapOf(s.outerLayers(ctx, run))
	if cap == nil {
		return caps, nil
	}
	if caps = withinGitHubCap(caps, cap); len(caps) == 0 {
		return nil, runError(http.StatusForbidden, reasonGitHubCapabilitiesCapped, githubCapLaneNothingBody)
	}
	return caps, nil
}

// boundaryProviderCap is one Azure DevOps provider row the drafted cap narrows: the ceiling and default
// profile entries it would remove, pr expanded.
type boundaryProviderCap struct {
	ID                    string                `json:"id"`
	CeilingRemoved        []adoscope.Capability `json:"ceiling_removed"`
	DefaultProfileRemoved []adoscope.Capability `json:"default_profile_removed,omitempty"`
}

// boundaryProviderRows lists, per git provider row with an Entra block, what the drafted boundary's Azure
// DevOps cap removes from its ceiling and its default profile. Rows that lose nothing are left out, and a
// boundary with no cap lists none. GitHub's lists live on components and policies, not rows, so the dry run
// does not list them.
func (s *Server) boundaryProviderRows(ctx context.Context, b types.Boundary) ([]boundaryProviderCap, error) {
	out := []boundaryProviderCap{}
	if b.Spec.AzureDevOpsCapabilities == nil || s.cfg.Store == nil {
		return out, nil
	}
	site, err := s.cfg.Store.GetSiteConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("read the provider rows: %w", err)
	}
	if site.WorkspaceProviders == nil {
		return out, nil
	}
	expanded := adoscope.ExpandPR(*b.Spec.AzureDevOpsCapabilities)
	cap := &expanded
	for _, row := range site.WorkspaceProviders.Git {
		if row.Entra == nil {
			continue
		}
		capped := effectiveADOCeiling(row.Entra, cap)
		removed, profileRemoved := adoCapsOutside(row.Entra.Ceiling(), capped), adoCapsOutside(row.Entra.Profile(), capped)
		if len(removed) > 0 || len(profileRemoved) > 0 {
			if removed == nil {
				removed = []adoscope.Capability{}
			}
			out = append(out, boundaryProviderCap{ID: row.ID, CeilingRemoved: removed, DefaultProfileRemoved: profileRemoved})
		}
	}
	return out, nil
}

// vcsCapsAtLeastAsStrict reports that next keeps every cap old sets and lists no capability old's leaves
// out. Dropping a cap, or widening one, loosens the restriction, so it is never exempt from a second human.
func vcsCapsAtLeastAsStrict(old, next types.RestrictionSpec) bool {
	return capNoWider(old.AzureDevOpsCapabilities, next.AzureDevOpsCapabilities, adoscope.ExpandPR) &&
		capNoWider(old.GitHubCapabilities, next.GitHubCapabilities, ghscope.ExpandPR)
}

func capNoWider[C comparable](old, next *[]C, expand func([]C) []C) bool {
	if old == nil {
		return true
	}
	if next == nil {
		return false
	}
	have := expand(*old)
	return !slices.ContainsFunc(expand(*next), func(c C) bool { return !slices.Contains(have, c) })
}
