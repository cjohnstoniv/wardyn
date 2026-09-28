// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// anyRunLive answers GetRun with a RUNNING run for ANY id.
//
// It exists because internalAuth now asks the store whether the run behind a
// presented token is still alive (refuseTerminalRun), so a double that
// serves an /internal/* route has to be able to answer that one question. It is
// embedded rather than copied into each double so "this fake has a live run"
// reads as one fact in one place, and so a double that wants a DIFFERENT answer
// overrides GetRun explicitly rather than by omission.
type anyRunLive struct{ store.Store }

func (anyRunLive) GetRun(_ context.Context, id uuid.UUID) (types.AgentRun, error) {
	return types.AgentRun{ID: id, State: types.RunRunning, UpdatedAt: time.Now().UTC()}, nil
}

// terminalRunStore serves one run in a terminal state, `age` after its terminal
// transition.
type terminalRunStore struct {
	store.Store
	runID uuid.UUID
	state types.RunState
	age   time.Duration
}

func (s terminalRunStore) GetRun(_ context.Context, id uuid.UUID) (types.AgentRun, error) {
	if id != s.runID {
		return types.AgentRun{}, store.ErrNotFound
	}
	return types.AgentRun{ID: id, State: s.state, UpdatedAt: time.Now().UTC().Add(-s.age)}, nil
}

// internalDoors is every /internal/* route a run token can open, with a body
// that gets past decoding. The table is the point: a run-state re-check added to
// one door (handleInternalTokenRenew, say) and not its siblings leaves the
// surface open, so the pin has to be over the whole surface rather than over
// any one door.
func internalDoors(runID, grantID uuid.UUID) []struct {
	name, method, path, body string
} {
	return []struct{ name, method, path, body string }{
		{"decisions", http.MethodPost, "/api/v1/internal/decisions",
			`{"request":{"host":"api.anthropic.com","method":"POST"},"decision":"allow"}`},
		{"approvals request", http.MethodPost, "/api/v1/internal/approvals",
			`{"kind":"egress_domain","host":"evil.example"}`},
		{"approvals get", http.MethodGet, "/api/v1/internal/approvals/" + uuid.New().String(), ""},
		{"approvals expire", http.MethodPost, "/api/v1/internal/approvals/" + uuid.New().String() + "/expire", ""},
		{"credentials mint", http.MethodPost, "/api/v1/internal/credentials/mint",
			`{"grant_id":"` + grantID.String() + `"}`},
		{"injection resolve", http.MethodGet, "/api/v1/internal/injection/" + grantID.String(), ""},
		{"recording upload", http.MethodPut, "/api/v1/internal/recordings/" + runID.String(), "cast"},
		{"recording part upload", http.MethodPut, "/api/v1/internal/recordings/" + runID.String() + "/parts/2", "cast"},
		{"scan-result upload", http.MethodPut, "/api/v1/internal/scan-results/" + runID.String(), `{}`},
		{"sso-token upload", http.MethodPut, "/api/v1/internal/sso-token/" + runID.String(), `{}`},
		// EXEMPT from refuseTerminalRun (internalSelfGatedRoutes) and in the table
		// anyway: renew runs its own, stricter check, and the only structural proof
		// that the exemption is still safe is asserting the refusal is STILL 403
		// here — from renew's own path (R10). Gut that re-read and this row reds.
		{"token renew", http.MethodPost, "/api/v1/internal/token/renew", ""},
	}
}

// internalAuth verified signature, expiry, audience and the revocation list —
// but revokeRunCascade is best-effort (Identity.RevokeRun's error is audited and
// swallowed, Broker.RevokeRun is audit-only), so a run that went terminal while
// its revocation write failed kept presenting a token Verify accepts. Only
// handleInternalTokenRenew re-read the run's state; the mint door, the injection
// resolve door and the approval doors did not, and kept minting and injecting —
// including the operator's live Anthropic OAuth token — for up to tokenTTL after
// the run was killed.
func TestInternalAuth_TerminalRunIsRefusedAtEveryDoor(t *testing.T) {
	h := newHarness(t)
	runID, grantID := uuid.New(), uuid.New()
	cfg := baseTestConfig(h, terminalRunStore{runID: runID, state: types.RunKilled, age: terminalUploadGrace + time.Minute})
	cfg.Secrets = &memSecrets{m: map[string][]byte{}}
	cfg.RecordingStore = &fakeRecordingStore{}
	cfg.Approvals = h.approvals
	cfg.Broker = h.broker
	srv := New(cfg)
	tok := h.mintRunToken(t, runID)

	for _, d := range internalDoors(runID, grantID) {
		t.Run(d.name, func(t *testing.T) {
			w := do(t, srv, d.method, d.path, tok, d.body)
			if w.Code != http.StatusForbidden {
				t.Fatalf("code = %d, want 403 for a terminal run past the upload grace; body=%s", w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), "terminal") {
				t.Errorf("body = %s, want the refusal to say the run is terminal", w.Body.String())
			}
		})
	}
}

// TestInternalAuth_RenewRunNotFoundReasonIsPinned (#656 final review round
// FIX-3): renew's own GetRun call, exempt from refuseTerminalRun's earlier
// gate, on a run token store.GetRun cannot find at all. Asserts the LITERAL
// wire value, not the Go const, so a rename of reasonRunNotFound without
// updating docs/sdk.md fails here too.
func TestInternalAuth_RenewRunNotFoundReasonIsPinned(t *testing.T) {
	h := newHarness(t)
	knownRunID, missingRunID := uuid.New(), uuid.New()
	cfg := baseTestConfig(h, terminalRunStore{runID: knownRunID, state: types.RunRunning})
	srv := New(cfg)
	tok := h.mintRunToken(t, missingRunID)

	w := do(t, srv, http.MethodPost, "/api/v1/internal/token/renew", tok, "")
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (body %s)", w.Code, w.Body.String())
	}
	if got := errorReason(w); got != "run_not_found" {
		t.Errorf("reason = %q, want the literal %q", got, "run_not_found")
	}
}

// TestInternalAuth_TailUploadsLandInsideTheGrace is the asserted exemption.
// wardyn-rec, wardyn-scan and wardyn-aws-sso all PUT their artifact as the run
// finishes, racing the completion watcher that flips the state — so a gate with
// no grace would discard exactly the recordings and scan facts the run existed
// to produce. Every OTHER door stays shut in the same window.
func TestInternalAuth_TailUploadsLandInsideTheGrace(t *testing.T) {
	h := newHarness(t)
	runID, grantID := uuid.New(), uuid.New()
	cfg := baseTestConfig(h, terminalRunStore{runID: runID, state: types.RunCompleted, age: time.Second})
	cfg.Secrets = &memSecrets{m: map[string][]byte{}}
	cfg.RecordingStore = &fakeRecordingStore{}
	cfg.Approvals = h.approvals
	cfg.Broker = h.broker
	srv := New(cfg)
	tok := h.mintRunToken(t, runID)

	graced := map[string]bool{"recording upload": true, "recording part upload": true, "scan-result upload": true, "sso-token upload": true}
	for _, d := range internalDoors(runID, grantID) {
		t.Run(d.name, func(t *testing.T) {
			w := do(t, srv, d.method, d.path, tok, d.body)
			if graced[d.name] {
				if w.Code == http.StatusForbidden && strings.Contains(w.Body.String(), "terminal") {
					t.Fatalf("a tail upload inside the grace was refused: %s", w.Body.String())
				}
				return
			}
			if w.Code != http.StatusForbidden {
				t.Fatalf("code = %d, want 403 — the grace is for the tail uploads alone; body=%s", w.Code, w.Body.String())
			}
		})
	}
}

// keptRunStore serves one run kept by its end or a loss, marked `age` ago:
// RUNNING unless state says otherwise, updated `updatedAge` ago.
type keptRunStore struct {
	store.Store
	runID      uuid.UUID
	reason     types.LostReason
	age        time.Duration
	state      types.RunState
	updatedAge time.Duration
}

func (s keptRunStore) GetRun(_ context.Context, id uuid.UUID) (types.AgentRun, error) {
	if id != s.runID {
		return types.AgentRun{}, store.ErrNotFound
	}
	state := s.state
	if state == "" {
		state = types.RunRunning
	}
	lostAt := time.Now().UTC().Add(-s.age)
	return types.AgentRun{ID: id, State: state, LostAt: &lostAt, LostReason: s.reason,
		UpdatedAt: time.Now().UTC().Add(-s.updatedAge)}, nil
}

// TestInternalAuth_KeptRunIsRefusedAtEveryDoor is #1176: a run its lease ended
// (or one lost to a reboot or an outage) is still RUNNING, and its token is not
// revoked, so before this gate refused it the stopped proxy's token could still
// mint and resolve injections until its TTL lapsed. Every door refuses it; the
// tail uploads alone keep their grace, counted from the mark, and lose it
// after. UpdatedAt is fresh on purpose: the grace must not be measured from it.
func TestInternalAuth_KeptRunIsRefusedAtEveryDoor(t *testing.T) {
	graced := map[string]bool{"recording upload": true, "recording part upload": true, "scan-result upload": true, "sso-token upload": true}
	for _, reason := range []types.LostReason{types.LostEnded, types.LostReboot, types.LostOutage} {
		for _, age := range []time.Duration{time.Second, terminalUploadGrace + time.Minute} {
			h := newHarness(t)
			runID, grantID := uuid.New(), uuid.New()
			cfg := baseTestConfig(h, keptRunStore{runID: runID, reason: reason, age: age})
			cfg.Secrets = &memSecrets{m: map[string][]byte{}}
			cfg.RecordingStore = &fakeRecordingStore{}
			cfg.Approvals = h.approvals
			cfg.Broker = h.broker
			srv := New(cfg)
			tok := h.mintRunToken(t, runID)
			inGrace := age < terminalUploadGrace
			for _, d := range internalDoors(runID, grantID) {
				t.Run(string(reason)+"/"+age.String()+"/"+d.name, func(t *testing.T) {
					w := do(t, srv, d.method, d.path, tok, d.body)
					refused := w.Code == http.StatusForbidden && strings.Contains(w.Body.String(), "lost")
					if graced[d.name] && inGrace {
						if refused {
							t.Fatalf("a tail upload inside the grace was refused: %s", w.Body.String())
						}
						return
					}
					if !refused {
						t.Fatalf("code = %d body=%s, want 403 saying the run is lost", w.Code, w.Body.String())
					}
				})
			}
		}
	}
}

// TestInternalAuth_KeptThenTerminalRunIsRefusedAsTerminal: a kept run keeps
// its lost mark when it is later killed or torn down (only a revive clears
// it), so the gate must answer the terminal state first, auditing
// run_terminal with run_state as documented, never run_kept. Its tail-upload
// grace is the terminal one, from updated_at, not from the old mark.
func TestInternalAuth_KeptThenTerminalRunIsRefusedAsTerminal(t *testing.T) {
	h := newHarness(t)
	runID, grantID := uuid.New(), uuid.New()
	cfg := baseTestConfig(h, keptRunStore{runID: runID, reason: types.LostEnded, age: time.Hour,
		state: types.RunKilled, updatedAge: time.Second})
	cfg.Secrets = &memSecrets{m: map[string][]byte{}}
	cfg.RecordingStore = &fakeRecordingStore{}
	cfg.Approvals = h.approvals
	cfg.Broker = h.broker
	srv := New(cfg)
	tok := h.mintRunToken(t, runID)

	graced := map[string]bool{"recording upload": true, "recording part upload": true, "scan-result upload": true, "sso-token upload": true}
	for _, d := range internalDoors(runID, grantID) {
		t.Run(d.name, func(t *testing.T) {
			w := do(t, srv, d.method, d.path, tok, d.body)
			if graced[d.name] {
				if w.Code == http.StatusForbidden && (strings.Contains(w.Body.String(), "run is terminal") || strings.Contains(w.Body.String(), "run is lost")) {
					t.Fatalf("a tail upload inside the terminal grace was refused by the gate: %s", w.Body.String())
				}
				return
			}
			if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "terminal") {
				t.Fatalf("code = %d body=%s, want 403 saying the run is terminal", w.Code, w.Body.String())
			}
		})
	}
	var terminal, kept int
	for _, ev := range h.audit.snapshot() {
		if ev.Action != "authz.denied" {
			continue
		}
		switch {
		case strings.Contains(string(ev.Data), `"reason":"run_terminal"`) && strings.Contains(string(ev.Data), `"run_state":"KILLED"`):
			terminal++
		case strings.Contains(string(ev.Data), `"run_kept"`):
			kept++
		}
	}
	if terminal == 0 || kept != 0 {
		t.Errorf("authz.denied rows: run_terminal/KILLED %d, run_kept %d; want run_terminal only", terminal, kept)
	}
}

// TestInternalAuth_LiveRunIsUntouched is the negative control: a run that is
// still going must see byte-identical behaviour, gate or no gate.
func TestInternalAuth_LiveRunIsUntouched(t *testing.T) {
	h := newHarness(t)
	runID := uuid.New()
	srv := New(baseTestConfig(h, &touchStore{}))
	tok := h.mintRunToken(t, runID)

	w := do(t, srv, http.MethodPost, "/api/v1/internal/decisions", tok,
		`{"request":{"host":"api.anthropic.com","method":"POST"},"decision":"allow"}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("code = %d, want 202 for a live run; body=%s", w.Code, w.Body.String())
	}
}

// TestInternalUploadWithinGrace covers the predicate's own edges without a
// server: the window's far side, and the two "we cannot tell" inputs that must
// fail closed rather than widen it.
func TestInternalUploadWithinGrace(t *testing.T) {
	now := time.Now().UTC()
	for name, tc := range map[string]struct {
		path string
		run  types.AgentRun
		want bool
	}{
		"a recording just after the run ended":  {"/api/v1/internal/recordings/x", types.AgentRun{UpdatedAt: now.Add(-time.Second)}, true},
		"a recording long after the run ended":  {"/api/v1/internal/recordings/x", types.AgentRun{UpdatedAt: now.Add(-2 * terminalUploadGrace)}, false},
		"a mint is never graced":                {"/api/v1/internal/credentials/mint", types.AgentRun{UpdatedAt: now}, false},
		"a run row with no terminal timestamp":  {"/api/v1/internal/recordings/x", types.AgentRun{}, false},
		"a terminal timestamp in the future":    {"/api/v1/internal/recordings/x", types.AgentRun{UpdatedAt: now.Add(time.Hour)}, false},
		"a scan result just after the run ends": {"/api/v1/internal/scan-results/x", types.AgentRun{UpdatedAt: now}, true},
	} {
		t.Run(name, func(t *testing.T) {
			if got := internalUploadWithinGrace(tc.path, tc.run, now); got != tc.want {
				t.Errorf("internalUploadWithinGrace = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestInternalAuth_StoreErrorIsARetryable503WithNoDriverText is R7 + the
// store-error arm R10 asked for: the gate fails CLOSED on a read it could not
// make, says so retryably, and puts no driver text on the wire — the caller here
// is the in-sandbox proxy.
func TestInternalAuth_StoreErrorIsARetryable503WithNoDriverText(t *testing.T) {
	h := newHarness(t)
	runID := uuid.New()
	srv := New(baseTestConfig(h, errRunStore{err: errors.New("conn closed by peer host=10.0.0.5 db=wardyn")}))
	tok := h.mintRunToken(t, runID)

	w := do(t, srv, http.MethodPost, "/api/v1/internal/decisions", tok,
		`{"request":{"host":"api.anthropic.com","method":"POST"},"decision":"allow"}`)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("code = %d, want 503 — an unreadable run must refuse RETRYABLY, never admit; body=%s", w.Code, w.Body.String())
	}
	for _, leak := range []string{"10.0.0.5", "conn closed by peer", "db=wardyn"} {
		if strings.Contains(w.Body.String(), leak) {
			t.Errorf("body = %s, must not carry the driver's %q at the sandbox boundary", w.Body.String(), leak)
		}
	}
}

// TestInternalAuth_NoStoreAdmits is the nil-store arm (R10). A deployment with
// no run store has no run lifecycle to be past, and refusing would turn "no
// store" into "no internal surface" for every store-less embedding. Renew keeps
// its own 503 there, which is the stricter answer for a fresh grant of
// authority.
func TestInternalAuth_NoStoreAdmits(t *testing.T) {
	h := newHarness(t) // newHarness wires no Store
	if h.srv.cfg.Store != nil {
		t.Fatal("fixture now has a Store; this test is about the nil-store arm")
	}
	runID := uuid.New()
	tok := h.mintRunToken(t, runID)

	w := do(t, h.srv, http.MethodPost, "/api/v1/internal/decisions", tok,
		`{"request":{"host":"api.anthropic.com","method":"POST"},"decision":"allow"}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("code = %d, want 202; a store-less embedding must keep its internal surface; body=%s", w.Code, w.Body.String())
	}
}
