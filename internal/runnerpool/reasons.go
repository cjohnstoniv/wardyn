// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package runnerpool is the pure half of the runner pool contract: the closed
// set of refusal reasons with their sentences, the default-precedence resolver
// and the rule that only a person's own runners are ever candidates. It reads no
// store and touches no network; the storage and the admission fold call it.
package runnerpool

import (
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
	// barrier the pool does not allow. A pool at its concurrent-run cap is not a
	// refusal: see Wait.
	ReasonRunTypeNotAllowed Reason = "runner_pool_run_type_not_allowed"
	// ReasonRunTypeMissing: the request carries no run type (an older client's shape), so a pool with
	// limits cannot tell which of its run types applies. An unknown value is ReasonRunTypeNotAllowed.
	ReasonRunTypeMissing    Reason = "runner_pool_run_type_missing"
	ReasonBarrierNotAllowed Reason = "runner_pool_barrier_not_allowed"
	// The pool queue's two refusals (a wait is never one: see Wait). QueueFull answers a create when the
	// person already has the most runs waiting for the pool; Full answers a revive into a pool at its cap
	// or with others waiting.
	ReasonQueueFull Reason = "runner_pool_queue_full"
	ReasonFull      Reason = "runner_pool_full"
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
	ReasonRunTypeMissing:     http.StatusUnprocessableEntity,
	ReasonBarrierNotAllowed:  http.StatusUnprocessableEntity,
	ReasonQueueFull:          http.StatusUnprocessableEntity,
	ReasonFull:               http.StatusUnprocessableEntity,
}

// What a pool row's reason holds when the pool cannot take a run for the caller, beyond the refusal
// reasons above: GET /runner-pools and the dry doors answer 200 with them. A launch on such a pool
// answers the same token, except a pool whose own runners are all offline, which create answers
// runner_offline with NoOnlineRunnerMsg. The two placement tokens are placement.Reason's.
const (
	AvailabilityNoOnlineRunner       Reason = "runner_pool_no_online_runner"
	AvailabilityPlacementDenied      Reason = "placement_denied"
	AvailabilityPlacementUnavailable Reason = "placement_unavailable"
)

// AvailabilityReasons is the closed set a pool row's reason may hold, in the order the list checks
// them. runner-pool-refusals.ts repeats it as POOL_AVAILABILITY_REASONS.
func AvailabilityReasons() []Reason {
	return []Reason{ReasonUnavailable, AvailabilityPlacementDenied, AvailabilityPlacementUnavailable, ReasonNoEligibleMember,
		AvailabilityNoOnlineRunner, ReasonRunTypeNotAllowed, ReasonNotFound}
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

// The sentences. The console prints the server's sentence on a refusal, so each
// has a TypeScript twin in runner-pool-refusals.ts, and
// TestRefusalSentencesMatchGolden pins both to runner-pool-refusals.golden.json.
// Change a sentence here, there and in that table together.

func InvalidMsg() string { return "runner_pool_id must be the id of a pool from your pool list." }

// RequiredMsg: no pool was named and no default applies.
func RequiredMsg() string { return "Choose a runner pool. No default pool applies to this run." }

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

// NoOnlineRunnerMsg is the one sentence for a user pool whose own runners are all offline: the
// pool list's reason, the dry doors' blocker and the create refusal.
func NoOnlineRunnerMsg(name string) string {
	return fmt.Sprintf("None of your runners in %s is online.", name)
}

// RunnerAtCapacityMsg is the create and preflight refusal (runner_at_capacity) for a run that would
// start at once while every runner in the pool that could take it is at one of its limits: its Runs at
// once, or a total this run would pass.
func RunnerAtCapacityMsg(name string) string {
	return fmt.Sprintf("Every runner in %s is at its limit. Try again when a run there finishes, or raise the runner's limits.", name)
}

// RunnerLimitsUnsetMsg is the refusal (runner_limits_unset) when no runner in the pool has declared
// its limits, so none may take a run.
func RunnerLimitsUnsetMsg(name string) string {
	return fmt.Sprintf("No runner in %s has its limits set. Every runner needs limits before it can run anything.", name)
}

// MemberMismatchMsg names the runner the caller asked for: their own runner's
// name, or the id as they sent it. Never another person's runner's name; a
// runner that is not the caller's is answered exactly like an unknown one.
func MemberMismatchMsg(runner, name string) string {
	return fmt.Sprintf("%s is not one of your runners in %s.", runner, name)
}

// KindMismatchMsg: the pool has no runner of the kind the request's placement asks for.
func KindMismatchMsg(name string) string {
	return fmt.Sprintf("%s has no runner of the kind this run asks for. Choose another pool.", name)
}

// RunTypeMissingMsg: the request names no run type, so a pool with limits has none to check.
func RunTypeMissingMsg() string {
	return "This request has no run type, which older clients leave out. Set experience (--experience on the CLI) to background or interactive, or upgrade the client."
}

func RunTypeNotAllowedMsg(name string, t types.RunnerPoolRunType) string {
	kind := t.Plural()
	if kind == "" {
		kind = "this kind of run"
	}
	return fmt.Sprintf("%s doesn't take %s. Choose another pool or run type.", name, kind)
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

func UnavailableServerMsg() string { return "This server does not manage runner pools yet." }

// QueueFullMsg: the person already has n runs waiting for the pool.
func QueueFullMsg(name string, n int) string {
	return fmt.Sprintf("You already have %d runs waiting for %s. Cancel one or wait for one to start.", n, name)
}

// FullMsg: a revive finds the pool at its limit of n, or others waiting ahead of it.
func FullMsg(name string, n int) string {
	return fmt.Sprintf("%s is running its limit of %d, and others are waiting. Try again when one finishes.", name, n)
}

// OrgPoolSelfHostedMsg refuses a self-hosted runner offered to an organization pool.
func OrgPoolSelfHostedMsg() string { return types.OrgPoolSelfHostedMsg }

// RemoteNotConfiguredMsg refuses an administrator's add of a runner id this server is not set up with
// as a remote-hosted runner, the id as they sent it.
func RemoteNotConfiguredMsg(id string) string {
	return fmt.Sprintf("%s isn't a remote-hosted runner this server is set up with.", id)
}

// RemoteNotAvailableMsg refuses a person's add of a remote-hosted runner to their own user pool when
// its people policy does not admit them; an unknown id is answered the same, as they sent it.
func RemoteNotAvailableMsg(id string) string {
	return fmt.Sprintf("%s is not available to people's pools. Ask your admin to make it available.", id)
}
