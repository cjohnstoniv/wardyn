// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/hostrules"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Wardyn deliberately has no test-connection buttons anywhere else: a green
// tick that only ever means "we wrote a credential down" is exactly the false
// reassurance that gets someone paged. These two endpoints are the approved
// exception because a REAL test is possible here -- launch a throwaway
// confined sandbox and actually traverse the path, the same footing as
// githubRefRulesetCheck (setup.go), which really does ask GitHub. Keep that
// honesty: report only what the probe actually observed, never an inference.

const (
	// siteConfigProbeIdleCapSec is the probe run's own AutoStopAfterSec: a
	// second, runner-enforced backstop behind siteConfigProbeWaitTimeout
	// (below) in case the control-plane wait itself never got the chance to
	// run (e.g. a daemon restart mid-probe). Deliberately LARGER than the
	// wait timeout so the handler's own reclaim is what normally fires
	// first. 120: the same worst-case math as siteConfigProbeWaitTimeout
	// (task budget + the recorder's upload tail), plus margin over it.
	siteConfigProbeIdleCapSec = 120
	// redirectProbeBypassCode is redirectProbeScript's own explicit sentinel
	// exit code for "the mirror (To) is reachable AND the public host (From)
	// is STILL reachable directly" -- bypass. Chosen far outside curl's own
	// documented exit-code range (0-~96 as of curl 8.x) so it can never
	// collide with a real curl failure code: see redirectProbeScript, where
	// this literal is reached only via its own explicit `exit`, never via a
	// passed-through `$?`.
	redirectProbeBypassCode = 250
	// proxyProbeInterceptedCode is proxyProbeScript's own sentinel for "an
	// endpoint ANSWERED, but with something other than its known payload" --
	// i.e. a captive portal or a corporate block page replied 200 in its place.
	// Same reserved range and same rule as redirectProbeBypassCode: reached
	// only via an explicit `exit`, never a passed-through curl code.
	proxyProbeInterceptedCode = 251
	// waitForRunTerminalPollInterval is how often waitForRunTerminal polls the
	// run row.
	waitForRunTerminalPollInterval = 500 * time.Millisecond
)

// waitForRunTerminal polls the run row until it reaches a terminal state,
// returning that state. It errors on ctx cancellation/timeout (the caller
// reclaims the run). Server-side twin of the CLI's `run --wait` poll.
func (s *Server) waitForRunTerminal(ctx context.Context, runID uuid.UUID) (types.RunState, error) {
	ticker := time.NewTicker(waitForRunTerminalPollInterval)
	defer ticker.Stop()
	for {
		if run, err := s.cfg.Store.GetRun(ctx, runID); err == nil && isTerminalRunState(run.State) {
			return run.State, nil
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-ticker.C:
		}
	}
}

// proxyProbeTargets are the endpoints a connectivity probe tries, in order,
// each paired with a substring its REAL response is known to contain.
//
// Two properties matter, and a plausible-looking pick fails both. First,
// availability: this must not be a host an enterprise plausibly blocks, because
// a false "no internet" now GATES setup. api.github.com was the original choice
// and is a bad one -- plenty of orgs block GitHub outright, and Wardyn would
// have reported their working network as broken. These two are the endpoints
// Windows (NCSI) and Firefox use for their own network detection, so blocking
// them breaks the OS's connectivity indicator -- about as close to
// structurally-unblockable as the public internet offers.
//
// Second, and easier to miss: the response CONTENT has to be checked. A
// corporate block page is a 200 with HTML in it, so an exit-code-only probe
// reports "reached" while egress is firmly blocked -- precisely backwards on
// the networks this feature exists for. Matching the known payload is how
// captive-portal detection works everywhere, and it is why these endpoints
// publish a fixed body at all.
var proxyProbeTargets = []struct{ url, want string }{
	{"https://www.msftconnecttest.com/connecttest.txt", "Microsoft Connect Test"},
	{"https://detectportal.firefox.com/success.txt", "success"},
}

// proxyProbeHosts is proxyProbeTargets' host set, for the probe run's egress
// allowlist. Derived, never hand-listed -- a target added above without its
// host allowed would fail as "DNS resolution failed" and read as a real
// network fault.
var proxyProbeHosts = func() []string {
	hosts := make([]string, 0, len(proxyProbeTargets))
	for _, t := range proxyProbeTargets {
		if h := hostrules.HostOf(t.url); h != "" {
			hosts = append(hosts, h)
		}
	}
	return hosts
}()

// stripURLScheme is display-only compaction for detail messages -- the mock's
// endpoint spelling ("www.msftconnecttest.com/connecttest.txt") drops the
// scheme but keeps the path, because a payload claim is about the full
// endpoint, not just its host.
func stripURLScheme(u string) string {
	if _, after, ok := strings.Cut(u, "://"); ok {
		return after
	}
	return u
}

// proxyProbeEndpointsLabel names every builtin target, scheme-stripped and
// "and"-joined, for the detail messages that make a PAYLOAD claim (reached /
// intercepted). Connection-level failures use the host-only join instead --
// a refused connection is a host-level fact, the path never got a say.
var proxyProbeEndpointsLabel = func() string {
	parts := make([]string, 0, len(proxyProbeTargets))
	for _, t := range proxyProbeTargets {
		parts = append(parts, stripURLScheme(t.url))
	}
	return strings.Join(parts, " and ")
}()

// siteConfigProbeWaitTimeout bounds how long a handler waits for the
// throwaway probe run to reach a terminal state before reclaiming it. The UI
// shows seconds, not minutes, and a healthy path still finishes in ~20s — but
// the budget must cover the run's actual WORST case, not just its curls:
// task (2 targets x 15s --max-time, which bounds the connect too = 30s) + startup (~2s) + the
// recorder's upload tail (uploadClientTimeout, cmd/wardyn-rec/main.go, 20s)
// + margin = 90s. Every curl inside the probe scripts below is capped well
// under this on its own, but a run.exec success does not mean the RUN is
// done — see runSiteConfigProbe's timeout branch, which distinguishes a
// timeout after the task started (state `timed_out`) from one where the
// sandbox never got that far (`not_run`). A var (not a const) purely so
// tests can shrink it instead of taking 90 real seconds to exercise the
// timeout/reclaim path.
var siteConfigProbeWaitTimeout = 90 * time.Second

// proxyProbeScript walks proxyProbeTargets through the sandbox's normal egress
// path (HTTP_PROXY/HTTPS_PROXY already point at wardyn-proxy, which chains to
// the operator's configured upstream automatically -- see
// resolveRunUpstreamProxy in runs_dispatch.go) and succeeds on the FIRST target
// that returns its own known payload.
//
// Three outcomes, deliberately distinguished:
//   - a target answers with its expected body  -> exit 0, genuinely reached.
//   - every target fails at the connection level -> propagate the last curl
//     exit code, so curlExitDetail can name the real cause (DNS/refused/TLS).
//   - something ANSWERED but with the wrong body -> exit the intercepted
//     sentinel. This is the case an exit-code-only probe gets backwards: a
//     corporate block page is a perfectly well-formed 200.
//
// The URL/expectation pairs are compiled-in constants, never operator input, so
// they are interpolated into the script directly (see buildProxyProbeScript);
// nothing a caller controls reaches this shell.
var proxyProbeScript = buildProxyProbeScript()

func buildProxyProbeScript() string {
	var b strings.Builder
	b.WriteString("answered=0\nlast=0\n")
	for _, t := range proxyProbeTargets {
		fmt.Fprintf(&b, `out=$(curl -sS --connect-timeout 5 --max-time 15 %q 2>/dev/null)
rc=$?
if [ "$rc" -eq 0 ]; then
  answered=1
  case "$out" in *%q*) exit 0 ;; esac
else
  last=$rc
fi
`, t.url, t.want)
	}
	fmt.Fprintf(&b, "[ \"$answered\" -eq 1 ] && exit %d\nexit \"$last\"\n", proxyProbeInterceptedCode)
	return b.String()
}

// redirectProbeScript runs the TWO-probe check a redirect needs, inside ONE
// sandbox so both probes observe the SAME confinement class's structural
// egress:
//  1. fetch To through the normal (proxied) path. If this fails, propagate
//     curl's own exit code unchanged (`|| exit $?`) -- classified blocked. -f
//     turns an HTTP-level error response (4xx/5xx) into a failure too (curl
//     exit 22) -- without it, an HTTP error response is a curl SUCCESS
//     (connection worked, curl doesn't look at the status line), so
//     wardyn-proxy's own policy deny (a well-formed 403) scored as "To
//     reached" -- exactly backwards, and the one case this probe exists to
//     catch.
//  2. To succeeded: try From again, but with --noproxy '*' -- a DIRECT dial
//     that bypasses wardyn-proxy's policy entirely, so this tests whether the
//     confinement class's OWN network setup (not the proxy's allowlist)
//     structurally blocks the public host. Success here means the redirect is
//     configured but not enforced (bypass); the script exits the reserved
//     sentinel explicitly (see redirectProbeBypassCode) -- never a
//     passed-through curl code. -f here too, so a public host answering with
//     an HTTP error (rather than refusing the connection outright) is not
//     mistaken for a bypass.
//  3. From correctly failed: the redirect is enforced end to end (reached).
const redirectProbeScript = `curl -sS -f -o /dev/null --connect-timeout 5 --max-time 15 "$WARDYN_PROBE_TO_URL" || exit $?
curl -sS -f -o /dev/null --connect-timeout 5 --max-time 15 --noproxy '*' "$WARDYN_PROBE_FROM_URL" && exit 250
exit 0`

// curlExitDetail maps curl's own stable, documented exit codes to a SPECIFIC,
// real cause -- never a generic "probe failed" string. Only the codes a
// network-reachability probe can plausibly hit are named; any other code
// still reports the real number curl returned rather than inventing a label.
var curlExitDetail = map[int]string{
	6:  "DNS resolution failed",
	7:  "connection refused (or the host is unreachable)",
	22: "the endpoint answered with an HTTP error (4xx/5xx) -- this is what wardyn-proxy's own policy deny looks like, but a mirror's own 401/500 reads identically here; check the mirror directly if the cause matters",
	28: "connection timed out",
	35: "TLS handshake failed",
	52: "empty reply from the server",
	56: "connection reset while receiving data",
	60: "TLS certificate verification failed",
}

// curlFailureDetail reports the REAL, specific reason a probe curl failed.
func curlFailureDetail(exitCode int) string {
	if d, ok := curlExitDetail[exitCode]; ok {
		return d
	}
	return fmt.Sprintf("curl exit code %d", exitCode)
}

// upstreamResolveFailDetail names resolveUpstreamProxyURL's (runs_bedrock.go)
// failReason codes in the same human-readable style as curlFailureDetail.
var upstreamResolveFailDetail = map[string]string{
	"unsupported-scheme":   "it is not an http:// URL (https is not supported)",
	"reserved-secret-name": "its secret ref names a reserved secret",
	"no-secret-store":      "no secret store is configured",
	"secret-not-found":     "its secret ref does not resolve to a stored secret",
}

func upstreamFailDetail(reason string) string {
	if d, ok := upstreamResolveFailDetail[reason]; ok {
		return d
	}
	return reason
}

// probeRunResult is what a throwaway site-config probe run actually observed,
// resolved from its own audit trail -- never inferred. A COMPLETED run always
// reports hasExitCode=true, exitCode=0; a FAILED run reports the real exit
// code the task exited with (hasExitCode=true) -- but a run.complete FAILURE
// event (a Wait error or a watcher panic, see startCompletionWatcher in
// runs_lifecycle.go) carries no exit_code at all, so hasExitCode stays false
// there too, same as a launch that never started. incompleteReason is set
// whenever there IS no exit code to report. neverRan distinguishes the two
// incomplete shapes: true when the
// audit trail's failure event is anything OTHER than run.complete (the
// sandbox never got to running the task at all -- e.g. CreateSandbox itself
// failed), false when it IS run.complete (the task started running; only its
// own completion accounting failed) -- UNLESS that run.complete event itself
// carries exec_started:false (startCompletionWatcher's runner.ErrExecNeverStarted
// branch, runs_lifecycle.go), which means the driver already proved the
// agent exec never started at all, so neverRan is true there too despite
// the action being run.complete. classify* reports either shape as
// `blocked` UNLESS neverRan, which gets its own `not_run` verdict -- "we
// tried and did not get a clean answer" is not the same claim as "nothing
// ever ran to observe".
//
// timedOut is the third, disjoint shape: the WAIT itself expired (never a
// terminal run state at all) after run.exec had already succeeded -- the
// sandbox started and the task launched, so this is provably not a network
// verdict, just a run that never reported back. agentStatus is the sandbox
// agent's own observed state at the deadline (AgentStatus, best-effort --
// "unknown" on any read failure), surfaced in the timed_out detail because
// it is the one thing an operator staring at a stuck probe has no other way
// to see. Set only alongside timedOut; every other field stays zero-value.
type probeRunResult struct {
	hasExitCode      bool
	exitCode         int
	neverRan         bool
	incompleteReason string
	timedOut         bool
	agentStatus      string
	elapsed          time.Duration
}

// runSiteConfigProbe launches ONE throwaway, one-shot exec run (task_mode:
// exec) and waits for it to finish, via the mint -> CreateRun -> dispatchRun ->
// wait -> reclaim-on-timeout shape -- no second dispatch path. script is a FIXED
// shell command, never interpolated with operator-authored data (extraEnv
// carries the actual probe target(s) as plain, non-secret env vars the script
// reads by name). It returns the run id (so a launch failure can still be
// audited against it) and what was actually observed.
//
// grants are the eligible credential grants the probe run should carry (nil for
// the two site-config probes, which authenticate to nothing). They are
// persisted and wired proxy-side exactly as persistRunGrants does for a real
// run, so an integration probe traverses the SAME injection path a run granted
// that integration takes -- the whole point of testing it at all.
func (s *Server) runSiteConfigProbe(ctx context.Context, actor, script string, allowedDomains []string, grants []types.GrantSpec, extraEnv map[string]string) (uuid.UUID, probeRunResult, error) {
	start := s.cfg.Now()
	// Detach the durable launch work from request cancellation -- a client
	// that walks away before the mint/CreateRun lands must not abort it and
	// leave an orphaned identity/row.
	launchCtx := context.WithoutCancel(ctx)
	runID := uuid.New()
	// The probe tests EGRESS, not the operator's confinement floor -- dispatch
	// at the strongest class the runner actually advertises (bestClass), same
	// rationale as launchRecordRun (workspace_run.go). A CC2 floor with no
	// RuntimeClass otherwise fails the probe before it ever reaches the
	// network and reads as a proxy problem it never was. A Capabilities error,
	// or a runner that advertises no usable class at all, is the same honest
	// no_runner state as s.cfg.Runner == nil, just discovered one layer
	// deeper -- both handlers below translate errProbeNoRunner before their
	// generic 500 path.
	caps, capsErr := s.cfg.Runner.Capabilities(ctx)
	cc := bestClass(caps.ConfinementClasses)
	if capsErr != nil || cc == "" {
		return runID, probeRunResult{}, errProbeNoRunner
	}
	run, token, err := s.newStepRun(launchCtx, runID, actor, script, cc, func(run *types.AgentRun) {
		run.AutoStopAfterSec = siteConfigProbeIdleCapSec
		// The probe runs a plain curl, never a coding agent (see the Image
		// comment on dispatchRun below) -- its own agent label should say so,
		// not the newStepRun default of "claude-code". run.Agent gates only
		// claude-code/codex-cli-specific LLM env (runs_dispatch_llm.go), which
		// this probe must never receive anyway, so "base" is also the more
		// correct value, not merely a cosmetic fix.
		run.Agent = "base"
	})
	if err != nil {
		return runID, probeRunResult{}, err
	}
	created, err := s.cfg.Store.CreateRun(launchCtx, run)
	if err != nil {
		s.cfg.Identity.RevokeRun(launchCtx, runID) //nolint:errcheck // best-effort cleanup of the minted-but-unused token
		return runID, probeRunResult{}, fmt.Errorf("create probe run: %w", err)
	}
	injections, err := s.probeInjections(launchCtx, runID, grants)
	if err != nil {
		return runID, probeRunResult{}, err
	}

	s.dispatchRun(launchCtx, created, dispatchParams{
		RunToken: token,
		// "base": the probe runs a plain curl, never a coding agent -- base is
		// the image Wardyn actually publishes for exec-only tasks. "claude-code"
		// here pulled an image the project ships as a devcontainer harness, not
		// a bare registry tag, and reliably failed to pull on any host that
		// hadn't already built it.
		Image: agentImage("base", s.cfg.AgentImages),
		Policy: types.RunPolicySpec{
			MinConfinementClass: cc,
			AllowedDomains:      allowedDomains,
			AutoStopAfterSec:    siteConfigProbeIdleCapSec,
			EligibleGrants:      grants,
		},
		Injections: injections,
		TaskMode:   "exec",
		ExtraEnv:   extraEnv,
	})

	// The wait stays on the CALLER's ctx (a client disconnect stops it early)
	// but is hard-bounded regardless.
	waitCtx, cancel := context.WithTimeout(ctx, siteConfigProbeWaitTimeout)
	defer cancel()
	finalState, werr := s.waitForRunTerminal(waitCtx, runID)
	elapsed := s.cfg.Now().Sub(start)
	if werr != nil {
		// Timed out, or the client left before the probe finished: reclaim the
		// run + its sandbox so a hung probe can never hold one open. This is
		// still a real, definite answer ("no clean response within the budget"), not a
		// transport-level failure of the ENDPOINT -- report it as an incomplete
		// probe (classify* turns that into `blocked`), never a 5xx.
		//
		// BUT a timeout after run.exec already succeeded is not a network
		// verdict at all: the sandbox started and the task launched, so
		// whatever ran out the clock (most often the recorder's own upload
		// tail hanging against an unreachable control plane on Kubernetes,
		// see cmd/wardyn-rec/main.go) is a completion-reporting problem, not
		// proof egress is blocked. Detach: the caller's ctx just expired.
		detachedCtx := context.WithoutCancel(ctx)
		res := probeRunResult{
			incompleteReason: fmt.Sprintf("did not finish within %ds", int(siteConfigProbeWaitTimeout.Seconds())),
			elapsed:          elapsed,
		}
		if s.execSucceeded(detachedCtx, runID) {
			res = probeRunResult{timedOut: true, agentStatus: s.probeAgentStatusAtDeadline(detachedCtx, runID), elapsed: elapsed}
		}
		s.reclaimProbeRun(detachedCtx, runID)
		return runID, res, nil
	}
	if finalState == types.RunCompleted {
		return runID, probeRunResult{hasExitCode: true, exitCode: 0, elapsed: elapsed}, nil
	}
	return runID, s.probeFailureDetail(ctx, runID, elapsed), nil
}

// probeInjections persists the probe run's eligible grants and derives the
// proxy-side injection wiring — the same two steps persistRunGrants does for a
// real run (runs_create.go), minus the SCM lanes a probe never has (an
// integration authors api_key grants only). A write failure is FATAL, exactly
// as it is there: a probe that reports "blocked" because its credential
// silently never got wired would be worse than no probe at all.
func (s *Server) probeInjections(ctx context.Context, runID uuid.UUID, grants []types.GrantSpec) ([]runner.InjectionGrant, error) {
	var out []runner.InjectionGrant
	for _, g := range grants {
		if g.Kind != types.GrantAPIKey || g.RequiresApproval {
			continue
		}
		grantID := uuid.New()
		if _, err := s.cfg.Store.CreateGrant(ctx, types.CredentialGrant{
			ID: grantID, RunID: runID, CreatedAt: s.cfg.Now().UTC(), Spec: g,
		}); err != nil {
			return nil, fmt.Errorf("create probe grant: %w", err)
		}
		rule, err := injectionRuleFromScope(g.Scope)
		if err != nil {
			return nil, fmt.Errorf("probe grant scope: %w", err)
		}
		out = append(out, runner.InjectionGrant{GrantID: grantID, Rule: rule})
	}
	return out, nil
}

// execSucceeded reports whether runID's audit trail already recorded a
// successful run.exec -- i.e. the sandbox started and the agent process
// launched -- as of the read. Used only by runSiteConfigProbe's timeout
// branch to tell "the task never got a chance to run" (not_run, the existing
// path) from "the task ran but the run itself never reported completion"
// (timed_out, a new one).
func (s *Server) execSucceeded(ctx context.Context, runID uuid.UUID) bool {
	events, err := s.cfg.Store.QueryAuditEvents(ctx, runID, 100)
	if err != nil {
		return false
	}
	for _, ev := range events {
		if ev.Action == "run.exec" && ev.Outcome == "success" {
			return true
		}
	}
	return false
}

// probeAgentStatusAtDeadline reads the probe run's sandbox agent status at
// the moment its wait budget expired -- the cluster-internal fact an operator
// staring at a stuck setup screen has no other way to see. Best-effort: a
// missing run row, no configured Runner, or an AgentStatus error all degrade
// to "unknown" rather than failing the whole timed_out report over a status
// read that was only ever a bonus.
func (s *Server) probeAgentStatusAtDeadline(ctx context.Context, runID uuid.UUID) string {
	run, err := s.cfg.Store.GetRun(ctx, runID)
	if err != nil || s.cfg.Runner == nil {
		return "unknown"
	}
	st, err := s.cfg.Runner.AgentStatus(ctx, run.SandboxRef, run.AgentExecID)
	if err != nil {
		return "unknown"
	}
	if st.Message != "" {
		return fmt.Sprintf("%s: %s", st.State, st.Message)
	}
	return string(st.State)
}

// probeTrailSettleTries/Interval bound how long probeFailureDetail waits for a
// terminal run's audit trail to catch up with its state row (see the loop in
// probeFailureDetail). 10 x 200ms: the gap is one recordAudit round-trip.
var (
	probeTrailSettleTries    = 10
	probeTrailSettleInterval = 200 * time.Millisecond
)

// hasRunComplete reports whether events already carry the run.complete event
// the completion watcher records once the task has exited.
func hasRunComplete(events []types.AuditEvent) bool {
	for _, ev := range events {
		if ev.Action == "run.complete" {
			return true
		}
	}
	return false
}

// probeFailureDetail inspects a terminal-FAILED probe run's own audit trail
// for the REAL reason: curl's exit code (from the run.complete event every
// exec-mode run's completion watcher records -- runs_lifecycle.go) when the
// task actually ran, or the first launch-phase failure event when the sandbox
// never got that far (e.g. CreateSandbox itself failed). Never a generic
// label.
func (s *Server) probeFailureDetail(ctx context.Context, runID uuid.UUID, elapsed time.Duration) probeRunResult {
	res := probeRunResult{elapsed: elapsed}
	events, err := s.cfg.Store.QueryAuditEvents(ctx, runID, 100)
	// The run row and the audit trail commit in sequence: the completion
	// watcher CASes the terminal state first and records run.complete inside
	// finalizeRunTail after it (runs_lifecycle.go), so a poll that just saw
	// the terminal state can read a trail with no run.complete AND no failure
	// event yet. Re-read briefly before concluding anything -- unless the run
	// row says it died at launch (a launch failure never gets a run.complete).
	// A launch failure is recognised by the ROW, not the trail: failAndRevoke
	// stamps FailureHint on every create/dispatch failure, whereas a trail can
	// carry NON-fatal failure events (a proxy resolve, a lost sandbox ref)
	// ahead of the run.complete that is still on its way.
	launchFailed := false
	if run, gerr := s.cfg.Store.GetRun(ctx, runID); gerr == nil && run.FailureHint != "" {
		launchFailed = true
	}
	for i := 0; i < probeTrailSettleTries && err == nil && !launchFailed && !hasRunComplete(events); i++ {
		select {
		case <-ctx.Done():
			i = probeTrailSettleTries
		case <-time.After(probeTrailSettleInterval):
			events, err = s.cfg.Store.QueryAuditEvents(ctx, runID, 100)
		}
	}
	if err != nil {
		res.incompleteReason = "could not read the probe run's own audit trail: " + err.Error()
		return res
	}
	for _, ev := range events {
		if ev.Action != "run.complete" {
			continue
		}
		// *int, not int: a run.complete FAILURE event (Wait error or watcher
		// panic) carries no exit_code key at all, and an int field would
		// silently decode that absence as 0 -- a clean exit that never
		// happened, reported as if the probe reached its target. A nil
		// pointer here means "no exit code observed", so this run falls
		// through to the failure-event loop below.
		var d struct {
			ExitCode *int `json:"exit_code"`
		}
		if json.Unmarshal(ev.Data, &d) == nil && d.ExitCode != nil {
			res.hasExitCode, res.exitCode = true, *d.ExitCode
			return res
		}
	}
	// Any launch-phase action (run.create, run.dispatch, run.exec, ...)
	// failing means the sandbox never got to running the task; run.complete
	// itself failing means it did (only the completion accounting did not)
	// -- see the probeRunResult doc comment. The trail is in seq order and
	// some launch-phase failures are NON-fatal (a lost sandbox ref, a grant
	// that could not be written) and precede a run.complete that proves the
	// task ran, so a run.complete failure wins over any earlier one.
	var launchFail *types.AuditEvent
	for i := range events {
		ev := &events[i]
		if ev.Outcome != "failure" {
			continue
		}
		if ev.Action == "run.complete" {
			// exec_started:false (startCompletionWatcher, runs_lifecycle.go)
			// means the driver already proved the agent exec never started
			// at all (runner.ErrExecNeverStarted, e.g. a k8s ephemeral
			// container stuck on a hard Waiting reason) -- the task never
			// ran, same claim as any other launch-phase failure below, even
			// though the event's own action is run.complete.
			var es struct {
				ExecStarted *bool `json:"exec_started"`
			}
			_ = json.Unmarshal(ev.Data, &es)
			res.neverRan = es.ExecStarted != nil && !*es.ExecStarted
			res.incompleteReason = probeFailureReason(ev)
			return res
		}
		if launchFail == nil {
			launchFail = ev
		}
	}
	if launchFail != nil {
		res.neverRan = true
		res.incompleteReason = probeFailureReason(launchFail)
		return res
	}
	res.incompleteReason = "the probe sandbox failed for an unreported reason"
	return res
}

// probeFailureReason words one failure-outcome audit event for a probe verdict:
// "<action>: <error>" when the event carried one, else the action alone.
func probeFailureReason(ev *types.AuditEvent) string {
	var d struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(ev.Data, &d)
	if d.Error != "" {
		return fmt.Sprintf("%s: %s", ev.Action, d.Error)
	}
	return ev.Action + " did not succeed"
}

// reclaimProbeRun tears down a site-config probe run that never reached a
// terminal state within its wait window -- the SAME shape as
// reclaimComposeRun (composeresult.go): CAS to KILLED from whatever
// non-terminal state it's in, then run the shared terminal tail (revoke +
// sandbox teardown), so a hung probe can never hold a live sandbox. A
// separate copy (rather than calling reclaimComposeRun itself) only because
// the audited action name must be this endpoint's own, never "run.compose".
// Best-effort + idempotent: a run that finished on its own is left untouched.
func (s *Server) reclaimProbeRun(ctx context.Context, runID uuid.UUID) {
	run, err := s.cfg.Store.GetRun(ctx, runID)
	if err != nil || isTerminalRunState(run.State) {
		return
	}
	if applied, _ := s.casRunState(ctx, runID, run.State, types.RunKilled); applied {
		s.finalizeRunTail(ctx, runID, run.SandboxRef, "site_config.test_probe",
			"failure", map[string]any{"reason": "probe wait timed out; run reclaimed"})
	}
}

// probeTargetURL normalizes an EgressRedirect endpoint (a full URL OR a bare
// host, per validSiteURLOrHost) into something curlable: pass a URL through
// unchanged, else assume https -- every redirect target this probes is
// dialed as a CONNECT tunnel in real traffic, i.e. always TLS.
func probeTargetURL(raw string) string {
	if strings.Contains(raw, "://") {
		return raw
	}
	return "https://" + raw
}

// handleTestSiteConfigProxy is POST /api/v1/site-config/test-proxy
// (operator-only, audited). It launches a throwaway one-shot sandbox that
// curls a known-reachable host through wardyn-proxy chained to the
// configured upstream proxy, and reports what actually happened. Takes no
// request fields -- there is nothing here for a caller to redirect toward an
// arbitrary target (see the file doc comment for why that matters).
func (s *Server) handleTestSiteConfigProxy(w http.ResponseWriter, r *http.Request) {
	// The body is OPTIONAL. An absent body is the ordinary client shape — a
	// fetch() POST with nothing to send transmits none — so EOF is success
	// here, not a 400. Anything actually sent still decodes strictly, so a
	// typo'd field can't be silently swallowed.
	var req testProxyRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxJSONBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	// A caller-supplied probe target is a deliberate exception to the rule
	// test-redirect follows, and it is safe for reasons that do NOT hold there.
	// The dial happens inside the confined probe sandbox, never from the
	// control plane; the caller is already operator-authenticated and can
	// configure arbitrary run egress anyway, so naming a URL here escalates
	// nothing; and the response body is never returned, so this cannot be used
	// as a read oracle — only reachability is reported. It still has to survive
	// the same validation every stored site-config URL does.
	custom := strings.TrimSpace(req.URL)
	if custom != "" && !validSiteURL(custom) {
		writeError(w, http.StatusBadRequest,
			"url: must be a plain http(s) URL with a real host, and no shell metacharacters")
		return
	}
	ctx := r.Context()
	siteCfg, err := s.cfg.Store.GetSiteConfig(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "get site config: "+err.Error())
		return
	}
	// An unconfigured proxy is not an error, it is the common case — and the
	// question the operator is actually asking ("can a sandbox reach the
	// internet from this host?") is worth answering either way. With no
	// upstream, the probe proves direct egress works; with one, it proves the
	// chain works. Refusing to run without a proxy made the button useless on
	// exactly the hosts where nothing is wrong yet.
	//
	// upstream is a DISPLAY name only: the plain configured URL (which
	// validateSiteConfig guarantees carries no userinfo) or the secret's NAME.
	// The resolved secret VALUE stays inside dispatch (resolveRunUpstreamProxy)
	// and must never surface in a detail or audit line.
	//
	// Set ONLY when it actually RESOLVES (W13-S1-4 / W12-W12-C-2): configured
	// but unresolvable (an https:// URL from a pre-gate row, a dangling secret
	// ref) must never claim "through wardyn-proxy chained to X" — that is
	// exactly the chain dispatch would silently drop and run direct instead,
	// same gate resolveRunUpstreamProxy applies at real dispatch time.
	var getSecret func(context.Context, string) ([]byte, error)
	if s.cfg.Secrets != nil {
		// Operator namespace ONLY, matching resolveRunUpstreamProxy: the probe
		// must resolve the same value real dispatch would.
		getSecret = s.cfg.Secrets.For("").Get
	}
	resolvedUpstream, upstreamFailReason := resolveUpstreamProxyURL(ctx, siteCfg.UpstreamProxyURL, siteCfg.UpstreamProxySecretRef, getSecret)
	var upstream string
	if resolvedUpstream != "" {
		upstream = siteCfg.UpstreamProxyURL
		if upstream == "" {
			upstream = "the upstream proxy in secret " + siteCfg.UpstreamProxySecretRef
		}
	}
	if s.cfg.Runner == nil {
		writeJSON(w, http.StatusOK, noRunnerResponse)
		return
	}

	script, hosts := proxyProbeScript, proxyProbeHosts
	subj := proxyProbeSubject{
		endpoints: proxyProbeEndpointsLabel, hosts: strings.Join(proxyProbeHosts, ", "),
		upstream: upstream, resolveFailReason: upstreamFailReason,
	}
	if custom != "" {
		// One target, and NO body check: Wardyn has no idea what an operator's
		// own endpoint is supposed to return, so the honest claim is only "the
		// request completed" -- classifyProxyProbe words it exactly that way
		// rather than implying the verification the default targets get. -f
		// makes an HTTP error status a failure, the closest thing to a
		// correctness signal available without a known payload.
		script = fmt.Sprintf("curl -fsS -o /dev/null --connect-timeout 5 --max-time 15 %q\n", custom)
		hosts = []string{hostrules.HostOf(custom)}
		subj = proxyProbeSubject{
			endpoints: stripURLScheme(custom), hosts: hostrules.HostOf(custom),
			upstream: upstream, custom: true, resolveFailReason: upstreamFailReason,
		}
	}

	actor := principalFromRequest(r)
	runID, res, perr := s.runSiteConfigProbe(ctx, actor, script, hosts, nil, nil)
	if perr != nil {
		if errors.Is(perr, errProbeNoRunner) {
			// Same STATE as an absent runner, its own detail: "no runner
			// configured" would be false on this host.
			writeJSON(w, http.StatusOK, siteConfigProbeResponse{State: "no_runner", Detail: perr.Error()})
			return
		}
		writeError(w, http.StatusInternalServerError, "launch proxy probe: "+perr.Error())
		return
	}
	resp := classifyProxyProbe(res, subj, s.cfg.ControlPlaneURL)
	resp.Warning = s.probeRecordingWarning(ctx, resp.State, runID)
	s.recordAudit(ctx, s.auditEvent(&runID, actorTypeFromRequest(r), actor, "site_config.test_proxy",
		"site_config", outcomeBool(resp.State == "reached"), mustJSON(map[string]any{
			"state": resp.State, "target_host": strings.Join(hosts, ", "), "elapsed_ms": resp.ElapsedMS,
			"custom_target": custom != "", "intercepted": resp.Intercepted,
		})))
	writeJSON(w, http.StatusOK, resp)
}

// testProxyRequest is the OPTIONAL POST /site-config/test-proxy body. URL lets
// an operator whose host has no public internet -- an internal-only or
// air-gapped deployment -- point the probe at something it CAN reach and prove
// egress genuinely works, instead of clicking past a check it could never pass.
// Empty (or absent) runs the default multi-target, body-verified check.
type testProxyRequest struct {
	URL string `json:"url,omitempty"`
}

// testRedirectRequest is the POST /site-config/test-redirect body. Both
// fields are DISPLAY input only: the probe never dials a caller-supplied
// target. from selects the stored row (404 if it names none); to is never
// trusted (see handleTestSiteConfigRedirect) -- only the row's own stored To
// is ever dialed.
type testRedirectRequest struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// handleTestSiteConfigRedirect is POST /api/v1/site-config/test-redirect
// (operator-only, audited). from must name a row already in the stored
// EgressRedirects (404 otherwise) -- the caller's to is NEVER the probe
// target; only the STORED row's own To is ever dialed, or a caller could turn
// this into an SSRF gadget by naming an arbitrary internal host as "to".
func (s *Server) handleTestSiteConfigRedirect(w http.ResponseWriter, r *http.Request) {
	var req testRedirectRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.From) == "" {
		writeError(w, http.StatusBadRequest, "from is required")
		return
	}
	ctx := r.Context()
	siteCfg, err := s.cfg.Store.GetSiteConfig(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "get site config: "+err.Error())
		return
	}
	red, ok := findEgressRedirect(siteCfg, req.From)
	if !ok {
		writeError(w, http.StatusNotFound, "no egress_redirects entry with that from")
		return
	}
	if s.cfg.Runner == nil {
		writeJSON(w, http.StatusOK, noRunnerResponse)
		return
	}

	toHost := hostrules.HostOf(red.To)
	fromHost := hostrules.HostOf(red.From)
	actor := principalFromRequest(r)
	runID, res, perr := s.runSiteConfigProbe(ctx, actor, redirectProbeScript,
		[]string{toHost}, nil, map[string]string{
			"WARDYN_PROBE_TO_URL":   probeTargetURL(red.To),
			"WARDYN_PROBE_FROM_URL": probeTargetURL(red.From),
		})
	if perr != nil {
		if errors.Is(perr, errProbeNoRunner) {
			// Same STATE as an absent runner, its own detail: "no runner
			// configured" would be false on this host.
			writeJSON(w, http.StatusOK, siteConfigProbeResponse{State: "no_runner", Detail: perr.Error()})
			return
		}
		writeError(w, http.StatusInternalServerError, "launch redirect probe: "+perr.Error())
		return
	}
	resp := classifyRedirectProbe(res, toHost, fromHost, s.cfg.ControlPlaneURL)
	resp.Warning = s.probeRecordingWarning(ctx, resp.State, runID)
	s.recordAudit(ctx, s.auditEvent(&runID, actorTypeFromRequest(r), actor, "site_config.test_redirect",
		"site_config", outcomeBool(resp.State == "reached"), mustJSON(map[string]any{
			"state": resp.State, "to_host": toHost, "from_host": fromHost, "elapsed_ms": resp.ElapsedMS,
		})))
	writeJSON(w, http.StatusOK, resp)
}
