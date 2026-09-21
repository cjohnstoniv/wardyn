// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"

	"github.com/cjohnstoniv/wardyn/internal/agentpolicy"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// applyRunAgentPolicy generates the agent-side half of this run's autonomy
// level and records what the sandbox actually gets (0.8 #95).
//
// Sited in dispatch rather than at create for the reason every other sandbox
// input is: the file is part of what the sandbox comes up with, and dispatch is
// the one place that composes that. It reads the LEVEL off the run row — the
// value resolveRunAutonomy froze there — so the file, the sandbox env
// (WARDYN_AUTONOMY_LEVEL) and the create audit row's `autonomy` block all cite
// one source.
//
// SILENT for a run with no agent-side layer, which is the ordinary case: an
// unrestricted or unbound level, and every agent but claude-code. A row saying
// "no file, and there was never going to be one" would put a governance event
// on every run on every deployment that has authored no rubric. The honest
// signal for an agent that CAN be gated but has no managed layer is where a
// person will meet it: the launch warning resolveRunAutonomy already raises on
// the 201, and the one line the image's launcher prints into the run's own log.
//
// `delivered` is FALSE and hardcoded, and that is this branch's truth rather
// than a placeholder: the runner contract that places a root-owned file inside
// a sandbox lands with #94, and until it does the file is generated, recorded
// and not delivered. Auditing it as delivered would be the one lie that makes
// the row worthless — an operator reading `run.agent_policy` is asking exactly
// whether the ceiling reached the agent. When #94 lands, the ManagedFiles entry
// is built from the same (path, content) pair returned here and set on
// dispatchRun's SandboxSpec literal, and this flag becomes the answer to
// whether the driver advertised the capability.
func (s *Server) applyRunAgentPolicy(ctx context.Context, run types.AgentRun) {
	path, content, ok := agentpolicy.ForAgent(run.Agent, run.AutonomyLevel)
	if !ok {
		return
	}
	s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.agent_policy",
		run.ID.String(), "success", mustJSON(map[string]any{
			"agent": run.Agent,
			"level": string(run.AutonomyLevel),
			"path":  path,
			// The size, not the content: the document is not a secret, but a
			// governance row that carries a file body invites the next one to.
			// Enough to tell the three documents apart in a trail.
			"bytes": len(content),
			// Whether the agent is actually running under it. See above.
			"delivered": false,
		})))
}
