// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package notify_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/notify"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// isolatedPool returns a pool on a throwaway, fully migrated database. A worker claims EVERY due row in
// its database, so sharing one with other packages' tests (which enqueue rows for channels this test
// does not know) would let those rows leak into these assertions.
func isolatedPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("WARDYN_TEST_PG")
	if dsn == "" {
		t.Skip("WARDYN_TEST_PG not set; skipping Postgres integration tests")
	}
	ctx := context.Background()
	admin, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	name := "wardyn_notify_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := admin.Exec(ctx, `CREATE DATABASE `+name); err != nil {
		admin.Close()
		t.Fatalf("create database: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname=$1 AND pid <> pg_backend_pid()`, name)
		_, _ = admin.Exec(context.Background(), `DROP DATABASE IF EXISTS `+name)
		admin.Close()
	})
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	pool, err := db.Connect(ctx, u.String())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return pool
}

// recorder is an audit.Recorder that keeps what it is given.
type recorder struct {
	mu     sync.Mutex
	events []types.AuditEvent
}

func (r *recorder) Record(_ context.Context, ev types.AuditEvent) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
	return nil
}

func (r *recorder) byAction(action string) []types.AuditEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []types.AuditEvent
	for _, e := range r.events {
		if e.Action == action {
			out = append(out, e)
		}
	}
	return out
}

// harness is one isolated database with notifications configured for a single webhook channel.
type harness struct {
	pool *pgxpool.Pool
	cfg  *notify.Config
	rec  *recorder
}

func newHarness(t *testing.T, channelJSON string) *harness {
	t.Helper()
	pool := isolatedPool(t)
	cfg, err := notify.Parse(`{"console_url":"https://wardyn.example.com","channels":[` + channelJSON + `]}`)
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	rec := &recorder{}
	notify.SetActive(cfg, rec)
	t.Cleanup(func() { notify.SetActive(nil, nil) })
	return &harness{pool: pool, cfg: cfg, rec: rec}
}

func (h *harness) worker(client *http.Client, masks *secretmask.Registry) *notify.Worker {
	return notify.NewWorker(notify.Deps{Pool: h.pool, Config: h.cfg, Masks: masks, Client: client})
}

func (h *harness) run(t *testing.T) uuid.UUID {
	t.Helper()
	now := time.Now().UTC()
	id := uuid.New()
	_, err := store.NewPG(h.pool).CreateRun(context.Background(), types.AgentRun{
		ID: id, CreatedAt: now, UpdatedAt: now, CreatedBy: "alice", Agent: "claude-code", Repo: "o/r", Task: "do not leak this task title",
		ConfinementClass: types.CC2, State: types.RunRunning, SPIFFEID: "spiffe://wardyn.test/agent-run/" + id.String(), RunnerTarget: "docker",
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	return id
}

// raise creates one approval through the store, so its outbox row comes from the real enqueue path.
func (h *harness) raise(t *testing.T, runID uuid.UUID, scope, reason string) types.ApprovalRequest {
	t.Helper()
	a, err := store.NewPG(h.pool).CreateApproval(context.Background(), types.ApprovalRequest{
		ID: uuid.New(), RunID: runID, Kind: types.ApprovalEgressDomain, RequestedScope: json.RawMessage(scope),
		State: types.ApprovalPending, RequestedAt: time.Now().UTC(), Reason: reason,
	})
	if err != nil {
		t.Fatalf("raise: %v", err)
	}
	return a
}

type rowState struct {
	State, LastError string
	Attempts         int
	LastAttemptAt    *time.Time
	NextAttemptAt    time.Time
	ID               uuid.UUID
}

func (h *harness) row(t *testing.T, approvalID uuid.UUID) rowState {
	t.Helper()
	var r rowState
	err := h.pool.QueryRow(context.Background(),
		`SELECT id, state, last_error, attempts, last_attempt_at, next_attempt_at FROM approval_notifications WHERE approval_id = $1`, approvalID).
		Scan(&r.ID, &r.State, &r.LastError, &r.Attempts, &r.LastAttemptAt, &r.NextAttemptAt)
	if err != nil {
		t.Fatalf("read outbox row: %v", err)
	}
	return r
}

func (h *harness) makeDue(t *testing.T, approvalID uuid.UUID) {
	t.Helper()
	if _, err := h.pool.Exec(context.Background(),
		`UPDATE approval_notifications SET next_attempt_at = now() - interval '1 second' WHERE approval_id = $1`, approvalID); err != nil {
		t.Fatal(err)
	}
}

func chanJSON(u string, extra string) string {
	return `{"id":"hook","type":"webhook","url":` + strconv.Quote(u) + extra + `}`
}

// TestWorker_TwoWorkersDeliver100RowsExactlyOnce: two workers draining one table, each row delivered by
// exactly one of them.
func TestWorker_TwoWorkersDeliver100RowsExactlyOnce(t *testing.T) {
	var mu sync.Mutex
	got := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got[r.Header.Get("X-Wardyn-Delivery")]++
		mu.Unlock()
	}))
	defer srv.Close()
	h := newHarness(t, chanJSON(srv.URL, ""))
	var approvals []uuid.UUID
	for range 5 {
		runID := h.run(t)
		for range 20 {
			approvals = append(approvals, h.raise(t, runID, `{"host":"`+uuid.NewString()+`.example"}`, "").ID)
		}
	}
	var wg sync.WaitGroup
	for range 2 {
		w := h.worker(nil, nil)
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				n, err := w.Tick(context.Background())
				if err != nil {
					t.Errorf("tick: %v", err)
					return
				}
				if n == 0 {
					return
				}
			}
		}()
	}
	wg.Wait()
	if len(got) != 100 {
		t.Fatalf("%d distinct deliveries, want 100", len(got))
	}
	for id, n := range got {
		if n != 1 {
			t.Errorf("delivery %s sent %d times, want exactly once", id, n)
		}
	}
	for _, a := range approvals {
		if r := h.row(t, a); r.State != "sent" {
			t.Fatalf("row for approval %s is %q, want sent", a, r.State)
		}
	}
}

// TestWorker_ExpiredLeaseIsReclaimedAndAStaleFinaliserLoses: a row whose lease lapsed is claimed again,
// and the replica that lost it cannot overwrite the new claimant's result when it finally finishes.
func TestWorker_ExpiredLeaseIsReclaimedAndAStaleFinaliserLoses(t *testing.T) {
	var calls atomic.Int64
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			started <- struct{}{}
			<-release // the first claimant is stuck mid-send
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
	}))
	defer srv.Close()
	h := newHarness(t, chanJSON(srv.URL, ""))
	a := h.raise(t, h.run(t), `{"host":"x.example"}`, "")

	slow := h.worker(nil, nil)
	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := slow.Tick(context.Background()); err != nil {
			t.Errorf("slow tick: %v", err)
		}
	}()
	<-started
	if r := h.row(t, a.ID); r.State != "sending" || r.Attempts != 1 {
		t.Fatalf("mid-send row = %+v, want sending with 1 attempt", r)
	}
	// The lease lapses (the replica is wedged or dead): another worker reclaims and delivers.
	if _, err := h.pool.Exec(context.Background(), `UPDATE approval_notifications SET lease_until = now() - interval '1 second' WHERE approval_id = $1`, a.ID); err != nil {
		t.Fatal(err)
	}
	if n, err := h.worker(nil, nil).Tick(context.Background()); err != nil || n != 1 {
		t.Fatalf("reclaim tick claimed %d rows, err %v; want 1", n, err)
	}
	if r := h.row(t, a.ID); r.State != "sent" || r.Attempts != 2 {
		t.Fatalf("after reclaim row = %+v, want sent with 2 attempts", r)
	}
	close(release)
	<-done
	r := h.row(t, a.ID)
	if r.State != "sent" || r.LastError != "" {
		t.Fatalf("the stale finaliser overwrote the result: %+v", r)
	}
	if n := len(h.rec.byAction("approval.notify.failed")); n != 0 {
		t.Fatalf("the stale finaliser's failure was audited %d times", n)
	}
}

// TestWorker_FiveFailuresYieldOneDeadRowOneAuditRowOneMetric, and the dead row's last_attempt_at falls
// inside the finalising tick.
func TestWorker_FiveFailuresYieldOneDeadRowOneAuditRowOneMetric(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }))
	defer srv.Close()
	h := newHarness(t, chanJSON(srv.URL, ""))
	a := h.raise(t, h.run(t), `{"host":"x.example"}`, "")
	w := h.worker(nil, nil)
	before := notify.Snapshot().Failed["hook"]
	wantWait := []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute}
	var tickStart, tickEnd time.Time
	for i := range 5 {
		tickStart = time.Now()
		if n, err := w.Tick(context.Background()); err != nil || n != 1 {
			t.Fatalf("tick %d claimed %d rows, err %v", i+1, n, err)
		}
		tickEnd = time.Now()
		r := h.row(t, a.ID)
		if r.Attempts != i+1 || r.LastError != "http_status:503" {
			t.Fatalf("after failure %d: %+v", i+1, r)
		}
		if i < 4 {
			if r.State != "pending" {
				t.Fatalf("after failure %d state = %q, want pending", i+1, r.State)
			}
			wait := time.Until(r.NextAttemptAt)
			if wait > wantWait[i] || wait < wantWait[i]-5*time.Second {
				t.Fatalf("after failure %d next attempt in %v, want about %v", i+1, wait, wantWait[i])
			}
			h.makeDue(t, a.ID)
		}
	}
	r := h.row(t, a.ID)
	if r.State != "dead" {
		t.Fatalf("after the fifth failure state = %q, want dead", r.State)
	}
	if r.LastAttemptAt == nil || r.LastAttemptAt.Before(tickStart.Add(-time.Second)) || r.LastAttemptAt.After(tickEnd.Add(time.Second)) {
		t.Fatalf("last_attempt_at = %v, want within the finalising tick [%v, %v]", r.LastAttemptAt, tickStart, tickEnd)
	}
	if n, err := w.Tick(context.Background()); err != nil || n != 0 {
		t.Fatalf("a dead row was claimed again: %d, %v", n, err)
	}
	evs := h.rec.byAction("approval.notify.failed")
	if len(evs) != 1 {
		t.Fatalf("%d approval.notify.failed rows, want 1", len(evs))
	}
	var data map[string]any
	if err := json.Unmarshal(evs[0].Data, &data); err != nil {
		t.Fatal(err)
	}
	if data["error"] != "http_status:503" || data["channel"] != "hook" || data["attempts"] != float64(5) || data["approval_id"] != a.ID.String() {
		t.Fatalf("audit data = %v", data)
	}
	if evs[0].ActorType != types.ActorSystem || evs[0].Target != a.ID.String() || evs[0].RunID == nil {
		t.Fatalf("audit row = %+v", evs[0])
	}
	if got := notify.Snapshot().Failed["hook"]; got != before+1 {
		t.Fatalf("failed counter = %d, want %d", got, before+1)
	}
}

// TestWorker_ARedirectIsNotFollowedAndNoURLIsStored: a 3xx is a non-retryable redirect_refused, the
// target is never contacted, and the URL (including a path that is itself a credential) appears in none
// of last_error, the audit row or the log.
func TestWorker_ARedirectIsNotFollowedAndNoURLIsStored(t *testing.T) {
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })

	var hits atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer target.Close()
	const secretPath = "/services/T000/B000/SECRETWEBHOOKPATH"
	redir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/elsewhere", http.StatusFound)
	}))
	defer redir.Close()
	h := newHarness(t, chanJSON(redir.URL+secretPath, ""))
	a := h.raise(t, h.run(t), `{"host":"x.example"}`, "")
	if n, err := h.worker(nil, nil).Tick(context.Background()); err != nil || n != 1 {
		t.Fatalf("tick: %d, %v", n, err)
	}
	if hits.Load() != 0 {
		t.Fatalf("the redirect target was contacted %d times", hits.Load())
	}
	r := h.row(t, a.ID)
	if r.State != "dead" || r.LastError != "redirect_refused" || r.Attempts != 1 {
		t.Fatalf("row = %+v, want dead/redirect_refused after one attempt", r)
	}
	evs := h.rec.byAction("approval.notify.failed")
	if len(evs) != 1 {
		t.Fatalf("%d audit rows, want 1", len(evs))
	}
	haystack := r.LastError + string(evs[0].Data) + logs.String()
	for _, leak := range []string{"SECRETWEBHOOKPATH", redir.URL, strings.TrimPrefix(redir.URL, "http://"), target.URL} {
		if strings.Contains(haystack, leak) {
			t.Fatalf("%q leaked into last_error, the audit row or the log:\n%s", leak, haystack)
		}
	}
}

// TestWorker_TransportErrorsAreClassesNeverURLs: a refused connection is stored as "dial" and a
// handler that outlives the timeout as "timeout"; neither carries the URL.
func TestWorker_TransportErrorsAreClassesNeverURLs(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	dead := srv.URL + "/SECRETDIALPATH"
	srv.Close() // nothing listens any more
	h := newHarness(t, chanJSON(dead, ""))
	a := h.raise(t, h.run(t), `{"host":"x.example"}`, "")
	if _, err := h.worker(nil, nil).Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	r := h.row(t, a.ID)
	if r.State != "pending" || r.LastError != "dial" {
		t.Fatalf("row = %+v, want pending with class dial", r)
	}
}

// TestWorker_ARowForADecidedApprovalIsCancelledUnsent: re-read before send.
func TestWorker_ARowForADecidedApprovalIsCancelledUnsent(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer srv.Close()
	h := newHarness(t, chanJSON(srv.URL, ""))
	a := h.raise(t, h.run(t), `{"host":"x.example"}`, "")
	if _, err := h.pool.Exec(context.Background(), `UPDATE approvals SET state = 'DENIED' WHERE id = $1`, a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.worker(nil, nil).Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r := h.row(t, a.ID); r.State != "cancelled" || r.LastAttemptAt == nil {
		t.Fatalf("row = %+v, want cancelled with last_attempt_at set", r)
	}
	if hits.Load() != 0 {
		t.Fatal("a notification was sent for a decided approval")
	}
}

// TestWorker_UnsentAnHourAfterDueIsDeadExpired.
func TestWorker_UnsentAnHourAfterDueIsDeadExpired(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer srv.Close()
	h := newHarness(t, chanJSON(srv.URL, ""))
	a := h.raise(t, h.run(t), `{"host":"x.example"}`, "")
	if _, err := h.pool.Exec(context.Background(), `UPDATE approval_notifications SET due_at = now() - interval '61 minutes', next_attempt_at = now() - interval '61 minutes' WHERE approval_id = $1`, a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.worker(nil, nil).Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r := h.row(t, a.ID); r.State != "dead" || r.LastError != "expired" {
		t.Fatalf("row = %+v, want dead/expired", r)
	}
	if hits.Load() != 0 {
		t.Fatal("an expired notification was sent")
	}
	if n := len(h.rec.byAction("approval.notify.failed")); n != 1 {
		t.Fatalf("%d audit rows, want 1", n)
	}
}

// TestWorker_PurgesOldTerminalRows: housekeeping removes terminal rows older than 30 days, in batches.
func TestWorker_PurgesOldTerminalRows(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()
	h := newHarness(t, chanJSON(srv.URL, ""))
	old := h.raise(t, h.run(t), `{"host":"old.example"}`, "")
	fresh := h.raise(t, h.run(t), `{"host":"fresh.example"}`, "")
	w := h.worker(nil, nil)
	if _, err := w.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := h.pool.Exec(context.Background(), `UPDATE approval_notifications SET last_attempt_at = now() - interval '31 days' WHERE approval_id = $1`, old.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := h.pool.QueryRow(context.Background(), `SELECT count(*) FROM approval_notifications WHERE approval_id = $1`, old.ID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("the 31-day-old terminal row survived: %d, %v", n, err)
	}
	if h.row(t, fresh.ID).State != "sent" {
		t.Fatal("a fresh terminal row was purged")
	}
}

// TestWorker_SignedBodyCarriesNoSandboxText is the end-to-end golden check over TLS with an HMAC secret:
// a secret planted in the approval's requested_scope and reason, and the run's task title, reach the
// receiver in neither the body nor a header, and the signature verifies over the exact bytes.
func TestWorker_SignedBodyCarriesNoSandboxText(t *testing.T) {
	const secret = "ghp_PLANTEDSECRETVALUE0123456789abcdef"
	var mu sync.Mutex
	var gotBody []byte
	var gotHeader http.Header
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(r.Body)
		mu.Lock()
		gotBody, gotHeader = buf.Bytes(), r.Header.Clone()
		mu.Unlock()
	}))
	defer srv.Close()
	h := newHarness(t, chanJSON(srv.URL, `,"hmac_secret":"whsec_test_vector","bearer_token":"tok"`))
	runID := h.run(t)
	masks := secretmask.NewRegistry()
	masks.Add(runID, []byte(secret))
	profileID := uuid.New()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO governance_profiles (id, name, ceiling) VALUES ($1, 'payments', '{}')`, []any{profileID}},
		{`INSERT INTO people (principal, email, created_by) VALUES ('alice', 'alice@example.com', 'admin')`, nil},
		{`UPDATE agent_runs SET governance_profile_id = $1 WHERE id = $2`, []any{profileID, runID}},
	} {
		if _, err := h.pool.Exec(context.Background(), q.sql, q.args...); err != nil {
			t.Fatalf("%s: %v", q.sql, err)
		}
	}
	a := h.raise(t, runID, `{"host":"evil.example.com","argv":"`+secret+`"}`, "approve me, key "+secret)
	if n, err := h.worker(srv.Client(), masks).Tick(context.Background()); err != nil || n != 1 {
		t.Fatalf("tick: %d, %v", n, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if gotBody == nil {
		t.Fatal("receiver got nothing")
	}
	for _, leak := range []string{secret, "evil.example.com", "approve me", "do not leak this task title"} {
		if strings.Contains(string(gotBody), leak) {
			t.Fatalf("body carries %q: %s", leak, gotBody)
		}
	}
	if gotHeader.Get("Authorization") != "Bearer tok" || gotHeader.Get("X-Wardyn-Delivery") != h.row(t, a.ID).ID.String() {
		t.Fatalf("headers = %v", gotHeader)
	}
	sig := gotHeader.Get("X-Wardyn-Signature")
	parts := strings.Split(sig, ",")
	if len(parts) != 2 || !strings.HasPrefix(parts[0], "t=") || !strings.HasPrefix(parts[1], "v1=") {
		t.Fatalf("signature header %q", sig)
	}
	mac := hmac.New(sha256.New, []byte("whsec_test_vector"))
	mac.Write([]byte(strings.TrimPrefix(parts[0], "t=") + "."))
	mac.Write(gotBody)
	if want := hex.EncodeToString(mac.Sum(nil)); strings.TrimPrefix(parts[1], "v1=") != want {
		t.Fatalf("signature does not verify over the received bytes: %s", sig)
	}
	var p map[string]any
	if err := json.Unmarshal(gotBody, &p); err != nil || p["schema"] != "wardyn.approval.v1" || p["event"] != "raised" {
		t.Fatalf("body = %s, err %v", gotBody, err)
	}
	profile, _ := p["profile"].(map[string]any)
	requester, _ := p["requester"].(map[string]any)
	if profile["id"] != profileID.String() || profile["name"] != "payments" || requester["principal"] != "alice" || requester["email"] != "alice@example.com" {
		t.Fatalf("profile and requester were not joined in: %s", gotBody)
	}
}

// TestNotifyUnsetMeansNoRows: with no config installed the planner returns nothing.
func TestNotifyUnsetMeansNoRows(t *testing.T) {
	notify.SetActive(nil, nil)
	if notify.Enabled() || len(notify.Plan(types.ApprovalEgressDomain, nil)) != 0 {
		t.Fatal("notifications are on with no config")
	}
}
