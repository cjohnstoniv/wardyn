// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// The device routes' boundaries beyond authentication: what a device context
// may never be mistaken for, how many failure rows the routes may write, and
// what never reaches a log or a row.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A device context is never an operator. deviceAuth publishes no human, and
// the predicates' no-human branch is the admin token — so they refuse a device
// FIRST, whichever file the handler asking was written in, and a device request
// is attributed to the device, never to "admin-token". The AST guard over
// devices_auth.go is the second line, not the only one.
func TestDevices_OperatorPredicatesRefuseADeviceContext(t *testing.T) {
	for _, withOIDC := range []bool{false, true} {
		t.Run(fmt.Sprintf("oidc=%v", withOIDC), func(t *testing.T) {
			srv, _, _ := newDeviceTestServer(t, withOIDC)
			ctx := context.WithValue(context.Background(), deviceCtxKey{}, types.Device{ID: uuid.New(), Name: "laptop"})
			if _, ok := deviceFromContext(ctx); !ok {
				t.Fatal("control: device is on the context")
			}
			if srv.isOperator(ctx) {
				t.Errorf("isOperator(device ctx) = true, want false: a device context reads as the SUPER admin")
			}
			if srv.isSecurityOperator(ctx) {
				t.Errorf("isSecurityOperator(device ctx) = true, want false")
			}
			r := httptest.NewRequest(http.MethodPost, "/api/v1/devices/x/audit", nil).WithContext(ctx)
			if got := principalFromRequest(r); got == adminTokenPrincipal {
				t.Errorf("principalFromRequest(device request) = %q: a device would be audited as the admin token", got)
			}
			// Router level: a handler mounted behind deviceAuth that asks for
			// the operator tier is refused rather than answered as the admin.
			id, tok := enrolTestDevice(t, srv, "laptop")
			rt := chi.NewRouter()
			rt.With(srv.deviceAuth, srv.requireOperator).Get("/devices/{id}/probe", func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
			})
			rq := httptest.NewRequest(http.MethodGet, "/devices/"+id.String()+"/probe", nil)
			rq.Header.Set("Authorization", "Bearer "+tok)
			rw := httptest.NewRecorder()
			rt.ServeHTTP(rw, rq)
			if rw.Code != http.StatusForbidden {
				t.Errorf("operator-gated handler behind deviceAuth answered a device token %d, want 403", rw.Code)
			}
		})
	}
}

// The anonymous enrol route's failure rows are coalesced like auth.fail's.
// Ten rotating peers stay under the per-peer limiter; without the fold they
// would write one row per second forever — 86,400 a day into the append-only
// chain from an unauthenticated caller — and spend auth.fail's budget too.
func TestDevices_EnrolFailureDripIsCoalesced(t *testing.T) {
	ast := newAuthzStore()
	rec := &safeRecorder{}
	cfg := baseTestConfig(newHarness(t), ast)
	cfg.Audit = rec
	cfg.AuditCoalesceWindow = 5 * time.Minute
	clock := time.Unix(1_800_000_000, 0).UTC()
	cfg.Now = func() time.Time { return clock }
	srv := New(cfg)

	const seconds = 120
	for i := 0; i < seconds; i++ {
		clock = clock.Add(time.Second)
		peer := fmt.Sprintf("198.51.100.%d:4000", i%10)
		w := doPeer(t, srv, http.MethodPost, "/api/v1/devices/enrol", "", `{"token":"wde_guess"}`, peer)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("enrol #%d from %s: %d, want 401 (limiter must not trip at 1/s over 10 peers): %s", i, peer, w.Code, w.Body.String())
		}
		// The same drip on the public routes, for comparison.
		if w := doPeer(t, srv, http.MethodGet, "/api/v1/me", "not-the-admin-token", "", peer); w.Code != http.StatusUnauthorized {
			t.Fatalf("control: bad admin token = %d", w.Code)
		}
	}
	enrol := auditRows(rec, "device.enrol", "failure")
	authFailed := auditRows(rec, "auth.fail", "failure")
	t.Logf("%d simulated seconds: device.enrol failure rows = %d, auth.fail rows (coalesced) = %d", seconds, len(enrol), len(authFailed))
	if len(enrol) > len(authFailed)+2 {
		t.Errorf("device.enrol failure rows = %d over %d s; auth.fail on the same drip = %d — the anonymous enrol drip is not coalesced", len(enrol), seconds, len(authFailed))
	}
}

// device.audit.ingest failure rows are bounded per device: a laptop sending
// refused pushes as fast as it likes writes a handful of rows, not one per
// request. auth.fail on the same server caps at its burst.
func TestDevices_IngestFailureRowsAreBoundedPerDevice(t *testing.T) {
	srv, _, rec := newDeviceTestServer(t, false)
	id, tok := enrolTestDevice(t, srv, "laptop")
	const n = 300
	for i := 0; i < n; i++ {
		w := do(t, srv, http.MethodPost, "/api/v1/devices/"+id.String()+"/audit", tok, `{"not":"an array"}`)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("#%d: %d, want 400: %s", i, w.Code, w.Body.String())
		}
		do(t, srv, http.MethodPost, "/api/v1/devices/"+id.String()+"/audit", "wdd_wrong", "[]")
	}
	ingest := auditRows(rec, "device.audit.ingest", "failure")
	authFailed := auditRows(rec, "auth.fail", "failure")
	t.Logf("%d refused pushes -> device.audit.ingest failure rows = %d; %d bad tokens -> auth.fail rows = %d", n, len(ingest), n, len(authFailed))
	if len(ingest) >= n {
		t.Errorf("device.audit.ingest failure rows = %d for %d requests: unbounded per-request writes into the append-only log", len(ingest), n)
	}

	// With the fold on (the boot default), one refused batch replayed is ONE
	// streak: its opening row now, and a summary carrying the count when the
	// streak closes — here at shutdown.
	t.Run("coalesced", func(t *testing.T) {
		st := newAuthzStore()
		rec := &safeRecorder{}
		cfg := baseTestConfig(newHarness(t), st)
		cfg.Audit = rec
		cfg.AuditCoalesceWindow = 5 * time.Minute
		srv := New(cfg)
		id, tok := enrolTestDevice(t, srv, "laptop")
		for range n {
			do(t, srv, http.MethodPost, "/api/v1/devices/"+id.String()+"/audit", tok, `{"not":"an array"}`)
		}
		if got := len(auditRows(rec, "device.audit.ingest", "failure")); got != 1 {
			t.Fatalf("%d identical refusals wrote %d rows, want the opening row only", n, got)
		}
		srv.FlushAuthFailedStreak()
		rows := auditRows(rec, "device.audit.ingest", "failure")
		if len(rows) != 2 {
			t.Fatalf("after the streak closed: %d rows, want the opening row and one summary", len(rows))
		}
		var data map[string]any
		_ = json.Unmarshal(rows[1].Data, &data)
		if data["count"] != float64(n) || data["reason"] != "invalid_body" || rows[1].Actor != deviceActor(id) {
			t.Fatalf("summary = %s by %s, want count %d, reason invalid_body, the device as actor", rows[1].Data, rows[1].Actor, n)
		}
	})
}

// A device revoked between deviceAuth and the ingest transaction is answered
// as revoked — the 401 its next request gets — and recorded with reason
// "revoked", never as a chain mismatch.
type revokeDuringIngest struct{ *authzStore }

func (s revokeDuringIngest) IngestDeviceAudit(ctx context.Context, id uuid.UUID, peer string, rows []types.FederatedAuditEvent) (store.DeviceIngestResult, error) {
	if _, err := s.authzStore.RevokeDevice(ctx, id, time.Now().UTC()); err != nil {
		return store.DeviceIngestResult{}, err
	}
	return s.authzStore.IngestDeviceAudit(ctx, id, peer, rows)
}

func TestDevices_RevokedBetweenAuthAndIngestIsReportedAsRevoked(t *testing.T) {
	rs := revokeDuringIngest{newAuthzStore()}
	rec := &safeRecorder{}
	cfg := baseTestConfig(newHarness(t), rs)
	cfg.Audit = rec
	srv := New(cfg)
	id, tok := enrolTestDevice(t, srv, "laptop")
	body, _ := json.Marshal(chainRows(1, 2, ""))
	w := do(t, srv, http.MethodPost, "/api/v1/devices/"+id.String()+"/audit", tok, string(body))
	rows := auditRows(rec, "device.audit.ingest", "failure")
	reason := ""
	if len(rows) > 0 {
		reason = auditReason(t, rows[0])
	}
	t.Logf("status = %d body = %s; failure row reason = %q", w.Code, strings.TrimSpace(w.Body.String()), reason)
	if w.Code == http.StatusUnprocessableEntity || reason == "chain_mismatch" {
		t.Errorf("revoked-during-ingest answered %d with reason %q — should read as revoked (401), not as a chain mismatch", w.Code, reason)
	}
	// Control: the next request IS 401.
	if w := do(t, srv, http.MethodPost, "/api/v1/devices/"+id.String()+"/heartbeat", tok, ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("control: next request after revoke = %d, want 401", w.Code)
	}
}

// Raw tokens and their hashes never reach the log or an audit row, on the
// mint, enrol, replayed-token, bad-token and store-error paths.
func TestDevices_SecretsNeverReachLogsOrAuditRows(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	srv, ast, rec := newDeviceTestServer(t, false)
	enrolTok := mintEnrolmentToken(t, srv, "laptop")
	w := doPeer(t, srv, http.MethodPost, "/api/v1/devices/enrol", "", `{"token":"`+enrolTok+`"}`, "203.0.113.10:4000")
	var got deviceEnrolResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || w.Code != http.StatusCreated {
		t.Fatalf("enrol: %d %s", w.Code, w.Body.String())
	}
	// Failure paths that log: a store error on ingest, a bad token, a second redemption.
	ast.ingestErr = fmt.Errorf("boom: %s", "synthetic")
	do(t, srv, http.MethodPost, "/api/v1/devices/"+got.DeviceID.String()+"/audit", got.Token, "[]")
	ast.ingestErr = nil
	doPeer(t, srv, http.MethodPost, "/api/v1/devices/enrol", "", `{"token":"`+enrolTok+`"}`, "203.0.113.10:4000")
	do(t, srv, http.MethodPost, "/api/v1/devices/"+got.DeviceID.String()+"/audit", got.Token+"x", "[]")

	sum := func(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }
	secrets := map[string]string{"raw enrolment token": enrolTok, "raw device token": got.Token,
		"sha256(enrolment token)": sum(enrolTok), "sha256(device token)": sum(got.Token)}
	rec.mu.Lock()
	var auditText strings.Builder
	for _, ev := range rec.events {
		b, _ := json.Marshal(ev)
		auditText.Write(b)
	}
	rec.mu.Unlock()
	for name, s := range secrets {
		if strings.Contains(logs.String(), s) {
			t.Errorf("%s appears in slog output", name)
		}
		if strings.Contains(auditText.String(), s) {
			t.Errorf("%s appears in an audit row", name)
		}
	}
	t.Logf("checked %d log bytes and %d audit rows for 4 secret strings", logs.Len(), len(rec.events))
}

// Deeply nested JSON is refused promptly, and a full 500-row batch under the
// 8 MiB cap is answered, not hung.
func TestDevices_DeepJSONAndFullBatchAreAnswered(t *testing.T) {
	srv, _, _ := newDeviceTestServer(t, false)
	id, tok := enrolTestDevice(t, srv, "laptop")
	deep := `[{"seq":1,"id":"` + uuid.NewString() + `","time":"2026-01-01T00:00:00Z","actor_type":"human","actor":"a","action":"x","outcome":"success","data":` +
		strings.Repeat("[", 300000) + strings.Repeat("]", 300000) + `}]`
	start := time.Now()
	w := do(t, srv, http.MethodPost, "/api/v1/devices/"+id.String()+"/audit", tok, deep)
	t.Logf("deep JSON (600 KB, depth 300000): %d in %s", w.Code, time.Since(start))
	if w.Code != http.StatusBadRequest || time.Since(start) > 5*time.Second {
		t.Errorf("deep JSON: %d in %s, want 400 promptly", w.Code, time.Since(start))
	}
	// 500 rows x ~16 KiB data, under the 8 MiB cap.
	rows := chainRows(1, 500, "")
	pad := strings.Repeat("x", 16000)
	for i := range rows {
		rows[i].Data = json.RawMessage(`{"pad":"` + pad + `"}`)
	}
	body, _ := json.Marshal(rows)
	start = time.Now()
	w = do(t, srv, http.MethodPost, "/api/v1/devices/"+id.String()+"/audit", tok, string(body))
	t.Logf("full batch (%d bytes, 500 rows): %d in %s", len(body), w.Code, time.Since(start))
	if w.Code != http.StatusOK {
		t.Errorf("full batch: %d, want 200: %.200s", w.Code, w.Body.String())
	}
}

// A wdd_ bearer is refused on /metrics, and on the human surface in LocalMode
// from a non-loopback peer; the device routes ignore LocalMode entirely.
func TestDevices_DeviceTokenOnMetricsAndLocalMode(t *testing.T) {
	srv, _, _ := newDeviceTestServer(t, false)
	id, tok := enrolTestDevice(t, srv, "laptop")
	if w := do(t, srv, http.MethodGet, "/metrics", tok, ""); w.Code != http.StatusUnauthorized {
		t.Errorf("device token on /metrics = %d, want 401", w.Code)
	}
	ast := newAuthzStore()
	cfg := baseTestConfig(newHarness(t), ast)
	cfg.LocalMode = true
	lm := New(cfg)
	for _, p := range []string{"/api/v1/me", "/api/v1/admin/devices", "/api/v1/runs"} {
		if w := doPeer(t, lm, http.MethodGet, p, tok, "", "203.0.113.9:1"); w.Code != http.StatusUnauthorized && w.Code != http.StatusForbidden {
			t.Errorf("LocalMode, non-loopback peer, device token on %s = %d, want 401/403", p, w.Code)
		}
	}
	// deviceAuth ignores LocalMode: a device route still needs the wdd_ bearer.
	if w := doPeer(t, lm, http.MethodPost, "/api/v1/devices/"+id.String()+"/heartbeat", "", "", "127.0.0.1:1"); w.Code != http.StatusUnauthorized {
		t.Errorf("LocalMode loopback, no bearer, device route = %d, want 401", w.Code)
	}
}

// blockingIngestStore parks every IngestDeviceAudit until released, so a test
// can hold one push in flight and see what a second one gets.
type blockingIngestStore struct {
	*authzStore
	entered chan uuid.UUID
	release chan struct{}
}

func (s blockingIngestStore) IngestDeviceAudit(ctx context.Context, id uuid.UUID, peer string, rows []types.FederatedAuditEvent) (store.DeviceIngestResult, error) {
	s.entered <- id
	<-s.release
	return s.authzStore.IngestDeviceAudit(ctx, id, peer, rows)
}

// One push per device at a time: while a device's push is in the store, its
// second concurrent push is 429 with Retry-After and never reaches the store
// (where the hash recompute would hold a pool connection); another device is
// unaffected, and the cap lifts when the first push completes.
func TestDevices_OneIngestInFlightPerDevice(t *testing.T) {
	bs := blockingIngestStore{authzStore: newAuthzStore(), entered: make(chan uuid.UUID, 8), release: make(chan struct{})}
	cfg := baseTestConfig(newHarness(t), bs)
	cfg.Audit = &safeRecorder{}
	srv := New(cfg)
	idA, tokA := enrolTestDevice(t, srv, "a")
	idB, tokB := enrolTestDevice(t, srv, "b")
	push := func(id uuid.UUID, tok string, seq int64, prev string) chan *httptest.ResponseRecorder {
		out := make(chan *httptest.ResponseRecorder, 1)
		go func() {
			out <- do(t, srv, http.MethodPost, "/api/v1/devices/"+id.String()+"/audit", tok, string(mustJSON(chainRows(seq, 1, prev))))
		}()
		return out
	}
	first := push(idA, tokA, 1, "")
	if got := <-bs.entered; got != idA {
		t.Fatalf("store entered for %s, want %s", got, idA)
	}
	second := push(idA, tokA, 1, "")
	select {
	case w := <-second:
		if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") == "" {
			t.Fatalf("second concurrent push from one device = %d (Retry-After %q), want 429 with Retry-After: %s",
				w.Code, w.Header().Get("Retry-After"), w.Body.String())
		}
	case id := <-bs.entered:
		close(bs.release)
		t.Fatalf("a second concurrent push from device %s reached the store", id)
	case <-time.After(5 * time.Second):
		t.Fatal("second concurrent push neither answered nor reached the store")
	}
	other := push(idB, tokB, 1, "")
	select {
	case got := <-bs.entered:
		if got != idB {
			t.Fatalf("store entered for %s, want %s", got, idB)
		}
	case w := <-other:
		t.Fatalf("another device's push was refused while device A's was in flight: %d", w.Code)
	}
	close(bs.release)
	if w := <-first; w.Code != http.StatusOK {
		t.Fatalf("first push = %d: %s", w.Code, w.Body.String())
	}
	if w := <-other; w.Code != http.StatusOK {
		t.Fatalf("other device's push = %d: %s", w.Code, w.Body.String())
	}
	if w := <-push(idA, tokA, 2, "h1"); w.Code != http.StatusOK {
		t.Fatalf("a push after the first completed = %d, want 200 (the cap lifts): %s", w.Code, w.Body.String())
	}
}

// A claim the organisation could only store by changing it — so the device's
// hash would no longer recompute from the stored row — is a 400 invalid_row
// before any database work (store.FederatedRowProblem, which the store also
// enforces).
func TestDevices_IngestRefusesClaimsThatCannotReCheck(t *testing.T) {
	srv, st, rec := newDeviceTestServer(t, false)
	srv.ingestFailureLimiter.burst, srv.ingestFailureLimiter.rate = 1000, 1000
	id, tok := enrolTestDevice(t, srv, "laptop")
	for name, mutate := range map[string]func(*types.FederatedAuditEvent){
		"a device_origin key":   func(r *types.FederatedAuditEvent) { r.Data = json.RawMessage(`{"device_origin":{"device_id":"x"}}`) },
		"array data":            func(r *types.FederatedAuditEvent) { r.Data = json.RawMessage(`[1,2]`) },
		"scalar data":           func(r *types.FederatedAuditEvent) { r.Data = json.RawMessage(`42`) },
		"a target over the cap": func(r *types.FederatedAuditEvent) { r.Target = strings.Repeat("t", store.MaxAuditTargetLen+1) },
		"a claimed device_id":   func(r *types.FederatedAuditEvent) { other := uuid.New(); r.DeviceID = &other },
	} {
		t.Run(name, func(t *testing.T) {
			rows := chainRows(1, 1, "")
			mutate(&rows[0])
			w := do(t, srv, http.MethodPost, "/api/v1/devices/"+id.String()+"/audit", tok, string(mustJSON(rows)))
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
			}
			fails := auditRows(rec, "device.audit.ingest", "failure")
			if len(fails) == 0 || auditReason(t, fails[len(fails)-1]) != "invalid_row" {
				t.Fatalf("want an invalid_row failure row, have %d rows", len(fails))
			}
			if st.ingestedFor(id) != 0 {
				t.Fatal("a refused claim reached the store")
			}
		})
	}
	// JSON null and absent data are what data-less laptop rows hold: accepted.
	rows := chainRows(1, 2, "")
	rows[0].Data = json.RawMessage(`null`)
	if got := ackedSeq(t, do(t, srv, http.MethodPost, "/api/v1/devices/"+id.String()+"/audit", tok, string(mustJSON(rows)))); got != 2 {
		t.Fatalf("acked %d, want 2", got)
	}
}

// A claimed value the store cannot represent is the device's fault: 400
// invalid_row, which the forwarder stops on — never the 500 it would retry
// forever. (The store half is TestPG_Devices_UnstorableClaimedValueIsErrFederatedRowInvalid.)
func TestDevices_UnstorableClaimedValueIsA400NotARetry(t *testing.T) {
	srv, st, rec := newDeviceTestServer(t, false)
	id, tok := enrolTestDevice(t, srv, "laptop")
	st.mu.Lock()
	st.ingestErr = fmt.Errorf("store: a claimed value cannot be stored (SQLSTATE 22P05): %w", store.ErrFederatedRowInvalid)
	st.mu.Unlock()
	w := do(t, srv, http.MethodPost, "/api/v1/devices/"+id.String()+"/audit", tok, string(mustJSON(chainRows(1, 1, ""))))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
	}
	fails := auditRows(rec, "device.audit.ingest", "failure")
	if len(fails) != 1 || auditReason(t, fails[0]) != "invalid_row" {
		t.Fatalf("failure rows = %+v, want one with reason invalid_row", fails)
	}
}

// A device cycling two refusal reasons opens a new streak on every switch, so
// every refusal is an opening row — bounded by the per-device bucket (5, then
// one a minute) all the same.
func TestDevices_IngestFailureRowsBoundedWhenReasonsAlternate(t *testing.T) {
	st := newAuthzStore()
	rec := &safeRecorder{}
	cfg := baseTestConfig(newHarness(t), st)
	cfg.Audit = rec
	cfg.AuditCoalesceWindow = 5 * time.Minute
	clock := time.Unix(1_800_000_000, 0).UTC()
	cfg.Now = func() time.Time { return clock }
	srv := New(cfg)
	id, tok := enrolTestDevice(t, srv, "laptop")
	big, _ := json.Marshal(chainRows(1, 501, ""))
	for i := range 600 {
		clock = clock.Add(time.Second)
		body := `{"not":"an array"}`
		if i%2 == 1 {
			body = string(big)
		}
		do(t, srv, http.MethodPost, "/api/v1/devices/"+id.String()+"/audit", tok, body)
	}
	srv.FlushAuthFailedStreak()
	rows := auditRows(rec, "device.audit.ingest", "failure")
	t.Logf("600 refusals alternating invalid_body/batch_too_large over 600 simulated seconds: %d failure rows", len(rows))
	if len(rows) > 5+10+2 {
		t.Errorf("per-device bound exceeded: %d rows (bucket 5 + 10 minutes of refill)", len(rows))
	}
}
