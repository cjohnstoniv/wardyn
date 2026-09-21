// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package federation

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// memStore is the local audit table and org_federation cursor.
type memStore struct {
	mu     sync.Mutex
	rows   []types.FederatedAuditEvent
	cursor int64
}

func (m *memStore) add(n int, chained bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for range n {
		seq := int64(len(m.rows) + 1)
		e := types.FederatedAuditEvent{Seq: seq}
		e.ID, e.Action, e.Outcome, e.ActorType = uuid.New(), "run.create", "success", types.ActorSystem
		if chained {
			e.RowHash = "h" + strconv.FormatInt(seq, 10)
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

func (m *memStore) GetFederationCursor(context.Context) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cursor, nil
}

func (m *memStore) SetFederationCursor(_ context.Context, seq int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cursor = seq
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
		if stop || wait != tc.want {
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
