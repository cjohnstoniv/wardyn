// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// Revoking the `minted_pat` lane's tokens. Every token a run holds is revoked
// together, and only where the run can no longer use them:
//
//   - the end paths: revokeRunCascade (completion, failure, lease loss,
//     reconcile, the probe reclaim, the sandbox sweeps), the kill
//     (killTeardownTail), the idle stop (CancelTerminalRunApprovals) and a
//     kept run's end (revokeRunBroker);
//   - pause (pauseRun), drift (resolveADOInjection), and the erase of a
//     person's credentials (handleErasePersonCredentials);
//   - the sweep below, the backstop for all of them.
//
// Renewal and widening revoke nothing: a host may still hold the older token
// (ado_run_pat_cache.go).

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// adoRunPATSweepEvery is the sweep's cadence; it rides the run watcher's tick.
const adoRunPATSweepEvery = 5 * time.Minute

// revokeRunPATs revokes every token still recorded live for runID and drops
// the run's cache entry, or, for a pause, empties it so the resume creates a
// new one. Best-effort: a revoke that did not complete stays recorded for the
// sweep.
func (s *Server) revokeRunPATs(ctx context.Context, runID uuid.UUID, reason string) {
	st, ok := s.cfg.Store.(store.RunPATStore)
	if !ok {
		return
	}
	e, unlock := s.adoRunPATs.lock(runID)
	defer unlock()
	rows, err := st.ListUnrevokedRunPATs(ctx, store.RunPATFilter{RunID: runID})
	if err != nil {
		slog.WarnContext(ctx, "wardynd: listing a run's Azure DevOps tokens to revoke failed; the sweep retries",
			slog.String("run_id", runID.String()), slog.Any("err", err))
	}
	for _, p := range rows {
		s.revokeRunPAT(ctx, st, p, reason)
	}
	if reason == adoPATRevokePause {
		e.cur, e.caps, e.paused = adoPAT{}, nil, true
		return
	}
	s.adoRunPATs.drop(runID, e)
}

// revokeOwnerRunPATs revokes every live token created in owner's name, run by
// run: a person disconnected, or an admin erased their credentials.
func (s *Server) revokeOwnerRunPATs(ctx context.Context, owner, reason string) {
	st, ok := s.cfg.Store.(store.RunPATStore)
	if !ok || owner == "" {
		return
	}
	rows, err := st.ListUnrevokedRunPATs(ctx, store.RunPATFilter{Owner: owner})
	if err != nil {
		slog.WarnContext(ctx, "wardynd: listing a person's Azure DevOps tokens to revoke failed; the sweep retries",
			slog.Any("err", err))
		return
	}
	done := map[uuid.UUID]bool{}
	for _, p := range rows {
		if !done[p.RunID] {
			done[p.RunID] = true
			s.revokeRunPATs(ctx, p.RunID, reason)
		}
	}
}

// revokeRunPAT revokes one token and closes its row. A token past its validTo
// has expired on its own and is only closed. A revoke Azure DevOps refused for
// good is closed with the error (the token then lives to its validTo); one
// that did not complete stays open for the sweep.
func (s *Server) revokeRunPAT(ctx context.Context, st store.RunPATStore, p store.RunPAT, reason string) {
	if !p.ValidTo.After(s.cfg.Now()) {
		s.closeRunPAT(ctx, st, p, adoPATRevokeExpired, p.LastError)
		return
	}
	data := map[string]any{"reason": reason, "authorization_id": p.AuthorizationID, "owner": p.Owner,
		"provider_row": p.ProviderRowID, "organisation": p.Org}
	transient, err := s.deleteRunPAT(ctx, p)
	if err == nil {
		if s.closeRunPAT(ctx, st, p, reason, "") {
			s.recordAudit(ctx, s.auditEvent(&p.RunID, types.ActorSystem, "wardynd", adoPATAuditRevoke,
				p.AuthorizationID.String(), "success", mustJSON(data)))
		}
		return
	}
	data["error"], data["abandoned"] = err.Error(), !transient
	s.recordAudit(ctx, s.auditEvent(&p.RunID, types.ActorSystem, "wardynd", adoPATAuditRevokeFailed,
		p.AuthorizationID.String(), "failure", mustJSON(data)))
	if !transient {
		s.closeRunPAT(ctx, st, p, reason, err.Error())
		return
	}
	if nerr := st.NoteRunPATRevokeFailed(ctx, p.RunID, p.AuthorizationID, err.Error()); nerr != nil {
		slog.WarnContext(ctx, "wardynd: recording an Azure DevOps token's failed revoke failed",
			slog.String("run_id", p.RunID.String()), slog.Any("err", nerr))
	}
}

// closeRunPAT marks p's row closed and reports whether this call closed it.
func (s *Server) closeRunPAT(ctx context.Context, st store.RunPATStore, p store.RunPAT, reason, lastError string) bool {
	closed, err := st.MarkRunPATRevoked(ctx, p.RunID, p.AuthorizationID, reason, lastError)
	if err != nil {
		slog.WarnContext(ctx, "wardynd: recording an Azure DevOps token's revoke failed; the sweep retries",
			slog.String("run_id", p.RunID.String()), slog.Any("err", err))
	}
	return closed
}

// deleteRunPAT asks Azure DevOps to revoke p in its owner's name. A token
// Azure DevOps no longer knows is revoked. transient reports whether trying
// again later could succeed.
func (s *Server) deleteRunPAT(ctx context.Context, p store.RunPAT) (transient bool, err error) {
	if s.cfg.ADOEntra == nil {
		return false, errADOPATUnavailable
	}
	// The row is looked up by id: the sign-in source serves only an enabled
	// row, and a disabled one is the most common reason a token is revoked.
	source := s.cfg.ADOEntra
	if byRow := s.cfg.ADOEntraByRow; byRow != nil {
		source = func(ctx context.Context) (ADOEntraConfig, bool, error) { return byRow(ctx, p.ProviderRowID) }
	}
	cfg, found, err := source(ctx)
	switch {
	case err != nil:
		return true, fmt.Errorf("read the sign-in configuration: %w", err)
	case !found || cfg.RowID != p.ProviderRowID:
		return false, errors.New("the sign-in this token was created through is no longer configured")
	}
	client, err := s.runPATClient(cfg)
	if err != nil {
		return false, err
	}
	access, err := s.mintAccess(ctx, cfg, p.Owner)
	if err != nil {
		return ADOEntraClassify(err) == ADOEntraFailureUnavailable && !errors.Is(err, ErrADOMintNeedsSecret), err
	}
	err = client.Revoke(ctx, p.Org, access.AccessToken, p.AuthorizationID.String())
	var pe *adoPATError
	switch {
	case err == nil, errors.As(err, &pe) && pe.Status == http.StatusNotFound:
		return false, nil
	case pe != nil:
		return adoPATTransientStatus(pe.Status), err
	}
	return true, err
}

// sweepRunPATs is the backstop: at most once per adoRunPATSweepEvery it
// revokes the tokens of every run that has ended, is kept after its end, is
// paused or is gone, and closes the rows of tokens past their validTo. On a
// live run it also retries a token whose revoke failed transiently (a drift
// refusal, or a pause revoke followed by a resume); its other older tokens, kept
// by a renewal or a widening, are left to their validTo.
func (s *Server) sweepRunPATs(ctx context.Context) error {
	st, ok := s.cfg.Store.(store.RunPATStore)
	if !ok || !s.adoRunPATs.sweepDue(s.cfg.Now()) {
		return nil
	}
	rows, err := st.ListUnrevokedRunPATs(ctx, store.RunPATFilter{})
	if err != nil {
		return fmt.Errorf("list unrevoked run tokens: %w", err)
	}
	byRun := map[uuid.UUID][]store.RunPAT{}
	var order []uuid.UUID
	for _, p := range rows {
		if byRun[p.RunID] == nil {
			order = append(order, p.RunID)
		}
		byRun[p.RunID] = append(byRun[p.RunID], p)
	}
	for _, runID := range order {
		run, gerr := s.cfg.Store.GetRun(ctx, runID)
		switch {
		case errors.Is(gerr, store.ErrNotFound):
			s.revokeRunPATs(ctx, runID, adoPATRevokeSweep)
		case gerr != nil:
			continue
		case run.State.IsTerminal() || run.LostAt != nil:
			s.revokeRunPATs(ctx, runID, adoPATRevokeSweep)
		case run.PausedAt != nil:
			s.revokeRunPATs(ctx, runID, adoPATRevokePause)
		default:
			s.sweepLiveRunPATs(ctx, st, runID, byRun[runID])
		}
	}
	return nil
}

// sweepLiveRunPATs closes the expired rows of a live run and retries, under
// the run's lock, each unexpired row whose earlier revoke failed transiently
// (LastError is only ever set by that failure on an open row). A row without
// one is a renewal's or a widening's older token and is left alone.
func (s *Server) sweepLiveRunPATs(ctx context.Context, st store.RunPATStore, runID uuid.UUID, rows []store.RunPAT) {
	_, unlock := s.adoRunPATs.lock(runID)
	defer unlock()
	for _, p := range rows {
		if !p.ValidTo.After(s.cfg.Now()) {
			s.closeRunPAT(ctx, st, p, adoPATRevokeExpired, p.LastError)
		} else if p.LastError != "" {
			s.revokeRunPAT(ctx, st, p, adoPATRevokeSweep)
		}
	}
}

// sweepDue reports whether a sweep is due at now, and if so claims it.
func (c *adoRunPATCache) sweepDue(now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.sweptAt.IsZero() && now.Sub(c.sweptAt) < adoRunPATSweepEvery {
		return false
	}
	c.sweptAt = now
	return true
}
