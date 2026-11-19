// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const keyedRunBody = `{"agent":"claude-code","repo":"acme/widgets","task":"t"}`

// keyedPost sends method path with body, the headers in hdr, and auth's credential.
func keyedPost(t *testing.T, srv *Server, method, path, body string, auth func(*http.Request), hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	auth(r)
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	r.Host, r.RemoteAddr = "127.0.0.1", "127.0.0.1:54321"
	w := httptest.NewRecorder()
	panicFails(t, srv.Handler()).ServeHTTP(w, r)
	return w
}

func bearerAuth(tok string) func(*http.Request) {
	return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+tok) }
}

func cookieAuth(c *http.Cookie) func(*http.Request) { return func(r *http.Request) { r.AddCookie(c) } }

// postRunWithKey is POST /api/v1/runs as the admin token, with key as its Idempotency-Key.
func postRunWithKey(t *testing.T, srv *Server, key, body string) *httptest.ResponseRecorder {
	t.Helper()
	return keyedPost(t, srv, http.MethodPost, "/api/v1/runs", body, bearerAuth(adminToken), map[string]string{"Idempotency-Key": key})
}

func keySHA(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// requestSHA is the fingerprint a key is held to: the body's bytes, a zero byte, the break-glass reason.
func requestSHA(body, breakGlass string) string {
	h := sha256.New()
	h.Write([]byte(body))
	h.Write([]byte{0})
	h.Write([]byte(breakGlass))
	return hex.EncodeToString(h.Sum(nil))
}

func keyRows(t *testing.T, pool *pgxpool.Pool, key string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM run_create_keys WHERE key_sha256 = $1`, keySHA(key)).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func runsTitled(t *testing.T, pool *pgxpool.Pool, task string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM agent_runs WHERE task = $1`, task).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// uniqueBody is keyedRunBody with a task no other test uses, so its runs can be counted.
func uniqueBody() (body, task string) {
	task = "idem-" + uuid.NewString()
	return strings.Replace(keyedRunBody, `"task":"t"`, `"task":"`+task+`"`, 1), task
}

func recAudits(srv *Server, action string) []types.AuditEvent {
	var out []types.AuditEvent
	for _, ev := range srv.cfg.Audit.(*recRecorder).snapshot() {
		if ev.Action == action {
			out = append(out, ev)
		}
	}
	return out
}

func auditsOnRun(srv *Server, action string, id uuid.UUID) []types.AuditEvent {
	var out []types.AuditEvent
	for _, ev := range recAudits(srv, action) {
		if ev.RunID != nil && *ev.RunID == id {
			out = append(out, ev)
		}
	}
	return out
}

func createdID(t *testing.T, w *httptest.ResponseRecorder) uuid.UUID {
	t.Helper()
	var out createRunResponse
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || out.ID == uuid.Nil {
		t.Fatalf("decode run: %v %s", err, w.Body)
	}
	return out.ID
}

// TestCreateRunNoKeyByteEqual: a create without the header takes the old path: no replay header, no key
// row, no key field on run.create, and two such creates are two runs.
func TestCreateRunNoKeyByteEqual(t *testing.T) {
	srv, pool := pgHarness(t)
	body, task := uniqueBody()
	first := do(t, srv, http.MethodPost, "/api/v1/runs", adminToken, body)
	second := do(t, srv, http.MethodPost, "/api/v1/runs", adminToken, body)
	for _, w := range []*httptest.ResponseRecorder{first, second} {
		if w.Code != http.StatusCreated || w.Header().Get("Idempotent-Replayed") != "" {
			t.Fatalf("keyless create = %d replayed=%q %s", w.Code, w.Header().Get("Idempotent-Replayed"), w.Body)
		}
	}
	if n := runsTitled(t, pool, task); n != 2 {
		t.Errorf("keyless creates made %d runs, want 2", n)
	}
	for _, w := range []*httptest.ResponseRecorder{first, second} {
		rows := auditsOnRun(srv, "run.create", createdID(t, w))
		if len(rows) != 1 {
			t.Fatalf("run.create rows = %d, want 1", len(rows))
		}
		if _, ok := auditData(t, rows[0])["idempotency_key_sha256"]; ok {
			t.Error("a keyless run.create carries idempotency_key_sha256")
		}
	}
}

// TestCreateRunKeyReplay: the second POST with the same key and body is the first answer, byte for byte,
// marked Idempotent-Replayed; one run, one run.create, one run.create.replay.
func TestCreateRunKeyReplay(t *testing.T) {
	srv, pool := pgHarness(t)
	body, task := uniqueBody()
	key := uuid.NewString()
	first := postRunWithKey(t, srv, key, body)
	if first.Code != http.StatusCreated || first.Header().Get("Idempotent-Replayed") != "" {
		t.Fatalf("first = %d replayed=%q %s", first.Code, first.Header().Get("Idempotent-Replayed"), first.Body)
	}
	second := postRunWithKey(t, srv, key, body)
	if second.Code != first.Code || !bytes.Equal(second.Body.Bytes(), first.Body.Bytes()) ||
		second.Header().Get("Location") != first.Header().Get("Location") || second.Header().Get("Idempotent-Replayed") != "true" {
		t.Fatalf("replay = %d %q replayed=%q %s; want the first answer %d %q %s", second.Code, second.Header().Get("Location"),
			second.Header().Get("Idempotent-Replayed"), second.Body, first.Code, first.Header().Get("Location"), first.Body)
	}
	for _, w := range []*httptest.ResponseRecorder{first, second} {
		if w.Header().Get("Content-Type") != "application/json" || w.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("answer headers = %v, want application/json and nosniff", w.Header())
		}
	}
	if n := runsTitled(t, pool, task); n != 1 {
		t.Errorf("runs = %d, want 1", n)
	}
	id := createdID(t, first)
	creates := auditsOnRun(srv, "run.create", id)
	if len(creates) != 1 || auditData(t, creates[0])["idempotency_key_sha256"] != keySHA(key) {
		t.Errorf("run.create rows = %+v, want one carrying the key's SHA-256", creates)
	}
	replays := auditsOnRun(srv, "run.create.replay", id)
	if len(replays) != 1 || replays[0].Outcome != "success" || auditData(t, replays[0])["reconstructed"] != false {
		t.Errorf("run.create.replay rows = %+v, want one success, not reconstructed", replays)
	}
	for _, ev := range srv.cfg.Audit.(*recRecorder).snapshot() {
		if strings.Contains(string(ev.Data), key) {
			t.Errorf("%s row carries the raw key", ev.Action)
		}
	}
}

// TestCreateRunKeyMismatch422: the same key with another body is refused and audited, and makes no run.
func TestCreateRunKeyMismatch422(t *testing.T) {
	srv, pool := pgHarness(t)
	body, _ := uniqueBody()
	other, otherTask := uniqueBody()
	key := uuid.NewString()
	if w := postRunWithKey(t, srv, key, body); w.Code != http.StatusCreated {
		t.Fatalf("first = %d %s", w.Code, w.Body)
	}
	w := postRunWithKey(t, srv, key, other)
	if w.Code != http.StatusUnprocessableEntity || errorReason(w) != "idempotency_key_reused" {
		t.Fatalf("other body = %d %s, want 422 idempotency_key_reused", w.Code, w.Body)
	}
	if n := runsTitled(t, pool, otherTask); n != 0 {
		t.Errorf("the refused body made %d runs", n)
	}
	denied := 0
	for _, ev := range recAudits(srv, "run.create.replay") {
		if ev.Outcome == "denied" && ev.RunID == nil && auditData(t, ev)["reason"] == "idempotency_key_reused" {
			denied++
		}
	}
	if denied != 1 {
		t.Errorf("denied run.create.replay rows = %d, want 1", denied)
	}
}

// TestCreateRunKeyConcurrent: eight creates with one key at once make one run; the others replay it or
// are told the first is still running.
func TestCreateRunKeyConcurrent(t *testing.T) {
	srv, pool := pgHarness(t)
	body, task := uniqueBody()
	key := uuid.NewString()
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		codes = map[int]int{}
	)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := postRunWithKey(t, srv, key, body)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case w.Code == http.StatusCreated && w.Header().Get("Idempotent-Replayed") == "":
				codes[201]++
			case w.Code == http.StatusCreated:
				codes[-201]++ // a replay
			case w.Code == http.StatusConflict && errorReason(w) == "idempotency_key_in_flight" && w.Header().Get("Retry-After") == "1":
				codes[409]++
			default:
				codes[w.Code]++
			}
		}()
	}
	wg.Wait()
	if codes[201] != 1 || codes[201]+codes[-201]+codes[409] != 8 {
		t.Errorf("answers = %v, want one fresh 201 and the rest replays or 409 in flight", codes)
	}
	if n := runsTitled(t, pool, task); n != 1 {
		t.Errorf("runs = %d, want 1", n)
	}
}

// TestCreateRunKeyRefusalReleases: a refusal before the insert keeps nothing, so the same key and body
// are judged afresh once the cause is gone.
func TestCreateRunKeyRefusalReleases(t *testing.T) {
	srv, pool := pgHarness(t)
	if w := do(t, srv, http.MethodPost, "/api/v1/runs", adminToken, keyedRunBody); w.Code != http.StatusCreated {
		t.Fatalf("occupant = %d %s", w.Code, w.Body)
	}
	srv.cfg.MaxConcurrentRuns = 1 // at least the occupant is live, so the next create is over the cap
	body, task := uniqueBody()
	key := uuid.NewString()
	if w := postRunWithKey(t, srv, key, body); w.Code == http.StatusCreated {
		t.Fatalf("over the cap = %d %s, want a refusal", w.Code, w.Body)
	}
	if n := keyRows(t, pool, key); n != 0 {
		t.Errorf("the refused create left %d key rows, want 0", n)
	}
	srv.cfg.MaxConcurrentRuns = 0
	w := postRunWithKey(t, srv, key, body)
	if w.Code != http.StatusCreated || w.Header().Get("Idempotent-Replayed") != "" {
		t.Fatalf("after the fix = %d replayed=%q %s, want a fresh 201", w.Code, w.Header().Get("Idempotent-Replayed"), w.Body)
	}
	if n := runsTitled(t, pool, task); n != 1 {
		t.Errorf("runs = %d, want 1", n)
	}
}

// failHarnessStore refuses the run's harness record, which create writes after the insert.
type failHarnessStore struct{ store.PG }

func (failHarnessStore) PutRunHarnesses(context.Context, uuid.UUID, []types.RunHarness) error {
	return errors.New("harness record refused")
}

// TestCreateRunKeyPostInsertRefusalReplaysSameStatus: a refusal after the run row exists is the key's
// answer; a retry gets that answer back, never a 201 rebuilt from the run.
func TestCreateRunKeyPostInsertRefusalReplaysSameStatus(t *testing.T) {
	srv, pool := pgHarness(t)
	srv.cfg.Store = failHarnessStore{store.NewPG(pool)}
	srv.cfg.AgentImages = composeAgentImages
	// A request that names its harness, so create records the harness set after the insert.
	task := "idem-" + uuid.NewString()
	body := strings.Replace(endBody("background", ""), `"task":"t"`, `"task":"`+task+`"`, 1)
	key := uuid.NewString()
	first := postRunWithKey(t, srv, key, body)
	if first.Code == http.StatusCreated || runsTitled(t, pool, task) != 1 {
		t.Fatalf("first = %d %s with %d runs; want a refusal after the insert", first.Code, first.Body, runsTitled(t, pool, task))
	}
	second := postRunWithKey(t, srv, key, body)
	if second.Code != first.Code || !bytes.Equal(second.Body.Bytes(), first.Body.Bytes()) || second.Header().Get("Idempotent-Replayed") != "true" {
		t.Fatalf("retry = %d replayed=%q %s; want the first answer %d %s", second.Code, second.Header().Get("Idempotent-Replayed"), second.Body, first.Code, first.Body)
	}
	if n := runsTitled(t, pool, task); n != 1 {
		t.Errorf("runs = %d, want 1", n)
	}
}

// revokeRecorder records the run identities revoked.
type revokeRecorder struct {
	identity.Provider
	mu      sync.Mutex
	revoked []uuid.UUID
}

func (r *revokeRecorder) RevokeRun(ctx context.Context, id uuid.UUID) error {
	r.mu.Lock()
	r.revoked = append(r.revoked, id)
	r.mu.Unlock()
	return r.Provider.RevokeRun(ctx, id)
}

// TestCreateRunKeyStaleAttemptCannotInsert: an attempt whose lease ran out and was taken over cannot
// insert its run: it gets 409 and the identity minted for it is revoked.
func TestCreateRunKeyStaleAttemptCannotInsert(t *testing.T) {
	srv, pool := pgHarness(t)
	ids := &revokeRecorder{Provider: srv.cfg.Identity}
	srv.cfg.Identity = ids
	ctx := context.Background()
	probe := do(t, srv, http.MethodPost, "/api/v1/runs", adminToken, keyedRunBody)
	var run types.AgentRun
	if err := json.Unmarshal(probe.Body.Bytes(), &run); err != nil || probe.Code != http.StatusCreated {
		t.Fatalf("probe = %d %s", probe.Code, probe.Body)
	}
	body, task := uniqueBody()
	key := uuid.NewString()
	id := store.RunCreateKeyID{Principal: run.CreatedBy, KeySHA256: keySHA(key)}
	st := store.NewPG(pool)
	stale, err := st.ClaimRunCreateKey(ctx, store.RunCreateKey{RunCreateKeyID: id, RequestSHA256: requestSHA(body, "")}, 5*time.Minute)
	if err != nil || stale.State != store.RunCreateKeyClaimed {
		t.Fatalf("stale claim = %+v %v", stale, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE run_create_keys SET claim_until = now() - interval '1 second' WHERE key_sha256 = $1`, id.KeySHA256); err != nil {
		t.Fatal(err)
	}
	if w := postRunWithKey(t, srv, key, body); w.Code != http.StatusCreated || w.Header().Get("Idempotent-Replayed") != "" {
		t.Fatalf("takeover = %d %s, want a fresh 201", w.Code, w.Body)
	}
	// The stale attempt reaches the insert holding its old claim.
	h := panicFails(t, srv.Handler())
	staleSrv := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(w, r.WithContext(store.WithRunCreateKey(r.Context(), id, stale.Token)))
	})
	r := httptest.NewRequest(http.MethodPost, "/api/v1/runs", strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+adminToken)
	r.Host, r.RemoteAddr = "127.0.0.1", "127.0.0.1:54321"
	w := httptest.NewRecorder()
	staleSrv.ServeHTTP(w, r)
	if w.Code != http.StatusConflict || errorReason(w) != "idempotency_key_in_flight" {
		t.Fatalf("stale attempt = %d %s, want 409 idempotency_key_in_flight", w.Code, w.Body)
	}
	if n := runsTitled(t, pool, task); n != 1 {
		t.Errorf("runs = %d, want 1", n)
	}
	ids.mu.Lock()
	defer ids.mu.Unlock()
	uncommitted := 0
	for _, rid := range ids.revoked {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM agent_runs WHERE id = $1`, rid).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			uncommitted++
		}
	}
	if uncommitted != 1 {
		t.Errorf("revoked identities = %v, want the stale attempt's one, whose run was never committed", ids.revoked)
	}
}

// TestCreateRunKeyReconstructedReplay: a key whose answer was not kept answers 409 while the claim that
// bound it is inside its lease (its first request may still be answering), however old the key, then a
// 201 rebuilt from the run, with no warnings, once the lease is over.
func TestCreateRunKeyReconstructedReplay(t *testing.T) {
	srv, pool := pgHarness(t)
	body, task := uniqueBody()
	key := uuid.NewString()
	first := postRunWithKey(t, srv, key, body)
	if first.Code != http.StatusCreated {
		t.Fatalf("first = %d %s", first.Code, first.Body)
	}
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `UPDATE run_create_keys SET response_status = NULL, response_location = NULL, response_body = NULL
		WHERE key_sha256 = $1`, keySHA(key)); err != nil {
		t.Fatal(err)
	}
	if w := postRunWithKey(t, srv, key, body); w.Code != http.StatusConflict || errorReason(w) != "idempotency_key_in_flight" {
		t.Fatalf("within the lease = %d %s, want 409 idempotency_key_in_flight", w.Code, w.Body)
	}
	if _, err := pool.Exec(ctx, `UPDATE run_create_keys SET created_at = now() - interval '10 minutes' WHERE key_sha256 = $1`, keySHA(key)); err != nil {
		t.Fatal(err)
	}
	if w := postRunWithKey(t, srv, key, body); w.Code != http.StatusConflict || errorReason(w) != "idempotency_key_in_flight" {
		t.Fatalf("an old key inside its lease = %d %s, want 409 idempotency_key_in_flight", w.Code, w.Body)
	}
	if _, err := pool.Exec(ctx, `UPDATE run_create_keys SET claim_until = now() - interval '1 second' WHERE key_sha256 = $1`, keySHA(key)); err != nil {
		t.Fatal(err)
	}
	w := postRunWithKey(t, srv, key, body)
	var out createRunResponse
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || w.Code != http.StatusCreated || w.Header().Get("Idempotent-Replayed") != "true" {
		t.Fatalf("rebuilt replay = %d replayed=%q %s %v", w.Code, w.Header().Get("Idempotent-Replayed"), w.Body, err)
	}
	if out.ID != createdID(t, first) || len(out.Warnings) != 0 || w.Header().Get("Location") != first.Header().Get("Location") {
		t.Errorf("rebuilt replay = id %s warnings %v location %q; want the first run, no warnings, its Location", out.ID, out.Warnings, w.Header().Get("Location"))
	}
	replays := auditsOnRun(srv, "run.create.replay", out.ID)
	if len(replays) != 1 || auditData(t, replays[0])["reconstructed"] != true {
		t.Errorf("run.create.replay rows = %+v, want one reconstructed", replays)
	}
	if n := runsTitled(t, pool, task); n != 1 {
		t.Errorf("runs = %d, want 1", n)
	}
}

// TestCreateRunKeyScopes: a person, a portal acting for that person, and another person sending the same
// key and body are three callers and get three runs.
func TestCreateRunKeyScopes(t *testing.T) {
	e := newDelegationPG(t)
	portal, cred := e.registerPortal(t, delegGroup)
	tok := e.delegate(t, portal, cred, delegPerson, delegPersonEmail, delegGroup)
	person := accessSession(t, delegPerson, delegPersonEmail, oidc.RoleUser, []string{delegGroup})
	// An admin starts runs from the user view only (POST /runs in the Admin view is 409 admin_view).
	admin := signedAccessCookie(t, oidc.Session{V: oidc.SessionCodecVersion, Sub: "root", Email: "root@corp.example",
		Role: oidc.RoleAdmin, UserType: types.UserTypeStandard, MemberMode: true,
		Groups: []string{delegGroup}, Expiry: time.Now().UTC().Add(time.Hour)})
	body := `{"agent":"claude-code","task":"idem-scopes-` + uuid.NewString() + `"}`
	hdr := map[string]string{"Idempotency-Key": uuid.NewString()}
	seen := map[uuid.UUID]bool{}
	for name, auth := range map[string]func(*http.Request){"person": cookieAuth(person), "portal": bearerAuth(tok), "admin": cookieAuth(admin)} {
		w := keyedPost(t, e.h.srv, http.MethodPost, "/api/v1/runs", body, auth, hdr)
		if w.Code != http.StatusCreated || w.Header().Get("Idempotent-Replayed") != "" {
			t.Fatalf("%s = %d replayed=%q %s, want a fresh 201", name, w.Code, w.Header().Get("Idempotent-Replayed"), w.Body)
		}
		seen[createdID(t, w)] = true
	}
	if len(seen) != 3 {
		t.Errorf("runs = %d, want 3", len(seen))
	}
}

// TestWorkloadLaunchIgnoresKey: a workload launch is not a POST /runs, so its header keeps no key.
func TestWorkloadLaunchIgnoresKey(t *testing.T) {
	e := newSvcEnv(t)
	pol := e.plainPolicy(t, "wl-idem-pol")
	key := uuid.NewString()
	w := keyedPost(t, e.h.srv, http.MethodPut, "/api/v1/workloads/idem", workloadBody(pol, "t", ""), cookieAuth(e.mem), map[string]string{"Idempotency-Key": key})
	launchedRun(t, decodeApply(t, w, http.StatusCreated))
	if n := keyRows(t, e.pool, key); n != 0 {
		t.Errorf("the workload launch kept %d key rows, want 0", n)
	}
}

// TestCreateRunKeyQueuedPath: a launch that waits for a pool slot binds and replays the same way.
func TestCreateRunKeyQueuedPath(t *testing.T) {
	e := newQueueEnv(t)
	pool := e.cappedPool("idem-queue", 1)
	e.occupy(pool)
	body := endBody("background", `"runner_pool_id":"`+pool.ID.String()+`"`)
	key := uuid.NewString()
	first := postRunWithKey(t, e.srv, key, body)
	var out queuedLaunchAnswer
	if err := json.Unmarshal(first.Body.Bytes(), &out); err != nil || first.Code != http.StatusCreated || out.State != types.RunQueued {
		t.Fatalf("first = %d %s, want 201 QUEUED", first.Code, first.Body)
	}
	second := postRunWithKey(t, e.srv, key, body)
	if second.Code != http.StatusCreated || !bytes.Equal(second.Body.Bytes(), first.Body.Bytes()) || second.Header().Get("Idempotent-Replayed") != "true" {
		t.Fatalf("replay = %d replayed=%q %s; want the first answer", second.Code, second.Header().Get("Idempotent-Replayed"), second.Body)
	}
	var bound uuid.UUID
	if err := e.pool.QueryRow(context.Background(), `SELECT run_id FROM run_create_keys WHERE key_sha256 = $1`, keySHA(key)).Scan(&bound); err != nil || bound != out.ID {
		t.Errorf("key bound to %s (%v), want the queued run %s", bound, err, out.ID)
	}
	if creates := auditsOnRun(e.srv, "run.create", out.ID); len(creates) != 1 || auditData(t, creates[0])["idempotency_key_sha256"] != keySHA(key) {
		t.Errorf("run.create rows = %+v, want one carrying the key's SHA-256", creates)
	}
}

// TestCreateRunKeyInvalid400: a value outside 1-255 visible ASCII characters is refused before anything
// is kept or made; an RFC 8941 string in quotes is accepted without its quotes.
func TestCreateRunKeyInvalid400(t *testing.T) {
	srv, pool := pgHarness(t)
	for name, v := range map[string]string{
		"empty": "", "space": "a b", "non-ASCII": "café", "too long": strings.Repeat("k", 256),
		"control": "a\x7f", "inner quote": `"a"b"`, "escape": `"a\"b"`,
	} {
		body, task := uniqueBody()
		w := keyedPost(t, srv, http.MethodPost, "/api/v1/runs", body, bearerAuth(adminToken), map[string]string{"Idempotency-Key": v})
		if w.Code != http.StatusBadRequest || errorReason(w) != "idempotency_key_invalid" {
			t.Errorf("%s = %d %s, want 400 idempotency_key_invalid", name, w.Code, w.Body)
		}
		if n := runsTitled(t, pool, task); n != 0 {
			t.Errorf("%s made %d runs", name, n)
		}
	}
	body, task := uniqueBody()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/runs", strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+adminToken)
	r.Header.Add("Idempotency-Key", "one")
	r.Header.Add("Idempotency-Key", "two")
	r.Host, r.RemoteAddr = "127.0.0.1", "127.0.0.1:54321"
	w := httptest.NewRecorder()
	panicFails(t, srv.Handler()).ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest || errorReason(w) != "idempotency_key_invalid" || runsTitled(t, pool, task) != 0 {
		t.Errorf("two values = %d %s, want 400 idempotency_key_invalid and no run", w.Code, w.Body)
	}

	key := uuid.NewString()
	body, _ = uniqueBody()
	if w := keyedPost(t, srv, http.MethodPost, "/api/v1/runs", body, bearerAuth(adminToken), map[string]string{"Idempotency-Key": `"` + key + `"`}); w.Code != http.StatusCreated {
		t.Fatalf("quoted = %d %s", w.Code, w.Body)
	}
	if n := keyRows(t, pool, key); n != 1 {
		t.Errorf("the quoted key is kept as %d rows under its unquoted value, want 1", n)
	}
	if w := postRunWithKey(t, srv, key, body); w.Header().Get("Idempotent-Replayed") != "true" {
		t.Errorf("the unquoted key = %d replayed=%q, want the quoted key's replay", w.Code, w.Header().Get("Idempotent-Replayed"))
	}
}

// TestHealthz_IdempotentRunCreate: /healthz says a POST /runs retry with a key is safe when the store
// keeps keys.
func TestHealthz_IdempotentRunCreate(t *testing.T) {
	srv, _ := pgHarness(t)
	w := keyedPost(t, srv, http.MethodGet, "/healthz", "", func(*http.Request) {}, nil)
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("healthz body: %v", err)
	}
	if body["idempotent_run_create"] != true {
		t.Errorf("idempotent_run_create = %v, want true", body["idempotent_run_create"])
	}
}

// TestCreateRunKeyReplayAfterRunTasksErasure: once a person's run tasks are erased, a same-key retry is
// rebuilt from the erased run (no task, title or description) and makes no second run; another
// person's kept answer is untouched.
func TestCreateRunKeyReplayAfterRunTasksErasure(t *testing.T) {
	srv, pool := pgHarness(t)
	ctx := context.Background()
	task := "idem-erase-" + uuid.NewString()
	body := `{"agent":"claude-code","repo":"acme/widgets","task":"` + task + `","title":"` + task + `-title","description":"` + task + `-desc"}`
	key := uuid.NewString()
	first := postRunWithKey(t, srv, key, body)
	if first.Code != http.StatusCreated || !strings.Contains(first.Body.String(), task) {
		t.Fatalf("first = %d %s, want a 201 carrying the task", first.Code, first.Body)
	}
	var run types.AgentRun
	if err := json.Unmarshal(first.Body.Bytes(), &run); err != nil {
		t.Fatal(err)
	}

	// Another person's keyed run with a kept answer.
	st := store.NewPG(pool)
	bob := store.RunCreateKeyID{Principal: "bob-" + uuid.NewString()[:8], KeySHA256: keySHA(uuid.NewString())}
	claim, err := st.ClaimRunCreateKey(ctx, store.RunCreateKey{RunCreateKeyID: bob, RequestSHA256: "r"}, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	bobRun := run
	bobRun.ID, bobRun.CreatedBy = uuid.New(), bob.Principal
	// Bob's own text, so the count of runs still carrying the caller's task sees only the caller's run.
	bobRun.Task, bobRun.Title, bobRun.Description = "bob-task", "bob-title", "bob-desc"
	bobRun.SPIFFEID = "spiffe://wardyn.local/agent-run/" + bobRun.ID.String()
	if _, err := st.CreateRun(store.WithRunCreateKey(ctx, bob, claim.Token), bobRun); err != nil {
		t.Fatal(err)
	}
	if ok, err := st.SaveRunCreateResponse(ctx, bob, claim.Token, 201, "/b", []byte(`{"task":"bob"}`)); err != nil || !ok {
		t.Fatalf("bob's save = %v %v", ok, err)
	}

	if _, err := srv.eraseRunTasks(ctx, run.CreatedBy); err != nil {
		t.Fatalf("erase run tasks: %v", err)
	}
	w := postRunWithKey(t, srv, key, body)
	if w.Code != http.StatusCreated || w.Header().Get("Idempotent-Replayed") != "true" || strings.Contains(w.Body.String(), task) {
		t.Fatalf("replay after the erase = %d replayed=%q %s; want a rebuilt 201 with no erased field", w.Code, w.Header().Get("Idempotent-Replayed"), w.Body)
	}
	if createdID(t, w) != run.ID || runsTitled(t, pool, task) != 0 {
		t.Errorf("replay names %s (want %s); runs still carrying the task = %d, want 0", createdID(t, w), run.ID, runsTitled(t, pool, task))
	}
	var runs int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM agent_runs WHERE created_by = $1 AND created_at >= $2`, run.CreatedBy, run.CreatedAt).Scan(&runs); err != nil || runs != 1 {
		t.Errorf("runs created by the caller since the first = %d (%v), want 1", runs, err)
	}
	var bound uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT run_id FROM run_create_keys WHERE key_sha256 = $1 AND response_body IS NULL`, keySHA(key)).Scan(&bound); err != nil || bound != run.ID {
		t.Errorf("the key after the erase names %s (%v), want %s with no kept body", bound, err, run.ID)
	}
	var bobBody []byte
	if err := pool.QueryRow(ctx, `SELECT response_body FROM run_create_keys WHERE principal = $1 AND key_sha256 = $2`, bob.Principal, bob.KeySHA256).Scan(&bobBody); err != nil || string(bobBody) != `{"task":"bob"}` {
		t.Errorf("bob's kept answer = %q (%v), want it untouched", bobBody, err)
	}
}
