// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

// Hosted-only (WARDYN_TEST_PG): an issuer portal's exchange on the real store, recorded through the audit
// chain run() builds and fanned out to every supported sink encoder over loopback receivers this test serves.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
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
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/api"
	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// fixedPortalVerifier answers every subject token as one verified person of the portal's own issuer, issued
// 10 s ago. The real verifier refuses a loopback issuer, so its own tests drive it; everything after it is real.
type fixedPortalVerifier struct{ uid string }

func (f fixedPortalVerifier) VerifyDetail(_ context.Context, reg oidc.PortalRegistration, _, _ string) (oidc.PortalSubject, oidc.PortalRefusal) {
	now := time.Now()
	return oidc.PortalSubject{
		Principal: oidc.PortalPrincipal(reg.Issuer, f.uid), Issuer: reg.Issuer, K: oidc.PortalIssuerKey(reg.Issuer),
		PersonID: f.uid, Sub: "sub-of-" + f.uid, IssuedAt: now.Add(-10 * time.Second), Expiry: now.Add(time.Hour),
		GrantEnd: now.Add(290 * time.Second),
	}, oidc.PortalRefusal{}
}

// bindIssuerAs binds issuer on pool under the registry lock, as a registration's decision does.
func bindIssuerAs(t *testing.T, pool *pgxpool.Pool, issuer string) types.PortalIssuer {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := store.LockPortalIssuerRegistry(ctx, tx); err != nil {
		t.Fatalf("the registry lock: %v", err)
	}
	b, err := store.NewPG(pool).BindPortalIssuerQ(ctx, tx, issuer, "", "uid", "test")
	if err != nil {
		t.Fatalf("bind %s: %v", issuer, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return b
}

// registerIssuerPortal registers a portal named name on binding b through pool and returns it with its credential.
func registerIssuerPortal(t *testing.T, pool *pgxpool.Pool, b types.PortalIssuer, name string) (types.Delegate, string) {
	t.Helper()
	d := types.Delegate{
		ID: uuid.New(), Name: name, IdPClientID: uuid.NewString(), RegisteredBy: "test",
		Issuer: b.Issuer, InternalIssuer: b.InternalIssuerNorm, IssuerKey: b.IssuerKey, TokenProfile: oidc.PortalProfileAccessToken,
		Audience: "api://wardyn", RequiredScopes: []string{"wardyn.portal"}, UserType: types.UserTypeStandard,
		PortalManagedAdmission: true, MaxSubjectTokenAgeSeconds: 300, GrantTTLSeconds: 300,
	}
	cred := store.PortalCredentialPrefix + uuid.NewString()
	sum := sha256.Sum256([]byte(cred))
	d, err := store.NewPG(pool).CreateDelegateQ(context.Background(), pool, d, hex.EncodeToString(sum[:]))
	if err != nil {
		t.Fatalf("register portal %s: %v", name, err)
	}
	return d, cred
}

// portalExchangeOn posts the token exchange to h as portal d.
func portalExchangeOn(h http.Handler, d types.Delegate, cred string) *httptest.ResponseRecorder {
	form := url.Values{
		"grant_type":         {"urn:ietf:params:oauth:grant-type:token-exchange"},
		"subject_token":      {"subject-token"},
		"subject_token_type": {"urn:ietf:params:oauth:token-type:access_token"},
	}
	r := httptest.NewRequest(http.MethodPost, "/api/v1/token", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.SetBasicAuth(d.ID.String(), cred)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// portalAPIAdminToken is the admin bearer of newPortalAPI's server.
const portalAPIAdminToken = "portal-api-admin-token"

// newPortalAPI is a server over st that records through rec, verifies every issuer portal's subject token as
// person uid, and orders session cuts on pool as run() does.
func newPortalAPI(t *testing.T, st store.Store, rec audit.Recorder, pool *pgxpool.Pool, uid string) http.Handler {
	t.Helper()
	return api.New(api.Config{
		Store: st, Audit: rec, AdminToken: portalAPIAdminToken, TrustDomain: "wardyn.local", BaseCtx: t.Context(),
		PortalIssuers: fixedPortalVerifier{uid: uid}, SessionRevocations: &pgSessionRevocations{pool: pool},
	}).Handler()
}

// stopBeforeMarkPG is the real store, except that the first delivery mark is lost, as when the process stops
// after the durable append and before the mark.
type stopBeforeMarkPG struct {
	store.PG
	stopped atomic.Bool
}

func (s *stopBeforeMarkPG) MarkPortalIntroductionAudited(ctx context.Context, id uuid.UUID) (bool, error) {
	if s.stopped.CompareAndSwap(false, true) {
		return false, errors.New("the process stopped before the mark")
	}
	return s.PG.MarkPortalIntroductionAudited(ctx, id)
}

// sinkReceivers are the loopback ends of the webhook, syslog and OTLP sinks, and the file sink's path.
type sinkReceivers struct {
	mu      sync.Mutex
	webhook [][]byte // one JSON event per line
	syslog  [][]byte // one datagram each
	otlp    [][]byte // one logs request body each
	file    string
}

func newSinkReceivers(t *testing.T) (*sinkReceivers, string) {
	t.Helper()
	rc := &sinkReceivers{file: t.TempDir() + "/audit.jsonl"}
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		rc.mu.Lock()
		for _, line := range bytes.Split(bytes.TrimSpace(body), []byte("\n")) {
			rc.webhook = append(rc.webhook, line)
		}
		rc.mu.Unlock()
	}))
	t.Cleanup(hook.Close)
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.URL.Path == "/v1/logs" {
			rc.mu.Lock()
			rc.otlp = append(rc.otlp, body)
			rc.mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(collector.Close)
	udp, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = udp.Close() })
	go func() {
		buf := make([]byte, 64<<10)
		for {
			n, _, err := udp.ReadFrom(buf)
			if err != nil {
				return
			}
			rc.mu.Lock()
			rc.syslog = append(rc.syslog, append([]byte(nil), buf[:n]...))
			rc.mu.Unlock()
		}
	}()
	cfg, _ := json.Marshal(map[string]any{
		"webhook": map[string]any{"url": hook.URL, "batch_size": 1, "flush_interval": "20ms"},
		"syslog":  map[string]any{"network": "udp", "addr": udp.LocalAddr().String()},
		"file":    map[string]any{"path": rc.file},
		"otlp":    map[string]any{"endpoint": collector.URL, "batch_size": 1, "flush_interval": "20ms"},
	})
	return rc, string(cfg)
}

// sinkEvent is the id and time a webhook, file or syslog copy carries.
type sinkEvent struct {
	ID     uuid.UUID `json:"id"`
	Time   time.Time `json:"time"`
	Action string    `json:"action"`
}

// copies is each sink's copies of action: the decoded events of the three that send the whole event, and the
// raw OTLP log records whose body is action.
func (rc *sinkReceivers) copies(t *testing.T, action string) (byName map[string][]sinkEvent, otlp []json.RawMessage) {
	t.Helper()
	rc.mu.Lock()
	defer rc.mu.Unlock()
	byName = map[string][]sinkEvent{}
	add := func(name string, raw []byte) {
		var ev sinkEvent
		if err := json.Unmarshal(raw, &ev); err != nil {
			t.Fatalf("%s copy %q: %v", name, raw, err)
		}
		if ev.Action == action {
			byName[name] = append(byName[name], ev)
		}
	}
	for _, l := range rc.webhook {
		add("webhook", l)
	}
	for _, d := range rc.syslog {
		// RFC 3164 framing from log/syslog, then the event's JSON.
		if i := bytes.IndexByte(d, '{'); i >= 0 {
			add("syslog", bytes.TrimSpace(d[i:]))
		}
	}
	f, err := os.Open(rc.file)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		add("file", sc.Bytes())
	}
	for _, body := range rc.otlp {
		var req struct {
			ResourceLogs []struct {
				ScopeLogs []struct {
					LogRecords []json.RawMessage `json:"logRecords"`
				} `json:"scopeLogs"`
			} `json:"resourceLogs"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			t.Fatalf("otlp logs request %q: %v", body, err)
		}
		for _, rl := range req.ResourceLogs {
			for _, sl := range rl.ScopeLogs {
				for _, rec := range sl.LogRecords {
					var r struct {
						Body struct {
							StringValue string `json:"stringValue"`
						} `json:"body"`
					}
					if json.Unmarshal(rec, &r) == nil && r.Body.StringValue == action {
						otlp = append(otlp, rec)
					}
				}
			}
		}
	}
	return byName, otlp
}

// TestPortalIntroduction_StopAfterAppendReplaysSameIDToEverySink_PG: the first exchange appends the
// introduction durably and stops before marking it delivered; a later exchange of the same person through
// another portal on the issuer replays it. The audit table keeps one row, under the introducing portal and the
// real issuer; the webhook, file and syslog sinks each receive two copies with the row's id and time; the OTLP
// sink receives two identical records with the row's time and no id.
func TestPortalIntroduction_StopAfterAppendReplaysSameIDToEverySink_PG(t *testing.T) {
	pool := portalBootPool(t, "replay")
	ctx := context.Background()
	rc, sinksJSON := newSinkReceivers(t)
	rec, fan, _, _, err := buildAuditChain(t.Context(), sinksJSON, "", "", pool, secretmask.NewRegistry(), newAuditSealSource(audit.SealOff))
	if err != nil {
		t.Fatalf("the audit chain: %v", err)
	}
	if fan == nil {
		t.Fatal("no sink fanout was built")
	}
	closeFan := sync.OnceValue(fan.Close)
	t.Cleanup(func() { _ = closeFan() })
	const issuer = "https://idp.replay.example/oauth2/default"
	b := bindIssuerAs(t, pool, issuer)
	first, credFirst := registerIssuerPortal(t, pool, b, "replay-first")
	later, credLater := registerIssuerPortal(t, pool, b, "replay-later")
	st := &stopBeforeMarkPG{PG: store.NewPG(pool)}
	h := newPortalAPI(t, st, rec, pool, "u1")

	if w := portalExchangeOn(h, first, credFirst); w.Code != http.StatusOK {
		t.Fatalf("first exchange = %d %s", w.Code, w.Body)
	}
	principal := oidc.PortalPrincipal(issuer, "u1")
	owed := func() int {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM principal_identities WHERE principal = $1 AND introduction_audited_at IS NULL`, principal).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if !st.stopped.Load() || owed() != 1 {
		t.Fatalf("after the stop: mark attempted %v, owed %d; want the introduction appended and still owed", st.stopped.Load(), owed())
	}
	// The event's id and time are the identity row's, fixed when it was inserted: exactly one audit row has
	// that pair before the replay, and the same one after it.
	var eventID uuid.UUID
	var eventAt time.Time
	if err := pool.QueryRow(ctx, `SELECT introduction_event_id, introduction_event_at FROM principal_identities WHERE principal = $1`, principal).Scan(&eventID, &eventAt); err != nil {
		t.Fatalf("read the identity's introduction event: %v", err)
	}
	withTuple := func() int {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE id = $1 AND time = $2 AND action = 'person.create'`, eventID, eventAt).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := withTuple(); n != 1 {
		t.Fatalf("audit rows with the identity's introduction id and time before the replay = %d, want 1", n)
	}
	if w := portalExchangeOn(h, later, credLater); w.Code != http.StatusOK {
		t.Fatalf("later exchange = %d %s", w.Code, w.Body)
	}
	if n := owed(); n != 0 {
		t.Fatalf("owed after the replay = %d, want 0", n)
	}
	var (
		n      int
		rowID  uuid.UUID
		rowAt  time.Time
		actor  string
		target string
		data   string
	)
	if err := pool.QueryRow(ctx, `SELECT count(*) OVER (), id, time, actor, target, data::text FROM audit_events WHERE action = 'person.create' AND target = $1`, principal).
		Scan(&n, &rowID, &rowAt, &actor, &target, &data); err != nil {
		t.Fatalf("read the introduction row: %v", err)
	}
	if n != 1 || actor != "delegate:"+first.ID.String() || !strings.Contains(data, issuer) || !strings.Contains(data, b.IssuerKey) {
		t.Fatalf("person.create rows = %d, actor %q, data %s; want one, under the introducing portal and the real issuer", n, actor, data)
	}
	if n := withTuple(); n != 1 || rowID != eventID || !rowAt.Equal(eventAt) {
		t.Fatalf("after the replay: %d rows with the identity's pair, row %s %v; want the same single row %s %v", n, rowID, rowAt, eventID, eventAt)
	}
	if err := closeFan(); err != nil {
		t.Fatal(err)
	}
	var byName map[string][]sinkEvent
	var otlp []json.RawMessage
	deadline := time.Now().Add(10 * time.Second)
	for {
		byName, otlp = rc.copies(t, "person.create")
		if (len(byName["webhook"]) >= 2 && len(byName["syslog"]) >= 2 && len(byName["file"]) >= 2 && len(otlp) >= 2) || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	for _, name := range []string{"webhook", "file", "syslog"} {
		got := byName[name]
		if len(got) != 2 {
			t.Errorf("%s copies of the introduction = %d, want 2 (the append and the replay)", name, len(got))
			continue
		}
		for _, c := range got {
			if c.ID != rowID || !c.Time.Equal(rowAt) {
				t.Errorf("%s copy id %s time %v, want the row's %s %v", name, c.ID, c.Time, rowID, rowAt)
			}
		}
	}
	if len(otlp) != 2 || !bytes.Equal(otlp[0], otlp[1]) {
		t.Fatalf("otlp records of the introduction = %s, want two identical", otlp)
	}
	var r struct {
		TimeUnixNano string `json:"timeUnixNano"`
	}
	if err := json.Unmarshal(otlp[0], &r); err != nil || r.TimeUnixNano != strconv.FormatInt(rowAt.UnixNano(), 10) {
		t.Errorf("otlp record time %q, want the row's %d", r.TimeUnixNano, rowAt.UnixNano())
	}
	if bytes.Contains(otlp[0], []byte(rowID.String())) {
		t.Errorf("otlp record %s carries the row id", otlp[0])
	}
}

// TestPortalIntroduction_SpooledReceiptSettlesOnAnotherReplica_PG: one replica's audit chain cannot reach the
// database, so its recorder spools the introduction locally and answers with the production spool result.
// The introduction stays owed and no shared row exists. A later exchange on another replica, whose chain
// reaches the database, appends the one row with the identity's stored id and time, and settles it. The
// first replica's spool is never drained, as when its pod is replaced.
func TestPortalIntroduction_SpooledReceiptSettlesOnAnotherReplica_PG(t *testing.T) {
	pool := portalBootPool(t, "spool")
	ctx := context.Background()
	dead, err := db.Connect(ctx, pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	dead.Close()
	spoolPath := t.TempDir() + "/audit-spool.jsonl"
	recA, _, spoolA, _, err := buildAuditChain(t.Context(), "", spoolPath, "", dead, secretmask.NewRegistry(), newAuditSealSource(audit.SealOff))
	if err != nil || spoolA == nil {
		t.Fatalf("the first replica's chain: spool %v, %v", spoolA, err)
	}
	recB, _, _, _, err := buildAuditChain(t.Context(), "", "", "", pool, secretmask.NewRegistry(), newAuditSealSource(audit.SealOff))
	if err != nil {
		t.Fatal(err)
	}
	const issuer = "https://idp.spool.example/oauth2/default"
	p, cred := registerIssuerPortal(t, pool, bindIssuerAs(t, pool, issuer), "spool-portal")
	principal := oidc.PortalPrincipal(issuer, "u1")
	count := func(q string, args ...any) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, q, args...).Scan(&n); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return n
	}
	if w := portalExchangeOn(newPortalAPI(t, store.NewPG(pool), recA, pool, "u1"), p, cred); w.Code != http.StatusOK {
		t.Fatalf("exchange on the replica that cannot reach the audit table = %d %s", w.Code, w.Body)
	}
	var eventID uuid.UUID
	var eventAt time.Time
	if err := pool.QueryRow(ctx, `SELECT introduction_event_id, introduction_event_at FROM principal_identities WHERE principal = $1 AND introduction_audited_at IS NULL`, principal).Scan(&eventID, &eventAt); err != nil {
		t.Fatalf("after a spooled receipt the introduction is not owed: %v", err)
	}
	if n := count(`SELECT count(*) FROM audit_events WHERE action = 'person.create' AND target = $1`, principal); n != 0 {
		t.Fatalf("person.create rows after a spooled receipt = %d, want 0", n)
	}
	spooled, err := os.ReadFile(spoolPath)
	if err != nil || !strings.Contains(string(spooled), eventID.String()) {
		t.Fatalf("the first replica's spool does not hold the introduction (%v)", err)
	}
	if w := portalExchangeOn(newPortalAPI(t, store.NewPG(pool), recB, pool, "u1"), p, cred); w.Code != http.StatusOK {
		t.Fatalf("exchange on the other replica = %d %s", w.Code, w.Body)
	}
	if n := count(`SELECT count(*) FROM audit_events WHERE action = 'person.create' AND target = $1`, principal); n != 1 {
		t.Fatalf("person.create rows after the retry = %d, want 1", n)
	}
	if n := count(`SELECT count(*) FROM audit_events WHERE id = $1 AND time = $2 AND action = 'person.create'`, eventID, eventAt); n != 1 {
		t.Fatalf("rows with the identity's introduction id and time = %d, want 1", n)
	}
	if n := count(`SELECT count(*) FROM principal_identities WHERE principal = $1 AND introduction_audited_at IS NOT NULL`, principal); n != 1 {
		t.Fatal("the introduction was not settled once the shared row existed")
	}
}
