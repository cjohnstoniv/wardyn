// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"fmt"
	"net/http"

	"github.com/cjohnstoniv/wardyn/internal/authz"
)

func (s *Server) runCapabilityRefusal(r *http.Request, kind, value, target, msg string) *runRefusal {
	allowed, err := s.capSeamAllowed(r.Context(), kind, value)
	if err != nil {
		return runServerError("resolve capability", err)
	}
	if allowed {
		return nil
	}
	return runDenied(authz.Deny(capKinds[kind].reason, target, msg))
}

// Only member-authored workspace images need this re-check. Applying it to
// operator-owned workspaces would refuse their images wherever capImage is off.
func (s *Server) seededImageRefusal(r *http.Request, seededOwner, image string) *runRefusal {
	if seededOwner == "" {
		return nil
	}
	granted, err := s.capGranted(r.Context(), capImage, image)
	if err != nil {
		return runServerError("resolve capability", err)
	}
	if granted {
		return nil
	}
	// The SAME refusal the explicit --image branch raises (target, reason and
	// shape), because it is the same capability answered about the same value —
	// only the door differs, and the message says which one.
	return runDenied(authz.Deny(authz.ReasonBYOIUser, "runs.image",
		"image "+image+" comes from your own workspace's base image and is not granted to you — "+
			"ask an admin to grant the exact image ref, or launch with the agent's convention image"))
}

func (s *Server) runRequestGovernance(r *http.Request, req createRunRequest) (governanceCeiling, *runRefusal) {
	if s.runUngoverned(r.Context()) {
		return governanceCeiling{Operator: true}, nil
	}
	// One capability snapshot for every field below (capBatch's ctx memo).
	r = r.WithContext(withCapBatch(r.Context()))
	if req.DevcontainerRepo != "" {
		return governanceCeiling{}, runDenied(authz.Deny(authz.ReasonBYOIUser, "runs.image",
			"a custom devcontainer repo (devcontainer_repo) is operator-only; launch with the agent's convention image or an onboarded workspace's base image"))
	}
	if req.Image != "" {
		granted, err := s.capGranted(r.Context(), capImage, req.Image)
		if err != nil {
			return governanceCeiling{}, runServerError("resolve capability", err)
		}
		if !granted {
			return governanceCeiling{}, runDenied(authz.Deny(authz.ReasonBYOIUser, "runs.image",
				"image "+req.Image+" is not granted to you — ask an admin to grant the exact image ref, "+
					"or launch with the agent's convention image or an onboarded workspace's base image"))
		}
	}
	if req.WorkspaceID != nil {
		if refusal := s.runCapabilityRefusal(r, capWorkspace, req.WorkspaceID.String(), "runs.workspace",
			"you are not granted workspace "+req.WorkspaceID.String()+" — ask an admin for access, or launch without a workspace"); refusal != nil {
			return governanceCeiling{}, refusal
		}
	}
	// An empty agent names nothing to bound — an exec run may legitimately omit
	// it (agentRequirementError), and gating "" would refuse those on a kind
	// that has nothing to say about them.
	if req.Agent != "" {
		if refusal := s.runCapabilityRefusal(r, capAgent, req.Agent, "runs.agent",
			"you are not granted agent "+req.Agent+" — ask an admin to grant it, or launch one you hold"); refusal != nil {
			return governanceCeiling{}, refusal
		}
	}
	// The stored policy the caller SELECTED, and only that: a run naming no
	// policy runs under its own ceiling, which is nothing to bound.
	if req.PolicyID != nil {
		if refusal := s.runCapabilityRefusal(r, capPolicy, req.PolicyID.String(), "runs.policy",
			"Stored policy "+req.PolicyID.String()+" isn't available to you. Ask your admin, or launch without policy_id."); refusal != nil {
			return governanceCeiling{}, refusal
		}
	}
	ceiling, err := s.effectiveCeiling(r.Context())
	if err != nil {
		return governanceCeiling{}, ceilingRunRefusal(err)
	}
	return ceiling, nil
}

func (s *Server) runGovernancePosture(r *http.Request, req createRunRequest, ceiling governanceCeiling, complete bool) *runRefusal {
	if ceiling.Profile == nil {
		return nil
	}
	name := ceiling.Profile.Name
	policy := s.ceilingPolicy(r.Context(), ceiling)
	// exec runs a bare command: no agent, no toolgate, nothing for tool_rules to
	// bind. A profile that wants supervised tool use has to be able to close the
	// door that routes around the gate entirely.
	if ceiling.Limits.DenyTaskModeExec && req.TaskMode == "exec" {
		return runDenied(authz.Deny(authz.ReasonGovernanceProfile, "runs.task_mode", fmt.Sprintf(
			"`task_mode=exec` is not allowed by your governance profile %q — an exec run carries no agent and no tool approvals, so nothing supervises it. Launch with an agent instead.", name)).WithPolicy(policy))
	}
	// The same door through an interactive run: a task with interactive_start
	// unset or "shell" is run by the image as `bash -lc` at boot, before anyone
	// attaches. Same predicate as the autonomy gate and dispatch, so the limit
	// and the seed cannot disagree; `!= "agent"` keeps an unknown value refused.
	if ceiling.Limits.DenyTaskModeExec && req.InteractiveStart != "agent" && interactiveBootSeed(requestIsInteractive(req), req.Task) != "" {
		return runDenied(authz.Deny(authz.ReasonGovernanceProfile, "runs.interactive_start", fmt.Sprintf(
			"a shell startup command is not allowed by your governance profile %q — with `interactive_start` unset or `shell` the task runs as a shell command at sandbox boot, before anyone attaches, unattended the way exec does. Launch with `interactive_start=agent`, or without a task.", name)).WithPolicy(policy))
	}
	// Post-coercion, and that is the whole gate. req.Interactive is still the RAW
	// field here — this function runs before the empty-task→interactive coercion
	// — so reading it directly would be evaded by simply omitting the task, which
	// is the one request shape a deny_interactive profile most needs to refuse.
	if ceiling.Limits.DenyInteractive && (req.Interactive || (complete && requestIsInteractive(req))) {
		return runDenied(authz.Deny(authz.ReasonGovernanceProfile, "runs.interactive", fmt.Sprintf(
			"interactive runs are not allowed by your governance profile %q, and a request with no task comes up interactive too. Launch with a task, and without `--interactive`.", name)).WithPolicy(policy))
	}
	return nil
}

func (s *Server) runGovernanceSupervision(r *http.Request, req createRunRequest, ceiling governanceCeiling) *runRefusal {
	if ceiling.Profile == nil {
		return nil
	}
	name := ceiling.Profile.Name
	policy := s.ceilingPolicy(r.Context(), ceiling)
	if !governanceHoldRules(ceiling) {
		return nil
	}
	// The pre-attach seed span runs skip-permissions with no toolgate and
	// no human at the pane yet, so the "interactive is human-supervised"
	// rationale above is explicitly false for it — which is why this refusal is NOT
	// scoped to the derivation's non-interactive lane the way the codex one is.
	if req.SeedAutoTools {
		return runDenied(authz.Deny(authz.ReasonGovernanceProfile, "runs.seed_auto_tools", fmt.Sprintf(
			"`seed_auto_tools` is not allowed by your governance profile %q: its tool rules hold or deny, and the pre-attach seed runs before any human is at the pane. Launch without it.", name)).WithPolicy(policy))
	}
	// Scoped to exactly the case where effectiveToolApprovals WOULD derive
	// hold: codex-cli has no external tool-approval contract, so a derived hold
	// there would silently ship the unsupervised run the explicit-hold refusal
	// exists to reject — the same contradiction, arriving through a
	// field the caller never set.
	if req.Agent == "codex-cli" && !requestIsInteractive(req) {
		return runDenied(authz.Deny(authz.ReasonGovernanceProfile, "runs.agent", fmt.Sprintf(
			"codex-cli is not supported under your governance profile %q: its tool rules hold or deny, and codex-cli has no external tool-approval contract. Launch a different agent.", name)).WithPolicy(policy))
	}
	return nil
}

func (s *Server) runQuotaRefusal(r *http.Request, ceiling governanceCeiling) *runRefusal {
	limit := ceiling.Limits.MaxConcurrentRuns
	if ceiling.Profile == nil || limit <= 0 {
		return nil
	}
	active, err := s.cfg.Store.CountActiveRunsBy(r.Context(), principalFromRequest(r))
	if err != nil {
		return runServerError("count active runs", err)
	}
	if active < limit {
		return nil
	}
	return runDenied(authz.Deny(authz.ReasonRunQuota, "runs.quota", fmt.Sprintf(
		"too many runs at once (max %d) — your governance profile %q caps how many runs you can have going, and %d are still active. Stop one first.",
		limit, ceiling.Profile.Name, active)).WithPolicy(s.ceilingPolicy(r.Context(), ceiling)))
}
