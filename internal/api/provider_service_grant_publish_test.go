// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// The publication of a service grant's token against the in-memory doubles: the pure decision and install
// predicates, the order of the row, the cache and the answer, the bounds of an attempt, the permit cap, and the
// queue that carries each mint row's copy to the audit sinks. The helpers here carry a pub prefix.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// pubWait bounds every wait of these tests that has no tighter limit of its own.
const pubWait = 10 * time.Second

// pubBounds are the time bounds a test shortens; a zero field leaves that bound as it is.
type pubBounds struct {
	attempt, token, tail, publish, recheck time.Duration
}

// pubShorten sets the bounds and restores them in Cleanup. A test calls it before it builds its fixture, so the
// restore runs after the fixture's quiesce, once nothing in flight can still read a bound.
func pubShorten(t *testing.T, b pubBounds) {
	t.Helper()
	attempt, token, tail, publish, recheck := serviceGrantAttemptBound, serviceGrantTokenBound, serviceGrantTailBound,
		serviceGrantPublishBound, serviceGrantRecheckBound
	if b.attempt > 0 {
		serviceGrantAttemptBound = b.attempt
	}
	if b.token > 0 {
		serviceGrantTokenBound = b.token
	}
	if b.tail > 0 {
		serviceGrantTailBound = b.tail
	}
	if b.publish > 0 {
		serviceGrantPublishBound = b.publish
	}
	if b.recheck > 0 {
		serviceGrantRecheckBound = b.recheck
	}
	t.Cleanup(func() {
		serviceGrantAttemptBound, serviceGrantTokenBound, serviceGrantTailBound = attempt, token, tail
		serviceGrantPublishBound, serviceGrantRecheckBound = publish, recheck
	})
}

// pubShortenCaps sets the permit cap and the sink queue's length (zero leaves one as it is) and restores both in
// Cleanup. Like pubShorten, it runs before the fixture is built.
func pubShortenCaps(t *testing.T, permits int64, queue int) {
	t.Helper()
	oldPermits, oldQueue := serviceGrantRowsOutstanding, serviceGrantSIEMQueue
	if permits > 0 {
		serviceGrantRowsOutstanding = permits
	}
	if queue > 0 {
		serviceGrantSIEMQueue = queue
	}
	t.Cleanup(func() { serviceGrantRowsOutstanding, serviceGrantSIEMQueue = oldPermits, oldQueue })
}

// pubUntil polls cond every 5 ms and fails the test after d.
func pubUntil(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %v waiting for %s", d, what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// pubGate holds code at a point until the test releases it. Every hold ends by itself after pubWait.
type pubGate struct {
	reached chan struct{}
	free    chan struct{}
	once    sync.Once
	n       atomic.Int64
	// release frees every holder; it is idempotent, and Cleanup calls it too.
	release func()
}

func newPubGate(t *testing.T) *pubGate {
	t.Helper()
	g := &pubGate{reached: make(chan struct{}), free: make(chan struct{})}
	g.release = sync.OnceFunc(func() { close(g.free) })
	t.Cleanup(g.release)
	return g
}

func (g *pubGate) mark() { g.once.Do(func() { close(g.reached) }) }

func (g *pubGate) park() {
	select {
	case <-g.free:
	case <-time.After(pubWait):
	}
}

// hold marks the gate reached and parks every caller.
func (g *pubGate) hold() {
	g.n.Add(1)
	g.mark()
	g.park()
}

// holdFirst parks the first caller only; later callers pass.
func (g *pubGate) holdFirst() {
	if g.n.Add(1) != 1 {
		return
	}
	g.mark()
	g.park()
}

// holdCtx is hold for a store double's gate: it also ends, with the context's error, when ctx does.
func (g *pubGate) holdCtx(ctx context.Context) error {
	g.n.Add(1)
	g.mark()
	select {
	case <-g.free:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(pubWait):
		return nil
	}
}

// pubHook is the one dispatcher a test assigns to svcGrant.hook before its first mint. It reads each stage's
// function under a mutex, so a test may add or change a stage later without reassigning the hook.
type pubHook struct {
	mu   sync.Mutex
	fns  map[string]func()
	hits map[string]int
}

func pubInstallHook(f *svcFix) *pubHook {
	h := &pubHook{fns: map[string]func(){}, hits: map[string]int{}}
	f.srv.svcGrant.hook = func(stage string) bool {
		h.mu.Lock()
		fn := h.fns[stage]
		h.hits[stage]++
		h.mu.Unlock()
		if fn != nil {
			fn()
		}
		return false
	}
	return h
}

// on sets what stage does.
func (h *pubHook) on(stage string, fn func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.fns[stage] = fn
}

// pubResult is the answer of a call started with pubGo, and how long the call took.
type pubResult struct {
	tok  serviceGrantMinted
	err  error
	took time.Duration
}

// pubGo runs fn on its own goroutine.
func pubGo(fn func() (serviceGrantMinted, error)) <-chan pubResult {
	ch := make(chan pubResult, 1)
	go func() {
		started := time.Now()
		tok, err := fn()
		ch <- pubResult{tok: tok, err: err, took: time.Since(started)}
	}()
	return ch
}

// pubRecvWithin waits up to d for a call's answer.
func pubRecvWithin(t *testing.T, ch <-chan pubResult, d time.Duration, what string) pubResult {
	t.Helper()
	if d < time.Millisecond {
		d = time.Millisecond
	}
	select {
	case r := <-ch:
		return r
	case <-time.After(d):
		t.Fatalf("%s did not answer within %v", what, d)
	}
	return pubResult{}
}

func pubRecv(t *testing.T, ch <-chan pubResult, what string) pubResult {
	t.Helper()
	return pubRecvWithin(t, ch, pubWait, what)
}

// pubWantUnavailable checks err is the transient unavailable answer.
func pubWantUnavailable(t *testing.T, err error) *serviceGrantError {
	t.Helper()
	se := svcErr(t, err)
	if se.reason != svcReasonUnavailable || se.definitive {
		t.Fatalf("answer = %+v, want the transient unavailable", se)
	}
	return se
}

// pubLogRec is one captured log record.
type pubLogRec struct {
	level slog.Level
	msg   string
	attrs map[string]slog.Value
}

func (r pubLogRec) str(key string) string {
	if v, ok := r.attrs[key]; ok {
		return v.String()
	}
	return ""
}

// flag reads a boolean attribute; ok is false when the record has none.
func (r pubLogRec) flag(key string) (val, ok bool) {
	v, has := r.attrs[key]
	if !has || v.Kind() != slog.KindBool {
		return false, false
	}
	return v.Bool(), true
}

// pubLogs is a slog handler that keeps every record with its attributes.
type pubLogs struct {
	mu   sync.Mutex
	recs []pubLogRec
}

func (l *pubLogs) Enabled(context.Context, slog.Level) bool { return true }
func (l *pubLogs) WithAttrs([]slog.Attr) slog.Handler       { return l }
func (l *pubLogs) WithGroup(string) slog.Handler            { return l }

func (l *pubLogs) Handle(_ context.Context, r slog.Record) error {
	rec := pubLogRec{level: r.Level, msg: r.Message, attrs: map[string]slog.Value{}}
	r.Attrs(func(a slog.Attr) bool {
		rec.attrs[a.Key] = a.Value.Resolve()
		return true
	})
	l.mu.Lock()
	defer l.mu.Unlock()
	l.recs = append(l.recs, rec)
	return nil
}

// pubCaptureLogs makes the capture the default logger for the test. A fixture sets its own default logger, so the
// test calls this after it builds the fixture.
func pubCaptureLogs(t *testing.T) *pubLogs {
	t.Helper()
	l := &pubLogs{}
	prev := slog.Default()
	slog.SetDefault(slog.New(l))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return l
}

func (l *pubLogs) all() []pubLogRec {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]pubLogRec(nil), l.recs...)
}

// forEvent is every record whose event_id is id.
func (l *pubLogs) forEvent(id uuid.UUID) []pubLogRec {
	var out []pubLogRec
	for _, r := range l.all() {
		if r.str("event_id") == id.String() {
			out = append(out, r)
		}
	}
	return out
}

// withMsg is every record of level whose message contains sub.
func (l *pubLogs) withMsg(level slog.Level, sub string) []pubLogRec {
	var out []pubLogRec
	for _, r := range l.all() {
		if r.level == level && strings.Contains(r.msg, sub) {
			out = append(out, r)
		}
	}
	return out
}

// pubCopyRecs is the records of id whose copy attribute is copy.
func pubCopyRecs(l *pubLogs, id uuid.UUID, copyEnd string) []pubLogRec {
	var out []pubLogRec
	for _, r := range l.forEvent(id) {
		if r.str("copy") == copyEnd {
			out = append(out, r)
		}
	}
	return out
}

// pubQueue is the sink queue's state, read under its mutex.
type pubQueue struct {
	queued  int
	sending uuid.UUID
	running bool
	stopped bool
	drops   int64
}

func pubQueueOf(srv *Server) pubQueue {
	q := &srv.svcGrant.siem
	q.mu.Lock()
	defer q.mu.Unlock()
	return pubQueue{queued: len(q.q), sending: q.sending, running: q.running, stopped: q.stopped, drops: q.drops}
}

func pubFlights(srv *Server) int {
	srv.svcGrant.mu.Lock()
	defer srv.svcGrant.mu.Unlock()
	return len(srv.svcGrant.flights)
}

// pubFlightsIdle waits until no flight is running. A caller that returned at its deadline does not prove its
// flight ended, so a test waits here before it checks the sink for absence or counts.
func pubFlightsIdle(t *testing.T, srv *Server) {
	t.Helper()
	pubUntil(t, pubWait, "the flights to end", func() bool { return pubFlights(srv) == 0 })
}

func (f *svcFix) pubRespond(fn func(n int, r svcReq) svcAnswer) {
	f.idp.mu.Lock()
	defer f.idp.mu.Unlock()
	f.idp.respond = fn
}

func (f *svcFix) pubHoldSecrets(fn func(op, name string)) {
	f.sec.mu.Lock()
	defer f.sec.mu.Unlock()
	f.sec.hold = fn
}

// pubHoldFirstAnswer makes the provider hold its first answer until the returned release runs; Cleanup runs it too.
func (f *svcFix) pubHoldFirstAnswer(t *testing.T) (release func()) {
	t.Helper()
	g := newPubGate(t)
	f.pubRespond(func(n int, _ svcReq) svcAnswer {
		a := svcOK(n, 3600)
		if n == 1 {
			a.Block = g.free
		}
		return a
	})
	return g.release
}

// pubHoldSink makes the fixture's sink block every Emit until the returned release runs; Cleanup runs it too.
func (f *svcFix) pubHoldSink(t *testing.T) (release func()) {
	t.Helper()
	gate := make(chan struct{})
	release = sync.OnceFunc(func() { close(gate) })
	t.Cleanup(release)
	f.sink.mu.Lock()
	defer f.sink.mu.Unlock()
	f.sink.gate = gate
	return release
}

func (f *svcFix) pubTokenFor(p types.ModelProvider, trigger string) (serviceGrantMinted, error) {
	return f.srv.serviceGrantToken(context.Background(), p, "", trigger)
}

// pubRemint evicts the cache and asks for a token as the refresher does, so the attempt mints.
func (f *svcFix) pubRemint() (serviceGrantMinted, error) {
	f.srv.dropServiceGrantCache(f.uid)
	return f.token(serviceGrantTriggerRefresh)
}

func (f *svcFix) pubCache() (serviceGrantCached, bool) {
	f.srv.svcGrant.mu.Lock()
	defer f.srv.svcGrant.mu.Unlock()
	c, ok := f.srv.svcGrant.cache[f.uid]
	return c, ok
}

// pubKey is the digest and the secret's present revision that a cache read for p is keyed by.
func (f *svcFix) pubKey(p types.ModelProvider) (digest, rev string) {
	f.sec.mu.Lock()
	defer f.sec.mu.Unlock()
	return serviceGrantDigest(p), f.sec.revOf(serviceGrantSecretName(p.UID))
}

func (f *svcFix) pubStatus(uid string) (store.ServiceGrantStatusRow, bool) {
	return f.st.statusOf(uid, f.srv.svcGrant.replica())
}

func (f *svcFix) pubFailed(uid string) bool {
	f.srv.svcGrant.mu.Lock()
	defer f.srv.svcGrant.mu.Unlock()
	_, ok := f.srv.svcGrant.failed[uid]
	return ok
}

func (f *svcFix) pubRunning(uid string) bool {
	f.srv.svcGrant.mu.Lock()
	defer f.srv.svcGrant.mu.Unlock()
	return f.srv.svcGrant.running[uid]
}

// pubRequestsTo counts the provider's requests whose URL contains host.
func (f *svcFix) pubRequestsTo(host string) int {
	n := 0
	for _, r := range f.idp.requests() {
		if strings.Contains(r.URL, host) {
			n++
		}
	}
	return n
}

// pubRowsFor is the mint rows of the provider with uid.
func (f *svcFix) pubRowsFor(uid string) []types.AuditEvent {
	var out []types.AuditEvent
	for _, ev := range f.mintRows() {
		var d map[string]any
		if json.Unmarshal(ev.Data, &d) == nil && d["provider_uid"] == uid {
			out = append(out, ev)
		}
	}
	return out
}

func pubData(t *testing.T, ev types.AuditEvent) map[string]any {
	t.Helper()
	var d map[string]any
	if err := json.Unmarshal(ev.Data, &d); err != nil {
		t.Fatalf("a mint row's data %q: %v", ev.Data, err)
	}
	return d
}

// pubCause is the row's superseded_by, or "" for a row that has none.
func pubCause(t *testing.T, ev types.AuditEvent) string {
	t.Helper()
	s, _ := pubData(t, ev)["superseded_by"].(string)
	return s
}

// pubRecordFor is the secret store's record of p's grant with the given client secret.
func pubRecordFor(p types.ModelProvider, secret string) []byte {
	raw, _ := json.Marshal(serviceGrantRecord{V: 1, TokenURL: p.OIDC.ServiceGrant.TokenURL, ClientID: p.OIDC.ServiceGrant.ClientID,
		BaseURL: p.BaseURL, ClientSecret: secret})
	return raw
}

// pubCacheT1 mints the first token and moves the clock to five minutes before its end: inside the ten-minute reuse
// margin, so the next attempt mints, and outside the one-minute fallback margin, so a failed attempt may serve it.
func (f *svcFix) pubCacheT1() serviceGrantMinted {
	f.t.Helper()
	t1 := f.mustToken(serviceGrantTriggerRefresh)
	f.clock.Advance(55 * time.Minute)
	return t1
}

// pubNewSpool opens an audit spool in a temporary directory.
func pubNewSpool(t *testing.T) (*AuditSpool, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "audit-spool.jsonl")
	sp, err := NewAuditSpool(path)
	if err != nil {
		t.Fatalf("open the spool: %v", err)
	}
	return sp, path
}

// pubSpoolEvents are the events on the spool's lines.
func pubSpoolEvents(t *testing.T, path string) []types.AuditEvent {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the spool: %v", err)
	}
	var out []types.AuditEvent
	for _, line := range bytes.Split(raw, []byte("\n")) {
		var ev types.AuditEvent
		if len(bytes.TrimSpace(line)) > 0 && json.Unmarshal(line, &ev) == nil {
			out = append(out, ev)
		}
	}
	return out
}

func pubCountID(evs []types.AuditEvent, id uuid.UUID) int {
	n := 0
	for _, ev := range evs {
		if ev.ID == id {
			n++
		}
	}
	return n
}

// pubReplays drains the spool into a recorder that collects what it is given and counts the events with id.
func pubReplays(t *testing.T, sp *AuditSpool, id uuid.UUID) int {
	t.Helper()
	rec := &fakeRecorder{}
	if _, err := sp.Drain(context.Background(), rec, 10); err != nil {
		t.Fatalf("drain the spool: %v", err)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return pubCountID(rec.got, id)
}

// pubSyncFailing is a spool append that writes and syncs the whole line, then reports that the sync failed.
func pubSyncFailing(sp *AuditSpool) func(types.AuditEvent) error {
	return func(ev types.AuditEvent) error {
		if err := sp.Append(ev); err != nil {
			return err
		}
		return &fs.PathError{Op: "sync", Path: "spool", Err: syscall.EIO}
	}
}

// The store double's gates used by the publication-failure cases.
func pubCutGate(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}

func pubErrGate(context.Context) error { return errSvcDown }

func pubUnknownSettle(context.Context) error { return store.ErrServiceGrantCommitUnknown }

func pubLandedCommit(context.Context, types.AuditEvent) (bool, error) { return true, errSvcDown }

func pubLostCommit(context.Context, types.AuditEvent) (bool, error) { return false, errSvcDown }

// The decision is a pure function of what the transaction read. The first true case wins, in the order settings,
// retiring, secret_gone, revision, eviction, and a store that keeps no revision compares none. Each row below is one
// input of the decision and the cause it must give.
// Plant: swap the revision and secret_gone cases.
func TestServiceGrant_PublicationDecisionValues(t *testing.T) {
	const uid = "uid-decision"
	provider := func(edit func(*types.ModelProvider)) types.ModelProvider {
		p := types.ModelProvider{ID: "corp", UID: uid, BaseURL: "https://corp.example", OIDC: &types.ProviderOIDC{
			ServiceGrant: &types.ProviderServiceGrant{GrantType: types.ServiceGrantTypeClientCreds, TokenURL: svcTokenURL,
				ClientID: svcClientID, Scope: "api.read"},
		}}
		if edit != nil {
			edit(&p)
		}
		return p
	}
	siteOf := func(ps ...types.ModelProvider) types.SiteConfig {
		return types.SiteConfig{ModelProviders: &types.ModelProviders{Providers: ps}}
	}
	current := provider(nil)
	digest := serviceGrantDigest(current)
	claimed := store.ServiceGrantDoc{Cleanups: map[string]store.ServiceGrantCleanup{uid: {ID: "claim-1"}}}
	noDoc := store.ServiceGrantDoc{}
	disabled := provider(func(p *types.ModelProvider) { p.Disabled = true })
	noGrant := provider(func(p *types.ModelProvider) { p.OIDC.ServiceGrant = nil })
	moved := provider(func(p *types.ModelProvider) { p.OIDC.ServiceGrant.TokenURL = "https://other.example/token" })

	cases := []struct {
		name             string
		sc               types.SiteConfig
		doc              store.ServiceGrantDoc
		digest           string
		revOK            bool
		liveRev          string
		haveRev          bool
		genNow, cacheGen uint64
		want             string
	}{
		{"current, publishes", siteOf(current), noDoc, digest, true, "7", true, 3, 3, ""},
		{"locked revision differs", siteOf(current), noDoc, digest, true, "8", true, 3, 3, svcSupersededRevision},
		{"secret row absent", siteOf(current), noDoc, digest, true, "", true, 3, 3, svcSupersededSecretGone},
		{"row gone", siteOf(), noDoc, digest, true, "7", true, 3, 3, svcSupersededSettings},
		{"row disabled", siteOf(disabled), noDoc, digest, true, "7", true, 3, 3, svcSupersededSettings},
		{"grant removed", siteOf(noGrant), noDoc, digest, true, "7", true, 3, 3, svcSupersededSettings},
		{"digest differs", siteOf(moved), noDoc, digest, true, "7", true, 3, 3, svcSupersededSettings},
		{"clean-up claim", siteOf(current), claimed, digest, true, "7", true, 3, 3, svcSupersededRetiring},
		{"generation moved before the decision", siteOf(current), noDoc, digest, true, "7", true, 4, 3, svcSupersededEviction},
		{"no revision kept: a different revision is not compared", siteOf(current), noDoc, digest, false, "8", true, 3, 3, ""},
		{"no revision kept: an absent revision is not compared", siteOf(current), noDoc, digest, false, "", true, 3, 3, ""},
		{"no revision kept: the generation still decides", siteOf(current), noDoc, digest, false, "7", false, 4, 3, svcSupersededEviction},
		{"revision read before the transaction differs", siteOf(current), noDoc, digest, true, "8", true, 3, 3, svcSupersededRevision},
		{"revision read before the transaction is the same, generation moved", siteOf(current), noDoc, digest, true, "7", true, 4, 3, svcSupersededEviction},
		{"revision not read: nothing to compare", siteOf(current), noDoc, digest, true, "8", false, 3, 3, ""},
		{"settings beats retiring", siteOf(), claimed, digest, true, "", true, 4, 3, svcSupersededSettings},
		{"retiring beats secret_gone", siteOf(current), claimed, digest, true, "", true, 3, 3, svcSupersededRetiring},
		{"secret_gone beats revision and eviction", siteOf(current), noDoc, digest, true, "", true, 4, 3, svcSupersededSecretGone},
		{"revision beats eviction", siteOf(current), noDoc, digest, true, "8", true, 4, 3, svcSupersededRevision},
		{"every case true: settings", siteOf(moved), claimed, digest, true, "", true, 4, 3, svcSupersededSettings},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := serviceGrantSupersededBy(c.sc, c.doc, uid, c.digest, "7", c.revOK, c.liveRev, c.haveRev, c.genNow, c.cacheGen)
			if got != c.want {
				t.Fatalf("serviceGrantSupersededBy = %q, want %q", got, c.want)
			}
		})
	}
}

// A committed publication replaces the cache entry only while the row's generation is still the one it began under,
// and only over an entry from an older publication.
// Plant: return genNow == cacheGen alone.
func TestServiceGrant_CacheInstallValues(t *testing.T) {
	cases := []struct {
		name             string
		held             bool
		prevSeq, seq     int64
		genNow, cacheGen uint64
		want             bool
	}{
		{"no entry, generation the same", false, 0, 5, 2, 2, true},
		{"an older entry is replaced", true, 4, 5, 2, 2, true},
		{"a newer entry is kept", true, 6, 5, 2, 2, false},
		{"the same publication does not replace itself", true, 5, 5, 2, 2, false},
		{"no entry, generation moved", false, 0, 5, 3, 2, false},
		{"an older entry, generation moved", true, 4, 5, 3, 2, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := serviceGrantInstalls(c.held, c.prevSeq, c.seq, c.genNow, c.cacheGen); got != c.want {
				t.Fatalf("serviceGrantInstalls(held %v, prev %d, seq %d, gen %d, began %d) = %v, want %v",
					c.held, c.prevSeq, c.seq, c.genNow, c.cacheGen, got, c.want)
			}
		})
	}
}

// A caller that hangs up while its flight waits on the provider does not cut the flight others share: a second
// caller on the same flight gets the token, which is cached once and recorded in one success row.
// Plant: build the attempt context from the caller's context, without WithoutCancel.
func TestServiceGrant_CallerCancelDuringAnswerStillPublishes(t *testing.T) {
	f := newSvcFixWithSecret(t)
	ids := f.captureRowIDs()
	release := f.pubHoldFirstAnswer(t)
	p := f.provider()
	ctx1, cancel1 := context.WithCancel(context.Background())
	defer cancel1()
	first := pubGo(func() (serviceGrantMinted, error) {
		return f.srv.serviceGrantToken(ctx1, p, "", serviceGrantTriggerResolve)
	})
	pubUntil(t, pubWait, "the token request to reach the provider", func() bool { return f.idp.count() == 1 })
	second := pubGo(func() (serviceGrantMinted, error) {
		return f.srv.serviceGrantToken(context.Background(), p, "", serviceGrantTriggerResolve)
	})
	pubUntil(t, pubWait, "the second caller's revision read", func() bool {
		f.sec.mu.Lock()
		defer f.sec.mu.Unlock()
		return len(f.sec.revDeadlines) >= 2
	})
	cancel1()
	release()
	got := pubRecv(t, second, "the second caller")
	if got.err != nil || got.tok.token != svcToken(1) {
		t.Fatalf("the second caller = %q, %v; want the first token", got.tok.token, got.err)
	}
	pubRecv(t, first, "the first caller")
	if c, ok := f.pubCache(); !ok || c.token != svcToken(1) {
		t.Fatalf("cache = %q (present %v), want the first token", c.token, ok)
	}
	id := ids()[0]
	f.awaitRows(t, id)
	rows := f.mintRows()
	if len(rows) != 1 || rows[0].ID != id || rows[0].Outcome != "success" || pubCause(t, rows[0]) != "" {
		t.Fatalf("mint rows = %+v, want the one success row", rows)
	}
	if n := f.idp.count(); n != 1 {
		t.Fatalf("%d token requests, want 1", n)
	}
}

// A token is invisible until its row has committed. While the publication is held after its row is written and
// before the commit, the cache still serves the first token, a concurrent resolve has not returned, and the second
// token's row is nowhere; after the release both callers get the second token, and a later resolve is a cache hit.
// A publication held after the commit and before the install, with an eviction in between, still returns its token
// to both callers but does not cache it, so the next resolve sends a new request.
// Plant: install the cache inside Decide (the cache read then returns the second token during the hold); drop the
// generation test from serviceGrantInstalls (the evicted publication then caches its token).
func TestServiceGrant_TokenInvisibleUntilRowCommits(t *testing.T) {
	setup := func(t *testing.T) (*svcFix, *pubHook, func() []uuid.UUID, serviceGrantMinted) {
		f := newSvcFixWithSecret(t)
		h := pubInstallHook(f)
		ids := f.captureRowIDs()
		t1 := f.pubCacheT1()
		return f, h, ids, t1
	}
	notReturned := func(t *testing.T, ch <-chan pubResult) {
		t.Helper()
		select {
		case r := <-ch:
			t.Fatalf("a concurrent resolve returned %q, %v during the hold", r.tok.token, r.err)
		case <-time.After(300 * time.Millisecond):
		}
	}

	t.Run("after the row, before commit", func(t *testing.T) {
		f, h, ids, t1 := setup(t)
		g := newPubGate(t)
		h.on("publish.precommit", g.holdFirst)
		refresh := pubGo(func() (serviceGrantMinted, error) { return f.token(serviceGrantTriggerRefresh) })
		svcAwait(t, g.reached, "the publication to reach its commit")
		concurrent := pubGo(func() (serviceGrantMinted, error) { return f.token(serviceGrantTriggerResolve) })
		digest, rev := f.pubKey(f.provider())
		if got, ok := f.srv.cachedServiceGrantToken(f.uid, digest, rev, true, serviceGrantFallbackMargin); !ok || got.token != t1.token {
			t.Fatalf("direct cache read = %q (present %v), want the first token", got.token, ok)
		}
		if rows := f.st.rowsOf(ids()[1]); len(rows) != 0 {
			t.Fatalf("the second token's row is visible before its commit: %+v", rows)
		}
		notReturned(t, concurrent)
		g.release()
		for name, ch := range map[string]<-chan pubResult{"the refresh": refresh, "the resolve": concurrent} {
			if r := pubRecv(t, ch, name); r.err != nil || r.tok.token != svcToken(2) {
				t.Fatalf("%s = %q, %v; want the second token", name, r.tok.token, r.err)
			}
		}
		if rows := f.mintRows(); len(rows) != 2 || rows[0].Outcome != "success" || rows[1].Outcome != "success" {
			t.Fatalf("mint rows = %+v, want the first and second success rows only", rows)
		}
		before := f.idp.count()
		if tok, err := f.token(serviceGrantTriggerResolve); err != nil || tok.token != svcToken(2) || f.idp.count() != before || len(f.mintRows()) != 2 {
			t.Fatalf("one more resolve = %q, %v, %d requests, %d rows; want a cache hit with no request and no row",
				tok.token, err, f.idp.count()-before, len(f.mintRows()))
		}
	})

	t.Run("after commit, before install, evicted", func(t *testing.T) {
		f, h, _, _ := setup(t)
		g := newPubGate(t)
		h.on("publish.committed", g.holdFirst)
		refresh := pubGo(func() (serviceGrantMinted, error) { return f.token(serviceGrantTriggerRefresh) })
		svcAwait(t, g.reached, "the publication to commit")
		f.srv.dropServiceGrantCache(f.uid)
		concurrent := pubGo(func() (serviceGrantMinted, error) { return f.token(serviceGrantTriggerResolve) })
		notReturned(t, concurrent)
		g.release()
		for name, ch := range map[string]<-chan pubResult{"the refresh": refresh, "the resolve": concurrent} {
			if r := pubRecv(t, ch, name); r.err != nil || r.tok.token != svcToken(2) {
				t.Fatalf("%s = %q, %v; want the second token", name, r.tok.token, r.err)
			}
		}
		if f.cached() {
			t.Fatal("the evicted publication's token was cached")
		}
		before := f.idp.count()
		if tok, err := f.token(serviceGrantTriggerResolve); err != nil || tok.token != svcToken(3) || f.idp.count() != before+1 {
			t.Fatalf("one more resolve = %q, %v, %d requests; want a new request and the third token", tok.token, err, f.idp.count()-before)
		}
	})
}

// pubM5 is one publication-failure subtest's fixture: the row ids captured, the log captured and, when asked for, a
// spool in a temporary directory.
type pubM5 struct {
	f     *svcFix
	ids   func() []uuid.UUID
	logs  *pubLogs
	spool *AuditSpool
	path  string
}

func pubNewM5(t *testing.T, b pubBounds, spool bool) *pubM5 {
	t.Helper()
	pubShorten(t, b)
	f := newSvcFixWithSecret(t)
	m := &pubM5{f: f, ids: f.captureRowIDs(), logs: pubCaptureLogs(t)}
	f.srv.cfg.AuditSpool = nil
	if spool {
		m.spool, m.path = pubNewSpool(t)
		f.srv.cfg.AuditSpool = m.spool
	}
	return m
}

// last is the id of the latest request sent.
func (m *pubM5) last(t *testing.T) uuid.UUID {
	t.Helper()
	got := m.ids()
	if len(got) == 0 {
		t.Fatal("no mint row id was captured")
	}
	return got[len(got)-1]
}

// wantRow waits for id's one row and checks its superseded_by.
func (m *pubM5) wantRow(t *testing.T, id uuid.UUID, cause string) types.AuditEvent {
	t.Helper()
	m.f.awaitRows(t, id)
	rows := m.f.st.rowsOf(id)
	if len(rows) != 1 {
		t.Fatalf("rows for %s = %+v, want exactly one", id, rows)
	}
	if got := pubCause(t, rows[0]); got != cause {
		t.Fatalf("the row for %s is superseded_by %q, want %q", id, got, cause)
	}
	return rows[0]
}

func (m *pubM5) wantNoRow(t *testing.T, id uuid.UUID) {
	t.Helper()
	m.f.sinkSettled(t)
	if rows := m.f.st.rowsOf(id); len(rows) != 0 {
		t.Fatalf("rows for %s = %+v, want none", id, rows)
	}
}

// wantSinkOnce waits for id's copy, then checks that it arrived once and whether it carries hashes.
func (m *pubM5) wantSinkOnce(t *testing.T, id uuid.UUID, hashes bool) types.AuditEvent {
	t.Helper()
	got := m.f.awaitSink(t, id)[0]
	if (got.RowHash != "") != hashes {
		t.Fatalf("the sink's copy of %s has row hash %q, want hashes %v", id, got.RowHash, hashes)
	}
	m.f.sinkSettled(t)
	if n := m.f.sink.copiesOf(id); n != 1 {
		t.Fatalf("the sink holds %d copies of %s, want 1", n, id)
	}
	return got
}

func (m *pubM5) wantSinkNone(t *testing.T, id uuid.UUID) {
	t.Helper()
	m.f.sinkSettled(t)
	if n := m.f.sink.copiesOf(id); n != 0 {
		t.Fatalf("the sink holds %d copies of %s, want none", n, id)
	}
}

// wantSpool checks how many lines of the spool carry id, and that each is the publish_unavailable fallback.
func (m *pubM5) wantSpool(t *testing.T, id uuid.UUID, n int) {
	t.Helper()
	m.f.sinkSettled(t)
	evs := pubSpoolEvents(t, m.path)
	if got := pubCountID(evs, id); got != n {
		t.Fatalf("the spool holds %d lines of %s, want %d", got, id, n)
	}
	for _, ev := range evs {
		if ev.ID == id && pubCause(t, ev) != svcSupersededUnpublished {
			t.Fatalf("the spooled row of %s is %s, want the publish_unavailable fallback", id, ev.Data)
		}
	}
}

// wantLogged checks that one ERROR names id with the disposition, and no other disposition line does.
func (m *pubM5) wantLogged(t *testing.T, id uuid.UUID, disposition string, commitUnknown, spoolFailed bool) pubLogRec {
	t.Helper()
	m.f.sinkSettled(t)
	var hits []pubLogRec
	for _, r := range m.logs.forEvent(id) {
		if r.level == slog.LevelError && r.str("disposition") != "" {
			hits = append(hits, r)
		}
	}
	if len(hits) != 1 {
		t.Fatalf("%d ERROR lines name a disposition for %s, want exactly one: %+v", len(hits), id, hits)
	}
	r := hits[0]
	if got := r.str("disposition"); got != disposition {
		t.Fatalf("disposition of %s = %q, want %q", id, got, disposition)
	}
	if !strings.Contains(r.msg, "AUDIT WRITE FAILED") {
		t.Fatalf("the ERROR for %s reads %q, want AUDIT WRITE FAILED in it", id, r.msg)
	}
	if got, ok := r.flag("commit_unknown"); !ok || got != commitUnknown {
		t.Fatalf("commit_unknown of %s = %v (present %v), want %v", id, got, ok, commitUnknown)
	}
	if got, ok := r.flag("spool_append_failed"); !ok || got != spoolFailed {
		t.Fatalf("spool_append_failed of %s = %v (present %v), want %v", id, got, ok, spoolFailed)
	}
	return r
}

// wantSpoolFailure checks the spool-failed ERROR carries its message and the spool's error, and that a drain
// replays id once: the line the failed sync left is complete.
func (m *pubM5) wantSpoolFailure(t *testing.T, id uuid.UUID, r pubLogRec) {
	t.Helper()
	if !strings.Contains(r.msg, "AUDIT FALLBACK SPOOL FAILED (EVENT LOST)") {
		t.Fatalf("the ERROR for %s reads %q, want AUDIT FALLBACK SPOOL FAILED (EVENT LOST)", id, r.msg)
	}
	if got := r.str("spool_err"); !strings.Contains(got, "sync") {
		t.Fatalf("spool_err of %s = %q, want the spool's sync error", id, got)
	}
	if n := pubReplays(t, m.spool, id); n != 1 {
		t.Fatalf("a drain of the spool replayed %s %d times, want once", id, n)
	}
}

// The attempt answers a publication that does not commit with the transient error, never with the token it was
// about to publish, and each outcome of the row it owes is recorded and sent to the sinks as its case says. The
// publication bound is 300 ms.
// Plant: send the publish_unavailable candidate to the sink without settling (the landed-and-settled case then
// fails); log every no-spool failure as lost (the two ambiguous no-spool cases and the lost-answer case fail on the
// log); log every append error as lost (the spool-sync case finds lost); pick the message by ambiguity before the
// append's error (the combined case loses the spool-failed message); log the definite loss through
// audit.LogWriteFailure (the definite no-spool case finds no line with its id); treat a commit error as definite
// (both ambiguous settle-fails cases fail); in the double, wrap only a landed commit error as ambiguous (the
// not-landed settle-fails case fails).
func TestServiceGrant_PublicationFailureWithholdsNewToken(t *testing.T) {
	bound := pubBounds{publish: 300 * time.Millisecond}

	t.Run("definite: cut, nothing cached", func(t *testing.T) {
		m := pubNewM5(t, bound, true)
		m.f.st.setGates(pubCutGate, nil, nil, nil)
		_, err := m.f.token(serviceGrantTriggerRefresh)
		pubWantUnavailable(t, err)
		if m.f.cached() {
			t.Fatal("a token was cached after a publication that did not commit")
		}
		row, ok := m.f.pubStatus(m.f.uid)
		if !ok || row.Outcome != "failed" || row.Reason != svcReasonUnavailable || row.ValidUntil != nil {
			t.Fatalf("status = %+v (present %v), want failed/unavailable with no valid_until", row, ok)
		}
		id := m.last(t)
		m.wantRow(t, id, svcSupersededUnpublished)
		m.wantSinkOnce(t, id, true)
	})

	t.Run("definite: cut, T1 cached", func(t *testing.T) {
		m := pubNewM5(t, bound, true)
		t1 := m.f.pubCacheT1()
		m.f.st.setGates(pubCutGate, nil, nil, nil)
		tok, err := m.f.token(serviceGrantTriggerRefresh)
		if err != nil || tok.token != t1.token {
			t.Fatalf("answer = %q, %v; want the first token as the fallback", tok.token, err)
		}
		if c, ok := m.f.pubCache(); !ok || c.token != t1.token {
			t.Fatalf("cache = %q (present %v), want the first token and never the second", c.token, ok)
		}
		row, ok := m.f.pubStatus(m.f.uid)
		if !ok || row.Outcome != "failed" || row.ValidUntil == nil || !row.ValidUntil.Equal(t1.expiresAt) {
			t.Fatalf("status = %+v (present %v), want failed with the first token's valid_until %v", row, ok, t1.expiresAt)
		}
		m.wantRow(t, m.last(t), svcSupersededUnpublished)
	})

	t.Run("definite after Decide, T1 cached, settle held", func(t *testing.T) {
		m := pubNewM5(t, pubBounds{attempt: 1200 * time.Millisecond, token: 400 * time.Millisecond, tail: 700 * time.Millisecond,
			publish: 2000 * time.Millisecond}, true)
		t1 := m.f.pubCacheT1()
		settle := newPubGate(t)
		m.f.st.setGates(nil, pubErrGate, nil, settle.holdCtx)
		started := time.Now()
		tok, err := m.f.token(serviceGrantTriggerRefresh)
		took := time.Since(started)
		if err != nil || tok.token != t1.token {
			t.Fatalf("answer = %q, %v; want the first token as the fallback", tok.token, err)
		}
		if took >= 400*time.Millisecond {
			t.Fatalf("the caller waited %v, want its answer inside 400 ms, without the held settle", took)
		}
		row, ok := m.f.pubStatus(m.f.uid)
		if !ok || row.Outcome != "failed" || row.Reason != svcReasonUnavailable || row.ValidUntil == nil || !row.ValidUntil.Equal(t1.expiresAt) {
			t.Fatalf("status = %+v (present %v), want failed/unavailable with the first token's valid_until", row, ok)
		}
		svcAwait(t, settle.reached, "the settle to reach its gate")
		id := m.last(t)
		if rows := m.f.st.rowsOf(id); len(rows) != 0 {
			t.Fatalf("the held settle already recorded %+v", rows)
		}
		settle.release()
		m.wantRow(t, id, svcSupersededUnpublished)
	})

	t.Run("definite after Decide, settle fails", func(t *testing.T) {
		m := pubNewM5(t, bound, true)
		m.f.st.setGates(nil, pubErrGate, nil, pubErrGate)
		_, err := m.f.token(serviceGrantTriggerRefresh)
		pubWantUnavailable(t, err)
		id := m.last(t)
		m.wantSinkOnce(t, id, false)
		m.wantSpool(t, id, 1)
		m.wantNoRow(t, id)
	})

	t.Run("ambiguous, landed, settle succeeds", func(t *testing.T) {
		m := pubNewM5(t, bound, true)
		m.f.st.setGates(nil, nil, pubLandedCommit, nil)
		_, err := m.f.token(serviceGrantTriggerRefresh)
		pubWantUnavailable(t, err)
		if m.f.cached() {
			t.Fatal("a token was cached after a commit whose answer was lost")
		}
		id := m.last(t)
		row := m.wantRow(t, id, "")
		if row.Outcome != "success" {
			t.Fatalf("the row for %s is %s, want the success the database holds", id, row.Outcome)
		}
		got := m.wantSinkOnce(t, id, true)
		if !bytes.Equal(got.Data, row.Data) || got.RowHash != row.RowHash || got.PrevHash != row.PrevHash {
			t.Fatalf("the sink's copy %+v differs from the row %+v, want its data and hashes byte for byte", got, row)
		}
		for _, ev := range m.f.mintRows() {
			if pubCause(t, ev) == svcSupersededUnpublished {
				t.Fatalf("a publish_unavailable row exists beside the landed success: %+v", ev)
			}
		}
		for _, ev := range m.f.sink.mintCopies() {
			if pubCause(t, ev) == svcSupersededUnpublished {
				t.Fatalf("the sink holds a publish_unavailable copy: %+v", ev)
			}
		}
		m.wantSpool(t, id, 0)
	})

	t.Run("ambiguous, not landed, settle succeeds", func(t *testing.T) {
		m := pubNewM5(t, bound, true)
		m.f.st.setGates(nil, nil, pubLostCommit, nil)
		_, err := m.f.token(serviceGrantTriggerRefresh)
		pubWantUnavailable(t, err)
		id := m.last(t)
		m.wantRow(t, id, svcSupersededUnpublished)
		m.wantSinkOnce(t, id, true)
		m.wantSpool(t, id, 0)
	})

	t.Run("ambiguous, landed, settle fails", func(t *testing.T) {
		m := pubNewM5(t, bound, true)
		m.f.st.setGates(nil, nil, pubLandedCommit, pubErrGate)
		_, err := m.f.token(serviceGrantTriggerRefresh)
		pubWantUnavailable(t, err)
		id := m.last(t)
		m.wantSpool(t, id, 1)
		m.wantSinkNone(t, id)
		if rows := m.f.st.rowsOf(id); len(rows) != 1 || rows[0].Outcome != "success" {
			t.Fatalf("rows for %s = %+v, want the landed success", id, rows)
		}
	})

	t.Run("ambiguous, not landed, settle fails", func(t *testing.T) {
		m := pubNewM5(t, bound, true)
		m.f.st.setGates(nil, nil, pubLostCommit, pubErrGate)
		_, err := m.f.token(serviceGrantTriggerRefresh)
		pubWantUnavailable(t, err)
		id := m.last(t)
		m.wantSpool(t, id, 1)
		m.wantSinkNone(t, id)
		m.wantNoRow(t, id)
	})

	t.Run("definite, settle's answer lost", func(t *testing.T) {
		m := pubNewM5(t, bound, true)
		m.f.st.setGates(nil, pubErrGate, nil, pubUnknownSettle)
		_, err := m.f.token(serviceGrantTriggerRefresh)
		pubWantUnavailable(t, err)
		id := m.last(t)
		m.wantSpool(t, id, 1)
		m.wantSinkNone(t, id)
	})

	t.Run("definite, settle fails, no spool", func(t *testing.T) {
		m := pubNewM5(t, bound, false)
		m.f.st.setGates(nil, pubErrGate, nil, pubErrGate)
		_, err := m.f.token(serviceGrantTriggerRefresh)
		pubWantUnavailable(t, err)
		id := m.last(t)
		m.wantNoRow(t, id)
		m.wantLogged(t, id, "lost", false, false)
		m.wantSinkNone(t, id)
	})

	t.Run("ambiguous, landed, settle fails, no spool", func(t *testing.T) {
		m := pubNewM5(t, bound, false)
		m.f.st.setGates(nil, nil, pubLandedCommit, pubErrGate)
		_, err := m.f.token(serviceGrantTriggerRefresh)
		pubWantUnavailable(t, err)
		id := m.last(t)
		m.wantLogged(t, id, "unconfirmed", true, false)
		if rows := m.f.st.rowsOf(id); len(rows) != 1 || rows[0].Outcome != "success" {
			t.Fatalf("rows for %s = %+v, want the landed success", id, rows)
		}
		m.wantSinkNone(t, id)
	})

	t.Run("ambiguous, not landed, settle fails, no spool", func(t *testing.T) {
		m := pubNewM5(t, bound, false)
		m.f.st.setGates(nil, nil, pubLostCommit, pubErrGate)
		_, err := m.f.token(serviceGrantTriggerRefresh)
		pubWantUnavailable(t, err)
		id := m.last(t)
		m.wantNoRow(t, id)
		m.wantLogged(t, id, "unconfirmed", true, false)
		m.wantSinkNone(t, id)
	})

	t.Run("definite, settle's answer lost, no spool", func(t *testing.T) {
		m := pubNewM5(t, bound, false)
		m.f.st.setGates(nil, pubErrGate, nil, pubUnknownSettle)
		_, err := m.f.token(serviceGrantTriggerRefresh)
		pubWantUnavailable(t, err)
		id := m.last(t)
		m.wantNoRow(t, id)
		m.wantLogged(t, id, "unconfirmed", true, false)
		m.wantSinkNone(t, id)
	})

	t.Run("definite, settle fails, spool sync fails", func(t *testing.T) {
		m := pubNewM5(t, bound, true)
		m.f.srv.svcGrant.spoolAppend = pubSyncFailing(m.spool)
		m.f.st.setGates(nil, pubErrGate, nil, pubErrGate)
		_, err := m.f.token(serviceGrantTriggerRefresh)
		pubWantUnavailable(t, err)
		id := m.last(t)
		m.wantNoRow(t, id)
		m.wantSinkNone(t, id)
		r := m.wantLogged(t, id, "unconfirmed", false, true)
		m.wantSpoolFailure(t, id, r)
	})

	t.Run("ambiguous, not landed, settle fails, spool sync fails", func(t *testing.T) {
		m := pubNewM5(t, bound, true)
		m.f.srv.svcGrant.spoolAppend = pubSyncFailing(m.spool)
		m.f.st.setGates(nil, nil, pubLostCommit, pubErrGate)
		_, err := m.f.token(serviceGrantTriggerRefresh)
		pubWantUnavailable(t, err)
		id := m.last(t)
		m.wantNoRow(t, id)
		m.wantSinkNone(t, id)
		r := m.wantLogged(t, id, "unconfirmed", true, true)
		m.wantSpoolFailure(t, id, r)
	})
}

// One attempt ends within its own bound whatever its phases take, and a refresher slot is released when it does.
// The bounds are shortened to real milliseconds. A provider answering at 300 ms with a 150 ms publication read is
// published, cached and recorded ok, and the pass returns with the slot free. A publication read still running at
// the attempt's deadline is cut there, answers the transient error with nothing cached, and its row is settled. A
// failure row whose settle stalls holds neither the slot nor the status: the pass returns, the status row is
// written inside the budget, the pacing entry is set, and the settle ends on its own bound and spools the row. A
// late answer followed by a failed publication and slow re-reads still gets its status written, because the tail
// keeps time for it.
// Plant: wait for the request's row in the flight before the failure runs, so the status write follows the settle
// and is refused after the deadline; reserve only the publication and the re-reads in serviceGrantTryContext, so
// the combined path's status write has about 50 ms of slack left instead of about 640 ms.
func TestServiceGrant_AttemptBoundReleasesRefresherSlot(t *testing.T) {
	const answerAt = 300 * time.Millisecond
	base := pubBounds{attempt: 1200 * time.Millisecond, token: 400 * time.Millisecond, tail: 700 * time.Millisecond,
		publish: 200 * time.Millisecond, recheck: 200 * time.Millisecond}
	// slowFix answers the token request at answerAt and then makes the next bounded revision read, the
	// publication's, take read.
	slowFix := func(t *testing.T, read time.Duration) *svcFix {
		f := newSvcFixWithSecret(t)
		f.pubRespond(func(n int, _ svcReq) svcAnswer {
			time.Sleep(answerAt)
			f.sec.mu.Lock()
			f.sec.revDelay = read
			f.sec.mu.Unlock()
			return svcOK(n, 3600)
		})
		return f
	}

	t.Run("a delayed publish completes and frees the slot", func(t *testing.T) {
		pubShorten(t, base)
		f := slowFix(t, 150*time.Millisecond)
		started := time.Now()
		f.srv.serviceGrantPass(context.Background())
		took := time.Since(started)
		if took < answerAt || took >= serviceGrantAttemptBound+400*time.Millisecond {
			t.Fatalf("the pass took %v, want the attempt's work inside its %v bound", took, serviceGrantAttemptBound)
		}
		if f.pubRunning(f.uid) {
			t.Fatal("the refresher slot is still held after the pass returned")
		}
		if c, ok := f.pubCache(); !ok || c.token != svcToken(1) {
			t.Fatalf("cache = %q (present %v), want the delayed token published", c.token, ok)
		}
		if row, ok := f.pubStatus(f.uid); !ok || row.Outcome != "ok" || row.LastOKAt == nil {
			t.Fatalf("status row = %+v (present %v), want ok with the success time", row, ok)
		}
	})

	t.Run("a publish running at the attempt deadline is cut there", func(t *testing.T) {
		b := base
		b.publish = 10 * time.Second // the publication's own bound is longer, so the attempt's deadline is the one that cuts
		pubShorten(t, b)
		f := slowFix(t, 10*time.Second)
		ids := f.captureRowIDs()
		started := time.Now()
		_, err := f.token(serviceGrantTriggerRefresh)
		took := time.Since(started)
		pubWantUnavailable(t, err)
		if f.cached() {
			t.Fatal("a token was cached after a publication cut by the attempt's deadline")
		}
		if took < serviceGrantAttemptBound-50*time.Millisecond || took >= serviceGrantAttemptBound+time.Second {
			t.Fatalf("the attempt took %v, want it to end at its %v bound", took, serviceGrantAttemptBound)
		}
		pubFlightsIdle(t, f.srv)
		id := ids()[0]
		f.awaitRows(t, id)
		if rows := f.st.rowsOf(id); len(rows) != 1 || pubCause(t, rows[0]) != svcSupersededUnpublished {
			t.Fatalf("rows for %s = %+v, want one publish_unavailable row", id, rows)
		}
	})

	t.Run("a stalled failure row holds neither the slot nor the status", func(t *testing.T) {
		b := base
		b.publish = 2000 * time.Millisecond // also bounds the settle
		pubShorten(t, b)
		f := newSvcFixWithSecret(t)
		ids := f.captureRowIDs()
		sp, path := pubNewSpool(t)
		f.srv.cfg.AuditSpool = sp
		f.pubRespond(func(int, svcReq) svcAnswer { return svcOAuthError(401, "invalid_client") })
		// The settle blocks until its own context ends, as a row waiting on the chain lock does.
		f.st.setGates(nil, nil, nil, pubCutGate)
		started := time.Now()
		f.srv.serviceGrantPass(context.Background())
		took := time.Since(started)
		if took >= serviceGrantAttemptBound+400*time.Millisecond {
			t.Fatalf("the pass took %v, want it to return without waiting for the settle", took)
		}
		if f.pubRunning(f.uid) {
			t.Fatal("the refresher slot is still held after the pass returned")
		}
		// The double refuses a status write after its deadline, so a failed row proves it was written inside the budget.
		if row, ok := f.pubStatus(f.uid); !ok || row.Outcome != "failed" {
			t.Fatalf("status row = %+v (present %v), want failed", row, ok)
		}
		if !f.pubFailed(f.uid) {
			t.Fatal("the row has no failed entry, so nothing paces the next resolve")
		}
		id := ids()[0]
		pubUntil(t, 3*time.Second, "the failure row to reach the spool", func() bool {
			return pubCountID(pubSpoolEvents(t, path), id) == 1
		})
		if rows := f.st.rowsOf(id); len(rows) != 0 {
			t.Fatalf("the double holds %+v, want none: the stalled settle recorded nothing", rows)
		}
	})

	t.Run("late answer, failed publication, slow re-reads: the status is still written", func(t *testing.T) {
		pubShorten(t, pubBounds{attempt: 2400 * time.Millisecond, token: 2000 * time.Millisecond, tail: 1800 * time.Millisecond,
			publish: 600 * time.Millisecond, recheck: 600 * time.Millisecond})
		f := newSvcFixWithSecret(t)
		ids := f.captureRowIDs()
		// The try is cut by the tail, 600 ms into the attempt. The provider answers 10 ms before its request's deadline.
		f.pubRespond(func(n int, r svcReq) svcAnswer {
			time.Sleep(time.Until(r.Deadline) - 10*time.Millisecond)
			return svcOK(n, 3600)
		})
		// The gate makes the next deadline-bounded revision read, the failure's first re-read, take 560 ms, then blocks
		// until the publication's context ends.
		var gateEnd atomic.Int64
		f.st.setGates(func(ctx context.Context) error {
			f.sec.mu.Lock()
			f.sec.revDelay = 560 * time.Millisecond
			f.sec.mu.Unlock()
			<-ctx.Done()
			gateEnd.Store(time.Now().UnixNano())
			return ctx.Err()
		}, nil, nil, nil)
		started := time.Now()
		f.srv.serviceGrantPass(context.Background())
		took := time.Since(started)
		if took >= serviceGrantAttemptBound+400*time.Millisecond {
			t.Fatalf("the pass took %v, want it inside the attempt's %v bound", took, serviceGrantAttemptBound)
		}
		f.sec.mu.Lock()
		deadlines := append([]time.Time(nil), f.sec.revDeadlines...)
		f.sec.mu.Unlock()
		if len(deadlines) == 0 || gateEnd.Load() == 0 {
			t.Fatalf("revision reads = %d, gate ended = %v; want the publication gate to have run and a re-read after it", len(deadlines), gateEnd.Load())
		}
		reread := deadlines[len(deadlines)-1]
		if diff := reread.Sub(time.Unix(0, gateEnd.Load()).Add(serviceGrantRecheckBound)); diff < 0 || diff > 50*time.Millisecond {
			t.Fatalf("the re-read's deadline is %v from the end of the publication plus the recheck bound, want it within 50 ms", diff)
		}
		row, ok := f.pubStatus(f.uid)
		if !ok || row.Outcome != "failed" || row.Reason != svcReasonUnavailable {
			t.Fatalf("status row = %+v (present %v), want failed/unavailable", row, ok)
		}
		f.st.gmu.Lock()
		slack := f.st.lastStatusSlack
		f.st.gmu.Unlock()
		if slack < 300*time.Millisecond {
			t.Fatalf("the status write had %v left, want at least 300 ms (about 640 ms expected)", slack)
		}
		f.awaitRows(t, ids()[0])
	})
}

// A blocked sink never holds the attempt. With the sink's queue two long and its Emit blocked, the first copy is
// taken by the worker, two are queued and two are dropped, and all five publications return within their bound. After
// the sink is released it holds the three copies in queue order, which is seq order here because the publications
// run one after another.
// Plant: call sink.Emit inline in the publication, so the first attempt blocks until the release.
func TestServiceGrant_BlockedSinkNeverHoldsTheAttempt(t *testing.T) {
	pubShortenCaps(t, 0, 2)
	f := newSvcFixWithSecret(t)
	ids := f.captureRowIDs()
	release := f.pubHoldSink(t)
	publish := func(i int) {
		t.Helper()
		r := pubRecvWithin(t, pubGo(f.pubRemint), 5*time.Second, fmt.Sprintf("publication %d", i))
		if r.err != nil || r.tok.token != svcToken(i) {
			t.Fatalf("publication %d = %q, %v; want token %d", i, r.tok.token, r.err, i)
		}
	}
	publish(1)
	first := ids()[0]
	pubUntil(t, pubWait, "the worker to take the first copy", func() bool {
		q := pubQueueOf(f.srv)
		return q.queued == 0 && q.sending == first
	})
	for i := 2; i <= 5; i++ {
		publish(i)
	}
	if q := pubQueueOf(f.srv); q.queued != 2 || q.drops != 2 {
		t.Fatalf("queue = %+v, want one copy taken, two queued and two dropped", q)
	}
	release()
	f.sinkSettled(t)
	got := f.sink.mintCopies()
	all := ids()
	if len(all) != 5 || len(got) != 3 {
		t.Fatalf("%d request ids and %d sink copies, want 5 and 3", len(all), len(got))
	}
	for i, ev := range got {
		if ev.ID != all[i] || ev.RowHash != fmt.Sprintf("h%d", i+1) {
			t.Fatalf("copy %d is %s (row hash %q), want %s (h%d): queue order is seq order here", i, ev.ID, ev.RowHash, all[i], i+1)
		}
	}
}

// A mint that waits behind a held record lock gives up at its try's deadline, not at the attempt's: the caller
// answers unavailable within 400 ms + 300 ms, nothing is sent and no row is owed, the status row is failed, and
// the failure paces the next resolve, which sends nothing. A refresh attempt is not paced and then mints.
// Plant: make lockName ignore its context, so the caller returns only at the 1200 ms attempt bound.
func TestServiceGrant_NameLockWaitEndsAtTheTryDeadline(t *testing.T) {
	pubShorten(t, pubBounds{attempt: 1200 * time.Millisecond, token: 400 * time.Millisecond, tail: 700 * time.Millisecond})
	f := newSvcFixWithSecret(t)
	unlock, ok := f.srv.svcGrant.lockName(context.Background(), f.secretName())
	if !ok {
		t.Fatal("could not take the record's lock")
	}
	free := sync.OnceFunc(unlock)
	t.Cleanup(free)
	started := time.Now()
	_, err := f.token(serviceGrantTriggerResolve)
	took := time.Since(started)
	pubWantUnavailable(t, err)
	if took >= 700*time.Millisecond {
		t.Fatalf("the attempt took %v, want it to end at its try's 400 ms deadline (limit 700 ms)", took)
	}
	if n := f.idp.count(); n != 0 {
		t.Fatalf("%d token requests, want none", n)
	}
	if rows := f.mintRows(); len(rows) != 0 {
		t.Fatalf("mint rows = %+v, want none: nothing was sent", rows)
	}
	if row, ok := f.pubStatus(f.uid); !ok || row.Outcome != "failed" || row.Reason != svcReasonUnavailable {
		t.Fatalf("status row = %+v (present %v), want failed/unavailable", row, ok)
	}
	free()
	_, err = f.token(serviceGrantTriggerResolve)
	pubWantUnavailable(t, err)
	if n := f.idp.count(); n != 0 {
		t.Fatalf("an immediate resolve sent %d requests, want it paced by the failure", n)
	}
	if tok, err := f.token(serviceGrantTriggerRefresh); err != nil || tok.token != svcToken(1) {
		t.Fatalf("a refresh attempt = %q, %v; want it to mint, as the refresher is not paced", tok.token, err)
	}
}

// A caller leaves a flight that is wedged at its own deadline, and the flight stays one: twenty callers on one key
// each answer unavailable within 600 ms + 300 ms of their start, the refresher's slot is free, the flight map holds
// the one flight and the provider has seen one request. After the release, the flight's publication runs on an
// expired context and leaves one publish_unavailable row, nothing cached and an empty map. A second case wedges a
// flight inside the record read, where it holds the record's lock: its callers leave, a new key's flight gives up
// at its try's deadline, another grant mints meanwhile, and the old flight sends nothing when it is released.
// Plant: make a caller that finds a running flight past the elected attempt's deadline start a new one (the provider's
// count grows); make lockName ignore its context (the second flight is still in the map past its deadline); drop the
// try check before the permit (the old flight sends a request once the read returns).
func TestServiceGrant_CallersLeaveAWedgedFlightAtTheirDeadline(t *testing.T) {
	bounds := pubBounds{attempt: 600 * time.Millisecond, token: 300 * time.Millisecond, tail: 300 * time.Millisecond}

	t.Run("wedged before the publication", func(t *testing.T) {
		pubShorten(t, bounds)
		f := newSvcFixWithSecret(t)
		h := pubInstallHook(f)
		ids := f.captureRowIDs()
		wedge := newPubGate(t)
		h.on("publish.begin", wedge.holdFirst)
		// The first caller is a refresher pass, which elects the flight and owns the slot.
		pass := make(chan time.Duration, 1)
		go func() {
			started := time.Now()
			f.srv.serviceGrantPass(context.Background())
			pass <- time.Since(started)
		}()
		svcAwait(t, wedge.reached, "the elected attempt to reach publish.begin")
		var callers []<-chan pubResult
		for i := 0; i < 19; i++ {
			callers = append(callers, pubGo(func() (serviceGrantMinted, error) { return f.token(serviceGrantTriggerResolve) }))
			time.Sleep(50 * time.Millisecond)
		}
		if n := pubFlights(f.srv); n != 1 {
			t.Fatalf("%d flights while the wedge holds, want 1", n)
		}
		if n := f.idp.count(); n != 1 {
			t.Fatalf("%d token requests while the wedge holds, want 1", n)
		}
		for i, ch := range callers {
			r := pubRecv(t, ch, fmt.Sprintf("caller %d", i+1))
			pubWantUnavailable(t, r.err)
			if r.took >= 900*time.Millisecond {
				t.Fatalf("caller %d waited %v, want its answer inside 600 ms + 300 ms", i+1, r.took)
			}
		}
		select {
		case took := <-pass:
			if took >= 900*time.Millisecond {
				t.Fatalf("the pass took %v, want it inside 600 ms + 300 ms", took)
			}
		case <-time.After(pubWait):
			t.Fatal("the pass never returned")
		}
		if f.pubRunning(f.uid) {
			t.Fatal("the refresher slot is still held")
		}
		wedge.release()
		pubFlightsIdle(t, f.srv)
		id := ids()[0]
		f.awaitRows(t, id)
		if rows := f.st.rowsOf(id); len(rows) != 1 || pubCause(t, rows[0]) != svcSupersededUnpublished {
			t.Fatalf("rows for %s = %+v, want one publish_unavailable row", id, rows)
		}
		if f.cached() {
			t.Fatal("a token was cached by a publication on an expired context")
		}
		if n := f.idp.count(); n != 1 {
			t.Fatalf("%d token requests after the release, want 1", n)
		}
		f.sinkSettled(t)
		if n := f.sink.copiesOf(id); n != 1 {
			t.Fatalf("the sink holds %d copies of %s, want 1", n, id)
		}
	})

	t.Run("wedged inside the record read", func(t *testing.T) {
		pubShorten(t, bounds)
		f := newSvcFixWithSecret(t)
		g1 := f.addRow("wedge-one", "https://wedge-one.example.test/token")
		g2 := f.addRow("wedge-two", "https://wedge-two.example.test/token")
		g1Name := serviceGrantSecretName(g1.UID)
		gate := newPubGate(t)
		// The hold ignores the read's context, as a wedged recorder chain does, and holds only grant one's record.
		f.pubHoldSecrets(func(op, name string) {
			if op == "get" && name == g1Name {
				gate.hold()
			}
		})
		var first []<-chan pubResult
		for i := 0; i < 3; i++ {
			first = append(first, pubGo(func() (serviceGrantMinted, error) { return f.pubTokenFor(g1, serviceGrantTriggerResolve) }))
			time.Sleep(50 * time.Millisecond)
		}
		for i, ch := range first {
			r := pubRecv(t, ch, fmt.Sprintf("caller %d", i+1))
			pubWantUnavailable(t, r.err)
			if r.took >= 900*time.Millisecond {
				t.Fatalf("caller %d waited %v, want its answer inside 600 ms + 300 ms", i+1, r.took)
			}
		}
		if n := f.pubRequestsTo("wedge-one"); n != 0 {
			t.Fatalf("%d requests for the wedged grant, want none", n)
		}
		// A new revision is a new key, so a new flight, which waits on the record's lock.
		f.sec.set(g1Name, pubRecordFor(g1, svcSecretTwo))
		started := time.Now()
		_, err := f.pubTokenFor(g1, serviceGrantTriggerResolve)
		pubWantUnavailable(t, err)
		if took := time.Since(started); took >= 600*time.Millisecond {
			t.Fatalf("the second flight's caller waited %v, want it to end at its try's 300 ms deadline", took)
		}
		pubUntil(t, 100*time.Millisecond, "the second flight to leave the map", func() bool { return pubFlights(f.srv) == 1 })
		if n := f.pubRequestsTo("wedge-one"); n != 0 {
			t.Fatalf("%d requests for the wedged grant after the second flight, want none", n)
		}
		// Another grant mints meanwhile, so the block is outside the double's lock.
		if tok, err := f.pubTokenFor(g2, serviceGrantTriggerRefresh); err != nil || tok.token == "" {
			t.Fatalf("the other grant = %q, %v; want a token while the record read is wedged", tok.token, err)
		}
		before, ok := f.pubStatus(g1.UID)
		if !ok || before.Outcome != "failed" {
			t.Fatalf("status row of the wedged grant = %+v (present %v), want the second flight's failure", before, ok)
		}
		gate.release()
		pubFlightsIdle(t, f.srv)
		if n := f.pubRequestsTo("wedge-one"); n != 0 {
			t.Fatalf("%d requests for the wedged grant after the release, want none: its try had ended before the permit", n)
		}
		if rows := f.pubRowsFor(g1.UID); len(rows) != 0 {
			t.Fatalf("mint rows of the wedged grant = %+v, want none", rows)
		}
		if after, ok := f.pubStatus(g1.UID); !ok || after.Outcome != before.Outcome || !after.AttemptedAt.Equal(before.AttemptedAt) {
			t.Fatalf("status row of the wedged grant = %+v, want it as the second flight wrote it: %+v", after, before)
		}
		// The second flight's failure paces a resolve, and a refresh attempt is not paced.
		_, err = f.pubTokenFor(g1, serviceGrantTriggerResolve)
		pubWantUnavailable(t, err)
		if n := f.pubRequestsTo("wedge-one"); n != 0 {
			t.Fatalf("a paced resolve sent %d requests, want none", n)
		}
		if tok, err := f.pubTokenFor(g1, serviceGrantTriggerRefresh); err != nil || tok.token == "" {
			t.Fatalf("a refresh attempt = %q, %v; want it to mint", tok.token, err)
		}
		if n := f.pubRequestsTo("wedge-one"); n != 1 {
			t.Fatalf("%d requests for the wedged grant, want the refresh attempt's one", n)
		}
	})
}

// A mint row is a shared system action: it carries no sponsor, no proxy peer and no relay stamp from the request that
// happened to start the attempt, and its action seals nothing. A success row and a failure row are written under a
// context that carries all three stamps, and neither the rows nor their sink copies name any of them.
// Plant: stamp the event from the attempt's context with stampAuditEvent.
func TestServiceGrant_MintRowCarriesNoRequestStamps(t *testing.T) {
	if audit.SealsAction(auditServiceGrantMintAction) || audit.SealsTarget(auditServiceGrantMintAction) {
		t.Fatal("the mint action seals a field or its target, want neither")
	}
	f := newSvcFixWithSecret(t)
	ids := f.captureRowIDs()
	f.pubRespond(func(n int, _ svcReq) svcAnswer {
		if n == 1 {
			return svcOK(n, 3600)
		}
		return svcOAuthError(401, "invalid_client")
	})
	ctx := audit.WithRelay(audit.WithProxyPeer(audit.WithSponsor(context.Background(), "person:sponsor"), "10.9.8.7"), uuid.New())
	p := f.provider()
	if _, err := f.srv.serviceGrantToken(ctx, p, "", serviceGrantTriggerRefresh); err != nil {
		t.Fatalf("the first mint: %v", err)
	}
	f.srv.dropServiceGrantCache(f.uid)
	if _, err := f.srv.serviceGrantToken(ctx, p, "", serviceGrantTriggerRefresh); err == nil {
		t.Fatal("the second mint succeeded, want the provider's refusal")
	}
	got := ids()
	if len(got) != 2 {
		t.Fatalf("%d request ids, want 2", len(got))
	}
	f.awaitRows(t, got...)
	copies := f.awaitSink(t, got...)
	known := map[string]bool{"provider": true, "provider_uid": true, "client_id": true, "token_host": true, "scope": true,
		"audience": true, "trigger": true, "replica": true, "expires_at": true, "egress": true, "reason": true,
		"oauth_error": true, "http_status": true, "previous_valid_until": true, "superseded": true, "superseded_by": true}
	rows := f.mintRows()
	if len(rows) != 2 {
		t.Fatalf("%d mint rows, want 2", len(rows))
	}
	for _, ev := range append(rows, copies...) {
		if ev.SourceIP != "" || audit.SealsSponsor(ev) {
			t.Fatalf("a mint row names a source address or a sponsor: %+v", ev)
		}
		for key := range pubData(t, ev) {
			if !known[key] {
				t.Fatalf("a mint row carries the field %q, which no mint writes: %s", key, ev.Data)
			}
		}
	}
}

// Without an audit preparation no request is sent: a resolve answers unavailable, the provider sees nothing and no
// row exists.
// Plant: remove the check at the top of serviceGrantMint, so a request is sent.
func TestServiceGrant_PrepareMissingSendsNothing(t *testing.T) {
	f := newSvcFixWithSecret(t)
	f.srv.cfg.RunnerAuditPrepare = nil
	_, err := f.token(serviceGrantTriggerResolve)
	pubWantUnavailable(t, err)
	if n := f.idp.count(); n != 0 {
		t.Fatalf("%d token requests, want none", n)
	}
	f.sinkSettled(t)
	f.st.gmu.Lock()
	rows := len(f.st.rows)
	f.st.gmu.Unlock()
	if rows != 0 || len(f.sink.mintCopies()) != 0 {
		t.Fatalf("%d rows and %d sink copies, want none", rows, len(f.sink.mintCopies()))
	}
}

// The permit cap bounds token requests and unrecorded rows together, across grants. With three permits and five
// grants whose requests block at the provider, a fourth grant is refused before its record is read (no request, no
// status row, no pacing entry) and a fifth, which holds a token with five minutes left, answers that token. The
// callers of the three held requests return within 300 ms of the release without waiting for their held settles, the
// permits stay held until the settles end, a refused grant is still refused meanwhile, and once the settles end the
// refused grant sends a request at once.
// Plant: check the cap before the request but count at the settle (all five send); release the permit when the
// attempt returns rather than when its settle ends (step three sends); route a refusal through serviceGrantFailure
// (the fourth grant shows a failed status and is then paced); drop the fallback on refusal (the fifth answers
// unavailable); drop the load before the record read (the record reads grow by five); wait for the settle in the
// flight before answering (the three callers return only at their deadline).
func TestServiceGrant_PermitsBoundRequestsAndRowsAcrossGrants(t *testing.T) {
	pubShortenCaps(t, 3, 0)
	pubShorten(t, pubBounds{attempt: 2000 * time.Millisecond, token: 1200 * time.Millisecond, tail: 800 * time.Millisecond,
		recheck: 100 * time.Millisecond, publish: 10 * time.Second})
	f := newSvcFixWithSecret(t)
	ids := f.captureRowIDs()
	var grants [5]types.ModelProvider
	for i := range grants {
		grants[i] = f.addRow(fmt.Sprintf("permit-%d", i+1), fmt.Sprintf("https://permit-%d.example.test/token", i+1))
	}
	barrier := make(chan struct{})
	releaseBarrier := sync.OnceFunc(func() { close(barrier) })
	t.Cleanup(releaseBarrier)
	settled := make(chan struct{})
	releaseSettled := sync.OnceFunc(func() { close(settled) })
	t.Cleanup(releaseSettled)
	var phase atomic.Int32
	f.pubRespond(func(n int, _ svcReq) svcAnswer {
		if phase.Load() == 0 {
			return svcOK(n, 3600)
		}
		a := svcOAuthError(401, "invalid_client")
		a.Block = barrier
		return a
	})
	var waiting, peakWaiting atomic.Int64
	f.st.setGates(nil, nil, nil, func(ctx context.Context) error {
		n := waiting.Add(1)
		defer waiting.Add(-1)
		for {
			old := peakWaiting.Load()
			if n <= old || peakWaiting.CompareAndSwap(old, n) {
				break
			}
		}
		select {
		case <-settled:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	// The fifth grant holds a token this replica published, with five minutes left: inside the reuse margin, so its
	// attempt mints, and outside the fallback margin, so a refusal may serve it.
	t5, err := f.pubTokenFor(grants[4], serviceGrantTriggerRefresh)
	if err != nil {
		t.Fatalf("the fifth grant's first token: %v", err)
	}
	f.clock.Advance(55 * time.Minute)
	phase.Store(1)
	statusFive, _ := f.pubStatus(grants[4].UID)

	// Step 1: three requests hold the three permits; the fourth and fifth grants are refused at the load.
	readsBefore, requestsBefore := f.sec.callsOf("get"), f.idp.count()
	started := time.Now()
	held := make([]<-chan pubResult, 3)
	for i := range held {
		p := grants[i]
		held[i] = pubGo(func() (serviceGrantMinted, error) { return f.pubTokenFor(p, serviceGrantTriggerResolve) })
	}
	pubUntil(t, pubWait, "three requests in flight", func() bool { return f.idp.inFlight() == 3 })
	_, err = f.pubTokenFor(grants[3], serviceGrantTriggerResolve)
	if se := pubWantUnavailable(t, err); !se.refused {
		t.Fatalf("the fourth grant's answer = %+v, want a refusal at the cap", se)
	}
	if tok, err := f.pubTokenFor(grants[4], serviceGrantTriggerResolve); err != nil || tok.token != t5.token {
		t.Fatalf("the fifth grant's answer = %q, %v; want its token with five minutes left", tok.token, err)
	}
	if _, ok := f.pubStatus(grants[3].UID); ok || f.pubFailed(grants[3].UID) {
		t.Fatal("the refused grant has a status row or a failed entry")
	}
	if row, _ := f.pubStatus(grants[4].UID); !row.AttemptedAt.Equal(statusFive.AttemptedAt) || f.pubFailed(grants[4].UID) {
		t.Fatalf("the fifth grant's status row changed to %+v or it has a failed entry", row)
	}
	if four, five := f.pubRequestsTo("permit-4"), f.pubRequestsTo("permit-5"); four != 0 || five != 1 {
		t.Fatalf("requests for the fourth and fifth grants = %d and %d, want 0 and the fifth's one earlier token", four, five)
	}
	if n := f.sec.callsOf("get") - readsBefore; n != 3 {
		t.Fatalf("record reads grew by %d, want 3: a refused grant reads nothing", n)
	}

	// Step 2: the release lets the three requests fail; their callers do not wait for the held settles.
	releaseBarrier()
	releasedAt := time.Now()
	// The refusals and the barrier's release both come inside the 1200 ms token bound the held requests live under.
	if elapsed := releasedAt.Sub(started); elapsed >= 1000*time.Millisecond {
		t.Fatalf("the refusals and the barrier's release took %v from the first resolve, want under 1000 ms", elapsed)
	}
	for i, ch := range held {
		r := pubRecvWithin(t, ch, time.Until(releasedAt.Add(300*time.Millisecond)), fmt.Sprintf("held grant %d", i+1))
		if se := svcErr(t, r.err); se.reason != "invalid_client" {
			t.Fatalf("held grant %d answered %+v, want its provider's invalid_client", i+1, se)
		}
	}
	pubUntil(t, pubWait, "three settles waiting", func() bool { return waiting.Load() == 3 })
	if n := f.srv.svcGrant.permits.Load(); n != 3 {
		t.Fatalf("%d permits held after the callers returned, want 3: the settles own them", n)
	}

	// Step 3: the fourth grant is still refused before its record is read.
	readsNow := f.sec.callsOf("get")
	_, err = f.pubTokenFor(grants[3], serviceGrantTriggerResolve)
	pubWantUnavailable(t, err)
	if n, reads := f.idp.count()-requestsBefore, f.sec.callsOf("get")-readsNow; n != 3 || reads != 0 {
		t.Fatalf("after the fourth grant's second try: %d requests since step one and %d more reads, want 3 and 0", n, reads)
	}

	// Step 4: the settles end, the permits return, and the fourth grant sends at once.
	releaseSettled()
	pubUntil(t, 2*time.Second, "the permits to return", func() bool { return f.srv.svcGrant.permits.Load() == 0 })
	_, err = f.pubTokenFor(grants[3], serviceGrantTriggerResolve)
	if se := svcErr(t, err); se.refused || se.reason != "invalid_client" {
		t.Fatalf("the fourth grant's answer after the permits returned = %+v, want its request's invalid_client", se)
	}
	if n := f.idp.count() - requestsBefore; n != 4 {
		t.Fatalf("%d requests since step one, want 4", n)
	}
	f.awaitRows(t, ids()...)

	// Step 5: the cap held.
	if peak := f.idp.peak(); peak != 3 {
		t.Fatalf("the provider saw %d requests at once, want 3", peak)
	}
	if peak := peakWaiting.Load(); peak != 3 {
		t.Fatalf("%d settles waited at once, want 3", peak)
	}
}

// Sink copies leave in queue order, which is not commit order. Attempt A commits seq 1 and is held before it queues
// its copy; the secret's revision moves and attempt B, under the new key, commits seq 2 and queues its copy. After A
// is released the sink holds both copies, B's first, and neither is dropped.
// Plant: queue a copy only when its seq is the highest seen, so A's copy is lost.
func TestServiceGrant_SinkCopiesFollowQueueOrderNotCommitOrder(t *testing.T) {
	f := newSvcFixWithSecret(t)
	h := pubInstallHook(f)
	ids := f.captureRowIDs()
	g := newPubGate(t)
	h.on("publish.committed", g.holdFirst)
	a := pubGo(func() (serviceGrantMinted, error) { return f.token(serviceGrantTriggerRefresh) })
	svcAwait(t, g.reached, "attempt A to commit")
	f.writeSecret(svcSecretTwo)
	b := pubGo(func() (serviceGrantMinted, error) { return f.token(serviceGrantTriggerRefresh) })
	if r := pubRecv(t, b, "attempt B"); r.err != nil || r.tok.token != svcToken(2) {
		t.Fatalf("attempt B = %q, %v; want the second token", r.tok.token, r.err)
	}
	g.release()
	if r := pubRecv(t, a, "attempt A"); r.err != nil || r.tok.token != svcToken(1) {
		t.Fatalf("attempt A = %q, %v; want the first token, published before the revision moved", r.tok.token, r.err)
	}
	f.sinkSettled(t)
	got, all := f.sink.mintCopies(), ids()
	if len(all) != 2 || len(got) != 2 {
		t.Fatalf("%d request ids and %d sink copies, want 2 and 2", len(all), len(got))
	}
	idA, idB := all[0], all[1]
	if got[0].ID != idB || got[0].RowHash != "h2" || got[1].ID != idA || got[1].RowHash != "h1" {
		t.Fatalf("copies = %s (%s), %s (%s); want B's seq 2 first, then A's seq 1 (A %s, B %s)",
			got[0].ID, got[0].RowHash, got[1].ID, got[1].RowHash, idA, idB)
	}
}

// pubFailFrom makes every token request from the nth on fail at the provider.
func pubFailFrom(first int) func(n int, r svcReq) svcAnswer {
	return func(n int, _ svcReq) svcAnswer {
		if n >= first {
			return svcOAuthError(401, "invalid_client")
		}
		return svcOK(n, 3600)
	}
}

// Each sink copy ends exactly once at a stop, and the end is logged. With a blocked sink: the stop counts the queued
// copies as dropped and logs the copy the worker holds as unconfirmed; the wait for background work ends at its
// budget; a settle that records its row after the stop has its copy dropped on arrival; and once the sink is
// released it holds only the copy it had taken. A copy that arrives after the context is cancelled is dropped before
// any stop has run. A stop on a replica whose serve loop failed, with a settle still pending, drops that settle's
// late copy once. A stop between the worker's dequeue and its call to the sink counts nothing as dropped and logs the
// held copy as unconfirmed; the copy may still reach the sink after the stop, which is the stated limit.
// Plant: leave queued copies uncounted at the stop (step one's drops is 0); read only the stopped flag and not the
// context at admission and dequeue (the first-use case sends its copy); read only the context and not the stopped
// flag (the listener-exit case records an Emit after close); count the taken copy as dropped at the stop (the
// between-dequeue-and-Emit case has drops 1 while afterClose holds the copy).
func TestServiceGrant_SinkCopiesAtStopAreCountedOnce(t *testing.T) {
	t.Run("stop with a blocked sink", func(t *testing.T) {
		pubShortenCaps(t, 0, 4)
		f := newSvcFixWithSecret(t)
		logs := pubCaptureLogs(t)
		f.srv.bgWaitBudget = 300 * time.Millisecond
		ids := f.captureRowIDs()
		sinkGate := f.pubHoldSink(t)
		settle := newPubGate(t)
		f.st.setGates(nil, nil, nil, settle.holdCtx)
		f.pubRespond(pubFailFrom(4))
		for i := 1; i <= 3; i++ {
			if r := pubRecvWithin(t, pubGo(f.pubRemint), 5*time.Second, fmt.Sprintf("publication %d", i)); r.err != nil {
				t.Fatalf("publication %d: %v", i, r.err)
			}
			if i == 1 {
				pubUntil(t, pubWait, "the first copy to be inside Emit", func() bool {
					q := pubQueueOf(f.srv)
					return f.sink.entered.Load() == 1 && q.queued == 0 && q.sending == ids()[0]
				})
			}
		}
		fourth := pubGo(f.pubRemint)
		svcAwait(t, settle.reached, "the fourth attempt's settle to reach its gate")
		all := ids()
		if len(all) != 4 {
			t.Fatalf("%d request ids, want 4", len(all))
		}
		taken, queued2, queued3, late := all[0], all[1], all[2], all[3]

		// One: cancel, then stop.
		f.stop()
		if !f.srv.StopServiceGrantSIEM() {
			t.Fatal("the first stop reported that it did not stop the queue")
		}
		if q := pubQueueOf(f.srv); q.drops != 2 {
			t.Fatalf("drops = %d after the stop, want the two queued copies", q.drops)
		}
		var listed []string
		for _, r := range logs.all() {
			if v, ok := r.attrs["event_ids"]; ok && r.level == slog.LevelWarn && r.str("copy") == "dropped" {
				listed, _ = v.Any().([]string)
			}
		}
		if len(listed) != 2 || !(listed[0] == queued2.String() && listed[1] == queued3.String() ||
			listed[0] == queued3.String() && listed[1] == queued2.String()) {
			t.Fatalf("the stop's dropped WARN lists %v, want %s and %s", listed, queued2, queued3)
		}
		if got := pubCopyRecs(logs, taken, "unconfirmed"); len(got) != 1 {
			t.Fatalf("%d unconfirmed lines for the taken copy, want 1", len(got))
		}

		// Two: the wait for background work ends at its budget, because the worker is inside the sink.
		started := time.Now()
		f.srv.WaitBackground()
		if took := time.Since(started); took < 250*time.Millisecond || took > 5*time.Second {
			t.Fatalf("WaitBackground took %v, want it to end at its 300 ms budget", took)
		}
		if len(logs.withMsg(slog.LevelWarn, "shutdown budget hit")) == 0 {
			t.Fatal("WaitBackground logged no budget WARN")
		}

		// Three: the late settle records its row, and its copy is dropped on arrival.
		settle.release()
		f.awaitRows(t, late)
		pubUntil(t, pubWait, "the late copy to be counted as dropped", func() bool { return pubQueueOf(f.srv).drops == 3 })
		if got := pubCopyRecs(logs, late, "dropped"); len(got) != 1 {
			t.Fatalf("%d dropped lines for the late copy, want 1", len(got))
		}
		if r := pubRecv(t, fourth, "the fourth attempt"); svcErr(t, r.err).reason != "invalid_client" {
			t.Fatalf("the fourth attempt answered %v, want the provider's invalid_client", r.err)
		}

		// Four: the sink is released and holds only the copy it had taken.
		sinkGate()
		pubUntil(t, pubWait, "the taken copy to reach the sink", func() bool { return f.sink.copiesOf(taken) == 1 })
		f.srv.WaitBackground()
		if got := f.sink.mintCopies(); len(got) != 1 || got[0].ID != taken {
			t.Fatalf("the sink holds %+v, want only the taken copy %s", got, taken)
		}
		if q := pubQueueOf(f.srv); q.drops != 3 {
			t.Fatalf("drops = %d at the end, want 3", q.drops)
		}
		if f.srv.StopServiceGrantSIEM() {
			t.Fatal("a second stop reported that it stopped the queue")
		}
	})

	t.Run("first use after cancellation", func(t *testing.T) {
		f := newSvcFixWithSecret(t)
		logs := pubCaptureLogs(t)
		ids := f.captureRowIDs()
		f.stop()
		if _, err := f.token(serviceGrantTriggerRefresh); err != nil {
			t.Fatalf("the publication: %v", err)
		}
		id := ids()[0]
		if q := pubQueueOf(f.srv); q.drops != 1 || q.running || q.stopped {
			t.Fatalf("queue = %+v, want one drop, no worker and no stop", q)
		}
		if got := pubCopyRecs(logs, id, "dropped"); len(got) != 1 {
			t.Fatalf("%d dropped lines for the copy, want 1", len(got))
		}
		if n := f.sink.entered.Load(); n != 0 || len(f.sink.copies()) != 0 {
			t.Fatalf("the sink was entered %d times and holds %d copies, want nothing", n, len(f.sink.copies()))
		}
	})

	t.Run("listener exit with a pending settle", func(t *testing.T) {
		f := newSvcFixWithSecret(t)
		logs := pubCaptureLogs(t)
		ids := f.captureRowIDs()
		settle := newPubGate(t)
		f.st.setGates(nil, nil, nil, settle.holdCtx)
		f.pubRespond(pubFailFrom(1))
		failing := pubGo(f.pubRemint)
		svcAwait(t, settle.reached, "the settle to reach its gate")
		id := ids()[0]
		if !f.srv.StopServiceGrantSIEM() {
			t.Fatal("the stop reported that it did not stop the queue")
		}
		f.sink.mu.Lock()
		f.sink.closed = true
		f.sink.mu.Unlock()
		settle.release()
		pubRecv(t, failing, "the failing attempt")
		f.awaitRows(t, id)
		pubUntil(t, pubWait, "the settle's permit to return", func() bool { return f.srv.svcGrant.permits.Load() == 0 })
		if q := pubQueueOf(f.srv); q.drops != 1 {
			t.Fatalf("drops = %d, want the late copy counted once", q.drops)
		}
		if got := pubCopyRecs(logs, id, "dropped"); len(got) != 1 {
			t.Fatalf("%d dropped lines for the late copy, want 1", len(got))
		}
		f.sink.mu.Lock()
		after, held := len(f.sink.afterClose), len(f.sink.evs)
		f.sink.mu.Unlock()
		if after != 0 || held != 0 {
			t.Fatalf("the sink recorded %d copies after its close and holds %d, want none", after, held)
		}
	})

	t.Run("stop between dequeue and Emit", func(t *testing.T) {
		f := newSvcFixWithSecret(t)
		h := pubInstallHook(f)
		logs := pubCaptureLogs(t)
		ids := f.captureRowIDs()
		emit := newPubGate(t)
		h.on("siem.emit", emit.holdFirst)
		f.mustToken(serviceGrantTriggerRefresh)
		id := ids()[0]
		svcAwait(t, emit.reached, "the worker to reach siem.emit")
		pubUntil(t, pubWait, "the worker to hold the copy", func() bool { return pubQueueOf(f.srv).sending == id })
		if !f.srv.StopServiceGrantSIEM() {
			t.Fatal("the stop reported that it did not stop the queue")
		}
		if q := pubQueueOf(f.srv); q.drops != 0 {
			t.Fatalf("drops = %d, want 0: the held copy is not counted as dropped", q.drops)
		}
		if got := pubCopyRecs(logs, id, "unconfirmed"); len(got) != 1 {
			t.Fatalf("%d unconfirmed lines for the held copy, want 1", len(got))
		}
		f.sink.mu.Lock()
		f.sink.closed = true
		f.sink.mu.Unlock()
		emit.release()
		pubUntil(t, pubWait, "the held copy to reach the closed sink", func() bool {
			f.sink.mu.Lock()
			defer f.sink.mu.Unlock()
			return len(f.sink.afterClose) == 1
		})
		f.sink.mu.Lock()
		afterID, held := f.sink.afterClose[0].ID, len(f.sink.evs)
		f.sink.mu.Unlock()
		if afterID != id || held != 0 {
			t.Fatalf("after the close the sink recorded %s and holds %d copies, want %s once and none", afterID, held, id)
		}
		if q := pubQueueOf(f.srv); q.drops != 0 {
			t.Fatalf("drops = %d at the end, want 0", q.drops)
		}
		if got := pubCopyRecs(logs, id, "dropped"); len(got) != 0 {
			t.Fatalf("%d dropped lines for the held copy, want none", len(got))
		}
	})
}

// The sink worker runs only while copies are queued. After one publication the sink holds its copy, background work
// finishes within a second and the worker is not running; a second publication starts a worker again, and the sink
// then holds both copies. A stop on the idle queue reports that it stopped it and logs nothing, because it dropped
// nothing and no copy was taken.
// Plant: loop until the context ends (WaitBackground then waits its 3 s budget); log the stop's WARN on every call
// (the silent stop logs a line with dropped 0).
func TestServiceGrant_SinkWorkerEndsWhenQueueEmpties(t *testing.T) {
	f := newSvcFixWithSecret(t)
	logs := pubCaptureLogs(t)
	f.srv.bgWaitBudget = 3 * time.Second
	ids := f.captureRowIDs()
	f.mustToken(serviceGrantTriggerRefresh)
	first := ids()[0]
	f.awaitSink(t, first)
	started := time.Now()
	f.srv.WaitBackground()
	if took := time.Since(started); took >= time.Second {
		t.Fatalf("WaitBackground took %v, want it to return within a second: no worker waits on an empty queue", took)
	}
	if q := pubQueueOf(f.srv); q.running {
		t.Fatalf("queue = %+v, want no worker running", q)
	}
	if _, err := f.pubRemint(); err != nil {
		t.Fatalf("the second publication: %v", err)
	}
	second := ids()[1]
	f.awaitSink(t, first, second)
	f.srv.WaitBackground()
	if q := pubQueueOf(f.srv); q.running || q.sending != uuid.Nil {
		t.Fatalf("queue = %+v, want the worker ended again", q)
	}
	logged := len(logs.all())
	if !f.srv.StopServiceGrantSIEM() {
		t.Fatal("the stop reported that it did not stop the queue")
	}
	if n := len(logs.all()); n != logged {
		t.Fatalf("the stop of an idle queue logged %d lines, want none", n-logged)
	}
	if q := pubQueueOf(f.srv); q.drops != 0 {
		t.Fatalf("drops = %d, want 0", q.drops)
	}
	if a, b := f.sink.copiesOf(first), f.sink.copiesOf(second); a != 1 || b != 1 {
		t.Fatalf("the sink holds %d and %d copies of the two rows, want 1 and 1", a, b)
	}
}

// A panic in a flight, in a settle or in the sink ends that one piece of work with a logged ERROR and stack, never
// the process. The caller of a panicking flight gets unavailable, the flight map is empty and the permit is back; the
// next resolve starts a new flight and mints. A settle that panics releases its permit and names its row in the
// ERROR; the next failure row settles. A sink that panics on its first copy logs that copy as unconfirmed and drops
// nothing; the second copy is sent by a new worker.
// Plant: drop the recover in the flight; drop the recover in the settle; drop the recover in the sink worker. Each
// ends the test binary with the panic.
func TestServiceGrant_FlightPanicAnswersUnavailable(t *testing.T) {
	t.Run("the flight panics", func(t *testing.T) {
		f := newSvcFixWithSecret(t)
		h := pubInstallHook(f)
		logs := pubCaptureLogs(t)
		var fired atomic.Bool
		h.on("publish.begin", func() {
			if fired.CompareAndSwap(false, true) {
				panic("planted: publish.begin")
			}
		})
		_, err := f.token(serviceGrantTriggerRefresh)
		pubWantUnavailable(t, err)
		if n := pubFlights(f.srv); n != 0 {
			t.Fatalf("%d flights after the panic, want none", n)
		}
		if n := f.srv.svcGrant.permits.Load(); n != 0 {
			t.Fatalf("%d permits held after the panic, want 0", n)
		}
		recs := logs.withMsg(slog.LevelError, "attempt panicked")
		if len(recs) != 1 || recs[0].str("stack") == "" {
			t.Fatalf("panic ERROR lines = %+v, want one with a stack", recs)
		}
		tok, err := f.token(serviceGrantTriggerResolve)
		if err != nil || tok.token != svcToken(2) {
			t.Fatalf("the next resolve = %q, %v; want a new flight that mints the second token", tok.token, err)
		}
	})

	t.Run("settle panics", func(t *testing.T) {
		f := newSvcFixWithSecret(t)
		logs := pubCaptureLogs(t)
		ids := f.captureRowIDs()
		f.pubRespond(pubFailFrom(1))
		var fired atomic.Bool
		f.st.setGates(nil, nil, nil, func(context.Context) error {
			if fired.CompareAndSwap(false, true) {
				panic("planted: settle")
			}
			return nil
		})
		_, err := f.token(serviceGrantTriggerRefresh)
		if se := svcErr(t, err); se.reason != "invalid_client" {
			t.Fatalf("the first failure answered %+v, want invalid_client", se)
		}
		first := ids()[0]
		pubUntil(t, pubWait, "the panicking settle's permit to return", func() bool { return f.srv.svcGrant.permits.Load() == 0 })
		var hits []pubLogRec
		for _, r := range logs.forEvent(first) {
			if r.level == slog.LevelError && strings.Contains(r.msg, "settle panicked") {
				hits = append(hits, r)
			}
		}
		if len(hits) != 1 || hits[0].str("stack") == "" {
			t.Fatalf("settle panic ERROR lines for %s = %+v, want one with a stack", first, hits)
		}
		if _, err := f.token(serviceGrantTriggerRefresh); err == nil {
			t.Fatal("the second failure answered with a token")
		}
		f.awaitRows(t, ids()[1])
	})

	t.Run("sink panics", func(t *testing.T) {
		f := newSvcFixWithSecret(t)
		logs := pubCaptureLogs(t)
		ids := f.captureRowIDs()
		f.sink.mu.Lock()
		f.sink.panicNext = true
		f.sink.mu.Unlock()
		f.mustToken(serviceGrantTriggerRefresh)
		first := ids()[0]
		pubUntil(t, pubWait, "the sink panic to be logged", func() bool { return len(pubCopyRecs(logs, first, "unconfirmed")) == 1 })
		if r := pubCopyRecs(logs, first, "unconfirmed")[0]; r.level != slog.LevelError || r.str("stack") == "" {
			t.Fatalf("the sink panic line = %+v, want an ERROR with a stack", r)
		}
		if _, err := f.pubRemint(); err != nil {
			t.Fatalf("the second publication: %v", err)
		}
		second := ids()[1]
		f.awaitSink(t, second)
		f.sinkSettled(t)
		if a, b := f.sink.copiesOf(first), f.sink.copiesOf(second); a != 0 || b != 1 {
			t.Fatalf("the sink holds %d and %d copies of the two rows, want none of the first and one of the second", a, b)
		}
		if q := pubQueueOf(f.srv); q.drops != 0 {
			t.Fatalf("drops = %d, want 0: the held copy is not counted twice", q.drops)
		}
	})
}

// A refusal at the final reservation, after a record read that raced a rotation, serves no cached token. Grant G holds
// a token with five minutes left and its record read blocks; grant H takes the one permit and its request blocks. A
// rotation of G's record is written meanwhile, then G's read returns well inside its try: G is refused at the
// reservation and answers unavailable, not its old token, with one WARN, no failed entry, no new status row and no
// request.
// Plant: serve the fallback on every refusal (G answers its old token); take the permit before the record read (H is
// refused, and G sends a request).
func TestServiceGrant_LateRefusalAfterARotationServesNoFallback(t *testing.T) {
	pubShortenCaps(t, 1, 0)
	pubShorten(t, pubBounds{attempt: 2000 * time.Millisecond, token: 1200 * time.Millisecond, tail: 800 * time.Millisecond})
	f := newSvcFixWithSecret(t)
	logs := pubCaptureLogs(t)
	g := f.addRow("late-g", "https://late-g.example.test/token")
	h := f.addRow("late-h", "https://late-h.example.test/token")
	gName := serviceGrantSecretName(g.UID)
	barrier := make(chan struct{})
	releaseBarrier := sync.OnceFunc(func() { close(barrier) })
	t.Cleanup(releaseBarrier)
	var phase atomic.Int32
	f.pubRespond(func(n int, _ svcReq) svcAnswer {
		if phase.Load() == 0 {
			return svcOK(n, 3600)
		}
		a := svcOAuthError(401, "invalid_client")
		a.Block = barrier
		return a
	})
	gGate := newPubGate(t)
	var armed atomic.Bool
	f.pubHoldSecrets(func(op, name string) {
		if op == "get" && name == gName && armed.Load() {
			gGate.hold()
		}
	})
	// G's token was published under G's current revision and has five minutes left: inside the reuse margin, so its
	// attempt mints, and outside the fallback margin, so a refusal at the load could serve it.
	old, err := f.pubTokenFor(g, serviceGrantTriggerRefresh)
	if err != nil {
		t.Fatalf("G's first token: %v", err)
	}
	f.clock.Advance(55 * time.Minute)
	phase.Store(1)
	armed.Store(true)
	statusBefore, _ := f.pubStatus(g.UID)

	// One: G's attempt passes the load with no permit held, and its record read blocks.
	reads := f.sec.callsOf("get")
	started := time.Now()
	gRes := pubGo(func() (serviceGrantMinted, error) { return f.pubTokenFor(g, serviceGrantTriggerResolve) })
	pubUntil(t, pubWait, "G's record read", func() bool { return f.sec.callsOf("get") == reads+1 })
	// Two: H takes the one permit and its request waits at the provider.
	hRes := pubGo(func() (serviceGrantMinted, error) { return f.pubTokenFor(h, serviceGrantTriggerResolve) })
	pubUntil(t, pubWait, "H's request in flight", func() bool { return f.idp.inFlight() == 1 })
	// Three: a rotation committed on another replica during G's read.
	f.sec.set(gName, pubRecordFor(g, svcSecretTwo))
	// Four: G's read returns inside its try and meets the cap at the reservation.
	if elapsed := time.Since(started); elapsed >= 900*time.Millisecond {
		t.Fatalf("the setup took %v, want G's read to return well inside its 1200 ms try", elapsed)
	}
	gGate.release()
	gr := pubRecv(t, gRes, "grant G")
	se := pubWantUnavailable(t, gr.err)
	if !se.refused || !se.afterRead || gr.tok.token != "" || gr.tok.token == old.token {
		t.Fatalf("G answered %q with %+v, want a refusal after the read and no token", gr.tok.token, se)
	}
	var warns int
	for _, r := range logs.withMsg(slog.LevelWarn, "permits are all held") {
		if r.str("provider_uid") == g.UID {
			warns++
		}
	}
	if warns != 1 {
		t.Fatalf("%d WARN lines for G's refusal, want 1", warns)
	}
	if f.pubFailed(g.UID) {
		t.Fatal("G has a failed entry after a refusal")
	}
	if row, ok := f.pubStatus(g.UID); !ok || !row.AttemptedAt.Equal(statusBefore.AttemptedAt) || row.Outcome != statusBefore.Outcome {
		t.Fatalf("G's status row is %+v after the refusal, want it unchanged: %+v", row, statusBefore)
	}
	if n := f.pubRequestsTo("late-g"); n != 1 {
		t.Fatalf("%d requests for G, want only its first token's", n)
	}
	// Five: H's request fails and its permit returns.
	releaseBarrier()
	if r := pubRecv(t, hRes, "grant H"); svcErr(t, r.err).reason != "invalid_client" {
		t.Fatalf("H answered %v, want the provider's invalid_client", r.err)
	}
	pubUntil(t, pubWait, "the permit to return", func() bool { return f.srv.svcGrant.permits.Load() == 0 })
}

// A check of the sink for absence waits for a copy the worker holds before its call to the sink. One publication
// commits its row and its copy waits at siem.emit: the sink holds nothing yet, so an absence check read now would
// pass wrongly. A drain of the sink does not return while that copy is held, and returns once it is released, with
// the sink holding the copy once and before the marker.
// Plant: make drainSink return once its marker is queued rather than once the sink holds it.
func TestServiceGrant_SinkChecksWaitForHeldCopies(t *testing.T) {
	f := newSvcFixWithSecret(t)
	h := pubInstallHook(f)
	ids := f.captureRowIDs()
	emit := newPubGate(t)
	h.on("siem.emit", emit.holdFirst)
	f.mustToken(serviceGrantTriggerRefresh)
	row := ids()[0]
	svcAwait(t, emit.reached, "the worker to reach siem.emit")
	pubUntil(t, pubWait, "the worker to hold the copy", func() bool { return pubQueueOf(f.srv).sending == row })
	if n := f.sink.copiesOf(row); n != 0 {
		t.Fatalf("the sink holds %d copies of the held row, want none yet", n)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	drained := make(chan error, 1)
	go func() { drained <- f.drainSink(ctx) }()
	select {
	case err := <-drained:
		t.Fatalf("the drain returned %v while a copy was held before the sink", err)
	case <-time.After(300 * time.Millisecond):
	}
	emit.release()
	select {
	case err := <-drained:
		if err != nil {
			t.Fatalf("the drain after the release: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("the drain did not return within a second of the release")
	}
	got := f.sink.copies()
	if len(got) != 2 || got[0].ID != row || got[1].Action != svcMarkerAction {
		t.Fatalf("the sink holds %+v, want the held row's copy and then the marker", got)
	}
	if n := f.sink.copiesOf(row); n != 1 {
		t.Fatalf("the sink holds %d copies of the row, want 1", n)
	}
}
