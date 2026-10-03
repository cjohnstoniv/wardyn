// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/sweephealth"
)

// substrate_health.go reports a broken substrate or a stalled background sweep
// on two surfaces, a gauge and a /setup/status row, and on neither of them
// touches /readyz. /readyz stays store-only on purpose: the chart's
// readinessProbe reads it, and teaching it about the substrate would pull every
// replica out of the Service on a substrate fault, and with them the console,
// the one diagnostic surface the operator has.

// substrateProbeTTL is how long one probe answer is served. The runner's
// substrate changes state on the scale of a rotated token or a deleted binding,
// not of a scrape, so a few seconds collapses every concurrent /metrics and
// /setup/status read into one substrate call per replica.
const substrateProbeTTL = 5 * time.Second

// substrateProbeCache is one replica's cached, single-flight substrate probe.
//
// Every caller waits at most storePingTimeout: a read inside the TTL gets the
// cached answer, and one past it waits on the single probe in flight, which has
// that same deadline. A probe that ignores its context (a wedged client) is
// judged unreachable at the deadline and not started again until it returns, so
// a black-holed substrate costs one stuck goroutine, never one per scrape.
type substrateProbeCache struct {
	mu      sync.Mutex
	state   runner.SubstrateState
	at      time.Time
	have    bool
	flight  chan struct{} // non-nil while a probe the callers share is in progress
	running bool          // a probe call has not returned yet; it may outlive its deadline
}

func (c *substrateProbeCache) get(ctx context.Context, now func() time.Time, probe func(context.Context) runner.SubstrateState) runner.SubstrateState {
	c.mu.Lock()
	if c.have && now().Sub(c.at) < substrateProbeTTL {
		st := c.state
		c.mu.Unlock()
		return st
	}
	flight := c.flight
	if flight == nil {
		if c.running {
			c.mu.Unlock()
			return runner.SubstrateUnreachable
		}
		flight = make(chan struct{})
		c.flight, c.running = flight, true
		go c.run(flight, now, probe)
	}
	c.mu.Unlock()
	select {
	case <-flight:
	case <-ctx.Done():
		return runner.SubstrateUnreachable
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state
}

// run is the one probe in flight. It owns its deadline, so a caller that goes
// away does not cancel the answer the others are waiting for.
func (c *substrateProbeCache) run(flight chan struct{}, now func() time.Time, probe func(context.Context) runner.SubstrateState) {
	pctx, cancel := context.WithTimeout(context.Background(), storePingTimeout)
	defer cancel()
	res := make(chan runner.SubstrateState, 1)
	go func() {
		st := runner.SubstrateUnreachable // a panic in the probe reads as unreachable
		defer func() {
			_ = recover()
			c.mu.Lock()
			c.running = false
			c.mu.Unlock()
			res <- st
		}()
		st = probe(pctx)
	}()
	st := runner.SubstrateUnreachable
	select {
	case st = <-res:
	case <-pctx.Done():
	}
	c.mu.Lock()
	c.state, c.at, c.have, c.flight = st, now(), true, nil
	c.mu.Unlock()
	close(flight)
}

// runnerSubstrateState is this replica's classified view of the runner's
// substrate. ok is false when no runner is configured: there is no substrate to
// be down, which the existing runner row already says.
func (s *Server) runnerSubstrateState(ctx context.Context) (state runner.SubstrateState, ok bool) {
	rn := s.cfg.Runner
	if rn == nil {
		return "", false
	}
	return s.substrateProbe.get(ctx, s.cfg.Now, func(pctx context.Context) runner.SubstrateState {
		if sp, ok := rn.(runner.SubstrateProber); ok {
			if st := sp.ProbeSubstrate(pctx); st != runner.SubstrateOK {
				return st
			}
		}
		// A substrate that answers its probe but cannot report its capabilities
		// is not usable either, and it is the failure the runner row points here for.
		if _, err := rn.Capabilities(pctx); err != nil {
			return runner.SubstrateUnreachable
		}
		return runner.SubstrateOK
	}), true
}

// Setup-row causes. A machine key, never prose.
const (
	causeRunnerUnreachable = "runner_unreachable"
	causeRunnerAuth        = "runner_auth"
	causeSweepStale        = "sweep_stale"
)

// substrateHealthCheck grades the substrate_health row, or reports ok=false when
// there is nothing to grade (no runner and no registered sweep). It never sets
// Blocking: a substrate outage must leave the console open, because the console
// is where the operator reads this row. Its detail is the classified state and
// the names of stale sweeps, never the text of a substrate error, which can carry
// API-server wording this row was never meant to publish.
func substrateHealthCheck(driver string, state runner.SubstrateState, haveRunner bool, sweeps []sweephealth.Status) (SetupCheck, bool) {
	var stale []string
	for _, sw := range sweeps {
		if sw.Stale {
			stale = append(stale, sw.Name)
		}
	}
	if !haveRunner && len(sweeps) == 0 {
		return SetupCheck{}, false
	}
	chk := SetupCheck{ID: "substrate_health", Label: "Substrate and sweep health", Status: "ok",
		Detail: "The sandbox runner answers and every background sweep is running."}
	if !haveRunner {
		chk.Detail = "Every background sweep is running."
	}
	if len(sweeps) == 0 {
		chk.Detail = "The sandbox runner answers."
	}
	if len(stale) > 0 {
		chk.Status, chk.Cause = "warn", causeSweepStale
		chk.Detail = fmt.Sprintf("These background sweeps have not finished in %d of their intervals: %s.", sweephealth.StaleAfterIntervals, strings.Join(stale, ", "))
		chk.Fix = "Read the wardynd log for the sweep's error and check the database. If every replica is running, one of them should hold the sweeper lease."
	}
	switch state {
	case runner.SubstrateUnreachable:
		chk.Status, chk.Cause = "fail", causeRunnerUnreachable
		chk.Detail = "The control plane cannot reach the sandbox runner's substrate, so runs cannot launch or be controlled." + staleSuffix(stale)
		chk.Fix = "Check that the substrate answers from this replica: the Docker daemon is running and its socket is mounted, or the Kubernetes API server is reachable from the wardynd pod. /readyz is unaffected."
	case runner.SubstrateUnauthorized, runner.SubstrateForbidden:
		chk.Status, chk.Cause = "fail", causeRunnerAuth
		chk.Detail = fmt.Sprintf("The sandbox runner's substrate refused the control plane (%s), so runs cannot launch or be controlled.", state) + staleSuffix(stale)
		chk.Fix = "Docker: check that wardynd's user may use the Docker socket. Kubernetes: check the wardynd ServiceAccount token, and that the runner Role and RoleBinding exist in the runs namespace. /readyz is unaffected."
		if driver == "k8s" {
			chk.Fix = "Check the wardynd ServiceAccount token, and that the runner Role and RoleBinding exist in the runs namespace. /readyz is unaffected."
		}
	}
	return chk, true
}

func staleSuffix(stale []string) string {
	if len(stale) == 0 {
		return ""
	}
	return " Sweeps not finished in time: " + strings.Join(stale, ", ") + "."
}

// substrateHealthRow reads this replica's probe and the shared sweep record and
// grades the row. Operator callers only: a member's Checks are discarded, so the
// caller does not ask for a row a member would never see (and so a member's poll
// never triggers or waits on a substrate call).
func (s *Server) substrateHealthRow(ctx context.Context) (SetupCheck, bool) {
	state, haveRunner := s.runnerSubstrateState(ctx)
	sweeps := s.sweepStatuses(ctx)
	driver := ""
	if haveRunner {
		driver = s.cfg.Runner.Name()
	}
	return substrateHealthCheck(driver, state, haveRunner, sweeps)
}

// sweepStatuses reads the shared sweep record. A record that cannot be read is
// "cannot tell": no sweep is reported, and none is called stale.
func (s *Server) sweepStatuses(ctx context.Context) []sweephealth.Status {
	st, err := s.cfg.SweepHealth.Status(ctx)
	if err != nil {
		return nil
	}
	return st
}

// writeSubstrateGauges appends wardyn_runner_up and the sweep tick series.
// wardyn_runner_up is per replica (each replica probes for itself) and absent
// with no runner. The sweep series come from the shared record, so a follower
// reports the leader's ticks, and a sweep with no tick yet has no sample.
func (s *Server) writeSubstrateGauges(ctx context.Context, w io.Writer) {
	if state, ok := s.runnerSubstrateState(ctx); ok {
		up := 0
		if state == runner.SubstrateOK {
			up = 1
		}
		fmt.Fprintf(w, "# HELP wardyn_runner_up 1 when this replica's last probe of the sandbox runner's substrate succeeded, 0 when it did not (unreachable, unauthorized or forbidden; the /setup/status substrate_health row names which). Each replica probes for itself. Not part of /readyz.\n"+
			"# TYPE wardyn_runner_up gauge\nwardyn_runner_up %d\n", up)
	}
	sweeps := s.sweepStatuses(ctx)
	if len(sweeps) == 0 {
		return
	}
	fmt.Fprint(w, "# HELP wardyn_sweep_last_tick_seconds Unix time a background sweep last started a tick (result=\"attempt\") and last finished one without error (result=\"success\"). "+
		"Shared by every replica. A sweep that has not ticked yet has no sample. Alert on time() minus the success series exceeding the sweep's interval (docs/operations/monitoring.md).\n"+
		"# TYPE wardyn_sweep_last_tick_seconds gauge\n")
	for _, sw := range sweeps {
		if !sw.AttemptedAt.IsZero() {
			fmt.Fprintf(w, "wardyn_sweep_last_tick_seconds{sweep=%q,result=\"attempt\"} %d\n", sw.Name, sw.AttemptedAt.Unix())
		}
		if !sw.SucceededAt.IsZero() {
			fmt.Fprintf(w, "wardyn_sweep_last_tick_seconds{sweep=%q,result=\"success\"} %d\n", sw.Name, sw.SucceededAt.Unix())
		}
	}
}

// HealthSweeps returns the background sweeps this package owns whose start
// condition holds on this install: the run watcher needs a runner, and the
// orphaned build sweep needs an image builder that can sweep. cmd/wardynd
// registers them with its own, on every replica, whether or not it holds the
// sweeper lock.
func (s *Server) HealthSweeps() []sweephealth.Sweep {
	var out []sweephealth.Sweep
	if s.cfg.Runner != nil {
		out = append(out, sweephealth.Sweep{Name: sweephealth.RunWatcher, Interval: watcherSweepInterval})
	}
	if _, ok := s.cfg.ImageBuilder.(ImageBuildSweeper); ok {
		out = append(out, sweephealth.Sweep{Name: sweephealth.OrphanedBuild, Interval: buildSweepInterval})
	}
	return out
}
