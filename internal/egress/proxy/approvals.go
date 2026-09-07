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
	"net/http"
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
	// terminal state (poll returns it; every writer of `state` must write this
	// too, or a scope decided while the host was PENDING — the normal path —
	// silently degrades to run). ALWAYS read it through Normalize(), never with a
	// raw == compare: the zero value means "decided before scopes existed, or by a
	// client that does not send one", which Normalize maps to ScopeRun (today's
	// shipped behavior), while an UNRECOGNISED value can only come from a NEWER
	// control plane and Normalize floors it to ScopeOnce rather than widening it.
	// A raw compare throws both of those away and fails open across versions.
	scope types.ApprovalScope
	// expiresAt bounds a scope=until grant; zero means no expiry — every
	// once/run/always entry, and any `until` whose expiry the control plane did
	// not send (the API rejects until-without-an-expiry at the write boundary, so
	// that shape only comes from an older peer, and behaving like run is the
	// honest read of it).
	expiresAt time.Time
}

const pollInterval = 2 * time.Second

const (
	// defaultHoldTimeout / defaultMaxHolds back wait_for_review when the config
	// leaves them unset.
	defaultHoldTimeout = 30 * time.Second
	defaultMaxHolds    = 16
)

// holdPollInterval is how often a held (wait_for_review) connection re-polls the
// control plane for its decision. A var so tests can shrink it. It intentionally
// bypasses the pollInterval fan-out throttle: a hold is a single goroutine that
// wants snappy release, not a herd re-checking the same host.
var holdPollInterval = 1 * time.Second

// concurrentRaiseRetries bounds how many times ResolveWait re-resolves a host
// stuck with State==apPending && ApprovalID==Nil before giving up (see
// ResolveWait's doc comment). Each retry costs one holdPollInterval sleep, so
// production worst case is a handful of seconds — well inside holdTimeout.
const concurrentRaiseRetries = 5

// maxApprovalHosts bounds the per-run first-use approval cache — and, with it,
// how many approvals ONE run can raise, because this client is the only thing
// that raises an egress_domain approval (the sandbox-facing route accepts
// tool_call and nothing else, deliberately).
//
// Neither side was bounded: a.hosts had no cap and entries are never removed
// (consumeIfOnce RESETS in place — delete() would nil-panic the unguarded
// re-reads after Resolve's unlock), and the control plane has no per-run cap, so
// 20,000 generated hostnames from inside the sandbox produced 20,000 cache
// entries and 20,000 PENDING rows in under four seconds. Decided rows are never
// deleted either, so the console list then has to scan them.
//
// Past the cap a NEW host resolves to pending, which evaluate turns into a
// refusal: fail CLOSED, and no eviction, so a grant a human already made can
// never be silently dropped and re-raised. The cap is far above any governed
// run's real distinct-host count — the shape it stops is a loop, not a workload.
// The sibling bound is maxLeafCerts (mitm.go).
const maxApprovalHosts = 4096

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
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
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

// configureHold sets the wait_for_review mode + hold limits. Called once at
// NewServer before the proxy serves. timeout<=0 / maxHolds<=0 keep the defaults
// — which is what production always passes; only tests set the limits, to hold
// briefly and to saturate the cap without 16 sends.
func (a *approvalClient) configureHold(mode types.FirstUseMode, timeout time.Duration, maxHolds int) {
	a.firstUseMode = mode
	if timeout > 0 {
		a.holdTimeout = timeout
	}
	if maxHolds > 0 {
		a.holdSem = make(chan struct{}, maxHolds)
	}
}

// Scope enforcement ceilings — the honest limits of what `once` and `until` can
// promise from inside the proxy. They live here rather than in a doc because
// every one of them reads as a bug in the field:
//
//   - `once` means one DECISION, and how much that covers depends on the
//     protocol. On HTTPS it is one CONNECT tunnel: Proxy.evaluate is reached
//     from exactly two callers (handlePlain and handleConnect), serveMITMRequest
//     never re-enters it, and mitmConnect serves the terminated tunnel with a
//     real http.Server carrying a 90s IdleTimeout — so MANY requests ride one
//     grant. On plain HTTP the opposite holds: handlePlain re-enters evaluate
//     PER REQUEST, so a keep-alive connection burns one `once` per request. The
//     copy says "one connection": honest for HTTPS, understated (never
//     overstated) for HTTP.
//   - A `once` grant is SPENT BEFORE SUCCESS IS GUARANTEED. evaluate falls
//     through from apApproved into the method check and VetHost IP vetting, and
//     handleConnect dials only after that — so a DNS-rebind denial or a failed
//     dial burns the grant and the operator is re-asked. Consuming after the
//     dial instead would mean holding a.mu across it; this is the cheaper end of
//     that trade, not an oversight.
//   - `until` is enforced against TWO CLOCKS. The control plane validates
//     DecisionExpiresAt (<= now+30d) against its own; this code enforces
//     time.Now().After(st.expiresAt) against the sidecar's. Negligible when they
//     share a host, real on a split topology.
//   - Resolve/ResolveWait can now return apNone, which they never could before
//     (the expiry reset inside consumeIfOnce). That is FAIL-CLOSED and already
//     handled: evaluate's approval switch routes apPending and apNone through
//     the same default arm into egress.Pending. Stated so nobody chases it as a
//     bug.
//
// consumeIfOnce expires a stale grant, then reports the entry's current terminal
// grant and — when that grant is scope=once — SPENDS it, clearing the WHOLE entry
// so no sibling can take it again. The caller must hold a.mu.
//
// INVARIANT the whole design rests on: A TERMINAL `once` ENTRY NEVER SURVIVES AN
// UNLOCK. Every write of state/scope/expiresAt — Resolve's snapshot, Resolve's
// needPoll branch, and ResolveWait's publish — is followed by a consumeIfOnce
// under the SAME lock. Nothing else in this file makes that visible, so a fourth
// writer needs a fourth call.
//
// Both resets clear the ENTIRE entry, not just `state`. A surviving approvalID is
// a fail-OPEN blocker: the raise path refuses to overwrite a non-nil id (see
// Resolve's needRaise branch), so the next request POSTs a fresh approval and
// DISCARDS its id, and the one after polls the OLD, already-APPROVED id and
// writes apApproved back — the host re-approves itself with no human in the loop
// (`once` degrades to run; `until` re-grants itself forever). Reset, do NOT
// delete: a.hosts[host] is re-read unguarded after the unlock in Resolve's
// needRaise/needPoll branches, and delete() would nil-panic there.
//
// `consumed` is returned explicitly rather than re-derived from st.state == apNone
// at the call site: after an EXPIRY reset the entry is ALSO apNone, so the derived
// form is only accidentally correct (the returned `state` is apNone there, which
// fails the terminal test) and one refactor away from conflating "spent a grant"
// with "the grant timed out".
func consumeIfOnce(st *hostApproval) (state approvalState, id uuid.UUID, consumed bool) {
	if !st.expiresAt.IsZero() && time.Now().After(st.expiresAt) {
		// `until T` elapsed -> UNKNOWN, not denied: "until T" means the operator is
		// re-asked, not that the host became forbidden. Folding this INTO the helper
		// (rather than checking once at the top of Resolve) is what makes an `until`
		// shorter than approvalTTL honored to the second EVEN ACROSS A SLOW POLL:
		// otherwise a round trip straddling T lets the post-poll write stamp
		// apApproved plus a now-past expiresAt onto an entry whose top-of-Resolve
		// check already passed, and — since a non-`once` scope is not consumed —
		// that expired grant is served, bounded only by the 10s client timeout.
		*st = hostApproval{state: apNone}
	}
	state, id = st.state, st.approvalID
	if (state == apApproved || state == apDenied) && st.scope.Normalize() == types.ScopeOnce {
		*st = hostApproval{state: apNone}
		consumed = true
	}
	// Either reset also zeroes lastPoll, so a later needPoll is computed from the
	// zero time — benign, because cur is apNone there and needRaise wins and
	// overwrites lastPoll before anything acts on it. Noted so nobody chases it.
	return state, id, consumed
}

// ResolveWait implements wait_for_review: it HOLDS the caller until the host's
// approval reaches a terminal state (approved/denied) or the hold deadline
// passes, reusing Resolve to raise/cache. It fails CLOSED — on deadline, ctx
// cancel, a failed raise, or a saturated per-run hold cap it returns the current
// (pending) state, which the caller turns into a 403 with the approval left
// PENDING (so wait_for_review degrades to deny_with_review, never to allow).
func (a *approvalClient) ResolveWait(ctx context.Context, host string) resolveResult {
	// Arm the operator's budget HERE, before the first control-plane round trip —
	// not at the timer below, which is armed only after the concurrent-raise retry
	// loop. Nothing else bounds these calls: handleConnect/handlePlain carry no
	// deadline (server.go sets ReadTimeout/WriteTimeout 0), so the only ceiling
	// used to be the shared control-plane http.Client's own Timeout, and the
	// retry loop spends up to concurrentRaiseRetries+1 of them PLUS its sleeps
	// before the hold timer exists. Against a black-holed control plane that is
	// 6x the client timeout — 785s on the shipped 130s client — for a hold
	// docs/POLICIES.md sells as first_use_hold_seconds (default 30s).
	//
	// Deriving the ctx once and passing it to every Resolve/raise/poll below puts
	// the retry loop, the raise and every poll inside that one budget; the timer
	// stays because it is what makes the WAIT itself readable, and it can only
	// fire at or before this deadline. Cancellation of the caller's ctx still
	// propagates through, and the result on expiry is unchanged: pending, i.e.
	// fail closed.
	ctx, cancel := context.WithTimeout(ctx, a.holdTimeout)
	defer cancel()

	// First resolve raises the approval (or returns a cached terminal state).
	r := a.Resolve(ctx, host)
	// A concurrent first-touch connection to the SAME new host can land here
	// while ANOTHER goroutine's raise() for that host is still in flight: the
	// raiser claims apPending under lock BEFORE its network round trip returns
	// and records the real approval id (see the needRaise branch below), so a
	// sibling that resolves in that window observes apPending with a Nil id —
	// indistinguishable, from resolveResult alone, from a raise that already
	// failed outright. Without this retry, ApprovalID==uuid.Nil below would
	// bail out immediately: wait_for_review would silently degrade to a
	// deny_with_review-style fail-fast for every connection except the one
	// that won the raise race (W20-hold-fsm-4). The raise is a single HTTP
	// round trip, so a few short re-resolves clear it in practice; a raise
	// that genuinely failed stays apPending/Nil and this exits the same as
	// before, just after a bounded extra wait.
	for i := 0; r.State == apPending && r.ApprovalID == uuid.Nil && i < concurrentRaiseRetries; i++ {
		select {
		case <-ctx.Done():
			return r
		case <-time.After(holdPollInterval):
		}
		r = a.Resolve(ctx, host)
	}
	if r.State == apApproved || r.State == apDenied || r.ApprovalID == uuid.Nil {
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
			// The hold loop used to return its OWN poll result and ignore the cache:
			// N held connections each run their own ticker, each poll the SAME
			// approval, each get apApproved, each return it — up to defaultMaxHolds
			// callers consuming one `once`. The control plane cannot help, because it
			// does not know consumption exists; the cache is the only place it does.
			// So this arm goes through the cache, under the lock, and consumes.
			decided, newState, polledScope, polledExpiry := a.poll(ctx, r.ApprovalID)
			if !decided {
				continue
			}
			// A CLOSURE, so `defer` covers every return path. Do NOT flatten this
			// into Lock() … early-return … Unlock(): an early return between the
			// Lock and the Unlock LEAKS a.mu, and then every subsequent Resolve on
			// ANY host blocks forever and all holdSem slots fill permanently — a
			// proxy-wide egress outage for the run, far worse than the leak below.
			return func() resolveResult {
				a.mu.Lock()
				defer a.mu.Unlock()
				st := a.hosts[host]
				// (1) DISCRIMINATE FIRST. Storing before discriminating re-opens the
				// fail-open blocker: a stale holder that writes first stamps
				// apApproved + its scope over a FRESH PENDING entry, then correctly
				// returns pending to itself — leaving the cache holding
				// {apApproved, <new PENDING id>}, which the next Resolve serves from
				// its fast path as a grant no human decided.
				if st == nil || st.approvalID != r.ApprovalID {
					// A sibling consumed the grant (which clears the whole entry) and a
					// fresh raise may already have taken the slot. Write NOTHING; this
					// holder did not get the grant. Return OUR OWN r.ApprovalID, not
					// st.approvalID and not Nil: this value goes straight out of
					// ResolveWait to evaluate, whose pending arm stamps it onto the
					// decision log only when it is non-Nil — Nil would leave an
					// audit line saying "blocked, awaiting approval" with no approval to
					// point at, and st.approvalID would name an approval this caller
					// never raised. (Resolve's stale path returns st.approvalID instead,
					// which CAN be Nil — correct there, because that value DOES feed the
					// concurrent-raise retry loop above and a Nil merely costs one
					// iteration before the re-raise lands. The asymmetry is deliberate;
					// don't "fix" one to match the other.)
					return resolveResult{State: apPending, ApprovalID: r.ApprovalID}
				}
				// (2) Publish the terminal decision — state, scope AND expiry — to the
				// shared cache so sibling connections see it without re-polling.
				// Publishing stays correct for run/until/always and is harmless for
				// once, because step (3) clears it before this unlock.
				st.state, st.scope, st.expiresAt = newState, polledScope, polledExpiry
				st.lastPoll = time.Now()
				// (3) Consume if this was a `once` grant — and expire it if it went
				// stale while the holder was parked. An already-past expiresAt makes
				// this return apNone, which evaluate's default arm turns into pending:
				// fail-closed, and the right answer for a grant that expired before the
				// holder was released.
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

// Resolve advances the first-use approval state machine for host and returns
// the current actionable state. It is safe for concurrent use and never
// blocks on the request path beyond a single bounded HTTP round-trip used to
// raise or poll an approval.
//
// KEYED ON THE HOST, AND ON NOTHING ELSE. evaluate calls this with req.Host and
// discards the port, so ONE approval releases EVERY port of that host for
// whatever reach its scope names, with no second approval raised — an approve
// raised by a CONNECT to :443 also allows :22 and :5432, and on scope=always it
// does so for every future run of the workspace. That is a property of the whole
// path and not of this map: the raise body a human is shown carries {host, mode}
// and no port (egressScope), and the durable always-write goes through
// hostrules.ValidApprovedHost, which refuses a port by construction. Port
// scoping would therefore be a behaviour change at all three places, not a key
// change here.
//
// It is written down in code as well as in docs/POLICIES.md's scope table
// ("Every scope in that table is HOST-wide — on every port") because the table's
// columns describe how LONG a decision lasts and never how NARROW it is, and the
// one nearby passage that does mention ports documents the DENY side as
// port-blind — which a reader can fairly take to imply that a grant is not.
func (a *approvalClient) Resolve(ctx context.Context, host string) resolveResult {
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
			return resolveResult{State: apPending}
		}
		st = &hostApproval{state: apNone}
		a.hosts[host] = st
	}
	// Snapshot under lock; perform network calls without holding the lock. The
	// snapshot is also a CLAIM-AND-CONSUME: consumeIfOnce first drops an `until`
	// grant whose T has passed, then SPENDS a `once` grant so no concurrent caller
	// can take the same one. Both steps are no-ops for run/always — their
	// expiresAt is the zero value and Normalize never yields once — which is what
	// keeps the default path byte-identical to before scopes existed.
	cur, id, consumed := consumeIfOnce(st)
	needRaise := cur == apNone
	if needRaise {
		// Claim the raise UNDER the lock: transition to pending now so a
		// concurrent first-use caller observes pending and polls instead of
		// raising a DUPLICATE approval. lastPoll=now throttles that second caller
		// to the no-op default branch until this raise records the real id below.
		// A failed raise rolls this back to apNone so a later request can retry.
		st.state = apPending
		st.lastPoll = time.Now()
	}
	// Re-poll a pending approval (throttled), OR re-validate a granted approval
	// whose cache entry is older than approvalTTL — the latter is how a later
	// EXPIRE/REVOKE is observed instead of failing open forever. The pending
	// clause requires a real id: while another goroutine's raise() is still in
	// flight the entry is apPending with a Nil id, and polling GET
	// /internal/approvals/00000000-... would be a wasted control-plane call.
	needPoll := (cur == apPending && id != uuid.Nil && time.Since(st.lastPoll) >= pollInterval) ||
		(cur == apApproved && time.Since(st.lastPoll) >= approvalTTL)
	if needPoll {
		st.lastPoll = time.Now()
	}
	a.mu.Unlock()

	if consumed {
		// We SPENT the grant; hand it to THIS caller and stop. This must return
		// BEFORE the switch: the entry is apNone again, so falling through would
		// re-raise on the very request the operator's decision released.
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
			// Fail closed: roll the claimed pending back to none so a later
			// request can retry raising. Only roll back OUR claim (still pending
			// with no id); never clobber a decision that landed meanwhile.
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
		// Either a pending approval awaiting a decision, or an approved entry past
		// its TTL being re-validated. A now-Expired/Denied result flips the entry
		// to denied (fail closed); a still-Approved result refreshes it. A
		// transient poll error (decided=false) leaves the prior state and retries
		// after the interval — staleness bounded by the TTL AS LONG AS the control
		// plane recovers; under a persistent CP outage an already-approved host stays
		// open (a deliberate availability-over-strictness tradeoff for a live run).
		decided, newState, polledScope, polledExpiry := a.poll(ctx, id)
		a.mu.Lock()
		defer a.mu.Unlock()
		st = a.hosts[host]
		if st.approvalID != id {
			// STALE POLLER. Two goroutines can both pass needPoll whenever a poll
			// round trip exceeds pollInterval (the client timeout is 10s). The fast
			// one lands, consumes, and RESETS the entry; a re-raise may already have
			// claimed the slot. Writing here would resurrect apApproved onto a fresh
			// PENDING id — the next Resolve serves it from the fast path, and
			// evaluate stamps that PENDING id onto the ALLOW log, attributing an
			// allow to an approval nobody decided (breaks the W20-hold-fsm-1 audit
			// join). It would also let `until` fail open, by re-stamping apApproved
			// plus a past expiresAt onto an entry a sibling just expired. So write
			// NOTHING and report pending. None of this was reachable before scopes:
			// state only moved pending->terminal, so the stale write was idempotent.
			//
			// st.approvalID may be Nil here (the entry was reset and not yet
			// re-raised). That costs ResolveWait's retry loop exactly one iteration
			// before the re-raise lands, which is why this side returns st.approvalID
			// while ResolveWait's stale path returns its own r.ApprovalID.
			return resolveResult{State: apPending, ApprovalID: st.approvalID}
		}
		if decided {
			// SPLIT from the id check ABOVE ON PURPOSE — do not fold them into one
			// `if decided && st.approvalID == id`. !decided is a TRANSIENT
			// control-plane error and must stay on today's path (fall through and
			// return st.state unchanged, which on the TTL-revalidation path is still
			// apApproved). That is the deliberate availability-over-strictness
			// tradeoff documented just above; collapsing the two conditions turns it
			// into a 403 and changes behavior for scope=run — the DEFAULT scope, and
			// the one this change must leave byte-identical.
			st.state, st.scope, st.expiresAt = newState, polledScope, polledExpiry
		}
		// This branch is ITSELF a grant-serving site. On the FIRST observation of a
		// decision the entry was still apPending at the snapshot, so the snapshot's
		// consume did nothing; without this call the branch returns the grant AND
		// leaves the entry APPROVED for the next caller to take again. Under
		// deny_with_review that is the NORMAL path, not a race, and `once` would be
		// served exactly twice. (Consuming ONLY here is the mirror bug: the fast
		// path above would hand the cached grant to every concurrent caller.)
		s, sid, _ := consumeIfOnce(st)
		return resolveResult{State: s, ApprovalID: sid}
	default:
		// Pending but throttled: report pending without a network call.
		return resolveResult{State: apPending, ApprovalID: id}
	}
}

// egressScope is the requested_scope body for an egress_domain approval. Mode
// carries the run's first-use mode so the UI can tell a live-HELD
// (wait_for_review) request apart from a passive deny_with_review pending.
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
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
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

// poll fetches an approval's current state. It returns decided=true only for
// terminal states (APPROVED/DENIED/EXPIRED). EXPIRED is treated as denied
// (fail closed).
//
// It also returns the DECISION'S SCOPE and expiry, which the caller must store
// onto the cache entry: this decode already read the whole ApprovalRequest and
// used to throw both away, and handleInternalGetApproval already writes the full
// struct — so only the caller-side store is new. Without it every scope decided
// while the host was PENDING (i.e. the normal path) silently degrades to run.
//
// scope/expiresAt are meaningful only when decided; they are zero otherwise, so a
// transient error can never overwrite a good scope with a blank one — and callers
// gate the store on `decided` anyway.
func (a *approvalClient) poll(ctx context.Context, id uuid.UUID) (decided bool, newState approvalState, scope types.ApprovalScope, expiresAt time.Time) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		a.base+"/api/v1/internal/approvals/"+id.String(), nil)
	if err != nil {
		return false, apPending, "", time.Time{}
	}
	req.Header.Set("Authorization", "Bearer "+a.token.Get())
	resp, err := a.client.Do(req)
	if err != nil {
		return false, apPending, "", time.Time{}
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return false, apPending, "", time.Time{}
	}
	var ar types.ApprovalRequest
	if err := json.NewDecoder(resp.Body).Decode(&ar); err != nil {
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
	case types.ApprovalDenied, types.ApprovalExpired:
		// EXPIRED rows carry no scope — the stale-PENDING sweeper is not a human
		// decision and writes the zero value, which normalizes to run: a cached
		// deny for the rest of the run, exactly today's behavior.
		return true, apDenied, ar.DecisionScope, exp
	default:
		return false, apPending, "", time.Time{}
	}
}
