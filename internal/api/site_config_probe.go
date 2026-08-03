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

	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/internal/workspacescan"
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
	// wait timeout so the handler's own reclaim is what normally fires first.
	siteConfigProbeIdleCapSec = 60
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
)

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
		if h := workspacescan.HostOf(t.url); h != "" {
			hosts = append(hosts, h)
		}
	}
	return hosts
}()

// siteConfigProbeWaitTimeout bounds how long a handler waits for the
// throwaway probe run to reach a terminal state before reclaiming it. The UI
// shows seconds, not minutes; every curl inside the probe scripts below is
// capped well under this, so the budget is never the tight constraint. A var
// (not a const) purely so tests can shrink it instead of taking 50 real
// seconds to exercise the timeout/reclaim path.
var siteConfigProbeWaitTimeout = 50 * time.Second

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
//     curl's own exit code unchanged (`|| exit $?`) -- classified blocked.
//  2. To succeeded: try From again, but with --noproxy '*' -- a DIRECT dial
//     that bypasses wardyn-proxy's policy entirely, so this tests whether the
//     confinement class's OWN network setup (not the proxy's allowlist)
//     structurally blocks the public host. Success here means the redirect is
//     configured but not enforced (bypass); the script exits the reserved
//     sentinel explicitly (see redirectProbeBypassCode) -- never a
//     passed-through curl code.
//  3. From correctly failed: the redirect is enforced end to end (reached).
const redirectProbeScript = `curl -sS -o /dev/null --connect-timeout 5 --max-time 15 "$WARDYN_PROBE_TO_URL" || exit $?
curl -sS -o /dev/null --connect-timeout 5 --max-time 15 --noproxy '*' "$WARDYN_PROBE_FROM_URL" && exit 250
exit 0`

// curlExitDetail maps curl's own stable, documented exit codes to a SPECIFIC,
// real cause -- never a generic "probe failed" string. Only the codes a
// network-reachability probe can plausibly hit are named; any other code
// still reports the real number curl returned rather than inventing a label.
var curlExitDetail = map[int]string{
	6:  "DNS resolution failed",
	7:  "connection refused (or the host is unreachable)",
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

// probeRunResult is what a throwaway site-config probe run actually observed,
// resolved from its own audit trail -- never inferred. A COMPLETED run always
// reports hasExitCode=true, exitCode=0; a FAILED run reports the real exit
// code the task exited with (hasExitCode=true). incompleteReason is set
// instead whenever there IS no exit code to report: the sandbox never got to
// running the task at all (e.g. CreateSandbox itself failed), or the wait
// budget ran out before the run reached a terminal state. Either way this is
// still a definite, real observation -- classify* reports it as `blocked`,
// never a transport-level error, because "we tried and did not get a clean
// answer" is exactly what blocked means.
type probeRunResult struct {
	hasExitCode      bool
	exitCode         int
	incompleteReason string
	elapsed          time.Duration
}

// runSiteConfigProbe launches ONE throwaway, one-shot exec run (task_mode:
// exec) and waits for it to finish, reusing the exact mint -> CreateRun ->
// dispatchRun -> wait -> reclaim-on-timeout shape RunClaudeCompose already
// uses (composeresult.go) -- no second dispatch path. script is a FIXED shell
// command, never interpolated with operator-authored data (extraEnv carries
// the actual probe target(s) as plain, non-secret env vars the script reads
// by name -- the same channel compose uses for WARDYN_COMPOSE_*). It returns
// the run id (so a launch failure can still be audited against it) and what
// was actually observed.
func (s *Server) runSiteConfigProbe(ctx context.Context, actor, script string, allowedDomains []string, extraEnv map[string]string) (uuid.UUID, probeRunResult, error) {
	start := s.cfg.Now()
	// Detach the durable launch work from request cancellation -- a client
	// that walks away before the mint/CreateRun lands must not abort it and
	// leave an orphaned identity/row (same rationale as RunClaudeCompose).
	launchCtx := context.WithoutCancel(ctx)
	runID := uuid.New()
	id, err := s.cfg.Identity.MintRunIdentity(launchCtx, runID, actor, actor, internalAudience)
	if err != nil {
		return runID, probeRunResult{}, fmt.Errorf("mint run identity: %w", err)
	}
	// Read-only, ephemeral, holds no credentials -- the operator's floor still
	// governs, exactly like launchScanRun's rationale (workspace_run.go).
	cc := s.defaultFloorClass()
	now := s.cfg.Now().UTC()
	run := types.AgentRun{
		ID: runID, CreatedAt: now, UpdatedAt: now, CreatedBy: actor,
		Agent: "claude-code", Task: script,
		ConfinementClass: cc, State: types.RunPending, SPIFFEID: id.SPIFFEID,
		RunnerTarget:     s.cfg.RunnerTarget,
		AutoStopAfterSec: siteConfigProbeIdleCapSec,
	}
	created, err := s.cfg.Store.CreateRun(launchCtx, run)
	if err != nil {
		s.cfg.Identity.RevokeRun(launchCtx, runID) //nolint:errcheck // best-effort cleanup of the minted-but-unused token
		return runID, probeRunResult{}, fmt.Errorf("create probe run: %w", err)
	}

	s.dispatchRun(launchCtx, created, dispatchParams{
		RunToken: id.Token,
		Image:    agentImage("claude-code", s.cfg.AgentImages),
		Policy: types.RunPolicySpec{
			MinConfinementClass: cc,
			AllowedDomains:      allowedDomains,
			AutoStopAfterSec:    siteConfigProbeIdleCapSec,
		},
		TaskMode: "exec",
		ExtraEnv: extraEnv,
	})

	// The wait stays on the CALLER's ctx (a client disconnect stops it early,
	// same as RunClaudeCompose) but is hard-bounded regardless.
	waitCtx, cancel := context.WithTimeout(ctx, siteConfigProbeWaitTimeout)
	defer cancel()
	finalState, werr := s.waitForRunTerminal(waitCtx, runID)
	elapsed := s.cfg.Now().Sub(start)
	if werr != nil {
		// Timed out, or the client left before the probe finished: reclaim the
		// run + its sandbox so a hung probe can never hold one open (same
		// precedent as reclaimComposeRun, composeresult.go). This is still a
		// real, definite answer ("no clean response within the budget"), not a
		// transport-level failure of the ENDPOINT -- report it as an incomplete
		// probe (classify* turns that into `blocked`), never a 5xx.
		s.reclaimProbeRun(context.WithoutCancel(ctx), runID)
		return runID, probeRunResult{
			incompleteReason: fmt.Sprintf("did not finish within %s", siteConfigProbeWaitTimeout),
			elapsed:          elapsed,
		}, nil
	}
	if finalState == types.RunCompleted {
		return runID, probeRunResult{hasExitCode: true, exitCode: 0, elapsed: elapsed}, nil
	}
	return runID, s.probeFailureDetail(ctx, runID, elapsed), nil
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
	if err != nil {
		res.incompleteReason = "could not read the probe run's own audit trail: " + err.Error()
		return res
	}
	for _, ev := range events {
		if ev.Action != "run.complete" {
			continue
		}
		var d struct {
			ExitCode int `json:"exit_code"`
		}
		if json.Unmarshal(ev.Data, &d) == nil {
			res.hasExitCode, res.exitCode = true, d.ExitCode
			return res
		}
	}
	for _, ev := range events {
		if ev.Outcome != "failure" {
			continue
		}
		var d struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(ev.Data, &d)
		if d.Error != "" {
			res.incompleteReason = fmt.Sprintf("%s: %s", ev.Action, d.Error)
		} else {
			res.incompleteReason = ev.Action + " did not succeed"
		}
		return res
	}
	res.incompleteReason = "the probe sandbox failed for an unreported reason"
	return res
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

// siteConfigProbeResponse is the shared {state, detail, elapsed_ms?} shape
// both test-proxy and test-redirect return, always HTTP 200 -- the STATE,
// never the transport, carries a probe's outcome (including no_runner).
type siteConfigProbeResponse struct {
	State     string `json:"state"`
	Detail    string `json:"detail"`
	ElapsedMS int64  `json:"elapsed_ms,omitempty"`
}

// noRunnerResponse is the honest, non-error answer for s.cfg.Runner == nil:
// there is nothing to launch a probe with. Always HTTP 200 (a state, not a
// transport failure).
var noRunnerResponse = siteConfigProbeResponse{State: "no_runner", Detail: "no runner configured, nothing to launch a probe with"}

// classifyProxyProbe turns what runSiteConfigProbe actually observed into the
// test-proxy endpoint's {state, detail}. host is what was probed, named in
// every detail message so "what did we actually try" is always answerable.
// viaProxy selects the wording only: the probe traverses whatever egress path
// the host actually has, so with no upstream configured this reports on direct
// reachability rather than claiming a proxy it never chained through.
func classifyProxyProbe(res probeRunResult, host string, viaProxy bool) siteConfigProbeResponse {
	path := "directly (no upstream proxy configured)"
	if viaProxy {
		path = "through wardyn-proxy (chained to the configured upstream)"
	}
	switch {
	case res.hasExitCode && res.exitCode == 0:
		return siteConfigProbeResponse{
			State:     "reached",
			Detail:    fmt.Sprintf("reached the public internet %s in %s (verified against %s)", path, res.elapsed.Round(time.Millisecond), host),
			ElapsedMS: res.elapsed.Milliseconds(),
		}
	case res.hasExitCode && res.exitCode == proxyProbeInterceptedCode:
		// The single most misleading corporate-network state, and the one an
		// exit-code-only probe scores as success: something replied, so the
		// connection worked, but it was not the endpoint we asked for.
		return siteConfigProbeResponse{
			State: "blocked",
			Detail: fmt.Sprintf("something answered %s but it was not the real endpoint — a captive portal or a corporate block page is intercepting. "+
				"Egress is not actually open, whatever the reply said", path),
			ElapsedMS: res.elapsed.Milliseconds(),
		}
	case res.hasExitCode:
		return siteConfigProbeResponse{
			State:     "blocked",
			Detail:    fmt.Sprintf("could not reach %s %s: %s", host, path, curlFailureDetail(res.exitCode)),
			ElapsedMS: res.elapsed.Milliseconds(),
		}
	default:
		return siteConfigProbeResponse{
			State:     "blocked",
			Detail:    fmt.Sprintf("the probe of %s did not get a clean answer: %s", host, res.incompleteReason),
			ElapsedMS: res.elapsed.Milliseconds(),
		}
	}
}

// classifyRedirectProbe turns what runSiteConfigProbe actually observed into
// the test-redirect endpoint's {state, detail}. See redirectProbeScript for
// exactly what ran and what each exit code means.
func classifyRedirectProbe(res probeRunResult, toHost, fromHost string) siteConfigProbeResponse {
	switch {
	case res.hasExitCode && res.exitCode == 0:
		return siteConfigProbeResponse{
			State: "reached",
			Detail: fmt.Sprintf("%s is reachable via the mirror; %s is correctly blocked when dialed directly (redirect enforced) — checked in %s",
				toHost, fromHost, res.elapsed.Round(time.Millisecond)),
			ElapsedMS: res.elapsed.Milliseconds(),
		}
	case res.hasExitCode && res.exitCode == redirectProbeBypassCode:
		return siteConfigProbeResponse{
			State: "bypass",
			Detail: fmt.Sprintf("%s is reachable via the mirror, but %s is ALSO still reachable directly from a sandbox — "+
				"the redirect is configured but not enforced; runs can still bypass the mirror", toHost, fromHost),
			ElapsedMS: res.elapsed.Milliseconds(),
		}
	case res.hasExitCode:
		return siteConfigProbeResponse{
			State:     "blocked",
			Detail:    fmt.Sprintf("could not reach the mirror %s: %s", toHost, curlFailureDetail(res.exitCode)),
			ElapsedMS: res.elapsed.Milliseconds(),
		}
	default:
		return siteConfigProbeResponse{
			State:     "blocked",
			Detail:    fmt.Sprintf("the probe of the mirror %s did not get a clean answer: %s", toHost, res.incompleteReason),
			ElapsedMS: res.elapsed.Milliseconds(),
		}
	}
}

// findEgressRedirect resolves the stored row whose From matches want
// (case-insensitive on the stored spelling -- same convention as
// unionAllowedDomains, helpers.go). This lookup IS the SSRF guard: it is the
// only path from a caller's request to an actual probe target, so a `from`
// the operator never configured can never be dialed.
func findEgressRedirect(sc types.SiteConfig, want string) (types.EgressRedirect, bool) {
	for _, red := range sc.EgressRedirects {
		if strings.EqualFold(red.From, want) {
			return red, true
		}
	}
	return types.EgressRedirect{}, false
}

// handleTestSiteConfigProxy is POST /api/v1/site-config/test-proxy
// (operator-only, audited). It launches a throwaway one-shot sandbox that
// curls a known-reachable host through wardyn-proxy chained to the
// configured upstream proxy, and reports what actually happened. Takes no
// request fields -- there is nothing here for a caller to redirect toward an
// arbitrary target (see the file doc comment for why that matters).
func (s *Server) handleTestSiteConfigProxy(w http.ResponseWriter, r *http.Request) {
	// No request fields. An empty body is the ORDINARY client shape — a fetch()
	// POST with nothing to send transmits no body at all — so EOF is success
	// here, not a 400. Anything actually sent still decodes strictly, so a
	// typo'd field can't be silently swallowed.
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxJSONBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&struct{}{}); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
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
	viaProxy := siteCfg.UpstreamProxyURL != "" || siteCfg.UpstreamProxySecretRef != ""
	if s.cfg.Runner == nil {
		writeJSON(w, http.StatusOK, noRunnerResponse)
		return
	}

	actor := principalFromRequest(r)
	runID, res, perr := s.runSiteConfigProbe(ctx, actor, proxyProbeScript,
		proxyProbeHosts, nil)
	if perr != nil {
		writeError(w, http.StatusInternalServerError, "launch proxy probe: "+perr.Error())
		return
	}
	resp := classifyProxyProbe(res, strings.Join(proxyProbeHosts, ", "), viaProxy)
	s.recordAudit(ctx, s.auditEvent(&runID, actorTypeFromRequest(r), actor, "site_config.test_proxy",
		"site_config", outcomeBool(resp.State == "reached"), mustJSON(map[string]any{
			"state": resp.State, "target_host": strings.Join(proxyProbeHosts, ", "), "elapsed_ms": resp.ElapsedMS,
		})))
	writeJSON(w, http.StatusOK, resp)
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

	toHost := workspacescan.HostOf(red.To)
	fromHost := workspacescan.HostOf(red.From)
	actor := principalFromRequest(r)
	runID, res, perr := s.runSiteConfigProbe(ctx, actor, redirectProbeScript,
		[]string{toHost}, map[string]string{
			"WARDYN_PROBE_TO_URL":   probeTargetURL(red.To),
			"WARDYN_PROBE_FROM_URL": probeTargetURL(red.From),
		})
	if perr != nil {
		writeError(w, http.StatusInternalServerError, "launch redirect probe: "+perr.Error())
		return
	}
	resp := classifyRedirectProbe(res, toHost, fromHost)
	s.recordAudit(ctx, s.auditEvent(&runID, actorTypeFromRequest(r), actor, "site_config.test_redirect",
		"site_config", outcomeBool(resp.State == "reached"), mustJSON(map[string]any{
			"state": resp.State, "to_host": toHost, "from_host": fromHost, "elapsed_ms": resp.ElapsedMS,
		})))
	writeJSON(w, http.StatusOK, resp)
}
