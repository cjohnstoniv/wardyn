// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Unit deciders: who besides the security tier may decide each kind of hold for runs launched in a
// unit. One row per unit (governance_unit_deciders); the nearest unit up the tree with a rule for a
// kind governs it, and with no rule anywhere the verdicts are the ones a deployment without deciders
// gives. The security tier always decides and is never listed.

// The kinds a rule names. tool_call is two kinds here, because a capability escalation and an MCP
// approve-each call are different decisions: a rule for one never binds the other.
const (
	DeciderKindEgressDomain  = "egress_domain"
	DeciderKindADOEscalation = "ado_escalation"
	DeciderKindMCPCall       = "mcp_call"
	DeciderKindCredential    = "credential"
	DeciderKindPushContent   = "push_content"
	DeciderKindElevation     = "elevation"
)

// DeciderKinds is the closed set of kinds a rule may name.
var DeciderKinds = []string{
	DeciderKindEgressDomain, DeciderKindADOEscalation, DeciderKindMCPCall,
	DeciderKindCredential, DeciderKindPushContent, DeciderKindElevation,
}

// The decider types a rule's who may hold. A group is "group:<name>", written as the group subject of
// a grant is (CanonicalGroupSubject).
const (
	// DeciderOwner is the person who created the run: today's member path.
	DeciderOwner = "owner"
	// DeciderUnitAdmins is anyone who administers the run's unit by a grant.
	DeciderUnitAdmins = "unit_admins"
	// DeciderGroupPrefix starts a session-group or App Role decider.
	DeciderGroupPrefix = "group:"
)

const (
	// MaxDeciderWho bounds one rule's who list.
	MaxDeciderWho = 8
	// MinMaxElevation and MaxMaxElevation bound a rule's max_elevation.
	MinMaxElevation = 15 * time.Minute
	MaxMaxElevation = 24 * time.Hour
	// DeciderLockKnob is the locked_knobs name that stops a unit below the locker from setting its rules.
	DeciderLockKnob = "deciders"
)

// DeciderRule is one rule of a unit: the kinds it governs and who may decide them. Kinds and Who
// always encode as lists; an empty Who means the security tier alone.
type DeciderRule struct {
	Kinds []string `json:"kinds"`
	Who   []string `json:"who"`
	// SecondHuman refuses the run's own people, security included. It is required for credential and
	// push_content and not allowed for elevation, where the requester is always excluded.
	SecondHuman bool `json:"second_human,omitempty"`
	// MaxScope caps how far one decision reaches: once, until or run. Never always. Empty leaves the
	// kind's own range.
	MaxScope ApprovalScope `json:"max_scope,omitempty"`
	// MaxElevation is an elevation rule's longest grant, from 15m to 24h, as a Go duration.
	MaxElevation string `json:"max_elevation,omitempty"`
}

// UnitDeciders is the stored rules of one unit.
type UnitDeciders struct {
	UnitID    uuid.UUID     `json:"unit_id"`
	Rules     []DeciderRule `json:"rules"`
	UpdatedBy string        `json:"updated_by,omitempty"`
	UpdatedAt time.Time     `json:"updated_at,omitzero"`
}

// DeciderRuleError is a rule the model refuses, naming it by its position.
type DeciderRuleError struct {
	// Index is the rule's position in the list.
	Index int
	// Msg says what is wrong, field first.
	Msg string
}

func (e *DeciderRuleError) Error() string { return fmt.Sprintf("rules[%d]: %s", e.Index, e.Msg) }

// MaxElevationDuration is the rule's max_elevation, zero when it names none or does not parse.
func (r DeciderRule) MaxElevationDuration() time.Duration {
	d, err := time.ParseDuration(r.MaxElevation)
	if err != nil {
		return 0
	}
	return d
}

// Names reports whether the rule governs kind.
func (r DeciderRule) Names(kind string) bool { return slices.Contains(r.Kinds, kind) }

// DeciderRuleFor is the rule among rules that governs kind. Each kind is named at most once in a valid
// list, so order carries no meaning.
func DeciderRuleFor(rules []DeciderRule, kind string) (DeciderRule, bool) {
	for _, r := range rules {
		if r.Names(kind) {
			return r, true
		}
	}
	return DeciderRule{}, false
}

// DeciderGroupOf is the group name of a "group:<name>" who entry.
func DeciderGroupOf(who string) (string, bool) {
	g, ok := strings.CutPrefix(who, DeciderGroupPrefix)
	return g, ok && g != ""
}

// ValidateDeciderRules checks a unit's rules as a write receives them and returns them normalized:
// groups folded as a grant's subject is, and empty lists where a list was absent. The error is a
// *DeciderRuleError naming the first rule that is refused. An empty list is valid and clears the unit.
func ValidateDeciderRules(rules []DeciderRule) ([]DeciderRule, error) {
	out := make([]DeciderRule, 0, len(rules))
	named := map[string]int{}
	for i, in := range rules {
		r, msg := validateDeciderRule(in)
		if msg == "" {
			for _, k := range r.Kinds {
				if j, dup := named[k]; dup {
					msg = fmt.Sprintf("kinds: %s is already governed by rules[%d]; name each kind once", k, j)
					break
				}
				named[k] = i
			}
		}
		if msg != "" {
			return nil, &DeciderRuleError{Index: i, Msg: msg}
		}
		out = append(out, r)
	}
	return out, nil
}

// validateDeciderRule is one rule's checks; msg is empty when it holds.
func validateDeciderRule(in DeciderRule) (r DeciderRule, msg string) {
	r = DeciderRule{Kinds: []string{}, Who: []string{}, SecondHuman: in.SecondHuman, MaxScope: in.MaxScope, MaxElevation: in.MaxElevation}
	if len(in.Kinds) == 0 {
		return r, "kinds: name at least one kind"
	}
	for _, k := range in.Kinds {
		switch {
		case k == string(ApprovalToolCall):
			return r, `kinds: "tool_call" is two kinds here, ado_escalation and mcp_call`
		case !slices.Contains(DeciderKinds, k):
			return r, fmt.Sprintf("kinds: %q is not one of %s", k, strings.Join(DeciderKinds, ", "))
		case slices.Contains(r.Kinds, k):
			return r, fmt.Sprintf("kinds: %s is named twice", k)
		}
		r.Kinds = append(r.Kinds, k)
	}
	elevation := r.Names(DeciderKindElevation)
	if elevation && len(r.Kinds) > 1 {
		return r, "kinds: elevation is its own rule: it has no scope and no second_human"
	}
	if msg = validateDeciderWho(&r, in.Who, elevation); msg != "" {
		return r, msg
	}
	needsSecond := r.Names(DeciderKindCredential) || r.Names(DeciderKindPushContent)
	switch {
	case needsSecond && !r.SecondHuman:
		return r, "second_human: required for credential and push_content, which a person must not decide for their own run"
	case elevation && r.SecondHuman:
		return r, "second_human: not allowed for elevation, where the requester is always excluded"
	case r.SecondHuman && slices.Contains(r.Who, DeciderOwner):
		return r, "who: owner can't be listed with second_human, which refuses the run's own people"
	}
	return r, validateDeciderLimits(r, elevation)
}

// validateDeciderWho normalizes who into r and checks each entry.
func validateDeciderWho(r *DeciderRule, who []string, elevation bool) string {
	if len(who) > MaxDeciderWho {
		return fmt.Sprintf("who: at most %d entries", MaxDeciderWho)
	}
	for _, w := range who {
		switch w = strings.TrimSpace(w); {
		case w == DeciderOwner:
			if elevation || r.Names(DeciderKindCredential) || r.Names(DeciderKindPushContent) {
				return "who: owner can't decide credential, push_content or elevation: it would approve its own request"
			}
		case w == DeciderUnitAdmins:
		case w == "security":
			return "who: security always decides and is not listed"
		case w == "manager" || w == "delegate":
			return fmt.Sprintf("who: %s is not a decider type", w)
		case strings.HasPrefix(w, DeciderGroupPrefix):
			g, ok := CanonicalGroupSubject(strings.TrimPrefix(w, DeciderGroupPrefix))
			if !ok {
				return fmt.Sprintf("who: %q does not name a group (printable ASCII, not empty)", w)
			}
			w = DeciderGroupPrefix + g
		default:
			return fmt.Sprintf("who: %q is not owner, unit_admins or group:<name>", w)
		}
		if slices.Contains(r.Who, w) {
			return fmt.Sprintf("who: %s is listed twice", w)
		}
		r.Who = append(r.Who, w)
	}
	return ""
}

// validateDeciderLimits checks max_scope and max_elevation.
func validateDeciderLimits(r DeciderRule, elevation bool) string {
	switch r.MaxScope {
	case "", ScopeOnce, ScopeUntil, ScopeRun:
	case ScopeAlways:
		return "max_scope: always is never allowed under a rule: it persists a host beyond the run"
	default:
		return fmt.Sprintf("max_scope: %q is not once, until or run", r.MaxScope)
	}
	if elevation && r.MaxScope != "" {
		return "max_scope: does not apply to elevation"
	}
	if !elevation {
		if r.MaxElevation != "" {
			return "max_elevation: only an elevation rule has one"
		}
		return ""
	}
	d, err := time.ParseDuration(r.MaxElevation)
	if err != nil || d < MinMaxElevation || d > MaxMaxElevation {
		return fmt.Sprintf("max_elevation: required, a duration from %s to %s (got %q)", MinMaxElevation, MaxMaxElevation, r.MaxElevation)
	}
	return ""
}
