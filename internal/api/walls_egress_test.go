// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// wallsEgressStore holds runs' policy rows and generations, read the way Postgres serves both readers: the
// effective-policy reader's filtered page (newest first when asked, cut at the limit) and the batch envelope
// read (each resolve spelling's newest rows, and the current generation's newest rows from generation 2).
// Methods neither reader calls are the nil embedded ones.
type wallsEgressStore struct {
	store.Store
	store.Pager
	store.RunPolicyGenerations
	rows   map[uuid.UUID][]types.AuditEvent // oldest first, as seq orders them
	states map[uuid.UUID]types.RunPolicyState
	err    error // fails RunPolicyEnvelopes
	reads  int   // RunPolicyEnvelopes calls
}

func (s *wallsEgressStore) QueryAuditEventsFilteredPage(_ context.Context, runID *uuid.UUID, f store.AuditFilter, p store.Page) ([]types.AuditEvent, error) {
	out := f.Keep(slices.Clone(s.rows[*runID]))
	if p.Newest {
		slices.Reverse(out)
	}
	if p.Limit > 0 && len(out) > p.Limit {
		out = out[:p.Limit]
	}
	return out, nil
}

func (s *wallsEgressStore) RunPolicyState(_ context.Context, id uuid.UUID) (types.RunPolicyState, bool, error) {
	st, ok := s.states[id]
	return st, ok, nil
}

func (s *wallsEgressStore) RunPolicyEnvelopes(_ context.Context, ids []uuid.UUID, resolve []string) (map[uuid.UUID]store.RunPolicyEnvelope, error) {
	s.reads++
	if s.err != nil {
		return nil, s.err
	}
	out := map[uuid.UUID]store.RunPolicyEnvelope{}
	for _, id := range ids {
		st, ok := s.states[id]
		if !ok {
			continue
		}
		env := store.RunPolicyEnvelope{State: st, Resolve: s.newest(id, resolve, "")}
		if n := st.CurrentGeneration(); n >= 2 {
			env.Generation = s.newest(id, []string{"run.policy.generation"}, fmt.Sprintf(`{"generation":%d}`, n))
		}
		out[id] = env
	}
	return out, nil
}

// newest is each of actions' newest five rows of run whose data contains contains, oldest first.
func (s *wallsEgressStore) newest(run uuid.UUID, actions []string, contains string) []types.AuditEvent {
	var out []types.AuditEvent
	kept := map[string]int{}
	rows := s.rows[run]
	for i := len(rows) - 1; i >= 0; i-- {
		ev := rows[i]
		if slices.Contains(actions, ev.Action) && (store.AuditFilter{DataContains: contains}).Matches(ev) && kept[ev.Action] < 5 {
			kept[ev.Action]++
			out = append(out, ev)
		}
	}
	slices.Reverse(out)
	return out
}

// wallsEgressCase is one fixture run and the door rule it has.
type wallsEgressCase struct {
	name string
	run  types.AgentRun
	want string
}

// wallsEgressFixture is the store and runs of the door rule's cases: a No network run with no rows, an
// allow-all run, an allowlist run, a generation-3 run whose generation row turns allow-all on over an allowlist
// resolve, a generation-3 run whose generation row cannot be read, and a run with no resolve row.
func wallsEgressFixture(t *testing.T) (*wallsEgressStore, []wallsEgressCase) {
	t.Helper()
	st := &wallsEgressStore{rows: map[uuid.UUID][]types.AuditEvent{}, states: map[uuid.UUID]types.RunPolicyState{}}
	base := time.Unix(0, 0).UTC()
	gen3 := types.RunPolicyState{Generations: []types.PolicyGeneration{{Generation: 1}, {Generation: 2}, {Generation: 3}}}
	row := func(run uuid.UUID, action string, data any) {
		raw, err := json.Marshal(data)
		if err != nil {
			t.Fatal(err)
		}
		n := len(st.rows[run])
		st.rows[run] = append(st.rows[run], types.AuditEvent{ID: uuid.New(), Time: base.Add(time.Duration(n) * time.Second), RunID: &run,
			Action: action, Data: raw})
	}
	resolve := func(allowAll bool) effectivePolicyDatum {
		return effectivePolicyDatum{RunPolicySpec: types.RunPolicySpec{AllowedDomains: []string{"pypi.org"}, AllowAllEgress: allowAll}}
	}
	mk := func(name, want string, state types.RunPolicyState) wallsEgressCase {
		c := wallsEgressCase{name: name, run: types.AgentRun{ID: uuid.New(), State: types.RunRunning}, want: want}
		st.states[c.run.ID] = state
		return c
	}
	sealed := mk("no network", wallsEgressSealed, types.RunPolicyState{})
	sealed.run.WallPreset = &types.WallPresetOnRun{ID: types.WallPresetNoNetwork, Scope: types.WallPresetScopeBuiltin}
	open := mk("allow-all", wallsEgressOpen, types.RunPolicyState{})
	row(open.run.ID, "run.policy.resolve", resolve(false))
	row(open.run.ID, "run.policy.resolve", resolve(true))
	allowlist := mk("allowlist", wallsEgressAllowlist, types.RunPolicyState{})
	row(allowlist.run.ID, "run.policy.resolve", resolve(false))
	turned := mk("generation 3 turns allow-all on", wallsEgressOpen, gen3)
	row(turned.run.ID, "run.policy.resolve", resolve(false))
	row(turned.run.ID, "run.policy.generation", map[string]any{"generation": 2, "policy": types.RunPolicySpec{AllowedDomains: []string{"pypi.org"}}})
	row(turned.run.ID, "run.policy.generation", map[string]any{"generation": 3, "policy": types.RunPolicySpec{AllowAllEgress: true}})
	unread := mk("unreadable generation row", "", gen3)
	row(unread.run.ID, "run.policy.resolve", resolve(true))
	row(unread.run.ID, "run.policy.generation", map[string]any{"generation": 3, "policy": "allow everything"})
	bare := mk("no resolve row", "", types.RunPolicyState{})
	row(bare.run.ID, "run.create", map[string]any{"agent": "shell"})
	return st, []wallsEgressCase{sealed, open, allowlist, turned, unread, bare}
}

// TestWallsEgressRule: a No network run is sealed with no envelope; allow-all is open, otherwise allowlist; a
// generation-3 run whose generation row turns allow-all on is open; an unreadable generation row omits egress,
// and so does a run with no resolve row. wallsEgressOf serves the same rule for a page in one read, and a
// failed read leaves every run but the sealed one out.
func TestWallsEgressRule(t *testing.T) {
	st, cases := wallsEgressFixture(t)
	ctx := context.Background()
	if got := wallsRunEgress(cases[0].run, store.RunPolicyEnvelope{}); got != wallsEgressSealed {
		t.Errorf("a No network run with no envelope = %q, want %q", got, wallsEgressSealed)
	}
	runs := make([]types.AgentRun, len(cases))
	for i, c := range cases {
		runs[i] = c.run
		envs, err := st.RunPolicyEnvelopes(ctx, []uuid.UUID{c.run.ID}, spellingsOf("run.policy.resolve"))
		if err != nil {
			t.Fatal(err)
		}
		if got := wallsRunEgress(c.run, envs[c.run.ID]); got != c.want {
			t.Errorf("%s: wallsRunEgress = %q, want %q", c.name, got, c.want)
		}
	}
	s := &Server{cfg: Config{Store: st}}
	st.reads = 0
	doors := s.wallsEgressOf(ctx, append(runs, runs[1]))
	if st.reads != 1 {
		t.Errorf("one page made %d envelope reads, want 1", st.reads)
	}
	for _, c := range cases {
		got, has := doors[c.run.ID]
		if got != c.want || has != (c.want != "") {
			t.Errorf("%s: wallsEgressOf = %q (present %v), want %q", c.name, got, has, c.want)
		}
	}
	st.err = errors.New("read refused")
	doors = s.wallsEgressOf(ctx, runs)
	if len(doors) != 1 || doors[cases[0].run.ID] != wallsEgressSealed {
		t.Errorf("a failed read = %v, want only the No network run, sealed", doors)
	}
	if doors := (&Server{cfg: Config{Store: &capStore{}}}).wallsEgressOf(ctx, runs[1:]); len(doors) != 0 {
		t.Errorf("a store without the envelope reader = %v, want no egress", doors)
	}
}

// TestWallsEgressMatchesRunEffectivePolicy: over the same fixtures, wallsRunEgress equals the rule applied to
// runEffectivePolicy's envelope for each run: sealed first, "" when the reader has no envelope, else open or
// allowlist.
func TestWallsEgressMatchesRunEffectivePolicy(t *testing.T) {
	st, cases := wallsEgressFixture(t)
	ctx := context.Background()
	s := &Server{cfg: Config{Store: st}}
	for _, c := range cases {
		envelope, _, ok, err := s.runEffectivePolicy(ctx, c.run.ID)
		if err != nil {
			t.Fatalf("%s: runEffectivePolicy: %v", c.name, err)
		}
		want := ""
		switch {
		case noNetworkPreset(c.run.WallPreset):
			want = wallsEgressSealed
		case !ok:
		case envelope.AllowAllEgress:
			want = wallsEgressOpen
		default:
			want = wallsEgressAllowlist
		}
		envs, err := st.RunPolicyEnvelopes(ctx, []uuid.UUID{c.run.ID}, spellingsOf("run.policy.resolve"))
		if err != nil {
			t.Fatal(err)
		}
		if got := wallsRunEgress(c.run, envs[c.run.ID]); got != want {
			t.Errorf("%s: wallsRunEgress = %q, runEffectivePolicy's rule = %q", c.name, got, want)
		}
	}
}
