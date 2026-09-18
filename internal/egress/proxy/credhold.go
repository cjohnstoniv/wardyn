// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// THE HOLD — the second caller of the ResolveWait shape.
//
// A captured AWS SSO session can lapse WHILE a run is working. Before 0.7.6 that
// was terminal: the sandbox's next GetRoleCredentials failed, and an agent
// mid-task lost its context. Now the control plane answers 423 with the id of a
// visible sign-in request, and this file PARKS that one request while its owner
// signs in, bounded by one knob.
//
// WHAT IT DOES NOT CLAIM. A parked request is not a paused agent: the agent's
// tool call is simply slow, and a client that gives up first loses the turn
// exactly as it does today. The knob exists to be lowered below an SDK that is
// less patient than the hold.
//
// WHAT IT NEVER DOES. It never re-polls the INJECTION url: every resolve there
// re-MINTS through the broker and commits a credential.mint audit row in the
// same transaction, so a one-second poll would write three hundred rows per
// ten-minute hold onto a hash-chained log. It polls the APPROVAL instead, by id,
// and re-resolves the injection exactly ONCE when the answer arrives.

const (
	// envCredentialReauthTimeout bounds ONE re-auth workflow: the approval wait
	// and the single re-resolve that ends it.
	envCredentialReauthTimeout = "WARDYN_CREDENTIAL_REAUTH_TIMEOUT"
	// defaultCredentialReauthTimeout is maxHoldTimeout's own ceiling and the
	// order of a device-code window. A hold that outlives the sign-in it waits
	// for waits for nothing.
	defaultCredentialReauthTimeout = 600 * time.Second
	// The clamp. A 1-second budget would make the feature a lie (one poll, then
	// the same failure as before); an hour-long one would park a sandbox's
	// connection long past any SDK's patience and any person's attention.
	minCredentialReauthTimeout = 10 * time.Second
	maxCredentialReauthTimeout = 1800 * time.Second
	// maxReauthHolds bounds re-auth WORKFLOWS per run — the sidecar half of the
	// control plane's own cap of the same name. A run that has asked its owner
	// to sign in eight times is not going to be fixed by a ninth wait.
	maxReauthHolds = 8
	// reauthFinalResolveTimeout bounds the ONE re-resolve that ends a successful
	// hold. Deliberately its own bound rather than the budget's remainder — see
	// the call site.
	reauthFinalResolveTimeout = 30 * time.Second
	// reauth404Reads is how many consecutive 404s end a hold. ONE is too eager
	// (a read racing the row's own insert), and forever is the bug: a row that
	// has really gone is a hold waiting on nothing.
	reauth404Reads = 3
)

// credentialReauthBudget is the operator's ceiling for ONE hold, clamped.
// Unparseable or non-positive keeps the default — this knob is reached in a
// hurry, and a typo must not turn the hold off silently or wedge it open.
func credentialReauthBudget() time.Duration {
	d, err := time.ParseDuration(os.Getenv(envCredentialReauthTimeout))
	if err != nil || d <= 0 {
		return defaultCredentialReauthTimeout
	}
	return min(max(d, minCredentialReauthTimeout), maxCredentialReauthTimeout)
}

// errReauthPending is what a 423 from the injection resolve becomes: the
// control plane is not refusing, it is asking for a human. It carries the id of
// the request to poll.
type errReauthPending struct{ approvalID uuid.UUID }

func (e errReauthPending) Error() string {
	return "credential re-auth pending: approval " + e.approvalID.String()
}

// errReauthTimedOut ends a hold without a credential. It is DISTINCT from every
// other resolve error because it alone earns the 401 UnauthorizedException body
// (a modelled, non-retryable GetRoleCredentials error both SDKs map to a
// credential failure) rather than the generic 502 — see writeSSOUnauthorized.
//
// It is handed to the FIRST live caller that observes the workflow's terminal
// result. That caller writes the ONE credential:reauth-timeout decision row.
var errReauthTimedOut = errors.New("credential re-auth hold expired")

// errReauthTimedOutAgain is the SAME expiry, already recorded: every later
// caller of the same workflow gets it. errors.Is(errReauthTimedOutAgain,
// errReauthTimedOut) is true, so the 401 body is unchanged for all of them —
// each parked request is its own HTTP request and each needs an answer — while
// the decision row is written once per hold rather than once per retry. With
// the measured ~30 s SDK cadence the difference is one row versus twenty.
var errReauthTimedOutAgain = fmt.Errorf("%w (already recorded)", errReauthTimedOut)

// errReauthCapped is the per-run workflow cap refusing a NEW lifecycle. It is
// its own sentinel so the cap reads as a cap in the decision trail rather than
// as an expiry that never happened.
var errReauthCapped = fmt.Errorf("%w (no further sign-in will be requested for this run)", errReauthTimedOut)

// reauthTimedOutSentence is what the sandbox's SDK is told. It says what was
// done, what was not, and — because this is the one place a person could
// otherwise conclude a credential was silently swapped — that nothing was.
//
// DRAFT (M2 canon pending)
const reauthTimedOutSentence = "wardyn held this AWS SSO credential request while its owner was " +
	"asked to sign in again, and nobody signed in before the hold expired; nothing was substituted"

// reauthWorkflow is ONE bounded re-auth lifecycle, shared by every caller that
// arrives for the same approval id.
//
// IT OWNS ITS OWN GOROUTINE, and that is the whole correction (security
// BLOCKER-1 / general B1). The first shape ran the poll loop on the LEADER's
// goroutine under the entry's reMu, which made every clause of Codex #2's
// contract unreachable in production: followers queued on an uncancellable
// mutex instead of a ctx-cancellable wait, a disconnected SDK was not released,
// and when the leader's budget ended the workflow was deleted so the next mutex
// holder re-resolved, got the same 423 for the same PENDING row, and opened a
// NEW counted workflow with a FULL fresh budget — up to maxReauthHolds serial
// budgets for ONE lapse, with the measured ~30 s retry cadence feeding it.
//
// Detaching the loop makes every clause true by construction:
//   - the DEADLINE is fixed at creation and belongs to the workflow, so it
//     cannot be renewed by who happens to be waiting, and it cannot be ended
//     early by whoever happened to arrive first;
//   - every caller (first or not) merely WAITS, on wf.done or its own ctx, so a
//     caller that hangs up is released at once and writes nothing;
//   - a caller leaving does not end the hold: the workflow persists until an
//     arrival takes its result or its deadline passes, so the next retry joins
//     the SAME wait instead of starting a second one.
type reauthWorkflow struct {
	approvalID uuid.UUID
	deadline   time.Time
	done       chan struct{}
	// resolved/err are written ONCE by run(), before done is closed; every
	// reader reads them after done is closed. No lock needed and none wanted.
	resolved types.ResolvedInjection
	err      error
	// poll is the tick this workflow uses, SNAPSHOT at creation. The package var
	// it comes from is a test seam, and this goroutine outlives its first
	// caller by design — reading the var from here would race a test's own
	// restore (it did, under -race).
	poll time.Duration
	// stop ends the workflow early. The sidecar is per-run and normally dies
	// with its sandbox, but a proxy that shuts down cleanly should not leave
	// poll loops talking to a control plane about a run that has ended.
	stop chan struct{}
	// reported claims the ONE decision row for this workflow's expiry.
	reported atomic.Bool
}

// finished reports whether the workflow already has its terminal result.
func (w *reauthWorkflow) finished() bool {
	select {
	case <-w.done:
		return true
	default:
		return false
	}
}

// await blocks until the workflow finishes or the CALLER's ctx ends.
//
// A ctx that ends first returns ctx.Err() and nothing else: no decision row, no
// 401 body, no effect on the workflow. That is the hung-up SDK, and the MITM
// lane writes nothing for a client that is gone.
func (w *reauthWorkflow) await(ctx context.Context) (types.ResolvedInjection, error) {
	select {
	case <-w.done:
	case <-ctx.Done():
		return types.ResolvedInjection{}, ctx.Err()
	}
	if w.err == nil {
		return w.resolved, nil
	}
	// The first live observer reports the expiry; everyone after it gets the
	// same answer without a second decision row.
	if errors.Is(w.err, errReauthTimedOut) && !w.reported.CompareAndSwap(false, true) {
		return types.ResolvedInjection{}, errReauthTimedOutAgain
	}
	return types.ResolvedInjection{}, w.err
}

// run is the workflow's own goroutine: it polls the approval under a deadline
// that is nobody's request ctx, publishes the terminal result and wakes every
// waiter. Started exactly once, by the caller that created the workflow.
func (w *reauthWorkflow) run(base string, token *tokenSource, grantID uuid.UUID,
	client *http.Client, approvals approvalReader,
) {
	// context.Background, deliberately: the budget is the WORKFLOW's, not the
	// first caller's. A caller that disconnects must not end a hold its owner is
	// still signing in for.
	ctx, cancel := context.WithDeadline(context.Background(), w.deadline)
	defer cancel()
	w.resolved, w.err = holdForReauth(ctx, w.poll, w.stop, base, token, grantID, client, approvals, w.approvalID)
	close(w.done)
}

// reauthCoordinator owns the workflows, keyed by APPROVAL ID. One per injector.
type reauthCoordinator struct {
	mu sync.Mutex
	// quit ends every workflow this coordinator owns (proxy shutdown).
	quit chan struct{}
	once sync.Once
	// workflows keeps TERMINAL workflows too, deliberately: a later 423 naming
	// the same approval id must get that terminal result at once — no second
	// hold, no second count, no second decision row — because it is the same
	// lapse and the same PENDING row. A 423 naming a DIFFERENT id is a new
	// lapse and starts a new counted lifecycle. Bounded by counted <= maxReauthHolds.
	workflows map[uuid.UUID]*reauthWorkflow
	// counted is how many LIFECYCLES this run has opened, ever — never
	// decremented: the cap is a budget for the run, not a concurrency limit.
	counted int
}

func newReauthCoordinator() *reauthCoordinator {
	return &reauthCoordinator{workflows: map[uuid.UUID]*reauthWorkflow{}, quit: make(chan struct{})}
}

// stop ends every open workflow. Idempotent, and safe on a nil coordinator so a
// caller never has to ask whether the lane is on.
func (c *reauthCoordinator) stop() {
	if c == nil {
		return
	}
	c.once.Do(func() { close(c.quit) })
}

// admit returns the workflow for approvalID. fresh=true means THIS caller
// created it and must start its goroutine. ok=false means the per-run cap
// refuses a NEW lifecycle; an EXISTING one is always joinable, because refusing
// a caller for arriving late would punish it for the cadence of its own SDK.
func (c *reauthCoordinator) admit(approvalID uuid.UUID, budget time.Duration) (wf *reauthWorkflow, fresh, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if existing, found := c.workflows[approvalID]; found {
		return existing, false, true
	}
	if c.counted >= maxReauthHolds {
		return nil, false, false
	}
	c.counted++
	wf = &reauthWorkflow{
		approvalID: approvalID, deadline: time.Now().Add(budget),
		done: make(chan struct{}), poll: holdPollInterval, stop: c.quit,
	}
	c.workflows[approvalID] = wf
	return wf, true, true
}

// approvalReader is the one control-plane read a hold makes. An interface so
// the tests can drive the classification without an HTTP server, and so the
// hold cannot accidentally acquire a second capability.
type approvalReader interface {
	readApproval(ctx context.Context, id uuid.UUID) (state types.ApprovalState, status int, err error)
}

// holdForReauth polls the APPROVAL until it is answered, the budget ends or the
// caller disconnects, then re-resolves the injection exactly once.
func holdForReauth(ctx context.Context, poll time.Duration, stop <-chan struct{},
	base string, token *tokenSource, grantID uuid.UUID,
	client *http.Client, approvals approvalReader, approvalID uuid.UUID,
) (types.ResolvedInjection, error) {
	tick := time.NewTicker(poll)
	defer tick.Stop()
	notFound := 0
	for {
		select {
		case <-stop:
			// The proxy is shutting down: end the hold rather than keep polling
			// about a run that is going away.
			return types.ResolvedInjection{}, errReauthTimedOut
		case <-ctx.Done():
			// Budget spent, or the SDK hung up. Either way there is no
			// credential, and the sign-in is still wanted.
			return types.ResolvedInjection{}, errReauthTimedOut
		case <-tick.C:
		}
		state, status, err := approvals.readApproval(ctx, approvalID)
		switch {
		case err != nil, status >= 500:
			// TRANSIENT. A control plane that is down is not a decision; keep
			// waiting, bounded by the budget.
			continue
		case status == http.StatusUnauthorized, status == http.StatusForbidden, status == http.StatusGone:
			// TERMINAL, AND CLASSIFIED ON PURPOSE (Codex #4). After a run is
			// killed the internal auth/liveness middleware can answer 401/403
			// before the CANCELLED row is readable, and the generic poll treats
			// every non-200 as "still pending" — so the hold used to run its
			// whole budget against a run that had already ended.
			return types.ResolvedInjection{}, errReauthTimedOut
		case status == http.StatusNotFound:
			// A read racing the row's own insert answers 404 once; a row that
			// has really gone answers it every time.
			if notFound++; notFound >= reauth404Reads {
				return types.ResolvedInjection{}, errReauthTimedOut
			}
			continue
		}
		notFound = 0
		switch state {
		case types.ApprovalApproved:
			// ONE re-resolve, on the CURRENT run token.
			//
			// NOT under the budget ctx (security NIT-4): a deadline landing
			// mid-call leaves the control plane with a credential.mint, a
			// secret.read success and a per-run mask registration for a retry
			// the proxy then discards as expired — a trail saying the retry
			// succeeded beside a sandbox that never got a token. Its own short
			// bound instead, so the hold ends on a resolve that either landed
			// or did not.
			rctx, rcancel := context.WithTimeout(context.Background(), reauthFinalResolveTimeout)
			out, rerr := resolveInjection(rctx, base, token.Get(), grantID, client)
			rcancel()
			if rerr != nil {
				return types.ResolvedInjection{}, errReauthTimedOut
			}
			return out, nil
		case types.ApprovalDenied, types.ApprovalExpired, types.ApprovalCancelled:
			return types.ResolvedInjection{}, errReauthTimedOut
		}
	}
}

// httpApprovalReader reads GET /internal/approvals/{id} and reports the STATUS
// alongside the state, which is the whole difference from approvalClient.poll:
// that one folds every non-200 into "not decided" (correct for its own caller,
// and left untouched), and this hold needs to tell a dead run from a slow one.
type httpApprovalReader struct {
	base   string
	token  *tokenSource
	client *http.Client
}

func (a httpApprovalReader) readApproval(ctx context.Context, id uuid.UUID) (types.ApprovalState, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		a.base+"/api/v1/internal/approvals/"+id.String(), nil)
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Authorization", "Bearer "+a.token.Get())
	resp, err := a.client.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", resp.StatusCode, nil
	}
	var ar types.ApprovalRequest
	if err := json.NewDecoder(resp.Body).Decode(&ar); err != nil {
		return "", resp.StatusCode, err
	}
	return ar.State, resp.StatusCode, nil
}

// writeSSOUnauthorized answers a spent hold with the shape the AWS SDKs
// understand.
//
// 401 + __type UnauthorizedException, not the 502 a refresh failure gives:
// UnauthorizedException is a MODELLED, non-retryable GetRoleCredentials error,
// so aws-sdk-js-v3 and botocore both map it to a credential failure and stop,
// where a 502 is a transport error they retry — three more full holds for one
// lapse. The message is Wardyn's own sentence, so the person reading the
// agent's output learns that a sign-in is what fixes this.
func writeSSOUnauthorized(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("x-amzn-errortype", "UnauthorizedException")
	w.WriteHeader(http.StatusUnauthorized)
	body, _ := json.Marshal(map[string]string{
		"__type":  "UnauthorizedException",
		"message": reauthTimedOutSentence,
	})
	_, _ = w.Write(body)
}

// reauthPendingFrom decodes the control plane's 423 body into the id to poll.
// A 423 whose body cannot be read is still a 423 — the hold just has nothing to
// wait on, so it fails closed rather than guessing.
func reauthPendingFrom(body []byte) (errReauthPending, bool) {
	var p struct {
		State      string `json:"state"`
		ApprovalID string `json:"approval_id"`
	}
	if json.Unmarshal(body, &p) != nil {
		return errReauthPending{}, false
	}
	id, err := uuid.Parse(p.ApprovalID)
	if err != nil || id == uuid.Nil {
		return errReauthPending{}, false
	}
	return errReauthPending{approvalID: id}, true
}

// reauthHoldError renders a hold failure for a log line. Never for the sandbox:
// the sandbox gets writeSSOUnauthorized's modelled body.
func reauthHoldError(err error) string { return fmt.Sprintf("credential re-auth hold: %v", err) }

// stopReauthHolds ends every open re-auth hold this proxy owns. Called at
// shutdown; nil-safe at every level, because a run with no injector, no
// captured-SSO lane or no hold in flight must not have to ask.
func (p *Proxy) stopReauthHolds() {
	if p == nil || p.inject == nil {
		return
	}
	p.inject.reauth.stop()
}
