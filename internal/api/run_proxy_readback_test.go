// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
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

	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// memProxyStarts is store.RunProxyStarts in memory, with the PG semantics: one clock for start and
// renewal times, the active start the newest not abandoned, and every write fenced on it.
type memProxyStarts struct {
	pmu    sync.Mutex
	clock  time.Time
	starts map[uuid.UUID]map[int]*store.ProxyStart
	addrs  map[uuid.UUID]string
	// lineage is the run's token lineage as agent_runs holds it: current jti, previous jti, renewed at.
	lineage func(uuid.UUID) (string, string, time.Time)
	// failReports is how many more ReportProxyStart calls fail, as a database blip would.
	failReports int
}

func newMemProxyStarts() *memProxyStarts {
	return &memProxyStarts{clock: time.Now().UTC(),
		starts: map[uuid.UUID]map[int]*store.ProxyStart{}, addrs: map[uuid.UUID]string{}}
}

var _ store.RunProxyStarts = (*memProxyStarts)(nil)

// tick is the database's now: each read moves it on a millisecond.
func (m *memProxyStarts) tick() time.Time {
	m.pmu.Lock()
	defer m.pmu.Unlock()
	m.clock = m.clock.Add(time.Millisecond)
	return m.clock
}

func (m *memProxyStarts) advance(d time.Duration) {
	m.pmu.Lock()
	m.clock = m.clock.Add(d)
	m.pmu.Unlock()
}

func (m *memProxyStarts) activeLocked(runID uuid.UUID) *store.ProxyStart {
	var out *store.ProxyStart
	for _, p := range m.starts[runID] {
		if p.State != types.ProxyStartAbandoned && (out == nil || p.Generation > out.Generation) {
			out = p
		}
	}
	return out
}

func (m *memProxyStarts) get(runID uuid.UUID, gen int) store.ProxyStart {
	m.pmu.Lock()
	defer m.pmu.Unlock()
	if p := m.starts[runID][gen]; p != nil {
		return *p
	}
	return store.ProxyStart{}
}

func memSettle(ch *store.ProxyStartChange, now time.Time) {
	state, reason := store.SettleProxyStart(ch.Start, now)
	if state == ch.Start.State {
		return
	}
	ch.Start.State, ch.Settled, ch.Reason = state, state, reason
	if state == types.ProxyStartEnforced {
		ch.Start.ConfirmedAt = &now
	}
}

func (m *memProxyStarts) BeginProxyStart(_ context.Context, runID uuid.UUID, gen int, jti, mode string) (store.ProxyStart, error) {
	now := m.tick()
	m.pmu.Lock()
	defer m.pmu.Unlock()
	if m.starts[runID] == nil {
		m.starts[runID] = map[int]*store.ProxyStart{}
	}
	p := &store.ProxyStart{RunID: runID, Generation: gen, JTI: jti, Mode: mode, StartedAt: now, State: types.ProxyStartPending}
	m.starts[runID][gen] = p
	return *p, nil
}

func (m *memProxyStarts) AnswerProxyStart(_ context.Context, runID uuid.UUID, gen int, sha string, grace time.Duration) (store.ProxyStartChange, error) {
	now := m.tick()
	m.pmu.Lock()
	defer m.pmu.Unlock()
	var ch store.ProxyStartChange
	a := m.activeLocked(runID)
	if a == nil {
		return ch, nil
	}
	ch.Start = *a
	if a.Generation != gen || a.State != types.ProxyStartPending {
		return ch, nil
	}
	ch.Active = true
	switch {
	case a.Mode == store.ProxyStartLegacy:
		ch.Start.State, ch.Settled = types.ProxyStartUnconfirmed, types.ProxyStartUnconfirmed
	case sha == "":
		ch.Start.State, ch.Settled, ch.Reason = types.ProxyStartFailed, types.ProxyStartFailed, store.ProxyStartHashMissing
	default:
		d := now.Add(grace)
		ch.Start.ExpectedSHA256, ch.Start.Deadline = sha, &d
		memSettle(&ch, now)
	}
	*a = ch.Start
	for g, p := range m.starts[runID] {
		if g < gen && p.State == types.ProxyStartPending {
			p.State = types.ProxyStartSuperseded
		}
	}
	return ch, nil
}

func (m *memProxyStarts) ReportProxyStart(_ context.Context, runID uuid.UUID, jti, sha string) (store.ProxyStartChange, error) {
	now := m.tick()
	cur, prev, renewedAt := "", "", time.Time{}
	if m.lineage != nil {
		cur, prev, renewedAt = m.lineage(runID)
	}
	m.pmu.Lock()
	defer m.pmu.Unlock()
	var ch store.ProxyStartChange
	if m.failReports > 0 {
		m.failReports--
		return ch, errors.New("report proxy start: connection reset")
	}
	a := m.activeLocked(runID)
	if a == nil {
		return ch, nil
	}
	ch.Start = *a
	in := jti == a.JTI || (!renewedAt.Before(a.StartedAt) && (jti == cur || jti == prev))
	if !in || jti == "" {
		return ch, nil
	}
	ch.Active = true
	if a.State != types.ProxyStartPending {
		return ch, nil
	}
	ch.Start.ReportedSHA256 = sha
	memSettle(&ch, now)
	*a = ch.Start
	return ch, nil
}

func (m *memProxyStarts) AbandonProxyStart(_ context.Context, runID uuid.UUID, gen int) error {
	m.pmu.Lock()
	defer m.pmu.Unlock()
	if p := m.starts[runID][gen]; p != nil && p.State == types.ProxyStartPending {
		p.State = types.ProxyStartAbandoned
	}
	return nil
}

func (m *memProxyStarts) ExpireProxyStart(_ context.Context, runID uuid.UUID, gen int) (store.ProxyStartChange, error) {
	now := m.tick()
	m.pmu.Lock()
	defer m.pmu.Unlock()
	var ch store.ProxyStartChange
	a := m.activeLocked(runID)
	if a == nil {
		return ch, nil
	}
	ch.Start = *a
	if a.Generation != gen || a.State != types.ProxyStartPending || a.Deadline == nil || now.Before(*a.Deadline) {
		return ch, nil
	}
	ch.Active = true
	memSettle(&ch, now)
	*a = ch.Start
	return ch, nil
}

func (m *memProxyStarts) OverdueProxyStarts(context.Context) ([]store.ProxyStart, error) {
	now := m.tick()
	m.pmu.Lock()
	defer m.pmu.Unlock()
	var out []store.ProxyStart
	for _, gens := range m.starts {
		for _, p := range gens {
			if p.State == types.ProxyStartPending && p.Deadline != nil && !now.Before(*p.Deadline) {
				out = append(out, *p)
			}
		}
	}
	return out, nil
}

func (m *memProxyStarts) ActiveProxyStart(_ context.Context, runID uuid.UUID) (store.ProxyStart, bool, error) {
	m.pmu.Lock()
	defer m.pmu.Unlock()
	if a := m.activeLocked(runID); a != nil {
		return *a, true, nil
	}
	return store.ProxyStart{}, false, nil
}

func (m *memProxyStarts) SetRunProxyAddr(_ context.Context, runID uuid.UUID, addr string) error {
	m.pmu.Lock()
	defer m.pmu.Unlock()
	m.addrs[runID] = addr
	return nil
}

// postLoaded posts a proxy report as the proxy whose run token carries jti, past internalAuth.
func postLoaded(t *testing.T, srv *Server, runID uuid.UUID, jti, sha string) (int, string) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"config_sha256": sha, "proxy_version": "0.10.0-rc.18",
		"switches": map[string]bool{"llm_scan": true}})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/internal/proxy/loaded", strings.NewReader(string(body)))
	req = req.WithContext(contextWithClaims(req.Context(), &identity.Claims{RunID: runID, JTI: jti}))
	w := httptest.NewRecorder()
	srv.handleInternalProxyLoaded(w, req)
	var out struct {
		State string `json:"state"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out.State
}

func hashHex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// ---- revive -----------------------------------------------------------------------------------

// readbackStore is generationStore with the run's proxy starts, its renewal stamped on the same clock.
type readbackStore struct {
	*generationStore
	*memProxyStarts
	renewedAt time.Time
}

func (s *readbackStore) StampRunTokenRenewed(ctx context.Context, id uuid.UUID, jti, prev string) (bool, error) {
	at := s.memProxyStarts.tick()
	ok, err := s.generationStore.StampRunTokenRenewed(ctx, id, jti, prev)
	if ok {
		s.generationStore.mu.Lock()
		s.renewedAt = at
		s.generationStore.mu.Unlock()
	}
	return ok, err
}

// MarkRunRevived stamps token_renewed_at, as the PG claim does.
func (s *readbackStore) MarkRunRevived(ctx context.Context, id uuid.UUID, from types.LostReason, ended *store.EndedKept, limit int, startsAgent bool) (bool, error) {
	at := s.memProxyStarts.tick()
	ok, err := s.generationStore.MarkRunRevived(ctx, id, from, ended, limit, startsAgent)
	if ok {
		s.generationStore.mu.Lock()
		s.renewedAt = at
		s.generationStore.mu.Unlock()
	}
	return ok, err
}

// hashedReviveRunner is reviveRunner answering the hash of the bytes each new proxy was handed;
// hash overrides it, as a driver that sends none ("") or another would.
type hashedReviveRunner struct {
	*reviveRunner
	hash func(cfg []byte) string
}

func (r *hashedReviveRunner) ReplaceProxyHashed(ctx context.Context, ref string, cfg []byte) (string, error) {
	if err := r.reviveRunner.ReplaceProxy(ctx, ref, cfg); err != nil {
		return "", err
	}
	if r.hash != nil {
		return r.hash(cfg), nil
	}
	return hashHex(cfg), nil
}

type readbackFixture struct {
	*reviveFixture
	rb *readbackStore
	hr *hashedReviveRunner
}

// newReadbackFixture is newGenerationFixture whose store keeps proxy starts and whose runner answers
// the new proxy's hash. Deadline watchdogs are run by the test, never on a timer.
func newReadbackFixture(t *testing.T) *readbackFixture {
	t.Helper()
	f, gs, _ := newGenerationFixture(t)
	rb := &readbackStore{generationStore: gs, memProxyStarts: newMemProxyStarts()}
	rb.lineage = func(uuid.UUID) (string, string, time.Time) {
		gs.mu.Lock()
		defer gs.mu.Unlock()
		j := append(append([]string(nil), gs.jtis...), "", "")
		return j[0], j[1], rb.renewedAt
	}
	f.srv.cfg.Store = rb
	hr := &hashedReviveRunner{reviveRunner: f.rr}
	f.srv.cfg.Runner = hr
	old := proxyStartWatch
	proxyStartWatch = func(time.Duration, func()) {}
	t.Cleanup(func() { proxyStartWatch = old })
	return &readbackFixture{reviveFixture: f, rb: rb, hr: hr}
}

// lastHandover is the newest proxy config the runner was handed, its hash, and its token's jti.
func (f *readbackFixture) lastHandover(t *testing.T) ([]byte, string, string) {
	t.Helper()
	if len(f.rr.replaced) == 0 {
		t.Fatal("no proxy was replaced")
	}
	raw := f.rr.replaced[len(f.rr.replaced)-1]
	var c struct {
		RunToken string `json:"run_token"`
	}
	if err := json.Unmarshal(raw, &c); err != nil || c.RunToken == "" {
		t.Fatalf("the handed-over config has no run token: %v", err)
	}
	claims, err := f.srv.cfg.Identity.Verify(context.Background(), c.RunToken, internalAudience)
	if err != nil {
		t.Fatalf("the new proxy's token does not verify: %v", err)
	}
	return raw, hashHex(raw), claims.JTI
}

func (f *readbackFixture) active(t *testing.T) store.ProxyStart {
	t.Helper()
	p, ok, _ := f.rb.ActiveProxyStart(context.Background(), f.run.ID)
	if !ok {
		t.Fatal("the run has no active proxy start")
	}
	return p
}

// A report that arrives while ReplaceProxy is still running is held (202, pending); the matching
// hash the driver then answers enforces the start, and the run read shows state, generation and
// confirmed_at, never a hash.
func TestReadback_ReportBeforeReviveReturnsIsHeldThenEnforced(t *testing.T) {
	f := newReadbackFixture(t)
	f.rr.onReplace = func(cfg []byte) {
		var c struct {
			RunToken string `json:"run_token"`
		}
		_ = json.Unmarshal(cfg, &c)
		claims, err := f.srv.cfg.Identity.Verify(context.Background(), c.RunToken, internalAudience)
		if err != nil {
			t.Errorf("verify the new token: %v", err)
			return
		}
		if code, state := postLoaded(t, f.srv, f.run.ID, claims.JTI, hashHex(cfg)); code != http.StatusAccepted || state != "pending" {
			t.Errorf("early report: %d %q, want 202 pending", code, state)
		}
	}
	if code := f.revive(t); code != http.StatusOK {
		t.Fatalf("revive: code %d, want 200", code)
	}
	p := f.active(t)
	if p.State != types.ProxyStartEnforced || p.ConfirmedAt == nil {
		t.Fatalf("start = %s (confirmed %v), want enforced", p.State, p.ConfirmedAt)
	}
	if len(f.audit.eventsFor(f.run.ID, "run.policy.enforced")) != 1 {
		t.Error("want one run.policy.enforced row")
	}
	rb := f.srv.runPolicyReadback(context.Background(), f.run.ID)
	b, _ := json.Marshal(rb)
	if rb == nil || rb.State != types.ProxyStartEnforced || rb.Generation != p.Generation || strings.Contains(string(b), "sha") {
		t.Errorf("run read policy_readback = %s, want enforced at generation %d with no hash", b, p.Generation)
	}
}

// An early report of different bytes, then the driver's hash: the new proxy is stopped and the run is
// lost again.
func TestReadback_ReportBeforeReviveReturnsDifferentHashStops(t *testing.T) {
	f := newReadbackFixture(t)
	f.rr.onReplace = func(cfg []byte) {
		var c struct {
			RunToken string `json:"run_token"`
		}
		_ = json.Unmarshal(cfg, &c)
		claims, _ := f.srv.cfg.Identity.Verify(context.Background(), c.RunToken, internalAudience)
		postLoaded(t, f.srv, f.run.ID, claims.JTI, hashHex([]byte("other bytes")))
	}
	if code := f.revive(t); code != http.StatusBadGateway {
		t.Fatalf("revive: code %d, want 502", code)
	}
	if p := f.active(t); p.State != types.ProxyStartFailed {
		t.Fatalf("start = %s, want failed", p.State)
	}
	if lostAt, _ := f.st.lost(); lostAt == nil {
		t.Error("the run must be lost again, its new proxy stopped")
	}
}

// A driver that answers without a hash stops the new proxy at once (hash_missing), never legacy, and
// with no grace.
func TestReadback_ReviveWithoutHashStopsAtOnce(t *testing.T) {
	f := newReadbackFixture(t)
	f.hr.hash = func([]byte) string { return "" }
	if code := f.revive(t); code != http.StatusBadGateway {
		t.Fatalf("revive: code %d, want 502", code)
	}
	p := f.active(t)
	if p.State != types.ProxyStartFailed || p.Mode != store.ProxyStartExpected {
		t.Fatalf("start = %s/%s, want failed/expected", p.State, p.Mode)
	}
	rows := f.audit.eventsFor(f.run.ID, "run.policy.unconfirmed")
	if len(rows) != 1 || !strings.Contains(string(rows[0].Data), store.ProxyStartHashMissing) {
		t.Fatalf("want one run.policy.unconfirmed row with hash_missing, got %d", len(rows))
	}
	if lostAt, _ := f.st.lost(); lostAt == nil {
		t.Error("the run must be lost again at once")
	}
}

// No report by the driver's answer plus the grace: the deadline fails the start and stops the proxy;
// a late report then confirms nothing.
func TestReadback_NoReportByTheDeadlineStopsTheProxy(t *testing.T) {
	f := newReadbackFixture(t)
	if code := f.revive(t); code != http.StatusOK {
		t.Fatalf("revive: code %d, want 200", code)
	}
	_, sha, jti := f.lastHandover(t)
	p := f.active(t)
	if p.State != types.ProxyStartPending || p.Deadline == nil {
		t.Fatalf("start = %s (deadline %v), want pending with a deadline", p.State, p.Deadline)
	}
	f.srv.expireProxyStart(context.Background(), f.run.ID, p.Generation)
	if got := f.active(t).State; got != types.ProxyStartPending {
		t.Fatalf("before the deadline the start is %s, want pending", got)
	}
	f.rb.advance(proxyReportGrace)
	stops := f.lr.proxyStopCount()
	if err := f.srv.sweepOverdueProxyStarts(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := f.active(t).State; got != types.ProxyStartFailed {
		t.Fatalf("after the deadline the start is %s, want failed", got)
	}
	if f.lr.proxyStopCount() <= stops {
		t.Error("the deadline must stop the run's proxy")
	}
	if code, state := postLoaded(t, f.srv, f.run.ID, jti, sha); code != http.StatusOK || state != string(types.ProxyStartFailed) {
		t.Errorf("late report: %d %q, want 200 failed", code, state)
	}
	if len(f.audit.eventsFor(f.run.ID, "run.policy.enforced")) != 0 {
		t.Error("a late report must confirm nothing")
	}
}

// The first report is lost (the store fails it, 503); the proxy's retry confirms.
func TestReadback_LostFirstReportThenRetryConfirms(t *testing.T) {
	f := newReadbackFixture(t)
	if code := f.revive(t); code != http.StatusOK {
		t.Fatalf("revive: code %d, want 200", code)
	}
	_, sha, jti := f.lastHandover(t)
	f.rb.failReports = 1
	if code, _ := postLoaded(t, f.srv, f.run.ID, jti, sha); code != http.StatusServiceUnavailable {
		t.Fatalf("first report: %d, want 503", code)
	}
	if code, state := postLoaded(t, f.srv, f.run.ID, jti, sha); code != http.StatusOK || state != string(types.ProxyStartEnforced) {
		t.Fatalf("retry: %d %q, want 200 enforced", code, state)
	}
}

// A token renewed after the start is in its lineage: J1 (renewed from the start's J0) reports after
// J2 was stamped but never delivered, which leaves J1 the run's previous jti. J2 itself is accepted
// too, and J0 always is.
func TestReadback_RenewedTokenLineageIsAccepted(t *testing.T) {
	for _, reporter := range []string{"J1", "J2", "J0"} {
		t.Run(reporter, func(t *testing.T) {
			f := newReadbackFixture(t)
			if code := f.revive(t); code != http.StatusOK {
				t.Fatalf("revive: code %d, want 200", code)
			}
			_, sha, j0 := f.lastHandover(t)
			ctx := context.Background()
			if ok, err := f.rb.StampRunTokenRenewed(ctx, f.run.ID, "J1", j0); !ok || err != nil {
				t.Fatalf("renew J0->J1: %v %v", ok, err)
			}
			if ok, err := f.rb.StampRunTokenRenewed(ctx, f.run.ID, "J2", "J1"); !ok || err != nil {
				t.Fatalf("renew J1->J2: %v %v", ok, err)
			}
			jti := map[string]string{"J0": j0, "J1": "J1", "J2": "J2"}[reporter]
			if code, state := postLoaded(t, f.srv, f.run.ID, jti, sha); code != http.StatusOK || state != string(types.ProxyStartEnforced) {
				t.Fatalf("report on %s: %d %q, want 200 enforced", reporter, code, state)
			}
		})
	}
}

// The reporter posts with whichever run token is current: a first post on J0 is lost (503), the proxy
// renews J0 to J1, and the retry on J1, now the run's current token, confirms.
func TestReadback_RenewalBetweenReportRetriesIsAccepted(t *testing.T) {
	f := newReadbackFixture(t)
	if code := f.revive(t); code != http.StatusOK {
		t.Fatalf("revive: code %d, want 200", code)
	}
	_, sha, j0 := f.lastHandover(t)
	f.rb.failReports = 1
	if code, _ := postLoaded(t, f.srv, f.run.ID, j0, sha); code != http.StatusServiceUnavailable {
		t.Fatalf("first report: %d, want 503", code)
	}
	if ok, err := f.rb.StampRunTokenRenewed(context.Background(), f.run.ID, "J1", j0); !ok || err != nil {
		t.Fatalf("renew J0->J1: %v %v", ok, err)
	}
	if code, state := postLoaded(t, f.srv, f.run.ID, "J1", sha); code != http.StatusOK || state != string(types.ProxyStartEnforced) {
		t.Fatalf("retry on J1: %d %q, want 200 enforced", code, state)
	}
}

// A live run's revive on a substrate whose ReplaceProxyHashed refuses (runner.ErrReviveUnsupported, as
// the orchestrator answers for a substrate that cannot hash) is 409 revive_readback_unsupported, and
// the run keeps its proxy; its start is abandoned.
func TestReadback_SubstrateRefusingTheHashKeepsTheProxy(t *testing.T) {
	f := newReadbackFixture(t)
	if code := f.revive(t); code != http.StatusOK {
		t.Fatalf("first revive: code %d, want 200", code)
	}
	kept := f.active(t).Generation
	f.srv.liveRevived.Delete(f.run.ID)
	f.rr.replaceErr = runner.ErrReviveUnsupported
	w := do(t, f.srv, http.MethodPost, "/api/v1/runs/"+f.run.ID.String()+"/revive", adminToken, "")
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), reasonReviveReadbackUnsupported) {
		t.Fatalf("revive: %d %s, want 409 %s", w.Code, w.Body.String(), reasonReviveReadbackUnsupported)
	}
	if got := f.rb.get(f.run.ID, kept+1).State; got != types.ProxyStartAbandoned {
		t.Errorf("the refused revive's start is %s, want abandoned", got)
	}
	if lostAt, _ := f.st.lost(); lostAt != nil {
		t.Error("a refused revive must leave the live run as it was")
	}
}

// Two revives in a row: a report on the first new proxy's token, sent while the second revive replaces
// it (after its lineage is retired and its pending start published, before that token is revoked), is
// stale and audited; only the second start confirms.
func TestReadback_TwoRevivesOnlyTheLastConfirms(t *testing.T) {
	f := newReadbackFixture(t)
	if code := f.revive(t); code != http.StatusOK {
		t.Fatalf("first revive: code %d, want 200", code)
	}
	_, sha1, jti1 := f.lastHandover(t)
	gen1 := f.active(t).Generation
	f.srv.liveRevived.Delete(f.run.ID) // the live-revive bound is not what this test is about
	f.rr.onReplace = func([]byte) {
		if code, state := postLoaded(t, f.srv, f.run.ID, jti1, sha1); code != http.StatusOK || state != "stale" {
			t.Errorf("the first proxy's report during the second revive: %d %q, want 200 stale", code, state)
		}
	}
	if code := f.revive(t); code != http.StatusOK {
		t.Fatalf("second revive: code %d, want 200", code)
	}
	f.rr.onReplace = nil
	if got := f.rb.get(f.run.ID, gen1).State; got != types.ProxyStartSuperseded {
		t.Errorf("the first start is %s, want superseded", got)
	}
	if len(f.audit.eventsFor(f.run.ID, "run.policy.report_stale")) != 1 {
		t.Error("want one run.policy.report_stale row")
	}
	_, sha2, jti2 := f.lastHandover(t)
	if code, state := postLoaded(t, f.srv, f.run.ID, jti2, sha2); code != http.StatusOK || state != string(types.ProxyStartEnforced) {
		t.Fatalf("second proxy's report: %d %q, want 200 enforced", code, state)
	}
}

// The previous start's current token, renewed before the revive, is no longer in any active lineage
// once the revive retired it: it never confirms the new start.
func TestReadback_OldCurrentTokenAfterLineageRetirementIsStale(t *testing.T) {
	f := newReadbackFixture(t)
	at := f.rb.tick() // the old proxy renewed to old-current before the revive
	f.rb.generationStore.mu.Lock()
	f.rb.jtis, f.rb.renewedAt = []string{"old-current", ""}, at
	f.rb.generationStore.mu.Unlock()
	f.rr.onReplace = func(cfg []byte) {
		if code, state := postLoaded(t, f.srv, f.run.ID, "old-current", hashHex(cfg)); code != http.StatusOK || state != "stale" {
			t.Errorf("old-current report: %d %q, want 200 stale", code, state)
		}
	}
	if code := f.revive(t); code != http.StatusOK {
		t.Fatalf("revive: code %d, want 200", code)
	}
	if got := f.active(t).State; got != types.ProxyStartPending {
		t.Errorf("the new start is %s, want pending: the old token must not have confirmed it", got)
	}
}

// A revive of a live run whose replace fails keeps the old proxy: its pending start is abandoned before
// the old lineage is restored, so a report on that restored lineage confirms the old start, never the
// abandoned one.
func TestReadback_FailedReviveRestoringOldLineageNeverConfirmsItsStart(t *testing.T) {
	f := newReadbackFixture(t)
	if code := f.revive(t); code != http.StatusOK {
		t.Fatalf("first revive: code %d, want 200", code)
	}
	_, sha, jti := f.lastHandover(t)
	kept := f.active(t).Generation
	f.srv.liveRevived.Delete(f.run.ID)
	f.rr.replaceErr = errors.New("docker: inspect failed")
	if code := f.revive(t); code != http.StatusBadGateway {
		t.Fatalf("failing revive: code %d, want 502", code)
	}
	if got := f.rb.get(f.run.ID, kept+1).State; got != types.ProxyStartAbandoned {
		t.Fatalf("the failed revive's start is %s, want abandoned", got)
	}
	if code, state := postLoaded(t, f.srv, f.run.ID, jti, sha); code != http.StatusOK || state != string(types.ProxyStartEnforced) {
		t.Fatalf("the kept proxy's report: %d %q, want 200 enforced", code, state)
	}
	if p := f.active(t); p.Generation != kept {
		t.Errorf("the confirmed start is generation %d, want the kept proxy's %d", p.Generation, kept)
	}
	if got := f.rb.get(f.run.ID, kept+1).State; got != types.ProxyStartAbandoned {
		t.Errorf("the abandoned start became %s", got)
	}
}

// A policy whose inspection corpus is restored for the proxy confirms: the hash is of the bytes handed
// over, never of the redacted audit copy.
func TestReadback_WorkspaceSecretValuesConfirm(t *testing.T) {
	f := newReadbackFixture(t)
	cfg, err := runner.BuildProxyConfig(f.run.ID, runner.ProxyConfig{
		RunToken:        "old-token",
		ControlPlaneURL: "http://127.0.0.1:8081",
		Policy: types.RunPolicySpec{AllowedDomains: []string{"api.anthropic.com"},
			LLMInspection: &types.LLMInspectionSpec{WorkspaceSecretValues: []string{"s3cret-value"}}},
		MITMCACertPEM: "ca-cert",
		MITMCAKeyPEM:  "ca-key",
	}, runner.ProxyListenPort)
	if err != nil {
		t.Fatal(err)
	}
	f.rs.cfg = cfg
	if code := f.revive(t); code != http.StatusOK {
		t.Fatalf("revive: code %d, want 200", code)
	}
	_, sha, jti := f.lastHandover(t)
	if code, state := postLoaded(t, f.srv, f.run.ID, jti, sha); code != http.StatusOK || state != string(types.ProxyStartEnforced) {
		t.Fatalf("report: %d %q, want 200 enforced", code, state)
	}
}

// A substrate that cannot answer the hash refuses an expected start's revive before any change (409);
// a positively legacy proxy image revives as before, unconfirmed.
func TestReadback_UnhashedSubstrateRefusedUnlessLegacy(t *testing.T) {
	f := newReadbackFixture(t)
	f.srv.cfg.Runner = f.rr // no ReplaceProxyHashed
	w := do(t, f.srv, http.MethodPost, "/api/v1/runs/"+f.run.ID.String()+"/revive", adminToken, "")
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), reasonReviveReadbackUnsupported) {
		t.Fatalf("revive: %d %s, want 409 %s", w.Code, w.Body.String(), reasonReviveReadbackUnsupported)
	}
	if len(f.rr.replaced) != 0 {
		t.Fatal("nothing may change before the refusal")
	}
	f.srv.cfg.ProxyImage = "ghcr.io/example/wardyn-proxy:0.10.0-rc.17"
	if code := f.revive(t); code != http.StatusOK {
		t.Fatalf("legacy revive: code %d, want 200", code)
	}
	p := f.active(t)
	if p.Mode != store.ProxyStartLegacy || p.State != types.ProxyStartUnconfirmed {
		t.Fatalf("start = %s/%s, want legacy/unconfirmed", p.Mode, p.State)
	}
	rows := f.audit.eventsFor(f.run.ID, "run.policy.unconfirmed")
	if len(rows) != 1 || rows[0].Outcome != "success" || !strings.Contains(string(rows[0].Data), `"legacy"`) {
		t.Fatalf("want one run.policy.unconfirmed success row, reason legacy")
	}
}

// ---- mode -------------------------------------------------------------------------------------

type versionRunnerStore struct {
	store.Store
	version string
}

func (s versionRunnerStore) GetRunner(context.Context, uuid.UUID) (types.Runner, error) {
	return types.Runner{Version: s.version}, nil
}

// Only a parsed, older release is legacy evidence: an empty, unparsable, digest-only or readback-release
// version is expected.
func TestReadback_ModeNeedsPositiveEvidence(t *testing.T) {
	rid := uuid.New()
	cases := []struct {
		version, image, want string
	}{
		{"0.10.0-rc.17", "", store.ProxyStartLegacy},
		{"v0.9.8", "", store.ProxyStartLegacy},
		{"0.10.0-rc.18", "", store.ProxyStartExpected},
		{"0.10.0", "", store.ProxyStartExpected},
		{"", "", store.ProxyStartExpected},
		{"dev", "", store.ProxyStartExpected},
		{"sha256:abcd", "", store.ProxyStartExpected},
	}
	for _, c := range cases {
		srv := &Server{cfg: Config{Store: versionRunnerStore{version: c.version}}}
		run := types.AgentRun{ID: uuid.New(), RunnerID: &rid}
		if got := srv.proxyStartMode(context.Background(), run); got != c.want {
			t.Errorf("runner version %q: mode %s, want %s", c.version, got, c.want)
		}
	}
	for image, want := range map[string]string{
		"ghcr.io/example/wardyn-proxy:0.10.0-rc.17":           store.ProxyStartLegacy,
		"ghcr.io/example/wardyn-proxy:0.10.0-rc.18":           store.ProxyStartExpected,
		"ghcr.io/example/wardyn-proxy@sha256:" + hashHex(nil): store.ProxyStartExpected,
		"ghcr.io/example/wardyn-proxy:latest":                 store.ProxyStartExpected,
		"":                                                    store.ProxyStartExpected,
	} {
		srv := &Server{cfg: Config{ProxyImage: image}}
		if got := srv.proxyStartMode(context.Background(), types.AgentRun{ID: uuid.New()}); got != want {
			t.Errorf("in-process proxy image %q: mode %s, want %s", image, got, want)
		}
	}
}

// ---- dispatch ---------------------------------------------------------------------------------

// readbackDispatchStore is dispatchTestStore with proxy starts; its run has no token renewals.
type readbackDispatchStore struct {
	*dispatchTestStore
	*memProxyStarts
}

// readbackCreateRunner is fakeRunner whose create blocks until release, after its proxy would have
// started, and answers hash and addr.
type readbackCreateRunner struct {
	*killRaceRunner
	started chan struct{}
	release chan struct{}
	hash    string
	addr    string
}

func (r *readbackCreateRunner) CreateSandbox(ctx context.Context, spec runner.SandboxSpec) (runner.Sandbox, error) {
	if r.started != nil {
		close(r.started)
		<-r.release
	}
	sb, err := r.killRaceRunner.CreateSandbox(ctx, spec)
	sb.ProxyConfigSHA256, sb.ProxyAddr = r.hash, r.addr
	return sb, err
}

type dispatchReadback struct {
	srv   *Server
	st    *readbackDispatchStore
	rn    *readbackCreateRunner
	run   types.AgentRun
	token string
	jti   string
	audit *recRecorder
}

func newDispatchReadback(t *testing.T, hash string, held bool) *dispatchReadback {
	t.Helper()
	rn := &readbackCreateRunner{killRaceRunner: &killRaceRunner{fakeRunner: &fakeRunner{}}, hash: hash}
	if held {
		rn.started, rn.release = make(chan struct{}), make(chan struct{})
	}
	srv, st, audit, run := dispatchTeardownFixture(t, rn, types.RunPending)
	rst := &readbackDispatchStore{dispatchTestStore: st, memProxyStarts: newMemProxyStarts()}
	srv.cfg.Store = rst
	id, err := srv.cfg.Identity.MintRunIdentity(context.Background(), run.ID, run.CreatedBy, run.CreatedBy, internalAudience, false)
	if err != nil {
		t.Fatal(err)
	}
	old := proxyStartWatch
	proxyStartWatch = func(time.Duration, func()) {}
	t.Cleanup(func() { proxyStartWatch = old })
	return &dispatchReadback{srv: srv, st: rst, rn: rn, run: run, token: id.Token, jti: id.JTI, audit: audit}
}

func (d *dispatchReadback) dispatch() {
	d.srv.dispatchRun(context.Background(), d.run, ceilingForDispatch(governanceCeiling{}, adoEntraUngraded(), bedrockCredUngraded()), dispatchParams{
		RunToken: d.token, Image: "wardyn/claude-code:latest",
		Policy: types.RunPolicySpec{MinConfinementClass: types.CC1},
	})
}

// A create that answers success without a hash stops at once: the sandbox is stopped and the run fails
// (hash_missing), with no grace.
func TestReadback_DispatchWithoutHashStopsAtOnce(t *testing.T) {
	d := newDispatchReadback(t, "", false)
	d.dispatch()
	if got := d.st.State(); got != types.RunFailed {
		t.Fatalf("run state = %s, want FAILED", got)
	}
	if d.rn.stopCount() != 1 {
		t.Errorf("sandbox stops = %d, want 1", d.rn.stopCount())
	}
	if p := d.st.get(d.run.ID, 1); p.State != types.ProxyStartFailed || p.Mode != store.ProxyStartExpected {
		t.Errorf("start = %s/%s, want failed/expected", p.State, p.Mode)
	}
	ev := findAudit(d.audit.snapshot(), d.run.ID, "run.create", "failure")
	if ev == nil || !strings.Contains(string(ev.Data), "policy_unconfirmed") || !strings.Contains(string(ev.Data), store.ProxyStartHashMissing) {
		t.Fatal("want a run.create failure row, reason policy_unconfirmed, readback hash_missing")
	}
}

// The create is still blocked after its proxy started: the proxy's report is held (202, pending, no
// deadline); the create then answers the matching hash, the start is enforced and the run runs. The
// address it answered is stored; the run's JSON never carries it.
func TestReadback_DispatchReportWhileCreateBlockedThenMatch(t *testing.T) {
	sha := hashHex([]byte("final bytes"))
	d := newDispatchReadback(t, sha, true)
	d.rn.addr = "172.30.0.2"
	done := make(chan struct{})
	go func() { d.dispatch(); close(done) }()
	<-d.rn.started
	if code, state := postLoaded(t, d.srv, d.run.ID, d.jti, sha); code != http.StatusAccepted || state != "pending" {
		t.Fatalf("report during create: %d %q, want 202 pending", code, state)
	}
	if p := d.st.get(d.run.ID, 1); p.Deadline != nil {
		t.Fatal("a create still running has no readback deadline")
	}
	close(d.rn.release)
	<-done
	if p := d.st.get(d.run.ID, 1); p.State != types.ProxyStartEnforced {
		t.Fatalf("start = %s, want enforced", p.State)
	}
	if got := d.st.State(); got == types.RunFailed {
		t.Fatal("a confirmed start must not fail the run")
	}
	if got := d.st.addrs[d.run.ID]; got != "172.30.0.2" {
		t.Errorf("stored proxy address = %q, want 172.30.0.2", got)
	}
	b, _ := json.Marshal(types.AgentRun{ProxyAddr: "172.30.0.2"})
	if strings.Contains(string(b), "172.30.0.2") {
		t.Error("the proxy address must never be on the run's JSON")
	}
}

// The report held during the create is of different bytes: the create's answer stops the sandbox.
func TestReadback_DispatchReportWhileCreateBlockedThenMismatch(t *testing.T) {
	d := newDispatchReadback(t, hashHex([]byte("final bytes")), true)
	done := make(chan struct{})
	go func() { d.dispatch(); close(done) }()
	<-d.rn.started
	postLoaded(t, d.srv, d.run.ID, d.jti, hashHex([]byte("other bytes")))
	close(d.rn.release)
	<-done
	if got := d.st.State(); got != types.RunFailed {
		t.Fatalf("run state = %s, want FAILED", got)
	}
	if p := d.st.get(d.run.ID, 1); p.State != types.ProxyStartFailed {
		t.Errorf("start = %s, want failed", p.State)
	}
}

// A create that answers after its start stopped being the run's active pending one (a newer generation
// was published, or the start already failed) confirms nothing: the sandbox is stopped.
func TestReadback_LateCreateReturnIsFenced(t *testing.T) {
	for name, fence := range map[string]func(d *dispatchReadback){
		"new generation": func(d *dispatchReadback) {
			_, _ = d.st.BeginProxyStart(context.Background(), d.run.ID, 2, "J-next", store.ProxyStartExpected)
		},
		"start failed": func(d *dispatchReadback) {
			d.st.pmu.Lock()
			d.st.starts[d.run.ID][1].State = types.ProxyStartFailed
			d.st.pmu.Unlock()
		},
	} {
		t.Run(name, func(t *testing.T) {
			sha := hashHex([]byte("final bytes"))
			d := newDispatchReadback(t, sha, true)
			done := make(chan struct{})
			go func() { d.dispatch(); close(done) }()
			<-d.rn.started
			fence(d)
			close(d.rn.release)
			<-done
			if got := d.st.State(); got != types.RunFailed {
				t.Fatalf("run state = %s, want FAILED", got)
			}
			if d.rn.stopCount() == 0 {
				t.Error("the late create's sandbox must be stopped")
			}
			if p := d.st.get(d.run.ID, 1); p.State == types.ProxyStartEnforced {
				t.Error("a late create must never confirm its start")
			}
		})
	}
}

// A create answer without an address leaves the run's stored address empty.
func TestReadback_DispatchWithoutAddrStoresNone(t *testing.T) {
	d := newDispatchReadback(t, hashHex([]byte("final bytes")), false)
	d.dispatch()
	if got, ok := d.st.addrs[d.run.ID]; ok || got != "" {
		t.Errorf("stored proxy address = %q, want none", got)
	}
}
