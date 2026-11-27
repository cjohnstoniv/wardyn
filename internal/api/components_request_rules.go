// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"reflect"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Request rules on custom components (ComponentDefinition.RequestRules): host rules on a component's own exact
// hosts, which a run that attaches the component carries as one host rule set of their own. The switch is
// features.component_request_rules. While it is off, a component that carries rules is refused at every run door,
// never attached without them, and a save that adds or changes rules is refused.

const (
	componentRequestRulesOffMsg    = "This component carries request rules, and request rules are off on this deployment. An admin turns them on with features.component_request_rules.enabled."
	componentRequestRulesUnreadMsg = "This component carries request rules, and whether request rules are on could not be read. Try again in a moment."
)

// componentRequestRulesOn reports whether request rules on custom components are on, through the one reader of a
// feature's switch.
func componentRequestRulesOn(sc types.SiteConfig) bool {
	return siteFeatureOn(sc, featureComponentRequestRules)
}

// componentRequestRulesRefusal is the switch at a save door, asked once the row the save replaces is known: stored
// is that row's definition, nil for a new row. A definition with no rules, or one that sends back the stored rules
// unchanged, saves whatever the switch says, so a component saved while the switch was on can still be edited, and
// its rules removed, while it is off. Any other rules need the switch on, read fresh; a site config that cannot be
// read refuses.
func (s *Server) componentRequestRulesRefusal(ctx context.Context, def types.ComponentDefinition, stored *types.ComponentDefinition) (reason, msg string) {
	if len(def.RequestRules) == 0 {
		return "", ""
	}
	if stored != nil && reflect.DeepEqual(normalizeRequestRules(def.RequestRules), normalizeRequestRules(stored.RequestRules)) {
		return "", ""
	}
	sc, err := s.cfg.Store.GetSiteConfig(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "wardynd: read the site config for a component's request rules; the save is refused", slog.Any("err", err))
		return reasonComponentRequestRulesOff, componentRequestRulesUnreadMsg
	}
	if !componentRequestRulesOn(sc) {
		return reasonComponentRequestRulesOff, componentRequestRulesOffMsg
	}
	return "", ""
}

// normalizeRequestRules is a copy of rules with an empty methods or paths list read as none. Both are omitempty, so
// a stored rule reads back with nil where a client that sends "methods": [] decodes an empty list, and the two are
// the same rule.
func normalizeRequestRules(rules []types.HostRule) []types.HostRule {
	out := make([]types.HostRule, len(rules))
	for i, r := range rules {
		if len(r.Methods) == 0 {
			r.Methods = nil
		}
		if len(r.Paths) == 0 {
			r.Paths = nil
		}
		out[i] = r
	}
	return out
}

// componentRequestRulesAdmitted is the switch at run create, read from the gate's one site config read: a component
// that carries request rules is refused while the switch is off.
func componentRequestRulesAdmitted(adm *componentAdmission, c attachedComponent, i int) *componentRefusal {
	if len(c.snapshot.Definition.RequestRules) == 0 || componentRequestRulesOn(adm.site) {
		return nil
	}
	return componentRefused(c, i, reasonComponentRequestRulesOff,
		"request_rules: request rules are off on this deployment; an admin turns them on with features.component_request_rules.enabled",
		"carries request rules, which are off on this deployment.")
}

// componentRequestRuleSets is one host rule set per attached component that carries request rules, in attach order.
// Sets are never merged across components: each set that names a host must allow a request on it.
func componentRequestRuleSets(attached []attachedComponent) []types.HostRuleSet {
	var out []types.HostRuleSet
	for _, a := range attached {
		if a.builtin != nil || len(a.snapshot.Definition.RequestRules) == 0 {
			continue
		}
		out = append(out, types.HostRuleSet{Rules: a.snapshot.Definition.RequestRules})
	}
	return out
}

// componentRuleSetLimit refuses a run whose host rule sets, its policy's (the governance ceiling's among them) with
// one per component that carries request rules, would number more than types.MaxHostRuleSets. It never trims: a set
// left out would let through what its rules refuse.
func componentRuleSetLimit(spec types.RunPolicySpec, attached []attachedComponent) *componentRefusal {
	sets := componentRequestRuleSets(attached)
	if sets == nil {
		return nil
	}
	if len(types.AppendHostRuleSets(spec.HostRules, sets)) <= types.MaxHostRuleSets {
		return nil
	}
	msg := fmt.Sprintf("This run would carry more than %d host rule sets. Attach fewer components with request rules.", types.MaxHostRuleSets)
	return &componentRefusal{ordinal: -1, reason: reasonComponentRequestRulesLimit,
		refusal: runError(http.StatusUnprocessableEntity, reasonComponentRequestRulesLimit, msg)}
}
