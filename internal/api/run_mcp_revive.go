// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"log/slog"
	"net/http"
	"slices"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// reviveMCPSandboxRefusal refuses a revive that would start an MCP server that starts from a command when it
// cannot be started. A server with no sandbox row ran inside the agent's sandbox, as before this release: a
// rebooted revive of it is refused, since that is no longer allowed (an erased row keeps only the harnesses its
// server was turned on for, so it is judged the same way). A server with a row is re-created by a rebooted
// revive, and by any revive when its row is not ready with a live sandbox (every row a loss ended): that
// revive meets the dispatch re-check, the switch and the organisation's setting (a site config that cannot be
// read reads as off), the site's upstream proxy and the runner's MCP sandbox kinds, and is refused naming the
// server (D6). A live egress edit is not a revive and is never refused here: its MCP phase ends a server it
// cannot re-create.
func (s *Server) reviveMCPSandboxRefusal(ctx context.Context, run types.AgentRun, rebooted, edit bool, actorType types.ActorType, actor string) *reviveError {
	if edit {
		return nil
	}
	comps, err := s.runComponentSnapshot(ctx, run.ID)
	if err != nil {
		return reviveRefused(http.StatusServiceUnavailable, reasonReviveOwnerAuthorityUnreadable, loggedMsg(ctx, "re-check the run's components", err))
	}
	rows, err := s.reviveMCPRows(ctx, run.ID)
	if err != nil {
		return reviveRefused(http.StatusServiceUnavailable, reasonReviveOwnerAuthorityUnreadable, loggedMsg(ctx, "read the run's MCP servers", err))
	}
	for _, rc := range comps {
		if !rc.Definition.LocalMCP() && (!rc.Erased || len(rc.MCPHarnesses) == 0) {
			continue
		}
		i := slices.IndexFunc(rows, func(r store.RunMCPSandbox) bool { return r.Ordinal == rc.Ordinal })
		if i < 0 {
			if !rebooted {
				continue
			}
			sentence := mcpReviveLegacySentence
			if sc, err := s.cfg.Store.GetSiteConfig(ctx); mcpOwnSandboxOff(sc, err) {
				sentence = mcpReviveOffSentence
			}
			return s.refuseMCPRevive(ctx, run, actorType, actor, reasonComponentDefinitionInvalid, mcpSandboxSentence(sentence, mcpSlug(rc.Name), ""))
		}
		if rc.Erased || (!rebooted && s.mcpRowLive(ctx, run, rows[i])) {
			continue
		}
		site, siteErr := s.cfg.Store.GetSiteConfig(ctx)
		if mcpOwnSandboxOff(site, siteErr) {
			return s.refuseMCPRevive(ctx, run, actorType, actor, reasonComponentDefinitionInvalid, mcpSandboxSentence(mcpReviveOffSentence, rows[i].ServerKey, ""))
		}
		if reason, sentence := s.mcpDispatchRefusal(ctx, run, len(rows), rows[i].ServerKey, site, siteErr); reason != "" {
			return s.refuseMCPRevive(ctx, run, actorType, actor, reason, sentence)
		}
	}
	return nil
}

// refuseMCPRevive writes the revive's denied row and answers it 422 with reason and sentence.
func (s *Server) refuseMCPRevive(ctx context.Context, run types.AgentRun, actorType types.ActorType, actor, reason, sentence string) *reviveError {
	s.recordAudit(ctx, s.auditEvent(&run.ID, actorType, actor, "run.revive", run.ID.String(), "denied",
		mustJSON(map[string]any{"subject": run.CreatedBy, "reason": reason})))
	return reviveRefused(http.StatusUnprocessableEntity, reason, sentence)
}

// reviveMCPRows is the run's MCP sandbox rows, none on a store or runner without them.
func (s *Server) reviveMCPRows(ctx context.Context, runID uuid.UUID) ([]store.RunMCPSandbox, error) {
	st, _, ok := s.mcpSandboxSeams()
	if !ok {
		return nil, nil
	}
	return st.ListRunMCPSandboxes(ctx, runID)
}

// mcpRowLive reports whether row is ready with a live sandbox: a revive that does not start the agent leaves it.
func (s *Server) mcpRowLive(ctx context.Context, run types.AgentRun, row store.RunMCPSandbox) bool {
	_, m, ok := s.mcpSandboxSeams()
	if !ok || row.State != store.MCPSandboxReady {
		return false
	}
	sb, err := m.MCPSandboxStatus(ctx, mcpSandboxIDOf(run, row))
	return err == nil && sb.Live
}

// mcpSandboxReviveTimeout bounds a revive's MCP phase (MCP51-F2), on a bound of its own: never the claim's.
const mcpSandboxReviveTimeout = killCascadeTimeout

// reviveMCPContext is the MCP phase's context: its own bound, ended too if the run's lock is lost.
func reviveMCPContext(ctx, lockCtx context.Context) (context.Context, func()) {
	bound, cancel := context.WithTimeout(context.WithoutCancel(ctx), mcpSandboxReviveTimeout)
	held, end := whileLockHeld(bound, lockCtx)
	return held, func() { end(); cancel() }
}

// mcpReviveWriteContext is one write of the MCP phase that must land after the phase's bound is spent (a
// failed row, its audit row, the kill of what it started): its own short bound, with the phase's values.
func mcpReviveWriteContext(mcpCtx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(mcpCtx), heldStoreCallBound)
}

// mcpReviveRecreates reports whether a revive re-creates row: a rebooted revive, a changed policy digest, or a
// row not ready with a live sandbox. A live egress edit is not a revive: it re-creates only a row that is up
// and whose digest changed.
func (s *Server) mcpReviveRecreates(ctx context.Context, run types.AgentRun, row store.RunMCPSandbox, digest string, rebooted, edit bool) bool {
	changed := digest != row.PolicyDigest
	if edit {
		return changed && mcpRowUp(row)
	}
	return rebooted || changed || !s.mcpRowLive(ctx, run, row)
}

// reviveMCPSandboxes is a revive's MCP phase (D4-5), once the revive has succeeded and recorded it: each
// server's policy derived again with the current denies (only narrowing), and each row mcpReviveRecreates
// names killed, reopened to starting only while it is as read at the phase start and the run is live, created
// and started, and kept starting for the watcher's settle. A false reopen skips that row; a reopen that
// errors stops the phase; a policy it cannot derive again, a sandbox it cannot prepare, a failed create or
// start, or a kept write that errors, fails that server alone and kills what ran of it; a
// false keep ends that row (superseded); a cut on this replica at a generation not older than gen ends every
// row (lost) and stops the phase, as does the phase's bound or a lost lock. The run and the revive's answer
// are never changed by it. A server the dispatch re-check now refuses is not re-created: an edit that
// narrowed it ends it, as no wider sandbox may keep running.
func (s *Server) reviveMCPSandboxes(mcpCtx context.Context, run types.AgentRun, gen int, rebooted, edit bool) {
	st, _, ok := s.mcpSandboxSeams()
	if !ok {
		return
	}
	rows, err := st.ListRunMCPSandboxes(mcpCtx, run.ID)
	if err != nil || len(rows) == 0 {
		if err != nil {
			slog.WarnContext(mcpCtx, "wardynd: a revive could not read the run's MCP servers", slog.String("run_id", run.ID.String()), slog.Any("err", err))
		}
		return
	}
	b, servers, w, err := s.mcpLiveBase(mcpCtx, run)
	var grants []types.CredentialGrant
	if err == nil {
		grants, err = s.cfg.Store.ListGrantsByRun(mcpCtx, run.ID)
	}
	if err != nil {
		// No server's policy is known now, so none that runs may keep the one it was made under.
		for _, row := range rows {
			if mcpRowUp(row) {
				s.failMCPRecreate(mcpCtx, run, row, mcpFailCreate, err)
			}
		}
		return
	}
	site, siteErr := s.cfg.Store.GetSiteConfig(mcpCtx)
	reason, _ := s.mcpDispatchRefusal(mcpCtx, run, len(rows), rows[0].ServerKey, site, siteErr)
	for _, row := range rows {
		if mcpCtx.Err() != nil {
			return
		}
		i := slices.IndexFunc(servers, func(m mcpServer) bool { return m.ordinal == row.Ordinal })
		if i < 0 {
			continue
		}
		pol, err := s.mcpPolicyAgain(mcpCtx, b, servers[i], row)
		if err != nil {
			s.failMCPRecreate(mcpCtx, run, row, mcpFailCreate, err) // its policy is unknown: nothing of it keeps running
			continue
		}
		if !s.mcpReviveRecreates(mcpCtx, run, row, pol.spec.Digest(), rebooted, edit) {
			continue
		}
		if reason != "" {
			if mcpRowUp(row) {
				s.endMCPSandbox(mcpCtx, run, row, mcpEndReplaced)
			}
			continue
		}
		if !s.reviveMCPRow(mcpCtx, run, servers[i], row, b, grants, w, gen) {
			return
		}
	}
}

// failMCPRecreate fails a row the revive must re-create and cannot prepare: what runs of it is killed, so no
// sandbox outlives the policy it was made under, an up row goes failed with cause, and a create failure row is
// written. A stopped row stays stopped.
func (s *Server) failMCPRecreate(mcpCtx context.Context, run types.AgentRun, row store.RunMCPSandbox, cause string, err error) {
	slog.WarnContext(mcpCtx, "wardynd: a revive could not prepare an MCP server's sandbox", slog.String("run_id", run.ID.String()),
		slog.String("server", row.ServerKey), slog.Any("err", err))
	st, m, _ := s.mcpSandboxSeams()
	wctx, cancel := mcpReviveWriteContext(mcpCtx)
	defer cancel()
	_ = m.KillMCPSandbox(wctx, mcpSandboxIDOf(run, row))
	_, _ = st.FailMCPSandboxIfUp(wctx, run.ID, row.Ordinal, cause)
	s.auditMCPSandboxCreate(wctx, row, "failure", cause)
}

// reviveMCPRow re-creates one row (the steps of reviveMCPSandboxes). false stops the phase.
func (s *Server) reviveMCPRow(mcpCtx context.Context, run types.AgentRun, srv mcpServer, row store.RunMCPSandbox, b mcpPolicyBase,
	grants []types.CredentialGrant, w withholding, gen int) bool {
	st, m, _ := s.mcpSandboxSeams()
	id := mcpSandboxIDOf(run, row)
	rc, err := s.prepareMCPRecreate(mcpCtx, run, b, srv, row, grants, w)
	if err != nil {
		s.failMCPRecreate(mcpCtx, run, row, mcpFailCreate, err)
		return true
	}
	_ = m.KillMCPSandbox(mcpCtx, id)
	opened, err := st.ReopenMCPSandboxIfLive(mcpCtx, run.ID, row.Ordinal, row.UpdatedAt, rc.built.jti, s.cfg.Now().Add(terminalUploadGrace))
	if err != nil {
		return false // the store cannot record a container: nothing more is created
	}
	if !opened {
		return true
	}
	row.SandboxRef = mcpSandboxRef(run, row.Ordinal)
	if cause, err := s.createAndStartMCP(mcpCtx, run, rc); err != nil {
		wctx, cancel := mcpReviveWriteContext(mcpCtx)
		defer cancel()
		_ = m.KillMCPSandbox(wctx, id)
		_, _ = st.FailMCPSandboxIfStarting(wctx, run.ID, row.Ordinal, cause)
		s.auditMCPSandboxCreate(wctx, row, "failure", cause)
		return mcpCtx.Err() == nil
	}
	wctx, cancel := mcpReviveWriteContext(mcpCtx)
	defer cancel()
	kept, err := st.KeepMCPSandboxStartingIfLive(wctx, run.ID, row.Ordinal, row.SandboxRef)
	switch {
	case err != nil:
		_ = m.KillMCPSandbox(wctx, id)
		_, _ = st.FailMCPSandboxIfStarting(wctx, run.ID, row.Ordinal, mcpFailStateWrite)
		s.auditMCPSandboxCreate(wctx, row, "failure", mcpFailStateWrite)
		return true
	case !kept:
		s.endMCPSandbox(wctx, run, row, mcpEndReplaced)
		return true
	case s.lostCutHolds(run.ID, gen):
		_ = m.KillMCPSandbox(wctx, id)
		s.endRunMCPSandboxes(wctx, run.ID, mcpEndLost)
		return false
	}
	s.auditMCPPolicyResolve(wctx, run.ID, row.Ordinal, row.ServerKey, rc.pol)
	s.auditMCPSandboxCreate(wctx, row, "success", "")
	return true
}
