// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/composer/backends/sandbox"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// maxComposeResultUploadBytes caps a single compose-result PUT. A run-proposal
// JSON wrapper is small (bounded by the composer schema + input caps), so this
// is a generous DoS ceiling on a hostile in-sandbox process that streams junk.
const maxComposeResultUploadBytes = 1 << 20 // 1 MiB

// Compose-run launch tuning: how long RunClaudeCompose waits for the one-shot
// governed compose run to finish, and how often it polls the run state. The
// in-sandbox claude wire is self-bounded (--max-turns + the cli wire's per-call
// timeout) and the run has its own idle reaper (composeRunIdleCapSec); this wait
// is the control-plane backstop that returns a clear error instead of hanging.
const (
	composeRunWaitTimeout  = 4 * time.Minute
	composeRunPollInterval = 500 * time.Millisecond
	composeRunIdleCapSec   = 300
	composeRunTask         = "run compose" // discriminator on the run row (never harnessLoginTask / a workspace task)
	anthropicComposeHost   = "api.anthropic.com"
)

// composeRunnerSink is the LATE-BINDING seam a composer backend exposes when its
// Propose needs a governed-run launcher the Server owns. api.New iterates the
// registry's backends and hands each implementer the Server's RunClaudeCompose
// after construction (the registry is built at boot before the Server exists).
// The sandbox backend is the only implementer today.
type composeRunnerSink interface {
	SetRunClaude(sandbox.RunClaudeFunc)
}

// composeResultStore holds compose-run proposal-JSON uploads keyed by run id,
// pending a read by the waiting RunClaudeCompose. Short-lived and low-volume:
// each entry is written once by the in-sandbox compose PUT and taken
// (delete-on-read) once by the launcher — a sync.Map is plenty, no iteration.
type composeResultStore struct{ m sync.Map }

func (c *composeResultStore) put(runID uuid.UUID, raw []byte) { c.m.Store(runID, raw) }

func (c *composeResultStore) take(runID uuid.UUID) ([]byte, bool) {
	v, ok := c.m.LoadAndDelete(runID)
	if !ok {
		return nil, false
	}
	return v.([]byte), true
}

func (c *composeResultStore) discard(runID uuid.UUID) { c.m.Delete(runID) }

// handleUploadComposeResult accepts a PUT /api/v1/internal/compose-results/{runID}
// from the in-sandbox claude compose wire (maybe_exec_compose_mode). It MIRRORS
// handleUploadScanResult's brokered structured-JSON return channel, minus the
// workspace linkage: a compose run is an ordinary claude-code run (no
// WorkspaceID), so the only guard is the cross-run one — the caller must hold
// THIS run's token and the path run id must match it (claimsForRunUpload). The
// body is claude's raw stdout wrapper; it is stashed by run id for the waiting
// RunClaudeCompose to take once, then parsed control-plane-side (facts-out, not
// authority-out — same posture as scan).
func (s *Server) handleUploadComposeResult(w http.ResponseWriter, r *http.Request) {
	claims, ok := claimsForRunUpload(w, r)
	if !ok {
		return
	}
	raw, ok := readCappedBody(w, r, maxComposeResultUploadBytes, "compose result")
	if !ok {
		return
	}
	s.composeResults.put(claims.RunID, raw)
	// Counts only — never the proposal content (it may echo untrusted prompt text).
	s.recordAudit(r.Context(), s.auditEvent(&claims.RunID, types.ActorAgent, claims.SPIFFEID,
		"compose.result", claims.RunID.String(), "success", mustJSON(map[string]any{"bytes": len(raw)})))
	w.WriteHeader(http.StatusNoContent)
}

// RunClaudeCompose is the sandbox composer backend's late-bound run launcher (set
// via sandbox.Composer.SetRunClaude in New). It runs the REAL claude binary
// inside a GOVERNED one-shot run credentialed by the Wardyn-managed subscription
// injected PROXY-SIDE (never resident) — so a distroless container-mode wardynd
// with no host claude can do subscription-billed composing, ToS-clean.
//
// It mirrors launchScanRun's mint → CreateRun → dispatch flow, with three
// deltas: (1) NO WorkspaceID (an ordinary model run — resolveLLMTransport's
// managed path fires, injecting the token; a scan run makes no model call); (2)
// the WARDYN_COMPOSE_* env (discriminator + base64 prompt/schema) rides
// ExtraEnv, the exact same "only a discriminator + non-secret payload changes;
// clone/grants/EGRESS/recording/LLM-injection are identical" contract as
// scan/verify/exec; (3) it WAITS for the run to finish and reads the uploaded
// proposal from the compose-results store.
//
// Fail-closed: no managed subscription connected (managedInjectReady) is a clear
// error BEFORE any run is created; a run that finishes without uploading a
// proposal (e.g. the sandbox could not reach the control plane) is a clear error
// too. The returned bytes are claude's raw stdout wrapper — the caller
// (sandbox.Composer.Propose) extracts + parses them through the canonical loop.
func (s *Server) RunClaudeCompose(ctx context.Context, promptJSON []byte) ([]byte, error) {
	if s.cfg.Runner == nil {
		return nil, fmt.Errorf("sandbox composer: no runner configured")
	}
	// Fail closed: the whole point is the managed subscription token injected
	// proxy-side. Without it there is no credential (the sandbox holds only an
	// inert sentinel), so composing would 401 — refuse up front with a clear error.
	if !s.managedInjectReady("claude-code") {
		return nil, fmt.Errorf("sandbox composer: no managed subscription connected — connect one (claude setup-token) so the compose run can authenticate")
	}
	var cp sandbox.ComposePrompt
	if err := json.Unmarshal(promptJSON, &cp); err != nil {
		return nil, fmt.Errorf("sandbox composer: decode prompt: %w", err)
	}

	// Detach from request cancellation for the durable launch work (mint,
	// CreateRun, dispatch) exactly like launchScanRun; the wait below still honors
	// the caller's ctx (client disconnect stops waiting) via waitCtx.
	launchCtx := context.WithoutCancel(ctx)
	runID := uuid.New()
	actor := s.composeActor()
	id, err := s.cfg.Identity.MintRunIdentity(launchCtx, runID, actor, actor, internalAudience)
	if err != nil {
		return nil, fmt.Errorf("sandbox composer: mint run identity: %w", err)
	}
	cc := s.cfg.DefaultPolicy.MinConfinementClass
	if cc == "" {
		cc = types.CC1
	}
	now := s.cfg.Now().UTC()
	run := types.AgentRun{
		ID:               runID,
		CreatedAt:        now,
		UpdatedAt:        now,
		CreatedBy:        actor,
		Agent:            "claude-code", // carries the claude binary; managed subscription credentials it
		Task:             composeRunTask,
		ConfinementClass: cc,
		State:            types.RunPending,
		SPIFFEID:         id.SPIFFEID,
		RunnerTarget:     s.cfg.RunnerTarget,
		AutoStopAfterSec: composeRunIdleCapSec, // reaper reads the run row
	}
	created, err := s.cfg.Store.CreateRun(launchCtx, run)
	if err != nil {
		s.cfg.Identity.RevokeRun(launchCtx, runID) //nolint:errcheck // best-effort cleanup of the minted-but-unused token
		return nil, fmt.Errorf("sandbox composer: create run: %w", err)
	}

	// Managed-subscription policy: api.anthropic.com must be in AllowedDomains so
	// (a) resolveLLMTransport's managed gate fires (it requires a non-empty
	// allow-list) and (b) claude can actually reach the host over the tunnel. No
	// mounts, no grants — the managed token is injected proxy-side.
	policy := types.RunPolicySpec{
		MinConfinementClass: cc,
		AllowedDomains:      []string{anthropicComposeHost},
		AutoStopAfterSec:    composeRunIdleCapSec,
	}
	s.dispatchRun(launchCtx, created, dispatchParams{
		RunToken:   id.Token,
		Image:      agentImage("claude-code", s.cfg.AgentImages),
		Policy:     policy,
		ExtraEnv:   composeSandboxEnv(cp),
	})

	// Wait for the run to finish, then take its uploaded proposal. The in-sandbox
	// wire PUTs the proposal BEFORE the agent process exits (same ordering scan
	// relies on), so once the run is terminal the upload — if it happened — is in
	// the store.
	waitCtx, cancel := context.WithTimeout(ctx, composeRunWaitTimeout)
	defer cancel()
	finalState, err := s.waitForRunTerminal(waitCtx, runID)
	if err != nil {
		// Timed out / client left before the run finished: reclaim the run + its
		// sandbox best-effort so a hung compose can't linger, and drop any partial.
		s.reclaimComposeRun(context.WithoutCancel(ctx), runID)
		s.composeResults.discard(runID)
		return nil, fmt.Errorf("sandbox composer: compose run did not finish: %w", err)
	}
	raw, ok := s.composeResults.take(runID)
	if !ok {
		return nil, fmt.Errorf("sandbox composer: compose run %s finished (%s) but uploaded no proposal — the sandbox likely could not reach the control plane (check sandbox→wardynd networking)", runID, finalState)
	}
	return raw, nil
}

// composeActor is the recorded principal for a compose run: the configured local
// operator when set, else a system marker. A compose run is server-initiated (it
// backs an operator's compose request), so it has no per-request human here.
func (s *Server) composeActor() string {
	if s.cfg.LocalOperator != "" {
		return s.cfg.LocalOperator
	}
	return "wardyn-composer"
}

// composeSandboxEnv turns a decoded ComposePrompt into the WARDYN_COMPOSE_* env
// the in-sandbox claude wire (maybe_exec_compose_mode) reads. The system/user/
// schema payloads are base64-encoded so multi-line / shell-special content
// survives the env round-trip untouched; the tool denylist + turn cap are inert
// fixed strings that need none. The managed subscription token is NEVER here — it
// is injected proxy-side (invariant 1); the sandbox holds only the sentinel.
func composeSandboxEnv(cp sandbox.ComposePrompt) map[string]string {
	b64 := base64.StdEncoding.EncodeToString
	env := map[string]string{
		"WARDYN_COMPOSE_ONLY":             "1",
		"WARDYN_COMPOSE_SYSTEM_B64":       b64([]byte(cp.System)),
		"WARDYN_COMPOSE_PROMPT_B64":       b64([]byte(cp.User)),
		"WARDYN_COMPOSE_SCHEMA_B64":       b64([]byte(cp.Schema)),
		"WARDYN_COMPOSE_DISALLOWED_TOOLS": cp.DisallowedTools,
		"WARDYN_COMPOSE_MAX_TURNS":        cp.MaxTurns,
	}
	if cp.Model != "" {
		env["WARDYN_COMPOSE_MODEL"] = cp.Model
	}
	return env
}

// waitForRunTerminal polls the run row until it reaches a terminal state,
// returning that state. It errors on ctx cancellation/timeout (the caller
// reclaims the run). Server-side twin of the CLI's `run --wait` poll.
func (s *Server) waitForRunTerminal(ctx context.Context, runID uuid.UUID) (types.RunState, error) {
	ticker := time.NewTicker(composeRunPollInterval)
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

// reclaimComposeRun tears down a compose run that never reached a terminal state
// within the wait window (a hung claude). It CASes the run KILLED from its
// current non-terminal state and runs the shared terminal tail (revoke + sandbox
// teardown), so a stuck compose can't leak a live token or a running container.
// Best-effort + idempotent: a run that finished on its own is left untouched.
func (s *Server) reclaimComposeRun(ctx context.Context, runID uuid.UUID) {
	run, err := s.cfg.Store.GetRun(ctx, runID)
	if err != nil || isTerminalRunState(run.State) {
		return
	}
	if applied, _ := s.cfg.Store.UpdateRunStateIf(ctx, runID, run.State, types.RunKilled); applied {
		s.finalizeRunTail(ctx, runID, run.SandboxRef, "run.compose",
			"failure", map[string]any{"reason": "compose wait timed out; run reclaimed"})
	}
}
