// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func leaseServiceRun(now time.Time, endsIn time.Duration, l types.RunLimits) types.AgentRun {
	end, capAt := now.Add(endsIn), now.Add(24*time.Hour)
	return types.AgentRun{
		CreatedAt: now.Add(-time.Minute), Experience: types.ExperienceBackground, EndsAt: &end, RunLimits: l,
		LifetimeCapAt: &capAt, LifetimeCapSource: types.LimitSourcePool,
		Service: &types.RunService{Restart: types.ServiceRestartOnFailure},
	}
}

func TestPlanServiceRenewal(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	hour := time.Hour
	lease := types.RunLimits{MaxEndAheadSec: 7200}
	at := func(d time.Duration) *time.Time { v := now.Add(d); return &v }
	status := func(h types.ServiceHealthState) *types.RunServiceStatus {
		return &types.RunServiceStatus{Health: h, ObservedAt: now}
	}
	for _, tc := range []struct {
		name     string
		mutate   func(*types.AgentRun)
		endsIn   time.Duration
		limits   types.RunLimits
		boundary int
		want     time.Duration // the new end past now; 0 = no renewal
		unit     int
		source   types.LimitSource
		health   types.ServiceHealthState
	}{
		{name: "inside the half-life, no observation yet counts as starting", endsIn: hour, limits: lease,
			want: 2 * hour, unit: 7200, source: types.LimitSourceGovernance, health: types.ServiceHealthStarting},
		{name: "healthy", endsIn: 10 * time.Minute, limits: lease, mutate: func(r *types.AgentRun) { r.ServiceStatus = status(types.ServiceHealthHealthy) },
			want: 2 * hour, unit: 7200, source: types.LimitSourceGovernance, health: types.ServiceHealthHealthy},
		{name: "outside the half-life", endsIn: hour + time.Second, limits: lease},
		{name: "unhealthy is never renewed", endsIn: time.Minute, limits: lease, mutate: func(r *types.AgentRun) { r.ServiceStatus = status(types.ServiceHealthUnhealthy) }},
		{name: "unknown is never renewed", endsIn: time.Minute, limits: lease, mutate: func(r *types.AgentRun) { r.ServiceStatus = status(types.ServiceHealthUnknown) }},
		{name: "a stale observation is unknown", endsIn: time.Minute, limits: lease, mutate: func(r *types.AgentRun) {
			r.ServiceStatus = status(types.ServiceHealthHealthy)
			r.ServiceStatus.ObservedAt = now.Add(-serviceStatusStale - time.Second)
		}},
		{name: "an observation at the stale mark still counts", endsIn: time.Minute, limits: lease, mutate: func(r *types.AgentRun) {
			r.ServiceStatus = status(types.ServiceHealthHealthy)
			r.ServiceStatus.ObservedAt = now.Add(-serviceStatusStale)
		}, want: 2 * hour, unit: 7200, source: types.LimitSourceGovernance, health: types.ServiceHealthHealthy},
		{name: "no observation past the supervisor's grace is unknown", endsIn: time.Minute, limits: lease, mutate: func(r *types.AgentRun) {
			r.CreatedAt = now.Add(-serviceSupervisorGrace)
		}},
		{name: "the grace runs from admission for a queued run", endsIn: time.Minute, limits: lease, mutate: func(r *types.AgentRun) {
			r.CreatedAt = now.Add(-time.Hour)
			r.AdmittedAt = at(-time.Minute)
		}, want: 2 * hour, unit: 7200, source: types.LimitSourceGovernance, health: types.ServiceHealthStarting},
		{name: "not a service", endsIn: time.Minute, limits: lease, mutate: func(r *types.AgentRun) { r.Service = nil }},
		{name: "no end", endsIn: time.Minute, limits: lease, mutate: func(r *types.AgentRun) { r.EndsAt = nil }},
		{name: "no lifetime cap is no renewal", endsIn: time.Minute, limits: lease, mutate: func(r *types.AgentRun) {
			r.LifetimeCapAt, r.LifetimeCapSource = nil, ""
		}},
		{name: "lost", endsIn: time.Minute, limits: lease, mutate: func(r *types.AgentRun) { r.LostAt = at(-time.Minute) }},
		{name: "paused", endsIn: time.Minute, limits: lease, mutate: func(r *types.AgentRun) { r.PausedAt = at(-time.Minute) }},
		{name: "the default end is the unit when no lease is named", endsIn: time.Minute, limits: types.RunLimits{DefaultEndSec: 1800},
			want: 30 * time.Minute, unit: 1800, source: types.LimitSourceGovernance, health: types.ServiceHealthStarting},
		{name: "the lease wins over the default end", endsIn: time.Minute, limits: types.RunLimits{MaxEndAheadSec: 7200, DefaultEndSec: 1800},
			want: 2 * hour, unit: 7200, source: types.LimitSourceGovernance, health: types.ServiceHealthStarting},
		{name: "neither is no unit", endsIn: time.Minute},
		{name: "the boundary tightens the unit", endsIn: 20 * time.Minute, limits: lease, boundary: 3600,
			want: hour, unit: 3600, source: "boundary", health: types.ServiceHealthStarting},
		{name: "a looser boundary changes nothing", endsIn: time.Minute, limits: lease, boundary: 86400,
			want: 2 * hour, unit: 7200, source: types.LimitSourceGovernance, health: types.ServiceHealthStarting},
		{name: "the pool's cap clamps the target and names itself", endsIn: time.Minute, limits: lease,
			mutate: func(r *types.AgentRun) {
				r.LifetimeCapAt, r.LifetimeCapSource = at(90*time.Minute), types.LimitSourcePool
			},
			want: 90 * time.Minute, unit: 7200, source: types.LimitSourcePool, health: types.ServiceHealthStarting},
		{name: "an end already at the cap is left alone", endsIn: 30 * time.Minute, limits: lease,
			mutate: func(r *types.AgentRun) {
				r.LifetimeCapAt, r.LifetimeCapSource = at(30*time.Minute), types.LimitSourcePool
			}},
		{name: "a tightened unit is the one renewed with", endsIn: 10 * time.Minute, limits: types.RunLimits{MaxEndAheadSec: 1200},
			want: 20 * time.Minute, unit: 1200, source: types.LimitSourceGovernance, health: types.ServiceHealthStarting},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := leaseServiceRun(now, tc.endsIn, tc.limits)
			if tc.mutate != nil {
				tc.mutate(&run)
			}
			got, ok := planServiceRenewal(run, tc.boundary, now)
			if tc.want == 0 {
				if ok {
					t.Fatalf("renewed to %v, want no renewal", got.target)
				}
				return
			}
			if !ok {
				t.Fatal("no renewal, want one")
			}
			if !got.target.Equal(now.Add(tc.want)) || got.unit != tc.unit || got.source != tc.source || got.health != tc.health {
				t.Errorf("renewal = %+v, want target now+%v unit %d source %q health %q", got, tc.want, tc.unit, tc.source, tc.health)
			}
		})
	}
}

// TestServiceRenewsLater: a declined service renews later only with a lifetime cap its end is short of.
func TestServiceRenewsLater(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	lease := types.RunLimits{MaxEndAheadSec: 7200}
	for _, tc := range []struct {
		name   string
		mutate func(*types.AgentRun)
		want   bool
	}{
		{name: "capped, end short of the cap", want: true},
		{name: "no cap", mutate: func(r *types.AgentRun) { r.LifetimeCapAt, r.LifetimeCapSource = nil, "" }},
		{name: "end at the cap", mutate: func(r *types.AgentRun) { r.LifetimeCapAt = r.EndsAt }},
		{name: "not a service", mutate: func(r *types.AgentRun) { r.Service = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := leaseServiceRun(now, 45*time.Minute, lease)
			if tc.mutate != nil {
				tc.mutate(&run)
			}
			if got := serviceRenewsLater(run, now); got != tc.want {
				t.Errorf("serviceRenewsLater = %v, want %v", got, tc.want)
			}
		})
	}
}

// newServiceLeaseFixture is a lease fixture whose run is a service with a two-hour lease and a
// pool lifetime cap a day out, started a minute ago: with no health observation it is still
// starting (renewalHealth).
func newServiceLeaseFixture(t *testing.T, endsIn time.Duration) *leaseFixture {
	t.Helper()
	f := newLeaseFixture(t, endsIn)
	f.st.run.CreatedAt = f.now.Add(-time.Minute)
	f.st.run.Experience = types.ExperienceBackground
	f.st.run.Service = &types.RunService{Restart: types.ServiceRestartOnFailure}
	f.st.run.RunLimits = types.RunLimits{MaxEndAheadSec: 7200}
	capAt := f.now.Add(24 * time.Hour)
	f.st.run.LifetimeCapAt, f.st.run.LifetimeCapSource = &capAt, types.LimitSourcePool
	return f
}

func (f *leaseFixture) endsAt() time.Time {
	run, _ := f.st.GetRun(context.Background(), f.run.ID)
	return *run.EndsAt
}

func TestRunLease_AHealthyServiceIsRenewedAndAudited(t *testing.T) {
	f := newServiceLeaseFixture(t, 30*time.Minute)
	f.st.run.ServiceStatus = &types.RunServiceStatus{Health: types.ServiceHealthHealthy, ObservedAt: f.now}
	f.sweep(t)

	want := f.now.Add(2 * time.Hour)
	if got := f.endsAt(); !got.Equal(want) {
		t.Fatalf("ends_at = %v, want %v", got, want)
	}
	evs := f.audit.eventsFor(f.run.ID, "run.end.set")
	if len(evs) != 1 || evs[0].Outcome != "success" || evs[0].ActorType != types.ActorSystem {
		t.Fatalf("run.end.set events = %+v, want one system success", evs)
	}
	data := leaseAuditData(t, evs[0])
	if data["reason"] != "service_renewed" || data["health"] != "healthy" || data["max"] != float64(7200) || data["source"] != "governance" {
		t.Errorf("audit data = %v", data)
	}
	// The sweep goes on with the renewed end: the old one, 30 minutes out, would have warned.
	if warned := f.audit.eventsFor(f.run.ID, "run.ending_soon"); len(warned) != 0 {
		t.Errorf("run.ending_soon rows = %d, want none after a renewal", len(warned))
	}
	f.sweep(t)
	if n := len(f.audit.eventsFor(f.run.ID, "run.end.set")); n != 1 {
		t.Errorf("a second pass renewed again: %d rows", n)
	}
}

func TestRunLease_ANewServiceWithNoObservationIsRenewed(t *testing.T) {
	f := newServiceLeaseFixture(t, 30*time.Minute)
	f.sweep(t)
	if got := f.endsAt(); !got.Equal(f.now.Add(2 * time.Hour)) {
		t.Fatalf("ends_at = %v, want a renewal", got)
	}
	if h := leaseAuditData(t, f.audit.eventsFor(f.run.ID, "run.end.set")[0])["health"]; h != "starting" {
		t.Errorf("health = %v, want starting", h)
	}
}

func TestRunLease_AnUnhealthyServiceIsNotRenewed(t *testing.T) {
	for _, h := range []types.ServiceHealthState{types.ServiceHealthUnhealthy, types.ServiceHealthUnknown} {
		f := newServiceLeaseFixture(t, 30*time.Minute)
		f.st.run.ServiceStatus = &types.RunServiceStatus{Health: h, ObservedAt: f.now}
		end := f.endsAt()
		f.sweep(t)
		if got := f.endsAt(); !got.Equal(end) {
			t.Errorf("%s: ends_at moved to %v", h, got)
		}
		if n := len(f.audit.eventsFor(f.run.ID, "run.end.set")); n != 0 {
			t.Errorf("%s: %d run.end.set rows, want none", h, n)
		}
		if n := len(f.audit.eventsFor(f.run.ID, "run.ending_soon")); n != 1 {
			t.Errorf("%s: %d run.ending_soon rows, want the normal warning", h, n)
		}
	}
}

func TestRunLease_ARenewalNeverPassesTheLifetimeCap(t *testing.T) {
	f := newServiceLeaseFixture(t, 30*time.Minute)
	capAt := f.now.Add(90 * time.Minute)
	f.st.run.LifetimeCapAt, f.st.run.LifetimeCapSource = &capAt, types.LimitSourcePool
	f.sweep(t)
	if got := f.endsAt(); !got.Equal(capAt) {
		t.Fatalf("ends_at = %v, want the cap %v", got, capAt)
	}
	if src := leaseAuditData(t, f.audit.eventsFor(f.run.ID, "run.end.set")[0])["source"]; src != "pool" {
		t.Errorf("source = %v, want pool", src)
	}
}

// With no lifetime cap a service is not renewed: it ends at its lease and gets the normal warning.
func TestRunLease_AServiceWithNoLifetimeCapIsNotRenewed(t *testing.T) {
	f := newServiceLeaseFixture(t, 30*time.Minute)
	f.st.run.LifetimeCapAt, f.st.run.LifetimeCapSource = nil, ""
	f.st.run.ServiceStatus = &types.RunServiceStatus{Health: types.ServiceHealthHealthy, ObservedAt: f.now}
	end := f.endsAt()
	f.sweep(t)
	if got := f.endsAt(); !got.Equal(end) {
		t.Errorf("ends_at moved to %v, want %v", got, end)
	}
	if n := len(f.audit.eventsFor(f.run.ID, "run.end.set")); n != 0 {
		t.Errorf("%d run.end.set rows, want none", n)
	}
	if n := len(f.audit.eventsFor(f.run.ID, "run.ending_soon")); n != 1 {
		t.Errorf("%d run.ending_soon rows, want the normal warning", n)
	}
}

func TestRunLease_ALostRenewalWritesNothing(t *testing.T) {
	f := newServiceLeaseFixture(t, 30*time.Minute)
	moved := f.now.Add(40 * time.Minute)
	f.st.beforeSetEnd = func() { f.st.setEnd(moved) }
	f.sweep(t)
	if got := f.endsAt(); !got.Equal(moved) {
		t.Errorf("ends_at = %v, want the competing edit %v", got, moved)
	}
	if n := len(f.audit.eventsFor(f.run.ID, "run.end.set")); n != 0 {
		t.Errorf("%d run.end.set rows after a lost compare-and-set, want none", n)
	}
}

// ownerChainLeaseStore is the lease store with the run's captured unit read back as chainErr.
type ownerChainLeaseStore struct {
	*leaseStore
	chainErr error
	reads    atomic.Int32 // GetGovernanceUnitChain calls: the owner re-check's first read
}

func (s *ownerChainLeaseStore) GetGovernanceUnitChain(context.Context, uuid.UUID) ([]types.GovernanceUnit, error) {
	s.reads.Add(1)
	return nil, s.chainErr
}

// A renewal moves the end later, so it meets the owner re-check a PATCH extending the end meets;
// a refusal, and a check that cannot be answered, renew nothing and audit nothing.
func TestRunLease_ARenewalNeedsTheOwnersAuthorityToExtend(t *testing.T) {
	for _, tc := range []struct {
		name      string
		chainErr  error
		grantsErr error
	}{
		{name: "the captured unit is deleted", chainErr: store.ErrNotFound},
		{name: "the captured unit cannot be read", chainErr: errors.New("connection refused")},
		{name: "the run's grants cannot be read", grantsErr: errors.New("connection refused")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newServiceLeaseFixture(t, 30*time.Minute)
			f.st.run.ServiceStatus = &types.RunServiceStatus{Health: types.ServiceHealthHealthy, ObservedAt: f.now}
			if tc.chainErr != nil {
				unit := uuid.New()
				f.st.run.UnitID = &unit
				f.srv.cfg.Store = &ownerChainLeaseStore{leaseStore: f.st, chainErr: tc.chainErr}
			}
			f.st.credGrantsErr = tc.grantsErr
			end := f.endsAt()
			f.sweep(t)
			if got := f.endsAt(); !got.Equal(end) {
				t.Errorf("ends_at moved to %v, want %v", got, end)
			}
			if n := len(f.audit.eventsFor(f.run.ID, "run.end.set")); n != 0 {
				t.Errorf("%d run.end.set rows, want none", n)
			}
			if n := len(f.audit.eventsFor(f.run.ID, "run.ending_soon")); n != 1 {
				t.Errorf("%d run.ending_soon rows, want the normal warning", n)
			}
		})
	}
}

// serviceIdentityLeaseStore is the lease store with the service identity a run was created by read back as svc.
type serviceIdentityLeaseStore struct {
	*leaseStore
	store.ServiceIdentityStore // only GetServiceIdentity and ServiceStillSponsored are asked
	svc                        types.ServiceIdentity
}

func (s *serviceIdentityLeaseStore) GetServiceIdentity(_ context.Context, name string) (types.ServiceIdentity, error) {
	if name != s.svc.Name {
		return types.ServiceIdentity{}, store.ErrNotFound
	}
	return s.svc, nil
}

// ServiceStillSponsored answers as the real store does for a sponsorship that began before the run.
func (s *serviceIdentityLeaseStore) ServiceStillSponsored(_ context.Context, name, sponsor string, _ time.Time) (bool, error) {
	if name != s.svc.Name {
		return false, store.ErrNotFound
	}
	return s.svc.Usable() && s.svc.Sponsor == sponsor, nil
}

// A service whose identity is retired or suspended is not renewed, and nothing is audited: its tokens
// are gone or refused, so a renewal would keep a run alive nobody can act for. An active identity renews.
func TestRunLease_AServiceWithoutALiveIdentityIsNotRenewed(t *testing.T) {
	then := time.Now().Add(-time.Hour)
	for _, tc := range []struct {
		name    string
		svc     types.ServiceIdentity
		renewed bool
	}{
		{name: "retired", svc: types.ServiceIdentity{Name: "ops-bot", Sponsor: "t@example.com", RetiredAt: &then}},
		{name: "suspended", svc: types.ServiceIdentity{Name: "ops-bot", Sponsor: "t@example.com", SuspendedAt: &then}},
		{name: "active", svc: types.ServiceIdentity{Name: "ops-bot", Sponsor: "t@example.com"}, renewed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newServiceLeaseFixture(t, 30*time.Minute)
			f.st.run.ServiceStatus = &types.RunServiceStatus{Health: types.ServiceHealthHealthy, ObservedAt: f.now}
			f.st.run.CreatedBy = types.ServicePrincipalPrefix + "ops-bot"
			f.srv.cfg.Store = &serviceIdentityLeaseStore{leaseStore: f.st, svc: tc.svc}
			f.srv.cfg.SessionRevocations = fakeAuthzSessionRevocations{}
			end := f.endsAt()
			f.sweep(t)
			want, wantRows := end, 0
			if tc.renewed {
				want, wantRows = f.now.Add(2*time.Hour), 1
			}
			if got := f.endsAt(); !got.Equal(want) {
				t.Errorf("ends_at = %v, want %v", got, want)
			}
			if n := len(f.audit.eventsFor(f.run.ID, "run.end.set")); n != wantRows {
				t.Errorf("%d run.end.set rows, want %d", n, wantRows)
			}
		})
	}
}

// The health sweep rewrites an unchanged status only every serviceStatusRefresh: a healthy service
// observed one refresh and more ago is still renewed.
func TestRunLease_AnUnchangedHealthyServiceIsRenewedAcrossTheRefresh(t *testing.T) {
	f := newServiceLeaseFixture(t, 30*time.Minute)
	f.st.run.ServiceStatus = &types.RunServiceStatus{Health: types.ServiceHealthHealthy,
		ObservedAt: f.now.Add(-serviceStatusRefresh - 4*time.Minute)}
	f.sweep(t)
	if got := f.endsAt(); !got.Equal(f.now.Add(2 * time.Hour)) {
		t.Fatalf("ends_at = %v, want a renewal", got)
	}
}

// A runner no replica holds keeps its last status forever: renewal reads an old one as unknown.
func TestRunLease_AServiceNotHeardFromIsNotRenewed(t *testing.T) {
	for name, mutate := range map[string]func(f *leaseFixture){
		"a stale observation": func(f *leaseFixture) {
			f.st.run.ServiceStatus = &types.RunServiceStatus{Health: types.ServiceHealthHealthy,
				ObservedAt: f.now.Add(-serviceStatusStale - time.Minute)}
		},
		"no observation past the grace": func(f *leaseFixture) { f.st.run.CreatedAt = f.now.Add(-time.Hour) },
	} {
		f := newServiceLeaseFixture(t, 30*time.Minute)
		mutate(f)
		end := f.endsAt()
		f.sweep(t)
		if got := f.endsAt(); !got.Equal(end) {
			t.Errorf("%s: ends_at moved to %v", name, got)
		}
		if n := len(f.audit.eventsFor(f.run.ID, "run.end.set")); n != 0 {
			t.Errorf("%s: %d run.end.set rows, want none", name, n)
		}
	}
}

// A service renewal will keep alive has its owner's authority read only while a warning is due: before the
// warning window the sweep has nothing to quiet, so it reads nothing, and inside it a refused authority is
// still warned at once.
func TestRunLease_AQuietServiceReadsTheOwnersAuthorityOnlyInsideTheWarningWindow(t *testing.T) {
	for _, tc := range []struct {
		name   string
		endsIn time.Duration
		read   bool
	}{
		{name: "before the warning window", endsIn: 90 * time.Minute},
		{name: "inside the warning window", endsIn: 45 * time.Minute, read: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newServiceLeaseFixture(t, tc.endsIn)
			f.st.run.RunLimits = types.RunLimits{MaxEndAheadSec: 3600}
			f.st.run.ServiceStatus = &types.RunServiceStatus{Health: types.ServiceHealthHealthy, ObservedAt: f.now}
			unit := uuid.New()
			f.st.run.UnitID = &unit
			chain := &ownerChainLeaseStore{leaseStore: f.st, chainErr: store.ErrNotFound}
			f.srv.cfg.Store = chain
			f.sweep(t)
			if read := chain.reads.Load() > 0; read != tc.read {
				t.Errorf("owner authority read = %v (%d reads), want %v", read, chain.reads.Load(), tc.read)
			}
			if n := len(f.audit.eventsFor(f.run.ID, "run.ending_soon")); (n == 1) != tc.read || n > 1 {
				t.Errorf("%d run.ending_soon rows, want warned=%v", n, tc.read)
			}
		})
	}
}

// A service renewal keeps alive is never warned: with a unit of an hour or less the warning window
// opens before the half-life, and every renewal would send run.ending_soon. A service renewal will
// not keep alive is warned as before.
func TestRunLease_EndingSoonOnlyWhenRenewalWillNotHappen(t *testing.T) {
	healthy := func(f *leaseFixture) {
		f.st.run.ServiceStatus = &types.RunServiceStatus{Health: types.ServiceHealthHealthy, ObservedAt: f.now}
	}
	for _, tc := range []struct {
		name    string
		endsIn  time.Duration
		mutate  func(f *leaseFixture)
		renewed bool
		warned  bool
	}{
		{name: "healthy, inside the warning window, before the half-life", endsIn: 45 * time.Minute, mutate: healthy},
		{name: "healthy, renewed to a full unit inside the warning window", endsIn: 20 * time.Minute, mutate: healthy, renewed: true},
		{name: "starting, before the half-life", endsIn: 45 * time.Minute},
		// Renewal will be refused at the half-life (TestRunLease_ARenewalNeedsTheOwnersAuthorityToExtend),
		// so the run is warned now.
		{name: "healthy, before the half-life, its captured unit deleted", endsIn: 45 * time.Minute, warned: true, mutate: func(f *leaseFixture) {
			healthy(f)
			unit := uuid.New()
			f.st.run.UnitID = &unit
			f.srv.cfg.Store = &ownerChainLeaseStore{leaseStore: f.st, chainErr: store.ErrNotFound}
		}},
		{name: "unhealthy", endsIn: 45 * time.Minute, warned: true, mutate: func(f *leaseFixture) {
			f.st.run.ServiceStatus = &types.RunServiceStatus{Health: types.ServiceHealthUnhealthy, ObservedAt: f.now}
		}},
		{name: "unknown", endsIn: 45 * time.Minute, warned: true, mutate: func(f *leaseFixture) {
			f.st.run.ServiceStatus = &types.RunServiceStatus{Health: types.ServiceHealthUnknown, ObservedAt: f.now}
		}},
		{name: "at the lifetime cap", endsIn: 45 * time.Minute, warned: true, mutate: func(f *leaseFixture) {
			healthy(f)
			capAt := f.now.Add(45 * time.Minute)
			f.st.run.LifetimeCapAt, f.st.run.LifetimeCapSource = &capAt, types.LimitSourcePool
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newServiceLeaseFixture(t, tc.endsIn)
			f.st.run.RunLimits = types.RunLimits{MaxEndAheadSec: 3600}
			if tc.mutate != nil {
				tc.mutate(f)
			}
			end := f.endsAt()
			f.sweep(t)
			if renewed := !f.endsAt().Equal(end); renewed != tc.renewed {
				t.Errorf("renewed = %v, want %v", renewed, tc.renewed)
			}
			if n := len(f.audit.eventsFor(f.run.ID, "run.ending_soon")); (n == 1) != tc.warned || n > 1 {
				t.Errorf("%d run.ending_soon rows, want warned=%v", n, tc.warned)
			}
		})
	}
}

// The API never serves a stale or a paused run's last read as its health: both are unknown, saying why, with
// observed_at kept so the reader sees when it was last checked, and the stored status unchanged. A fresh
// read is served as stored.
func TestServiceHealthOnTheWire(t *testing.T) {
	f := newServiceLeaseFixture(t, time.Hour)
	paused := f.now.Add(-time.Minute)
	for _, tc := range []struct {
		name     string
		observed time.Time
		paused   *time.Time
		health   types.ServiceHealthState
		reason   string
	}{
		{name: "fresh", observed: f.now.Add(-time.Minute), health: types.ServiceHealthHealthy},
		{name: "stale", observed: f.now.Add(-serviceStatusStale - time.Second), health: types.ServiceHealthUnknown, reason: "unreachable"},
		{name: "paused", observed: f.now.Add(-time.Minute), paused: &paused, health: types.ServiceHealthUnknown, reason: "paused"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stored := &types.RunServiceStatus{Health: types.ServiceHealthHealthy, Restarts: 2, ObservedAt: tc.observed}
			run := f.run
			run.ServiceStatus, run.PausedAt = stored, tc.paused
			runs := []types.AgentRun{run}
			f.srv.projectStatusDetailFor(httptest.NewRequest(http.MethodGet, "/api/v1/runs/"+run.ID.String(), nil), runs)
			got := runs[0].ServiceStatus
			if got.Health != tc.health || got.Reason != tc.reason || !got.ObservedAt.Equal(tc.observed) || got.Restarts != 2 {
				t.Errorf("served %+v, want health %q reason %q, observed_at and restarts as stored", *got, tc.health, tc.reason)
			}
			if stored.Health != types.ServiceHealthHealthy || stored.Reason != "" {
				t.Errorf("the stored status changed to %+v", *stored)
			}
		})
	}
}
