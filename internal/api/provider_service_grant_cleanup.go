// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// The clean-up of a removed service grant's secret record. A removal writes a durable claim on the retired uid
// with the change; the door tries the delete at once, and whatever it could not finish is left to
// RunServiceGrantCleanup, which cmd/wardynd starts on the elected sweeper leader, and to the operator's retry
// route. An attempt takes the document lock only to read and to settle, never around the external delete.

import (
	"context"
	"log/slog"
	"net/http"
	"slices"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/cjohnstoniv/wardyn/internal/store"
)

const (
	serviceGrantCleanupEvery     = time.Minute
	serviceGrantCleanupPerPass   = 8
	serviceGrantCleanupBackoff   = time.Minute
	serviceGrantCleanupBackoffMx = 30 * time.Minute
)

type serviceGrantBackoff struct {
	next  time.Time
	delay time.Duration
}

// cleanupOutcome is what one clean-up attempt ended with.
type cleanupOutcome int

const (
	cleanupSettled cleanupOutcome = iota
	cleanupKept
	cleanupRevisionChanged
	cleanupUnsupported
	cleanupNotFound
	cleanupProceed
)

// RunServiceGrantCleanup works through due clean-up claims until ctx ends: every 60 seconds at most 8, oldest
// first, one at a time. It shares nothing with the token refresher or the replica heartbeat, so a slow store
// delays neither. A claim that fails is due again after a minute, doubling to 30 minutes, kept in this
// process's memory so that a new leader starts with every claim due. A failed attempt writes no audit row.
func (s *Server) RunServiceGrantCleanup(ctx context.Context) {
	t := time.NewTicker(serviceGrantCleanupEvery)
	defer t.Stop()
	for {
		s.serviceGrantCleanupPassSafely(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s *Server) serviceGrantCleanupPassSafely(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			slog.ErrorContext(ctx, "wardynd: the service grant clean-up pass panicked", slog.Any("panic", r))
		}
	}()
	s.serviceGrantCleanupPass(ctx)
}

// serviceGrantCleanupPass attempts the due claims, at most serviceGrantCleanupPerPass of them.
func (s *Server) serviceGrantCleanupPass(ctx context.Context) {
	gs, ok := s.serviceGrantStore()
	if !ok {
		return
	}
	doc, err := gs.ServiceGrantDoc(ctx)
	if err != nil {
		slog.WarnContext(ctx, "wardynd: could not read the service grant clean-up claims", slog.Any("err", err))
		return
	}
	uids := sortedCleanupUIDs(doc.Cleanups)
	slices.SortStableFunc(uids, func(a, b string) int { return doc.Cleanups[a].Since.Compare(doc.Cleanups[b].Since) })
	attempted := 0
	for _, uid := range uids {
		if attempted >= serviceGrantCleanupPerPass || ctx.Err() != nil {
			return
		}
		if !s.serviceGrantCleanupDue(uid) {
			continue
		}
		attempted++
		out := s.serviceGrantCleanupAttempt(ctx, uid)
		s.noteServiceGrantCleanup(uid, out)
		if out != cleanupSettled {
			slog.WarnContext(ctx, "wardynd: a removed service grant's secret is still to be deleted", slog.String("uid", uid), slog.Int("outcome", int(out)))
		}
	}
}

func (s *Server) serviceGrantCleanupDue(uid string) bool {
	s.svcGrant.mu.Lock()
	defer s.svcGrant.mu.Unlock()
	b, ok := s.svcGrant.cleanups[uid]
	return !ok || !s.cfg.Now().Before(b.next)
}

// noteServiceGrantCleanup keeps or clears a claim's backoff after an attempt.
func (s *Server) noteServiceGrantCleanup(uid string, out cleanupOutcome) {
	s.svcGrant.mu.Lock()
	defer s.svcGrant.mu.Unlock()
	if out == cleanupSettled || out == cleanupNotFound {
		delete(s.svcGrant.cleanups, uid)
		return
	}
	if s.svcGrant.cleanups == nil {
		s.svcGrant.cleanups = map[string]serviceGrantBackoff{}
	}
	b := s.svcGrant.cleanups[uid]
	b.delay = min(max(b.delay*2, serviceGrantCleanupBackoff), serviceGrantCleanupBackoffMx)
	b.next = s.cfg.Now().Add(b.delay)
	s.svcGrant.cleanups[uid] = b
}

// serviceGrantCleanupAttempt is one attempt on the claim for uid.
//
//  1. Under the document lock: read the claim again and the record's revision. Absence settles the claim; the
//     claim's own revision lets the attempt go on; another revision, a store that keeps none, or a read error keeps it.
//  2. Without the lock: delete with the claim's revision.
//  3. Under the lock again, whatever the delete answered: absence settles the claim, anything else keeps it.
func (s *Server) serviceGrantCleanupAttempt(ctx context.Context, uid string) cleanupOutcome {
	gs, ok := s.serviceGrantStore()
	if !ok {
		return cleanupKept
	}
	claim, out := s.serviceGrantCleanupRead(ctx, gs, uid)
	if out != cleanupProceed {
		return out
	}
	dctx, cancel := context.WithTimeout(ctx, serviceGrantCleanupAttemptBound)
	rev := claim.Revision
	_ = s.deleteServiceGrantRecord(dctx, serviceGrantSecretName(uid), &rev)
	cancel()
	if s.svcGrant.stop("after_delete") {
		return cleanupKept
	}
	return s.serviceGrantCleanupSettle(ctx, gs, uid, claim)
}

// serviceGrantCleanupRead is step 1, under the document lock.
func (s *Server) serviceGrantCleanupRead(ctx context.Context, gs store.ServiceGrantStore, uid string) (store.ServiceGrantCleanup, cleanupOutcome) {
	lctx, cancel := context.WithTimeout(ctx, serviceGrantCleanupAttemptBound)
	defer cancel()
	lctx, unlock, err := s.serviceGrantLock(lctx)
	if err != nil {
		return store.ServiceGrantCleanup{}, cleanupKept
	}
	defer unlock()
	doc, err := gs.ServiceGrantDoc(lctx)
	if err != nil {
		return store.ServiceGrantCleanup{}, cleanupKept
	}
	claim, found := doc.Cleanups[uid]
	if !found {
		return claim, cleanupNotFound
	}
	rev, revOK, err := s.serviceGrantRevision(lctx, serviceGrantSecretName(uid))
	switch {
	case err != nil:
		return claim, cleanupKept
	case !revOK:
		return claim, cleanupUnsupported
	case rev == "":
		return claim, s.settleServiceGrantClaim(lctx, gs, uid, claim)
	case rev == claim.Revision:
		return claim, cleanupProceed
	}
	return claim, cleanupRevisionChanged
}

// serviceGrantCleanupSettle is step 3, under the document lock.
func (s *Server) serviceGrantCleanupSettle(ctx context.Context, gs store.ServiceGrantStore, uid string, claim store.ServiceGrantCleanup) cleanupOutcome {
	lctx, cancel := context.WithTimeout(ctx, serviceGrantCleanupAttemptBound)
	defer cancel()
	lctx, unlock, err := s.serviceGrantLock(lctx)
	if err != nil {
		return cleanupKept
	}
	defer unlock()
	rev, revOK, err := s.serviceGrantRevision(lctx, serviceGrantSecretName(uid))
	if err != nil || !revOK || rev != "" {
		return cleanupKept
	}
	return s.settleServiceGrantClaim(lctx, gs, uid, claim)
}

func (s *Server) settleServiceGrantClaim(ctx context.Context, gs store.ServiceGrantStore, uid string, claim store.ServiceGrantCleanup) cleanupOutcome {
	if err := gs.SettleServiceGrantCleanup(ctx, uid); err != nil {
		return cleanupKept
	}
	s.auditServiceGrantSecretDelete(ctx, uid, claim.ProviderID, true)
	return cleanupSettled
}

// handleRetryServiceGrantCleanup is POST /api/v1/service-grant-cleanups/{uid}/retry: one attempt at once, on
// the replica that answers. The retry names the retired uid, never a provider id, so it can reach only the
// record its claim was written for.
func (s *Server) handleRetryServiceGrantCleanup(w http.ResponseWriter, r *http.Request) {
	uid := chi.URLParam(r, "uid")
	switch out := s.serviceGrantCleanupAttempt(r.Context(), uid); out {
	case cleanupSettled:
		s.noteServiceGrantCleanup(uid, out)
		w.WriteHeader(http.StatusNoContent)
	case cleanupNotFound:
		writeErrorReason(w, http.StatusNotFound, reasonServiceGrantCleanupNotFound, "no clean-up names that uid")
	case cleanupRevisionChanged:
		writeErrorReason(w, http.StatusConflict, reasonServiceGrantCleanupRevisionChanged,
			"the secret record "+uid+" was written outside Wardyn after its grant was removed; Wardyn deletes nothing it did not write. Report this uid to your administrator")
	case cleanupUnsupported:
		writeErrorReason(w, http.StatusConflict, reasonServiceGrantCleanupUnsupported,
			"the secret store keeps no revision, so Wardyn deletes nothing; the claim is kept")
	default:
		s.noteServiceGrantCleanup(uid, out)
		writeErrorReason(w, http.StatusServiceUnavailable, reasonServiceGrantSecretKept, "the secret's deletion did not finish; the claim is kept and retried")
	}
}
