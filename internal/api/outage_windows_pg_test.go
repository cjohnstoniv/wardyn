// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/federation"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func outageWindowRows(t *testing.T, st store.PG, where string, args ...any) int {
	t.Helper()
	var n int
	if err := st.Pool.QueryRow(context.Background(), `SELECT count(*) FROM outage_windows WHERE `+where, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// openServerWindowAge is how long ago the open server_outage window began; ok is false when none is open.
func openServerWindowAge(t *testing.T, st store.PG) (time.Duration, bool) {
	t.Helper()
	var secs []float64
	rows, err := st.Pool.Query(context.Background(),
		`SELECT EXTRACT(EPOCH FROM now() - started_at)::float8 FROM outage_windows WHERE kind = 'server_outage' AND ended_at IS NULL`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var s float64
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		secs = append(secs, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(secs) == 0 {
		return 0, false
	}
	if len(secs) > 1 {
		t.Fatalf("open server_outage windows = %d, want at most 1", len(secs))
	}
	return time.Duration(secs[0] * float64(time.Second)), true
}

func wantServerWindowAbout(t *testing.T, st store.PG, ago time.Duration) {
	t.Helper()
	age, ok := openServerWindowAge(t, st)
	if !ok {
		t.Fatal("no server_outage window is open")
	}
	if age < ago-30*time.Second || age > ago+30*time.Second {
		t.Fatalf("the server_outage window began %v ago, want about %v", age, ago)
	}
}

// newServerOutageFixture is the lapse sweep's fixture past its recovery grace, so only the census decides.
func newServerOutageFixture(t *testing.T) *lapseSweepFixture {
	t.Helper()
	f := newLapseSweepFixture(t)
	f.srv.cfg.StoreHealth = healthyTracker(lapseTestGrace, lapseTestGrace+time.Minute)
	return f
}

// censusFailPG is a store whose lapse census read fails.
type censusFailPG struct{ store.PG }

func (censusFailPG) LapseCensus(context.Context, time.Duration, time.Duration) (store.LapseCensus, error) {
	return store.LapseCensus{}, errors.New("census read failed")
}

// Server windows open before any token expires: two server runs that last renewed 21 minutes ago, none lapsed.
func TestServerOutageWindow_OpensBeforeAnyLapse_PG(t *testing.T) {
	f := newServerOutageFixture(t)
	f.serverRun(t, false, 21*time.Minute)
	f.serverRun(t, false, 21*time.Minute)
	f.sweepTokens(t)
	wantServerWindowAbout(t, f.st, 21*time.Minute)
	f.requireNothingEnded(t)
}

// A census that marks no outage closes the open server window.
func TestServerOutageWindow_ClosesOnCleanCensus_PG(t *testing.T) {
	f := newServerOutageFixture(t)
	f.serverRun(t, false, 21*time.Minute)
	f.serverRun(t, false, 21*time.Minute)
	f.sweepTokens(t)
	wantServerWindowAbout(t, f.st, 21*time.Minute)
	if _, err := f.st.Pool.Exec(context.Background(), `UPDATE agent_runs SET token_renewed_at = now()`); err != nil {
		t.Fatal(err)
	}
	f.sweepTokens(t)
	if _, ok := openServerWindowAge(t, f.st); ok {
		t.Fatal("a clean census left the server window open")
	}
	if n := outageWindowRows(t, f.st, `kind = 'server_outage' AND ended_at IS NOT NULL`); n != 1 {
		t.Fatalf("closed server_outage windows = %d, want 1", n)
	}
}

// A failed census read neither opens nor closes a window.
func TestServerOutageWindow_FailedReadKeepsItOpen_PG(t *testing.T) {
	f := newServerOutageFixture(t)
	f.serverRun(t, false, 21*time.Minute)
	f.serverRun(t, false, 21*time.Minute)
	f.sweepTokens(t)
	wantServerWindowAbout(t, f.st, 21*time.Minute)
	f.srv.cfg.Store = censusFailPG{f.st}
	f.sweepTokens(t)
	wantServerWindowAbout(t, f.st, 21*time.Minute)
}

// A lone server run whose proxy died does not cover its own expiry; two such runs are a group.
func TestServerOutageWindow_SingleRunDoesNotCoverItself_PG(t *testing.T) {
	t.Run("one run, newest renew 21 minutes old", func(t *testing.T) {
		f := newServerOutageFixture(t)
		run := f.serverRun(t, false, 21*time.Minute)
		f.sweepTokens(t)
		if n := outageWindowRows(t, f.st, `true`); n != 0 {
			t.Fatalf("windows = %d, want 0", n)
		}
		var renewed time.Time
		if err := f.st.Pool.QueryRow(context.Background(), `SELECT token_renewed_at FROM agent_runs WHERE id = $1`, run.ID).Scan(&renewed); err != nil {
			t.Fatal(err)
		}
		if w, ok, err := f.st.OutageWindowCovers(context.Background(), nil, "", renewed.Add(60*time.Minute)); err != nil || ok {
			t.Fatalf("the run's own expiry is covered: %+v, %v, %v", w, ok, err)
		}
	})
	t.Run("one lapsed run with a mass-lapse minimum of 1", func(t *testing.T) {
		f := newServerOutageFixture(t)
		f.srv.cfg.LapseMassMin = 1
		f.serverRun(t, false, 70*time.Minute)
		f.sweepTokens(t)
		if n := outageWindowRows(t, f.st, `true`); n != 0 {
			t.Fatalf("windows = %d, want 0", n)
		}
	})
	t.Run("two runs, newest renew 21 minutes old", func(t *testing.T) {
		f := newServerOutageFixture(t)
		f.serverRun(t, false, 21*time.Minute)
		f.serverRun(t, false, 21*time.Minute)
		f.sweepTokens(t)
		wantServerWindowAbout(t, f.st, 21*time.Minute)
	})
}

// A mass lapse of at least two of the server's runs opens a window going back the census window (30 min); one
// lapsed run opens none. A fresh run keeps the no-renewals condition off, so only the mass-lapse arm decides. It
// fails if the arm's `>= serverOutageGroupMin` becomes `>` (two lapsed open nothing), if the group minimum drops to
// 1 (one lapsed opens a window), or if the arm stops going back lapseCensusWindow.
func TestServerOutageWindow_MassLapseOpensForAGroup_PG(t *testing.T) {
	t.Run("two lapsed runs", func(t *testing.T) {
		f := newServerOutageFixture(t)
		f.srv.cfg.LapseMassMin = 1
		f.serverRun(t, false, time.Minute)
		f.serverRun(t, false, 70*time.Minute)
		f.serverRun(t, false, 70*time.Minute)
		f.sweepTokens(t)
		wantServerWindowAbout(t, f.st, lapseCensusWindow)
	})
	t.Run("one lapsed run", func(t *testing.T) {
		f := newServerOutageFixture(t)
		f.srv.cfg.LapseMassMin = 1
		f.serverRun(t, false, time.Minute)
		f.serverRun(t, false, 70*time.Minute)
		f.sweepTokens(t)
		if n := outageWindowRows(t, f.st, `true`); n != 0 {
			t.Fatalf("windows = %d, want 0", n)
		}
	})
	// Both arms hold: the window goes back the larger span, the 70 min since the newest renewal. It fails if the
	// mass-lapse arm overwrites the no-renewals span instead of taking the larger of the two.
	t.Run("both arms hold", func(t *testing.T) {
		f := newServerOutageFixture(t)
		f.srv.cfg.LapseMassMin = 1
		f.serverRun(t, false, 70*time.Minute)
		f.serverRun(t, false, 70*time.Minute)
		f.sweepTokens(t)
		wantServerWindowAbout(t, f.st, 70*time.Minute)
	})
}

// With rule M switched off, the next sweep pass closes an open server window rather than leaving it open to cover
// later expiries. It fails if the rule-M-off return comes back ahead of the close.
func TestServerOutageWindow_RuleMOffClosesIt_PG(t *testing.T) {
	f := newServerOutageFixture(t)
	f.serverRun(t, false, 21*time.Minute)
	f.serverRun(t, false, 21*time.Minute)
	f.sweepTokens(t)
	wantServerWindowAbout(t, f.st, 21*time.Minute)
	f.srv.cfg.StoreHealth = nil
	f.sweepTokens(t)
	if n := outageWindowRows(t, f.st, `ended_at IS NULL`); n != 0 {
		t.Fatalf("open windows = %d, want 0", n)
	}
	if n := outageWindowRows(t, f.st, `kind = 'server_outage' AND ended_at IS NOT NULL`); n != 1 {
		t.Fatalf("closed server windows = %d, want 1", n)
	}
}

// A recovering store neither opens nor closes a server window: the heartbeat's window covers that time. It fails if
// the recovering check is dropped, since the recovering verdict reads no census and would close the window.
func TestServerOutageWindow_RecoveringChangesNothing_PG(t *testing.T) {
	f := newServerOutageFixture(t)
	f.serverRun(t, false, 21*time.Minute)
	f.serverRun(t, false, 21*time.Minute)
	f.sweepTokens(t)
	wantServerWindowAbout(t, f.st, 21*time.Minute)
	f.srv.cfg.StoreHealth = recoveringTracker(lapseTestGrace)
	f.sweepTokens(t)
	wantServerWindowAbout(t, f.st, 21*time.Minute)
	if n := outageWindowRows(t, f.st, `true`); n != 1 {
		t.Fatalf("windows = %d, want the one still open", n)
	}
}

// linkFixture is a server over real Postgres with a claimed person-owned runner whose inventory row holds session.
type linkFixture struct {
	srv     *Server
	st      store.PG
	rn      types.Runner
	session string
}

func newLinkFixture(t *testing.T) *linkFixture {
	t.Helper()
	ctx := context.Background()
	st := store.NewPG(throwawayPGPool(t))
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	id := uuid.New()
	rn, err := st.CreateRunner(ctx, types.Runner{ID: id, Owner: "link-owner", OwnerKind: types.RunnerOwnerPerson, Name: "laptop", PublicKey: pub,
		KeyFingerprint: id.String(), Version: "0.9.0", OrgURLSHA256: federation.OrgURLSHA256(admissionOrgURL), Limits: roomyRunnerLimits()})
	if err != nil {
		t.Fatal(err)
	}
	if rn, err = st.ClaimRunner(ctx, rn.ID, rn.Owner, rn.KeyFingerprint, time.Now()); err != nil {
		t.Fatal(err)
	}
	session := uuid.NewString()
	if err := st.BeginRunnerInventory(ctx, rn.ID, session); err != nil {
		t.Fatal(err)
	}
	if _, err := st.PutSiteConfig(ctx, types.SiteConfig{Runners: &types.RunnerSettings{Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	return &linkFixture{srv: testServer(t, baseTestConfig(newHarness(t), st)), st: st, rn: rn, session: session}
}

// getRunnerFailPG is a store whose runner read fails.
type getRunnerFailPG struct{ store.PG }

func (getRunnerFailPG) GetRunner(context.Context, uuid.UUID) (types.Runner, error) {
	return types.Runner{}, errors.New("runner read failed")
}

// siteConfigFailPG is a server store whose site settings read fails.
type siteConfigFailPG struct{ store.PG }

func (siteConfigFailPG) GetSiteConfig(context.Context) (types.SiteConfig, error) {
	return types.SiteConfig{}, errors.New("site settings read failed")
}

// A session the server ended because the runner may not come back opens no window: a revoked or released runner,
// a person-owned runner whose owner is suspended, runners switched off, or a runner that cannot be read.
func TestRunnerLinkWindow_ServerEndedSessionOpensNothing_PG(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, f *linkFixture) store.RunnerStore
	}{
		{"revoked", func(t *testing.T, f *linkFixture) store.RunnerStore {
			if _, err := f.st.RevokeRunner(context.Background(), f.rn.ID, time.Now(), "admin", "test"); err != nil {
				t.Fatal(err)
			}
			return f.st
		}},
		{"released to unclaimed", func(t *testing.T, f *linkFixture) store.RunnerStore {
			if _, err := f.st.Pool.Exec(context.Background(), `UPDATE runners SET state = 'unclaimed' WHERE id = $1`, f.rn.ID); err != nil {
				t.Fatal(err)
			}
			return f.st
		}},
		{"owner suspended", func(t *testing.T, f *linkFixture) store.RunnerStore {
			if _, err := f.st.Pool.Exec(context.Background(), `INSERT INTO principal_identities (principal, issuer, deactivated_at) VALUES ($1, 'test-issuer', now())`,
				f.rn.Owner); err != nil {
				t.Fatal(err)
			}
			if rn, err := f.st.GetRunner(context.Background(), f.rn.ID); err != nil || rn.State != types.RunnerClaimed || rn.RevokedAt != nil {
				t.Fatalf("the suspended owner's runner = %+v, %v; want claimed and unrevoked", rn, err)
			}
			return f.st
		}},
		{"runners switched off", func(t *testing.T, f *linkFixture) store.RunnerStore {
			if _, err := f.st.PutSiteConfig(context.Background(), types.SiteConfig{Runners: &types.RunnerSettings{Enabled: false}}); err != nil {
				t.Fatal(err)
			}
			return f.st
		}},
		{"runner read fails", func(t *testing.T, f *linkFixture) store.RunnerStore {
			return getRunnerFailPG{f.st}
		}},
		// The owner read fails while the runner and site settings still read: it fails if that error is ignored.
		{"owner read fails", func(t *testing.T, f *linkFixture) store.RunnerStore {
			if _, err := f.st.Pool.Exec(context.Background(), `ALTER TABLE principal_identities RENAME TO principal_identities_away`); err != nil {
				t.Fatal(err)
			}
			return f.st
		}},
		// The site settings read fails while the runner reads: it fails if that error counts as runners switched on.
		{"site settings read fails", func(t *testing.T, f *linkFixture) store.RunnerStore {
			f.srv.cfg.Store = siteConfigFailPG{f.st}
			return f.st
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newLinkFixture(t)
			st := c.setup(t, f)
			f.srv.openRunnerLinkWindow(context.Background(), st, f.rn.ID, f.session, 120*time.Second)
			if n := outageWindowRows(t, f.st, `true`); n != 0 {
				t.Fatalf("windows = %d, want 0", n)
			}
		})
	}
	t.Run("control: an admissible runner", func(t *testing.T) {
		f := newLinkFixture(t)
		f.srv.openRunnerLinkWindow(context.Background(), f.st, f.rn.ID, f.session, 120*time.Second)
		if n := outageWindowRows(t, f.st, `kind = 'runner_link' AND runner_id = $1 AND ended_at IS NULL`, f.rn.ID); n != 1 {
			t.Fatalf("open runner_link windows = %d, want 1", n)
		}
	})
}

// The runner's next session closes its open link window in the production Connected callback.
func TestRunnerLink_ConnectedClosesWindow_PG(t *testing.T) {
	f := newAdmissionFixture(t)
	old := uuid.NewString()
	if err := f.st.BeginRunnerInventory(f.ctx, f.rn.ID, old); err != nil {
		t.Fatal(err)
	}
	if opened, err := f.st.OpenRunnerLinkWindow(f.ctx, f.rn.ID, old, 2*time.Minute); err != nil || !opened {
		t.Fatalf("OpenRunnerLinkWindow = %v, %v; want opened", opened, err)
	}
	s := f.connect(t, nil, f.liveRow())
	if !f.waitReady(s) {
		t.Fatalf("session never reached READY: %v", s.result())
	}
	if n := outageWindowRows(t, f.st, `kind = 'runner_link' AND runner_id = $1 AND ended_at IS NOT NULL`, f.rn.ID); n != 1 {
		t.Fatalf("closed runner_link windows = %d, want 1", n)
	}
	if n := outageWindowRows(t, f.st, `ended_at IS NULL`); n != 0 {
		t.Fatalf("open windows = %d, want 0", n)
	}
}

// swapBeatPG beats through whichever store cur holds: the live one, or one over a closed pool.
type swapBeatPG struct {
	store.PG
	cur   atomic.Pointer[store.PG]
	beats atomic.Int32
}

func (s *swapBeatPG) BeatControlPlane(ctx context.Context, gapOver time.Duration) (*store.OutageWindow, error) {
	w, err := s.cur.Load().BeatControlPlane(ctx, gapOver)
	if err == nil {
		s.beats.Add(1)
	}
	return w, err
}

func (s *swapBeatPG) HealRunnerLinkWindows(ctx context.Context) (int, error) {
	return s.cur.Load().HealRunnerLinkWindows(ctx)
}

func (s *swapBeatPG) PruneOutageWindows(ctx context.Context, olderThan time.Duration) (int, error) {
	return s.cur.Load().PruneOutageWindows(ctx, olderThan)
}

// Acceptance: the beat loop's database is gone for 45 s, then back. Exactly one closed cp_outage window of 40-60 s.
func TestOutageWindows_DatabaseStop45s_PG(t *testing.T) {
	live := store.NewPG(throwawayPGPool(t))
	closedPool, err := pgxpool.New(context.Background(), os.Getenv("WARDYN_TEST_PG"))
	if err != nil {
		t.Fatal(err)
	}
	closedPool.Close()
	stopped := store.NewPG(closedPool)
	sw := &swapBeatPG{PG: live}
	sw.cur.Store(&live)
	srv := testServer(t, baseTestConfig(newHarness(t), sw))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { srv.runCPHeartbeat(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	waitBeats := func(n int32, within time.Duration) {
		t.Helper()
		deadline := time.Now().Add(within)
		for sw.beats.Load() < n {
			if time.Now().After(deadline) {
				t.Fatalf("beats = %d, want %d within %v", sw.beats.Load(), n, within)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	waitBeats(1, 2*time.Second)
	sw.cur.Store(&stopped) // right after a beat, so the gap is measured from it
	time.Sleep(45 * time.Second)
	sw.cur.Store(&live)
	waitBeats(2, cpHeartbeatEvery+5*time.Second)
	if n := outageWindowRows(t, live, `kind = 'cp_outage'`); n != 1 {
		t.Fatalf("cp_outage windows = %d, want 1", n)
	}
	var gap float64
	if err := live.Pool.QueryRow(context.Background(),
		`SELECT EXTRACT(EPOCH FROM ended_at - started_at)::float8 FROM outage_windows WHERE kind = 'cp_outage' AND ended_at IS NOT NULL`).Scan(&gap); err != nil {
		t.Fatal(err)
	}
	if gap < 40 || gap > 60 {
		t.Fatalf("cp_outage window = %.1f s, want 40-60 s", gap)
	}
}
