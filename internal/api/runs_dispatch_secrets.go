// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// DISPATCH-TIME SECRET RESOLUTION: the two phases that read a STORED SECRET's
// VALUE out of the store and put it somewhere the run can reach — the LLM
// inspection corpus (in-process, for the detector) and env_secret (the sandbox
// process environment). Split out of runs_dispatch.go for the 1000-line
// file-size gate (scripts/check-file-size.sh), joining the llm/mounts/gitbroker/
// ceiling halves already split the same way.
//
// They belong together because they are the only two places in dispatch that
// hold PLAINTEXT, and they share the rules that follow from it: every resolved
// value is registered with the run's mask registry so a verbatim leak into PTY
// capture or any audit event is scrubbed; no value ever enters the audit stream
// (the events carry NAMES only); and each documents its own fail direction —
// LLM inspection fails OPEN because a missing name costs detection coverage,
// env_secret fails CLOSED per grant because a silently absent variable surfaces
// as an unauthenticated API call the agent reports as a task failure.
package api

import (
	"context"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// resolveLLMInspectionSecrets resolves the llm_inspection detection corpus
// store->proxy AT DISPATCH: WorkspaceSecretNames (the field a policy is
// actually allowed to author — see types.LLMInspectionSpec, validatePolicySpec
// refuses a raw value on any write) is looked up in the secret store and
// appended to WorkspaceSecretValues on THIS dispatch's local policy copy only
// — never a stored/ceiling spec, never re-read, never logged (the
// run.policy.effective audit above redacts it to a count).
//
// Belt-and-braces (W12-A-2): every resolved value is ALSO registered with the
// run's mask registry, so a verbatim leak into PTY capture, a session
// recording, or any OTHER audit event's Data/Target is scrubbed the same way
// any other run secret is (cmd/wardynd's maskingRecorder) — not merely kept
// out of this one event.
//
// Fail-open per name: a name that no longer resolves (deleted secret, no
// store configured) is skipped and audited by NAME only (never a value), and
// dispatch continues — this is a detection guardrail, not an access-control
// gate (types.LLMInspectionSpec's own doc: "a guardrail + visibility layer,
// NOT exfiltration prevention").
func (s *Server) resolveLLMInspectionSecrets(ctx context.Context, run types.AgentRun, policy *types.RunPolicySpec) {
	li := policy.LLMInspection
	if li == nil || len(li.WorkspaceSecretNames) == 0 {
		return
	}
	if s.cfg.Secrets == nil {
		s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.llm_inspection.secrets_resolve",
			run.ID.String(), "failure", mustJSON(map[string]any{
				"reason": "no secret store configured", "names": li.WorkspaceSecretNames,
			})))
		return
	}
	var resolved, missing int
	// run.CreatedBy: the run's own owner's row wins, falling back to the
	// operator's (secretOwnerFromRequest.stamped rows never collide with an
	// operator's own run identity string — see injection.go's Get for the
	// same reasoning).
	for _, name := range li.WorkspaceSecretNames {
		val, err := s.cfg.Secrets.For(run.CreatedBy).Get(ctx, name)
		if err != nil || len(val) == 0 {
			missing++
			continue
		}
		resolved++
		li.WorkspaceSecretValues = append(li.WorkspaceSecretValues, string(val))
		if s.cfg.MaskRegistry != nil {
			s.cfg.MaskRegistry.Add(run.ID, val)
		}
	}
	outcome := "success"
	if missing > 0 {
		outcome = "failure"
	}
	s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.llm_inspection.secrets_resolve",
		run.ID.String(), outcome, mustJSON(map[string]any{
			"resolved": resolved, "missing": missing, "names": li.WorkspaceSecretNames,
		})))
}

// envAllowMemberEnvSecret opts a deployment IN to letting MEMBERS hold
// env_secret grants. DEFAULT CLOSED: unset means a member's env_secret grant is
// dropped by filterMemberGrants even when the operator's ceiling lists the exact
// (name, secret) pairing. An operator's own runs are unaffected — the ceiling
// authority is never clamped by its own ceiling.
const envAllowMemberEnvSecret = "WARDYN_ALLOW_MEMBER_ENV_SECRET"

// resolveEnvSecretGrants resolves this run's env_secret grants store->sandbox
// env at dispatch: each grant's scope names a stored secret and the variable to
// put its VALUE under (envSecretScopeFields). This is the whole delivery
// mechanism for the kind — there is no mint, no approval and no broker
// involvement (mintKind refuses env_secret outright), which is why the closest
// precedent is resolveLLMInspectionSecrets and not any of the git lanes.
//
// Every resolved value is registered with the run's mask registry, so a verbatim
// leak into PTY capture, a session recording, or any audit event's Data is
// scrubbed like any other run secret. Values NEVER enter the audit stream: the
// events below carry the variable name and the secret NAME only.
//
// FAIL-CLOSED PER GRANT, deliberately the opposite of resolveLLMInspectionSecrets'
// fail-open: that one feeds a detection corpus, where a missing entry costs
// detection coverage; this one delivers a credential the task needs, where a
// silently absent variable surfaces as an unauthenticated API call the agent
// then reports as a task failure. So a grant is SKIPPED and audited (never
// substituted, never blank-set) when its scope is unreadable, its secret is
// reserved, the store is missing or the value is gone.
//
// It also refuses to OVERWRITE a variable dispatch already set to a NON-EMPTY
// value. Everything in sandboxEnv by this point is platform-authored (the
// harness's own WARDYN_*, the LLM transport's ANTHROPIC_*, artifact-redirect
// config, p.ExtraEnv), and a grant that could replace one would be a config
// override wearing a credential's clothes — the WARDYN_ prefix is already
// refused at write time, and this closes the rest of the set without having to
// enumerate it. Runs LAST in dispatch's env composition so "already set" means
// all of it, not just the part written so far. Non-empty, not merely present:
// an empty value carries no configuration to protect, and treating it as
// occupied would make a placeholder key unfillable for no gain.
// It REPORTS the variable names it actually filled, because those values are
// credential material and must not ride a substrate's readable object model:
// splitSecretEnv moves them onto SandboxSpec.SecretEnv, which the k8s driver
// delivers via secretKeyRef rather than inline in the agent Pod spec.
func (s *Server) resolveEnvSecretGrants(ctx context.Context, run types.AgentRun, policy types.RunPolicySpec, sandboxEnv map[string]string) []string {
	var resolvedNames []string
	for _, g := range policy.EligibleGrants {
		if g.Kind != types.GrantEnvSecret {
			continue
		}
		name, secretName, err := envSecretScopeFields(g.Scope)
		skip := ""
		switch {
		case err != nil:
			skip = "scope invalid: " + err.Error()
		case sinkReservedSecret(secretName):
			skip = "references a reserved platform-internal secret name"
		case s.cfg.Secrets == nil:
			skip = "no secret store configured"
		case sandboxEnv[name] != "":
			skip = "the sandbox env already sets this variable; a grant may not override platform-authored env"
		}
		if skip == "" {
			// run.CreatedBy: same owner-then-operator-fallback rule as
			// resolveLLMInspectionSecrets above.
			val, gerr := s.cfg.Secrets.For(run.CreatedBy).Get(ctx, secretName)
			if gerr != nil || len(val) == 0 {
				skip = "secret could not be resolved"
			} else {
				sandboxEnv[name] = string(val)
				resolvedNames = append(resolvedNames, name)
				if s.cfg.MaskRegistry != nil {
					s.cfg.MaskRegistry.Add(run.ID, val)
				}
			}
		}
		data := map[string]any{"name": name, "secret_name": secretName}
		outcome := "success"
		if skip != "" {
			outcome, data["reason"] = "failure", skip
		}
		s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.env_secret.resolve",
			run.ID.String(), outcome, mustJSON(data)))
	}
	return resolvedNames
}
