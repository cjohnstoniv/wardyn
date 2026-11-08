// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

// Unit deciders: who besides the security tier may decide each kind of hold for runs launched in a
// unit. One row per unit (governance_unit_deciders); every rule for a kind from the run's unit up the
// tree binds it, met into one rule (MeetDeciderRules), and the nearest unit with a rule is only the
// reference a decision records. With no rule anywhere the verdicts are the ones a deployment without
// deciders gives. The security tier always decides and is never listed.

// The kinds a rule names. tool_call is three kinds here, because a capability escalation, an MCP
// approve-each call and a kubectl approve-each call are different decisions: a rule for one never
// binds another. elevation and image are held requests, not run holds; file_download is a run hold
// (a reviewed download).
const (
	DeciderKindEgressDomain  = "egress_domain"
	DeciderKindADOEscalation = "ado_escalation"
	DeciderKindMCPCall       = "mcp_call"
	DeciderKindKubeCall      = "kube_call"
	DeciderKindCredential    = "credential"
	DeciderKindPushContent   = "push_content"
	DeciderKindElevation     = "elevation"
	DeciderKindImage         = "image"
	DeciderKindFileDownload  = "file_download"
)

// DeciderKinds is the closed set of kinds a rule may name.
var DeciderKinds = []string{
	DeciderKindEgressDomain, DeciderKindADOEscalation, DeciderKindMCPCall, DeciderKindKubeCall,
	DeciderKindCredential, DeciderKindPushContent, DeciderKindElevation, DeciderKindImage, DeciderKindFileDownload,
}

// deciderOwnRuleKinds are the kinds a rule names alone: each states its own second_human and limits.
var deciderOwnRuleKinds = []string{DeciderKindElevation, DeciderKindImage, DeciderKindFileDownload}

// deciderOwnerRefusedKinds are the kinds the requester or run owner never decides: deciding would
// approve their own request, credential, push or download.
var deciderOwnerRefusedKinds = []string{
	DeciderKindCredential, DeciderKindPushContent, DeciderKindElevation, DeciderKindImage, DeciderKindFileDownload,
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
	// MaxRenewAfter bounds an elevation rule's renew_after; MaxTicketPatternLen its ticket_pattern.
	MaxRenewAfter       = 168 * time.Hour
	MaxTicketPatternLen = 256
	// DeciderLockKnob is the locked_knobs name that stops a unit below the locker from setting its rules.
	DeciderLockKnob = "deciders"
)

// DeciderRule is one rule of a unit: the kinds it governs and who may decide them. Kinds and Who
// always encode as lists; an empty Who means the security tier alone.
type DeciderRule struct {
	Kinds []string `json:"kinds"`
	Who   []string `json:"who"`
	// SecondHuman refuses the run's own people, security included. It is required for credential and
	// push_content, stored as true for file_download, and not allowed for elevation or image, where the
	// requester is always excluded.
	SecondHuman bool `json:"second_human,omitempty"`
	// MaxScope caps how far one decision reaches: once, until or run. Never always. Empty leaves the
	// kind's own range.
	MaxScope ApprovalScope `json:"max_scope,omitempty"`
	// MaxElevation is an elevation rule's longest grant, from 15m to 24h, stored as one canonical
	// duration string ("4h" for "240m").
	MaxElevation string `json:"max_elevation,omitempty"`
	// EligibleRequesters is who may ask for an elevation under the rule: group:<name> entries, at most
	// MaxDeciderWho, canonical as who's groups are. Required on an elevation rule, refused on any other.
	EligibleRequesters []string `json:"eligible_requesters,omitempty"`
	// RenewAfter is the least time from one elevation's end to the next request, for one person in one
	// unit: 0s to 168h, stored canonical. Elevation only.
	RenewAfter string `json:"renew_after,omitempty"`
	// TicketPattern makes a ticket required and is the RE2 pattern it must match whole: at most 256
	// characters, compiled as ^(?:p)$ at write. Elevation only.
	TicketPattern string `json:"ticket_pattern,omitempty"`
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

// RenewAfterDuration is the rule's renew_after, zero when it names none or does not parse.
func (r DeciderRule) RenewAfterDuration() time.Duration {
	d, err := time.ParseDuration(r.RenewAfter)
	if err != nil {
		return 0
	}
	return d
}

// TicketRegexp is the rule's ticket_pattern compiled to match a whole ticket, nil when it names none or
// does not compile (a valid rule always compiles: the write checks it).
func (r DeciderRule) TicketRegexp() *regexp.Regexp {
	if r.TicketPattern == "" {
		return nil
	}
	re, err := regexp.Compile("^(?:" + r.TicketPattern + ")$")
	if err != nil {
		return nil
	}
	return re
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
	r = DeciderRule{Kinds: []string{}, Who: []string{}, SecondHuman: in.SecondHuman, MaxScope: in.MaxScope, MaxElevation: in.MaxElevation,
		EligibleRequesters: in.EligibleRequesters, RenewAfter: in.RenewAfter, TicketPattern: in.TicketPattern}
	if len(in.Kinds) == 0 {
		return r, "kinds: name at least one kind"
	}
	for _, k := range in.Kinds {
		switch {
		case k == string(ApprovalToolCall):
			return r, `kinds: "tool_call" is three kinds here: ado_escalation, mcp_call and kube_call`
		case !slices.Contains(DeciderKinds, k):
			return r, fmt.Sprintf("kinds: %q is not one of %s", k, strings.Join(DeciderKinds, ", "))
		case slices.Contains(r.Kinds, k):
			return r, fmt.Sprintf("kinds: %s is named twice", k)
		}
		r.Kinds = append(r.Kinds, k)
	}
	for _, k := range deciderOwnRuleKinds {
		if r.Names(k) && len(r.Kinds) > 1 {
			return r, fmt.Sprintf("kinds: %s is its own rule: it states its own second_human and limits", k)
		}
	}
	elevation := r.Names(DeciderKindElevation)
	requesterOnly := elevation || r.Names(DeciderKindImage)
	scopeless := requesterOnly || r.Names(DeciderKindFileDownload)
	if msg = validateDeciderWho(&r, in.Who); msg != "" {
		return r, msg
	}
	needsSecond := r.Names(DeciderKindCredential) || r.Names(DeciderKindPushContent)
	switch {
	case needsSecond && !r.SecondHuman:
		return r, "second_human: required for credential and push_content, which a person must not decide for their own run"
	case requesterOnly && r.SecondHuman:
		return r, "second_human: not allowed for elevation or image, where the requester is always excluded"
	case r.SecondHuman && slices.Contains(r.Who, DeciderOwner):
		return r, "who: owner can't be listed with second_human, which refuses the run's own people"
	}
	if r.Names(DeciderKindFileDownload) {
		r.SecondHuman = true // forced: the requester and the run's authors never decide a download
	}
	return validateDeciderLimits(r, elevation, scopeless)
}

// validateDeciderWho normalizes who into r and checks each entry.
func validateDeciderWho(r *DeciderRule, who []string) string {
	if len(who) > MaxDeciderWho {
		return fmt.Sprintf("who: at most %d entries", MaxDeciderWho)
	}
	for _, w := range who {
		switch w = strings.TrimSpace(w); {
		case w == DeciderOwner:
			if slices.ContainsFunc(deciderOwnerRefusedKinds, r.Names) {
				return "who: owner can't decide credential, push_content, elevation, image or file_download: it would approve its own request"
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

// validateDeciderLimits checks max_scope, max_elevation and the elevation-only fields and returns the
// rule with its durations canonical. scopeless kinds (elevation, image, file_download) take no max_scope.
func validateDeciderLimits(r DeciderRule, elevation, scopeless bool) (DeciderRule, string) {
	switch r.MaxScope {
	case "", ScopeOnce, ScopeUntil, ScopeRun:
	case ScopeAlways:
		return r, "max_scope: always is never allowed under a rule: it persists a host beyond the run"
	default:
		return r, fmt.Sprintf("max_scope: %q is not once, until or run", r.MaxScope)
	}
	if scopeless && r.MaxScope != "" {
		return r, "max_scope: does not apply to elevation, image or file_download"
	}
	if !elevation {
		switch {
		case r.MaxElevation != "":
			return r, "max_elevation: only an elevation rule has one"
		case len(r.EligibleRequesters) > 0:
			return r, "eligible_requesters: only for elevation"
		case r.RenewAfter != "":
			return r, "renew_after: only for elevation"
		case r.TicketPattern != "":
			return r, "ticket_pattern: only for elevation"
		}
		return r, ""
	}
	d, err := time.ParseDuration(r.MaxElevation)
	if err != nil || d < MinMaxElevation || d > MaxMaxElevation {
		return r, fmt.Sprintf("max_elevation: required, a duration from %s to %s (got %q)", MinMaxElevation, MaxMaxElevation, r.MaxElevation)
	}
	r.MaxElevation = CanonicalDuration(d)
	return validateElevationFields(r)
}

// validateElevationFields checks an elevation rule's eligible_requesters, renew_after and ticket_pattern
// and returns the rule with the groups folded and renew_after canonical.
func validateElevationFields(r DeciderRule) (DeciderRule, string) {
	if len(r.EligibleRequesters) == 0 {
		return r, "eligible_requesters: required, at least one group:<name>"
	}
	if len(r.EligibleRequesters) > MaxDeciderWho {
		return r, fmt.Sprintf("eligible_requesters: at most %d entries", MaxDeciderWho)
	}
	groups := make([]string, 0, len(r.EligibleRequesters))
	for _, e := range r.EligibleRequesters {
		name, ok := DeciderGroupOf(strings.TrimSpace(e))
		g, canonical := CanonicalGroupSubject(name)
		if !ok || !canonical {
			return r, fmt.Sprintf("eligible_requesters: %q is not group:<name>", e)
		}
		if slices.Contains(groups, DeciderGroupPrefix+g) {
			return r, fmt.Sprintf("eligible_requesters: %s is listed twice", DeciderGroupPrefix+g)
		}
		groups = append(groups, DeciderGroupPrefix+g)
	}
	r.EligibleRequesters = groups
	if r.RenewAfter != "" {
		d, err := time.ParseDuration(r.RenewAfter)
		if err != nil || d < 0 || d > MaxRenewAfter {
			return r, fmt.Sprintf("renew_after: a duration from 0s to %s (got %q)", CanonicalDuration(MaxRenewAfter), r.RenewAfter)
		}
		r.RenewAfter = CanonicalDuration(d)
	}
	if r.TicketPattern != "" {
		if utf8.RuneCountInString(r.TicketPattern) > MaxTicketPatternLen {
			return r, fmt.Sprintf("ticket_pattern: at most %d characters", MaxTicketPatternLen)
		}
		if _, err := regexp.Compile("^(?:" + r.TicketPattern + ")$"); err != nil {
			return r, "ticket_pattern: not a valid RE2 pattern: " + err.Error()
		}
	}
	return r, ""
}

// CanonicalDuration is d as the shortest Go duration string that parses back to it: "4h" for 240m,
// "1h30m" for 90m, never "4h0m0s".
func CanonicalDuration(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}
