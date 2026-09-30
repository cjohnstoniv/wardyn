// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// runPortalStore is a run store that also holds the portal registry.
type runPortalStore struct {
	runTypeStore
	*fakeDelegateStore
	listErr error
}

func (s runPortalStore) ListDelegates(ctx context.Context) ([]types.Delegate, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	return s.fakeDelegateStore.ListDelegates(ctx)
}

// ListRuns answers the unpaged admin listing from the runs putRun stored.
func (s runPortalStore) ListRuns(context.Context) ([]types.AgentRun, error) {
	s.sshMemStore.mu.Lock()
	defer s.sshMemStore.mu.Unlock()
	out := []types.AgentRun{}
	for _, r := range s.runs {
		out = append(out, r)
	}
	return out, nil
}

// #1234: GET /runs/{id} names the portal a run was launched through, for the
// run's own reader, so the run page can say "Launched via {portal}". A revoked
// portal keeps its name; one the registry lacks, or a registry that cannot be
// read, names nothing; and the list carries the id alone, as it always did.
func TestGetRun_NamesThePortalThatLaunchedIt(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name     string
		revoke   bool
		unknown  bool
		selfRun  bool
		listErr  error
		wantName any
	}{
		{name: "a live portal: its name", wantName: "Acme Support Portal"},
		{name: "a revoked portal: still its name", revoke: true, wantName: "Acme Support Portal"},
		{name: "a portal the registry does not hold: nothing", unknown: true},
		{name: "the registry is unreadable: nothing", listErr: errors.New("db down")},
		{name: "a self-launched run: nothing", selfRun: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := runPortalStore{runTypeStore: runTypeStore{newUIMemStore()}, fakeDelegateStore: newFakeDelegateStore(), listErr: tc.listErr}
			d, err := st.CreateDelegate(ctx, types.Delegate{ID: uuid.New(), Name: "Acme Support Portal", IdPClientID: "portal-client", Group: "portal-users"},
				newBearer(delegateCredentialPrefix))
			if err != nil {
				t.Fatal(err)
			}
			if tc.revoke {
				if _, err := st.RevokeDelegate(ctx, d.ID, time.Now()); err != nil {
					t.Fatal(err)
				}
			}
			cfg := baseTestConfig(newHarness(t), st)
			cfg.OIDC = newAccessAuth(t, nil, "", nil, nil)
			srv := New(cfg)
			owner := accessSessionOfType(t, "sub-pm", "pat@corp.example", oidc.RoleUser, "portfolio-manager", []string{})
			run := types.AgentRun{ID: uuid.New(), CreatedBy: "sub-pm", State: types.RunRunning}
			if !tc.selfRun {
				via := d.ID
				if tc.unknown {
					via = uuid.New()
				}
				run.CreatedVia = &via
			}
			st.putRun(run)

			w := doSSO(t, srv, http.MethodGet, "/api/v1/runs/"+run.ID.String(), owner, "")
			if w.Code != http.StatusOK {
				t.Fatalf("GET run = %d; body=%s", w.Code, w.Body.String())
			}
			var one map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &one); err != nil {
				t.Fatal(err)
			}
			if one["created_via_name"] != tc.wantName {
				t.Errorf("created_via_name = %v, want %v", one["created_via_name"], tc.wantName)
			}

			// The list and the single read agree on the portal's id; only the
			// detail read resolves its name.
			admin := accessSession(t, "sub-admin", "admin@corp.example", oidc.RoleAdmin, nil)
			lw := doSSO(t, srv, http.MethodGet, "/api/v1/runs", admin, "")
			if lw.Code != http.StatusOK {
				t.Fatalf("GET runs = %d; body=%s", lw.Code, lw.Body.String())
			}
			var list []map[string]any
			if err := json.Unmarshal(lw.Body.Bytes(), &list); err != nil {
				t.Fatal(err)
			}
			if len(list) != 1 {
				t.Fatalf("list = %d runs, want 1", len(list))
			}
			if list[0]["created_via"] != one["created_via"] {
				t.Errorf("list created_via = %v, single = %v", list[0]["created_via"], one["created_via"])
			}
			if _, named := list[0]["created_via_name"]; named {
				t.Error("the list carries created_via_name; it is a detail-only field")
			}
		})
	}
}
