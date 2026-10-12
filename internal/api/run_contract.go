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
// A field that only tunes a run (resources) is accepted and not yet applied;
// its doc comment on pkg/client says so. Each behaviour lane
// deletes its case in unappliedFieldsRefusal and adds its own step to
// runFoldSteps:
//
//	run-mode carriers (run_mode.go) -> A-L3, A-L9, A-L14, A-L4, A-L7, D-112-113
//	overrides                     -> A-L3
//	built-in components           -> A-L14
//	allowed_image, image facts     -> the image lane (A-L4/A-L6)
//	placement local               -> the placement lane (#117)
const (
	maxRunOverrideEntries  = 64
	maxRunOverrideValueLen = 256
	maxRunResourceCPU      = 1_000_000                  // milli-CPU: a thousand CPUs
	maxRunResourceMemory   = 16 << 20                   // MiB: 16 TiB
	maxRunResourceDisk     = types.RunnerPoolDiskMiBMax // MiB: 16 TiB, the pool disk limit's bound
	runUserHome            = "/home/agent"
	runDriveName           = "your drive"
	// maxRunWorkspaces bounds workspaces[]: each entry is a store read and an
	// authorization before anything is folded.
	maxRunWorkspaces = 32
)

// runContractRefusal answers the first refusal of the contract fields, shape
// first. A method on Server for the reason validateRunTextFields is: the parity
// guard reads gates as `s.<Gate>(…)` calls.
func (s *Server) runContractRefusal(req createRunRequest) *runRefusal {
	for _, check := range runContractShapeChecks {
		if refusal := check(req); refusal != nil {
			return refusal
		}
	}
	return unappliedFieldsRefusal(req)
}

// runContractShapeChecks are the contract's shape checks, in answer order. A
// template document is held to the same list when it is imported.
var runContractShapeChecks = []func(createRunRequest) *runRefusal{
	placementShapeRefusal, runnerPoolShapeRefusal, resourcesRefusal, allowedImageRefusal, workspaceTargetsRefusal, runOverridesRefusal, builtinComponentsRefusal,
	runModeShapeRefusal, runShapeRefusal, runEndShapeRefusal,
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
	if r.CPUMillis < 0 || r.CPUMillis > maxRunResourceCPU || r.MemoryMiB < 0 || r.MemoryMiB > maxRunResourceMemory ||
		r.DiskMiB < 0 || r.DiskMiB > maxRunResourceDisk {
		return runError(http.StatusBadRequest, reasonResourcesInvalid, fmt.Sprintf(
			"resources: cpu_millis must be 0 to %d, memory_mib 0 to %d and disk_mib 0 to %d", maxRunResourceCPU, maxRunResourceMemory, maxRunResourceDisk))
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

// workspaceTargetsRefusal checks the shape of the workspaces[] list: at most
// maxRunWorkspaces, each id once, workspace_id equal to the first entry when both are sent, each target
// and each repos entry well formed. Whether the targets collide is answered
// over every resolved target once the workspaces are seeded (stepWorkspaces).
//
// A target inside the drive's own path, with the drive on, answers
// workspace_target_overlap rather than _invalid: the drive mounts there.
func workspaceTargetsRefusal(req createRunRequest) *runRefusal {
	if len(req.Workspaces) > maxRunWorkspaces {
		return runError(http.StatusBadRequest, reasonWorkspaceSelectionInvalid, fmt.Sprintf("workspaces: attach at most %d workspaces.", maxRunWorkspaces))
	}
	drive := req.Drive != nil && req.Drive.Enabled
	seen := make(map[uuid.UUID]bool, len(req.Workspaces))
	for i, sel := range req.Workspaces {
		id, err := uuid.Parse(sel.WorkspaceID)
		switch {
		case err != nil || id == uuid.Nil || id.String() != sel.WorkspaceID:
			// The canonical spelling only: the selection's options are keyed by it,
			// so another spelling of the same id would drop them.
			return runError(http.StatusBadRequest, reasonWorkspaceSelectionInvalid, fmt.Sprintf("workspaces[%d].workspace_id: give a workspace id.", i))
		case seen[id]:
			return runError(http.StatusBadRequest, reasonWorkspaceSelectionInvalid, fmt.Sprintf("workspaces[%d]: %s is listed twice.", i, sel.WorkspaceID))
		case i == 0 && req.WorkspaceID != nil && *req.WorkspaceID != id:
			return runError(http.StatusBadRequest, reasonWorkspaceSelectionInvalid,
				"workspace_id and workspaces[0] name different workspaces. Send one primary workspace.")
		}
		seen[id] = true
		if t := sel.Target; t != "" {
			if drive && pathWithin(t, runner.DriveTarget) && path.IsAbs(t) && path.Clean(t) == t {
				return overlapRefusal(t, "", runDriveName, runner.DriveTarget)
			}
			if err := workspaceTargetShape(t); err != nil {
				return runError(http.StatusBadRequest, reasonWorkspaceTargetInvalid, fmt.Sprintf("workspaces[%d].target: %v", i, err))
			}
		}
		if refusal := repoSelectionsRefusal(i, sel.Repos); refusal != nil {
			return refusal
		}
	}
	return nil
}

// repoSelectionsRefusal checks one selection's repos: each repo named once,
// each ref one validRunRef accepts.
func repoSelectionsRefusal(i int, repos *[]client.RepoSelection) *runRefusal {
	if repos == nil {
		return nil
	}
	seen := make(map[string]bool, len(*repos))
	for j, rs := range *repos {
		switch {
		case rs.Repo == "" || len(rs.Repo) > maxRunRepoLen || !controlCharFree(rs.Repo):
			return runError(http.StatusBadRequest, reasonRunRefInvalid, fmt.Sprintf("workspaces[%d].repos[%d].repo: name a repository of this workspace.", i, j))
		case seen[rs.Repo]:
			return runError(http.StatusBadRequest, reasonRunRefInvalid, fmt.Sprintf("workspaces[%d].repos: %s is listed twice.", i, rs.Repo))
		case rs.Ref != "" && !validRunRef(rs.Ref):
			return runError(http.StatusBadRequest, reasonRunRefInvalid, fmt.Sprintf("workspaces[%d].repos[%d].ref: give a branch, tag or commit name.", i, j))
		}
		seen[rs.Repo] = true
	}
	return nil
}

// validRunRef is the shape of a per-run ref: what git check-ref-format accepts
// for a branch or tag name, or a commit. The leading "-" rule is the one that
// matters most: clone_one passes the ref to `git fetch ... origin "$ref"`,
// where a leading "-" is read as an option (--upload-pack=...).
func validRunRef(ref string) bool {
	if len(ref) > 255 || strings.HasPrefix(ref, "-") || strings.Contains(ref, "..") || strings.Contains(ref, "@{") ||
		strings.ContainsAny(ref, " ~^:?*[\\") || strings.HasSuffix(ref, "/") || strings.HasSuffix(ref, ".lock") {
		return false
	}
	for _, r := range ref {
		if r < 0x21 || r > 0x7e {
			return false
		}
	}
	return true
}

func overrideInvalid(format string, args ...any) *runRefusal {
	return runError(http.StatusBadRequest, reasonOverrideInvalid, "overrides."+fmt.Sprintf(format, args...))
}

// runOverridesRefusal validates the overrides' shape, then refuses every edit the
// narrowing table never allows from a request. What is left is allowed or
// ceiling-clamped; applyRunOverrides applies it inside the policy fold.
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
	if refusal := runHostOverridesShape(o.Hosts); refusal != nil {
		return refusal
	}
	if e := o.Egress; e != nil {
		if e.FirstUseApproval != "" && !e.FirstUseApproval.Valid() {
			return overrideInvalid("egress.first_use_approval: %q is not one of always_deny, deny_with_review, wait_for_review", e.FirstUseApproval)
		}
		if e.AllowAllEgress != nil && !*e.AllowAllEgress {
			return overrideInvalid("egress.allow_all_egress: set it to true, or leave it out")
		}
	}
	return pushRuleOverridesShape(o.PushRules)
}

// runHostOverridesShape checks the run-wide host lists. An allowed host is a bare
// DNS name (the table refuses wildcards); a denied one may be a wildcard, since a
// deny only takes reach away. Any removal is refused by name.
func runHostOverridesShape(h *client.RunHostOverrides) *runRefusal {
	if h == nil {
		return nil
	}
	for name, n := range map[string]int{"hosts.add_allowed": len(h.AddAllowed), "hosts.add_denied": len(h.AddDenied),
		"hosts.remove_allowed": len(h.RemoveAllowed), "hosts.remove_denied": len(h.RemoveDenied)} {
		if refusal := overrideListLen(name, n); refusal != nil {
			return refusal
		}
	}
	for i, host := range h.AddAllowed {
		if err := overrideHost(host); err != nil {
			return overrideInvalid("hosts.add_allowed[%d]: %v", i, err)
		}
	}
	for i, host := range h.AddDenied {
		if !overrideText(host) || net.ParseIP(host) != nil || strings.ContainsAny(host, ":/ ") || proxy.ValidDomainEntry(host) != nil {
			return overrideInvalid("hosts.add_denied[%d]: %q must be a DNS name such as api.example.com, or a wildcard such as *.example.com", i, host)
		}
	}
	if len(h.RemoveAllowed)+len(h.RemoveDenied) > 0 {
		return runError(http.StatusUnprocessableEntity, reasonOverrideRefused,
			"An override cannot remove a host of the base policy: its hosts are read-only on a run. Add a denied host instead, or choose another base.")
	}
	return nil
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
		case p.PushRuleKey() == "":
			_, err := types.PushRuleKey(p.Provider, p.Host, p.Org)
			return overrideInvalid("push_rules[%d]: %v", i, err)
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
	if refusal := runModeUnavailableRefusal(req); refusal != nil {
		return refusal
	}
	switch {
	case req.AllowedImage != "":
		return runError(http.StatusUnprocessableEntity, reasonRequestFieldUnavailable,
			"allowed_image: this server does not apply an image choice yet, so the run was not created.")
	}
	for i, ref := range req.Components {
		if ref.Builtin == types.ComponentBuiltinAzureDevOps {
			return builtinUnavailable(i, ref.Builtin)
		}
	}
	return nil
}
