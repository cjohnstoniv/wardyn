// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

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

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func mustZone(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

func TestRunScheduleNextOccurrence_Daily(t *testing.T) {
	london := mustZone(t, "Europe/London")
	r := scheduleRule{Every: scheduleEveryDaily, Hour: 9, Min: 0, Zone: london}
	before := time.Date(2026, 11, 24, 8, 0, 0, 0, london)
	if got, want := r.nextOccurrence(before), time.Date(2026, 11, 24, 9, 0, 0, 0, london); !got.Equal(want) {
		t.Errorf("before the time = %s, want %s", got, want)
	}
	at := time.Date(2026, 11, 24, 9, 0, 0, 0, london)
	if got, want := r.nextOccurrence(at), time.Date(2026, 11, 25, 9, 0, 0, 0, london); !got.Equal(want) {
		t.Errorf("at the time = %s, want the next day's %s (strictly after)", got, want)
	}
}

func TestRunScheduleNextOccurrence_Weekdays(t *testing.T) {
	london := mustZone(t, "Europe/London")
	r := scheduleRule{Every: scheduleEveryWeekdays, Hour: 9, Min: 0, Zone: london}
	fri := time.Date(2026, 11, 27, 9, 0, 0, 0, london) // a Friday
	if fri.Weekday() != time.Friday {
		t.Fatalf("fixture is a %s", fri.Weekday())
	}
	if got, want := r.nextOccurrence(fri), time.Date(2026, 11, 30, 9, 0, 0, 0, london); !got.Equal(want) || got.Weekday() != time.Monday {
		t.Errorf("Fri 09:00 = %s (%s), want Mon %s", got, got.Weekday(), want)
	}
}

func TestRunScheduleNextOccurrence_Weekly(t *testing.T) {
	london := mustZone(t, "Europe/London")
	r := scheduleRule{Every: scheduleEveryWeekly, Weekday: time.Tuesday, Hour: 9, Min: 0, Zone: london}
	tue := time.Date(2026, 11, 24, 9, 0, 0, 0, london) // a Tuesday
	if got, want := r.nextOccurrence(tue), time.Date(2026, 12, 1, 9, 0, 0, 0, london); !got.Equal(want) {
		t.Errorf("Tue 09:00 = %s, want the next Tuesday %s", got, want)
	}
	if got, want := r.nextOccurrence(tue.Add(-time.Hour)), tue; !got.Equal(want) {
		t.Errorf("Tue 08:00 = %s, want %s", got, want)
	}
}

// 01:30 on the spring-forward Sunday does not exist in Europe/London: the start is the instant time.Date gives,
// on that date and within one hour of 01:30.
func TestRunScheduleNextOccurrence_ClockChange(t *testing.T) {
	london := mustZone(t, "Europe/London")
	r := scheduleRule{Every: scheduleEveryDaily, Hour: 1, Min: 30, Zone: london}
	got := r.nextOccurrence(time.Date(2027, 3, 27, 12, 0, 0, 0, london)).In(london)
	if y, m, d := got.Date(); y != 2027 || m != time.March || d != 28 {
		t.Fatalf("the spring-forward start is %s, want on 2027-03-28", got)
	}
	wall := time.Date(2027, 3, 28, 1, 30, 0, 0, time.UTC)
	shown := time.Date(2027, 3, 28, got.Hour(), got.Minute(), 0, 0, time.UTC)
	if d := shown.Sub(wall); d < -time.Hour || d > time.Hour {
		t.Errorf("the spring-forward start reads %s in the zone, more than an hour from 01:30", got.Format("15:04"))
	}
}

// A weekly schedule made at 09:01 for 09:00 first starts the next day, and repeats on that weekday.
func TestRunScheduleFirstSlot_WeeklyWeekdayFromFirstSlot(t *testing.T) {
	london := mustZone(t, "Europe/London")
	mon := time.Date(2026, 11, 30, 9, 1, 0, 0, london)
	if mon.Weekday() != time.Monday {
		t.Fatalf("fixture is a %s", mon.Weekday())
	}
	r, ok := parseScheduleRule(scheduleEveryWeekly, "09:00", "Europe/London")
	if !ok {
		t.Fatal("rule refused")
	}
	first := r.firstSlot(mon)
	if want := time.Date(2026, 12, 1, 9, 0, 0, 0, london); !first.Equal(want) || r.Weekday != time.Tuesday {
		t.Errorf("first slot %s, weekday %s; want %s, Tuesday", first, r.Weekday, want)
	}
}

func TestNarrowScheduleGroups_DropsAndReportsGains(t *testing.T) {
	for _, tc := range []struct {
		name          string
		stored        []string
		storedTrunc   bool
		rec           store.PrincipalGroups
		kept          []string
		truncated     bool
		gained        bool
		wantNilResult bool
	}{
		{name: "a dropped group narrows", stored: []string{"a", "b"}, rec: store.PrincipalGroups{Groups: []string{"b"}}, kept: []string{"b"}},
		{name: "a gained group is reported", stored: []string{"b"}, rec: store.PrincipalGroups{Groups: []string{"b", "c"}}, kept: []string{"b"}, gained: true},
		{name: "no stored list, empty sign-in", rec: store.PrincipalGroups{Groups: []string{}}, wantNilResult: true},
		{name: "no stored list, a group at sign-in", rec: store.PrincipalGroups{Groups: []string{"a"}}, wantNilResult: true, gained: true},
		{name: "a near-match counts as gained", stored: []string{"b"}, rec: store.PrincipalGroups{Groups: []string{"B"}}, kept: []string{}, gained: true},
		{name: "a null record keeps the stored list", stored: []string{"a"}, rec: store.PrincipalGroups{}, kept: []string{"a"}, truncated: true},
		{name: "a partial sign-in marks partial", stored: []string{"a"}, rec: store.PrincipalGroups{Groups: []string{"a"}, Truncated: true}, kept: []string{"a"}, truncated: true},
		{name: "a partial stored list marks partial", stored: []string{"a"}, storedTrunc: true, rec: store.PrincipalGroups{Groups: []string{"a"}}, kept: []string{"a"}, truncated: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kept, truncated, gained := narrowScheduleGroups(tc.stored, tc.storedTrunc, tc.rec)
			if tc.wantNilResult != (kept == nil) || strings.Join(kept, ",") != strings.Join(tc.kept, ",") || truncated != tc.truncated || gained != tc.gained {
				t.Errorf("= %q (nil %v) truncated %v gained %v; want %q truncated %v gained %v", kept, kept == nil, truncated, gained, tc.kept, tc.truncated, tc.gained)
			}
		})
	}
}

// fakeScheduleStore is a runScheduleStore in memory, with the store faults the sweep tests need.
type fakeScheduleStore struct {
	store.Store
	mu        sync.Mutex
	rows      map[uuid.UUID]store.RunSchedule
	dueCalls  int
	signedIn  time.Time
	signInErr error
	groups    store.PrincipalGroups
	groupsErr error
}

func newFakeScheduleStore() *fakeScheduleStore {
	return &fakeScheduleStore{rows: map[uuid.UUID]store.RunSchedule{}, signedIn: time.Now(), groups: store.PrincipalGroups{Groups: []string{}}}
}

func (f *fakeScheduleStore) CreateRunSchedule(_ context.Context, sc store.RunSchedule, _ int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows[sc.ID] = sc
	return nil
}

func (f *fakeScheduleStore) ListRunSchedules(context.Context, string) ([]store.RunSchedule, error) {
	return nil, nil
}

func (f *fakeScheduleStore) GetRunSchedule(_ context.Context, id uuid.UUID) (store.RunSchedule, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	sc, ok := f.rows[id]
	if !ok {
		return store.RunSchedule{}, store.ErrNotFound
	}
	return sc, nil
}

func (f *fakeScheduleStore) PauseRunSchedule(_ context.Context, id uuid.UUID, reason string, at time.Time) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	sc, ok := f.rows[id]
	if !ok || sc.PausedAt != nil {
		return false, nil
	}
	sc.PausedAt, sc.PausedReason = &at, reason
	f.rows[id] = sc
	return true, nil
}

func (f *fakeScheduleStore) ResumeRunSchedule(context.Context, uuid.UUID, json.RawMessage, time.Time, time.Time) (bool, error) {
	return false, errors.New("fake: resume not expected")
}

func (f *fakeScheduleStore) DeleteRunSchedule(_ context.Context, id uuid.UUID) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.rows[id]
	delete(f.rows, id)
	return ok, nil
}

func (f *fakeScheduleStore) DueRunSchedules(_ context.Context, before time.Time, _ int) ([]store.RunSchedule, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dueCalls++
	var out []store.RunSchedule
	for _, sc := range f.rows {
		if sc.PausedAt == nil && !sc.NextAt.After(before) {
			out = append(out, sc)
		}
	}
	return out, nil
}

func (f *fakeScheduleStore) AdvanceRunSchedule(_ context.Context, id uuid.UUID, wasNext, next time.Time, runID *uuid.UUID, outcome string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	sc, ok := f.rows[id]
	if !ok || !sc.NextAt.Equal(wasNext) {
		return false, nil
	}
	sc.NextAt, sc.LastDueAt, sc.LastRunID, sc.LastOutcome = next, &wasNext, runID, outcome
	f.rows[id] = sc
	return true, nil
}

func (f *fakeScheduleStore) ScheduleOwnerSignedInAt(context.Context, string) (time.Time, bool, error) {
	return f.signedIn, f.signInErr == nil, f.signInErr
}

func (f *fakeScheduleStore) GetPrincipalGroups(context.Context, string, string) (store.PrincipalGroups, error) {
	return f.groups, f.groupsErr
}

func (f *fakeScheduleStore) BlankRunTaskByID(context.Context, uuid.UUID) error { return nil }

func (f *fakeScheduleStore) row(t *testing.T, id uuid.UUID) store.RunSchedule {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	sc, ok := f.rows[id]
	if !ok {
		t.Fatalf("schedule %s is gone", id)
	}
	return sc
}

// seed stores a daily schedule of sub-person's due at next, paused when paused is set.
func (f *fakeScheduleStore) seed(next time.Time, paused bool) store.RunSchedule {
	sc := store.RunSchedule{ID: uuid.New(), Owner: "sub-person", Every: scheduleEveryDaily, LocalTime: next.UTC().Format("15:04"),
		TimeZone: "UTC", Request: json.RawMessage(`{"agent":"claude-code","task":"t"}`),
		Authority: mustJSON(queueAuthority{Sub: "sub-person", Groups: []string{}}), AuthorityAt: next.Add(-time.Hour), NextAt: next}
	if paused {
		at := next.Add(-time.Minute)
		sc.PausedAt, sc.PausedReason = &at, schedulePausedByOwner
	}
	f.rows[sc.ID] = sc
	return sc
}

func scheduleTestServer(st store.Store) *Server {
	return New(Config{Store: st, Audit: &recRecorder{}})
}

// scheduleRequest is a request to a schedule route as ctx's caller, with the {id} parameter set when id is.
func scheduleRequest(ctx context.Context, method, path, id, body string) *http.Request {
	r := httptest.NewRequest(method, path, strings.NewReader(body)).WithContext(ctx)
	if id != "" {
		rc := chi.NewRouteContext()
		rc.URLParams.Add("id", id)
		r = r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rc))
	}
	return r
}

const scheduleTestBody = `{"name":"Nightly","every":"daily","time":"09:00","time_zone":"Europe/London","request":{"agent":"claude-code","task":"t"}}`

func createScheduleAs(ctx context.Context, srv *Server, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	srv.handleCreateRunSchedule(w, scheduleRequest(ctx, http.MethodPost, "/api/v1/run-schedules", "", body))
	return w
}

func TestRunScheduleCreate_Off(t *testing.T) {
	t.Setenv(envScheduledRuns, "")
	st := newFakeScheduleStore()
	w := createScheduleAs(memberCtxFor("sub-person"), scheduleTestServer(st), scheduleTestBody)
	if w.Code != http.StatusUnprocessableEntity || errorReason(w) != reasonScheduledRunsOff {
		t.Fatalf("create with the switch off = %d %s; want 422 %s", w.Code, w.Body, reasonScheduledRunsOff)
	}
	if len(st.rows) != 0 {
		t.Errorf("a refused create stored %d schedules", len(st.rows))
	}
}

func TestRunScheduleCreate_RefusesStartAtInRequest(t *testing.T) {
	t.Setenv(envScheduledRuns, "1")
	st := newFakeScheduleStore()
	srv := scheduleTestServer(st)
	for _, field := range []string{`"start_at":"2026-12-01T09:00"`, `"start_time_zone":"UTC"`} {
		body := `{"every":"daily","time":"09:00","time_zone":"UTC","request":{"agent":"claude-code",` + field + `}}`
		if w := createScheduleAs(memberCtxFor("sub-person"), srv, body); w.Code != http.StatusBadRequest || errorReason(w) != reasonRunScheduleInvalid {
			t.Errorf("request with %s = %d %s; want 400 %s", field, w.Code, w.Body, reasonRunScheduleInvalid)
		}
	}
	if len(st.rows) != 0 {
		t.Errorf("a refused create stored %d schedules", len(st.rows))
	}
}

func TestRunScheduleCreate_RefusesWeekdayInBody(t *testing.T) {
	t.Setenv(envScheduledRuns, "1")
	body := `{"every":"weekly","weekday":2,"time":"09:00","time_zone":"UTC","request":{"agent":"claude-code"}}`
	if w := createScheduleAs(memberCtxFor("sub-person"), scheduleTestServer(newFakeScheduleStore()), body); w.Code != http.StatusBadRequest || errorReason(w) != reasonRunScheduleInvalid {
		t.Errorf("a body with weekday = %d %s; want 400 %s", w.Code, w.Body, reasonRunScheduleInvalid)
	}
}

// A schedule's name is held to a run title's 200 characters, counted in runes, with the title's reason.
func TestRunScheduleCreate_RefusesALongName(t *testing.T) {
	t.Setenv(envScheduledRuns, "1")
	body := func(name string) string {
		return `{"name":"` + name + `","every":"daily","time":"09:00","time_zone":"UTC","request":{"agent":"claude-code"}}`
	}
	st := newFakeScheduleStore()
	w := createScheduleAs(memberCtxFor("sub-person"), scheduleTestServer(st), body(strings.Repeat("é", maxRunTitleLen+1)))
	if w.Code != http.StatusBadRequest || errorReason(w) != reasonRunFieldTooLong || !strings.Contains(w.Body.String(), "name is too long (201 chars, max 200)") {
		t.Fatalf("a 201-character name = %d %s; want 400 %s naming the name", w.Code, w.Body, reasonRunFieldTooLong)
	}
	if len(st.rows) != 0 {
		t.Errorf("a refused create stored %d schedules", len(st.rows))
	}
	if w := createScheduleAs(memberCtxFor("sub-person"), scheduleTestServer(st), body(strings.Repeat("é", maxRunTitleLen))); errorReason(w) == reasonRunFieldTooLong {
		t.Errorf("a 200-character name of 400 bytes = %d %s; want it past the length check", w.Code, w.Body)
	}
}

// The admin token (no person) and a service identity keep no schedule.
func TestRunScheduleCreate_RefusesNonPerson(t *testing.T) {
	t.Setenv(envScheduledRuns, "1")
	for name, ctx := range map[string]context.Context{
		"admin token":      context.Background(),
		"service identity": withServiceIdentity(memberCtxFor("svc-bot"), "svc-bot", "sub-sponsor"),
	} {
		st := newFakeScheduleStore()
		w := createScheduleAs(ctx, scheduleTestServer(st), scheduleTestBody)
		if w.Code != http.StatusUnprocessableEntity || errorReason(w) != reasonRunScheduleOwnerUnsupported {
			t.Errorf("%s: create = %d %s; want 422 %s", name, w.Code, w.Body, reasonRunScheduleOwnerUnsupported)
		}
		if len(st.rows) != 0 {
			t.Errorf("%s: a refused create stored %d schedules", name, len(st.rows))
		}
	}
}

// Every API token is refused, whatever its role or kind, with the single sign-on sentence: no schedule outlives
// a token.
func TestRunScheduleCreate_RefusesEveryToken(t *testing.T) {
	t.Setenv(envScheduledRuns, "1")
	token := func(role string, cli bool) context.Context {
		ctx := withAPITokenID(withHumanIdentity(context.Background(), "sub-person", "person@corp.example", role, types.UserTypeStandard, []string{}, false), uuid.New())
		if cli {
			ctx = withCLISessionID(ctx, uuid.New())
		}
		return ctx
	}
	for name, ctx := range map[string]context.Context{
		"wdn_ token, role admin":  token(oidc.RoleAdmin, false),
		"a member's wdn_ token":   token(oidc.RoleUser, false),
		"cli_access token":        token(oidc.RoleUser, true),
		"cli_admin step-up token": token(oidc.RoleAdmin, true),
	} {
		st := newFakeScheduleStore()
		w := createScheduleAs(ctx, scheduleTestServer(st), scheduleTestBody)
		if w.Code != http.StatusUnprocessableEntity || errorReason(w) != reasonRunScheduleOwnerUnsupported ||
			!strings.Contains(w.Body.String(), runScheduleOwnerUnsupportedMsg) {
			t.Errorf("%s: create = %d %s; want 422 %s with its sentence", name, w.Code, w.Body, reasonRunScheduleOwnerUnsupported)
		}
		if len(st.rows) != 0 {
			t.Errorf("%s: a refused create stored %d schedules", name, len(st.rows))
		}
	}
}

// A resume while scheduled runs are off says the schedule cannot resume, never "Start the run now", and the
// schedule stays paused.
func TestRunScheduleResume_OffIsRefusedWithItsOwnSentence(t *testing.T) {
	t.Setenv(envScheduledRuns, "")
	st := newFakeScheduleStore()
	sc := st.seed(time.Now().Add(time.Hour), true)
	w := httptest.NewRecorder()
	scheduleTestServer(st).handleResumeRunSchedule(w, scheduleRequest(memberCtxFor("sub-person"), http.MethodPost,
		"/api/v1/run-schedules/"+sc.ID.String()+"/resume", sc.ID.String(), ""))
	if w.Code != http.StatusUnprocessableEntity || errorReason(w) != reasonScheduledRunsOff || !strings.Contains(w.Body.String(), runScheduleResumeOffMsg) {
		t.Fatalf("resume with the switch off = %d %s; want 422 %s with the resume sentence", w.Code, w.Body, reasonScheduledRunsOff)
	}
	if strings.Contains(w.Body.String(), "Start the run now") {
		t.Errorf("the resume refusal gives one-time advice: %s", w.Body)
	}
	if st.row(t, sc.ID).PausedAt == nil {
		t.Error("the refused resume left the schedule active")
	}
}

func TestRunScheduleSweep_OffFiresNothing(t *testing.T) {
	t.Setenv(envScheduledRuns, "")
	st := newFakeScheduleStore()
	st.seed(time.Now().Add(time.Minute), false)
	if err := scheduleTestServer(st).RunScheduleSweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if st.dueCalls != 0 {
		t.Errorf("the sweep with the switch off read due schedules %d times", st.dueCalls)
	}
}

// testSweep is a tick over st with create answering as given, the owner live unless live says otherwise.
func testSweep(srv *Server, st *fakeScheduleStore, now time.Time, create http.HandlerFunc, live bool) (*scheduleSweep, *int) {
	creates := 0
	sw := srv.newScheduleSweep(st)
	sw.now = now
	sw.create = func(w http.ResponseWriter, r *http.Request) {
		creates++
		if create != nil {
			create(w, r)
		}
	}
	sw.live = func(context.Context, queueAuthority, time.Time) (bool, error) { return live, nil }
	sw.cancel = func(context.Context, uuid.UUID) error { return nil }
	return sw, &creates
}

func answers(codes ...int) http.HandlerFunc {
	i := 0
	return func(w http.ResponseWriter, _ *http.Request) {
		code := codes[min(i, len(codes)-1)]
		i++
		if code == http.StatusConflict {
			writeErrorReason(w, code, reasonIdempotencyKeyInFlight, "A request with this key is still being answered.")
			return
		}
		writeErrorReason(w, code, reasonScheduledRunsUnavailable, scheduledRunsUnavailableMsg)
	}
}

// A create still running under the key (409), then a server fault (503), leave a slot that is still ahead
// unadvanced: the next tick tries the same key.
func TestRunScheduleSweep_InFlightLeavesUnadvanced(t *testing.T) {
	t.Setenv(envScheduledRuns, "1")
	st := newFakeScheduleStore()
	now := time.Now()
	sc := st.seed(now.Add(time.Minute), false)
	srv := scheduleTestServer(st)
	sw, creates := testSweep(srv, st, now, answers(http.StatusConflict, http.StatusServiceUnavailable), true)
	for range 2 {
		if err := sw.run(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if got := st.row(t, sc.ID); !got.NextAt.Equal(sc.NextAt) || got.LastOutcome != "" || got.PausedAt != nil {
		t.Errorf("after 409 then 503 the schedule is %+v; want it unadvanced and active", got)
	}
	if *creates != 2 {
		t.Errorf("creates = %d, want 2", *creates)
	}
}

// A missed slot an hour old whose replay meets a create still running under the key is left for the next tick,
// never recorded missed; one 25 hours old is recorded missed without a replay.
func TestRunScheduleSweep_MissedInFlightLeavesUnadvanced(t *testing.T) {
	t.Setenv(envScheduledRuns, "1")
	now := time.Now()
	st := newFakeScheduleStore()
	sc := st.seed(now.Add(-time.Hour), false)
	sw, creates := testSweep(scheduleTestServer(st), st, now, answers(http.StatusConflict), true)
	if err := sw.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := st.row(t, sc.ID); !got.NextAt.Equal(sc.NextAt) || got.LastOutcome == scheduleOutcomeMissed || *creates != 1 {
		t.Errorf("an hour-old slot whose replay is in flight = %+v after %d creates; want unadvanced after one replay", got, *creates)
	}

	st = newFakeScheduleStore()
	sc = st.seed(now.Add(-25*time.Hour), false)
	sw, creates = testSweep(scheduleTestServer(st), st, now, answers(http.StatusConflict), true)
	if err := sw.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := st.row(t, sc.ID); got.LastOutcome != scheduleOutcomeMissed || !got.NextAt.After(now) || *creates != 0 {
		t.Errorf("a 25-hour-old slot = %+v after %d creates; want advanced missed with no replay", got, *creates)
	}
}

// A sign-in read that fails, or a groups read that fails with anything but not-found, books nothing and pauses
// nothing: the schedule waits for the next tick.
func TestRunScheduleSweep_StoreErrorsLeaveItForNextTick(t *testing.T) {
	t.Setenv(envScheduledRuns, "1")
	for name, fault := range map[string]func(*fakeScheduleStore){
		"sign-in read": func(f *fakeScheduleStore) { f.signInErr = errors.New("postgres is down") },
		"groups read":  func(f *fakeScheduleStore) { f.groupsErr = errors.New("postgres is down") },
	} {
		now := time.Now()
		st := newFakeScheduleStore()
		sc := st.seed(now.Add(time.Minute), false)
		fault(st)
		sw, creates := testSweep(scheduleTestServer(st), st, now, answers(http.StatusServiceUnavailable), true)
		if err := sw.run(context.Background()); err != nil {
			t.Fatal(err)
		}
		if got := st.row(t, sc.ID); *creates != 0 || got.PausedAt != nil || !got.NextAt.Equal(sc.NextAt) {
			t.Errorf("%s fault: %d creates, schedule %+v; want no create, no pause, next_at unchanged", name, *creates, got)
		}
	}
}

// A missed slot whose owner is no longer live pauses identity_inactive before any replay.
func TestRunScheduleSweep_MissedNotLivePausesBeforeReplay(t *testing.T) {
	t.Setenv(envScheduledRuns, "1")
	now := time.Now()
	st := newFakeScheduleStore()
	sc := st.seed(now.Add(-time.Hour), false)
	sw, creates := testSweep(scheduleTestServer(st), st, now, answers(http.StatusCreated), false)
	if err := sw.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := st.row(t, sc.ID); *creates != 0 || got.PausedAt == nil || got.PausedReason != schedulePausedIdentity {
		t.Errorf("%d creates, schedule %+v; want paused %s and the create never called", *creates, got, schedulePausedIdentity)
	}
}
