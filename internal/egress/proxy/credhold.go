// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Parks a request behind a control-plane 423 while its owner re-authenticates.
// SECURITY: polls the approval by id, never the injection url — re-polling it
// would re-mint through the broker and spam the credential.mint audit log.

const (
	// envCredentialReauthTimeout bounds ONE re-auth workflow end to end.
	envCredentialReauthTimeout = "WARDYN_CREDENTIAL_REAUTH_TIMEOUT"
	// defaultCredentialReauthTimeout matches a device-code window's order.
	defaultCredentialReauthTimeout = 600 * time.Second
	// maxCredentialReauthTimeout is the clamp's upper bound.
	maxCredentialReauthTimeout = 1800 * time.Second
	// maxReauthHolds bounds re-auth WORKFLOWS per run.
	maxReauthHolds = 8
	// reauthFinalResolveTimeout bounds the one re-resolve that ends a hold.
	reauthFinalResolveTimeout = 30 * time.Second
	// reauth404Reads is how many consecutive 404s end a hold.
	reauth404Reads = 3
	// maxCapabilityHolds bounds Azure DevOps capability escalations per run,
	// on its own budget separate from the sign-in budget above.
	maxCapabilityHolds = 16
	// maxCapabilityHoldTimeout clamps a capability hold; points at
	// types.HoldWindowADOCapability so approval.Hold's projection can't drift.
	maxCapabilityHoldTimeout = types.HoldWindowADOCapability
)

// minCredentialReauthTimeout is the clamp's lower bound; a var so a test can
// shrink it instead of waiting out the real 10s.
var minCredentialReauthTimeout = 10 * time.Second

// capabilityHoldBudget is the re-auth knob, clamped to the capability ceiling.
func capabilityHoldBudget() time.Duration {
	return min(credentialReauthBudget(), maxCapabilityHoldTimeout)
}

// injectionStatusError is the plain resolve's non-200, kept typed so a caller
// that needs the status need not parse the text.
type injectionStatusError struct {
	status int
	body   string
}

func (e injectionStatusError) Error() string {
	return fmt.Sprintf("injection status %d: %s", e.status, e.body)
}

// credentialReauthBudget is the operator's ceiling for ONE hold, clamped;
// unparseable or non-positive keeps the default.
func credentialReauthBudget() time.Duration {
	d, err := time.ParseDuration(os.Getenv(envCredentialReauthTimeout))
	if err != nil || d <= 0 {
		return defaultCredentialReauthTimeout
	}
	return min(max(d, minCredentialReauthTimeout), maxCredentialReauthTimeout)
}

// errReauthPending is a 423 asking for a human; it carries the id to poll.
type errReauthPending struct {
	approvalID uuid.UUID
	// state is the 423 body's own word: reauth_pending or capability_pending.
	state string
}

// capabilityPendingState is the 423 state the Azure DevOps capability arm
// answers while a person decides an escalation.
const capabilityPendingState = "capability_pending"

func (e errReauthPending) Error() string {
	return "credential re-auth pending: approval " + e.approvalID.String()
}

// errReauthNoCredential is common to every hold that ends without a
// credential; it earns the 401 UnauthorizedException body, never a generic
// 502 (see writeSSOUnauthorized). Never returned on its own.
var errReauthNoCredential = errors.New("credential re-auth hold ended without a credential")

// errReauthTimedOut is the hold's budget running out with nobody signed in.
// It alone earns the credential:reauth-timeout decision row, since a
// shutdown, killed run, or answered-but-not-approved request did not expire.
var errReauthTimedOut = fmt.Errorf("%w: the hold budget expired", errReauthNoCredential)

// errReauthTimedOutAgain is the same expiry, already recorded, so the
// decision row is written once per hold rather than once per retry.
var errReauthTimedOutAgain = fmt.Errorf("%w (already recorded)", errReauthTimedOut)

// errReauthEnded is a hold that ended for a reason that is NOT its budget, so
// it earns no reauth-timeout row or outcome=timeout count. The reason travels
// for the log line.
type errReauthEnded struct{ reason string }

func (e errReauthEnded) Error() string { return "credential re-auth hold ended: " + e.reason }
func (e errReauthEnded) Unwrap() error { return errReauthNoCredential }

// The reasons, named once so the log line and the tests agree.
var (
	reauthEndedShutdown    = errReauthEnded{reason: "the proxy is shutting down"}
	reauthEndedRunGone     = errReauthEnded{reason: "the run has ended"}
	reauthEndedAnswered    = errReauthEnded{reason: "the sign-in request was answered without an approval"}
	reauthEndedRequestGone = errReauthEnded{reason: "the sign-in request is gone"}
	reauthEndedResolve     = errReauthEnded{reason: "the credential could not be resolved after the sign-in"}
)

// errReauthCapped is the per-run workflow cap refusing a NEW lifecycle; being
// an errReauthEnded, it writes no timeout row.
var errReauthCapped = errReauthEnded{reason: "no further sign-in will be requested for this run"}

// reauthTimedOutSentence tells the sandbox's SDK nothing was substituted.
//
// DRAFT (M2 canon pending)
const reauthTimedOutSentence = "wardyn held this AWS SSO credential request while its owner was " +
	"asked to sign in again, and nobody signed in before the hold expired; nothing was substituted"

// reauthEndedSentence is the same answer for a hold that did NOT expire.
//
// DRAFT (M2 canon pending)
const reauthEndedSentence = "wardyn asked this AWS SSO credential request's owner to sign in again, and " +
	"the request ended before a sign-in arrived; nothing was substituted"

// reauthWorkflow is ONE bounded re-auth lifecycle, shared by every caller for
// the same approval id. It owns its own goroutine rather than running the
// poll loop under the entry's reMu, so a caller hanging up is free and a
// retry joins the same wait instead of opening a fresh counted workflow.
type reauthWorkflow struct {
	approvalID uuid.UUID
	deadline   time.Time
	done       chan struct{}
	// resolved/err are written ONCE by run(), before done is closed.
	resolved types.ResolvedInjection
	err      error
	// poll is the tick this workflow uses, snapshot at creation (a test seam).
	poll time.Duration
	// stop ends the workflow early on a clean proxy shutdown.
	stop chan struct{}
	// reported claims the ONE decision row for this workflow's expiry.
	reported atomic.Bool
	// query is the capability hold's re-resolve ask (nil on the re-auth hold).
	query url.Values
	// onceTaken is claimed by the ONE waiter that forwards on a `once` approval.
	onceTaken atomic.Bool
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

// await blocks until the workflow finishes or the CALLER's ctx ends. A ctx
// that ends first returns ctx.Err() and nothing else: no decision row, no
// 401 body, no effect on the workflow.
func (w *reauthWorkflow) await(ctx context.Context) (types.ResolvedInjection, error) {
	select {
	case <-w.done:
	case <-ctx.Done():
		return types.ResolvedInjection{}, ctx.Err()
	}
	if w.err == nil {
		return w.resolved, nil
	}
	// The first live observer reports the expiry; later callers get the same
	// answer without a second decision row.
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
	// context.Background: the budget is the WORKFLOW's, not the first
	// caller's, so a disconnecting caller can't end a hold still in progress.
	ctx, cancel := context.WithDeadline(context.Background(), w.deadline)
	defer cancel()
	w.resolved, w.err = holdForReauth(ctx, w.poll, w.stop, base, token, grantID, w.query, client, approvals, w.approvalID)
	close(w.done)
}

// reauthCoordinator owns the workflows, keyed by APPROVAL ID. One per injector.
type reauthCoordinator struct {
	mu sync.Mutex
	// quit ends every workflow this coordinator owns (proxy shutdown).
	quit chan struct{}
	once sync.Once
	// workflows keeps TERMINAL workflows too, so a later 423 naming the same
	// approval id gets that result at once instead of a second hold/row.
	workflows map[uuid.UUID]*reauthWorkflow
	// counted is how many LIFECYCLES this run has opened, ever — never
	// decremented; the cap is a run budget, not a concurrency limit.
	counted int
	// capCounted is the same budget for capability escalations (maxCapabilityHolds).
	capCounted int
	// standing is what a person approved "for this run" mid-run, on top of the
	// run's dispatch-time grant. Only ever grows.
	standing map[adoscope.Capability]bool
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
// refuses a NEW lifecycle; an EXISTING one is always joinable.
func (c *reauthCoordinator) admit(approvalID uuid.UUID, budget time.Duration) (wf *reauthWorkflow, fresh, ok bool) {
	return c.admitWith(approvalID, budget, nil)
}

// admitCapability is admit for an Azure DevOps escalation: its own budget, and
// the re-resolve ask the workflow ends with.
func (c *reauthCoordinator) admitCapability(approvalID uuid.UUID, query url.Values) (wf *reauthWorkflow, fresh, ok bool) {
	return c.admitWith(approvalID, capabilityHoldBudget(), query)
}

func (c *reauthCoordinator) admitWith(approvalID uuid.UUID, budget time.Duration, query url.Values) (wf *reauthWorkflow, fresh, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if existing, found := c.workflows[approvalID]; found {
		// A capability hold that ended without a credential is not sticky: a
		// retry behind this id waits again, counted against maxCapabilityHolds.
		if query == nil || !existing.finished() || existing.err == nil {
			return existing, false, true
		}
	}
	counter, limit := &c.counted, maxReauthHolds
	if query != nil {
		counter, limit = &c.capCounted, maxCapabilityHolds
	}
	if *counter >= limit {
		return nil, false, false
	}
	*counter++
	wf = &reauthWorkflow{
		approvalID: approvalID, deadline: time.Now().Add(budget),
		done: make(chan struct{}), poll: holdPollInterval, stop: c.quit, query: query,
	}
	c.workflows[approvalID] = wf
	return wf, true, true
}

// widen records a capability approved for the rest of this run.
func (c *reauthCoordinator) widen(capability adoscope.Capability) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.standing == nil {
		c.standing = map[adoscope.Capability]bool{}
	}
	c.standing[capability] = true
}

// holds reports whether capability was approved for the rest of this run.
func (c *reauthCoordinator) holds(capability adoscope.Capability) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.standing[capability]
}

// approvalReader is the one control-plane read a hold makes; an interface so
// tests can drive the classification without an HTTP server.
type approvalReader interface {
	readApproval(ctx context.Context, id uuid.UUID) (state types.ApprovalState, status int, err error)
}

// holdForReauth polls the APPROVAL until answered, the budget ends, or the
// caller disconnects, then re-resolves the injection exactly once. query is
// the capability hold's re-resolve ask (nil = the re-auth hold): a re-resolve
// there may answer a NEW 423, and the loop then polls that request under the
// same deadline.
func holdForReauth(ctx context.Context, poll time.Duration, stop <-chan struct{},
	base string, token *tokenSource, grantID uuid.UUID, query url.Values,
	client *http.Client, approvals approvalReader, approvalID uuid.UUID,
) (types.ResolvedInjection, error) {
	tick := time.NewTicker(poll)
	defer tick.Stop()
	notFound := 0
	for {
		select {
		case <-stop:
			// Shutting down: not an expiry, the budget may have had time left.
			return types.ResolvedInjection{}, reauthEndedShutdown
		case <-ctx.Done():
			return types.ResolvedInjection{}, errReauthTimedOut
		case <-tick.C:
		}
		state, status, err := approvals.readApproval(ctx, approvalID)
		switch {
		case err != nil, status >= 500:
			// Transient: a down control plane is not a decision.
			continue
		case status == http.StatusUnauthorized, status == http.StatusForbidden, status == http.StatusGone:
			// After a run is killed, auth/liveness middleware can answer
			// 401/403 before the CANCELLED row is readable; treat as terminal.
			return types.ResolvedInjection{}, reauthEndedRunGone
		case status == http.StatusNotFound:
			// A read racing the row's own insert answers 404 once; a row that
			// has really gone answers it every time.
			if notFound++; notFound >= reauth404Reads {
				return types.ResolvedInjection{}, reauthEndedRequestGone
			}
			continue
		}
		notFound = 0
		switch state {
		case types.ApprovalApproved:
			// Its own short bound, not the budget ctx: a deadline landing
			// mid-call would leave a mint success the proxy discards as expired.
			rctx, rcancel := context.WithTimeout(context.Background(), reauthFinalResolveTimeout)
			out, rerr := resolveInjectionQuery(rctx, base, token.Get(), grantID, query, client)
			rcancel()
			var next errReauthPending
			if query != nil && errors.As(rerr, &next) && next.approvalID != approvalID {
				approvalID, notFound = next.approvalID, 0
				continue
			}
			if rerr != nil {
				return types.ResolvedInjection{}, reauthEndedResolve
			}
			return out, nil
		case types.ApprovalDenied, types.ApprovalExpired, types.ApprovalCancelled:
			return types.ResolvedInjection{}, reauthEndedAnswered
		}
	}
}

// httpApprovalReader reads GET /internal/approvals/{id} and reports the
// STATUS alongside the state, so this hold can tell a dead run from a slow one.
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

// writeAWSSDKError answers an AWS-lane refusal with the shape both AWS SDKs
// parse as a modelled service error, instead of plain text — a 502 lets an
// SDK retry the same unparseable body ~20 times. SECURITY: message is the
// same masked, topology-redacted sentence as the decision log's Cause field,
// never a raw error or a credential.
func writeAWSSDKError(w http.ResponseWriter, status int, errType, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("x-amzn-errortype", errType)
	w.WriteHeader(status)
	body, _ := json.Marshal(map[string]string{
		"__type":  errType,
		"message": message,
	})
	_, _ = w.Write(body)
}

// writeSSOUnauthorized answers a spent hold with the shape the AWS SDKs
// understand: 401 + __type UnauthorizedException, a modelled non-retryable
// error, not the 502 a refresh failure gives (which both SDKs retry).
func writeSSOUnauthorized(w http.ResponseWriter, sentence string) {
	writeAWSSDKError(w, http.StatusUnauthorized, "UnauthorizedException", sentence)
}

// isAWSSSOPortalHost matches portal.sso.<region>.amazonaws.com, anchored like
// isBedrockHost — a literal 5-label shape, never a substring match.
func isAWSSSOPortalHost(h string) bool {
	labels := strings.Split(h, ".")
	return len(labels) == 5 && labels[0] == "portal" && labels[1] == "sso" &&
		awsRegionLabel.MatchString(labels[2]) && labels[3] == "amazonaws" && labels[4] == "com"
}

// isAWSLane reports whether host is a genuine AWS SDK endpoint, so a
// dial/vet/credential-shaped refusal on it earns writeAWSSDKError's modelled
// body. TRUST BOUNDARY: narrower than isMITMHost/isLLMHost — conflating these
// is a body-shape bug only; isMITMHost alone still gates tunnel termination.
func isAWSLane(host string) bool {
	h := strings.TrimSuffix(strings.ToLower(host), ".")
	return isAWSSSOPortalHost(h) || isBedrockHost(h)
}

// httpErrorAWSAware is httpError's AWS-lane-aware sibling: on an AWS endpoint
// it answers with the modelled AWS SDK error body instead of plain text,
// since an AWS SDK hands plain text straight to a JSON parser. withStage
// selects Cause's stage-prefixed composition for a dial-shaped err versus the
// bare masked sentence for a resolve/vet/refresh failure.
func (p *Proxy) httpErrorAWSAware(w http.ResponseWriter, host, plainMsg string, err error, withStage bool, status int, errType string) {
	if isAWSLane(host) {
		msg := p.dialFailureCause(err)
		if withStage {
			msg = p.causeSentence(err)
		}
		slog.Warn("proxy error returned to the sandbox", "msg", plainMsg, "status", status, "err", msg)
		writeAWSSDKError(w, status, errType, msg)
		return
	}
	p.httpError(w, plainMsg, err, http.StatusBadGateway)
}

// reauthPendingFrom decodes the control plane's 423 body into the id to poll;
// a body that cannot be read fails closed rather than guessing.
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
	return errReauthPending{approvalID: id, state: p.State}, true
}

// reauthHoldError renders a hold failure for a log line, never the sandbox.
func reauthHoldError(err error) string { return fmt.Sprintf("credential re-auth hold: %v", err) }

// stopReauthHolds ends every open re-auth hold this proxy owns; nil-safe at
// every level.
func (p *Proxy) stopReauthHolds() {
	if p == nil || p.inject == nil {
		return
	}
	p.inject.reauth.stop()
}
