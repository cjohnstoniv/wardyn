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
	"github.com/cjohnstoniv/wardyn/internal/runner/orchestrator"
	"github.com/cjohnstoniv/wardyn/internal/runner/remote"
	"github.com/cjohnstoniv/wardyn/internal/runner/substrate"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
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
	// failReports is how many more ReportProxyStart calls fail, as a database blip would; failAnswers
	// the same for AnswerProxyStart; commitThenFailAnswers how many more apply and then fail, as a
	// lost commit acknowledgement would.
	failReports, failAnswers, commitThenFailAnswers int
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

func (m *memProxyStarts) BeginProxyStart(_ context.Context, runID uuid.UUID, gen int, jti, mode string, inPlace bool) (store.ProxyStart, error) {
	now := m.tick()
	m.pmu.Lock()
	defer m.pmu.Unlock()
	if m.starts[runID] == nil {
		m.starts[runID] = map[int]*store.ProxyStart{}
	}
	p := &store.ProxyStart{RunID: runID, Generation: gen, JTI: jti, Mode: mode, InPlace: inPlace, StartedAt: now, State: types.ProxyStartPending}
	m.starts[runID][gen] = p
	return *p, nil
}

func (m *memProxyStarts) AnswerProxyStart(_ context.Context, runID uuid.UUID, gen int, sha string, grace time.Duration) (store.ProxyStartChange, error) {
	now := m.tick()
	m.pmu.Lock()
	defer m.pmu.Unlock()
	var ch store.ProxyStartChange
	if m.failAnswers > 0 {
		m.failAnswers--
		return ch, errors.New("database blip")
	}
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
	if m.commitThenFailAnswers > 0 {
		m.commitThenFailAnswers--
		return store.ProxyStartChange{}, errors.New("commit acknowledgement lost")
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
			uncontained := p.State == types.ProxyStartFailed && p.ContainedAt == nil && m.activeLocked(p.RunID) == p
			if uncontained || p.State == types.ProxyStartPending && p.Deadline != nil && !now.Before(*p.Deadline) {
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

// A failed readback says the run is lost until revived only where the store has it lost; a run that
// cannot be kept is torn down, and the answer says it is ended.
func TestReadback_FailureAnswerNamesTheStoredFate(t *testing.T) {
	for _, interactive := range []bool{true, false} {
		f := newReadbackFixture(t)
		f.st.run.Interactive, f.run.Interactive = interactive, interactive
		f.rr.onReplace = func(cfg []byte) {
			var c struct {
				RunToken string `json:"run_token"`
			}
			_ = json.Unmarshal(cfg, &c)
			claims, _ := f.srv.cfg.Identity.Verify(context.Background(), c.RunToken, internalAudience)
			postLoaded(t, f.srv, f.run.ID, claims.JTI, hashHex([]byte("other bytes")))
		}
		w := do(t, f.srv, http.MethodPost, "/api/v1/runs/"+f.run.ID.String()+"/revive", adminToken, "")
		run, _ := f.st.GetRun(context.Background(), f.run.ID)
		want, never := "lost until revived", "is ended"
		if !interactive {
			want, never = "is ended", "lost until revived"
		}
		if body := w.Body.String(); w.Code != http.StatusBadGateway || !strings.Contains(body, want) || strings.Contains(body, never) {
			t.Errorf("interactive=%v (stored %s, lost %v): %d %s, want 502 saying %q", interactive, run.State, run.LostAt != nil, w.Code, body, want)
		}
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

// seedFailedStart records the run's start of generation gen as failed at its deadline: answered with a hash,
// and either never reported (reported "") or reported with a different hash.
func (f *readbackFixture) seedFailedStart(gen int, reported string) {
	f.rb.pmu.Lock()
	defer f.rb.pmu.Unlock()
	f.rb.starts[f.run.ID] = map[int]*store.ProxyStart{gen: {RunID: f.run.ID, Generation: gen,
		State: types.ProxyStartFailed, ExpectedSHA256: "aa", ReportedSHA256: reported}}
	if gen > 1 {
		f.rb.gmu.Lock()
		f.rb.state.Generations = []types.PolicyGeneration{{Generation: 1}, {Generation: gen}}
		f.rb.gmu.Unlock()
	}
}

// reviveAnswer is what a substrate under the orchestrator says to CanReplaceProxy.
type reviveAnswer int

const (
	noReviver   reviveAnswer = iota // the substrate is no runner.ProxyReviver, as the Kubernetes driver
	canRevive                       // CanReplaceProxy answers nil, as the Docker driver
	unsupported                     // CanReplaceProxy answers runner.ErrReviveUnsupported
	unreadable                      // CanReplaceProxy answers an error that says nothing about reviving
)

// unrevivableSubstrate is a lostRunner as one substrate of the orchestrator: no runner.ProxyReviver.
type unrevivableSubstrate struct {
	*lostRunner
	name string
}

func (s unrevivableSubstrate) Name() string { return s.name }

func (unrevivableSubstrate) Classes(context.Context) (substrate.ClassSupport, error) {
	return substrate.ClassSupport{}, nil
}

// reviveSubstrate is a reviveRunner as one substrate of the orchestrator, answering CanReplaceProxy with answer.
type reviveSubstrate struct {
	*reviveRunner
	name   string
	answer error
}

func (s reviveSubstrate) Name() string { return s.name }

func (reviveSubstrate) Classes(context.Context) (substrate.ClassSupport, error) {
	return substrate.ClassSupport{}, nil
}

func (s reviveSubstrate) CanReplaceProxy(context.Context, string) error { return s.answer }

// substrateRefNames is an orchestrator.RefStore holding the substrate name of each ref.
type substrateRefNames map[string]string

func (m substrateRefNames) PutRef(_ context.Context, ref, name string) error {
	m[ref] = name
	return nil
}
func (m substrateRefNames) DeleteRef(_ context.Context, ref string) error { delete(m, ref); return nil }
func (m substrateRefNames) GetRef(_ context.Context, ref string) (string, bool, error) {
	name, ok := m[ref]
	return name, ok, nil
}

// orchestrated is the runner a deployment holds: an orchestrator around the run's substrate, as cmd/wardynd
// builds it, never a bare fake, which no deployment has. The substrate is called name and answers
// CanReplaceProxy as ans says. With beside set, a Docker substrate that can revive stands next to it and the
// ref store names the run's substrate, so a ref routed to the wrong one reads the Docker answer.
func (f *readbackFixture) orchestrated(name string, ans reviveAnswer, beside bool) *orchestrator.Orchestrator {
	var own substrate.Substrate = unrevivableSubstrate{lostRunner: f.lr, name: name}
	switch ans {
	case canRevive:
		own = reviveSubstrate{reviveRunner: f.rr, name: name}
	case unsupported:
		own = reviveSubstrate{reviveRunner: f.rr, name: name, answer: runner.ErrReviveUnsupported}
	case unreadable:
		own = reviveSubstrate{reviveRunner: f.rr, name: name, answer: errors.New("the substrate cannot be read")}
	}
	if !beside {
		return orchestrator.New(own)
	}
	docker := reviveSubstrate{reviveRunner: &reviveRunner{lostRunner: f.lr}, name: "docker"}
	return orchestrator.New(own, docker).WithRefStore(substrateRefNames{f.st.run.SandboxRef: name})
}

// A No network run's first proxy start that never reported, on Kubernetes (the k8s target, a driver that
// cannot revive), names the CNI cause and is ended, never kept lost. Every other run keeps today's handling:
// a Docker run that misses the deadline is kept lost, a run with another preset, a start that reported a
// different hash, a later generation and an in-place narrowing keep today's words.
// Catches the CNI case left silent, and the change reaching other substrates, other failures, other runs or
// later starts.
func TestReadback_NoNetworkFirstStartNamesTheCNI(t *testing.T) {
	noNetwork := &types.WallPresetOnRun{ID: types.WallPresetNoNetwork, Name: "No network", Scope: types.WallPresetScopeBuiltin}
	other := &types.WallPresetOnRun{ID: "other", Name: "Other", Scope: types.WallPresetScopeBuiltin}
	plain := unconfirmedPolicyHint
	cni := unconfirmedPolicyHint + unconfirmedNoNetworkCause
	const kept = "kept lost"
	for _, tc := range []struct {
		name        string
		target      string       // the server's runner target
		sub         reviveAnswer // how the run's substrate answers CanReplaceProxy
		preset      *types.WallPresetOnRun
		interactive bool
		starting    bool
		gen         int
		reported    string
		narrowed    bool
		want        string // kept, or the failure hint the run ends with
	}{
		{"Kubernetes, interactive", "k8s", noReviver, noNetwork, true, false, 1, "", false, cni},
		{"Kubernetes, background", "k8s", noReviver, noNetwork, false, false, 1, "", false, cni},
		{"Kubernetes, still starting", "k8s", noReviver, noNetwork, true, true, 1, "", false, cni},
		{"Docker, interactive", "docker", canRevive, noNetwork, true, false, 1, "", false, kept},
		{"Docker, background", "docker", canRevive, noNetwork, false, false, 1, "", false, plain},
		{"Docker, still starting", "docker", canRevive, noNetwork, true, true, 1, "", false, plain},
		{"another preset, interactive", "k8s", noReviver, other, true, false, 1, "", false, kept},
		{"another preset, background", "k8s", noReviver, other, false, false, 1, "", false, plain},
		{"another preset, still starting", "k8s", noReviver, other, true, true, 1, "", false, plain},
		{"a reported hash that differs, interactive", "k8s", noReviver, noNetwork, true, false, 1, "bb", false, kept},
		{"a reported hash that differs, background", "k8s", noReviver, noNetwork, false, false, 1, "bb", false, plain},
		{"generation 2, interactive", "k8s", noReviver, noNetwork, true, false, 2, "", false, kept},
		{"generation 2, background", "k8s", noReviver, noNetwork, false, false, 2, "", false, plain},
		{"an in-place narrowing keeps its words", "k8s", noReviver, other, false, false, 2, "", true, unconfirmedNarrowedHint},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newEgressEditFixture(t)
			f.srv.cfg.RunnerTarget = tc.target
			f.srv.cfg.Runner = f.orchestrated(tc.target, tc.sub, false)
			f.st.run.WallPreset, f.st.run.Interactive = tc.preset, tc.interactive
			if tc.starting {
				f.st.state = types.RunStarting
			}
			f.seedFailedStart(tc.gen, tc.reported)
			run, err := f.srv.cfg.Store.GetRun(context.Background(), f.st.run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if tc.narrowed {
				f.rb.pmu.Lock()
				f.rb.starts[f.run.ID][tc.gen].InPlace = true
				f.rb.pmu.Unlock()
			}
			if err := f.srv.stopUnconfirmedProxyAt(context.Background(), run, tc.gen); err != nil {
				t.Fatalf("stopUnconfirmedProxyAt: %v", err)
			}
			lostAt, _ := f.st.lost()
			if tc.want == kept {
				if lostAt == nil || f.st.State() != types.RunRunning {
					t.Errorf("state %s, lost %v; want the run kept lost, still RUNNING", f.st.State(), lostAt)
				}
				if got := f.st.FailureHint(); got != "" {
					t.Errorf("failure hint = %q, want none on a run kept lost", got)
				}
				return
			}
			if f.st.State() != types.RunFailed || lostAt != nil {
				t.Errorf("state %s, lost %v; want the run ended, not lost", f.st.State(), lostAt)
			}
			if got := f.st.FailureHint(); got != tc.want {
				t.Errorf("failure hint = %q, want %q", got, tc.want)
			}
		})
	}
}

// A failed start that a live egress edit applied in place ends its run with the narrowed words when the
// reconcile sweep contains it on a plain context, as when the edit does; a start the replace path recorded
// keeps the replace words. Catches the narrowed words riding only the edit's own context.
func TestReadback_InPlaceStartHintWhoeverContains(t *testing.T) {
	other := &types.WallPresetOnRun{ID: "other", Name: "Other", Scope: types.WallPresetScopeBuiltin}
	for _, tc := range []struct {
		name    string
		inPlace bool
		want    string
	}{
		{"applied in place", true, unconfirmedNarrowedHint},
		{"applied by a replace", false, unconfirmedPolicyHint},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newEgressEditFixture(t)
			f.srv.cfg.RunnerTarget = "k8s"
			f.srv.cfg.Runner = f.orchestrated("k8s", noReviver, false)
			f.st.run.WallPreset, f.st.run.Interactive = other, false
			f.seedFailedStart(2, "")
			f.rb.pmu.Lock()
			f.rb.starts[f.run.ID][2].InPlace = tc.inPlace
			f.rb.pmu.Unlock()
			if err := f.srv.sweepOverdueProxyStarts(context.Background()); err != nil {
				t.Fatal(err)
			}
			if lostAt, _ := f.st.lost(); f.st.State() != types.RunFailed || lostAt != nil {
				t.Errorf("state %s, lost %v; want the run ended, not lost", f.st.State(), lostAt)
			}
			if got := f.st.FailureHint(); got != tc.want {
				t.Errorf("failure hint = %q, want %q", got, tc.want)
			}
			if p := f.rb.get(f.run.ID, 2); p.ContainedAt == nil {
				t.Errorf("start 2 = %+v, want its stop recorded", p)
			}
		})
	}
}

// A No network run on a registered runner whose first proxy start never reported names the CNI cause when
// the runner's last caps say EndsUnkept (a Kubernetes runner), and is ended, never kept lost, even when it is
// interactive. A runner without that cap, or whose caps are not held, gets the plain hint. The server's own
// substrate is Docker that can revive, so only the runner's caps can make the run Kubernetes. Catches the
// runner arm of the CNI case left untested: every other pin runs in server mode.
func TestReadback_RunnerModeNoNetworkFirstStartNamesTheCNI(t *testing.T) {
	noNetwork := &types.WallPresetOnRun{ID: types.WallPresetNoNetwork, Name: "No network", Scope: types.WallPresetScopeBuiltin}
	cni := unconfirmedPolicyHint + unconfirmedNoNetworkCause
	for _, tc := range []struct {
		name        string
		caps        *runnerwire.Caps // the runner's last caps; nil: none held
		interactive bool
		want        string
	}{
		{"Kubernetes runner, interactive", &runnerwire.Caps{EndsUnkept: true}, true, cni},
		{"Kubernetes runner, background", &runnerwire.Caps{EndsUnkept: true}, false, cni},
		{"another runner, background", &runnerwire.Caps{MachineName: "build-mac"}, false, unconfirmedPolicyHint},
		{"caps not held, background", nil, false, unconfirmedPolicyHint},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newEgressEditFixture(t)
			f.srv.cfg.RunnerTarget = "docker"
			f.srv.cfg.Runner = f.orchestrated("docker", canRevive, false)
			rid := uuid.New()
			if tc.caps != nil {
				seedRunnerCaps(t, f.srv, rid, *tc.caps)
			}
			// remote: reconcileFinalize leaves a local runner run with a sandbox to its runner.
			f.st.run.RunnerID, f.st.run.Placement = &rid, types.PlacementRemote
			f.st.run.WallPreset, f.st.run.Interactive = noNetwork, tc.interactive
			f.run = f.st.run
			f.seedFailedStart(1, "")
			if err := f.srv.sweepOverdueProxyStarts(context.Background()); err != nil {
				t.Fatal(err)
			}
			if lostAt, _ := f.st.lost(); f.st.State() != types.RunFailed || lostAt != nil {
				t.Errorf("state %s, lost %v; want the run ended, not lost", f.st.State(), lostAt)
			}
			if got := f.st.FailureHint(); got != tc.want {
				t.Errorf("failure hint = %q, want %q", got, tc.want)
			}
			if p := f.rb.get(f.run.ID, 1); p.ContainedAt == nil {
				t.Errorf("start 1 = %+v, want its stop recorded", p)
			}
		})
	}
}

// seedRunnerCaps is a runner's last caps event as the server holds it.
func seedRunnerCaps(t *testing.T, srv *Server, id uuid.UUID, caps runnerwire.Caps) {
	t.Helper()
	data, err := json.Marshal(caps)
	if err != nil {
		t.Fatal(err)
	}
	sub := remote.New(id.String(), &capsTransport{online: true}, remote.Options{})
	sub.HandleEvent(runnerwire.Event{Kind: runnerwire.EventCaps, Data: data})
	srv.runnerStreams.subsMu.Lock()
	defer srv.runnerStreams.subsMu.Unlock()
	if srv.runnerStreams.subs == nil {
		srv.runnerStreams.subs = map[uuid.UUID]runnerEndpoint{}
	}
	srv.runnerStreams.subs[id] = runnerEndpoint{sub: sub, link: &remote.Link{}}
}

// runOnKubernetes and runUnrevivable: the k8s target says Kubernetes, and a server's run is unrevivable when its
// own substrate, asked through the orchestrator every deployment holds, answers runner.ErrReviveUnsupported (or is
// no runner.ProxyReviver); a runner whose last caps say EndsUnkept is both. Docker, an answer that is an error of
// another kind, a runner without that cap and a runner whose caps are not held are not unrevivable, so those runs
// keep today's handling.
func TestRunOnKubernetes_ServerAndRunner(t *testing.T) {
	srv := &Server{}
	kube, docker, held := uuid.New(), uuid.New(), uuid.New()
	seedRunnerCaps(t, srv, kube, runnerwire.Caps{EndsUnkept: true})
	seedRunnerCaps(t, srv, docker, runnerwire.Caps{MachineName: "build-mac"})
	f := &readbackFixture{reviveFixture: &reviveFixture{lostFixture: &lostFixture{lr: &lostRunner{}}, rr: &reviveRunner{lostRunner: &lostRunner{}}}}
	for _, tc := range []struct {
		name               string
		target             string
		sub                reviveAnswer
		runnerID           *uuid.UUID
		wantKube, wantUnrv bool
	}{
		{"server, Kubernetes driver", "k8s", noReviver, nil, true, true},
		{"server, Kubernetes driver answering unsupported", "k8s", unsupported, nil, true, true},
		{"server, Kubernetes driver answering another error", "k8s", unreadable, nil, true, false},
		{"server, Docker driver", "docker", canRevive, nil, false, false},
		{"runner that ends unkept", "docker", canRevive, &kube, true, true},
		{"runner that keeps its sandbox", "k8s", noReviver, &docker, false, false},
		{"runner with no caps held", "k8s", noReviver, &held, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv.cfg.RunnerTarget = tc.target
			srv.cfg.Runner = f.orchestrated(tc.target, tc.sub, false)
			run := types.AgentRun{RunnerID: tc.runnerID}
			if got := srv.runOnKubernetes(run); got != tc.wantKube {
				t.Errorf("runOnKubernetes = %v, want %v", got, tc.wantKube)
			}
			if got := srv.runUnrevivable(context.Background(), run); got != tc.wantUnrv {
				t.Errorf("runUnrevivable = %v, want %v", got, tc.wantUnrv)
			}
		})
	}
}

// A server-mode No network run's first start that never reported is ended with the CNI hint when its own
// substrate cannot replace a proxy, whatever else the orchestrator holds: on Kubernetes alone, whether the driver
// is no ProxyReviver or answers unsupported, and on Kubernetes beside Docker, where the run's ref routes to the
// Kubernetes substrate. A substrate whose answer is nil, or an error that says nothing about reviving, keeps an
// interactive run lost as today, and a Docker run's hint is exactly today's.
// Catches the skip tested on the orchestrator's interface (always a ProxyReviver in production), a ref routed to
// the other substrate, and an unreadable answer read as unrevivable.
func TestReadback_NoNetworkFirstStartDecidedBySubstrateAnswer(t *testing.T) {
	noNetwork := &types.WallPresetOnRun{ID: types.WallPresetNoNetwork, Name: "No network", Scope: types.WallPresetScopeBuiltin}
	plain := unconfirmedPolicyHint
	cni := unconfirmedPolicyHint + unconfirmedNoNetworkCause
	const kept = "kept lost"
	for _, tc := range []struct {
		name        string
		target      string
		sub         reviveAnswer
		beside      bool // a Docker substrate that can revive stands beside the run's
		interactive bool
		starting    bool
		want        string
	}{
		{"Kubernetes alone, no ProxyReviver, interactive", "k8s", noReviver, false, true, false, cni},
		{"Kubernetes alone, answers unsupported, interactive", "k8s", unsupported, false, true, false, cni},
		{"Kubernetes alone, answers unsupported, background", "k8s", unsupported, false, false, false, cni},
		{"Kubernetes alone, answers unsupported, still starting", "k8s", unsupported, false, true, true, cni},
		{"Kubernetes alone, answers another error, interactive", "k8s", unreadable, false, true, false, kept},
		{"Kubernetes alone, answers another error, background", "k8s", unreadable, false, false, false, cni},
		{"Kubernetes alone, answers nil, interactive", "k8s", canRevive, false, true, false, kept},
		{"Kubernetes beside Docker, no ProxyReviver, interactive", "k8s", noReviver, true, true, false, cni},
		{"Kubernetes beside Docker, answers unsupported, interactive", "k8s", unsupported, true, true, false, cni},
		{"Kubernetes beside Docker, answers unsupported, background", "k8s", unsupported, true, false, false, cni},
		{"Kubernetes beside Docker, answers unsupported, still starting", "k8s", unsupported, true, true, true, cni},
		{"Kubernetes beside Docker, answers another error, interactive", "k8s", unreadable, true, true, false, kept},
		{"Docker alone, answers nil, interactive", "docker", canRevive, false, true, false, kept},
		{"Docker alone, answers nil, background", "docker", canRevive, false, false, false, plain},
		{"Docker alone, answers nil, still starting", "docker", canRevive, false, true, true, plain},
		{"Docker alone, answers another error, interactive", "docker", unreadable, false, true, false, kept},
		{"Docker alone, answers unsupported, interactive", "docker", unsupported, false, true, false, plain},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newEgressEditFixture(t)
			f.srv.cfg.RunnerTarget = tc.target
			f.srv.cfg.Runner = f.orchestrated(tc.target, tc.sub, tc.beside)
			f.st.run.WallPreset, f.st.run.Interactive = noNetwork, tc.interactive
			if tc.starting {
				f.st.state = types.RunStarting
			}
			f.seedFailedStart(1, "")
			run, err := f.srv.cfg.Store.GetRun(context.Background(), f.st.run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if err := f.srv.stopUnconfirmedProxyAt(context.Background(), run, 1); err != nil {
				t.Fatalf("stopUnconfirmedProxyAt: %v", err)
			}
			lostAt, _ := f.st.lost()
			if tc.want == kept {
				if lostAt == nil || f.st.State() != types.RunRunning {
					t.Errorf("state %s, lost %v; want the run kept lost, still RUNNING", f.st.State(), lostAt)
				}
				if got := f.st.FailureHint(); got != "" {
					t.Errorf("failure hint = %q, want none on a run kept lost", got)
				}
				return
			}
			if f.st.State() != types.RunFailed || lostAt != nil {
				t.Errorf("state %s, lost %v; want the run ended, not lost", f.st.State(), lostAt)
			}
			if got := f.st.FailureHint(); got != tc.want {
				t.Errorf("failure hint = %q, want %q", got, tc.want)
			}
		})
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
		{"0.10.0-rc.17+0b4c6bac3", "", store.ProxyStartLegacy},
		{"0.10.0-rc.17+0b4c6bac3-dirty", "", store.ProxyStartLegacy},
		{"0.10.0-rc.18+0b4c6bac3", "", store.ProxyStartExpected},
		{"v0.10.0-rc.17+0b4c6bac3", "", store.ProxyStartLegacy},
		{"0.10.0+0b4c6bac3", "", store.ProxyStartExpected},
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
	if p := d.st.get(d.run.ID, 1); p.State != types.ProxyStartFailed || p.Mode != store.ProxyStartExpected || p.ContainedAt == nil {
		t.Errorf("start = %s/%s (contained %v), want failed/expected, contained", p.State, p.Mode, p.ContainedAt)
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
			_, _ = d.st.BeginProxyStart(context.Background(), d.run.ID, 2, "J-next", store.ProxyStartExpected, false)
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
