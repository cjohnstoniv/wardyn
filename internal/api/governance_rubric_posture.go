// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// The rubric half of a profile write's advisory: what the autonomy rubric can actually see on this
// deployment. An admin who sets a rubric row cannot tell, at write time, that the deployment pins an
// axis — a substrate that offers one barrier class, a model provider whose credential grades powerful
// for every run on it — so a row they set can cap nothing, or bind every run of a kind. The run create warning names
// the bound row; this names it before anyone launches.
//
// Only LOWER bounds are claimed. A run can always reach wider: a workspace's approved egress, an SCM
// host, a grant the run asks for. So a state is called unreachable only when the deployment forces
// every run above it, and the posture arithmetic is composer's own (AutonomyPostureOf, FoldAutonomy).
package api

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// rubricPostureAdvisory is the profile write's one sentence about the rubric on this deployment, or ""
// when the resolved profile carries no rubric row (nothing to advise on) or the site config cannot be
// read (an advisory graded on a baseline nobody could read is a guess, and the write itself never
// depends on it).
func (s *Server) rubricPostureAdvisory(ctx context.Context, res *ResolvedProfile) string {
	if res == nil || res.Limits.AutonomyRubric == nil || !setsALevel(*res.Limits.AutonomyRubric) {
		return ""
	}
	var sc types.SiteConfig
	if s.cfg.Store != nil {
		var err error
		if sc, err = s.cfg.Store.GetSiteConfig(ctx); err != nil {
			return ""
		}
	}
	launch, provider := launchModelCredentials(sc)
	return rubricPostureSentence(rubricReach{
		baseline: egressBaselineOf(sc), launch: launch, provider: provider,
		offered: s.offeredBarriers(ctx, sc), floor: res.Ceiling.MinConfinementClass,
	})
}

// setsALevel reports whether any of the nine rows caps a level (the guardrail-locks switch caps none).
func setsALevel(a types.AutonomyRubric) bool {
	a.AgentGuardrailLocks = false
	return a != (types.AutonomyRubric{})
}

// offeredBarriers is the barrier classes a run here can land on, nil when that is not knowable at write
// time: the deployment executor's own advertisement (requestCapabilities, the call Review's default
// class reads), unless self-hosted runners are on (runnersEnabled's switch), whose classes are each
// runner's own.
func (s *Server) offeredBarriers(ctx context.Context, sc types.SiteConfig) []types.ConfinementClass {
	if sc.Runners != nil && sc.Runners.Enabled {
		return nil
	}
	pc, err := s.requestCapabilities(ctx, createRunRequest{})
	if err != nil {
		return nil
	}
	return pc.ConfinementClasses
}

// launchModelCredentials is the model credential autonomyPostureSpec folds for each run shape this
// deployment can launch, and the subset for runs on a model provider. Every catalog harness counts as
// launchable: a capability restriction narrows it per person, which a profile write cannot evaluate
// for everyone the profile binds, and counting a harness nobody may launch only over-states what can
// bind. A harness with no model gateway (Your own tools), or one no enabled provider serves, resolves
// no provider (chooseModelProvider) and folds no credential: the empty-host grade, secrets none.
func launchModelCredentials(sc types.SiteConfig) (launch, provider []bedrockCredGrade) {
	for _, h := range harnessCatalog {
		served := false
		for _, p := range modelProviderRows(sc) {
			if h.Gateway == nil || p.Disabled || !p.Serves(h.ID) {
				continue
			}
			g := bedrockCredGrade{graded: true}
			if p.Kind.IsBedrock() {
				g.host = providerBedrockRuntimeHost(p)
			}
			served = true
			provider = append(provider, g)
		}
		if !served {
			launch = append(launch, bedrockCredGrade{graded: true})
		}
	}
	return append(launch, provider...), provider
}

// rubricReach is what the advisory grades from.
type rubricReach struct {
	baseline composer.Baseline
	// launch is the model credential of every run shape this deployment can launch (empty host: none);
	// provider is the subset for runs on a model provider, which the sentence names on its own.
	launch, provider []bedrockCredGrade
	offered          []types.ConfinementClass // nil: every class is reachable
	floor            types.ConfinementClass
}

// secretsReach is the secrets grades a run carrying one of creds can reach: from the weakest
// credential's grade up (a run can always ask for a stronger grant). No credentials: every grade.
func secretsReach(creds []bedrockCredGrade, b composer.Baseline) []types.AutonomySecretsPosture {
	if len(creds) == 0 {
		return secretsOrder
	}
	lowest := len(secretsOrder) - 1
	for _, g := range creds {
		var spec types.RunPolicySpec
		unionBedrockCredential(&spec, g)
		lowest = min(lowest, slices.Index(secretsOrder, composer.AutonomyPostureOf(spec, "", b).Secrets))
	}
	return secretsOrder[lowest:]
}

var (
	egressOrder  = []types.AutonomyEgressPosture{types.AutonomyEgressSealed, types.AutonomyEgressReviewed, types.AutonomyEgressOpen}
	secretsOrder = []types.AutonomySecretsPosture{types.AutonomySecretsNone, types.AutonomySecretsBaseline, types.AutonomySecretsPowerful}
	classOrder   = []types.ConfinementClass{types.CC1, types.CC2, types.CC3}
)

// rubricPostureSentence renders the reach: each axis's reachable grades, the rubric rows they select,
// and the rows nothing here selects.
func rubricPostureSentence(r rubricReach) string {
	// The narrowest run: no egress and no grant, plus the model credential its harness folds.
	floor := composer.AutonomyPostureOf(types.RunPolicySpec{}, "", r.baseline)
	egress := egressOrder[slices.Index(egressOrder, floor.Egress):]
	secrets := secretsReach(r.launch, r.baseline)
	var classes []types.ConfinementClass
	for _, c := range classOrder {
		if (r.offered == nil || slices.Contains(r.offered, c)) && c.Rank() >= r.floor.Rank() {
			classes = append(classes, c)
		}
	}
	if len(classes) == 0 {
		// No offered class meets the floor: such a run is refused, so the axis pins nothing.
		classes = classOrder
	}

	var bindable, inert []string
	row := func(p types.AutonomyPosture) string {
		_, bound := composer.FoldAutonomy(everyRow, p)
		return bound[0]
	}
	place := func(reach bool, name string) {
		if reach {
			bindable = append(bindable, name)
		} else {
			inert = append(inert, name)
		}
	}
	// Rows in the rubric's own field order (types.AutonomyRubric, the editor's and bound_by's order).
	for _, e := range slices.Backward(egressOrder) {
		place(slices.Contains(egress, e), row(types.AutonomyPosture{Egress: e}))
	}
	for _, sp := range slices.Backward(secretsOrder) {
		place(slices.Contains(secrets, sp), row(types.AutonomyPosture{Secrets: sp}))
	}
	for _, c := range classOrder {
		place(slices.Contains(classes, c), row(types.AutonomyPosture{Confinement: c}))
	}

	out := fmt.Sprintf("on this deployment a run under this profile grades egress %s; secrets %s; and confinement %s",
		orList(egress), orList(secrets), orList(classes))
	if len(inert) == 0 {
		out += " — so every rubric row can cap it"
	} else {
		out += fmt.Sprintf(" — so only %s can cap it, and %s cap nothing here", andList(bindable), andList(inert))
	}
	// What a run on a model provider is forced to, when that is more than every run is: the secrets row
	// that binds such a run whatever it asks for.
	if onProvider := secretsReach(r.provider, r.baseline); len(r.provider) > 0 && len(onProvider) < len(secrets) {
		out += fmt.Sprintf("; a run on one of its model providers always grades secrets %s", orList(onProvider))
	}
	return out
}

// everyRow sets all nine rows, so FoldAutonomy names exactly the rows a posture selects.
var everyRow = types.AutonomyRubric{
	EgressOpen: types.AutonomyL0, EgressReviewed: types.AutonomyL0, EgressSealed: types.AutonomyL0,
	SecretsPowerful: types.AutonomyL0, SecretsBaseline: types.AutonomyL0, SecretsNone: types.AutonomyL0,
	ConfinementCC1: types.AutonomyL0, ConfinementCC2: types.AutonomyL0, ConfinementCC3: types.AutonomyL0,
}

func orList[T ~string](xs []T) string  { return joinLast(xs, " or ") }
func andList[T ~string](xs []T) string { return joinLast(xs, " and ") }

func joinLast[T ~string](xs []T, last string) string {
	s := make([]string, len(xs))
	for i, x := range xs {
		s[i] = string(x)
	}
	if len(s) < 2 {
		return strings.Join(s, "")
	}
	return strings.Join(s[:len(s)-1], ", ") + last + s[len(s)-1]
}
