// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"path"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerpool"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// The New Run request contract (0.9): placement, resources, image choice,
// workspace targets, overrides and built-in components. stepRunContract runs first at every door
// and does two things with each field, in order:
//
//  1. validates its shape, with its final refusal reason, so a malformed value
//     answers the same way before and after the behaviour lands;
//  2. refuses, with request_field_unavailable, a field that changes the run's
//     posture and that nothing applies yet. It never drops one: a person who
//     asked to narrow a run must not be handed a wider one with a 201.
//
// Fields that only tune a run (resources, a workspace target) are accepted and
// not yet applied; their doc comments on pkg/client say so. Each behaviour lane
// deletes its case in unappliedFieldsRefusal and adds its own step to
// runFoldSteps:
//
//	overrides                     -> A-L3
//	built-in components           -> A-L14
//	allowed_image, image facts     -> the image lane (A-L4/A-L6)
//	placement local               -> the placement lane (#117)
//	runner_pool_id                -> the runner pool lane: resolve through runnerpool.Resolve and
//	                                 admit the exact target; an omitted pool stays accepted until then
//	resources clamp, targets      -> A-L5, A-L4 (accepted, applied nowhere yet)
const (
	maxRunOverrideEntries  = 64
	maxRunOverrideValueLen = 256
	maxRunResourceCPU      = 1_000_000 // milli-CPU: a thousand CPUs
	maxRunResourceMemory   = 16 << 20  // MiB: 16 TiB
	runUserHome            = "/home/agent"
	runDriveName           = "your drive"
)

// runContractRefusal answers the first refusal of the contract fields, shape
// first. A method on Server for the reason validateRunTextFields is: the parity
// guard reads gates as `s.<Gate>(…)` calls.
func (s *Server) runContractRefusal(req createRunRequest) *runRefusal {
	for _, check := range []func(createRunRequest) *runRefusal{
		placementShapeRefusal, runnerPoolShapeRefusal, resourcesRefusal, allowedImageRefusal, workspaceTargetsRefusal, runOverridesRefusal, builtinComponentsRefusal,
	} {
		if refusal := check(req); refusal != nil {
			return refusal
		}
	}
	return unappliedFieldsRefusal(req)
}

func placementShapeRefusal(req createRunRequest) *runRefusal {
	if err := req.PlacementRequest().Validate(); err != nil {
		return runError(http.StatusBadRequest, reasonPlacementInvalid, err.Error())
	}
	return nil
}

// runnerPoolShapeRefusal checks runner_pool_id's shape: a pool id. Whether the
// pool exists and the caller may use it is the pool lane's, and answers
// runner_pool_not_found either way.
func runnerPoolShapeRefusal(req createRunRequest) *runRefusal {
	if req.RunnerPoolID == "" {
		return nil
	}
	if id, err := uuid.Parse(req.RunnerPoolID); err != nil || id == uuid.Nil {
		return runError(runnerpool.ReasonInvalid.Status(), string(runnerpool.ReasonInvalid), runnerpool.InvalidMsg())
	}
	return nil
}

// allowedImageRefusal checks allowed_image's shape: a bare image ref, and never
// beside a request-built image (image, devcontainer_repo). Whether the ref is
// one the organisation allows is the image lane's.
func allowedImageRefusal(req createRunRequest) *runRefusal {
	ref := req.AllowedImage
	switch {
	case ref == "":
		return nil
	case len(ref) > maxRunOverrideValueLen || strings.ContainsAny(ref, " \t\r\n") || !controlCharFree(ref):
		return runError(http.StatusBadRequest, reasonAllowedImageInvalid, "allowed_image: give one image ref from the images your organisation allows")
	case req.Image != "" || req.DevcontainerRepo != "":
		return runError(http.StatusBadRequest, reasonAllowedImageInvalid, "allowed_image cannot be combined with image or devcontainer_repo")
	}
	return nil
}

func resourcesRefusal(req createRunRequest) *runRefusal {
	r := req.Resources
	if r == nil {
		return nil
	}
	if r.CPUMillis < 0 || r.CPUMillis > maxRunResourceCPU || r.MemoryMiB < 0 || r.MemoryMiB > maxRunResourceMemory {
		return runError(http.StatusBadRequest, reasonResourcesInvalid, fmt.Sprintf(
			"resources: cpu_millis must be 0 to %d and memory_mib 0 to %d", maxRunResourceCPU, maxRunResourceMemory))
	}
	return nil
}

// workspaceTargetShape is the rule a workspace selection's target must meet:
// absolute, already cleaned, no ".." element, strictly under /home/agent, and
// past the authored-target deny-list every in-sandbox path goes through.
func workspaceTargetShape(target string) error {
	if !path.IsAbs(target) || slices.Contains(strings.Split(target, "/"), "..") || path.Clean(target) != target ||
		!strings.HasPrefix(target, runUserHome+"/") {
		return errors.New(workspaceTargetShapeMsg(target))
	}
	return runner.ValidateAuthoredTarget(target)
}

// pathWithin reports whether inner is outer or lies under it.
func pathWithin(inner, outer string) bool {
	return inner == outer || strings.HasPrefix(inner, outer+"/")
}

// workspaceTargetsRefusal validates each selection's target and refuses equal or
// nested targets across different workspaces (OD-3), a drive's included. Only the
// targets the request names are compared: the multi-workspace lane widens the
// check to every resolved default.
func workspaceTargetsRefusal(req createRunRequest) *runRefusal {
	type placed struct{ id, target string }
	var seen []placed
	drive := req.Drive != nil && req.Drive.Enabled
	for i, sel := range req.Workspaces {
		t := sel.Target
		if t == "" {
			continue
		}
		if drive && pathWithin(t, runner.DriveTarget) && path.IsAbs(t) && path.Clean(t) == t {
			return overlapRefusal(t, sel.WorkspaceID, runDriveName, runner.DriveTarget)
		}
		if err := workspaceTargetShape(t); err != nil {
			return runError(http.StatusBadRequest, reasonWorkspaceTargetInvalid, fmt.Sprintf("workspaces[%d].target: %v", i, err))
		}
		for _, p := range seen {
			if p.id != sel.WorkspaceID && (pathWithin(t, p.target) || pathWithin(p.target, t)) {
				return overlapRefusal(t, sel.WorkspaceID, p.id, p.target)
			}
		}
		seen = append(seen, placed{sel.WorkspaceID, t})
	}
	return nil
}

// overlapRefusal words an overlap of this selection's target t with another
// mount, otherPath. A nested pair names the inner path and the outer mount.
func overlapRefusal(t, self, other, otherPath string) *runRefusal {
	msg := workspaceOverlapEqualMsg(t, other)
	switch {
	case t == otherPath:
	case pathWithin(t, otherPath):
		msg = workspaceOverlapNestedMsg(t, other, otherPath)
	default:
		msg = workspaceOverlapNestedMsg(otherPath, self, t)
	}
	return runError(http.StatusUnprocessableEntity, reasonWorkspaceTargetOverlap, msg)
}

func overrideInvalid(format string, args ...any) *runRefusal {
	return runError(http.StatusBadRequest, reasonOverrideInvalid, "overrides."+fmt.Sprintf(format, args...))
}

// runOverridesRefusal validates the overrides' shape, then refuses every edit the
// narrowing table never allows from a request. What is left is allowed or
// ceiling-clamped; applying it is unappliedFieldsRefusal's stub today.
func runOverridesRefusal(req createRunRequest) *runRefusal {
	o := req.Overrides
	if o == nil {
		return nil
	}
	if refusal := overridesShapeRefusal(*o); refusal != nil {
		return refusal
	}
	for _, item := range o.Items() {
		row, ok := types.OverrideRuleFor(item.Kind, item.Op)
		if !ok {
			return overrideInvalid("%s: %s has no %s operation", item.Subject, item.Kind, item.Op)
		}
		if row.Rule == types.OverrideRefused {
			return runError(http.StatusUnprocessableEntity, reasonOverrideRefused, overrideRefusedMsg(item))
		}
	}
	return nil
}

func overrideRefusedMsg(item client.OverrideItem) string {
	switch item.Kind {
	case types.OverrideAgentHostWildcard:
		return fmt.Sprintf("An override cannot add the wildcard host %s. Name each host.", item.Subject)
	case types.OverrideToolRuleAllow:
		return fmt.Sprintf("An override cannot let the agent use %s without asking. Hold or deny it instead.", item.Subject)
	}
	return fmt.Sprintf("An override cannot %s %s: it would give this run more reach than its source grants.", item.Op, item.Subject)
}

func overridesShapeRefusal(o client.RunOverrides) *runRefusal {
	if a := o.Agent; a != nil {
		if refusal := agentOverridesShape(*a); refusal != nil {
			return refusal
		}
	}
	if a := o.AzureDevOps; a != nil {
		if refusal := adoOverridesShape(*a); refusal != nil {
			return refusal
		}
	}
	if refusal := gitPATOverridesShape(o.GitPAT); refusal != nil {
		return refusal
	}
	return pushRuleOverridesShape(o.PushRules)
}

// overrideText is a printable single-line value within the entry's length cap.
func overrideText(v string) bool {
	return v != "" && len(v) <= maxRunOverrideValueLen && controlCharFree(v)
}

func overrideListLen(name string, n int) *runRefusal {
	if n > maxRunOverrideEntries {
		return overrideInvalid("%s: %d entries exceeds the %d-entry limit", name, n, maxRunOverrideEntries)
	}
	return nil
}

// overrideHost is a bare DNS name: no wildcard (the table decides those), no IP
// address, no port, no URL.
func overrideHost(h string) error {
	if types.IsWildcardHost(h) {
		return nil
	}
	if !overrideText(h) || strings.ContainsAny(h, ":/ ") || net.ParseIP(h) != nil {
		return fmt.Errorf("%q must be a DNS name such as api.example.com", h)
	}
	return proxy.ValidDomainEntry(h)
}

func agentOverridesShape(a client.AgentOverrides) *runRefusal {
	for name, n := range map[string]int{"agent.add_hosts": len(a.AddHosts), "agent.remove_hosts": len(a.RemoveHosts),
		"agent.add_secrets": len(a.AddSecrets), "agent.remove_secrets": len(a.RemoveSecrets), "agent.tool_rules": len(a.ToolRules)} {
		if refusal := overrideListLen(name, n); refusal != nil {
			return refusal
		}
	}
	for i, h := range a.AddHosts {
		if err := overrideHost(h); err != nil {
			return overrideInvalid("agent.add_hosts[%d]: %v", i, err)
		}
	}
	for i, h := range a.RemoveHosts {
		if !overrideText(h) {
			return overrideInvalid("agent.remove_hosts[%d]: name the host to drop", i)
		}
	}
	for i, sec := range a.AddSecrets {
		if err := secretOverrideShape(sec); err != nil {
			return overrideInvalid("agent.add_secrets[%d]: %v", i, err)
		}
	}
	for i, n := range a.RemoveSecrets {
		if !types.SecretNameRE.MatchString(n) {
			return overrideInvalid("agent.remove_secrets[%d]: %q is not a stored secret name", i, n)
		}
	}
	for i, r := range a.ToolRules {
		if !overrideText(r.Tool) || !types.ValidToolEffect(r.Effect) {
			return overrideInvalid("agent.tool_rules[%d]: give a tool name and an effect of allow, hold or deny", i)
		}
	}
	return nil
}

func secretOverrideShape(sec client.SecretOverride) error {
	if !types.SecretNameRE.MatchString(sec.SecretName) {
		return fmt.Errorf("secret_name %q is not a stored secret name", sec.SecretName)
	}
	if types.IsWildcardHost(sec.Host) || overrideHost(sec.Host) != nil {
		return fmt.Errorf("host %q must be a DNS name such as api.example.com", sec.Host)
	}
	if sec.Header != "" && !egress.ValidHeaderName(sec.Header) {
		return fmt.Errorf("header %q is not a valid header name", sec.Header)
	}
	return validInjectionFormat(sec.Format)
}

func adoOverridesShape(a client.ADOOverrides) *runRefusal {
	// An empty set reads as "keep the row's default profile" everywhere a spec is
	// read, which would widen the run a person meant to narrow.
	if len(a.Capabilities) == 0 {
		return overrideInvalid("azure_devops.capabilities: name at least one capability to keep")
	}
	if refusal := overrideListLen("azure_devops.capabilities", len(a.Capabilities)); refusal != nil {
		return refusal
	}
	for i, c := range a.Capabilities {
		if !adoscope.Capability(c).Valid() || slices.Contains(a.Capabilities[:i], c) {
			return overrideInvalid("azure_devops.capabilities[%d]: %q is not a capability, or is listed twice", i, c)
		}
	}
	return nil
}

func gitPATOverridesShape(grants []client.GitPATOverride) *runRefusal {
	if refusal := overrideListLen("git_pat", len(grants)); refusal != nil {
		return refusal
	}
	for i, g := range grants {
		switch {
		case !overrideText(g.Host) || slices.ContainsFunc(grants[:i], func(o client.GitPATOverride) bool { return strings.EqualFold(o.Host, g.Host) }):
			return overrideInvalid("git_pat[%d]: name the forge host, once", i)
		case g.Access != "" && g.Access != types.PATAccessRead && g.Access != types.PATAccessWrite:
			return overrideInvalid("git_pat[%d].access: %q is not one of read, write", i, g.Access)
		case g.Repos == nil && g.Access == "" && g.API == nil:
			return overrideInvalid("git_pat[%d]: change repos, access or api", i)
		case g.Repos != nil && len(*g.Repos) > maxRunOverrideEntries:
			return overrideInvalid("git_pat[%d].repos: more than %d entries", i, maxRunOverrideEntries)
		}
		if g.Repos != nil && slices.ContainsFunc(*g.Repos, func(r string) bool { return !overrideText(r) }) {
			return overrideInvalid("git_pat[%d].repos: every entry must be a repository name", i)
		}
	}
	return nil
}

// pushRuleOverridesShape checks each push-rule override and that no provider and
// organisation appears twice: overrides are keyed by that pair, so a second
// block for one pair has no meaning.
func pushRuleOverridesShape(rules []client.PushRuleOverride) *runRefusal {
	if refusal := overrideListLen("push_rules", len(rules)); refusal != nil {
		return refusal
	}
	for i, p := range rules {
		switch {
		case p.Provider != string(types.GitProviderGitHub) && p.Provider != string(types.GitProviderAzureDevOps):
			return overrideInvalid("push_rules[%d].provider: %q is not one of github, azure_devops", i, p.Provider)
		case !overrideText(p.Org):
			return overrideInvalid("push_rules[%d].org: name the organisation these rules belong to", i)
		case slices.ContainsFunc(rules[:i], func(o client.PushRuleOverride) bool { return o.PushRuleKey() == p.PushRuleKey() }):
			return overrideInvalid("push_rules[%d]: %s already has rules in this request", i, p.PushRuleKey())
		case len(p.DenyPaths)+len(p.RequireReviewPaths) == 0:
			return overrideInvalid("push_rules[%d]: add at least one deny or require-review path", i)
		case len(p.DenyPaths) > maxRunOverrideEntries || len(p.RequireReviewPaths) > maxRunOverrideEntries:
			return overrideInvalid("push_rules[%d]: more than %d paths", i, maxRunOverrideEntries)
		}
		for _, pattern := range slices.Concat(p.DenyPaths, p.RequireReviewPaths) {
			if _, err := types.DenyPathSegments(pattern); err != nil || len(pattern) > maxRunOverrideValueLen {
				return overrideInvalid("push_rules[%d]: path %q is not a usable pattern", i, pattern)
			}
		}
	}
	return nil
}

// builtinComponentsRefusal validates a built-in reference's shape the way the
// component gate validates every other reference, so the refusal does not change
// when the gate starts admitting them.
func builtinComponentsRefusal(req createRunRequest) *runRefusal {
	for i, ref := range req.Components {
		if ref.Builtin == "" {
			continue
		}
		if err := ref.Validate(proxy.ValidDomainEntry); err != nil {
			return runError(http.StatusBadRequest, reasonComponentRefInvalid, fmt.Sprintf("components[%d]: %v", i, err))
		}
	}
	return nil
}

// unappliedFieldsRefusal is the stub half: every field above that changes the
// run's posture and that no lane applies yet. Each is refused by name.
func unappliedFieldsRefusal(req createRunRequest) *runRefusal {
	switch {
	case req.Placement == placement.Local:
		return runError(placement.ReasonPlacementUnavailable.Status(), string(placement.ReasonPlacementUnavailable),
			"Your own runner is not available: this server cannot place a run on a runner yet.")
	case req.RunnerPoolID != "":
		return runError(http.StatusUnprocessableEntity, reasonRequestFieldUnavailable,
			"runner_pool_id: this server does not manage runner pools yet, so the run was not created.")
	case req.AllowedImage != "":
		return runError(http.StatusUnprocessableEntity, reasonRequestFieldUnavailable,
			"allowed_image: this server does not apply an image choice yet, so the run was not created.")
	case req.Overrides != nil && len(req.Overrides.Items()) > 0:
		return runError(http.StatusUnprocessableEntity, reasonRequestFieldUnavailable,
			"overrides: this server does not apply per-run overrides yet, so the run was not created.")
	}
	for i, ref := range req.Components {
		if ref.Builtin != "" {
			return runError(http.StatusUnprocessableEntity, reasonRequestFieldUnavailable, fmt.Sprintf(
				"components[%d]: built-in %s access is not available on this server yet, so the run was not created.", i, ref.Builtin))
		}
	}
	return nil
}
