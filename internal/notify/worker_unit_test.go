// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package notify

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// downPool is a pool whose Postgres refuses every connection: pgxpool dials lazily, so it
// builds, and every statement then fails at once and deterministically.
func downPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), "postgres://wardyn@127.0.0.1:1/wardyn?connect_timeout=1&sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// keepRecorder keeps every audit row it is given, or refuses them all when err is set.
type keepRecorder struct {
	mu   sync.Mutex
	rows []types.AuditEvent
	err  error
}

func (r *keepRecorder) Record(_ context.Context, ev types.AuditEvent) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rows = append(r.rows, ev)
	return r.err
}

func (r *keepRecorder) actions() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, ev := range r.rows {
		out = append(out, ev.Action)
	}
	return out
}

func routedConfig(t *testing.T, tiers string) (*Config, uuid.UUID) {
	t.Helper()
	profile := uuid.New()
	c, err := Parse(`{` + routeChannels + `,"routes":[{"kinds":["egress_domain"],"profiles":["` + profile.String() + `"],"tiers":` + tiers + `}]}`)
	if err != nil {
		t.Fatalf("config refused: %v", err)
	}
	return c, profile
}

func TestNewWorkerIndexesChannelsAndDefaultsTheClient(t *testing.T) {
	c, _ := routedConfig(t, `[`+tierJSON("0s", `"a"`, "")+`]`)

	w := NewWorker(Deps{Config: c})
	if w.client == nil {
		t.Error("no client: a worker with none would send nowhere")
	}
	for _, id := range []string{"a", "b", "red"} {
		if _, ok := w.channels[id]; !ok {
			t.Errorf("channel %q is not indexed", id)
		}
	}
	mine := &http.Client{Timeout: time.Second}
	if got := NewWorker(Deps{Config: c, Client: mine}).client; got != mine {
		t.Error("the injected client was replaced")
	}
	c.ConsoleURL = "https://console.example.com"
	if got := NewWorker(Deps{Config: c}).console; got != "https://console.example.com" {
		t.Errorf("console = %q, want the config's", got)
	}
}

func TestFailedRetriesOnlyTheRetryableAndOnlyUntilTheFifthAttempt(t *testing.T) {
	w := &Worker{}
	retry := result{class: "http_status:503", retryable: true}
	for attempts, wait := range retryBackoff {
		got := w.failed(claimed{attempts: int16(attempts + 1)}, retry)
		if got.state != types.NotifyPending || got.wait != wait || got.class != retry.class {
			t.Errorf("attempt %d: %+v, want pending after %v", attempts+1, got, wait)
		}
	}
	if got := w.failed(claimed{attempts: maxAttempts}, retry); got.state != types.NotifyDead || got.class != retry.class || got.wait != 0 {
		t.Errorf("the fifth failure = %+v, want dead with no wait", got)
	}
	if got := w.failed(claimed{attempts: 1}, result{class: "http_status:400"}); got.state != types.NotifyDead {
		t.Errorf("a non-retryable failure = %+v, want dead on the first attempt", got)
	}
}

func TestRecipientsResolveTheTiersTargetsAndSkipWhatHasNoAddress(t *testing.T) {
	c, profile := routedConfig(t, `[`+
		tierJSON("0s", `"a"`, `,"notify":["run_owner","profile_contact"]`)+`,`+
		tierJSON("1m", `"b"`, `,"notify":["profile_contact"]`)+`]`)
	w := &Worker{cfg: c}
	facts := approvalFacts{Kind: types.ApprovalEgressDomain, ProfileID: &profile, ProfileName: "interns", Email: "owner@corp.example"}
	contact := []byte(`{"owner":"Platform team","email":"platform@corp.example"}`)

	got := w.recipients(claimed{tier: 0, channel: "a"}, facts, contact)
	want := []recipient{{Role: TargetRunOwner, Email: "owner@corp.example"}, {Role: TargetProfileContact, Email: "platform@corp.example"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("tier 0 = %+v, want %+v", got, want)
	}

	// Only the tier's own channels resolve, and only tiers the route has.
	if got := w.recipients(claimed{tier: 0, channel: "b"}, facts, contact); got != nil {
		t.Errorf("a channel outside the tier resolved %+v", got)
	}
	if got := w.recipients(claimed{tier: 2, channel: "a"}, facts, contact); got != nil {
		t.Errorf("a tier past the route resolved %+v", got)
	}
	// No route matches an approval of another profile or kind: the fixed config answers no one.
	other := uuid.New()
	if got := w.recipients(claimed{tier: 0, channel: "a"}, approvalFacts{Kind: facts.Kind, ProfileID: &other}, contact); got != nil {
		t.Errorf("an unrouted profile resolved %+v", got)
	}

	// What is stored may no longer pass validation: it is dropped, never sent as is.
	for name, stored := range map[string][]byte{
		"no contact":              nil,
		"malformed JSON":          []byte(`{"email":`),
		"an invalid email":        []byte(`{"email":"not-an-email"}`),
		"a contact with no email": []byte(`{"owner":"Platform team"}`),
	} {
		got := w.recipients(claimed{tier: 1, channel: "b"}, facts, stored)
		if len(got) != 0 {
			t.Errorf("%s: resolved %+v, want no recipient", name, got)
		}
	}
	noOwner := facts
	noOwner.Email = ""
	if got := w.recipients(claimed{tier: 0, channel: "a"}, noOwner, nil); len(got) != 0 {
		t.Errorf("a run owner with no email resolved %+v", got)
	}
}

func TestWorkerOnAPostgresThatDoesNotAnswer(t *testing.T) {
	c, _ := routedConfig(t, `[`+tierJSON("0s", `"a"`, "")+`]`)
	w := NewWorker(Deps{Pool: downPool(t), Config: c})

	// Nothing is claimed, so nothing is delivered and nothing is invented.
	if n, err := w.Tick(context.Background()); err == nil || n != 0 {
		t.Errorf("Tick = %d, %v; want 0 and an error", n, err)
	}

	// A row whose facts cannot be read moves no failure counter: its outcome is only counted once
	// the lease update lands, and on a store outage that update cannot land either.
	before := Snapshot().Failed["a"]
	w.process(context.Background(), claimed{id: uuid.New(), approvalID: uuid.New(), channel: "a", attempts: 1, lease: time.Now()})
	if after := Snapshot().Failed["a"]; after != before {
		t.Errorf("failed counter moved %d -> %d on a store outage", before, after)
	}

	// Run returns once its context ends, even with the store down.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	go func() { w.Run(ctx); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after its context ended")
	}
}

func TestEnqueueCarriesThePlanAsThreeParallelArrays(t *testing.T) {
	c, profile := routedConfig(t, `[`+tierJSON("0s", `"a","b"`, "")+`,`+tierJSON("90s", `"red"`, "")+`]`)
	SetActive(c, nil)
	t.Cleanup(func() { SetActive(nil, nil) })

	e := NewEnqueue(types.ApprovalEgressDomain, &profile)
	if !e.On() {
		t.Fatal("a matching route did not turn the outbox insert on")
	}
	args := e.Args()
	want := []any{[]int16{0, 0, 1}, []string{"a", "b", "red"}, []int32{0, 0, 90}}
	if !reflect.DeepEqual(args, want) {
		t.Errorf("Args = %#v, want %#v", args, want)
	}
	tail := e.Tail(4)
	for _, p := range []string{"$4::smallint[]", "$5::text[]", "$6::int[]"} {
		if !strings.Contains(tail, p) {
			t.Errorf("Tail(4) does not number its parameters: missing %s", p)
		}
	}

	if off := NewEnqueue(types.ApprovalEgressDomain, nil); off.On() {
		t.Error("an approval no route matches planned rows")
	}
}

func TestDoneAuditsASuppressedRaiseOncePerRunAndCountsEveryChannel(t *testing.T) {
	c, profile := routedConfig(t, `[`+tierJSON("0s", `"a","b"`, "")+`,`+tierJSON("1m", `"red"`, "")+`]`)
	rec := &keepRecorder{}
	SetActive(c, rec)
	t.Cleanup(func() { SetActive(nil, nil) })
	e := NewEnqueue(types.ApprovalEgressDomain, &profile)
	run, approval := uuid.New(), uuid.New()
	before := Snapshot().Suppressed

	// Raised nothing, or queued something: not suppressed.
	e.Done(context.Background(), approval, run, 0, 0)
	e.Done(context.Background(), approval, run, 1, 3)
	if got := rec.actions(); len(got) != 0 {
		t.Fatalf("audit rows for raises that were not suppressed: %v", got)
	}

	e.Done(context.Background(), approval, run, 1, 0)
	e.Done(context.Background(), uuid.New(), run, 1, 0) // same run, same hour
	if got := rec.actions(); len(got) != 1 || got[0] != "approval.notify.suppressed" {
		t.Fatalf("audit rows = %v, want exactly one approval.notify.suppressed for the run", got)
	}
	var data map[string]any
	if err := json.Unmarshal(rec.rows[0].Data, &data); err != nil {
		t.Fatal(err)
	}
	if data["approval_id"] != approval.String() || data["limit"] != float64(RunBudget) || rec.rows[0].RunID == nil || *rec.rows[0].RunID != run {
		t.Errorf("row = %+v data %v, want the first suppressed approval, the budget and the run", rec.rows[0], data)
	}
	// Only tier 0 rows are the ones the budget bounds; each raise counted each of them.
	after := Snapshot().Suppressed
	for ch, want := range map[string]int64{"a": 2, "b": 2, "red": 0} {
		if got := after[ch] - before[ch]; got != want {
			t.Errorf("suppressed[%s] moved by %d, want %d", ch, got, want)
		}
	}
	// A different run has its own hour.
	e.Done(context.Background(), uuid.New(), uuid.New(), 1, 0)
	if got := rec.actions(); len(got) != 2 {
		t.Errorf("audit rows = %v, want a second row for another run", got)
	}
}

func TestEmitIsSilentWithoutARecorderAndSurvivesARefusingOne(t *testing.T) {
	ev := types.AuditEvent{ID: uuid.New(), Action: "approval.notify.failed"}

	SetActive(nil, nil)
	emit(context.Background(), ev) // no recorder: nothing to write to, nothing to fail

	rec := &keepRecorder{err: errors.New("sink down")}
	SetActive(nil, rec)
	t.Cleanup(func() { SetActive(nil, nil) })
	emit(context.Background(), ev) // a refusing sink is logged loudly, never a panic
	if got := rec.actions(); len(got) != 1 {
		t.Errorf("the refusing recorder saw %v, want the one row offered", got)
	}
}

func TestHourlyForgetsARunAfterAnHour(t *testing.T) {
	h := &hourly{last: map[uuid.UUID]time.Time{}}
	run, other := uuid.New(), uuid.New()
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	if !h.first(run, t0) || !h.first(other, t0) {
		t.Fatal("the first raise of a run was not admitted")
	}
	if h.first(run, t0.Add(59*time.Minute)) {
		t.Error("a second row inside the hour was admitted")
	}
	if !h.first(run, t0.Add(time.Hour)) {
		t.Error("a row an hour later was refused: the window never reopens")
	}
	// The write for run also prunes other, whose hour has passed and which was never raised again.
	if _, kept := h.last[other]; kept || len(h.last) != 1 {
		t.Errorf("tracked runs = %v, want only %s: the stale entry for another run pruned on write", h.last, run)
	}
}
