// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Wardyn's own check of a service's port, for a spec that opted in (service.health.probe: control_plane). The
// health sweep runs it after the state-file read, through the same exec path the UI gateway relays over
// (`socat - TCP:127.0.0.1:<port>`), so it reaches the port from inside the sandbox, never from its network. With a
// health path it sends one GET with no credentials and follows no redirect; without one it is a TCP connect. The
// answer is the sandbox's bytes: bounded, and judged on a strictly parsed status line alone.
const (
	// serviceCheckTimeout bounds one check, as the supervisor's own check is bounded.
	serviceCheckTimeout = 5 * time.Second
	// serviceCheckMaxBytes bounds what is read of an answer looking for its status line.
	serviceCheckMaxBytes = 4096
	// serviceCheckFailuresMax clamps the consecutive count.
	serviceCheckFailuresMax = 1_000_000
)

// The findings a check records (types.RunServiceProbe.Reason). A pass records none.
const (
	serviceCheckRefused   = "refused"    // nothing accepted the connection
	serviceCheckTimedOut  = "timeout"    // no answer within serviceCheckTimeout
	serviceCheckBadStatus = "bad_status" // an HTTP status outside 2xx and 3xx
	serviceCheckBadAnswer = "bad_answer" // not an HTTP status line, or none within serviceCheckMaxBytes
	serviceCheckNotRun    = "not_run"    // the check could not run at all
)

// serviceStatusLineRE is a status line read strictly: HTTP/1.0 or 1.1, a three-digit code, an optional printable
// reason phrase, then the line's end.
var serviceStatusLineRE = regexp.MustCompile(`^HTTP/1\.[01] ([1-5][0-9][0-9])(?: [\t\x20-\x7e]*)?\r?\n`)

// serviceCheck is what one check found: a finding, "" for a pass, and the status code when it read one.
type serviceCheck struct {
	finding string
	status  int
}

// serviceCheckArgv is the one command a check runs in the sandbox: socat, with fixed arguments. A health path
// relays a request to the port; without one, socat connects, sends nothing and exits 0 on a connection. A
// service rides a container sandbox only (serviceRefusal), so socat is the relay.
func serviceCheckArgv(sv *types.RunService) []string {
	target := fmt.Sprintf("TCP:127.0.0.1:%d", sv.Port)
	if sv.Health.Path == "" {
		return []string{"socat", "-u", "/dev/null", target}
	}
	return []string{"socat", "-", target}
}

// serviceCheckRequest is the request a check with a health path sends: a GET of the path, no credentials, and
// the connection closed after the answer. The path passed serviceHealthPathRE, so it holds no space or line break.
func serviceCheckRequest(sv *types.RunService) []byte {
	if sv.Health.Path == "" {
		return nil
	}
	return fmt.Appendf(nil, "GET %s HTTP/1.0\r\nHost: 127.0.0.1:%d\r\nUser-Agent: wardyn-health\r\nConnection: close\r\n\r\n",
		sv.Health.Path, sv.Port)
}

// checkServicePort runs the check once on the readServiceState pattern (exec, read, close), bounded by ctx.
func (s *Server) checkServicePort(ctx context.Context, run types.AgentRun) serviceCheck {
	sv := run.Service
	sess, err := s.cfg.Runner.ExecStream(ctx, run.SandboxRef, runner.ExecSpec{Argv: serviceCheckArgv(sv)})
	if sess != nil && sess.Close != nil {
		defer func() { _ = sess.Close() }() // tears down only this exec, never the sandbox
	}
	if err != nil || sess == nil || sess.Stdout == nil {
		return serviceCheck{finding: serviceCheckNotRun}
	}
	if sess.Stderr != nil {
		go func() { _, _ = io.Copy(io.Discard, sess.Stderr) }()
	}
	done := make(chan serviceCheck, 1)
	go func() { done <- serviceCheckAnswer(sess, serviceCheckRequest(sv)) }()
	select {
	case c := <-done:
		return c
	case <-ctx.Done():
		return serviceCheck{finding: serviceCheckTimedOut}
	}
}

// serviceCheckAnswer sends req (nil for a TCP connect) and judges what comes back. stdin is left open: socat
// ends the relay soon after its input closes, which could cut a slow answer short. The exec is closed by the caller.
func serviceCheckAnswer(sess *runner.ExecSession, req []byte) serviceCheck {
	if req != nil {
		if sess.Stdin == nil {
			return serviceCheck{finding: serviceCheckNotRun}
		}
		// A write the relay refuses is judged by what the exec says next: socat that could not connect exits.
		_, _ = sess.Stdin.Write(req)
	}
	out := io.LimitReader(sess.Stdout, serviceCheckMaxBytes)
	buf, chunk := make([]byte, 0, 512), make([]byte, 512)
	for req == nil || bytes.IndexByte(buf, '\n') < 0 {
		n, err := out.Read(chunk)
		buf = append(buf, chunk[:n]...)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return serviceCheck{finding: serviceCheckNotRun}
		}
	}
	switch {
	case req != nil && bytes.IndexByte(buf, '\n') >= 0:
		return judgeServiceStatusLine(buf)
	case len(buf) > 0:
		// No line within the bound, or a line cut off by the exec's end; a TCP connect writes nothing back.
		return serviceCheck{finding: serviceCheckBadAnswer}
	}
	code := -1
	if sess.Wait != nil {
		if c, err := sess.Wait(); err == nil {
			code = c
		}
	}
	switch {
	case code == 0 && req == nil:
		return serviceCheck{} // the port accepted the connection
	case code == 0:
		return serviceCheck{finding: serviceCheckBadAnswer} // accepted, then closed with no answer
	case code == 126 || code == 127 || code < 0:
		return serviceCheck{finding: serviceCheckNotRun} // no socat in the image, or no exit status
	}
	return serviceCheck{finding: serviceCheckRefused}
}

// judgeServiceStatusLine reads the first line of an HTTP answer: 2xx and 3xx pass (the supervisor's rule), any
// other status is bad_status, and a line that is not a status line is bad_answer.
func judgeServiceStatusLine(raw []byte) serviceCheck {
	m := serviceStatusLineRE.FindSubmatch(raw)
	if m == nil {
		return serviceCheck{finding: serviceCheckBadAnswer}
	}
	status, _ := strconv.Atoi(string(m[1])) //nolint:errcheck // three digits by the pattern
	if status >= 200 && status < 400 {
		return serviceCheck{status: status}
	}
	return serviceCheck{finding: serviceCheckBadStatus, status: status}
}

// serviceProbeObservation is the probe reading wardynd records after a check (c, nil when none ran this pass),
// from the last one (prev, nil before the first). Failures count Wardyn's own checks: a pass resets the count; a
// failure adds one and reads unhealthy only at threshold, keeping the last health below it (starting before any);
// a check that could not run reads unknown/not_run, never unhealthy, and counts nothing. With no check this pass
// the last reading is kept as it was, observed_at included.
func serviceProbeObservation(prev *types.RunServiceProbe, c *serviceCheck, threshold int, now time.Time) *types.RunServiceProbe {
	if c == nil {
		if prev == nil {
			return nil
		}
		kept := *prev
		return &kept
	}
	health, failures := types.ServiceHealthStarting, 0
	if prev != nil {
		health, failures = prev.Health, prev.Failures
	}
	next := &types.RunServiceProbe{HTTPStatus: c.status, ObservedAt: now}
	switch c.finding {
	case "":
		next.Health = types.ServiceHealthHealthy
	case serviceCheckNotRun:
		next.Health, next.Reason, next.Failures = types.ServiceHealthUnknown, c.finding, failures
	default:
		next.Reason, next.Failures = c.finding, min(failures+1, serviceCheckFailuresMax)
		next.Health = health
		if next.Failures >= max(threshold, 1) {
			next.Health = types.ServiceHealthUnhealthy
		}
	}
	return next
}

// observeServiceProbe runs the check for a service that opted in and returns its reading. A pass whose context
// is done (the sweep's budget, or a leader's term) runs no check, and a check that ran past it is not counted:
// a cut-short pass is no finding about the service.
func (s *Server) observeServiceProbe(ctx context.Context, run types.AgentRun) *serviceCheck {
	if ctx.Err() != nil {
		return nil
	}
	cctx, cancel := context.WithTimeout(ctx, serviceCheckTimeout)
	c := s.checkServicePort(cctx, run)
	cancel()
	if ctx.Err() != nil {
		return nil
	}
	return &c
}

// sameServiceProbe compares everything but observed_at.
func sameServiceProbe(a, b *types.RunServiceProbe) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Health == b.Health && a.Reason == b.Reason && a.HTTPStatus == b.HTTPStatus && a.Failures == b.Failures
}

// probeRenewalHealth is wardynd's own check as renewal reads it, for a service that opted in: the last reading
// while it is fresh (serviceStatusStale); with none yet, starting inside serviceSupervisorGrace from the run's
// start; unknown otherwise, so a missing or stale reading never renews.
func probeRenewalHealth(run types.AgentRun, now time.Time) types.ServiceHealthState {
	if st := run.ServiceStatus; st != nil && st.Probe != nil {
		if now.Sub(st.Probe.ObservedAt) > serviceStatusStale {
			return types.ServiceHealthUnknown
		}
		return st.Probe.Health
	}
	since := run.CreatedAt
	if run.AdmittedAt != nil {
		since = *run.AdmittedAt
	}
	return missingServiceReading(since, now)
}

// missingServiceReading is a check reading not yet made, for a run started (admitted, else created) at since: starting
// inside serviceSupervisorGrace, unknown after it.
func missingServiceReading(since, now time.Time) types.ServiceHealthState {
	if now.Sub(since) < serviceSupervisorGrace {
		return types.ServiceHealthStarting
	}
	return types.ServiceHealthUnknown
}

// renewableServiceHealth is whether a service's health lets renewal go on: renewalHealth starting or healthy, and
// for a service that opted in to Wardyn's check, probeRenewalHealth starting or healthy too. Without the check it
// is exactly renewalHealth's rule.
func renewableServiceHealth(run types.AgentRun, now time.Time) bool {
	renews := func(h types.ServiceHealthState) bool {
		return h == types.ServiceHealthStarting || h == types.ServiceHealthHealthy
	}
	if !renews(renewalHealth(run, now)) {
		return false
	}
	return !run.Service.ControlPlaneProbe() || renews(probeRenewalHealth(run, now))
}

// viewServiceHealth is the health a workload shows for its run's status st: the sandbox's reading, or for a spec that
// opted in to Wardyn's own check the worse of it and that check, a check with no reading yet counted as renewal counts
// it (starting inside the supervisor's grace, unknown after). Without the check it is st.Health.
func viewServiceHealth(v store.WorkloadView, st *types.RunServiceStatus, now time.Time) types.ServiceHealthState {
	h := st.CombinedHealth()
	if v.ServiceProbeOn && st.Probe == nil {
		since := now
		if v.RunStartedAt != nil {
			since = *v.RunStartedAt
		}
		h = types.WorseServiceHealth(h, missingServiceReading(since, now))
	}
	return h
}
