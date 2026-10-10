// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package runnerpool is the pure half of the runner pool contract: the closed
// set of refusal reasons with their sentences, the default-precedence resolver
// and the rule that only a person's own runners are ever candidates. It reads no
// store and touches no network; the storage and the admission fold call it.
package runnerpool

import (
	"cmp"
	"fmt"
	"net/http"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Reason is a machine-readable refusal reason of pool selection. The set is
// closed; ui/src/app/lib/runner-pool-refusals.ts repeats it and a test pins the
// two together.
type Reason string

const (
	ReasonInvalid            Reason = "runner_pool_invalid"
	ReasonRequired           Reason = "runner_pool_required"
	ReasonNotFound           Reason = "runner_pool_not_found"
	ReasonUnavailable        Reason = "runner_pool_unavailable"
	ReasonDefaultUnavailable Reason = "runner_pool_default_unavailable"
	ReasonStale              Reason = "runner_pool_stale"
	ReasonNoEligibleMember   Reason = "runner_pool_no_eligible_member"
	ReasonMemberMismatch     Reason = "runner_pool_member_mismatch"
	ReasonPoolsUnavailable   Reason = "runner_pools_unavailable"
)

var reasonStatus = map[Reason]int{
	ReasonInvalid:            http.StatusBadRequest,
	ReasonRequired:           http.StatusUnprocessableEntity,
	ReasonNotFound:           http.StatusNotFound,
	ReasonUnavailable:        http.StatusUnprocessableEntity,
	ReasonDefaultUnavailable: http.StatusUnprocessableEntity,
	ReasonStale:              http.StatusConflict,
	ReasonNoEligibleMember:   http.StatusUnprocessableEntity,
	ReasonMemberMismatch:     http.StatusUnprocessableEntity,
	ReasonPoolsUnavailable:   http.StatusNotImplemented,
}

// Status is the HTTP status the reason answers with; 0 for a reason outside the set.
func (r Reason) Status() int { return reasonStatus[r] }

// Valid reports whether r is in the closed set.
func (r Reason) Valid() bool { _, ok := reasonStatus[r]; return ok }

// Reasons lists the closed set, unordered.
func Reasons() []Reason {
	out := make([]Reason, 0, len(reasonStatus))
	for r := range reasonStatus {
		out = append(out, r)
	}
	return out
}

// HostingLabel is the name the console gives a hosting type.
func HostingLabel(h types.RunnerPoolHosting) string {
	switch h {
	case types.RunnerPoolSelfHosted:
		return "Self-Hosted"
	case types.RunnerPoolRemoteProvided:
		return "Remote Provided"
	}
	return ""
}

// The sentences. The console prints the server's sentence on a refusal, so each
// has a TypeScript twin in runner-pool-refusals.ts, and
// TestRefusalSentencesMatchGolden pins both to runner-pool-refusals.golden.json.
// Change a sentence here, there and in that table together.

func InvalidMsg() string { return "runner_pool_id must be the id of a pool from your pool list." }

// RequiredMsg names the hosting type when one is known and says "runner" otherwise.
func RequiredMsg(label string) string {
	return fmt.Sprintf("Choose a %s pool. No default pool applies to this run.", cmp.Or(label, "runner"))
}

func NotFoundMsg() string { return "That pool isn't available to you. Choose one from the list." }

func UnavailableMsg(name string) string {
	return fmt.Sprintf("%s is switched off right now. Choose another pool.", name)
}

func DefaultUnavailablePersonalMsg() string {
	return "Your default pool is no longer available. Choose a pool to continue."
}

func DefaultUnavailableOrgMsg() string {
	return "Your organisation's default pool is no longer available. Choose a pool to continue."
}

func StaleMsg(name string) string {
	return fmt.Sprintf("%s changed while this run was being set up. Review it and try again.", name)
}

func NoEligibleMemberMsg(name string) string {
	return fmt.Sprintf("No runner in %s can run this right now.", name)
}

func NoOwnRunnerMsg(name string) string {
	return fmt.Sprintf("None of your own runners in %s can run this right now.", name)
}

func MemberMismatchMsg(runner, name string) string {
	return fmt.Sprintf("%s is not one of your runners in %s.", runner, name)
}

func HostingMismatchMsg(name, label string) string {
	return fmt.Sprintf("%s is a %s pool, so it can't be used with this runner choice.", name, label)
}

func UnavailableServerMsg() string { return "This server does not manage runner pools yet." }
