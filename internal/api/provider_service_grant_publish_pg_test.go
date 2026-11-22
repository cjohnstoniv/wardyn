// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// The service grant's publication against a real Postgres store, across one or two servers: the share locks a
// publication takes on the secret's row and the site configuration, the row it commits with its decision, the
// order of publications in the cache, and the settle that records a row the publication did not commit. A is
// the fixture's server; B is an independent replica (its own pool, store, sink and secret store) over the same
// database. Skipped without WARDYN_TEST_PG, like every _pg_test.go file here.

import (
	"context"
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

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// pubPGWait bounds every wait on a caller or a request: shorter than the attempt's bound, longer than any hold.
const pubPGWait = 20 * time.Second

// pubPGBounds shortens the publication bound and the audit chain lock timeout for one test (zero keeps a value).
// Call it before the fixture is created: the fixture's quiesce Cleanup then runs before this restore.
func pubPGBounds(t *testing.T, publish, chain time.Duration) {
	t.Helper()
	oldPublish, oldChain := serviceGrantPublishBound, db.AuditChainLockTimeout
	if publish > 0 {
		serviceGrantPublishBound = publish
	}
	if chain > 0 {
		db.AuditChainLockTimeout = chain
	}
	t.Cleanup(func() { serviceGrantPublishBound, db.AuditChainLockTimeout = oldPublish, oldChain })
}

// pubPGGate holds one arrival at a stage: reached closes when the work is there, free lets it go.
type pubPGGate struct {
	stage   string
	reached chan struct{}
	free    chan struct{}
	freeFn  func()
}

// await fails the test when nothing reached the stage within ten seconds.
func (g *pubPGGate) await(t *testing.T) {
	t.Helper()
	select {
	case <-g.reached:
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for the work to reach %s", g.stage)
	}
}

// release lets the held arrival go; it is idempotent.
func (g *pubPGGate) release() { g.freeFn() }

// pubPGGates is one server's stage hook: a dispatcher reading its per-stage gates under a mutex. A gate holds
// the first arrival at its stage and is then spent, so a retry or a later mint passes the stage unheld.
type pubPGGates struct {
	mu   sync.Mutex
	held map[string]*pubPGGate
	all  []*pubPGGate
}

// pubPGHook installs the dispatcher on srv. Call it once per server, before its first mint; it is never
// reassigned. The gates close at the test's Cleanup, which runs before the fixture's quiesce.
func pubPGHook(t *testing.T, srv *Server) *pubPGGates {
	t.Helper()
	g := &pubPGGates{held: map[string]*pubPGGate{}}
	srv.svcGrant.hook = g.stop
	t.Cleanup(g.releaseAll)
	return g
}

func (g *pubPGGates) stop(stage string) bool {
	g.mu.Lock()
	gate := g.held[stage]
	delete(g.held, stage)
	g.mu.Unlock()
	if gate == nil {
		return false
	}
	close(gate.reached)
	select {
	case <-gate.free:
	case <-time.After(30 * time.Second):
	}
	return false
}

// hold makes the next arrival at stage wait.
func (g *pubPGGates) hold(stage string) *pubPGGate {
	gate := &pubPGGate{stage: stage, reached: make(chan struct{}), free: make(chan struct{})}
	gate.freeFn = sync.OnceFunc(func() { close(gate.free) })
	g.mu.Lock()
	defer g.mu.Unlock()
	g.held[stage] = gate
	g.all = append(g.all, gate)
	return gate
}

func (g *pubPGGates) releaseAll() {
	g.mu.Lock()
	all := append([]*pubPGGate(nil), g.all...)
	g.mu.Unlock()
	for _, gate := range all {
		gate.release()
	}
}

// pubPGMint is what one serviceGrantToken call answered.
type pubPGMint struct {
	tok serviceGrantMinted
	err error
}

// pubPGAsync asks f's server for row's token on a goroutine of its own.
func pubPGAsync(f *svcPG, row types.ModelProvider, trigger string) <-chan pubPGMint {
	out := make(chan pubPGMint, 1)
	go func() {
		tok, err := f.srv.serviceGrantToken(context.Background(), row, "", trigger)
		out <- pubPGMint{tok, err}
	}()
	return out
}

// pubPGCode runs one request on a goroutine of its own and hands back its recorder.
func pubPGCode(call func() *httptest.ResponseRecorder) <-chan *httptest.ResponseRecorder {
	out := make(chan *httptest.ResponseRecorder, 1)
	go func() { out <- call() }()
	return out
}

// pubPGRecv waits for one value, failing the test after pubPGWait.
func pubPGRecv[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(pubPGWait):
		t.Fatalf("timed out waiting for %s", what)
	}
	var zero T
	return zero
}

// pubPGPending reports whether ch has nothing to read yet.
func pubPGPending[T any](ch <-chan T) bool {
	select {
	case <-ch:
		return false
	default:
		return true
	}
}

// pubPGPoll polls cond every 5 ms and fails the test after within.
func pubPGPoll(t *testing.T, what string, within time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", within, what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// pubPGFlightsDone waits until srv has no flight running: a returned caller does not prove its producer ended.
func pubPGFlightsDone(t *testing.T, srv *Server) {
	t.Helper()
	pubPGPoll(t, "the flights to end", pubPGWait, func() bool {
		srv.svcGrant.mu.Lock()
		defer srv.svcGrant.mu.Unlock()
		return len(srv.svcGrant.flights) == 0
	})
}

// pubPGSecretRow is the operator namespace's owner of the secret rows and corp-a's record name.
func pubPGSecretRow(f *svcPG) (owner, name string) {
	f.t.Helper()
	owner, ok := secretstore.RevisionRowOf(f.srv.cfg.Secrets.For(""))
	if !ok {
		f.t.Fatal("the secret store keeps no revision row")
	}
	return owner, f.secretNameOf("corp-a")
}

// pubPGOne is the one row with id in audit_events.
func pubPGOne(t *testing.T, f *svcPG, id uuid.UUID) types.AuditEvent {
	t.Helper()
	rows := f.rowsByID(id)
	if len(rows) != 1 {
		t.Fatalf("rows with id %s = %d, want 1", id, len(rows))
	}
	return rows[0]
}

// pubPGSupersededBy is the row's superseded_by, "" for a row that names none.
func pubPGSupersededBy(ev types.AuditEvent) string {
	var d struct {
		SupersededBy string `json:"superseded_by"`
	}
	_ = json.Unmarshal(ev.Data, &d)
	return d.SupersededBy
}

// pubPGHashes are the chain hashes the database holds for the row with id.
func pubPGHashes(f *svcPG, id uuid.UUID) (prev, row string) {
	f.t.Helper()
	if err := f.observer.QueryRow(context.Background(),
		`SELECT COALESCE(prev_hash,''), COALESCE(row_hash,'') FROM audit_events WHERE id=$1`, id).Scan(&prev, &row); err != nil {
		f.t.Fatal(err)
	}
	return prev, row
}

// pubPGDurable is the target and data the database holds for the row with id.
func pubPGDurable(f *svcPG, id uuid.UUID) (target, data string) {
	f.t.Helper()
	var raw []byte
	if err := f.observer.QueryRow(context.Background(), `SELECT target, data FROM audit_events WHERE id=$1`, id).Scan(&target, &raw); err != nil {
		f.t.Fatal(err)
	}
	return target, string(raw)
}

// pubPGGrantStored reports whether the stored site configuration holds uid's grant.
func pubPGGrantStored(t *testing.T, raw []byte, uid string) bool {
	t.Helper()
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	var grants map[string]json.RawMessage
	_ = json.Unmarshal(doc[store.ServiceGrantsKey], &grants)
	_, ok := grants[uid]
	return ok
}

// pubPGProbeEvent is a row of no mint action, for a probe that only reads what a publication locked.
func pubPGProbeEvent() types.AuditEvent {
	return types.AuditEvent{ID: uuid.New(), Time: time.Now().UTC(), ActorType: types.ActorSystem, Actor: "test",
		Action: "test.revision_probe", Outcome: "success", Data: mustJSON(map[string]string{"probe": "revision"})}
}

// pubPGAppendWithin appends one row on a pool of its own, as an independent writer, and fails the test when it
// does not commit within the bound: the chain lock is free once the call before it has returned.
func pubPGAppendWithin(t *testing.T, f *svcPG, within time.Duration, after string) {
	t.Helper()
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, f.pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	ev := types.AuditEvent{ID: uuid.New(), Time: time.Now().UTC(), ActorType: types.ActorSystem, Actor: "test",
		Action: "test.chain_probe", Outcome: "success", Data: mustJSON(map[string]string{"after": after})}
	cctx, cancel := context.WithTimeout(ctx, within)
	defer cancel()
	start := time.Now()
	if err := store.InsertAuditEvent(cctx, pool, &ev); err != nil {
		t.Fatalf("an independent appender after %s did not commit within %s (waited %s): %v", after, within, time.Since(start), err)
	}
}

// pubPGStale is one server holding T1, inside its reuse margin and outside its fallback margin, so a refresh
// mints T2 and a failed one is answered with T1.
type pubPGStale struct {
	f      *svcPG
	gates  *pubPGGates
	ids    func() []uuid.UUID
	row    types.ModelProvider
	uid    string
	digest string
	rev1   string
	owner  string
	name   string
	t1     serviceGrantMinted
}

// pubPGStaleSetup builds a sealed fixture on a fake clock with corp-a seeded, mints and caches T1, and moves the
// clock to 5 minutes before T1's expiry. The hook and the row id capture are set before the first mint.
func pubPGStaleSetup(t *testing.T) *pubPGStale {
	t.Helper()
	clock := &svcClock{t: time.Now().UTC()}
	f := newSvcPGWith(t, true, clock, true)
	f.seedOne()
	s := &pubPGStale{f: f}
	s.ids = f.captureRowIDs()
	s.gates = pubPGHook(t, f.srv)
	s.row = f.row("corp-a")
	s.uid, s.digest = s.row.UID, serviceGrantDigest(s.row)
	s.owner, s.name = pubPGSecretRow(f)
	tok, err := f.srv.serviceGrantToken(context.Background(), s.row, "", serviceGrantTriggerRefresh)
	if err != nil || tok.token != svcToken(1) {
		t.Fatalf("T1 = %q, %v", tok.token, err)
	}
	s.t1 = tok
	if s.rev1 = f.valueRev(s.owner, s.name); s.rev1 == "" {
		t.Fatal("the secret has no revision")
	}
	clock.Advance(55 * time.Minute)
	if _, ok := f.srv.cachedServiceGrantToken(s.uid, s.digest, s.rev1, true, serviceGrantReuseMargin); ok {
		t.Fatal("T1 is still inside the reuse margin's hit range")
	}
	if _, ok := f.srv.cachedServiceGrantToken(s.uid, s.digest, s.rev1, true, serviceGrantFallbackMargin); !ok {
		t.Fatal("T1 is outside the fallback margin")
	}
	return s
}

// onlyT1 checks that the direct cache read, at the fallback margin, returns T1 and nothing newer.
func (s *pubPGStale) onlyT1(t *testing.T) {
	t.Helper()
	c, ok := s.f.srv.cachedServiceGrantToken(s.uid, s.digest, s.rev1, true, serviceGrantFallbackMargin)
	if !ok || c.token != svcToken(1) {
		t.Fatalf("the cache read = %q, %v, want T1 only", c.token, ok)
	}
}

// pair starts a refresh attempt for T2 and, once its row id is fixed, a concurrent resolve that joins its flight.
func (s *pubPGStale) pair(t *testing.T) (refresh, resolve <-chan pubPGMint, t2 uuid.UUID) {
	t.Helper()
	refresh = pubPGAsync(s.f, s.row, serviceGrantTriggerRefresh)
	pubPGPoll(t, "the attempt's row id", 5*time.Second, func() bool { return len(s.ids()) >= 2 })
	resolve = pubPGAsync(s.f, s.row, serviceGrantTriggerResolve)
	return refresh, resolve, s.ids()[1]
}

// TestServiceGrant_PublishCommitsBeforeRotation_TwoServers_PG: while A's publication holds both share locks, B's
// secret write waits on the secret's row and the revision stays put; once A's row commits, B's write lands and A's
// next resolve mints under the new revision instead of returning the old entry.
// Plant: drop the FOR SHARE on the secret's row. lockWaiters stays 0 and B commits during the hold.
func TestServiceGrant_PublishCommitsBeforeRotation_TwoServers_PG(t *testing.T) {
	f := newSvcPGSealed(t, true)
	f.seedOne()
	b := f.newIndependentReplica()
	ids := f.captureRowIDs()
	gates := pubPGHook(t, f.srv)
	owner, name := pubPGSecretRow(f)
	rev1 := f.valueRev(owner, name)
	if rev1 == "" {
		t.Fatal("the secret has no revision")
	}
	row := f.row("corp-a")
	locked := gates.hold("publish.locked")
	mint := pubPGAsync(f, row, serviceGrantTriggerRefresh)
	locked.await(t)
	body := b.secretBody("corp-a", svcSecretTwo)
	put := pubPGCode(func() *httptest.ResponseRecorder {
		return b.do(http.MethodPut, b.secretPath("corp-a"), b.admin, body)
	})
	pubPGPoll(t, "B's secret write to wait on A's share lock", 2*time.Second, func() bool { return f.lockWaiters("secrets") >= 1 })
	if got := f.valueRev(owner, name); got != rev1 {
		t.Fatalf("during A's hold value_rev = %q, want revision 1 (%q)", got, rev1)
	}
	if !pubPGPending(put) {
		t.Fatal("B's write answered while A's publication held its share lock")
	}
	locked.release()
	res := pubPGRecv(t, mint, "A's attempt")
	if res.err != nil || res.tok.token != svcToken(1) {
		t.Fatalf("A's attempt = %q, %v, want token 1", res.tok.token, res.err)
	}
	id1 := ids()[0]
	f.awaitRows(t, id1)
	if r := pubPGOne(t, f, id1); r.Outcome != "success" || pubPGSupersededBy(r) != "" {
		t.Fatalf("A's row = %s %s, want a success that names no cause", r.Outcome, r.Data)
	}
	if w := pubPGRecv(t, put, "B's secret write"); w.Code != http.StatusNoContent {
		t.Fatalf("B's write = %d %s, want 204", w.Code, w.Body.String())
	}
	if rev2 := f.valueRev(owner, name); rev2 == "" || rev2 == rev1 {
		t.Fatalf("after B's write value_rev = %q, want it moved from %q", rev2, rev1)
	}
	next, err := f.srv.serviceGrantToken(context.Background(), f.row("corp-a"), "", serviceGrantTriggerResolve)
	if err != nil || next.token != svcToken(2) || f.idp.count() != 2 {
		t.Fatalf("A's next resolve = %q, %v after %d requests, want token 2 minted under the new revision", next.token, err, f.idp.count())
	}
}

// TestServiceGrant_RotationCommitsBeforePublish_TwoServers_PG: a rotation that commits while A's token waits to
// publish makes A's row superseded_by revision, committed and sent once to A's sink with its hashes; token 1 is
// never cached, returned or named by a status, and A's caller gets token 2 through the retry. The second
// subtest has B publish a newer token for the new revision first.
// Plant: pass Secret nil, so A publishes token 1. Plant: queue the sink copy only on the published branch, so
// A's sink lacks the revision row.
func TestServiceGrant_RotationCommitsBeforePublish_TwoServers_PG(t *testing.T) {
	for _, tc := range []struct {
		name   string
		bFirst bool
	}{{"a newer publication on B first", true}, {"none", false}} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSvcPGSealed(t, true)
			f.seedOne()
			b := f.newIndependentReplica()
			b.idp.mu.Lock()
			b.idp.respond = func(n int, _ svcReq) svcAnswer { return svcOK(10+n, 3600) }
			b.idp.mu.Unlock()
			ids := f.captureRowIDs()
			gates := pubPGHook(t, f.srv)
			owner, name := pubPGSecretRow(f)
			row := f.row("corp-a")
			begin := gates.hold("publish.begin")
			mint := pubPGAsync(f, row, serviceGrantTriggerRefresh)
			begin.await(t)
			if w := b.writeSecret("corp-a", svcSecretTwo); w.Code != http.StatusNoContent {
				t.Fatalf("B's write = %d %s", w.Code, w.Body.String())
			}
			rev2 := f.valueRev(owner, name)
			if tc.bFirst {
				if tok, err := b.tokenOf("corp-a", serviceGrantTriggerResolve); err != nil || tok.token != svcToken(11) {
					t.Fatalf("B's newer publication = %q, %v", tok.token, err)
				}
			}
			begin.release()
			res := pubPGRecv(t, mint, "A's attempt")
			if res.err != nil || res.tok.token != svcToken(2) {
				t.Fatalf("A's caller = %q, %v, want token 2 through the retry", res.tok.token, res.err)
			}
			got := ids()
			if len(got) != 2 || f.idp.count() != 2 {
				t.Fatalf("row ids = %v, requests = %d, want two of each", got, f.idp.count())
			}
			f.awaitRows(t, got...)
			copies := f.awaitSink(t, got...)
			first, second := pubPGOne(t, f, got[0]), pubPGOne(t, f, got[1])
			if pubPGSupersededBy(first) != "revision" || first.Outcome != "success" {
				t.Fatalf("A's first row = %s %s, want a committed success superseded_by revision", first.Outcome, first.Data)
			}
			if pubPGSupersededBy(second) != "" || second.Outcome != "success" {
				t.Fatalf("A's second row = %s %s, want a published success", second.Outcome, second.Data)
			}
			pubPGFlightsDone(t, f.srv)
			f.sinkSettled(t)
			if n := f.sink.copiesOf(got[0]); n != 1 {
				t.Fatalf("A's sink holds the superseded row %d times, want once", n)
			}
			prev, rowHash := pubPGHashes(f, got[0])
			if rowHash == "" || copies[0].PrevHash != prev || copies[0].RowHash != rowHash {
				t.Fatalf("the sink copy's hashes = %q/%q, want the database's %q/%q", copies[0].PrevHash, copies[0].RowHash, prev, rowHash)
			}
			c, held := cachedOn(f.srv, row.UID)
			if !held || c.token != svcToken(2) || c.rev != rev2 {
				t.Fatalf("A's cache = %q rev %q, %v, want token 2 under %q", c.token, c.rev, held, rev2)
			}
			st, ok := f.pgStatus("corp-a", f.srv.svcGrant.replica())
			if !ok || st.Outcome != "ok" || st.ValidUntil == nil || st.ValidUntil.Sub(c.expiresAt).Abs() > time.Millisecond {
				t.Fatalf("A's status = %+v, %v, want ok with token 2's expiry %s", st, ok, c.expiresAt)
			}
			if !svcMasked(f.srv, svcToken(1)) {
				t.Error("the unpublished token is not masked")
			}
		})
	}
}

// TestServiceGrant_PublishStalledAuditAcrossRotation_TwoServers_PG: T1 is cached and a refresh for T2 is cut at
// the publication bound because the audit chain lock is held. When a rotation commits before the failure's
// re-reads the retry mints T3; when it stays uncommitted until the attempt returns both callers get T1, and
// never T2, whose row is recorded as publish_unavailable.
// Plant: in the failed-publication branch return tok, nil. Callers then get T2.
func TestServiceGrant_PublishStalledAuditAcrossRotation_TwoServers_PG(t *testing.T) {
	pubPGBounds(t, 2*time.Second, 2*time.Second)
	t.Run("rotation commits before the re-reads", func(t *testing.T) {
		s := pubPGStaleSetup(t)
		f := s.f
		b := f.newIndependentReplica()
		before := f.idp.count()
		locked := s.gates.hold("publish.locked")
		failure := s.gates.hold("failure.begin")
		settle := s.gates.hold("settle.begin")
		chain := f.holdAuditChain()
		refresh := pubPGAsync(f, s.row, serviceGrantTriggerRefresh)
		locked.await(t)
		resolve := pubPGAsync(f, s.row, serviceGrantTriggerResolve)
		s.onlyT1(t)
		body := b.secretBody("corp-a", svcSecretTwo)
		put := pubPGCode(func() *httptest.ResponseRecorder {
			return b.do(http.MethodPut, b.secretPath("corp-a"), b.admin, body)
		})
		pubPGPoll(t, "B's secret write to wait on A's share lock", 2*time.Second, func() bool { return f.lockWaiters("secrets") >= 1 })
		locked.release()
		if w := pubPGRecv(t, put, "B's secret write at A's cut"); w.Code != http.StatusNoContent {
			t.Fatalf("B's write = %d %s, want 204", w.Code, w.Body.String())
		}
		failure.await(t)
		settle.await(t)
		chain()
		settle.release()
		t2 := s.ids()[1]
		f.awaitRows(t, t2)
		f.awaitSink(t, t2)
		if r := pubPGOne(t, f, t2); pubPGSupersededBy(r) != svcSupersededUnpublished {
			t.Fatalf("T2's row = %s, want publish_unavailable", r.Data)
		}
		failure.release()
		for _, ch := range []<-chan pubPGMint{refresh, resolve} {
			if res := pubPGRecv(t, ch, "a caller of A"); res.err != nil || res.tok.token != svcToken(3) {
				t.Fatalf("a caller got %q, %v, want T3 and never T2", res.tok.token, res.err)
			}
		}
		ids := s.ids()
		if len(ids) != 3 || f.idp.count() != before+2 {
			t.Fatalf("row ids = %d, requests = %d, want 3 ids and %d requests", len(ids), f.idp.count(), before+2)
		}
		f.awaitRows(t, ids[2])
		if r := pubPGOne(t, f, ids[2]); r.Outcome != "success" || pubPGSupersededBy(r) != "" {
			t.Fatalf("T3's row = %s %s, want a published success", r.Outcome, r.Data)
		}
		pubPGFlightsDone(t, f.srv)
		f.sinkSettled(t)
		if n := f.sink.copiesOf(t2); n != 1 {
			t.Fatalf("A's sink holds T2's row %d times, want once", n)
		}
		rows, requests := len(f.mintRows()), f.idp.count()
		if tok, err := f.srv.serviceGrantToken(context.Background(), f.row("corp-a"), "", serviceGrantTriggerResolve); err != nil || tok.token != svcToken(3) {
			t.Fatalf("the cache read = %q, %v, want T3", tok.token, err)
		}
		if len(f.mintRows()) != rows || f.idp.count() != requests {
			t.Fatalf("a cache hit added rows (%d to %d) or requests (%d to %d)", rows, len(f.mintRows()), requests, f.idp.count())
		}
	})
	t.Run("rotation held until the attempt returns", func(t *testing.T) {
		s := pubPGStaleSetup(t)
		f := s.f
		before := f.idp.count()
		locked := s.gates.hold("publish.locked")
		settle := s.gates.hold("settle.begin")
		chain := f.holdAuditChain()
		refresh := pubPGAsync(f, s.row, serviceGrantTriggerRefresh)
		locked.await(t)
		resolve := pubPGAsync(f, s.row, serviceGrantTriggerResolve)
		s.onlyT1(t)
		commit := f.holdRotation(s.owner, s.name)
		pubPGPoll(t, "the held rotation to wait on A's share lock", 2*time.Second, func() bool { return f.lockWaiters("secrets") >= 1 })
		locked.release()
		pubPGPoll(t, "the held rotation to take the row lock at A's cut", 8*time.Second, func() bool { return f.lockWaiters("secrets") == 0 })
		chain()
		for _, ch := range []<-chan pubPGMint{refresh, resolve} {
			if res := pubPGRecv(t, ch, "a caller of A"); res.err != nil || res.tok.token != svcToken(1) {
				t.Fatalf("a caller got %q, %v, want T1 as the fallback and never T2", res.tok.token, res.err)
			}
		}
		st, ok := f.pgStatus("corp-a", f.srv.svcGrant.replica())
		if !ok || st.Outcome != "failed" || st.Reason != svcReasonUnavailable || st.ValidUntil == nil || st.ValidUntil.Sub(s.t1.expiresAt).Abs() > time.Millisecond {
			t.Fatalf("A's status = %+v, %v, want failed/unavailable with T1's valid_until %s", st, ok, s.t1.expiresAt)
		}
		settle.await(t)
		settle.release()
		t2 := s.ids()[1]
		f.awaitRows(t, t2)
		if r := pubPGOne(t, f, t2); pubPGSupersededBy(r) != svcSupersededUnpublished {
			t.Fatalf("T2's row = %s, want publish_unavailable", r.Data)
		}
		commit()
		next, err := f.srv.serviceGrantToken(context.Background(), f.row("corp-a"), "", serviceGrantTriggerResolve)
		if err != nil || next.token != svcToken(3) || f.idp.count() != before+2 {
			t.Fatalf("the next resolve = %q, %v after %d requests, want T3 under the new revision", next.token, err, f.idp.count())
		}
		ids := s.ids()
		if len(ids) != 3 {
			t.Fatalf("row ids = %d, want 3", len(ids))
		}
		f.awaitRows(t, ids[2])
		if r := pubPGOne(t, f, ids[2]); r.Outcome != "success" || pubPGSupersededBy(r) != "" {
			t.Fatalf("T3's row = %s %s, want one published success", r.Outcome, r.Data)
		}
		rows, requests := len(f.mintRows()), f.idp.count()
		if tok, err := f.srv.serviceGrantToken(context.Background(), f.row("corp-a"), "", serviceGrantTriggerResolve); err != nil || tok.token != svcToken(3) {
			t.Fatalf("the cache read = %q, %v, want T3", tok.token, err)
		}
		if len(f.mintRows()) != rows || f.idp.count() != requests {
			t.Fatalf("a cache hit added rows (%d to %d) or requests (%d to %d)", rows, len(f.mintRows()), requests, f.idp.count())
		}
	})
}

// TestServiceGrant_PublishOrderedAgainstRemoval_TwoServers_PG: a grant removal and a secret delete on B each
// wait for a publication on A that has decided, and each is honoured when it commits first: the row then names
// settings or secret_gone and A's caller gets drift or a definitive gone.
// Plant (a), (c): drop the FOR SHARE on the site configuration, or on the secret's row; lockWaiters stays 0.
// Plant (b): skip the settings case, so the row names retiring or secret_gone and the caller gets gone, not
// drift. Plant (d): skip the secret_gone case, so the row names revision.
func TestServiceGrant_PublishOrderedAgainstRemoval_TwoServers_PG(t *testing.T) {
	removeGrant := func(b *svcPG) func() *httptest.ResponseRecorder {
		return func() *httptest.ResponseRecorder {
			return b.do(http.MethodDelete, "/model-providers/corp-a/service-grant", b.admin, nil)
		}
	}
	deleteSecret := func(b *svcPG) func() *httptest.ResponseRecorder {
		return func() *httptest.ResponseRecorder {
			return b.do(http.MethodDelete, b.secretPath("corp-a"), b.admin, nil)
		}
	}
	t.Run("grant removal waits for a decided publication", func(t *testing.T) {
		f := newSvcPGSealed(t, true)
		f.seedOne()
		b := f.newIndependentReplica()
		ids := f.captureRowIDs()
		gates := pubPGHook(t, f.srv)
		row := f.row("corp-a")
		locked := gates.hold("publish.locked")
		mint := pubPGAsync(f, row, serviceGrantTriggerRefresh)
		locked.await(t)
		removal := pubPGCode(removeGrant(b))
		pubPGPoll(t, "B's removal to wait on the site configuration", 2*time.Second, func() bool { return f.lockWaiters("site_config") >= 1 })
		if !pubPGGrantStored(t, f.siteConfigRaw(), row.UID) {
			t.Fatal("the grant was removed during A's hold")
		}
		locked.release()
		res := pubPGRecv(t, mint, "A's attempt")
		if res.err != nil || res.tok.token != svcToken(1) {
			t.Fatalf("A's caller = %q, %v, want the token", res.tok.token, res.err)
		}
		f.awaitRows(t, ids()[0])
		if r := pubPGOne(t, f, ids()[0]); r.Outcome != "success" || pubPGSupersededBy(r) != "" {
			t.Fatalf("A's row = %s %s, want one published success", r.Outcome, r.Data)
		}
		if w := pubPGRecv(t, removal, "B's removal"); w.Code/100 != 2 {
			t.Fatalf("B's removal = %d %s", w.Code, w.Body.String())
		}
		if pubPGGrantStored(t, f.siteConfigRaw(), row.UID) {
			t.Fatal("the grant is still stored after B's removal answered")
		}
		_, err := f.srv.serviceGrantToken(context.Background(), f.row("corp-a"), "", serviceGrantTriggerResolve)
		if !errors.Is(err, errServiceGrantDrift) || f.idp.count() != 1 {
			t.Fatalf("A's next resolve = %v after %d requests, want drift and no request", err, f.idp.count())
		}
	})
	t.Run("removal first", func(t *testing.T) {
		f := newSvcPGSealed(t, true)
		f.seedOne()
		b := f.newIndependentReplica()
		ids := f.captureRowIDs()
		gates := pubPGHook(t, f.srv)
		row := f.row("corp-a")
		begin := gates.hold("publish.begin")
		mint := pubPGAsync(f, row, serviceGrantTriggerRefresh)
		begin.await(t)
		if w := pubPGRecv(t, pubPGCode(removeGrant(b)), "B's removal"); w.Code/100 != 2 {
			t.Fatalf("B's removal = %d %s", w.Code, w.Body.String())
		}
		begin.release()
		res := pubPGRecv(t, mint, "A's attempt")
		if !errors.Is(res.err, errServiceGrantDrift) || res.tok.token != "" {
			t.Fatalf("A's caller = %q, %v, want drift", res.tok.token, res.err)
		}
		f.awaitRows(t, ids()[0])
		if r := pubPGOne(t, f, ids()[0]); pubPGSupersededBy(r) != svcSupersededSettings {
			t.Fatalf("A's row = %s, want superseded_by settings", r.Data)
		}
	})
	t.Run("secret delete waits for a decided publication", func(t *testing.T) {
		f := newSvcPGSealed(t, true)
		f.seedOne()
		b := f.newIndependentReplica()
		ids := f.captureRowIDs()
		gates := pubPGHook(t, f.srv)
		owner, name := pubPGSecretRow(f)
		row := f.row("corp-a")
		locked := gates.hold("publish.locked")
		mint := pubPGAsync(f, row, serviceGrantTriggerRefresh)
		locked.await(t)
		removal := pubPGCode(deleteSecret(b))
		pubPGPoll(t, "B's delete to wait on A's share lock", 2*time.Second, func() bool { return f.lockWaiters("secrets") >= 1 })
		if f.valueRev(owner, name) == "" {
			t.Fatal("the secret was deleted during A's hold")
		}
		locked.release()
		res := pubPGRecv(t, mint, "A's attempt")
		if res.err != nil || res.tok.token != svcToken(1) {
			t.Fatalf("A's caller = %q, %v, want the token", res.tok.token, res.err)
		}
		f.awaitRows(t, ids()[0])
		if r := pubPGOne(t, f, ids()[0]); r.Outcome != "success" || pubPGSupersededBy(r) != "" {
			t.Fatalf("A's row = %s %s, want one published success", r.Outcome, r.Data)
		}
		if w := pubPGRecv(t, removal, "B's delete"); w.Code != http.StatusNoContent {
			t.Fatalf("B's delete = %d %s, want 204", w.Code, w.Body.String())
		}
		_, err := f.srv.serviceGrantToken(context.Background(), f.row("corp-a"), "", serviceGrantTriggerResolve)
		if se := svcErr(t, err); se.reason != svcReasonSecretGone || !se.definitive || f.idp.count() != 1 {
			t.Fatalf("A's next resolve = %+v after %d requests, want a definitive gone and no request", se, f.idp.count())
		}
	})
	t.Run("secret delete first", func(t *testing.T) {
		f := newSvcPGSealed(t, true)
		f.seedOne()
		b := f.newIndependentReplica()
		ids := f.captureRowIDs()
		gates := pubPGHook(t, f.srv)
		row := f.row("corp-a")
		begin := gates.hold("publish.begin")
		mint := pubPGAsync(f, row, serviceGrantTriggerRefresh)
		begin.await(t)
		if w := pubPGRecv(t, pubPGCode(deleteSecret(b)), "B's delete"); w.Code != http.StatusNoContent {
			t.Fatalf("B's delete = %d %s, want 204", w.Code, w.Body.String())
		}
		begin.release()
		res := pubPGRecv(t, mint, "A's attempt")
		if se := svcErr(t, res.err); se.reason != svcReasonSecretGone || !se.definitive || res.tok.token != "" {
			t.Fatalf("A's caller = %q, %+v, want a definitive gone", res.tok.token, se)
		}
		f.awaitRows(t, ids()[0])
		if r := pubPGOne(t, f, ids()[0]); pubPGSupersededBy(r) != svcSupersededSecretGone {
			t.Fatalf("A's row = %s, want superseded_by secret_gone", r.Data)
		}
		if _, held := cachedOn(f.srv, row.UID); held {
			t.Error("A's cache still holds a token after the secret's absence was seen")
		}
	})
}

// TestServiceGrant_TokenInvisibleUntilRowCommits_PG: with T1 cached, T2 is neither cached, returned nor visible
// as a row while its row waits for the chain lock or sits uncommitted; and a token whose row committed is not
// installed when the cache was evicted in between.
// Plant: install the cache inside Decide. The cache read returns T2.
// Plant: drop genNow == cacheGen from serviceGrantInstalls. The evicted subtest finds T2 cached and sends no
// request.
func TestServiceGrant_TokenInvisibleUntilRowCommits_PG(t *testing.T) {
	invisible := func(t *testing.T, s *pubPGStale, t2 uuid.UUID, refresh, resolve <-chan pubPGMint) {
		t.Helper()
		s.onlyT1(t)
		select {
		case <-refresh:
			t.Fatal("the refresh returned before its row committed")
		case <-resolve:
			t.Fatal("the concurrent resolve returned before the row committed")
		case <-time.After(300 * time.Millisecond):
		}
		s.onlyT1(t)
		if rows := s.f.rowsByID(t2); len(rows) != 0 {
			t.Fatalf("T2's row is visible before its commit: %v", rows)
		}
	}
	afterRelease := func(t *testing.T, s *pubPGStale, refresh, resolve <-chan pubPGMint) {
		t.Helper()
		for _, ch := range []<-chan pubPGMint{refresh, resolve} {
			if res := pubPGRecv(t, ch, "a caller"); res.err != nil || res.tok.token != svcToken(2) {
				t.Fatalf("a caller got %q, %v, want T2", res.tok.token, res.err)
			}
		}
		rows, requests := len(s.f.mintRows()), s.f.idp.count()
		tok, err := s.f.srv.serviceGrantToken(context.Background(), s.row, "", serviceGrantTriggerResolve)
		if err != nil || tok.token != svcToken(2) || s.f.idp.count() != requests || len(s.f.mintRows()) != rows {
			t.Fatalf("the next resolve = %q, %v (requests %d to %d, rows %d to %d), want a cache hit for T2",
				tok.token, err, requests, s.f.idp.count(), rows, len(s.f.mintRows()))
		}
	}
	t.Run("at the chain lock", func(t *testing.T) {
		s := pubPGStaleSetup(t)
		chain := s.f.holdAuditChain()
		refresh, resolve, t2 := s.pair(t)
		invisible(t, s, t2, refresh, resolve)
		chain()
		afterRelease(t, s, refresh, resolve)
	})
	t.Run("after the row, before commit", func(t *testing.T) {
		s := pubPGStaleSetup(t)
		pre := s.gates.hold("publish.precommit")
		refresh, resolve, t2 := s.pair(t)
		pre.await(t)
		invisible(t, s, t2, refresh, resolve)
		pre.release()
		afterRelease(t, s, refresh, resolve)
	})
	t.Run("after commit, before install, evicted", func(t *testing.T) {
		s := pubPGStaleSetup(t)
		committed := s.gates.hold("publish.committed")
		refresh, resolve, t2 := s.pair(t)
		committed.await(t)
		if len(s.f.rowsByID(t2)) != 1 {
			t.Fatal("T2's row is not committed at publish.committed")
		}
		s.f.srv.dropServiceGrantCache(s.uid)
		committed.release()
		for _, ch := range []<-chan pubPGMint{refresh, resolve} {
			if res := pubPGRecv(t, ch, "a caller"); res.err != nil || res.tok.token != svcToken(2) {
				t.Fatalf("a caller got %q, %v, want T2", res.tok.token, res.err)
			}
		}
		if c, held := cachedOn(s.f.srv, s.uid); held {
			t.Fatalf("the cache holds %q after the eviction, want no entry", c.token)
		}
		requests := s.f.idp.count()
		tok, err := s.f.srv.serviceGrantToken(context.Background(), s.row, "", serviceGrantTriggerResolve)
		if err != nil || tok.token != svcToken(3) || s.f.idp.count() != requests+1 {
			t.Fatalf("the next resolve = %q, %v after %d requests, want a new request for T3", tok.token, err, s.f.idp.count())
		}
	})
}

// TestServiceGrant_LowerSeqNeverDisplacesNewerEntry_TwoServers_PG: A's attempt X commits under revision 1 and
// waits before installing; a rotation on B and A's resolve Y (revision 2) commit and install after it; when X is
// released its older entry does not displace Y's, so the next resolve is a cache hit for Y's token.
// Plant: drop prevSeq < seq from serviceGrantInstalls. X's revision-1 entry displaces Y's and the resolve mints
// again.
func TestServiceGrant_LowerSeqNeverDisplacesNewerEntry_TwoServers_PG(t *testing.T) {
	f := newSvcPGSealed(t, true)
	f.seedOne()
	b := f.newIndependentReplica()
	ids := f.captureRowIDs()
	gates := pubPGHook(t, f.srv)
	owner, name := pubPGSecretRow(f)
	row := f.row("corp-a")
	committed := gates.hold("publish.committed")
	x := pubPGAsync(f, row, serviceGrantTriggerRefresh)
	committed.await(t)
	if w := b.writeSecret("corp-a", svcSecretTwo); w.Code != http.StatusNoContent {
		t.Fatalf("B's write = %d %s", w.Code, w.Body.String())
	}
	rev2 := f.valueRev(owner, name)
	y := pubPGAsync(f, f.row("corp-a"), serviceGrantTriggerResolve)
	res := pubPGRecv(t, y, "A's resolve Y")
	if res.err != nil || res.tok.token != svcToken(2) {
		t.Fatalf("Y = %q, %v, want token 2", res.tok.token, res.err)
	}
	installed, held := cachedOn(f.srv, row.UID)
	if !held || installed.token != svcToken(2) || installed.rev != rev2 || installed.seq <= 0 {
		t.Fatalf("after Y the cache = %q rev %q seq %d, %v, want token 2 under %q", installed.token, installed.rev, installed.seq, held, rev2)
	}
	committed.release()
	if res := pubPGRecv(t, x, "A's attempt X"); res.err != nil || res.tok.token != svcToken(1) {
		t.Fatalf("X = %q, %v, want token 1", res.tok.token, res.err)
	}
	got, held := cachedOn(f.srv, row.UID)
	if !held || got.token != svcToken(2) || got.seq != installed.seq {
		t.Fatalf("after X the cache = %q seq %d, %v, want Y's entry (token 2, seq %d)", got.token, got.seq, held, installed.seq)
	}
	rows, requests := len(f.mintRows()), f.idp.count()
	tok, err := f.srv.serviceGrantToken(context.Background(), f.row("corp-a"), "", serviceGrantTriggerResolve)
	if err != nil || tok.token != svcToken(2) || f.idp.count() != requests || len(f.mintRows()) != rows {
		t.Fatalf("the next resolve = %q, %v (requests %d to %d, rows %d to %d), want a cache hit for Y's token",
			tok.token, err, requests, f.idp.count(), rows, len(f.mintRows()))
	}
	for _, id := range ids() {
		f.awaitRows(t, id)
	}
}

// TestServiceGrant_PublishRevisionMatchesSecretStore_PG: the revision a publication reads under its share lock
// (value_rev of the secrets row) is what the secret store's Revision returns for the same name: at the seeded
// value, after a rotation, and for an absent record.
// Plant: read updated_at::text in the publication's SELECT. The two differ at the first comparison.
func TestServiceGrant_PublishRevisionMatchesSecretStore_PG(t *testing.T) {
	f := newSvcPGSealed(t, true)
	f.seedOne()
	ctx := context.Background()
	st := f.srv.cfg.Secrets.For("")
	owner, name := pubPGSecretRow(f)
	compare := func(when string) string {
		t.Helper()
		viaStore, ok, err := secretstore.RevisionOf(ctx, st, name)
		if err != nil || !ok {
			t.Fatalf("%s: the store's revision = %q, %v, %v", when, viaStore, ok, err)
		}
		var locked *string
		_, _, _, err = f.pg.PublishServiceGrantMint(ctx, store.ServiceGrantPublish{
			Secret: &store.ServiceGrantSecretRow{Owner: owner, Name: name},
			Decide: func(_ types.SiteConfig, _ store.ServiceGrantDoc, secretRev *string) (types.AuditEvent, bool) {
				locked = secretRev
				return pubPGProbeEvent(), false
			},
		})
		if err != nil || locked == nil {
			t.Fatalf("%s: the publication's read = %v, %v", when, locked, err)
		}
		if *locked != viaStore {
			t.Fatalf("%s: the publication locks revision %q, the store returns %q", when, *locked, viaStore)
		}
		if column := f.valueRev(owner, name); column != viaStore {
			t.Fatalf("%s: value_rev = %q, the store returns %q", when, column, viaStore)
		}
		return viaStore
	}
	rev1 := compare("seeded")
	if rev1 == "" {
		t.Fatal("the seeded secret has no revision")
	}
	if w := f.writeSecret("corp-a", svcSecretTwo); w.Code != http.StatusNoContent {
		t.Fatalf("rotate: %d %s", w.Code, w.Body.String())
	}
	if rev2 := compare("rotated"); rev2 == rev1 {
		t.Fatalf("the revision did not move on a rotation: %q", rev2)
	}
	if w := f.do(http.MethodDelete, f.secretPath("corp-a"), f.admin, nil); w.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", w.Code, w.Body.String())
	}
	if gone := compare("deleted"); gone != "" {
		t.Fatalf("the revision of a deleted secret = %q, want empty", gone)
	}
}

// TestServiceGrant_SettleReturnsCanonicalRow_PG: the settle returns the row the database holds when one has its id
// and time (outcome, data and hashes as stored, one row still), appends a new one with its hashes, fails inside
// the chain lock timeout, and fails on an ended context; after each of the four, an independent appender takes
// the chain lock and commits within a second.
// Plant: implement the settle as InsertAuditEvent under WithAuditReplay, which returns the fallback and so a
// different data. Plant: drop the deferred Rollback, so the appender after "found" waits past a second.
func TestServiceGrant_SettleReturnsCanonicalRow_PG(t *testing.T) {
	pubPGBounds(t, 0, 300*time.Millisecond)
	f := newSvcPGSealed(t, true)
	f.seedOne()
	ids := f.captureRowIDs()
	if _, err := f.tokenOf("corp-a", serviceGrantTriggerRefresh); err != nil {
		t.Fatal(err)
	}
	id := ids()[0]
	f.awaitRows(t, id)
	published := pubPGOne(t, f, id)
	wantPrev, wantRow := pubPGHashes(f, id)
	if published.Outcome != "success" || wantRow == "" {
		t.Fatalf("the published row = %s with hash %q", published.Outcome, wantRow)
	}
	ctx := context.Background()
	mintEvent := func(id uuid.UUID, at time.Time, outcome string) types.AuditEvent {
		return types.AuditEvent{ID: id, Time: at, ActorType: types.ActorSystem, Actor: serviceGrantActor, Action: auditServiceGrantMintAction,
			Target: "corp-a", Outcome: outcome, Data: mustJSON(map[string]any{"superseded": true, "superseded_by": svcSupersededUnpublished})}
	}

	got, err := f.pg.SettleServiceGrantMint(ctx, mintEvent(published.ID, published.Time, "failure"))
	if err != nil {
		t.Fatalf("settle of a row the database holds: %v", err)
	}
	if got.ID != id || got.Outcome != published.Outcome || string(got.Data) != string(published.Data) || got.PrevHash != wantPrev || got.RowHash != wantRow {
		t.Fatalf("settle returned %s %s %q/%q, want the stored row %s %s %q/%q", got.Outcome, got.Data, got.PrevHash, got.RowHash,
			published.Outcome, published.Data, wantPrev, wantRow)
	}
	if n := len(f.rowsByID(id)); n != 1 {
		t.Fatalf("rows with the published id = %d after the settle, want 1", n)
	}
	pubPGAppendWithin(t, f, time.Second, "a settle that found the row")

	fresh := mintEvent(uuid.New(), time.Now().UTC(), "failure")
	inserted, err := f.pg.SettleServiceGrantMint(ctx, fresh)
	if err != nil || inserted.RowHash == "" {
		t.Fatalf("settle of a new row = %q, %v, want it inserted with its hashes", inserted.RowHash, err)
	}
	if prev, row := pubPGHashes(f, fresh.ID); len(f.rowsByID(fresh.ID)) != 1 || prev != inserted.PrevHash || row != inserted.RowHash {
		t.Fatalf("the inserted row's stored hashes %q/%q differ from the returned %q/%q", prev, row, inserted.PrevHash, inserted.RowHash)
	}
	pubPGAppendWithin(t, f, time.Second, "a settle that inserted the row")

	release := f.holdAuditChain()
	timedOut := mintEvent(uuid.New(), time.Now().UTC(), "failure")
	start := time.Now()
	_, err = f.pg.SettleServiceGrantMint(ctx, timedOut)
	elapsed := time.Since(start)
	release()
	if err == nil || !strings.Contains(err.Error(), "lock audit chain") {
		t.Fatalf("settle under a held chain lock = %v, want the chain lock's timeout", err)
	}
	if elapsed > 700*time.Millisecond {
		t.Fatalf("settle waited %s on the held chain lock, want it cut near the 300ms timeout", elapsed)
	}
	if n := len(f.rowsByID(timedOut.ID)); n != 0 {
		t.Fatalf("the timed out settle left %d rows", n)
	}
	pubPGAppendWithin(t, f, time.Second, "a settle cut by the lock timeout")

	ended, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := f.pg.SettleServiceGrantMint(ended, mintEvent(uuid.New(), time.Now().UTC(), "failure")); err == nil {
		t.Fatal("settle under an ended context succeeded")
	}
	pubPGAppendWithin(t, f, time.Second, "a settle on an ended context")
}

// TestServiceGrant_MintRowMasksTargetAndData_PG: a provider id carrying a registered marker never reaches a mint
// row's durable target or data, nor its sink copy, for a success, a revision row, a failure and a
// publish_unavailable row; each holds the mask placeholder instead. Each publication prepares its seven
// candidates and each failure one row.
// Plant: skip preparation for the success candidate. The marker appears in the durable target.
func TestServiceGrant_MintRowMasksTargetAndData_PG(t *testing.T) {
	pubPGBounds(t, 0, 300*time.Millisecond)
	const marker = "mkr7q4x9z2w5v8t3"
	pid := "corp-" + marker
	f := newSvcPGSealed(t, true)
	if w := f.put(svcPGRow(pid, true)); w.Code/100 != 2 {
		t.Fatalf("seed %s: %d %s", pid, w.Code, w.Body.String())
	}
	if w := f.writeSecret(pid, svcClientSecret); w.Code != http.StatusNoContent {
		t.Fatalf("seed %s's secret: %d %s", pid, w.Code, w.Body.String())
	}
	ctx := context.Background()
	reg := f.srv.cfg.MaskRegistry
	gen, err := reg.GlobalGeneration(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.AddGlobal(gen, "", "pubpg-marker", time.Now(), []byte(marker)); err != nil {
		t.Fatal(err)
	}
	ids := f.captureRowIDs()
	gates := pubPGHook(t, f.srv)
	f.idp.mu.Lock()
	f.idp.respond = func(n int, _ svcReq) svcAnswer {
		if n == 3 {
			return svcOAuthError(400, "invalid_client")
		}
		return svcAnswer{}
	}
	f.idp.mu.Unlock()
	row := f.row(pid)

	// A revision row, then the success row of the retry: two publications.
	begin := gates.hold("publish.begin")
	before := f.prepares.Load()
	first := pubPGAsync(f, row, serviceGrantTriggerRefresh)
	begin.await(t)
	if w := f.writeSecret(pid, svcSecretTwo); w.Code != http.StatusNoContent {
		t.Fatalf("rotate: %d %s", w.Code, w.Body.String())
	}
	begin.release()
	if res := pubPGRecv(t, first, "the superseded attempt"); res.err != nil || res.tok.token != svcToken(2) {
		t.Fatalf("the first attempt = %q, %v, want token 2 through the retry", res.tok.token, res.err)
	}
	got := ids()
	if len(got) != 2 {
		t.Fatalf("row ids = %v, want 2", got)
	}
	f.awaitRows(t, got...)
	f.awaitSink(t, got...)
	revisionID, successID := got[0], got[1]
	if cause := pubPGSupersededBy(pubPGOne(t, f, revisionID)); cause != svcSupersededRevision {
		t.Fatalf("the first row names %q, want revision", cause)
	}
	if d := f.prepares.Load() - before; d != 14 {
		t.Fatalf("two publications prepared %d rows, want 14 (seven candidates each)", d)
	}

	// A failure row.
	f.srv.dropServiceGrantCache(row.UID)
	before = f.prepares.Load()
	_, _ = f.srv.serviceGrantToken(ctx, row, "", serviceGrantTriggerRefresh)
	got = ids()
	if len(got) != 3 {
		t.Fatalf("row ids = %v, want 3", got)
	}
	failureID := got[2]
	f.awaitRows(t, failureID)
	f.awaitSink(t, failureID)
	if r := pubPGOne(t, f, failureID); r.Outcome != "failure" {
		t.Fatalf("the third row = %s, want a failure", r.Outcome)
	}
	if d := f.prepares.Load() - before; d != 1 {
		t.Fatalf("a failure prepared %d rows, want 1", d)
	}

	// A publish_unavailable row: the chain lock is held past the lock timeout, and the settle starts after it.
	f.srv.dropServiceGrantCache(row.UID)
	settle := gates.hold("settle.begin")
	chain := f.holdAuditChain()
	before = f.prepares.Load()
	third := pubPGAsync(f, row, serviceGrantTriggerRefresh)
	pubPGRecv(t, third, "the stalled attempt")
	settle.await(t)
	chain()
	settle.release()
	got = ids()
	if len(got) != 4 {
		t.Fatalf("row ids = %v, want 4", got)
	}
	unavailableID := got[3]
	f.awaitRows(t, unavailableID)
	f.awaitSink(t, unavailableID)
	if cause := pubPGSupersededBy(pubPGOne(t, f, unavailableID)); cause != svcSupersededUnpublished {
		t.Fatalf("the fourth row names %q, want publish_unavailable", cause)
	}
	if d := f.prepares.Load() - before; d != 7 {
		t.Fatalf("a publication prepared %d rows, want 7", d)
	}

	placeholder := string(secretmask.Placeholder())
	for _, c := range []struct {
		what string
		id   uuid.UUID
	}{{"success", successID}, {"revision", revisionID}, {"failure", failureID}, {"publish_unavailable", unavailableID}} {
		target, data := pubPGDurable(f, c.id)
		copyOf := f.awaitSink(t, c.id)[0]
		for where, text := range map[string]string{"durable target": target, "durable data": data, "copy target": copyOf.Target, "copy data": string(copyOf.Data)} {
			if strings.Contains(text, marker) {
				t.Errorf("the %s row's %s carries the marker: %s", c.what, where, text)
			}
			if !strings.Contains(text, placeholder) {
				t.Errorf("the %s row's %s lacks the mask placeholder: %s", c.what, where, text)
			}
		}
	}
}
