// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// killGuard is the ledger row a portal suspension's tail is about to kill a run for, and the suspension it is bound
// to. A kill made under it (claimKill) makes its KILLED transition and a kept machine's delete intent in one
// transaction that locks that row, pending and of the current suspension, so a suspension that replaced the ledger
// since the tail read it makes the kill store.ErrSuperseded, with no transition, no intent and no teardown. The
// claim also pins the one run.kill event the occurrence writes (rcpt), which the kill's tail records under.
type killGuard struct {
	led  store.PortalSuspensionLedger
	id   uuid.UUID
	key  store.JobKey
	at   time.Time
	rcpt *killReceipt
}

// killReceipt is the id and time of the occurrence's run.kill event, as the claim pinned them.
type killReceipt struct {
	id uuid.UUID
	at time.Time
	// claim is the acquisition this attempt holds (zero until it has acquired one); queued says the claim, the first
	// one of the occurrence, killed the run from the queue, so a repair tears down as a queued kill.
	claim  uuid.UUID
	queued bool
}

type killGuardKey struct{}

func withKillGuard(ctx context.Context, g killGuard) context.Context {
	g.rcpt = &killReceipt{}
	return context.WithValue(ctx, killGuardKey{}, g)
}

func killGuardOf(ctx context.Context) (killGuard, bool) {
	g, ok := ctx.Value(killGuardKey{}).(killGuard)
	return g, ok
}

// killClaimOf is the ownership handle the attempt holding g acquired, zero if it acquired none.
func killClaimOf(g killGuard) uuid.UUID {
	if g.rcpt == nil {
		return uuid.Nil
	}
	return g.rcpt.claim
}

// killWasQueued reports whether the kill under ctx is one of a run that left the queue, the repair of one included.
func killWasQueued(ctx context.Context) bool {
	g, ok := killGuardOf(ctx)
	return ok && g.rcpt != nil && g.rcpt.queued
}

// killReceiptOf rewrites the run.kill event of a kill made under a claimed killGuard into the occurrence's one event
// and marks ctx as a replay: a failed attempt's row has its own id (derived from the pinned one), so the retry that
// succeeds adds its row, while a repeat of an attempt that was recorded adds none. Other kills are unchanged.
func killReceiptOf(ctx context.Context, ev types.AuditEvent) (context.Context, types.AuditEvent) {
	g, ok := killGuardOf(ctx)
	if !ok || g.rcpt == nil || g.rcpt.id == uuid.Nil {
		return ctx, ev
	}
	ev.ID, ev.Time = g.rcpt.id, g.rcpt.at
	if ev.Outcome != "success" {
		ev.ID = uuid.NewSHA1(g.rcpt.id, []byte(ev.Outcome))
	}
	return store.WithAuditReplay(ctx), ev
}

// claimGuardedKill is claimKill for a kill made under a killGuard. It is the same claim: a kill that is not the kept
// machine's owner's marks it for delete in the claim itself, and the transition's side effects are the generic
// ones (claimKillTransitionWith). A queued run leaves the queue in the claim's transaction too. A run that is
// already KILLED is claimed for repair without being written again, and not while another claim of the occurrence is
// live (store.ErrKillHeld).
func (s *Server) claimGuardedKill(ctx context.Context, run types.AgentRun, killerType types.ActorType, killer string, g killGuard) (bool, store.KeptMachineDeletion, error) {
	kill := store.RunKill{RunID: run.ID, From: run.State, ReceiptID: uuid.New(), ReceiptAt: s.cfg.Now().UTC(), ClaimID: uuid.New()}
	_, hasKept := s.cfg.Store.(store.KeptMachineStore)
	nonOwner := killerType == serviceActorType || killerType == types.ActorHuman && killer != run.CreatedBy
	if hasKept && nonOwner && !run.State.IsTerminal() && run.State != types.RunQueued {
		kill.MarkKept, kill.By, kill.Killer, kill.Exempt = true, keptMachineByNonOwnerKill, killer, types.CanonicalUserSubject(killer)
	}
	var intent store.KeptMachineDeletion
	applied, err := s.claimKillTransitionWith(ctx, run, func() (bool, error) {
		release := func() {}
		if kill.MarkKept || run.State == types.RunRunning {
			release = s.FenceRunStarts(ctx, run.ID)
		}
		c, err := g.led.ClaimSuspensionKillAt(ctx, g.id, g.key, g.at, kill)
		release()
		if err == nil && run.State != types.RunKilled {
			s.runStateMoved(run.ID, run.State, types.RunKilled, c.Applied)
		}
		if c.Applied {
			g.rcpt.id, g.rcpt.at, g.rcpt.claim, g.rcpt.queued = c.ReceiptID, c.ReceiptAt, c.ClaimID, c.From == types.RunQueued
		}
		intent = c.Intent
		return c.Applied, err
	})
	return applied, intent, err
}
