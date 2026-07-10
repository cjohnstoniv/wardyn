// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// trackAComposeBody is a single-shot (mode:skip) compose request over an
// ephemeral workspace, so the pipeline goes straight to a proposal (no git
// detection, no mounts) and both transports return the same body.
const trackAComposeBody = `{"prompt":"build a small website","workspace":{"kind":"ephemeral"},"mode":"skip"}`

// singleBackendRegistry wires c as the sole ("fake") backend on a composer
// registry, the one-entry shape every compose test harness in this package
// needs to make the compose/assist/telemetry endpoints Enabled().
func singleBackendRegistry(t *testing.T, c composer.Composer) *composer.Registry {
	t.Helper()
	reg, err := composer.NewRegistry("fake", []composer.RegistryEntry{{
		Info:     composer.BackendInfo{Name: "fake", Provider: "fake", Model: "test"},
		Composer: c,
	}})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	return reg
}

// newStreamComposerHarness wires a deterministic FakeComposer into the standard
// api harness so the compose endpoint is Enabled().
func newStreamComposerHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t)
	h.srv.cfg.Composer = singleBackendRegistry(t, &composer.FakeComposer{Result: composer.Proposal{
		Run:          composer.RunInput{Agent: "claude", Task: "build a small website"},
		InlinePolicy: types.RunPolicySpec{MinConfinementClass: types.CC2},
		Summary:      "proposed a throwaway sandbox",
	}})
	return h
}

// streamComposePOST POSTs trackAComposeBody with the given Accept header (empty =
// none) and the admin token, returning the recorded response.
func streamComposePOST(t *testing.T, srv *Server, accept string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/runs/compose", strings.NewReader(trackAComposeBody))
	r.Header.Set("Authorization", "Bearer "+adminToken)
	if accept != "" {
		r.Header.Set("Accept", accept)
	}
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	return w
}

// streamParseSSE splits an SSE body into decoded ComposeEvents (one per
// `data: <json>\n\n` frame).
func streamParseSSE(t *testing.T, body string) []composer.ComposeEvent {
	t.Helper()
	var out []composer.ComposeEvent
	for _, frame := range strings.Split(body, "\n\n") {
		frame = strings.TrimSpace(frame)
		if frame == "" {
			continue
		}
		data, ok := strings.CutPrefix(frame, "data: ")
		if !ok {
			t.Fatalf("SSE frame missing `data: ` prefix: %q", frame)
		}
		var ev composer.ComposeEvent
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			t.Fatalf("decode SSE frame %q: %v", frame, err)
		}
		out = append(out, ev)
	}
	return out
}

// TestTrackAComposeBufferPathJSON asserts the default (JSON) transport still
// returns the proposal body — the byte-identical path CLI/tests depend on.
func TestTrackAComposeBufferPathJSON(t *testing.T) {
	h := newStreamComposerHarness(t)
	w := streamComposePOST(t, h.srv, "application/json")
	if w.Code != http.StatusOK {
		t.Fatalf("buffer code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("buffer content-type = %q, want application/json", ct)
	}
	var resp composeResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode buffer body: %v", err)
	}
	if resp.Kind != "proposal" {
		t.Errorf("buffer kind = %q, want proposal", resp.Kind)
	}
	// The composer never sets the ceiling; the clamp/grade floor does. Proposal
	// must carry a deterministic overall risk (non-empty).
	if resp.OverallRisk == "" {
		t.Errorf("buffer overall_risk is empty; grade did not run")
	}
}

// TestComposeRaisesRunConfinementToFloor is the API-level pin for item 26: a
// composed proposal whose run advertises a WEAKER confinement class than the
// clamped policy floor must come back raised to the floor (else handleCreateRun
// would 422 the self-inconsistent proposal). Deleting the ClampRunConfinement
// wiring in compose.go must fail here — the composer-package unit test alone
// would not catch a broken pipeline.
func TestComposeRaisesRunConfinementToFloor(t *testing.T) {
	h := newHarness(t)
	h.srv.cfg.Composer = singleBackendRegistry(t, &composer.FakeComposer{Result: composer.Proposal{
		// The run asks for the WEAKEST tier while the policy floor is CC2.
		Run:          composer.RunInput{Agent: "claude", Task: "build a small website", ConfinementClass: "CC1"},
		InlinePolicy: types.RunPolicySpec{MinConfinementClass: types.CC2},
		Summary:      "proposed a throwaway sandbox",
	}})

	w := streamComposePOST(t, h.srv, "application/json")
	if w.Code != http.StatusOK {
		t.Fatalf("compose code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp composeResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if got := resp.Proposed.Run.ConfinementClass; got != string(types.CC2) {
		t.Fatalf("run confinement_class = %q, want CC2 (raised to the clamped floor)", got)
	}
	if resp.Proposed.InlinePolicy.MinConfinementClass != types.CC2 {
		t.Fatalf("inline_policy floor = %q, want CC2", resp.Proposed.InlinePolicy.MinConfinementClass)
	}
}

// TestTrackAComposeSSEMatchesBuffer asserts the SSE transport streams >=1 stage
// frame then a terminal result frame whose decoded payload deep-equals the
// buffer body — the core buffer/SSE equivalence guarantee of the seam.
func TestTrackAComposeSSEMatchesBuffer(t *testing.T) {
	h := newStreamComposerHarness(t)

	// Buffer body (the reference).
	buf := streamComposePOST(t, h.srv, "application/json")
	if buf.Code != http.StatusOK {
		t.Fatalf("buffer code = %d, want 200; body=%s", buf.Code, buf.Body.String())
	}
	var bufPayload map[string]any
	if err := json.Unmarshal(buf.Body.Bytes(), &bufPayload); err != nil {
		t.Fatalf("decode buffer body: %v", err)
	}

	// SSE stream.
	sse := streamComposePOST(t, h.srv, "text/event-stream")
	if sse.Code != http.StatusOK {
		t.Fatalf("sse code = %d, want 200; body=%s", sse.Code, sse.Body.String())
	}
	if ct := sse.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("sse content-type = %q, want text/event-stream", ct)
	}
	if cc := sse.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("sse cache-control = %q, want no-store", cc)
	}
	if xb := sse.Header().Get("X-Accel-Buffering"); xb != "no" {
		t.Errorf("sse x-accel-buffering = %q, want no", xb)
	}
	if !sse.Flushed {
		t.Errorf("sse response was never flushed")
	}

	events := streamParseSSE(t, sse.Body.String())
	if len(events) == 0 {
		t.Fatalf("no SSE frames parsed")
	}
	var stageCount int
	var result *composer.ComposeEvent
	for i := range events {
		switch events[i].Type {
		case composer.EvStage:
			stageCount++
			if events[i].Stage == "" {
				t.Errorf("stage frame %d has empty stage key", i)
			}
		case composer.EvResult:
			result = &events[i]
		case composer.EvError:
			t.Fatalf("unexpected error frame: %s", events[i].Error)
		}
	}
	if stageCount < 1 {
		t.Fatalf("want >=1 stage frame, got %d (events=%+v)", stageCount, events)
	}
	if result == nil {
		t.Fatalf("no terminal result frame in SSE stream")
	}
	// The result frame must be terminal (last).
	if last := events[len(events)-1]; last.Type != composer.EvResult {
		t.Errorf("result frame is not terminal; last frame type = %q", last.Type)
	}
	ssePayload, ok := result.Result.(map[string]any)
	if !ok {
		t.Fatalf("result payload type = %T, want JSON object", result.Result)
	}
	if !reflect.DeepEqual(ssePayload, bufPayload) {
		t.Errorf("SSE result payload != buffer body\nSSE:    %#v\nbuffer: %#v", ssePayload, bufPayload)
	}
}
