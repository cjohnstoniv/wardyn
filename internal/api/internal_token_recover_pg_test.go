// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/federation"
	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/identity/embedded"
	"github.com/cjohnstoniv/wardyn/internal/identity/identitytest"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// rdPerson is the human subject of every token these tests sign.
const rdPerson = "rd-person@example.com"

// rdSVID is the claim body of a run token, in the shape the embedded provider signs and verifies.
type rdSVID struct {
	jwt.Claims
	Act struct {
		Sub string `json:"sub"`
	} `json:"act"`
	Sponsor string `json:"sponsor"`
}

// rdFixture is a Server over a database of its own, with an embedded identity provider whose key the test holds, so
// it can sign a token that has already expired, and one claimed runner.
type rdFixture struct {
	srv     *Server
	handler http.Handler
	st      store.PG
	audit   *syncAudit
	key     *ecdsa.PrivateKey
	idp     *embedded.Provider
	rn      types.Runner
	peers   atomic.Int64
}

func rdNewFixture(t *testing.T) *rdFixture {
	t.Helper()
	ctx := t.Context()
	st := store.NewPG(throwawayPGPool(t))
	h := newHarness(t)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	rec := &syncAudit{}
	idp, err := embedded.New(key, "wardyn.local", identitytest.NewMemRevocationStore(), rec)
	if err != nil {
		t.Fatal(err)
	}
	f := &rdFixture{srv: h.srv, st: st, audit: rec, key: key, idp: idp}
	f.srv.cfg.Store = st
	f.srv.cfg.Identity = idp
	f.srv.cfg.Audit = rec

	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	id := uuid.New()
	rn, err := st.CreateRunner(ctx, types.Runner{ID: id, Owner: "rd-owner", Name: "laptop", PublicKey: pub, KeyFingerprint: id.String(),
		Version: "0.9.0", OrgURLSHA256: federation.OrgURLSHA256(admissionOrgURL), Limits: roomyRunnerLimits()})
	if err != nil {
		t.Fatal(err)
	}
	if f.rn, err = st.ClaimRunner(ctx, rn.ID, rn.Owner, rn.KeyFingerprint, time.Now()); err != nil {
		t.Fatal(err)
	}
	f.handler = panicFails(t, f.srv.Handler())
	return f
}

func (f *rdFixture) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := f.st.Pool.Exec(t.Context(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

// rdRunOpts describe one run. The zero value is a RUNNING server run whose token expired 10 minutes ago.
type rdRunOpts struct {
	onRunner   bool           // placed on the fixture's runner, so every request for it comes over that runner's relay
	renewedAgo time.Duration  // when its token was last stamped; 70 minutes when zero
	state      types.RunState // RUNNING when empty
	endsIn     *time.Duration // its end, from now; none when nil
	capIn      *time.Duration // its lifetime cap, from now; none when nil
	terms      *types.OutageTerms
	pauses     bool // its proxy reported that it pauses and recovers
}

// rdRun is a stored run and the expired token its proxy holds.
type rdRun struct {
	run   types.AgentRun
	jti   string
	token string
}

func (f *rdFixture) newRun(t *testing.T, o rdRunOpts) rdRun {
	t.Helper()
	ctx := t.Context()
	if o.renewedAgo == 0 {
		o.renewedAgo = 70 * time.Minute
	}
	if o.state == "" {
		o.state = types.RunRunning
	}
	now := time.Now().UTC()
	r := types.AgentRun{
		ID: uuid.New(), CreatedAt: now.Add(-48 * time.Hour), UpdatedAt: now, CreatedBy: rdPerson,
		Agent: "claude-code", Task: "recover", ConfinementClass: types.CC2, State: types.RunRunning,
		RunnerTarget: "docker", SandboxRef: "wardyn-agent-" + uuid.NewString()[:8],
	}
	if o.onRunner {
		r.CreatedBy, r.Placement, r.RunnerID = f.rn.Owner, types.PlacementLocal, &f.rn.ID
		r.SandboxRef = "runner:" + f.rn.ID.String() + "/sandbox"
	}
	if o.endsIn != nil {
		at := now.Add(*o.endsIn)
		r.EndsAt = &at
	}
	if o.capIn != nil {
		at := now.Add(*o.capIn)
		r.LifetimeCapAt, r.LifetimeCapSource = &at, types.LimitSourceDeployment
	}
	run, err := f.st.CreateRun(ctx, r)
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	jti := "rd-" + uuid.NewString()
	f.exec(t, `UPDATE agent_runs SET state = $2, token_renewed_at = now() - $3::interval, token_jti = $4, token_prev_jti = NULL WHERE id = $1`,
		run.ID, string(o.state), o.renewedAgo.String(), jti)
	if o.terms != nil {
		if _, err := f.st.SetRunOutageTerms(ctx, run.ID, *o.terms); err != nil {
			t.Fatal(err)
		}
	}
	if o.pauses {
		if err := f.st.SetRunProxyPauses(ctx, run.ID, true); err != nil {
			t.Fatal(err)
		}
	}
	// The token expires an hour after the stamp the database holds, as the run's real one does.
	return rdRun{run: run, jti: jti, token: f.token(t, run.ID, jti, f.lineage(t, run.ID).RenewedAt.Add(identity.RunTokenTTL))}
}

// token signs a run token for runID that expires at expiry, which may be in the past.
func (f *rdFixture) token(t *testing.T, runID uuid.UUID, jti string, expiry time.Time) string {
	t.Helper()
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES256, Key: f.key}, (&jose.SignerOptions{}).WithType("JWT"))
	if err != nil {
		t.Fatal(err)
	}
	path, err := identity.RunPath(identity.Workload{}, runID)
	if err != nil {
		t.Fatal(err)
	}
	c := rdSVID{Claims: jwt.Claims{
		Issuer: "spiffe://wardyn.local", Subject: rdPerson, Audience: jwt.Audience{internalAudience}, ID: jti,
		IssuedAt: jwt.NewNumericDate(expiry.Add(-identity.RunTokenTTL)), NotBefore: jwt.NewNumericDate(expiry.Add(-identity.RunTokenTTL)),
		Expiry: jwt.NewNumericDate(expiry),
	}, Sponsor: rdPerson}
	c.Act.Sub = "spiffe://wardyn.local" + path
	tok, err := jwt.Signed(signer).Claims(c).Serialize()
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func (f *rdFixture) lineage(t *testing.T, id uuid.UUID) store.TokenLineage {
	t.Helper()
	l, err := f.st.RunRecoveryLineage(t.Context(), id)
	if err != nil {
		t.Fatalf("RunRecoveryLineage: %v", err)
	}
	return l
}

// window records an outage window that began startedAgo ago on the database's clock and ended endedAgo ago, or is
// still open when endedAgo is nil. runner is the link's runner for a runner_link window.
func (f *rdFixture) window(t *testing.T, kind string, runner *uuid.UUID, startedAgo time.Duration, endedAgo *time.Duration) {
	t.Helper()
	var ended *string
	if endedAgo != nil {
		s := endedAgo.String()
		ended = &s
	}
	f.exec(t, `INSERT INTO outage_windows (kind, runner_id, started_at, ended_at) VALUES ($1, $2, now() - $3::interval, now() - $4::interval)`,
		kind, runner, startedAgo.String(), ended)
}

// rdAnswer is what the door answered.
type rdAnswer struct {
	code   int
	reason string
	token  string
	retry  string
}

// ask posts a recovery for token, over runner via's relay when via is set, as the run's proxy does.
func (f *rdFixture) ask(t *testing.T, token string, via uuid.UUID) rdAnswer {
	t.Helper()
	return f.askInto(t, token, via, nil)
}

// askDropped is ask whose answer never reaches the caller: the status is recorded, the body is lost.
func (f *rdFixture) askDropped(t *testing.T, token string, via uuid.UUID) int {
	t.Helper()
	return f.askInto(t, token, via, func(w *httptest.ResponseRecorder) http.ResponseWriter { return rdDroppedBody{w} }).code
}

// rdDroppedBody is a response writer whose body write fails, as a connection that reset after the status was sent.
type rdDroppedBody struct{ *httptest.ResponseRecorder }

func (rdDroppedBody) Write([]byte) (int, error) { return 0, errors.New("connection reset by peer") }

// askInto is safe to call from a goroutine: it reports through t.Errorf, never t.Fatal.
func (f *rdFixture) askInto(t *testing.T, token string, via uuid.UUID, wrap func(*httptest.ResponseRecorder) http.ResponseWriter) rdAnswer {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/internal/token/recover", nil)
	r.RemoteAddr = fmt.Sprintf("192.0.2.%d:4000", f.peers.Add(1)%250+1) // each ask its own peer, so no per-address limit meets a long run of them
	if via != uuid.Nil {
		r = r.WithContext(context.WithValue(audit.WithRelay(r.Context(), via), relayRunnerKey{}, via))
	}
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	var rw http.ResponseWriter = w
	if wrap != nil {
		rw = wrap(w)
	}
	f.handler.ServeHTTP(rw, r)
	a := rdAnswer{code: w.Code, retry: w.Header().Get("Retry-After")}
	var body struct {
		Token  string `json:"token"`
		Reason string `json:"reason"`
	}
	if w.Body.Len() > 0 {
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Errorf("recover answer %d is not JSON: %v: %s", w.Code, err, w.Body)
		}
	}
	a.token, a.reason = body.Token, body.Reason
	return a
}

// successRows is how many run.token.recover rows the run has.
func (f *rdFixture) successRows(runID uuid.UUID) int {
	return len(f.audit.eventsFor(runID, "run.token.recover"))
}

// deniedRows is how many identity.recover denied rows the run has for reason.
func (f *rdFixture) deniedRows(t *testing.T, runID uuid.UUID, reason string) int {
	t.Helper()
	n := 0
	for _, ev := range f.audit.eventsFor(runID, "identity.recover") {
		if ev.Outcome == "denied" && leaseAuditData(t, ev)["reason"] == reason {
			n++
		}
	}
	return n
}

func rdSameLineage(a, b store.TokenLineage) bool {
	return a.JTI == b.JTI && a.PrevJTI == b.PrevJTI && a.RenewedAt.Equal(b.RenewedAt)
}

// rdRefusal is what a refused recovery must look like.
type rdRefusal struct {
	code      int
	reason    string // the wire reason
	audit     string // the audit row's reason; reason when empty
	successes int    // run.token.recover rows the run has in all
}

// wantRefused asserts the status, the reason, that no token came back, the one identity.recover denied row, and that
// the run's lineage is the one it was before the ask.
func (f *rdFixture) wantRefused(t *testing.T, run rdRun, before store.TokenLineage, a rdAnswer, w rdRefusal) {
	t.Helper()
	auditReason := w.audit
	if auditReason == "" {
		auditReason = w.reason
	}
	if a.code != w.code || a.reason != w.reason {
		t.Fatalf("recover = %d %q, want %d %q", a.code, a.reason, w.code, w.reason)
	}
	if a.token != "" {
		t.Fatalf("a refused recovery (%s) handed out a token", w.reason)
	}
	if n := f.successRows(run.run.ID); n != w.successes {
		t.Fatalf("run.token.recover rows = %d, want %d", n, w.successes)
	}
	if n := f.deniedRows(t, run.run.ID, auditReason); n != 1 {
		t.Fatalf("identity.recover denied rows for %q = %d, want 1", auditReason, n)
	}
	if after := f.lineage(t, run.run.ID); !rdSameLineage(before, after) {
		t.Fatalf("a refused recovery changed the run's token lineage: %+v -> %+v", before, after)
	}
}

// wantRecovered asserts a 200 with a token the provider verifies for the run, the one run.token.recover row with its
// keys and the window's kind, and a lineage that moved to the new token with the presented one cut.
func (f *rdFixture) wantRecovered(t *testing.T, run rdRun, before store.TokenLineage, a rdAnswer, windowKind string) {
	t.Helper()
	if a.code != http.StatusOK || a.token == "" {
		t.Fatalf("recover = %d %q with token %v, want 200 and a token", a.code, a.reason, a.token != "")
	}
	claims, err := f.idp.Verify(t.Context(), a.token, internalAudience)
	if err != nil {
		t.Fatalf("the recovered token does not verify: %v", err)
	}
	if claims.RunID != run.run.ID || claims.JTI == run.jti {
		t.Fatalf("recovered token is for run %s jti %s, want run %s with a new jti", claims.RunID, claims.JTI, run.run.ID)
	}
	rows := f.audit.eventsFor(run.run.ID, "run.token.recover")
	if len(rows) != 1 {
		t.Fatalf("run.token.recover rows = %d, want 1", len(rows))
	}
	if rows[0].Outcome != "success" || rows[0].Target != claims.JTI {
		t.Fatalf("run.token.recover row = outcome %q target %q, want success for the new jti %q", rows[0].Outcome, rows[0].Target, claims.JTI)
	}
	d := leaseAuditData(t, rows[0])
	for _, k := range []string{"prev_jti", "expired_for_s", "window_kind", "window_id", "expires_at"} {
		if _, ok := d[k]; !ok {
			t.Errorf("run.token.recover data %v has no %q", d, k)
		}
	}
	if d["window_kind"] != windowKind || d["prev_jti"] != run.jti {
		t.Errorf("run.token.recover data = %v, want window_kind %q and prev_jti %q", d, windowKind, run.jti)
	}
	after := f.lineage(t, run.run.ID)
	if after.JTI != claims.JTI || after.PrevJTI != "" || !after.RenewedAt.After(before.RenewedAt) {
		t.Fatalf("lineage after = %+v, want %q alone (the presented token cut) and a newer stamp than %v", after, claims.JTI, before.RenewedAt)
	}
}

// TestRecover_ReplayAfterRecoveryRefused_PG: a recovery cuts the presented token from the lineage, so it can never be replayed.
func TestRecover_ReplayAfterRecoveryRefused_PG(t *testing.T) {
	f := rdNewFixture(t)
	run := f.newRun(t, rdRunOpts{pauses: true})
	f.window(t, store.OutageKindServer, nil, time.Hour, nil)

	before := f.lineage(t, run.run.ID)
	f.wantRecovered(t, run, before, f.ask(t, run.token, uuid.Nil), store.OutageKindServer)

	after := f.lineage(t, run.run.ID)
	replay := f.ask(t, run.token, uuid.Nil)
	f.wantRefused(t, run, after, replay, rdRefusal{code: http.StatusServiceUnavailable, reason: reasonTokenLineageRetired, successes: 1})
}

// TestRecover_PrevJTIRefused_PG: the previous token of a renewal is never accepted at the door.
func TestRecover_PrevJTIRefused_PG(t *testing.T) {
	f := rdNewFixture(t)
	run := f.newRun(t, rdRunOpts{pauses: true})
	f.window(t, store.OutageKindServer, nil, time.Hour, nil)
	f.exec(t, `UPDATE agent_runs SET token_jti = 'rd-current', token_prev_jti = $2 WHERE id = $1`, run.run.ID, run.jti)

	before := f.lineage(t, run.run.ID)
	if before.JTI != "rd-current" || before.PrevJTI != run.jti {
		t.Fatalf("fixture lineage = %+v", before)
	}
	a := f.ask(t, run.token, uuid.Nil)
	f.wantRefused(t, run, before, a, rdRefusal{code: http.StatusServiceUnavailable, reason: reasonTokenLineageRetired})
}

// TestRecover_EmptyLineageAccepted_PG: a run with no token recorded accepts its presenter.
func TestRecover_EmptyLineageAccepted_PG(t *testing.T) {
	f := rdNewFixture(t)
	run := f.newRun(t, rdRunOpts{pauses: true})
	f.window(t, store.OutageKindServer, nil, time.Hour, nil)
	f.exec(t, `UPDATE agent_runs SET token_jti = NULL, token_prev_jti = NULL WHERE id = $1`, run.run.ID)

	before := f.lineage(t, run.run.ID)
	if before.JTI != "" || before.PrevJTI != "" {
		t.Fatalf("fixture lineage = %+v, want empty", before)
	}
	f.wantRecovered(t, run, before, f.ask(t, run.token, uuid.Nil), store.OutageKindServer)
}

// TestRecover_LostAnswerFailsClosed_PG: a recovery whose answer was lost left the new token with nobody; the retry gets no token, and neither does any
// later one. Only a revive runs the run again.
func TestRecover_LostAnswerFailsClosed_PG(t *testing.T) {
	f := rdNewFixture(t)
	run := f.newRun(t, rdRunOpts{pauses: true})
	f.window(t, store.OutageKindServer, nil, time.Hour, nil)

	if code := f.askDropped(t, run.token, uuid.Nil); code != http.StatusOK {
		t.Fatalf("recover whose answer was dropped = %d, want 200 (the stamp landed)", code)
	}
	rows := f.audit.eventsFor(run.run.ID, "run.token.recover")
	if len(rows) != 1 {
		t.Fatalf("run.token.recover rows = %d, want 1", len(rows))
	}
	stamped := f.lineage(t, run.run.ID)
	if stamped.JTI != rows[0].Target || stamped.JTI == run.jti {
		t.Fatalf("lineage = %+v, want the dropped answer's jti %q", stamped, rows[0].Target)
	}
	for range 2 {
		retry := f.ask(t, run.token, uuid.Nil)
		f.wantRefused(t, run, stamped, retry, rdRefusal{code: http.StatusServiceUnavailable, reason: reasonTokenLineageRetired, successes: 1})
	}
}

// TestRecover_RacesLoseRun_PG: a recovery's stamp and a loss mark are single-row updates on the same row, and exactly one of them wins; the
// recovery that loses hands out no token.
func TestRecover_RacesLoseRun_PG(t *testing.T) {
	f := rdNewFixture(t)
	f.window(t, store.OutageKindServer, nil, 2*time.Hour, nil)
	recoveries, losses := 0, 0
	for round := range 50 {
		run := f.newRun(t, rdRunOpts{pauses: true})
		before := f.lineage(t, run.run.ID)
		var (
			wg   sync.WaitGroup
			a    rdAnswer
			lost bool
			lerr error
		)
		start := make(chan struct{})
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			a = f.ask(t, run.token, uuid.Nil)
		}()
		go func() {
			defer wg.Done()
			<-start
			// The renew's own loss mark: it re-requires the token to be lapsed, so a stamp that lands first wins.
			lost, lerr = f.st.MarkRunLost(t.Context(), run.run.ID, types.LostOutage, time.Now(), identity.RunTokenLapseAfter)
		}()
		close(start)
		wg.Wait()
		if lerr != nil {
			t.Fatalf("round %d: MarkRunLost: %v", round, lerr)
		}
		got, err := f.st.GetRun(t.Context(), run.run.ID)
		if err != nil {
			t.Fatal(err)
		}
		recovered := a.code == http.StatusOK
		if recovered == lost {
			t.Fatalf("round %d: recovery answered %d %q and MarkRunLost = %v; exactly one must win", round, a.code, a.reason, lost)
		}
		after := f.lineage(t, run.run.ID)
		if lost {
			losses++
			if a.token != "" || a.code != http.StatusForbidden || a.reason != string(authz.ReasonRunKept) {
				t.Fatalf("round %d: the recovery that lost answered %d %q with token %v, want 403 %q and none", round, a.code, a.reason, a.token != "", authz.ReasonRunKept)
			}
			if got.LostAt == nil || !rdSameLineage(before, after) || f.successRows(run.run.ID) != 0 {
				t.Fatalf("round %d: lost run %+v, lineage %+v -> %+v, success rows %d; want it lost with its lineage and no success row", round, got.LostAt, before, after, f.successRows(run.run.ID))
			}
			continue
		}
		recoveries++
		if a.token == "" || got.LostAt != nil || got.State != types.RunRunning || after.JTI == run.jti || f.successRows(run.run.ID) != 1 {
			t.Fatalf("round %d: recovered run lost_at %v state %s lineage %+v token %v success rows %d; want a live run on a new token",
				round, got.LostAt, got.State, after, a.token != "", f.successRows(run.run.ID))
		}
	}
	t.Logf("50 rounds: %d recoveries won, %d losses won", recoveries, losses)
}

// TestRecover_WindowPendingIsRetryable_PG: a recovery that finds no window yet is retryable: 503 with Retry-After, and once the outage window writer has
// recorded one the next ask is answered.
func TestRecover_WindowPendingIsRetryable_PG(t *testing.T) {
	f := rdNewFixture(t)
	run := f.newRun(t, rdRunOpts{pauses: true})

	before := f.lineage(t, run.run.ID)
	a := f.ask(t, run.token, uuid.Nil)
	f.wantRefused(t, run, before, a, rdRefusal{code: http.StatusServiceUnavailable, reason: reasonOutageWindowPending})
	if a.retry == "" {
		t.Fatal("a pending refusal carries no Retry-After")
	}

	if err := f.st.OpenServerOutageWindow(t.Context(), time.Hour, map[string]any{"server_runs": 2}); err != nil {
		t.Fatal(err)
	}
	f.wantRecovered(t, run, before, f.ask(t, run.token, uuid.Nil), store.OutageKindServer)
}

// TestRecover_CutLineageNeverRecovers_PG: a lineage cut at a stopped pause is never recovered, stamped or held, and the door needs no gate of the
// proxy-pause change to know it.
func TestRecover_CutLineageNeverRecovers_PG(t *testing.T) {
	f := rdNewFixture(t)
	ctx := t.Context()
	run := f.newRun(t, rdRunOpts{onRunner: true, pauses: true})
	f.window(t, store.OutageKindRunnerLink, &f.rn.ID, time.Hour, nil)
	const sentinel = store.TokenCutPrefix + "3"
	f.exec(t, `UPDATE agent_runs SET token_jti = $2, token_prev_jti = NULL WHERE id = $1`, run.run.ID, sentinel)
	before := f.lineage(t, run.run.ID)
	if before.JTI != sentinel {
		t.Fatalf("fixture lineage = %+v", before)
	}

	// The door: 403 run_token_cut, no token, no success row.
	a := f.ask(t, run.token, f.rn.ID)
	f.wantRefused(t, run, before, a, rdRefusal{code: http.StatusForbidden, reason: reasonRunTokenCut})

	// The stamp refuses the presented token and the sentinel itself.
	for _, presented := range []string{run.jti, sentinel} {
		if ok, err := f.st.StampRunTokenRecovered(ctx, run.run.ID, "rd-fresh", presented); ok || err != nil {
			t.Fatalf("StampRunTokenRecovered(presented %q) = %v, %v; want false", presented, ok, err)
		}
	}
	if after := f.lineage(t, run.run.ID); !rdSameLineage(before, after) {
		t.Fatalf("a refused stamp changed the lineage: %+v -> %+v", before, after)
	}

	// The recovery facts: cut, and so never held.
	check, err := f.st.RunRecoveryCheck(ctx, run.run.ID, false)
	if err != nil || !check.Cut || check.Held {
		t.Fatalf("RunRecoveryCheck = %+v, %v; want Cut and not Held", check, err)
	}

	// The STATE page with the sandbox present still needs reconciliation: a cut run is not held, and its token has lapsed.
	row := runnerwire.RunState{RunID: run.run.ID, Ref: run.run.SandboxRef, SandboxPresent: true}
	if err := finalStatePage(t, f.srv, f.rn.ID, row); !errors.Is(err, store.ErrRunnerReconciliationRequired) {
		t.Fatalf("STATE page for a cut run with its sandbox present = %v, want reconciliation required", err)
	}

	// The sweeps leave it to their own action.
	stored, err := f.st.GetRun(ctx, run.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := f.srv.outageRecoveryFor(ctx, stored, lapseVerdict{}, false); got != outageRecoveryNone {
		t.Fatalf("outageRecoveryFor(cut run) = %d, want none (%d)", got, outageRecoveryNone)
	}

	// A fresh lineage (a revive's) makes the old token a retired one: retryable, still no token.
	if jti, _, err := f.st.RetireRunTokenLineage(ctx, run.run.ID, "rd-fresh"); err != nil || jti != sentinel {
		t.Fatalf("RetireRunTokenLineage = %q, %v; want the sentinel back", jti, err)
	}
	fresh := f.lineage(t, run.run.ID)
	retired := f.ask(t, run.token, f.rn.ID)
	f.wantRefused(t, run, fresh, retired, rdRefusal{code: http.StatusServiceUnavailable, reason: reasonTokenLineageRetired})
}

// rdPlainIdentity is an identity provider with no way to verify an expired token: the embedded provider behind the
// plain identity.Provider interface, as a SPIRE-backed server has it.
type rdPlainIdentity struct{ identity.Provider }

// begin starts proxy generation gen for the run on token jti.
func (f *rdFixture) begin(t *testing.T, run rdRun, gen int, jti string) {
	t.Helper()
	if _, err := f.st.BeginProxyStart(t.Context(), run.run.ID, gen, jti, store.ProxyStartExpected, false); err != nil {
		t.Fatalf("BeginProxyStart(%d): %v", gen, err)
	}
}

// postLoaded posts a proxy's loaded report as the proxy whose run token carries jti, past internalAuth. A nil
// switches sends none.
func (f *rdFixture) postLoaded(t *testing.T, runID uuid.UUID, jti string, switches map[string]string) (int, string) {
	t.Helper()
	body := map[string]any{"config_sha256": strings.Repeat("a", 64), "proxy_version": "test"}
	if switches != nil {
		body["switches"] = switches
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/internal/proxy/loaded", strings.NewReader(string(raw)))
	req = req.WithContext(contextWithClaims(req.Context(), &identity.Claims{RunID: runID, JTI: jti}))
	w := httptest.NewRecorder()
	f.srv.handleInternalProxyLoaded(w, req)
	return w.Code, w.Body.String()
}

func (f *rdFixture) proxyPauses(t *testing.T, id uuid.UUID) bool {
	t.Helper()
	terms, err := f.st.RunOutageTerms(t.Context(), id)
	if err != nil {
		t.Fatalf("RunOutageTerms: %v", err)
	}
	return terms.ProxyPauses
}

func rdIs2xx(code int) bool { return code >= 200 && code < 300 }

var rdPauseOn = map[string]string{"outage_pause": "on"}

// TestProxyLoaded_MarksAndClearsPauses_PG: a loaded report from the active start sets the flag both ways, and a later start clears it before its own
// report says otherwise.
func TestProxyLoaded_MarksAndClearsPauses_PG(t *testing.T) {
	f := rdNewFixture(t)
	run := f.newRun(t, rdRunOpts{})
	id := run.run.ID

	f.begin(t, run, 1, "J1")
	if code, body := f.postLoaded(t, id, "J1", rdPauseOn); !rdIs2xx(code) || !f.proxyPauses(t, id) {
		t.Fatalf("report with the switch on = %d %s, flag %v; want 2xx and true", code, body, f.proxyPauses(t, id))
	}
	// The same start reports again without the switch: the flag is written, not only set.
	if code, body := f.postLoaded(t, id, "J1", map[string]string{"outage_pause": "off"}); !rdIs2xx(code) || f.proxyPauses(t, id) {
		t.Fatalf("report with the switch off = %d %s, flag %v; want 2xx and false", code, body, f.proxyPauses(t, id))
	}
	if code, body := f.postLoaded(t, id, "J1", rdPauseOn); !rdIs2xx(code) || !f.proxyPauses(t, id) {
		t.Fatalf("report with the switch on again = %d %s, flag %v; want 2xx and true", code, body, f.proxyPauses(t, id))
	}

	// A new start clears it, and its report without the switch (an older proxy) leaves it false.
	f.begin(t, run, 2, "J2")
	if f.proxyPauses(t, id) {
		t.Fatal("a new proxy start left the flag true")
	}
	if code, body := f.postLoaded(t, id, "J2", nil); !rdIs2xx(code) || f.proxyPauses(t, id) {
		t.Fatalf("report with no switches = %d %s, flag %v; want 2xx and false", code, body, f.proxyPauses(t, id))
	}
	if code, body := f.postLoaded(t, id, "J2", rdPauseOn); !rdIs2xx(code) || !f.proxyPauses(t, id) {
		t.Fatalf("later start's report with the switch on = %d %s, flag %v; want 2xx and true", code, body, f.proxyPauses(t, id))
	}
}

// TestProxyLoaded_StaleReportDoesNotMark_PG: a report outside the active start's token lineage changes nothing, the flag included.
func TestProxyLoaded_StaleReportDoesNotMark_PG(t *testing.T) {
	f := rdNewFixture(t)
	run := f.newRun(t, rdRunOpts{})
	id := run.run.ID
	f.begin(t, run, 1, "J1")

	code, body := f.postLoaded(t, id, "J-old", rdPauseOn)
	if code != http.StatusOK || !strings.Contains(body, "stale") {
		t.Fatalf("stale report = %d %s, want 200 stale", code, body)
	}
	if f.proxyPauses(t, id) {
		t.Fatal("a stale report marked the run's proxy as pausing")
	}

	// And it does not clear what the active start's report set.
	if code, body := f.postLoaded(t, id, "J1", rdPauseOn); !rdIs2xx(code) || !f.proxyPauses(t, id) {
		t.Fatalf("active report = %d %s, flag %v; want 2xx and true", code, body, f.proxyPauses(t, id))
	}
	if code, body := f.postLoaded(t, id, "J-old", nil); code != http.StatusOK || !strings.Contains(body, "stale") {
		t.Fatalf("second stale report = %d %s, want 200 stale", code, body)
	}
	if !f.proxyPauses(t, id) {
		t.Fatal("a stale report cleared the flag the active start set")
	}
}

// TestProxyLoaded_NoRecoveryVerifierWritesFalse_PG: a server whose identity provider cannot verify an expired token cannot recover a run, so a proxy's switch on
// still leaves the flag false, and the sweep's verdict stays the one it had before recovery existed.
func TestProxyLoaded_NoRecoveryVerifierWritesFalse_PG(t *testing.T) {
	f := rdNewFixture(t)
	ctx := t.Context()
	f.srv.cfg.Identity = rdPlainIdentity{f.idp}
	if f.srv.recoverySupported() {
		t.Fatal("a server whose identity provider verifies no expired token claims it can recover")
	}
	run := f.newRun(t, rdRunOpts{})
	id := run.run.ID
	f.window(t, store.OutageKindServer, nil, time.Hour, nil)
	f.begin(t, run, 1, "J1")

	if code, body := f.postLoaded(t, id, "J1", rdPauseOn); !rdIs2xx(code) {
		t.Fatalf("report = %d %s, want 2xx", code, body)
	}
	if f.proxyPauses(t, id) {
		t.Fatal("the switch is on, but a server that cannot recover recorded the proxy as pausing")
	}
	stored, err := f.st.GetRun(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got := f.srv.outageRecoveryFor(ctx, stored, lapseVerdict{}, true); got != outageRecoveryNone {
		t.Fatalf("outageRecoveryFor with the flag false = %d, want none (%d)", got, outageRecoveryNone)
	}

	// Even a flag forced on is not held on such a server; with a provider that can recover, it is.
	if err := f.st.SetRunProxyPauses(ctx, id, true); err != nil {
		t.Fatal(err)
	}
	if got := f.srv.outageRecoveryFor(ctx, stored, lapseVerdict{}, true); got != outageRecoveryNone {
		t.Fatalf("outageRecoveryFor with no recovery verifier = %d, want none (%d)", got, outageRecoveryNone)
	}
	f.srv.cfg.Identity = f.idp
	if got := f.srv.outageRecoveryFor(ctx, stored, lapseVerdict{}, true); got != outageRecoveryHold {
		t.Fatalf("outageRecoveryFor with a recovering provider = %d, want hold (%d)", got, outageRecoveryHold)
	}
}

// rdPausesFailOnce is a store whose first SetRunProxyPauses fails.
type rdPausesFailOnce struct {
	store.PG
	failed atomic.Bool
}

func (s *rdPausesFailOnce) SetRunProxyPauses(ctx context.Context, id uuid.UUID, pauses bool) error {
	if s.failed.CompareAndSwap(false, true) {
		return errors.New("write failed")
	}
	return s.PG.SetRunProxyPauses(ctx, id, pauses)
}

// TestProxyLoaded_PausesWriteFailureAnswers503_PG: a report whose flag could not be written answers 503, so the proxy reports again; the repeat answers 2xx and
// sets the flag.
func TestProxyLoaded_PausesWriteFailureAnswers503_PG(t *testing.T) {
	f := rdNewFixture(t)
	run := f.newRun(t, rdRunOpts{})
	id := run.run.ID
	f.begin(t, run, 1, "J1")
	f.srv.cfg.Store = &rdPausesFailOnce{PG: f.st}

	code, body := f.postLoaded(t, id, "J1", rdPauseOn)
	if code != http.StatusServiceUnavailable || !strings.Contains(body, reasonProxyReportUnrecorded) {
		t.Fatalf("report whose flag write failed = %d %s, want 503 %s", code, body, reasonProxyReportUnrecorded)
	}
	if f.proxyPauses(t, id) {
		t.Fatal("the flag is set after its write failed")
	}
	if code, body := f.postLoaded(t, id, "J1", rdPauseOn); !rdIs2xx(code) || !f.proxyPauses(t, id) {
		t.Fatalf("repeated report = %d %s, flag %v; want 2xx and true", code, body, f.proxyPauses(t, id))
	}
}

// TestRecover_NotRunningRefused_PG: the door recovers only a RUNNING run.
func TestRecover_NotRunningRefused_PG(t *testing.T) {
	f := rdNewFixture(t)
	f.window(t, store.OutageKindServer, nil, time.Hour, nil)
	for _, state := range []types.RunState{types.RunStarting, types.RunWaiting} {
		t.Run(string(state), func(t *testing.T) {
			run := f.newRun(t, rdRunOpts{state: state, pauses: true})
			before := f.lineage(t, run.run.ID)
			a := f.ask(t, run.token, uuid.Nil)
			f.wantRefused(t, run, before, a, rdRefusal{code: http.StatusForbidden, reason: reasonRunNotRunning})
		})
	}
}

// TestRecover_KilledRunRefused_PG: a run that ended never regains its network, whatever its token's age and windows.
func TestRecover_KilledRunRefused_PG(t *testing.T) {
	f := rdNewFixture(t)
	f.window(t, store.OutageKindServer, nil, time.Hour, nil)
	run := f.newRun(t, rdRunOpts{state: types.RunKilled, pauses: true})
	before := f.lineage(t, run.run.ID)
	a := f.ask(t, run.token, uuid.Nil)
	f.wantRefused(t, run, before, a, rdRefusal{code: http.StatusForbidden, reason: string(authz.ReasonRunTerminal), audit: "run_terminal:" + string(types.RunKilled)})
}

// TestRecover_ExpiryOutsideWindowRefused_PG: a token that expired outside every recorded window is not recovered (503, retryable: a window may yet be
// recorded); one that expired inside a window is.
func TestRecover_ExpiryOutsideWindowRefused_PG(t *testing.T) {
	f := rdNewFixture(t)
	// A server window that ended 3 hours ago, after beginning 5 hours ago.
	f.window(t, store.OutageKindServer, nil, 5*time.Hour, dur(3*time.Hour))

	outside := f.newRun(t, rdRunOpts{pauses: true}) // expired 10 minutes ago
	before := f.lineage(t, outside.run.ID)
	a := f.ask(t, outside.token, uuid.Nil)
	f.wantRefused(t, outside, before, a, rdRefusal{code: http.StatusServiceUnavailable, reason: reasonOutageWindowPending})

	inside := f.newRun(t, rdRunOpts{pauses: true, renewedAgo: 5 * time.Hour}) // expired 4 hours ago
	f.wantRecovered(t, inside, f.lineage(t, inside.run.ID), f.ask(t, inside.token, uuid.Nil), store.OutageKindServer)
}

// TestRecover_BlipAtExpiryRefused_PG: a blip that began at the expiry, under 15 minutes before it, covers nothing; a window that began 15 minutes
// or more before it does.
func TestRecover_BlipAtExpiryRefused_PG(t *testing.T) {
	f := rdNewFixture(t)
	const expiredAgo = 10 * time.Minute
	// A 31 second control-plane blip: 15 s before the expiry to 16 s after it.
	f.window(t, store.OutageKindControlPlane, nil, expiredAgo+15*time.Second, dur(expiredAgo-16*time.Second))

	run := f.newRun(t, rdRunOpts{pauses: true})
	before := f.lineage(t, run.run.ID)
	a := f.ask(t, run.token, uuid.Nil)
	f.wantRefused(t, run, before, a, rdRefusal{code: http.StatusServiceUnavailable, reason: reasonOutageWindowPending})

	f.window(t, store.OutageKindControlPlane, nil, expiredAgo+16*time.Minute, dur(expiredAgo-16*time.Second))
	f.wantRecovered(t, run, before, f.ask(t, run.token, uuid.Nil), store.OutageKindControlPlane)
}

// TestRecover_OtherRunnersLinkWindowRefused_PG: another runner's link window covers nothing of this runner's run; this runner's own does.
func TestRecover_OtherRunnersLinkWindowRefused_PG(t *testing.T) {
	f := rdNewFixture(t)
	other := uuid.New()
	f.window(t, store.OutageKindRunnerLink, &other, time.Hour, nil)

	run := f.newRun(t, rdRunOpts{onRunner: true, pauses: true})
	before := f.lineage(t, run.run.ID)
	a := f.ask(t, run.token, f.rn.ID)
	f.wantRefused(t, run, before, a, rdRefusal{code: http.StatusServiceUnavailable, reason: reasonOutageWindowPending})

	f.window(t, store.OutageKindRunnerLink, &f.rn.ID, time.Hour, nil)
	f.wantRecovered(t, run, before, f.ask(t, run.token, f.rn.ID), store.OutageKindRunnerLink)
}

// TestRecover_ServerOutageWindowDoesNotCoverRunnerRun_PG: a server_outage window covers a server run and not a run on a runner.
func TestRecover_ServerOutageWindowDoesNotCoverRunnerRun_PG(t *testing.T) {
	f := rdNewFixture(t)
	f.window(t, store.OutageKindServer, nil, time.Hour, nil)

	run := f.newRun(t, rdRunOpts{onRunner: true, pauses: true})
	before := f.lineage(t, run.run.ID)
	a := f.ask(t, run.token, f.rn.ID)
	f.wantRefused(t, run, before, a, rdRefusal{code: http.StatusServiceUnavailable, reason: reasonOutageWindowPending})

	server := f.newRun(t, rdRunOpts{pauses: true})
	f.wantRecovered(t, server, f.lineage(t, server.run.ID), f.ask(t, server.token, uuid.Nil), store.OutageKindServer)
}

// TestRecover_StrictPastAllowanceRefused_PG: a strict run whose allowance from its last renewal has run out is never recovered, and the refusal is final
// (403); one inside its allowance is.
func TestRecover_StrictPastAllowanceRefused_PG(t *testing.T) {
	f := rdNewFixture(t)
	f.window(t, store.OutageKindServer, nil, time.Hour, nil)

	spent := f.newRun(t, rdRunOpts{pauses: true, terms: &types.OutageTerms{AllowanceSec: 1800, Strict: true}})
	before := f.lineage(t, spent.run.ID)
	a := f.ask(t, spent.token, uuid.Nil)
	f.wantRefused(t, spent, before, a, rdRefusal{code: http.StatusForbidden, reason: reasonStrictAllowanceSpent})

	inside := f.newRun(t, rdRunOpts{pauses: true, terms: &types.OutageTerms{AllowanceSec: 7200, Strict: true}})
	f.wantRecovered(t, inside, f.lineage(t, inside.run.ID), f.ask(t, inside.token, uuid.Nil), store.OutageKindServer)
}

// TestRecover_LostRunRefused_PG: a lost run never recovers, and the door never clears its mark.
func TestRecover_LostRunRefused_PG(t *testing.T) {
	f := rdNewFixture(t)
	f.window(t, store.OutageKindServer, nil, time.Hour, nil)
	run := f.newRun(t, rdRunOpts{pauses: true})
	f.exec(t, `UPDATE agent_runs SET lost_at = now(), lost_reason = 'outage' WHERE id = $1`, run.run.ID)

	before := f.lineage(t, run.run.ID)
	a := f.ask(t, run.token, uuid.Nil)
	f.wantRefused(t, run, before, a, rdRefusal{code: http.StatusForbidden, reason: string(authz.ReasonRunKept), audit: "run_lost:" + string(types.LostOutage)})
	got, err := f.st.GetRun(t.Context(), run.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.LostAt == nil || got.LostReason != types.LostOutage || got.State != types.RunRunning {
		t.Fatalf("run after the refusal = state %s lost_at %v reason %q; want RUNNING still lost to the outage", got.State, got.LostAt, got.LostReason)
	}
}

// TestRecover_RunnerHeldContainedRefused_PG: a run its runner holds contained gets no token, and so no lease.
func TestRecover_RunnerHeldContainedRefused_PG(t *testing.T) {
	f := rdNewFixture(t)
	ctx := t.Context()
	f.window(t, store.OutageKindRunnerLink, &f.rn.ID, time.Hour, nil)
	run := f.newRun(t, rdRunOpts{onRunner: true, pauses: true})

	// The runner applied a kill of the run and nothing has recovered it since.
	act, _, err := f.st.QueueRunnerAction(ctx, types.RunnerPendingAction{ID: uuid.New(), RunnerID: f.rn.ID, RunID: run.run.ID,
		Kind: types.RunnerActionKill, Ref: run.run.SandboxRef})
	if err != nil {
		t.Fatalf("QueueRunnerAction: %v", err)
	}
	prepare := func(_ context.Context, ev types.AuditEvent) (types.AuditEvent, error) { return ev, nil }
	if _, _, err := f.st.RecordRunnerActionResult(ctx, f.rn.ID, run.run.ID, act.ID, types.RunnerActionSuccess, "", time.Now().Add(-time.Second), prepare); err != nil {
		t.Fatalf("RecordRunnerActionResult: %v", err)
	}
	if held, err := f.st.RunHeldContainedOnRunner(ctx, run.run.ID); err != nil || !held {
		t.Fatalf("RunHeldContainedOnRunner = %v, %v; want held", held, err)
	}
	if pending, err := f.st.RunHasPendingRunnerAction(ctx, run.run.ID); err != nil || pending {
		t.Fatalf("RunHasPendingRunnerAction = %v, %v; want none, so the door is reached", pending, err)
	}

	before := f.lineage(t, run.run.ID)
	a := f.ask(t, run.token, f.rn.ID)
	f.wantRefused(t, run, before, a, rdRefusal{code: http.StatusForbidden, reason: reasonRunHeldContained})
}

// TestRecover_PastEndsAtRefused_PG: past its end, no recovery.
func TestRecover_PastEndsAtRefused_PG(t *testing.T) {
	f := rdNewFixture(t)
	f.window(t, store.OutageKindServer, nil, time.Hour, nil)
	run := f.newRun(t, rdRunOpts{pauses: true, endsIn: dur(-5 * time.Minute)})
	before := f.lineage(t, run.run.ID)
	a := f.ask(t, run.token, uuid.Nil)
	f.wantRefused(t, run, before, a, rdRefusal{code: http.StatusForbidden, reason: reasonRunPastLifetime})
}

// TestRecover_PastLifetimeCapRefused_PG: past its lifetime cap, no recovery.
func TestRecover_PastLifetimeCapRefused_PG(t *testing.T) {
	f := rdNewFixture(t)
	f.window(t, store.OutageKindServer, nil, time.Hour, nil)
	run := f.newRun(t, rdRunOpts{pauses: true, capIn: dur(-5 * time.Minute)})
	before := f.lineage(t, run.run.ID)
	a := f.ask(t, run.token, uuid.Nil)
	f.wantRefused(t, run, before, a, rdRefusal{code: http.StatusForbidden, reason: reasonRunPastLifetime})
}
