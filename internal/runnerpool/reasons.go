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
	"strings"

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
	// The pool's own limits refuse a request that names a pool: a run type or a
	// barrier the pool does not allow. AtCapacity is the reason a run queued behind
	// the pool's concurrent-run cap carries; it refuses only where queueing is impossible.
	ReasonRunTypeNotAllowed Reason = "runner_pool_run_type_not_allowed"
	ReasonBarrierNotAllowed Reason = "runner_pool_barrier_not_allowed"
	ReasonAtCapacity        Reason = "runner_pool_at_capacity"
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
	ReasonRunTypeNotAllowed:  http.StatusUnprocessableEntity,
	ReasonBarrierNotAllowed:  http.StatusUnprocessableEntity,
	ReasonAtCapacity:         http.StatusUnprocessableEntity,
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

// MemberMismatchMsg names the runner the caller asked for: their own runner's
// name, or the id as they sent it. Never another person's runner's name; a
// runner that is not the caller's is answered exactly like an unknown one.
func MemberMismatchMsg(runner, name string) string {
	return fmt.Sprintf("%s is not one of your runners in %s.", runner, name)
}

func HostingMismatchMsg(name, label string) string {
	return fmt.Sprintf("%s is a %s pool, so it can't be used with this runner choice.", name, label)
}

func RunTypeNotAllowedMsg(name string, t types.RunnerPoolRunType) string {
	return fmt.Sprintf("%s doesn't take %s. Choose another pool or run type.", name, t.Plural())
}

// BarrierNotAllowedMsg names the barrier asked for and the ones the pool allows.
func BarrierNotAllowedMsg(name string, asked types.ConfinementClass, allowed []types.ConfinementClass) string {
	labels := make([]string, len(allowed))
	for i, c := range allowed {
		labels[i] = types.BarrierLabel(c)
	}
	list := strings.Join(labels, ", ")
	if n := len(labels); n > 1 {
		list = strings.Join(labels[:n-1], ", ") + " and " + labels[n-1]
	}
	return fmt.Sprintf("%s doesn't allow the %s barrier. It allows %s.", name, types.BarrierLabel(asked), list)
}

// AtCapacityMsg is what a queued run says: it is waiting, not refused.
func AtCapacityMsg(name string, max int) string {
	return fmt.Sprintf("%s is running its limit of %d. This run starts when one finishes.", name, max)
}

func UnavailableServerMsg() string { return "This server does not manage runner pools yet." }
