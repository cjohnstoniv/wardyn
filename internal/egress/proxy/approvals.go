// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
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

// configureHold sets the wait_for_review mode + limits from the run's config.
// timeout<=0 / maxHolds<=0 keep the defaults. Called once at NewServer before
// the proxy serves.
func (a *approvalClient) configureHold(mode types.FirstUseMode, timeout time.Duration, maxHolds int) {
	a.firstUseMode = mode
	if timeout > 0 {
		a.holdTimeout = timeout
	}
	if maxHolds > 0 {
		a.holdSem = make(chan struct{}, maxHolds)
	}
}

// ResolveWait implements wait_for_review: it HOLDS the caller until the host's
// approval reaches a terminal state (approved/denied) or the hold deadline
// passes, reusing Resolve to raise/cache. It fails CLOSED — on deadline, ctx
// cancel, a failed raise, or a saturated per-run hold cap it returns the current
// (pending) state, which the caller turns into a 403 with the approval left
// PENDING (so wait_for_review degrades to deny_with_review, never to allow).
func (a *approvalClient) ResolveWait(ctx context.Context, host string) resolveResult {
	// First resolve raises the approval (or returns a cached terminal state).
	r := a.Resolve(ctx, host)
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
			decided, newState := a.poll(ctx, r.ApprovalID)
			if !decided {
				continue
			}
			// Publish the terminal decision to the shared cache so sibling
			// connections to the same host see it without re-polling.
			a.mu.Lock()
			if st := a.hosts[host]; st != nil {
				st.state = newState
				st.lastPoll = time.Now()
			}
			a.mu.Unlock()
			return resolveResult{State: newState, ApprovalID: r.ApprovalID}
		}
	}
}

// resolveResult is what the proxy needs to act on a first-use decision.
type resolveResult struct {
	// Decision is allow (now approved), deny (denied), or pending.
	State approvalState
	// ApprovalID is set when pending/decided.
	ApprovalID uuid.UUID
}

// Resolve advances the first-use approval state machine for host and returns
// the current actionable state. It is safe for concurrent use and never
// blocks on the request path beyond a single bounded HTTP round-trip used to
// raise or poll an approval.
func (a *approvalClient) Resolve(ctx context.Context, host string) resolveResult {
	a.mu.Lock()
	st, ok := a.hosts[host]
	if !ok {
		st = &hostApproval{state: apNone}
		a.hosts[host] = st
	}
	// Snapshot under lock; perform network calls without holding the lock.
	cur := st.state
	id := st.approvalID
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
		decided, newState := a.poll(ctx, id)
		a.mu.Lock()
		defer a.mu.Unlock()
		st = a.hosts[host]
		if decided {
			st.state = newState
		}
		return resolveResult{State: st.state, ApprovalID: st.approvalID}
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
func (a *approvalClient) poll(ctx context.Context, id uuid.UUID) (decided bool, newState approvalState) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		a.base+"/api/v1/internal/approvals/"+id.String(), nil)
	if err != nil {
		return false, apPending
	}
	req.Header.Set("Authorization", "Bearer "+a.token.Get())
	resp, err := a.client.Do(req)
	if err != nil {
		return false, apPending
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return false, apPending
	}
	var ar types.ApprovalRequest
	if err := json.NewDecoder(resp.Body).Decode(&ar); err != nil {
		return false, apPending
	}
	switch ar.State {
	case types.ApprovalApproved:
		return true, apApproved
	case types.ApprovalDenied, types.ApprovalExpired:
		return true, apDenied
	default:
		return false, apPending
	}
}
