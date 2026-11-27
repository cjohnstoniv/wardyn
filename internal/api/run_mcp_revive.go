// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// reviveMCPSandboxRefusal refuses a rebooted revive of a run with an MCP server that starts from a command.
// Starting the agent again admits the run anew, so the server meets the switch and the organisation's setting
// as a queued run does (a site config that cannot be read reads as off), and the refusal names it. A run
// whose server ran inside the agent's sandbox, as before this release, is refused too: that is no longer
// allowed, and until a run holds MCP sandbox rows every such run started that way. A revive that leaves the
// agent running leaves its server running too, until the run ends. An erased row keeps only the harnesses
// its MCP server was turned on for, so the server it held may have been one that starts from a command; it
// is refused the same way.
func (s *Server) reviveMCPSandboxRefusal(ctx context.Context, run types.AgentRun, rebooted bool, actorType types.ActorType, actor string) *reviveError {
	if !rebooted {
		return nil
	}
	comps, err := s.runComponentSnapshot(ctx, run.ID)
	if err != nil {
		return reviveRefused(http.StatusServiceUnavailable, reasonReviveOwnerAuthorityUnreadable, loggedMsg(ctx, "re-check the run's components", err))
	}
	for _, rc := range comps {
		if !rc.Definition.LocalMCP() && (!rc.Erased || len(rc.MCPHarnesses) == 0) {
			continue
		}
		sentence := mcpReviveLegacySentence
		if sc, err := s.cfg.Store.GetSiteConfig(ctx); mcpOwnSandboxOff(sc, err) {
			sentence = mcpReviveOffSentence
		}
		s.recordAudit(ctx, s.auditEvent(&run.ID, actorType, actor, "run.revive", run.ID.String(), "denied",
			mustJSON(map[string]any{"subject": run.CreatedBy, "reason": reasonComponentDefinitionInvalid})))
		return reviveRefused(http.StatusUnprocessableEntity, reasonComponentDefinitionInvalid, mcpSandboxSentence(sentence, mcpSlug(rc.Name), ""))
	}
	return nil
}
