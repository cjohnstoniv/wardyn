// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"
	"slices"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The trust class through the local placement doors (K1 §2). A runner's trust class is stored and written
// only by a security operator (runner_registration_trust.go); the delivery policy is read at it
// (placement.DeliveryPolicy.ForTrust). Create judges a run's plan at each trust class among its eligible
// runners, highest first, records the first that admits it (agent_runs.runner_trust_class) and binds the
// run to a runner of that class. Dispatch takes the bound runner only when its class still gives every
// persisted grant its persisted mode; a revive refuses runner_trust_changed when the class differs.

// localJudgement is how a local run's credential plan was admitted: the delivery policy it was read
// from and the trust class it was read at. Create persists the grants by the same judgement.
type localJudgement struct {
	policy placement.DeliveryPolicy
	trust  types.RunnerTrustClass
}

// candidateTrusts is the distinct trust classes of the run's eligible runners in the order create judges
// them: the class of prefer (the runner a promotion found room on) first, then the classes with a runner
// that has room, then the rest, each by rank. A full runner of a higher class never hides a lower class
// with room. With no runner among the targets (an unpooled request, or one waiting on remote-hosted
// runners) it is every class, so the plan's own refusal is the one any runner would give.
func candidateTrusts(targets []poolTarget, prefer *uuid.UUID) []types.RunnerTrustClass {
	if !slices.ContainsFunc(targets, func(t poolTarget) bool { return t.runnerID != nil }) {
		return placement.TrustClassesByRank()
	}
	var out []types.RunnerTrustClass
	add := func(keep func(t poolTarget) bool) {
		for _, c := range placement.TrustClassesByRank() {
			if !slices.Contains(out, c) && slices.ContainsFunc(targets, func(t poolTarget) bool { return t.runnerID != nil && t.trust == c && keep(t) }) {
				out = append(out, c)
			}
		}
	}
	add(func(t poolTarget) bool { return prefer != nil && *t.runnerID == *prefer })
	add(func(t poolTarget) bool { return !t.full })
	add(func(poolTarget) bool { return true })
	return out
}

// waitingLocalTrusts is the classes a person's own runner can hold (org_cluster is an organization's
// only): what a run waiting on remote-hosted runners is judged at for the person's own.
func waitingLocalTrusts() []types.RunnerTrustClass {
	return []types.RunnerTrustClass{types.RunnerTrustManagedDevice, types.RunnerTrustPersonal}
}

// judgeAtTrust judges p at each class in turn and answers the first that admits it. When none does, the
// refusal is the first class's, which names that class.
func judgeAtTrust(p placement.LocalPlan, classes []types.RunnerTrustClass) (types.RunnerTrustClass, *placement.Refusal) {
	var first *placement.Refusal
	for _, c := range classes {
		p.Trust = c
		_, ref := placement.LocalEligibility(p)
		if ref == nil {
			return c, nil
		}
		if first == nil {
			first = ref
		}
	}
	if first == nil {
		first = &placement.Refusal{Reason: placement.ReasonPlacementCredential, Field: "trust_class", Detail: "no runner of a known trust class can take this run"}
	}
	return "", first
}

// narrowToTrust keeps the run's eligible runners of class c: the bind then picks one the plan was
// admitted at. Remote-hosted targets are left as they are. Whether every runner left is full is
// recomputed from the narrowed set.
func (p *runPlacement) narrowToTrust(c types.RunnerTrustClass) {
	p.eligible = slices.DeleteFunc(slices.Clone(p.eligible), func(t poolTarget) bool { return t.runnerID != nil && t.trust != c })
	noRoom := !slices.ContainsFunc(p.eligible, func(t poolTarget) bool { return !t.full })
	p.runnersFull = p.queues() && len(p.eligible) > 0 && noRoom
	p.waitsForRoom = p.waitsForRoom && noRoom
}

// recordRunTrust stores the class a local run was admitted at, before its grants. A store that keeps
// no run trust records none, and a revive there refuses.
func (s *Server) recordRunTrust(ctx context.Context, runID uuid.UUID, c types.RunnerTrustClass) error {
	ts, ok := s.cfg.Store.(store.RunTrustStore)
	if !ok {
		return nil
	}
	return ts.SetRunRunnerTrustClass(ctx, runID, c)
}

// grantModesRefusal is dispatch's trust door: the bound runner's trust class must give every grant
// recorded via_org or runner_resident its recorded mode. An own grant is the person's own material,
// which the policy does not deliver.
func grantModesRefusal(p placement.LocalPlan, rows []types.CredentialGrant, classOf func(types.CredentialGrant) (string, bool)) *placement.Refusal {
	for _, g := range rows {
		if g.Delivery != types.GrantDeliveryViaOrg && g.Delivery != types.GrantDeliveryResident {
			continue
		}
		class, ok := classOf(g)
		if !ok || p.Delivery.ForTrust(class, p.Trust).Mode != placement.Mode(g.Delivery) {
			return &placement.Refusal{Reason: placement.ReasonPlacementCredential, Field: "eligible_grants",
				Detail: fmt.Sprintf("a %s credential recorded %s is not delivered that way on a runner of trust class %s", class, g.Delivery, p.Trust)}
		}
	}
	return nil
}

// trustChangedRefusal is the revive's trust door: the bound runner's class must be the one the run was
// admitted at. A run with no recorded class is refused alike.
func (s *Server) trustChangedRefusal(ctx context.Context, run types.AgentRun, current types.RunnerTrustClass) error {
	ts, ok := s.cfg.Store.(store.RunTrustStore)
	if !ok {
		return &placement.Refusal{Reason: placement.ReasonRunnerTrustChanged, Field: "trust_class", Detail: "the run's admitted trust class cannot be read"}
	}
	recorded, err := ts.RunRunnerTrustClass(ctx, run.ID)
	if err != nil {
		return err
	}
	if !current.Valid() || recorded != current {
		return &placement.Refusal{Reason: placement.ReasonRunnerTrustChanged, Field: "trust_class",
			Detail: fmt.Sprintf("the runner's trust class is %s; the run was admitted at %s", current, trustLabel(recorded))}
	}
	return nil
}

func trustLabel(c types.RunnerTrustClass) string {
	if c == "" {
		return "no recorded class"
	}
	return string(c)
}
