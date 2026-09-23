// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package federation

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// memStore is the local audit table and the org_federation row.
type memStore struct {
	mu         sync.Mutex
	rows       []types.FederatedAuditEvent
	cursor     int64
	cursorHash string
	gen        int // bumped by a table reset, so a reused seq carries a new hash
	revoked    bool
}

func (m *memStore) FederationRevoked(context.Context) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.revoked, nil
}

func (m *memStore) MarkFederationRevoked(context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.revoked = true
	return nil
}

func (m *memStore) ResetFederation(context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cursor, m.cursorHash, m.revoked = 0, "", false
	return nil
}

// resetTable is TRUNCATE ... RESTART IDENTITY on the local table: the rows are
// gone and seq starts again at 1, under a kept cursor.
func (m *memStore) resetTable() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rows, m.gen = nil, m.gen+1
}

func (m *memStore) add(n int, chained bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for range n {
		seq := int64(len(m.rows) + 1)
		e := types.FederatedAuditEvent{Seq: seq}
		e.ID, e.Action, e.Outcome, e.ActorType = uuid.New(), "run.create", "success", types.ActorSystem
		if chained {
			e.RowHash = "h" + strconv.Itoa(m.gen) + "." + strconv.FormatInt(seq, 10)
		}
		m.rows = append(m.rows, e)
	}
}

func (m *memStore) ListAuditEventsAfterSeq(_ context.Context, seq int64, limit int) ([]types.FederatedAuditEvent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []types.FederatedAuditEvent
	for _, r := range m.rows {
		if r.Seq > seq && len(out) < limit {
			out = append(out, r)
		}
	}
	return out, nil
}

func (m *memStore) GetFederationCursor(context.Context) (int64, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cursor, m.cursorHash, nil
}

func (m *memStore) SetFederationCursor(_ context.Context, seq int64, rowHash string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cursor, m.cursorHash = seq, rowHash
	return nil
}

func (m *memStore) AuditHeadSeq(context.Context) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return int64(len(m.rows)), nil
}

type memRecorder struct{ events []types.AuditEvent }

func (r *memRecorder) Record(_ context.Context, ev types.AuditEvent) error {
	r.events = append(r.events, ev)
	return nil
}

// orgStub is an httptest organisation. answer, when set, overrides the
// ingest/heartbeat reply; otherwise a push acks its last row.
type orgStub struct {
	mu         sync.Mutex
	acked      int64
	pushes     [][]types.FederatedAuditEvent
	heartbeats int
	auth       string
	answer     func(w http.ResponseWriter) bool
}

func (o *orgStub) serve(t *testing.T, id uuid.UUID) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		o.mu.Lock()
		defer o.mu.Unlock()
		o.auth = r.Header.Get("Authorization")
		if o.answer != nil && o.answer(w) {
			return
		}
		switch r.URL.Path {
		case "/api/v1/devices/" + id.String() + "/audit":
			var rows []types.FederatedAuditEvent
			if err := json.NewDecoder(r.Body).Decode(&rows); err != nil {
				t.Errorf("decode push: %v", err)
			}
			o.pushes = append(o.pushes, rows)
			o.acked = rows[len(rows)-1].Seq
		case "/api/v1/devices/" + id.String() + "/heartbeat":
			o.heartbeats++
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(types.DeviceAck{AckedSeq: o.acked})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newTestForwarder(url string, st *memStore, rec *memRecorder) (*Forwarder, Credential) {
	cred := Credential{DeviceID: uuid.New(), Token: "wdd_test"}
	return NewForwarder(NewClient(url), st, cred, rec), cred
}

func TestForwarder_CursorAdvancesInBatchesAndSurvivesRestart(t *testing.T) {
	st, rec := &memStore{}, &memRecorder{}
	st.add(1200, true)
	cred := Credential{DeviceID: uuid.New(), Token: "wdd_test"}
	org := &orgStub{}
	url := org.serve(t, cred.DeviceID).URL
	f := NewForwarder(NewClient(url), st, cred, rec)
	ctx := context.Background()

	for i, wantWait := range []time.Duration{0, 0, tickInterval} {
		wait, stop := f.step(ctx)
		if stop || wait != wantWait {
			t.Fatalf("step %d: wait=%v stop=%v, want %v", i, wait, stop, wantWait)
		}
	}
	if len(org.pushes) != 3 || len(org.pushes[0]) != batchSize || len(org.pushes[2]) != 200 {
		t.Fatalf("pushes = %d batches", len(org.pushes))
	}
	if org.auth != "Bearer wdd_test" {
		t.Errorf("Authorization = %q", org.auth)
	}
	if st.cursor != 1200 || f.Status().AckedSeq != 1200 || f.Status().Lag() != 0 {
		t.Fatalf("cursor=%d status=%+v", st.cursor, f.Status())
	}

	// A restart: a fresh forwarder over the same table resumes at the durable
	// cursor, re-sends nothing and only heartbeats; new rows go next.
	g := NewForwarder(NewClient(url), st, cred, rec)
	g.step(ctx)
	if len(org.pushes) != 3 || org.heartbeats != 1 || g.Status().AckedSeq != 1200 {
		t.Fatalf("after restart: pushes=%d heartbeats=%d status=%+v", len(org.pushes), org.heartbeats, g.Status())
	}
	st.add(2, true)
	g.step(ctx)
	if len(org.pushes) != 4 || org.pushes[3][0].Seq != 1201 || st.cursor != 1202 {
		t.Fatalf("after new rows: pushes=%d cursor=%d", len(org.pushes), st.cursor)
	}
}

func TestForwarder_OrgDownRowsAccrueAndLagGrows(t *testing.T) {
	st, rec := &memStore{}, &memRecorder{}
	st.add(3, true)
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close() // connection refused from here on
	f, _ := newTestForwarder(srv.URL, st, rec)
	ctx := context.Background()

	var waits []time.Duration
	for i := range 6 {
		st.add(10, true)
		wait, stop := f.step(ctx)
		if stop {
			t.Fatal("an unreachable organisation must never read as revoked")
		}
		waits = append(waits, wait)
		s := f.Status()
		if s.AckedSeq != 0 || s.HeadSeq != int64(3+10*(i+1)) || s.Lag() != s.HeadSeq || s.LastError == "" {
			t.Fatalf("step %d: status = %+v", i, s)
		}
	}
	want := []time.Duration{15 * time.Second, 30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute, maxBackoff}
	for i := range want {
		if waits[i] != want[i] {
			t.Fatalf("backoff = %v, want %v", waits, want)
		}
	}
	if st.cursor != 0 {
		t.Fatalf("cursor moved while the organisation was down: %d", st.cursor)
	}
}

func TestForwarder_RevokedOn401And410(t *testing.T) {
	for _, code := range []int{http.StatusUnauthorized, http.StatusGone} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			st, rec := &memStore{}, &memRecorder{}
			st.add(2, true)
			cred := Credential{DeviceID: uuid.New(), Token: "wdd_test"}
			org := &orgStub{answer: func(w http.ResponseWriter) bool { w.WriteHeader(code); return true }}
			f := NewForwarder(NewClient(org.serve(t, cred.DeviceID).URL), st, cred, rec)
			if _, stop := f.step(context.Background()); !stop {
				t.Fatal("a revocation must stop the forwarder")
			}
			if s := f.Status(); !s.Revoked || s.AckedSeq != 0 {
				t.Fatalf("status = %+v", s)
			}
			if len(rec.events) != 1 || rec.events[0].Action != "device.local.revoke" || rec.events[0].Target != cred.DeviceID.String() {
				t.Fatalf("local rows = %+v", rec.events)
			}
		})
	}
}

func TestForwarder_NeverRetriesA400(t *testing.T) {
	st, rec := &memStore{}, &memRecorder{}
	st.add(2, true)
	cred := Credential{DeviceID: uuid.New(), Token: "wdd_test"}
	calls := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls[r.URL.Path]++
		if r.URL.Path == "/api/v1/devices/"+cred.DeviceID.String()+"/audit" {
			http.Error(w, `{"error":"row 0: invalid outcome"}`, http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(types.DeviceAck{})
	}))
	t.Cleanup(srv.Close)
	f := NewForwarder(NewClient(srv.URL), st, cred, rec)
	ctx := context.Background()
	for range 3 {
		if _, stop := f.step(ctx); stop {
			t.Fatal("a 400 is not a revocation")
		}
	}
	audit := "/api/v1/devices/" + cred.DeviceID.String() + "/audit"
	heartbeat := "/api/v1/devices/" + cred.DeviceID.String() + "/heartbeat"
	if calls[audit] != 1 || calls[heartbeat] != 2 {
		t.Fatalf("calls = %v: a refused batch was re-sent, or the halted forwarder stopped heart-beating", calls)
	}
	if s := f.Status(); s.LastError == "" || s.AckedSeq != 0 || s.Lag() != 2 {
		t.Fatalf("status = %+v", s)
	}
}

func TestForwarder_HonoursRetryAfter(t *testing.T) {
	for _, tc := range []struct {
		code  int
		after string
		want  time.Duration
	}{
		{http.StatusTooManyRequests, "7", 7 * time.Second},
		{http.StatusServiceUnavailable, "120", 120 * time.Second},
		{http.StatusServiceUnavailable, "86400", maxBackoff},
		{http.StatusTooManyRequests, time.Now().Add(90 * time.Second).UTC().Format(http.TimeFormat), 90 * time.Second},
	} {
		st, rec := &memStore{}, &memRecorder{}
		st.add(1, true)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Retry-After", tc.after)
			w.WriteHeader(tc.code)
		}))
		f, _ := newTestForwarder(srv.URL, st, rec)
		wait, stop := f.step(context.Background())
		srv.Close()
		// The HTTP-date form is second-granular and read a moment later.
		if stop || wait > tc.want || wait < tc.want-2*time.Second {
			t.Errorf("%d Retry-After %s: wait=%v stop=%v, want %v", tc.code, tc.after, wait, stop, tc.want)
		}
	}
}

func TestForwarder_SkipsRowsThatPredateTheChain(t *testing.T) {
	st, rec := &memStore{}, &memRecorder{}
	st.add(3, false)
	st.add(2, true)
	cred := Credential{DeviceID: uuid.New(), Token: "wdd_test"}
	org := &orgStub{}
	f := NewForwarder(NewClient(org.serve(t, cred.DeviceID).URL), st, cred, rec)
	f.step(context.Background())
	if len(org.pushes) != 1 || len(org.pushes[0]) != 2 || org.pushes[0][0].Seq != 4 || st.cursor != 5 {
		t.Fatalf("pushes=%v cursor=%d", org.pushes, st.cursor)
	}
}

// TestForwarder_RevocationSurvivesRestart: a revocation the forwarder saw is
// durable. A fresh forwarder over the same store — a restart, with the
// organisation now unreachable — starts revoked and never calls the org.
func TestForwarder_RevocationSurvivesRestart(t *testing.T) {
	st, rec := &memStore{}, &memRecorder{}
	st.add(2, true)
	cred := Credential{DeviceID: uuid.New(), Token: "wdd_test"}
	org := &orgStub{answer: func(w http.ResponseWriter) bool { w.WriteHeader(http.StatusUnauthorized); return true }}
	srv := org.serve(t, cred.DeviceID)
	f := NewForwarder(NewClient(srv.URL), st, cred, rec)
	if _, stop := f.step(context.Background()); !stop || !f.Status().Revoked {
		t.Fatal("setup: expected revocation")
	}
	srv.Close()
	g := NewForwarder(NewClient(srv.URL), st, cred, rec)
	if _, stop := g.step(context.Background()); !stop || !g.Status().Revoked {
		t.Fatalf("after a restart with the org unreachable: stop=%v status=%+v — the revocation was forgotten", stop, g.Status())
	}
	if len(rec.events) != 1 {
		t.Fatalf("a restart re-recorded the revocation: %d local rows", len(rec.events))
	}
}

// TestForwarder_429WithoutUsableRetryAfterBacksOff: an intermediary's 429 with
// no Retry-After, or one that does not parse, backs off like any other retry
// rather than hammering the org every second.
func TestForwarder_429WithoutUsableRetryAfterBacksOff(t *testing.T) {
	for _, after := range []string{"", "soon"} {
		st, rec := &memStore{}, &memRecorder{}
		st.add(1, true)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			if after != "" {
				w.Header().Set("Retry-After", after)
			}
			w.WriteHeader(http.StatusTooManyRequests)
		}))
		f, _ := newTestForwarder(srv.URL, st, rec)
		var waits []time.Duration
		for range 3 {
			wait, stop := f.step(context.Background())
			if stop {
				t.Fatal("a 429 is not a revocation")
			}
			waits = append(waits, wait)
		}
		srv.Close()
		if want := []time.Duration{15 * time.Second, 30 * time.Second, time.Minute}; !slices.Equal(waits, want) {
			t.Errorf("Retry-After %q: waits = %v, want %v", after, waits, want)
		}
	}
}

// TestForwarder_HeadBelowCursorResendsFromTheStart: a local table truncated or
// restored under a kept cursor is resent from seq 0 instead of being silently
// never forwarded with lag 0.
func TestForwarder_HeadBelowCursorResendsFromTheStart(t *testing.T) {
	st, rec := &memStore{cursor: 500}, &memRecorder{}
	st.add(10, true)
	cred := Credential{DeviceID: uuid.New(), Token: "wdd_test"}
	org := &orgStub{}
	f := NewForwarder(NewClient(org.serve(t, cred.DeviceID).URL), st, cred, rec)
	f.step(context.Background())
	if len(org.pushes) != 1 || len(org.pushes[0]) != 10 || org.pushes[0][0].Seq != 1 || st.cursor != 10 {
		t.Fatalf("pushes=%d cursor=%d status=%+v", len(org.pushes), st.cursor, f.Status())
	}
}

// TestForwarder_AResetThatRefillsTheCursorResendsFromGenesis: a local table
// reset that restarts seq (TRUNCATE ... RESTART IDENTITY, a restore) and then
// writes up to or past the old cursor leaves a row at the cursor's seq that the
// organisation never saw. The forwarder notices by that row's hash and resends
// from the new genesis, rather than heart-beating over the new rows (head ==
// cursor) or pushing rows that link to nothing the organisation holds, which
// it refuses 422 and the forwarder halts on (head past the cursor).
func TestForwarder_AResetThatRefillsTheCursorResendsFromGenesis(t *testing.T) {
	for _, refill := range []int{3, 5} {
		t.Run(strconv.Itoa(refill), func(t *testing.T) {
			st, rec := &memStore{}, &memRecorder{}
			st.add(3, true)
			cred := Credential{DeviceID: uuid.New(), Token: "wdd_test"}
			org := &orgStub{}
			f := NewForwarder(NewClient(org.serve(t, cred.DeviceID).URL), st, cred, rec)
			ctx := context.Background()
			f.step(ctx)
			if st.cursor != 3 || st.cursorHash != st.rows[2].RowHash {
				t.Fatalf("setup: cursor = (%d, %q), want (3, %q)", st.cursor, st.cursorHash, st.rows[2].RowHash)
			}

			st.resetTable()
			st.add(refill, true)
			if _, stop := f.step(ctx); stop {
				t.Fatal("a reset is not a revocation")
			}
			if len(org.pushes) != 2 || len(org.pushes[1]) != refill || org.pushes[1][0].Seq != 1 {
				t.Fatalf("pushes after the reset = %v, want all %d new rows from seq 1", org.pushes[1:], refill)
			}
			last := st.rows[refill-1]
			if s := f.Status(); st.cursor != last.Seq || st.cursorHash != last.RowHash || s.AckedSeq != last.Seq || s.LastError != "" || f.halted {
				t.Fatalf("after the resend: cursor = (%d, %q) status = %+v halted = %v, want (%d, %q)",
					st.cursor, st.cursorHash, s, f.halted, last.Seq, last.RowHash)
			}

			// A restart over the new chain resumes at its head: nothing resent.
			g := NewForwarder(NewClient(org.serve(t, cred.DeviceID).URL), st, cred, rec)
			g.step(ctx)
			if len(org.pushes) != 2 || g.Status().AckedSeq != last.Seq {
				t.Fatalf("after a restart: pushes = %d status = %+v", len(org.pushes), g.Status())
			}
		})
	}
}
