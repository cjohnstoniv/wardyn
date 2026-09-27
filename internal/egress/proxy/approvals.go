// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// approvalState is the per-host first-use approval cache entry.
type approvalState int

const (
	apNone     approvalState = iota // never seen
	apPending                       // approval raised, awaiting decision
	apApproved                      // human approved -> runtime allowlist
	apDenied                        // human denied  -> cached deny
	// apCapped: the per-run host table is full (maxApprovalHosts) — no entry
	// made, no approval raised, no row. FAIL CLOSED like apPending (evaluate's
	// default arm maps both to egress.Pending), but a DIFFERENT FACT:
	// ResolveWait must not retry a cap the way it retries a raise-in-flight
	// (apPending with a Nil id), or it parks a goroutine/socket for the whole hold budget.
	apCapped
)

// approvalClient implements the first-use approval flow against the control
// plane's internal approval endpoints. It NEVER blocks a request: on an
// unknown host it raises an egress_domain ApprovalRequest, caches the pending
// id, and returns Pending immediately. Subsequent requests to that host
// lazily poll the approval and transition the cached state.
type approvalClient struct {
	base   string // control plane base URL
	token  *tokenSource
	runID  uuid.UUID
	client *http.Client

	// firstUseMode is the run's uniform first-use mode (the policy carries one
	// value for the whole run). It is stamped onto the raised approval's scope so
	// the UI can distinguish a live-HELD wait_for_review request from a passive
	// deny_with_review pending. Empty/other => no hold hint.
	firstUseMode types.FirstUseMode
	// holdTimeout / holdSem bound wait_for_review holds (see ResolveWait).
	holdTimeout time.Duration
	holdSem     chan struct{}

	mu    sync.Mutex
	hosts map[string]*hostApproval
}

type hostApproval struct {
	state      approvalState
	approvalID uuid.UUID
	// lastPoll throttles polling so we hit the control plane at most once per
	// pollInterval per host while pending.
	lastPoll time.Time
	// scope is the DECIDED blast radius, learned from the poll that observed the
	// terminal state; every writer of `state` must write this too. ALWAYS read
	// it through Normalize(), never a raw == compare: the zero value maps to
	// ScopeRun, an unrecognised (newer-peer) value floors to ScopeOnce — a raw
	// compare fails open across versions.
	scope types.ApprovalScope
	// expiresAt bounds a scope=until grant; zero means no expiry (every
	// once/run/always entry, or an `until` an older peer sent with no expiry —
	// behaving like run is the honest read of that shape).
	expiresAt time.Time
}

const pollInterval = 2 * time.Second

const (
	// defaultHoldTimeout / defaultMaxHolds back wait_for_review when the config
	// leaves them unset.
	defaultHoldTimeout = 30 * time.Second
	defaultMaxHolds    = 16
	// maxHoldTimeout / maxHoldsCeiling are the absolute ceilings configureHold
	// clamps a policy's values to, mirroring the control plane's own
	// maxFirstUseHoldSeconds/maxHoldsPerSpec — the control plane bounds these
	// only at AUTHORING time, so a policy stored before that bound existed
	// reaches this sidecar unvalidated; this is the last door before it becomes
	// a channel capacity / poll duration.
	maxHoldTimeout  = 600 * time.Second
	maxHoldsCeiling = 256
)

// holdPollInterval is how often a held (wait_for_review) connection re-polls
// the control plane; intentionally bypasses the pollInterval fan-out throttle
// (a hold is one goroutine, not a herd re-checking the same host).
var holdPollInterval = 1 * time.Second

// concurrentRaiseRetries bounds how many times ResolveWait re-resolves a host
// stuck with State==apPending && ApprovalID==Nil before giving up (see
// ResolveWait's doc comment). Each retry costs one holdPollInterval sleep, so
// production worst case is a handful of seconds — well inside holdTimeout.
const concurrentRaiseRetries = 5

// maxApprovalHosts bounds the per-run first-use approval cache and, with it,
// how many approvals ONE run can raise (this client is the only thing that
// raises an egress_domain approval).
//
// Unbounded, a run generating thousands of hostnames would produce as many
// cache entries and PENDING rows, none ever evicted. Past the cap a NEW host
// resolves to pending -> refusal: fail CLOSED, no eviction, so an existing
// human grant is never dropped and re-raised. Sibling bound: maxLeafCerts (mitm.go).
const maxApprovalHosts = 4096

// approvalPendingReason names WHY a non-terminal approval verdict refused, for the
// decision row evaluate writes. Here rather than at the call site so the state
// and its audit label stay in one file.
func approvalPendingReason(st approvalState) string {
	if st == apCapped {
		return ruleSourceApprovalHostCap
	}
	return "approval:pending"
}

// ruleSourceApprovalHostCap is the decision-log reason for a refusal caused by
// maxApprovalHosts rather than by a human: "an approval is waiting on you" vs
// "this run's host table is full, nothing was raised and nothing ever will be".
const ruleSourceApprovalHostCap = "approval:host-cap"

// approvalCapWarnOnce keeps the cap's log line to once per process: the
// condition is a run-long state, and a per-request ERROR would bury it.
var approvalCapWarnOnce sync.Once

// approvalTTL bounds how long a granted (apApproved) host is trusted from cache
// before Resolve re-validates it against the control plane. Without it an
// approval that later EXPIRES or is REVOKED is never observed and egress keeps
// flowing until proxy/run restart (fail-open staleness). Fresh approvals inside
// the window are still served from cache with no per-request network call.
const approvalTTL = 60 * time.Second

func newApprovalClient(base string, token *tokenSource, runID uuid.UUID, client *http.Client) *approvalClient {
	return &approvalClient{
		base:        base,
		token:       token,
		runID:       runID,
		client:      client,
		holdTimeout: defaultHoldTimeout,
		holdSem:     make(chan struct{}, defaultMaxHolds),
		hosts:       make(map[string]*hostApproval),
	}
}

// configureHold sets the wait_for_review mode + hold limits, called once at
// NewServer. timeout<=0 / maxHolds<=0 keep the defaults (what production
// always passes; only tests set limits).
//
// Both are CLAMPED to maxHoldTimeout/maxHoldsCeiling: a policy stored before
// the control plane's own authoring-time bound existed reaches this sidecar
// unvalidated, and clamping (not refusing) is deliberate — boot runs the
// policy it was handed under a bound it can honour.
func (a *approvalClient) configureHold(mode types.FirstUseMode, timeout time.Duration, maxHolds int) {
	a.firstUseMode = mode
	if timeout > 0 {
		a.holdTimeout = min(timeout, maxHoldTimeout)
	}
	if maxHolds > 0 {
		a.holdSem = make(chan struct{}, min(maxHolds, maxHoldsCeiling))
	}
}

// Scope enforcement ceilings — the honest limits of what `once` and `until`
// can promise from inside the proxy:
//
//   - `once` means one CONNECT tunnel on HTTPS (many requests ride one grant,
//     since the terminated tunnel serves many), but one PER REQUEST on plain
//     HTTP (a keep-alive connection burns one `once` each) — honest for HTTPS,
//     understated for HTTP.
//   - A `once` grant is SPENT BEFORE SUCCESS IS GUARANTEED: evaluate consumes
//     it before VetHost/dial, so a DNS-rebind denial or failed dial burns the
//     grant and re-asks the operator. The METHOD check runs before the
//     approval flow, so a method-denied request never spends one.
//   - `until` is enforced against TWO CLOCKS (control plane vs. this sidecar's
//     clock) — negligible when they share a host, real on a split topology.
//   - Resolve/ResolveWait can now return apNone (an expiry reset inside
//     consumeIfOnce); that is FAIL-CLOSED, since evaluate's default arm routes
//     apPending and apNone identically into egress.Pending.
//
// consumeIfOnce expires a stale grant, then reports the entry's current
// terminal grant and — when that grant is scope=once — SPENDS it, clearing
// the WHOLE entry so no sibling can take it again. The caller must hold a.mu.
//
// Invariant: a terminal `once` entry never survives an unlock — every write
// of state/scope/expiresAt is followed by a consumeIfOnce under the SAME lock.
//
// Both resets clear the ENTIRE entry, not just `state`: a surviving approvalID
// would let a stale poll re-stamp apApproved onto it with no human in the
// loop. Reset, do NOT delete — a.hosts[host] is re-read unguarded after unlock elsewhere.
//
// `consumed` is returned explicitly rather than re-derived from state==apNone:
// an EXPIRY reset is also apNone, so the derived form would conflate "spent a
// grant" with "the grant timed out".
func consumeIfOnce(st *hostApproval) (state approvalState, id uuid.UUID, consumed bool) {
	if !st.expiresAt.IsZero() && time.Now().After(st.expiresAt) {
		// `until T` elapsed -> UNKNOWN, not denied: the operator is re-asked, not
		// forbidden. Folded into the helper (not checked once at Resolve's top) so
		// an `until` is honored to the second even across a slow poll straddling T.
		*st = hostApproval{state: apNone}
	}
	state, id = st.state, st.approvalID
	if (state == apApproved || state == apDenied) && st.scope.Normalize() == types.ScopeOnce {
		*st = hostApproval{state: apNone}
		consumed = true
	}
	// Either reset also zeroes lastPoll — benign, since cur is apNone there and
	// needRaise overwrites lastPoll before anything acts on it.
	return state, id, consumed
}

// ResolveWait implements wait_for_review: it HOLDS the caller until the host's
// approval reaches a terminal state (approved/denied) or the hold deadline
// passes, reusing Resolve to raise/cache. It fails CLOSED — on deadline, ctx
// cancel, a failed raise, or a saturated per-run hold cap it returns the current
// (pending) state, which the caller turns into a 403 with the approval left
// PENDING (so wait_for_review degrades to deny_with_review, never to allow).
func (a *approvalClient) ResolveWait(ctx context.Context, host string) resolveResult {
	host = approvalHostKey(host)
	// Arm the operator's budget HERE, before the first control-plane round trip,
	// not at the timer below (armed only after the concurrent-raise retry loop):
	// nothing else bounds these calls, so without this a black-holed control
	// plane could stretch a 30s hold to several minutes via the retry loop alone.
	//
	// Deriving ctx once and passing it to every Resolve/raise/poll below puts
	// everything inside that one budget; the result on expiry is unchanged:
	// pending, fail closed.
	ctx, cancel := context.WithTimeout(ctx, a.holdTimeout)
	defer cancel()

	// First resolve raises the approval (or returns a cached terminal state).
	r := a.Resolve(ctx, host)
	// A sibling first-touch connection can observe apPending with a Nil id
	// while ANOTHER goroutine's raise() for the same new host is still in
	// flight (the raiser claims apPending under lock before its round trip
	// returns). Without this retry, wait_for_review would fail-fast for every
	// connection except the one that won the raise race. apPending ONLY:
	// apCapped is the same fail-closed answer but settled, so it exits immediately instead.
	for i := 0; r.State == apPending && r.ApprovalID == uuid.Nil && i < concurrentRaiseRetries; i++ {
		select {
		case <-ctx.Done():
			return r
		case <-time.After(holdPollInterval):
		}
		r = a.Resolve(ctx, host)
	}
	if r.State == apApproved || r.State == apDenied || r.State == apCapped || r.ApprovalID == uuid.Nil {
		return r
	}
	// Bound concurrent holds: over the cap, don't park another goroutine — fall
	// back to fail-fast pending (the approval is already raised, so a retry works).
	select {
	case a.holdSem <- struct{}{}:
		defer func() { <-a.holdSem }()
	default:
		return r
	}

	timeout := time.NewTimer(a.holdTimeout)
	defer timeout.Stop()
	ticker := time.NewTicker(holdPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return resolveResult{State: apPending, ApprovalID: r.ApprovalID}
		case <-timeout.C:
			return resolveResult{State: apPending, ApprovalID: r.ApprovalID}
		case <-ticker.C:
			// This arm goes through the cache, under the lock, and consumes — never
			// the raw poll result — else N held connections would each poll the
			// SAME approval and each consume one `once` grant.
			decided, newState, polledScope, polledExpiry := a.poll(ctx, r.ApprovalID)
			if !decided {
				continue
			}
			// A closure, so `defer` covers every return path — flattening this into
			// Lock()...early-return...Unlock() would leak a.mu on early return,
			// blocking every subsequent Resolve for the run.
			return func() resolveResult {
				a.mu.Lock()
				defer a.mu.Unlock()
				st := a.hosts[host]
				// (1) Discriminate first: storing before discriminating would let a
				// stale holder stamp apApproved over a FRESH PENDING entry — a grant
				// no human decided, served by the next Resolve's fast path.
				if st == nil || st.approvalID != r.ApprovalID {
					// A sibling consumed the grant (clearing the whole entry) and a
					// fresh raise may have taken the slot; write NOTHING. Return OUR OWN
					// r.ApprovalID (never Nil, never st.approvalID) so evaluate's audit
					// log names the approval this caller actually raised.
					return resolveResult{State: apPending, ApprovalID: r.ApprovalID}
				}
				// (2) Publish the terminal decision to the shared cache so siblings
				// see it without re-polling; harmless for once since step (3) clears
				// it before this unlock.
				st.state, st.scope, st.expiresAt = newState, polledScope, polledExpiry
				st.lastPoll = time.Now()
				// (3) Consume if this was a `once` grant, expiring it if stale — an
				// already-past expiresAt returns apNone, fail-closed via evaluate's default arm.
				s, id, _ := consumeIfOnce(st)
				return resolveResult{State: s, ApprovalID: id}
			}()
		}
	}
}

// resolveResult is what the proxy needs to act on a first-use decision.
type resolveResult struct {
	// Decision is allow (now approved), deny (denied), pending, or — since
	// scope=until — apNone, when consumeIfOnce found the grant already expired.
	// apNone is fail-closed at the caller (see the ceilings note on consumeIfOnce).
	State approvalState
	// ApprovalID is set when pending/decided.
	ApprovalID uuid.UUID
}

// approvalHostKey is the ONE spelling of "which host this approval is about",
// used for the cache key AND the host in the raised requested_scope, so the
// two surfaces can never diverge.
//
// Egress_domain approvals are HOST-WIDE, not host:port-scoped: this strips a
// port if present, so "example.com:8443" gets the same grant as "example.com"
// rather than opening a second approval row for the same host. A port-scoped
// approval would need this key, requested_scope, and
// hostrules.ValidApprovedHost changed together.
func approvalHostKey(host string) string {
	h, _, err := net.SplitHostPort(host)
	if err != nil {
		h = host
	}
	return strings.ToLower(strings.TrimRight(h, "."))
}

// Resolve advances the first-use approval state machine for host and returns
// the current actionable state. Safe for concurrent use; never blocks beyond
// a single bounded HTTP round-trip to raise or poll an approval.
//
// KEYED ON THE HOST, AND ON NOTHING ELSE: evaluate discards the port, so ONE
// approval releases EVERY port of that host — a CONNECT to :443 also allows
// :22 and :5432, and on scope=always for every future run. Port scoping would
// therefore be a behaviour change at all three places, not a key change here:
// this key, egressScope, and hostrules.ValidApprovedHost — see
// docs/POLICIES.md's scope table.
func (a *approvalClient) Resolve(ctx context.Context, host string) resolveResult {
	host = approvalHostKey(host)
	a.mu.Lock()
	st, ok := a.hosts[host]
	if !ok {
		if len(a.hosts) >= maxApprovalHosts {
			// Fail closed: no entry, no raise, no row. Already-decided hosts keep
			// working — they have their entries — so this bites only new names.
			a.mu.Unlock()
			approvalCapWarnOnce.Do(func() {
				slog.Error("wardyn-proxy: first-use approval host cap reached; further NEW hosts are refused without raising an approval",
					slog.Int("cap", maxApprovalHosts),
					slog.String("run_id", a.runID.String()))
			})
			return resolveResult{State: apCapped}
		}
		st = &hostApproval{state: apNone}
		a.hosts[host] = st
	}
	// Snapshot under lock; perform network calls without holding the lock. The
	// snapshot is also a CLAIM-AND-CONSUME: consumeIfOnce drops an expired
	// `until` grant then SPENDS a `once` grant so no concurrent caller can take it.
	cur, id, consumed := consumeIfOnce(st)
	needRaise := cur == apNone
	if needRaise {
		// Claim the raise UNDER the lock so a concurrent caller polls instead of
		// raising a duplicate; a failed raise rolls this back to apNone.
		st.state = apPending
		st.lastPoll = time.Now()
	}
	// Re-poll a pending approval (throttled), OR re-validate an approved entry
	// older than approvalTTL — the latter is how a later EXPIRE/REVOKE is
	// observed instead of failing open forever. The pending clause requires a
	// real id (else raise() may still be in flight).
	needPoll := (cur == apPending && id != uuid.Nil && time.Since(st.lastPoll) >= pollInterval) ||
		(cur == apApproved && time.Since(st.lastPoll) >= approvalTTL)
	if needPoll {
		st.lastPoll = time.Now()
	}
	a.mu.Unlock()

	if consumed {
		// We SPENT the grant; must return BEFORE the switch, or falling through
		// would re-raise on the very request the operator's decision released.
		return resolveResult{State: cur, ApprovalID: id}
	}

	switch {
	case cur == apApproved && !needPoll:
		// Fresh approval: fast path, no per-request network call.
		return resolveResult{State: apApproved, ApprovalID: id}
	case cur == apDenied:
		return resolveResult{State: apDenied, ApprovalID: id}
	case needRaise:
		// We already claimed apPending under the lock above, so exactly one
		// goroutine reaches here for the first use of host — no duplicate raise.
		newID, err := a.raise(ctx, host)
		a.mu.Lock()
		defer a.mu.Unlock()
		st = a.hosts[host]
		if err != nil {
			// Fail closed: roll our claimed pending back to none; never clobber a
			// decision that landed meanwhile.
			if st.state == apPending && st.approvalID == uuid.Nil {
				st.state = apNone
			}
			return resolveResult{State: apPending}
		}
		// Record the real approval id against the pending we claimed.
		if st.approvalID == uuid.Nil {
			st.state = apPending
			st.approvalID = newID
			st.lastPoll = time.Now()
		}
		return resolveResult{State: apPending, ApprovalID: st.approvalID}
	case needPoll:
		// A pending approval, or an approved entry past its TTL being
		// re-validated; a transient poll error leaves the prior state (a
		// deliberate availability-over-strictness tradeoff under a persistent CP outage).
		decided, newState, polledScope, polledExpiry := a.poll(ctx, id)
		a.mu.Lock()
		defer a.mu.Unlock()
		st = a.hosts[host]
		if st.approvalID != id {
			// STALE POLLER: two goroutines can both pass needPoll when a poll
			// round trip exceeds pollInterval. Writing here would resurrect
			// apApproved onto a fresh PENDING id — breaking the audit join and
			// letting `until` fail open. Write NOTHING and report pending.
			// st.approvalID may be Nil (entry reset, not yet re-raised); that
			// costs ResolveWait's retry loop one extra iteration.
			return resolveResult{State: apPending, ApprovalID: st.approvalID}
		}
		if decided {
			// SPLIT from the id check ABOVE ON PURPOSE: !decided is a TRANSIENT
			// control-plane error that must fall through unchanged (the
			// availability-over-strictness tradeoff above) — folding the two
			// conditions would turn a transient error into a 403 for scope=run.
			st.state, st.scope, st.expiresAt = newState, polledScope, polledExpiry
		}
		// This branch is ITSELF a grant-serving site: without this call, a `once`
		// grant observed here for the first time would be served AND left
		// APPROVED for the next caller — served twice.
		s, sid, _ := consumeIfOnce(st)
		return resolveResult{State: s, ApprovalID: sid}
	default:
		// Pending but throttled: report pending without a network call.
		return resolveResult{State: apPending, ApprovalID: id}
	}
}

// egressScope is the requested_scope body for an egress_domain approval — the
// bytes a human is shown, stored verbatim by the control plane. Mode carries
// the run's first-use mode so the UI can tell a live-HELD request apart from
// a passive pending one.
//
// Host has no port, by design: a decision reaches EVERY port of that host.
// Host is always the bare host (approvalHostKey guarantees it), so the scope
// can never imply a narrowness the grant does not have.
type egressScope struct {
	Host string `json:"host"`
	Mode string `json:"mode,omitempty"`
}

type raiseBody struct {
	Kind           string      `json:"kind"`
	RequestedScope egressScope `json:"requested_scope"`
}

func (a *approvalClient) raise(ctx context.Context, host string) (uuid.UUID, error) {
	body, err := json.Marshal(raiseBody{
		Kind:           string(types.ApprovalEgressDomain),
		RequestedScope: egressScope{Host: host, Mode: string(a.firstUseMode)},
	})
	if err != nil {
		return uuid.Nil, err
	}
	return a.raiseBytes(ctx, body)
}

// raiseRefusedError is a raise the control plane answered with a refusal that
// the same raise will meet again — a request it will not accept (400), a run it
// will not ask for (403), a per-run cap (429) — as opposed to a failure a retry
// may clear.
type raiseRefusedError struct{ status int }

func (e *raiseRefusedError) Error() string {
	return fmt.Sprintf("raise approval: refused, status %d", e.status)
}

// raiseBytes POSTs one marshalled raise body and returns the approval's id —
// a new row, or the PENDING one the control plane deduplicated it to.
func (a *approvalClient) raiseBytes(ctx context.Context, body []byte) (uuid.UUID, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		a.base+"/api/v1/internal/approvals", bytes.NewReader(body))
	if err != nil {
		return uuid.Nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+a.token.Get())
	resp, err := a.client.Do(req)
	if err != nil {
		return uuid.Nil, fmt.Errorf("raise approval: %w", err)
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusCreated, http.StatusOK:
	case http.StatusBadRequest, http.StatusForbidden, http.StatusTooManyRequests:
		return uuid.Nil, &raiseRefusedError{status: resp.StatusCode}
	default:
		return uuid.Nil, fmt.Errorf("raise approval: status %d", resp.StatusCode)
	}
	var ar types.ApprovalRequest
	if err := json.NewDecoder(resp.Body).Decode(&ar); err != nil {
		return uuid.Nil, fmt.Errorf("decode approval: %w", err)
	}
	if ar.ID == uuid.Nil {
		return uuid.Nil, fmt.Errorf("raise approval: empty id")
	}
	return ar.ID, nil
}

// poll fetches an approval's current state. Returns decided=true only for
// terminal states; EXPIRED is treated as denied (fail closed).
//
// Also returns the DECISION'S SCOPE and expiry for the caller to store onto
// the cache entry — without it, every scope decided while PENDING (the normal
// path) silently degrades to run. Both are zero unless decided, so a
// transient error can never overwrite a good scope with a blank one.
func (a *approvalClient) poll(ctx context.Context, id uuid.UUID) (decided bool, newState approvalState, scope types.ApprovalScope, expiresAt time.Time) {
	ar, ok := a.fetch(ctx, id)
	if !ok {
		return false, apPending, "", time.Time{}
	}
	// Nullable on the wire (it is set only for scope=until), so a nil pointer
	// becomes the zero time == no expiry.
	var exp time.Time
	if ar.DecisionExpiresAt != nil {
		exp = *ar.DecisionExpiresAt
	}
	switch ar.State {
	case types.ApprovalApproved:
		return true, apApproved, ar.DecisionScope, exp
	case types.ApprovalDenied, types.ApprovalExpired, types.ApprovalCancelled:
		// EXPIRED/CANCELLED carry no scope (not a human decision); both MUST be
		// terminal here, else a sidecar whose run ended would hold its window
		// open against an approval nobody can ever decide.
		return true, apDenied, ar.DecisionScope, exp
	default:
		return false, apPending, "", time.Time{}
	}
}

// fetch reads one approval this run raised; ok=false on any failure, which
// every caller reads as "still pending".
func (a *approvalClient) fetch(ctx context.Context, id uuid.UUID) (types.ApprovalRequest, bool) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		a.base+"/api/v1/internal/approvals/"+id.String(), nil)
	if err != nil {
		return types.ApprovalRequest{}, false
	}
	req.Header.Set("Authorization", "Bearer "+a.token.Get())
	resp, err := a.client.Do(req)
	if err != nil {
		return types.ApprovalRequest{}, false
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return types.ApprovalRequest{}, false
	}
	var ar types.ApprovalRequest
	if err := json.NewDecoder(resp.Body).Decode(&ar); err != nil {
		return types.ApprovalRequest{}, false
	}
	return ar, true
}
