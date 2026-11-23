// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// asValueStore serves a run's audit rows oldest first, as QueryAuditEvents does; it is no store.Pager.
type asValueStore struct {
	store.Store
	rows []types.AuditEvent
}

func (s *asValueStore) QueryAuditEvents(context.Context, uuid.UUID, int) ([]types.AuditEvent, error) {
	return slices.Clone(s.rows), nil
}

// asValuePagedStore is the Pager arm: a filtered read the way Postgres serves it, newest first when asked.
type asValuePagedStore struct {
	asValueStore
	store.Pager
}

func (s *asValuePagedStore) QueryAuditEventsFilteredPage(_ context.Context, _ *uuid.UUID, f store.AuditFilter, p store.Page) ([]types.AuditEvent, error) {
	out := f.Keep(slices.Clone(s.rows))
	if p.Newest {
		slices.Reverse(out)
	}
	if p.Limit > 0 && len(out) > p.Limit {
		out = out[:p.Limit]
	}
	return out, nil
}

var asValueEpoch = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

// asValueRow is a run.secret_placeholder.resolve row, n seconds after the epoch.
func asValueRow(n int, outcome string, data map[string]any) types.AuditEvent {
	return types.AuditEvent{
		ID: uuid.New(), Time: asValueEpoch.Add(time.Duration(n) * time.Second), Action: "run.secret_placeholder.resolve",
		Outcome: outcome, Data: mustJSON(data),
	}
}

func asValueFailure(n int, name, host, cause string) types.AuditEvent {
	return asValueRow(n, "failure", map[string]any{
		"kind": types.GrantEnvSecret, "secret_name": name, "host": host, "cause": cause, "reason": placeholderCauseSentence[cause],
	})
}

func asValueSuccess(n int, name, host string) types.AuditEvent {
	return asValueRow(n, "success", map[string]any{"kind": types.GrantEnvSecret, "secret_name": name, "host": host})
}

func asValueServer(st store.Store) *Server { return &Server{cfg: Config{Store: st}} }

func TestRunSecretsAsValue_DedupesNewestFirst(t *testing.T) {
	rows := []types.AuditEvent{
		asValueFailure(2, "deploy", "b.test", placeholderCauseNotAllowed),
		asValueFailure(4, "deploy", "b.test", placeholderCauseHostTaken),
		asValueFailure(6, "alpha", "z.test", placeholderCauseMultiline),
		asValueFailure(8, "alpha", "a.test", placeholderCauseMultiline),
	}
	got, truncated, err := asValueServer(&asValueStore{rows: rows}).runSecretsAsValue(t.Context(), uuid.New(), specReadFull)
	if err != nil || truncated {
		t.Fatalf("err %v, truncated %v", err, truncated)
	}
	var hosts, causes []string
	for _, r := range got {
		hosts, causes = append(hosts, r.Host), append(causes, r.Cause)
	}
	if want := []string{"a.test", "b.test", "z.test"}; !slices.Equal(hosts, want) {
		t.Errorf("hosts %v, want %v (one entry per secret, sorted by kind, host, name)", hosts, want)
	}
	if causes[1] != placeholderCauseHostTaken {
		t.Errorf("deploy@b.test cause %q, want the newest row's %q", causes[1], placeholderCauseHostTaken)
	}
}

func TestRunSecretsAsValue_NameHiddenForMember(t *testing.T) {
	st := &asValueStore{rows: []types.AuditEvent{asValueFailure(2, "deploy", "a.test", placeholderCauseNotAllowed)}}
	for tier, wantName := range map[specReadTier]string{specReadMember: "", specReadSecurity: "deploy", specReadFull: "deploy"} {
		got, _, err := asValueServer(st).runSecretsAsValue(t.Context(), uuid.New(), tier)
		if err != nil || len(got) != 1 {
			t.Fatalf("tier %v: %v, %v", tier, got, err)
		}
		if got[0].SecretName != wantName || got[0].Host != "a.test" {
			t.Errorf("tier %v: %+v, want name %q", tier, got[0], wantName)
		}
		if b, _ := json.Marshal(got[0]); tier == specReadMember && string(b) != `{"kind":"env_secret","host":"a.test","cause":"host_not_allowlisted","reason":"the host is not an exact entry of the run's allowlist"}` {
			t.Errorf("member wire form %s carries a name", b)
		}
	}
}

// A row written before causes has none: the cause is found from its sentence, and stays empty for a sentence it does not know.
func TestRunSecretsAsValue_LegacyRowGetsCauseFromReason(t *testing.T) {
	legacy := asValueRow(2, "failure", map[string]any{"kind": "env_secret", "secret_name": "old", "host": "a.test", "reason": placeholderCauseSentence[placeholderCauseUnrecorded]})
	unknown := asValueRow(4, "failure", map[string]any{"kind": "env_secret", "secret_name": "odd", "host": "b.test", "reason": "a sentence nobody wrote"})
	got, _, err := asValueServer(&asValueStore{rows: []types.AuditEvent{legacy, unknown}}).runSecretsAsValue(t.Context(), uuid.New(), specReadFull)
	if err != nil || len(got) != 2 {
		t.Fatalf("%v, %v", got, err)
	}
	if got[0].Cause != placeholderCauseUnrecorded || got[1].Cause != "" || got[1].Reason != "a sentence nobody wrote" {
		t.Errorf("entries %+v", got)
	}
}

// The newest row of a secret decides. The rule is pinned for a run dispatched twice; no revive writes such a row,
// since a revive places nothing.
func TestRunSecretsAsValue_LaterSuccessClears(t *testing.T) {
	for name, tc := range map[string]struct {
		rows []types.AuditEvent
		want int
	}{
		"failure then success": {[]types.AuditEvent{asValueFailure(2, "deploy", "a.test", placeholderCauseNotAllowed), asValueSuccess(4, "deploy", "a.test")}, 0},
		"success then failure": {[]types.AuditEvent{asValueSuccess(2, "deploy", "a.test"), asValueFailure(4, "deploy", "a.test", placeholderCauseNotAllowed)}, 1},
	} {
		for _, paged := range []bool{false, true} {
			var st store.Store = &asValueStore{rows: tc.rows}
			if paged {
				st = &asValuePagedStore{asValueStore: asValueStore{rows: tc.rows}}
			}
			got, _, err := asValueServer(st).runSecretsAsValue(t.Context(), uuid.New(), specReadFull)
			if err != nil || len(got) != tc.want {
				t.Errorf("%s (pager %v): %v, %v; want %d entries", name, paged, got, err, tc.want)
			}
		}
	}
}

// A store that is no Pager returns rows oldest first; the newest row still decides.
func TestRunSecretsAsValue_OldestFirstFallbackKeepsNewest(t *testing.T) {
	st := &asValueStore{rows: []types.AuditEvent{
		asValueFailure(2, "deploy", "a.test", placeholderCauseNotAllowed),
		asValueFailure(4, "deploy", "a.test", placeholderCauseHostTaken),
	}}
	got, _, err := asValueServer(st).runSecretsAsValue(t.Context(), uuid.New(), specReadFull)
	if err != nil || len(got) != 1 || got[0].Cause != placeholderCauseHostTaken {
		t.Errorf("%+v, %v; want the newest row's cause %q", got, err, placeholderCauseHostTaken)
	}
}

func TestRunSecretsAsValue_CapsAtFiftyAndSaysSo(t *testing.T) {
	for _, tc := range []struct {
		n         int
		truncated bool
	}{{50, false}, {51, true}} {
		var rows []types.AuditEvent
		for i := 0; i < tc.n; i++ {
			rows = append(rows, asValueFailure(i+1, fmt.Sprintf("s%02d", i), "a.test", placeholderCauseNotAllowed))
		}
		got, truncated, err := asValueServer(&asValueStore{rows: rows}).runSecretsAsValue(t.Context(), uuid.New(), specReadFull)
		if err != nil || len(got) != 50 || truncated != tc.truncated {
			t.Errorf("%d rows: %d entries, truncated %v, err %v; want 50 entries and truncated %v", tc.n, len(got), truncated, err, tc.truncated)
		}
	}
}
