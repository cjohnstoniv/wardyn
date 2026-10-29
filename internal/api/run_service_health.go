// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The service health mirror: each tick wardynd reads the state file a service run's supervisor keeps in
// its sandbox and stores what it found on the run (agent_runs.service_status). The file is the
// sandbox's word, so it is decoded strictly and read as a hint: nothing here grants the run life.
const (
	// serviceStatePath is where the in-sandbox supervisor writes its state, one line of JSON.
	serviceStatePath = "/home/agent/.wardyn/service.json"
	// serviceStateMaxBytes bounds one read; the real file is under 100 bytes.
	serviceStateMaxBytes = 4096
	// serviceProbeTimeout bounds one run's read; serviceSweepBudget bounds a whole pass, and the runs
	// it did not reach wait for the next tick.
	serviceProbeTimeout = 5 * time.Second
	serviceSweepBudget  = 30 * time.Second
	serviceProbeWorkers = 8
	// serviceSupervisorGrace is how long a missing state file reads as a service still starting; after
	// it the file is missing for good (an image with no supervisor).
	serviceSupervisorGrace = 10 * time.Minute
	// serviceStatusRefresh is how often an unchanged status is written anyway, so observed_at says
	// when the sandbox was last heard from.
	serviceStatusRefresh = 5 * time.Minute
	// Sandbox-supplied ints are clamped to these.
	serviceRestartsMax = 1_000_000
	serviceExitMax     = 255
)

// serviceStateArgv is the one command run in the sandbox: fixed, no env, no TTY, as the agent user.
var serviceStateArgv = []string{"cat", serviceStatePath}

// serviceStateFile is the supervisor's service.json, version 1.
type serviceStateFile struct {
	V            int    `json:"v"`
	Health       string `json:"health"`
	Reason       string `json:"reason"`
	Restarts     int    `json:"restarts"`
	LastExitCode *int   `json:"last_exit_code"`
}

// decodeServiceState reads the file strictly: one JSON object of exactly the known keys, version 1, a
// health the supervisor can write, a reason it can write. Anything else is an error, and the caller
// counts the file unreadable. The ints are clamped, not refused.
func decodeServiceState(raw []byte) (serviceStateFile, error) {
	var f serviceStateFile
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return serviceStateFile{}, err
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return serviceStateFile{}, errors.New("service state: trailing data")
	}
	if f.V != 1 {
		return serviceStateFile{}, errors.New("service state: unknown version")
	}
	switch types.ServiceHealthState(f.Health) {
	case types.ServiceHealthStarting, types.ServiceHealthHealthy, types.ServiceHealthUnhealthy:
	default:
		return serviceStateFile{}, errors.New("service state: unknown health")
	}
	switch f.Reason {
	case "", "restarting", "probe_failing":
	default:
		return serviceStateFile{}, errors.New("service state: unknown reason")
	}
	f.Restarts = min(max(f.Restarts, 0), serviceRestartsMax)
	if f.LastExitCode != nil {
		code := min(max(*f.LastExitCode, 0), serviceExitMax)
		f.LastExitCode = &code
	}
	return f, nil
}

// serviceProbe is what one read of the state file amounted to.
type serviceProbe int

const (
	serviceProbeRead        serviceProbe = iota // the file decoded
	serviceProbeMissing                         // the sandbox answered and has no such file
	serviceProbeUnreachable                     // no answer, or an answer that is not the file
)

// readServiceState runs the read once on the capturePane pattern (exec, drain, close), bounded by ctx
// and serviceStateMaxBytes. The file is returned only with serviceProbeRead.
func (s *Server) readServiceState(ctx context.Context, ref string) (serviceStateFile, serviceProbe) {
	sess, err := s.cfg.Runner.ExecStream(ctx, ref, runner.ExecSpec{Argv: serviceStateArgv})
	if sess != nil && sess.Close != nil {
		defer func() { _ = sess.Close() }() // tears down only this exec, never the sandbox
	}
	if err != nil || sess == nil || sess.Stdout == nil {
		return serviceStateFile{}, serviceProbeUnreachable
	}
	if sess.Stderr != nil {
		go func() { _, _ = io.Copy(io.Discard, sess.Stderr) }()
	}
	type answer struct {
		raw  []byte
		code int
		err  error
	}
	done := make(chan answer, 1)
	go func() {
		raw, rerr := io.ReadAll(io.LimitReader(sess.Stdout, serviceStateMaxBytes+1))
		if rerr != nil || len(raw) > serviceStateMaxBytes || sess.Wait == nil {
			done <- answer{raw: raw, err: rerr}
			return
		}
		code, werr := sess.Wait()
		done <- answer{raw, code, werr}
	}()
	select {
	case a := <-done:
		switch {
		case a.err != nil || len(a.raw) > serviceStateMaxBytes:
			return serviceStateFile{}, serviceProbeUnreachable
		case a.code != 0: // cat found no file (or could not read it)
			return serviceStateFile{}, serviceProbeMissing
		}
		f, derr := decodeServiceState(a.raw)
		if derr != nil {
			return serviceStateFile{}, serviceProbeUnreachable
		}
		return f, serviceProbeRead
	case <-ctx.Done():
		return serviceStateFile{}, serviceProbeUnreachable
	}
}

// serviceObservation is the status wardynd records for one read. unknown/supervisor_missing and
// unknown/unreachable are derived here and never read from the file. A read that carries no counts
// keeps the last ones, so a blip does not rewind restarts.
func serviceObservation(run types.AgentRun, f serviceStateFile, probe serviceProbe, now time.Time) types.RunServiceStatus {
	st := types.RunServiceStatus{ObservedAt: now}
	if prev := run.ServiceStatus; prev != nil {
		st.Restarts, st.LastExitCode = prev.Restarts, copyServiceExit(prev.LastExitCode)
	}
	switch probe {
	case serviceProbeRead:
		st.Health, st.Reason = types.ServiceHealthState(f.Health), f.Reason
		st.Restarts, st.LastExitCode = f.Restarts, f.LastExitCode
	case serviceProbeMissing:
		since := run.CreatedAt
		if run.AdmittedAt != nil {
			since = *run.AdmittedAt
		}
		if now.Sub(since) < serviceSupervisorGrace {
			st.Health = types.ServiceHealthStarting
		} else {
			st.Health, st.Reason = types.ServiceHealthUnknown, "supervisor_missing"
		}
	default:
		st.Health, st.Reason = types.ServiceHealthUnknown, "unreachable"
	}
	return st
}

func copyServiceExit(p *int) *int {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

// sameServiceStatus compares everything but observed_at.
func sameServiceStatus(a, b types.RunServiceStatus) bool {
	return a.Health == b.Health && a.Reason == b.Reason && a.Restarts == b.Restarts &&
		(a.LastExitCode == nil) == (b.LastExitCode == nil) &&
		(a.LastExitCode == nil || *a.LastExitCode == *b.LastExitCode)
}

// sweepServiceHealth is the run watcher's service health pass. A run with a runner is read by the
// replica that holds that runner's session; a run with none (the in-process substrate or an
// organization's executor) is read by the sweep leader alone.
func (s *Server) sweepServiceHealth(ctx context.Context) error {
	st, ok := s.cfg.Store.(store.ServiceRunStore)
	if !ok || s.cfg.Runner == nil {
		return nil
	}
	runs, err := st.ListServiceRuns(ctx)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, serviceSweepBudget)
	defer cancel()
	var held, led []types.AgentRun
	for _, run := range runs {
		switch {
		case run.RunnerID == nil:
			led = append(led, run)
		case s.runnerOnline(*run.RunnerID):
			held = append(held, run)
		}
	}
	s.mirrorServiceRuns(ctx, st, held)
	if len(led) == 0 {
		return nil
	}
	lctx, end, ok := s.beginLeaderSweep(ctx)
	if !ok {
		return nil
	}
	defer end()
	s.mirrorServiceRuns(lctx, st, led)
	return nil
}

// mirrorServiceRuns reads runs in parallel, the least recently observed first so a pass cut short by
// its budget never starves the same runs.
func (s *Server) mirrorServiceRuns(ctx context.Context, st store.ServiceRunStore, runs []types.AgentRun) {
	observed := func(r types.AgentRun) time.Time {
		if r.ServiceStatus == nil {
			return time.Time{}
		}
		return r.ServiceStatus.ObservedAt
	}
	slices.SortStableFunc(runs, func(a, b types.AgentRun) int { return observed(a).Compare(observed(b)) })
	var wg sync.WaitGroup
	slots := make(chan struct{}, serviceProbeWorkers)
	defer wg.Wait()
	for _, run := range runs {
		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
			return
		}
		wg.Go(func() {
			defer func() { <-slots }()
			defer func() {
				if r := recover(); r != nil {
					slog.ErrorContext(ctx, "wardynd: PANIC in service health read (contained)",
						slog.String("run_id", run.ID.String()), slog.Any("panic", r))
				}
			}()
			s.mirrorServiceRun(ctx, st, run)
		})
	}
}

// mirrorServiceRun reads one run's state file and stores the status when it changed or is due. It
// never calls TouchRun: a probe must not keep a run alive past an idle wall.
func (s *Server) mirrorServiceRun(ctx context.Context, st store.ServiceRunStore, run types.AgentRun) {
	pctx, cancel := context.WithTimeout(ctx, serviceProbeTimeout)
	f, probe := s.readServiceState(pctx, run.SandboxRef)
	cancel()
	now := s.cfg.Now()
	next := serviceObservation(run, f, probe, now)
	if prev := run.ServiceStatus; prev != nil && sameServiceStatus(*prev, next) && now.Sub(prev.ObservedAt) < serviceStatusRefresh {
		return
	}
	written, err := st.SetRunServiceStatus(ctx, run.ID, next)
	if err != nil {
		slog.WarnContext(ctx, "wardynd: storing a service's health failed",
			slog.String("run_id", run.ID.String()), slog.Any("err", err))
		return
	}
	if written {
		s.auditServiceChange(ctx, run, next)
	}
}

// auditServiceChange writes the rows a stored status earns: one when the health changed, one when the
// restart count rose. A run with no earlier status reads as starting with no restarts, so a service
// that launches cleanly writes nothing until it turns healthy. At most two rows per run per pass.
func (s *Server) auditServiceChange(ctx context.Context, run types.AgentRun, next types.RunServiceStatus) {
	from, restarts := types.ServiceHealthStarting, 0
	if prev := run.ServiceStatus; prev != nil {
		from, restarts = prev.Health, prev.Restarts
	}
	if from != next.Health {
		s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.service.health", run.ID.String(),
			"success", mustJSON(map[string]any{
				"from": from, "to": next.Health, "reason": next.Reason, "source": "sandbox",
			})))
	}
	if next.Restarts > restarts {
		s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.service.restart", run.ID.String(),
			"success", mustJSON(map[string]any{
				"restarts": next.Restarts, "delta": next.Restarts - restarts,
				"last_exit_code": next.LastExitCode, "source": "sandbox",
			})))
	}
}
