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

// The hold — the second caller of the ResolveWait shape.
//
// A captured AWS SSO session can lapse WHILE a run is working; without this
// hold that's terminal (the sandbox's next GetRoleCredentials fails). The
// control plane answers 423 with the id of a visible sign-in request, and
// this file PARKS that request while its owner signs in, bounded by one knob.
//
// A parked request is not a paused agent: the tool call is just slow, and a
// client that gives up first loses the turn as it does today.
//
// It never re-polls the INJECTION url (every resolve there re-MINTS through
// the broker and commits a credential.mint audit row, so a one-second poll
// would write hundreds of rows onto a hash-chained log). It polls the
// APPROVAL by id, and re-resolves the injection exactly ONCE when answered.

const (
	// envCredentialReauthTimeout bounds ONE re-auth workflow: the approval wait
	// and the single re-resolve that ends it.
	envCredentialReauthTimeout = "WARDYN_CREDENTIAL_REAUTH_TIMEOUT"
	// defaultCredentialReauthTimeout is maxHoldTimeout's own ceiling and the
	// order of a device-code window. A hold that outlives the sign-in it waits
	// for waits for nothing.
	defaultCredentialReauthTimeout = 600 * time.Second
	// maxCredentialReauthTimeout is the clamp's upper bound: an hour-long budget
	// would park a sandbox's connection long past any SDK's or person's patience.
	maxCredentialReauthTimeout = 1800 * time.Second
	// maxReauthHolds bounds re-auth WORKFLOWS per run — the sidecar half of the
	// control plane's own cap of the same name. A run that has asked its owner
	// to sign in eight times is not going to be fixed by a ninth wait.
	maxReauthHolds = 8
	// reauthFinalResolveTimeout bounds the ONE re-resolve that ends a successful
	// hold — deliberately its own bound rather than the budget's remainder.
	reauthFinalResolveTimeout = 30 * time.Second
	// reauth404Reads is how many consecutive 404s end a hold. ONE is too eager
	// (a read racing the row's own insert), forever is a hold waiting on nothing.
	reauth404Reads = 3
	// maxCapabilityHolds bounds Azure DevOps capability ESCALATIONS per run, on
	// their own budget so it can't spend or be spent by the sign-in budget
	// above. The control plane keeps its own cap of the same kind; this one
	// alone is not a limit since a restarted sidecar starts it at zero.
	maxCapabilityHolds = 16
	// maxCapabilityHoldTimeout clamps a capability hold. NOT what keeps a held
	// request's body readable — a request whose body the gate didn't peek is
	// read after the hold, and ReadTimeout counts from its headers, so
	// serveMITMRequest re-arms the read deadline after each holding point.
	// #1197: points at types.HoldWindowADOCapability, the ONE definition
	// of this number — internal/approval.Hold reads the same constant, so the
	// api layer's Held/HeldUntil projection can never drift from this clamp.
	maxCapabilityHoldTimeout = types.HoldWindowADOCapability
)

// minCredentialReauthTimeout is the clamp's lower bound: a 1-second budget
// would make the feature a lie. A var (not const) so a test waiting out a
// hold's FULL budget can shrink the floor instead of taking the real 10s —
// see TestMinCredentialReauthTimeout_ProductionFloorUnchanged.
var minCredentialReauthTimeout = 10 * time.Second

// capabilityHoldBudget is the re-auth knob, clamped to the capability ceiling.
func capabilityHoldBudget() time.Duration {
	return min(credentialReauthBudget(), maxCapabilityHoldTimeout)
}

// injectionStatusError is the plain resolve's non-200, kept typed so a caller
// that needs the status (the capability hold) need not parse the text. Error()
// is the sentence resolveInjection has always returned.
type injectionStatusError struct {
	status int
	body   string
}

func (e injectionStatusError) Error() string {
	return fmt.Sprintf("injection status %d: %s", e.status, e.body)
}

// credentialReauthBudget is the operator's ceiling for ONE hold, clamped.
// Unparseable or non-positive keeps the default — a typo must not turn the
// hold off silently or wedge it open.
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
type errReauthPending struct {
	approvalID uuid.UUID
	// state is the 423 body's own word: reauth_pending (a sign-in or a consent
	// is wanted) or capability_pending (a person is deciding an escalation).
	state string
}

// capabilityPendingState is the 423 state the Azure DevOps capability arm
// answers while a person decides an escalation.
const capabilityPendingState = "capability_pending"

func (e errReauthPending) Error() string {
	return "credential re-auth pending: approval " + e.approvalID.String()
}

// errReauthNoCredential is what EVERY hold that ends without a credential has in
// common: it earns the 401 UnauthorizedException body rather than a generic
// 502 (see writeSSOUnauthorized). Never returned on its own — the sentinels
// below say WHY, and only ONE of them is an expiry.
var errReauthNoCredential = errors.New("credential re-auth hold ended without a credential")

// errReauthTimedOut is the hold's BUDGET running out with nobody signed in —
// and nothing else. It alone earns the credential:reauth-timeout decision row
// and outcome=timeout count: a shutdown, a killed run or an answered-but-not-
// approved request did not expire, and a trail that says they did lies about
// how long the owner had. Handed to the FIRST live observer of the terminal
// result, which writes the ONE decision row.
var errReauthTimedOut = fmt.Errorf("%w: the hold budget expired", errReauthNoCredential)

// errReauthTimedOutAgain is the SAME expiry, already recorded: every later
// caller gets it (errors.Is still true, so the 401 body is unchanged), while
// the decision row is written once per hold rather than once per retry — with
// the ~30s SDK cadence, one row versus twenty.
var errReauthTimedOutAgain = fmt.Errorf("%w (already recorded)", errReauthTimedOut)

// errReauthEnded is a hold that ended for a reason that is NOT its budget
// (shutdown, killed run, answered-but-not-approved, gone request, failed final
// resolve). The sandbox still gets the 401, but no reauth-timeout row and no
// outcome=timeout count, since none of these is an expiry. The reason travels
// for the log line; a run-kill already leaves its own approval.cancel row.
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

// errReauthCapped is the per-run workflow cap refusing a NEW lifecycle. Its own
// sentinel so it reads as a cap rather than an expiry that never happened —
// and, being an errReauthEnded, writes no timeout row, needing no per-retry
// dedupe.
var errReauthCapped = errReauthEnded{reason: "no further sign-in will be requested for this run"}

// reauthTimedOutSentence is what the sandbox's SDK is told: what was done,
// what was not, and — since this is where a person might otherwise conclude a
// credential was silently swapped — that nothing was.
//
// DRAFT (M2 canon pending)
const reauthTimedOutSentence = "wardyn held this AWS SSO credential request while its owner was " +
	"asked to sign in again, and nobody signed in before the hold expired; nothing was substituted"

// reauthEndedSentence is the same answer for a hold that did NOT expire — the
// sentence above would be false for a killed run or shut-down proxy (the
// owner may have had seconds, not the whole budget).
//
// DRAFT (M2 canon pending)
const reauthEndedSentence = "wardyn asked this AWS SSO credential request's owner to sign in again, and " +
	"the request ended before a sign-in arrived; nothing was substituted"

// reauthWorkflow is ONE bounded re-auth lifecycle, shared by every caller that
// arrives for the same approval id.
//
// It owns its own goroutine — running the poll loop on a caller's goroutine
// under the entry's reMu instead would make followers queue on an
// uncancellable mutex, never release a disconnected SDK, and let the caller
// whose budget ended delete the workflow so the next mutex holder re-resolves
// and opens a NEW counted workflow with a FULL fresh budget — up to
// maxReauthHolds serial budgets for ONE lapse.
//
// Detaching the loop makes every clause true by construction: the DEADLINE is
// fixed at creation, belonging to the workflow, not whoever is waiting; every
// caller merely WAITS on wf.done or its own ctx, so hanging up is free; and a
// caller leaving does not end the hold, so the next retry joins the SAME wait.
type reauthWorkflow struct {
	approvalID uuid.UUID
	deadline   time.Time
	done       chan struct{}
	// resolved/err are written ONCE by run(), before done is closed; every
	// reader reads them after done is closed. No lock needed and none wanted.
	resolved types.ResolvedInjection
	err      error
	// poll is the tick this workflow uses, SNAPSHOT at creation: the package
	// var it comes from is a test seam, and this goroutine outlives its first
	// caller, so reading the var here would race a test's own restore.
	poll time.Duration
	// stop ends the workflow early on a clean proxy shutdown, so poll loops
	// don't keep talking to a control plane about an ended run.
	stop chan struct{}
	// reported claims the ONE decision row for this workflow's expiry.
	reported atomic.Bool
	// query is the capability hold's re-resolve ask (nil on the re-auth hold).
	query url.Values
	// onceTaken is claimed by the ONE waiter that forwards on a `once`
	// approval: every request parked on the same approval shares this
	// workflow, and "once" is one request, not one per waiter.
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

// await blocks until the workflow finishes or the CALLER's ctx ends.
//
// A ctx that ends first returns ctx.Err() and nothing else: no decision row,
// no 401 body, no effect on the workflow — the hung-up SDK, for which the
// MITM lane writes nothing.
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
	// first caller's — a disconnecting caller must not end a hold its owner is
	// still signing in for.
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
	// workflows keeps TERMINAL workflows too, deliberately: a later 423 naming
	// the same approval id must get that terminal result at once — same lapse,
	// same PENDING row, no second hold/count/row. A 423 naming a DIFFERENT id
	// starts a new counted lifecycle. Bounded by counted <= maxReauthHolds.
	workflows map[uuid.UUID]*reauthWorkflow
	// counted is how many LIFECYCLES this run has opened, ever — never
	// decremented: the cap is a budget for the run, not a concurrency limit.
	counted int
	// capCounted is the same budget for capability escalations, counted
	// separately (maxCapabilityHolds).
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
// refuses a NEW lifecycle; an EXISTING one is always joinable, since refusing
// a caller for arriving late would punish it for its own SDK's cadence.
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
		// A capability hold that ENDED WITHOUT A CREDENTIAL is not sticky: the
		// control plane only reuses an approval id while it's still open or
		// approved-and-unspent, so a retry handed this id has a live question
		// behind it. Joining the dead workflow would refuse every retry until
		// restart; the retry waits again instead, counted against
		// maxCapabilityHolds.
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

// approvalReader is the one control-plane read a hold makes. An interface so
// the tests can drive the classification without an HTTP server, and so the
// hold cannot accidentally acquire a second capability.
type approvalReader interface {
	readApproval(ctx context.Context, id uuid.UUID) (state types.ApprovalState, status int, err error)
}

// holdForReauth polls the APPROVAL until it is answered, the budget ends or the
// caller disconnects, then re-resolves the injection exactly once.
//
// query is the capability hold's re-resolve ask (nil = the re-auth hold). On
// that hold a re-resolve may answer a NEW 423 (the approved capability needs a
// consent not yet given), and the loop then polls THAT request under the same
// deadline. The ask itself doesn't change: it still names the escalation
// approval, spent only by the re-resolve that finally succeeds.
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
			// The proxy is shutting down: end the hold rather than keep polling
			// about a run that is going away. NOT an expiry — the budget may
			// have had minutes left.
			return types.ResolvedInjection{}, reauthEndedShutdown
		case <-ctx.Done():
			// Budget spent, or the SDK hung up. Either way there is no
			// credential, and the sign-in is still wanted.
			return types.ResolvedInjection{}, errReauthTimedOut
		case <-tick.C:
		}
		state, status, err := approvals.readApproval(ctx, approvalID)
		switch {
		case err != nil, status >= 500:
			// Transient. A control plane that is down is not a decision; keep
			// waiting, bounded by the budget.
			continue
		case status == http.StatusUnauthorized, status == http.StatusForbidden, status == http.StatusGone:
			// Terminal, and classified on purpose: after a run is killed the
			// internal auth/liveness middleware can answer 401/403 before the
			// CANCELLED row is readable. Treating that like the generic poll
			// treats every other non-200 — "still pending" — would run the
			// hold's whole budget against a run that has already ended.
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
			// ONE re-resolve, on the CURRENT run token, NOT under the budget
			// ctx: a deadline landing mid-call would leave the control plane
			// with a mint/read success and a mask registration for a retry the
			// proxy discards as expired. Its own short bound instead, so the
			// hold ends on a resolve that either landed or did not.
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

// httpApprovalReader reads GET /internal/approvals/{id} and reports the STATUS
// alongside the state — the whole difference from approvalClient.poll, which
// folds every non-200 into "not decided"; this hold needs to tell a dead run
// from a slow one.
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
// parse as a modelled service error, instead of plain text. status/errType are
// a DELIBERATE per-class choice, never a default: keeping 502 for everything
// lets an SDK retry the same unparseable body ~20 times in seconds. message is
// the SAME masked, topology-redacted sentence the decision log's Cause field
// carries — never a raw error, never a credential.
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
// understand: 401 + __type UnauthorizedException, not the 502 a refresh
// failure gives — a MODELLED, non-retryable GetRoleCredentials error both
// aws-sdk-js-v3 and botocore map to a credential failure and stop on, where a
// 502 is a transport error they'd retry (three more full holds for one lapse).
func writeSSOUnauthorized(w http.ResponseWriter, sentence string) {
	writeAWSSDKError(w, http.StatusUnauthorized, "UnauthorizedException", sentence)
}

// isAWSSSOPortalHost matches ssoPortalHost's shape:
// portal.sso.<region>.amazonaws.com, the run's own AWS IAM Identity Center
// portal. Anchored the same way isBedrockHost is — a literal 5-label
// AWS-owned DNS shape, never a substring match — because this decides
// whether a refusal earns the MODELLED AWS error body instead of plain text.
func isAWSSSOPortalHost(h string) bool {
	labels := strings.Split(h, ".")
	return len(labels) == 5 && labels[0] == "portal" && labels[1] == "sso" &&
		awsRegionLabel.MatchString(labels[2]) && labels[3] == "amazonaws" && labels[4] == "com"
}

// isAWSLane reports whether host is a genuine AWS SDK endpoint — the run's
// own SSO portal or a Bedrock endpoint — so a dial/vet/credential-shaped
// refusal on it earns writeAWSSDKError's modelled body.
//
// TRUST BOUNDARY: NOT the MITM-eligibility set (isMITMHost) and NOT the
// LLM-classification set (isLLMHost) — NARROWER than both. Anthropic, OpenAI
// and a corp artifact mirror are MITM/LLM but not AWS SDK endpoints and keep
// the plain-text body; a configured LLM gateway is LLM-classified but not in
// this set either — it's the OPERATOR'S OWN endpoint, not AWS's. Conflating
// any of these is a body-shape bug, never a TLS-interception widening
// (isMITMHost alone decides whether a tunnel is terminated at all).
func isAWSLane(host string) bool {
	h := strings.TrimSuffix(strings.ToLower(host), ".")
	return isAWSSSOPortalHost(h) || isBedrockHost(h)
}

// httpErrorAWSAware is httpError's AWS-LANE-AWARE sibling: on the run's own
// SSO portal or a Bedrock endpoint it answers with the modelled AWS SDK error
// body instead of plain text — an AWS SDK hands that text straight to a JSON
// parser, the crash this lane fixes. Every other host keeps today's body.
//
// withStage selects Cause's stage-prefixed composition for a genuinely
// dial-shaped err (forwardInspectedLLM's RoundTrip failure) versus the bare
// masked sentence for a resolve/vet or credential-refresh failure, where
// "tcp dial"/"tls handshake" don't apply. status/errType are the caller's
// per-class decision.
func (p *Proxy) httpErrorAWSAware(w http.ResponseWriter, host, plainMsg string, err error, withStage bool, status int, errType string) {
	if isAWSLane(host) {
		msg := p.dialFailureCause(err)
		if withStage {
			msg = p.causeSentence(err)
		}
		writeAWSSDKError(w, status, errType, msg)
		return
	}
	p.httpError(w, plainMsg, err, http.StatusBadGateway)
}

// reauthPendingFrom decodes the control plane's 423 body into the id to poll.
// A 423 whose body cannot be read is still a 423 — the hold just has nothing
// to wait on, so it fails closed rather than guessing.
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
