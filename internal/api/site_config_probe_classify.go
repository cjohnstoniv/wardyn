// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Split from site_config_probe.go by seam (file-size gate): this file holds
// the probe VERDICT surface -- the shared response shape, the two classify*
// state-mapping functions, and the post-classify lost-recording warning
// check. site_config_probe.go keeps the probe LAUNCH/OBSERVE side (the
// scripts, runSiteConfigProbe, probeFailureDetail) and the two HTTP
// handlers, which call into this file's classify* and probeRecordingWarning.

package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/recording"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// siteConfigProbeResponse is the shared {state, detail, elapsed_ms?} shape
// both test-proxy and test-redirect return, always HTTP 200 -- the STATE,
// never the transport, carries a probe's outcome (including no_runner and
// not_run -- the sandbox that would have carried the probe never got to
// running it, so nothing was learned about the network at all). States:
//   - reached / blocked / bypass (test-redirect only): see classifyProxyProbe
//     / classifyRedirectProbe.
//   - no_runner / not_run: nothing was learned about the network either way
//     (no runner to launch a probe with, or the sandbox never got to
//     running it).
//   - timed_out: the sandbox DID start and the task DID launch (run.exec
//     succeeded), but the run itself never reported completion within the
//     wait budget -- provably not a network verdict. The usual cause is the
//     recorder's own upload tail hanging (cmd/wardyn-rec/main.go) against an
//     unreachable control plane, most often on Kubernetes where the proxy
//     pod -> WARDYN_CONTROL_PLANE_URL hop can be dropped by a cluster-wide
//     default-deny even with the ambient-deny ack in place. Distinct from
//     blocked (blocked means the probe DID complete and observed a real
//     network fact) and from not_run (not_run means the task never even
//     launched).
//
// The last four fields are test-proxy qualifiers the UI renders distinct
// treatments from (the mock's ok/okdirect/okcustom/intercepted kinds) --
// machine-readable so no client ever has to string-match a detail sentence:
//   - via: which path the probe actually traversed ("proxy" | "direct").
//   - intercepted: state=blocked's captive-portal flavor -- something
//     ANSWERED, but not with the endpoint's published payload. Same verdict
//     as blocked (it is NOT reachability), rendered apart because it sends
//     the operator to a different person than a refused connection does.
//   - custom: the probe hit a caller-named URL with no known payload to
//     verify, so a reached here is the deliberately WEAKER "request
//     completed" claim, never the builtin targets' "payloads matched".
//   - warning: set alongside a `reached` verdict when the probe's OWN
//     recording never reached the control plane (RecordingStore.OpenCast ->
//     recording.ErrNotFound) even though egress itself worked -- see the
//     handlers' post-classify check. A `reached` probe with no Warning still
//     says nothing about whether recordings for OTHER, non-probe runs
//     arrive; it only proves this probe run's own recording did.
type siteConfigProbeResponse struct {
	State       string `json:"state"`
	Detail      string `json:"detail"`
	ElapsedMS   int64  `json:"elapsed_ms,omitempty"`
	Via         string `json:"via,omitempty"`
	Intercepted bool   `json:"intercepted,omitempty"`
	Custom      bool   `json:"custom,omitempty"`
	Warning     string `json:"warning,omitempty"`
}

// noRunnerResponse is the honest, non-error answer for s.cfg.Runner == nil:
// there is nothing to launch a probe with. Always HTTP 200 (a state, not a
// transport failure).
var noRunnerResponse = siteConfigProbeResponse{State: "no_runner", Detail: "no runner configured, nothing to launch a probe with"}

// errProbeNoRunner is runSiteConfigProbe's own sentinel for "a runner IS
// configured, but it cannot describe itself, or it describes itself as
// advertising no confinement class at all" -- the same honest no_runner
// state noRunnerResponse reports for an absent Runner, just discovered one
// layer deeper (after a Capabilities() call, before any sandbox is
// attempted). Both handlers check for it with errors.Is before their generic
// 500 path.
var errProbeNoRunner = errors.New("the configured runner declares no usable confinement class, nothing to launch a probe with")

// proxyProbeSubject is what classifyProxyProbe words its verdicts about:
// endpoints for the messages that make a payload claim (reached/intercepted),
// hosts for connection-level failures (the path never got a say in a refused
// connection), upstream naming the chain hop the probe traversed ("" = the
// probe went direct). upstream is always DISPLAY-safe: the plain configured
// URL (validateSiteConfig rejects userinfo in it) or the secret's NAME --
// never a resolved secret value (see the NeverLogsCredentialedUpstreamURL
// test).
type proxyProbeSubject struct {
	endpoints string
	hosts     string
	upstream  string
	custom    bool
	// resolveFailReason is resolveUpstreamProxyURL's failReason (runs_bedrock.go)
	// when something WAS configured (a URL or a secret ref) but did not resolve
	// to a usable proxy — "" both when nothing is configured and when it
	// resolved fine. upstream stays "" in the fail case (never claim a hop
	// dispatch would drop), so classify uses this to say WHY the probe went
	// direct instead of silently reading like an unconfigured proxy.
	resolveFailReason string
}

// via is the wire spelling of which path the probe traversed.
func (p proxyProbeSubject) via() string {
	if p.upstream != "" {
		return "proxy"
	}
	return "direct"
}

// pathClause is the shared "through what" suffix wording ("through
// wardyn-proxy chained to X" / "directly").
func (p proxyProbeSubject) pathClause() string {
	if p.upstream != "" {
		return "through wardyn-proxy chained to " + p.upstream
	}
	return "directly"
}

// timedOutDetail words the shared state=timed_out detail both classify*
// functions use (see probeRunResult.timedOut's doc for what the state
// means). agentStatus is probeAgentStatusAtDeadline's best-effort read
// ("unknown" when it could not be determined); controlPlaneURL is
// s.cfg.ControlPlaneURL, unmasked (it is operator-configured, never a
// secret).
func timedOutDetail(agentStatus, controlPlaneURL string) string {
	return fmt.Sprintf(
		"The probe sandbox started and ran, but the run never reported completion within %s — not a network verdict. "+
			"Sandbox agent status at the deadline: %s. The usual cause on Kubernetes is the run's recording upload to the "+
			"control plane (via the proxy pod) hanging: check WARDYN_CONTROL_PLANE_URL (%s) is reachable from the runs namespace.",
		siteConfigProbeWaitTimeout, agentStatus, controlPlaneURL)
}

// classifyProxyProbe turns what runSiteConfigProbe actually observed into the
// test-proxy endpoint's {state, detail}, in the mock's own detail shapes
// (T.TEST_OK / TEST_OK_DIRECT / TEST_BLOCKED / TEST_INTERCEPTED /
// TEST_OK_CUSTOM -- wardyn-integrations.js). Every message still names what
// was actually probed, and a custom target's reached is worded as the WEAKER
// claim it is: "the request completed", never "payloads matched" -- the UI
// adds its own caveat line (T.CUSTOM_CAVEAT), and this sentence stays honest
// for any API/CLI consumer that never renders that line.
//
// controlPlaneURL is s.cfg.ControlPlaneURL, named in the timed_out detail
// only (the value an operator actually needs to go check).
func classifyProxyProbe(res probeRunResult, subj proxyProbeSubject, controlPlaneURL string) siteConfigProbeResponse {
	resp := siteConfigProbeResponse{
		ElapsedMS: res.elapsed.Milliseconds(),
		Via:       subj.via(),
		Custom:    subj.custom,
	}
	elapsed := res.elapsed.Round(time.Millisecond)
	switch {
	case res.hasExitCode && res.exitCode == 0:
		resp.State = "reached"
		switch {
		case subj.custom:
			resp.Detail = fmt.Sprintf("The request to %s completed %s in %s.", subj.endpoints, subj.pathClause(), elapsed)
		case subj.upstream != "":
			resp.Detail = fmt.Sprintf("Reached %s through wardyn-proxy chained to %s in %s — payloads matched, the full chain a run takes.",
				subj.endpoints, subj.upstream, elapsed)
		case subj.resolveFailReason != "":
			// A proxy WAS configured but did not resolve to something dispatch can
			// use — the probe went direct exactly like a real run would (W13-S1-4 /
			// W12-W12-C-2), and must say so rather than reading like an
			// unconfigured proxy (the branch below).
			resp.Detail = fmt.Sprintf("Reached %s directly in %s — payloads matched, but the configured upstream proxy was NOT used: %s. A run would go direct too, not through the chain you configured.",
				subj.endpoints, elapsed, upstreamFailDetail(subj.resolveFailReason))
		default:
			resp.Detail = fmt.Sprintf("Reached %s directly in %s — payloads matched. No proxy is configured and none was needed; sandboxes on this host go straight out.",
				subj.endpoints, elapsed)
		}
	case res.hasExitCode && res.exitCode == proxyProbeInterceptedCode:
		// The single most misleading corporate-network state, and the one an
		// exit-code-only probe scores as success: something replied, so the
		// connection worked, but it was not the endpoint we asked for.
		resp.State = "blocked"
		resp.Intercepted = true
		resp.Detail = fmt.Sprintf("Something answered at %s, but the payload wasn't the published one — a captive portal or a corporate block page is intercepting. "+
			"Egress is not actually open, whatever the reply said.", subj.endpoints)
	case res.hasExitCode:
		resp.State = "blocked"
		what := "either endpoint (" + subj.hosts + ")"
		if subj.custom {
			what = subj.endpoints
		}
		resp.Detail = fmt.Sprintf("Could not reach %s: %s — probed %s.", what, curlFailureDetail(res.exitCode), subj.pathClause())
	case res.timedOut:
		resp.State = "timed_out"
		resp.Detail = timedOutDetail(res.agentStatus, controlPlaneURL)
	case res.neverRan:
		// The sandbox that carries the probe never got to running it (a launch
		// failure, e.g. an image pull or a confinement class this host can't
		// enforce) -- distinct from blocked, which means the probe DID run and
		// observed a real network fact. Nothing was learned about the network
		// either way, so this must never render as a proxy problem.
		resp.State = "not_run"
		resp.Detail = fmt.Sprintf("The probe never ran: %s. Nothing was learned about %s — the sandbox that carries the probe could not start, so this says nothing about your proxy or your network.",
			res.incompleteReason, subj.hosts)
	default:
		resp.State = "blocked"
		resp.Detail = fmt.Sprintf("The probe of %s did not get a clean answer: %s", subj.hosts, res.incompleteReason)
	}
	return resp
}

// classifyRedirectProbe turns what runSiteConfigProbe actually observed into
// the test-redirect endpoint's {state, detail}. See redirectProbeScript for
// exactly what ran and what each exit code means. controlPlaneURL mirrors
// classifyProxyProbe's own parameter -- see its doc.
func classifyRedirectProbe(res probeRunResult, toHost, fromHost, controlPlaneURL string) siteConfigProbeResponse {
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
	case res.timedOut:
		return siteConfigProbeResponse{
			State:     "timed_out",
			Detail:    timedOutDetail(res.agentStatus, controlPlaneURL),
			ElapsedMS: res.elapsed.Milliseconds(),
		}
	case res.neverRan:
		return siteConfigProbeResponse{
			State: "not_run",
			Detail: fmt.Sprintf("The probe never ran: %s. Nothing was learned about %s — the sandbox that carries the probe could not start, so this says nothing about your proxy or your network.",
				res.incompleteReason, toHost),
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

// probeRecordingWarning checks whether the probe run just classified as
// `reached` (state == "reached") got its OWN session recording safely to the
// control plane, and returns a warning line when it did not -- the same
// "egress works, but recordings are silently lost" defect the probe timeout
// above is a more severe expression of, since a healthy-looking `reached`
// verdict is exactly the case an operator would otherwise never think to
// check. No-op (returns "") for any other state, when no RecordingStore is
// configured, or when the runner does not advertise SessionRecording at all
// -- and best-effort on the Capabilities/OpenCast calls themselves: a check
// failure here must never turn an already-classified probe response into an
// error.
func (s *Server) probeRecordingWarning(ctx context.Context, state string, runID uuid.UUID) string {
	if state != "reached" || s.cfg.RecordingStore == nil || s.cfg.Runner == nil {
		return ""
	}
	caps, err := s.cfg.Runner.Capabilities(ctx)
	if err != nil || !caps.SessionRecording {
		return ""
	}
	rc, err := s.cfg.RecordingStore.OpenCast(ctx, runID.String())
	if err == nil {
		_ = rc.Close()
		return ""
	}
	if !errors.Is(err, recording.ErrNotFound) {
		slog.DebugContext(ctx, "wardynd: probe recording check failed", slog.Any("err", err))
		return ""
	}
	return fmt.Sprintf("Egress works, but this run's session recording never reached the control plane — runs will "+
		"complete and their recordings will be lost. Check WARDYN_CONTROL_PLANE_URL (%s) is reachable from the runs "+
		"namespace (the proxy pod uploads recordings there).", s.cfg.ControlPlaneURL)
}
