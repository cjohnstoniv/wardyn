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
// since the tail read it makes the kill store.ErrSuperseded, with no transition, no intent and no teardown.
type killGuard struct {
	led store.PortalSuspensionLedger
	id  uuid.UUID
	key store.JobKey
	at  time.Time
}

type killGuardKey struct{}

func withKillGuard(ctx context.Context, g killGuard) context.Context {
	return context.WithValue(ctx, killGuardKey{}, g)
}

func killGuardOf(ctx context.Context) (killGuard, bool) {
	g, ok := ctx.Value(killGuardKey{}).(killGuard)
	return g, ok
}

// claimGuardedKill is claimKill for a kill made under a killGuard. It is the same claim: a kill that is not the kept
// machine's owner's marks it for delete in the claim itself, and the transition's side effects are the generic
// ones (claimKillTransitionWith). A queued run leaves through the queue's own path, which has no transaction to
// join, so there the check made just before it stands.
func (s *Server) claimGuardedKill(ctx context.Context, run types.AgentRun, killerType types.ActorType, killer string, g killGuard) (bool, store.KeptMachineDeletion, error) {
	if run.State == types.RunQueued {
		if err := g.led.ClaimSuspensionJobAt(ctx, g.id, g.key, g.at); err != nil {
			return false, store.KeptMachineDeletion{}, err
		}
		applied, err := s.claimKillTransition(ctx, run)
		return applied, store.KeptMachineDeletion{}, err
	}
	kill := store.RunKill{RunID: run.ID, From: run.State}
	_, hasKept := s.cfg.Store.(store.KeptMachineStore)
	nonOwner := killerType == serviceActorType || killerType == types.ActorHuman && killer != run.CreatedBy
	if hasKept && nonOwner && !run.State.IsTerminal() {
		kill.MarkKept, kill.By, kill.Killer, kill.Exempt = true, keptMachineByNonOwnerKill, killer, types.CanonicalUserSubject(killer)
	}
	var intent store.KeptMachineDeletion
	applied, err := s.claimKillTransitionWith(ctx, run, func() (bool, error) {
		release := func() {}
		if kill.MarkKept || run.State == types.RunRunning {
			release = s.FenceRunStarts(ctx, run.ID)
		}
		applied, d, err := g.led.ClaimSuspensionKillAt(ctx, g.id, g.key, g.at, kill)
		release()
		s.runStateMoved(run.ID, run.State, types.RunKilled, applied)
		intent = d
		return applied, err
	})
	return applied, intent, err
}
