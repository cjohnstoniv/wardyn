// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"strings"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// byoiExecLessRefused refuses a BYOI image outright, before it can even
// ATTEMPT the selftest, on an exec-less (krun microVM) substrate.
// runAsMainProcess (internal/runner/docker/driver.go) makes the
// sandbox's container process ITSELF the agent on that substrate — there is
// no separate exec slot — so byoiSelftest's own Exec would consume the
// sandbox's one process, guaranteeing the task Exec that follows it fails
// against an already-exited container (Exec would be called twice: once for
// the selftest, once for the task). Refuses up front (audit + teardown +
// FAILED) instead of wasting the slot finding that out the hard way.
// Capabilities().Resolved[cc] carries an "oci/krun" runtime label on that
// substrate (Kata/CC3 stays exec-capable: its Resolved label carries no such
// prefix). Reports whether it refused; the caller must return immediately.
func (s *Server) byoiExecLessRefused(ctx context.Context, run types.AgentRun, ref string) bool {
	caps, cerr := s.cfg.Runner.Capabilities(ctx)
	if cerr != nil || !strings.HasPrefix(caps.Resolved[run.ConfinementClass], "oci/krun") {
		return false
	}
	s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.selftest",
		run.ID.String(), "failure", mustJSON(map[string]any{
			"confinement_class": run.ConfinementClass,
			"detail": "BYOI images are refused on an exec-less (krun) runtime: the selftest's own exec " +
				"would consume the sandbox's only process, guaranteeing the task exec that follows it fails",
		})))
	s.stopSandboxOrAudit(ctx, run.ID, ref, "run.selftest")
	s.failAndRevoke(ctx, run.ID, types.RunRunning,
		"BYOI images are not supported on this exec-less (krun) runtime")
	return true
}

// byoiSelftest runs `agent-run --selftest` inside a BYOI sandbox and waits for
// its exit, auditing the outcome. It relies on the runner's "latest Exec wins"
// contract: this exec is tracked and Wait'd BEFORE the real task exec replaces
// it, so the subsequent task's completion watcher is unaffected. Returns true
// when the selftest passed (exit 0). failClosed only governs the audit tone —
// the caller decides what to do with a false (fail the batch run, or warn-only
// for interactive). A selftest that cannot even start (missing shell/binary,
// exit 127) surfaces as a non-nil Exec/Wait error → returns false.
// byoiSelftestTimeout bounds the fail-closed BYOI selftest gate so a hostile or
// broken base image whose agent-run --selftest hangs cannot block the dispatch
// goroutine forever — on timeout the gate fails closed (returns false).
const byoiSelftestTimeout = 2 * time.Minute

func (s *Server) byoiSelftest(ctx context.Context, run types.AgentRun, ref string, failClosed bool) bool {
	ctx, cancel := context.WithTimeout(ctx, byoiSelftestTimeout)
	defer cancel()
	if _, xerr := s.cfg.Runner.Exec(ctx, ref, []string{"/usr/local/bin/agent-run", "--selftest"}); xerr != nil {
		s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.selftest",
			run.ID.String(), "failure", mustJSON(map[string]any{
				"error": xerr.Error(), "fail_closed": failClosed,
				"detail": "BYOI image could not run agent-run --selftest (missing shell or harness binary?)",
			})))
		return false
	}
	code, werr := s.cfg.Runner.Wait(ctx, ref)
	if werr != nil {
		s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.selftest",
			run.ID.String(), "failure", mustJSON(map[string]any{
				"error": werr.Error(), "fail_closed": failClosed,
			})))
		return false
	}
	if code != 0 {
		s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.selftest",
			run.ID.String(), "failure", mustJSON(map[string]any{
				"exit_code": code, "fail_closed": failClosed,
				"detail": "BYOI image failed the agent-run contract selftest",
			})))
		return false
	}
	s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.selftest",
		run.ID.String(), "success", mustJSON(map[string]any{"exit_code": 0})))
	return true
}
