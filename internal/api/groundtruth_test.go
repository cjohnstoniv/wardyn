// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/groundtruth"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// mintGroundtruthToken mints a host-sensor token bound to the SEPARATE
// groundtruth audience.
func (h *harness) mintGroundtruthToken(t *testing.T) string {
	t.Helper()
	id, err := h.idp.MintRunIdentity(context.Background(), uuid.Nil, groundtruth.SensorActor, "", groundtruthAudience)
	if err != nil {
		t.Fatalf("mint groundtruth token: %v", err)
	}
	return id.Token
}

func TestGroundtruthAuthRejectsBadTokens(t *testing.T) {
	h := newHarness(t)
	body := `{"events":[]}`
	// No token.
	if w := do(t, h.srv, http.MethodPost, "/api/v1/internal/groundtruth", "", body); w.Code != http.StatusUnauthorized {
		t.Errorf("no token: code = %d, want 401", w.Code)
	}
	// Admin token must NOT pass (not a JWT for this audience).
	if w := do(t, h.srv, http.MethodPost, "/api/v1/internal/groundtruth", adminToken, body); w.Code != http.StatusUnauthorized {
		t.Errorf("admin token: code = %d, want 401", w.Code)
	}
	// A run token (aud=wardyn-internal) must NOT pass the groundtruth endpoint:
	// audience separation is the security boundary.
	runTok := h.mintRunToken(t, uuid.New())
	if w := do(t, h.srv, http.MethodPost, "/api/v1/internal/groundtruth", runTok, body); w.Code != http.StatusUnauthorized {
		t.Errorf("run token on groundtruth: code = %d, want 401 (audience separation)", w.Code)
	}
}

func TestGroundtruthTokenRejectedOnMintEndpoint(t *testing.T) {
	// The CONVERSE boundary: a groundtruth token must NOT be usable to mint
	// credentials. This proves the token is audit-write-only.
	h := newHarness(t)
	gtTok := h.mintGroundtruthToken(t)
	body := `{"grant_id":"` + uuid.New().String() + `"}`
	w := do(t, h.srv, http.MethodPost, "/api/v1/internal/credentials/mint", gtTok, body)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("groundtruth token on mint: code = %d, want 401 (audit-write-only)", w.Code)
	}
}

func TestGroundtruthHeartbeatAcceptedNullRun(t *testing.T) {
	// A heartbeat (run_id NULL) must be accepted with NO Pool wired (GetRun is
	// only called for non-NULL run ids), and recorded with forced attribution.
	h := newHarness(t)
	tok := h.mintGroundtruthToken(t)
	hb := groundtruth.HeartbeatEventWithDropped(0, 0)
	body, _ := json.Marshal(groundtruthBatch{Events: []types.AuditEvent{hb}})
	w := do(t, h.srv, http.MethodPost, "/api/v1/internal/groundtruth", tok, string(body))
	if w.Code != http.StatusAccepted {
		t.Fatalf("heartbeat code = %d, want 202; body=%s", w.Code, w.Body.String())
	}
	var found bool
	for _, ev := range h.audit.events {
		if ev.Action == groundtruth.ActionSensorHeartbeat {
			found = true
			if ev.ActorType != types.ActorSystem || ev.Actor != groundtruth.SensorActor {
				t.Errorf("heartbeat actor = %s/%s, want system/%s", ev.ActorType, ev.Actor, groundtruth.SensorActor)
			}
			if ev.RunID != nil {
				t.Errorf("heartbeat run id = %v, want nil", ev.RunID)
			}
		}
	}
	if !found {
		t.Fatal("heartbeat not recorded to audit (fanout path)")
	}
}

func TestGroundtruthForcesActorEvenIfSpoofed(t *testing.T) {
	// A compromised sensor that tries to set actor_type=human / actor=alice must
	// have those FORCED back to the system sensor identity server-side.
	h := newHarness(t)
	tok := h.mintGroundtruthToken(t)
	spoofed := types.AuditEvent{
		Action:    groundtruth.ActionProcessExec,
		ActorType: types.ActorHuman,
		Actor:     "alice@example.com",
		Target:    "/usr/bin/python3",
		Outcome:   "success",
		Data:      json.RawMessage(`{"stream":"ebpf","subtype":"process_exec","correlation":"unmapped"}`),
		// run_id NULL so no Pool lookup is needed in this no-DB harness.
	}
	body, _ := json.Marshal(groundtruthBatch{Events: []types.AuditEvent{spoofed}})
	w := do(t, h.srv, http.MethodPost, "/api/v1/internal/groundtruth", tok, string(body))
	if w.Code != http.StatusAccepted {
		t.Fatalf("code = %d, want 202; body=%s", w.Code, w.Body.String())
	}
	for _, ev := range h.audit.events {
		if ev.Action == groundtruth.ActionProcessExec {
			if ev.ActorType != types.ActorSystem {
				t.Errorf("actor_type = %s, want system (forced)", ev.ActorType)
			}
			if ev.Actor != groundtruth.SensorActor {
				t.Errorf("actor = %s, want %s (forced)", ev.Actor, groundtruth.SensorActor)
			}
		}
	}
}

func TestGroundtruthRejectsNonKernelAction(t *testing.T) {
	// The sensor must not be able to forge a non-kernel.* event (e.g. an
	// egress.deny or credential.mint).
	h := newHarness(t)
	tok := h.mintGroundtruthToken(t)
	forged := types.AuditEvent{
		Action:  "egress.deny", // not kernel.*
		Outcome: "denied",
	}
	body, _ := json.Marshal(groundtruthBatch{Events: []types.AuditEvent{forged}})
	w := do(t, h.srv, http.MethodPost, "/api/v1/internal/groundtruth", tok, string(body))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("non-kernel action: code = %d, want 400", w.Code)
	}
}

// failingRecorder records every event but returns an error on Record, modelling
// a durability failure on the "tamper-proof" audit stream.
type failingRecorder struct {
	events []types.AuditEvent
	err    error
}

func (r *failingRecorder) Record(_ context.Context, ev types.AuditEvent) error {
	r.events = append(r.events, ev)
	return r.err
}

// TestGroundtruthPartialBatchDoesNotCommitGoodEventsOnBadOne is the RED-FIRST
// regression for the partial-commit finding: a batch whose FIRST event is valid
// (kernel.*, NULL run_id) but whose SECOND event is invalid (non-kernel action)
// must NOT silently commit the good event while returning a 400 — that loses
// events and miscounts. The whole batch is validated before anything commits, so
// a single bad event rejects the batch atomically and nothing is recorded.
func TestGroundtruthPartialBatchDoesNotCommitGoodEventsOnBadOne(t *testing.T) {
	h := newHarness(t)
	tok := h.mintGroundtruthToken(t)

	good := types.AuditEvent{
		Action:  groundtruth.ActionProcessExec, // kernel.* + NULL run_id => no DB needed
		Outcome: "success",
		Data:    json.RawMessage(`{"stream":"ebpf","subtype":"process_exec","correlation":"unmapped"}`),
	}
	bad := types.AuditEvent{
		Action:  "egress.deny", // not kernel.* => rejected
		Outcome: "denied",
	}
	body, _ := json.Marshal(groundtruthBatch{Events: []types.AuditEvent{good, bad}})
	w := do(t, h.srv, http.MethodPost, "/api/v1/internal/groundtruth", tok, string(body))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("mixed batch code = %d, want 400 (whole batch rejected); body=%s", w.Code, w.Body.String())
	}
	// The GOOD event must NOT have been committed — validate-before-commit means
	// a rejected batch records nothing (no partial/lossy commit).
	for _, ev := range h.audit.events {
		if ev.Action == groundtruth.ActionProcessExec {
			t.Fatalf("good event was committed despite a bad event in the batch (partial commit / event loss)")
		}
	}
}

// TestGroundtruthAuditWriteFailureIsNon2xx is the RED-FIRST regression for the
// swallowed-write finding: when the audit Record fails (durability failure on
// the tamper-proof stream), the endpoint must propagate a non-2xx so the sender
// retries (fail-closed durability) rather than reporting the events accepted.
func TestGroundtruthAuditWriteFailureIsNon2xx(t *testing.T) {
	h := newHarness(t)
	tok := h.mintGroundtruthToken(t)
	// Swap in a recorder that fails every write.
	fail := &failingRecorder{err: errors.New("audit store down")}
	h.srv.cfg.Audit = fail

	hb := groundtruth.HeartbeatEventWithDropped(0, 0) // kernel.* + NULL run_id => no DB needed
	body, _ := json.Marshal(groundtruthBatch{Events: []types.AuditEvent{hb}})
	w := do(t, h.srv, http.MethodPost, "/api/v1/internal/groundtruth", tok, string(body))
	if w.Code/100 == 2 {
		t.Fatalf("audit write failed but endpoint reported success (code %d) — events would be silently lost; want non-2xx so the sender retries", w.Code)
	}
}

// TestGroundtruthClampsSuppliedFutureTime is the regression for the trusted-Time
// finding: a compromised/skewed sensor posting a heartbeat with Time far in the
// FUTURE must not have that Time trusted. /healthz computes ebpf_groundtruth
// health as Now().Sub(latest heartbeat Time) <= TTL, so a future Time makes the
// diff negative and pins "healthy" forever even after the sensor dies. The server
// FORCES ev.Time to its own clock, so the stream can still honestly degrade.
func TestGroundtruthClampsSuppliedFutureTime(t *testing.T) {
	h := newHarness(t)
	tok := h.mintGroundtruthToken(t)

	future := time.Now().Add(100 * 24 * time.Hour)    // ~100 days ahead
	hb := groundtruth.HeartbeatEventWithDropped(0, 0) // kernel.* + NULL run_id => no DB needed
	hb.Time = future
	body, _ := json.Marshal(groundtruthBatch{Events: []types.AuditEvent{hb}})
	w := do(t, h.srv, http.MethodPost, "/api/v1/internal/groundtruth", tok, string(body))
	if w.Code != http.StatusAccepted {
		t.Fatalf("code = %d, want 202; body=%s", w.Code, w.Body.String())
	}
	var found bool
	for _, ev := range h.audit.events {
		if ev.Action == groundtruth.ActionSensorHeartbeat {
			found = true
			if !ev.Time.Before(future) {
				t.Fatalf("supplied future Time was trusted (%s); server must clamp to its own clock so health can still degrade", ev.Time)
			}
		}
	}
	if !found {
		t.Fatal("heartbeat not recorded")
	}
}

func TestGroundtruthEmptyBatch(t *testing.T) {
	h := newHarness(t)
	tok := h.mintGroundtruthToken(t)
	w := do(t, h.srv, http.MethodPost, "/api/v1/internal/groundtruth", tok, `{"events":[]}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("empty batch: code = %d, want 202", w.Code)
	}
}

func TestHealthzEbpfUnavailableWithoutSensor(t *testing.T) {
	// With no Pool wired (and thus no heartbeat ever), /healthz must report the
	// ground-truth stream as unavailable — honest degradation, never a silent
	// claim that the stream exists.
	h := newHarness(t)
	w := do(t, h.srv, http.MethodGet, "/healthz", "", "")
	if w.Code != http.StatusOK {
		t.Fatalf("healthz code = %d", w.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	gt, ok := body["ebpf_groundtruth"].(map[string]any)
	if !ok {
		t.Fatalf("healthz missing ebpf_groundtruth object: %v", body["ebpf_groundtruth"])
	}
	if gt["state"] != "unavailable" {
		t.Errorf("ebpf_groundtruth.state = %v, want unavailable", gt["state"])
	}
}

// stubHeartbeatStore returns a fixed heartbeat from LatestAuditEventByAction so
// /healthz's ground-truth state can be exercised without a real Store/Pool. It
// embeds store.Store (nil) and overrides ONLY the one method /healthz calls.
type stubHeartbeatStore struct {
	store.Store
	ev types.AuditEvent
}

func (s stubHeartbeatStore) LatestAuditEventByAction(context.Context, string) (types.AuditEvent, error) {
	return s.ev, nil
}

// healthzEbpf GETs /healthz and returns its ebpf_groundtruth object.
func healthzEbpf(t *testing.T, h *harness) map[string]any {
	t.Helper()
	w := do(t, h.srv, http.MethodGet, "/healthz", "", "")
	if w.Code != http.StatusOK {
		t.Fatalf("healthz code = %d", w.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	gt, ok := body["ebpf_groundtruth"].(map[string]any)
	if !ok {
		t.Fatalf("healthz missing ebpf_groundtruth object: %v", body["ebpf_groundtruth"])
	}
	return gt
}

// TestHealthzEbpfIdleWhenNoEventsObserved is the red-first regression for
// a FRESH heartbeat proves the sidecar process is alive and reaching the control
// plane, but if it has mapped zero kernel events (Tetragon dead / wrong export
// path / no TracingPolicy) the stream is BLIND. /healthz must report "idle", not
// "healthy" — "heartbeat arriving" is not "kernel ground truth arriving".
func TestHealthzEbpfIdleWhenNoEventsObserved(t *testing.T) {
	h := newHarness(t)
	hb := groundtruth.HeartbeatEventWithDropped(0, 0) // fresh beat, observed_total==0
	hb.Time = time.Now()
	h.srv.cfg.Store = stubHeartbeatStore{ev: hb}

	gt := healthzEbpf(t, h)
	if gt["state"] != "idle" {
		t.Fatalf("ebpf_groundtruth.state = %v, want idle (heartbeat alive but zero kernel events observed = blind, not healthy)", gt["state"])
	}
}

// TestHealthzEbpfHealthyWhenEventsObserved is the companion: once real kernel
// events have been observed (observed_total>0) within the TTL, the stream is
// genuinely healthy.
func TestHealthzEbpfHealthyWhenEventsObserved(t *testing.T) {
	h := newHarness(t)
	hb := groundtruth.HeartbeatEventWithDropped(0, 5)
	hb.Time = time.Now()
	h.srv.cfg.Store = stubHeartbeatStore{ev: hb}

	gt := healthzEbpf(t, h)
	if gt["state"] != "healthy" {
		t.Fatalf("ebpf_groundtruth.state = %v, want healthy (events observed within TTL)", gt["state"])
	}
}

// notFoundStore is a minimal store.Store whose GetRun always reports
// ErrNotFound — modelling a stale/orphaned run_id (a DB reset/re-point, a
// purged run row, or a forged id) that names no real run.
type notFoundStore struct {
	store.Store
}

func (notFoundStore) GetRun(context.Context, uuid.UUID) (types.AgentRun, error) {
	return types.AgentRun{}, store.ErrNotFound
}

// TestGroundtruthUnknownRunIDDowngradesNotRejectsBatch is the RED-FIRST
// regression for a batch with ONE event whose run_id is
// present-but-unknown (e.g. an orphaned container after a DB reset/re-point)
// must NOT 400 the whole batch — the sensor treats a 4xx as a non-retryable
// whole-batch drop, so every co-batched event would be permanently lost. The
// bad run_id is downgraded to unmapped instead, and the whole batch,
// including the co-batched good event, is accepted and recorded.
func TestGroundtruthUnknownRunIDDowngradesNotRejectsBatch(t *testing.T) {
	h := newHarness(t)
	srv := New(baseTestConfig(h, notFoundStore{}))
	tok := h.mintGroundtruthToken(t)

	staleRun := uuid.New() // present but names no real run per notFoundStore
	stale := types.AuditEvent{
		Action:  groundtruth.ActionProcessExec,
		RunID:   &staleRun,
		Target:  "/usr/bin/python3",
		Outcome: "success",
		Data:    json.RawMessage(`{"stream":"ebpf","subtype":"process_exec","container_id":"abc123","correlation":"mapped"}`),
	}
	good := types.AuditEvent{
		Action:  groundtruth.ActionProcessExec, // NULL run_id, co-batched
		Target:  "/bin/true",
		Outcome: "success",
		Data:    json.RawMessage(`{"stream":"ebpf","subtype":"process_exec","correlation":"unmapped"}`),
	}
	body, _ := json.Marshal(groundtruthBatch{Events: []types.AuditEvent{stale, good}})
	w := do(t, srv, http.MethodPost, "/api/v1/internal/groundtruth", tok, string(body))
	if w.Code != http.StatusAccepted {
		t.Fatalf("batch with a present-but-unknown run_id: code = %d, want 202 (downgrade, not reject); body=%s", w.Code, w.Body.String())
	}

	var sawStale, sawGood bool
	for _, ev := range h.audit.events {
		switch ev.Target {
		case "/usr/bin/python3":
			sawStale = true
			if ev.RunID != nil {
				t.Errorf("downgraded event run_id = %v, want nil (cleared)", *ev.RunID)
			}
			var data map[string]any
			if err := json.Unmarshal(ev.Data, &data); err != nil {
				t.Fatalf("decode downgraded event data: %v", err)
			}
			if data["correlation"] != "unmapped" {
				t.Errorf("downgraded event data.correlation = %v, want unmapped", data["correlation"])
			}
			if data["reason"] != "run_id_not_found" {
				t.Errorf("downgraded event data.reason = %v, want run_id_not_found", data["reason"])
			}
			if data["container_id"] != "abc123" {
				t.Errorf("downgraded event data.container_id = %v, want abc123 preserved", data["container_id"])
			}
		case "/bin/true":
			sawGood = true
		}
	}
	if !sawStale {
		t.Fatal("event with a present-but-unknown run_id was dropped, not downgraded and recorded")
	}
	if !sawGood {
		t.Fatal("co-batched good event was lost (would indicate the whole batch was still rejected)")
	}
}
