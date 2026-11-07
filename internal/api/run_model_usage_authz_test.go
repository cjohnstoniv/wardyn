// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// usageStore is an authzStore that also keeps model usage rows in memory (store.ModelUsageStore).
type usageStore struct {
	*authzStore
	rows    map[uuid.UUID][]egress.ModelUsage // by run id, in arrival order
	tunnels int64
	asked   []uuid.UUID // the run ids RunModelUsage was asked for
}

func (s *usageStore) InsertModelUsage(_ context.Context, runID uuid.UUID, u egress.ModelUsage, _ time.Time, _ string) (bool, error) {
	s.authzStore.mu.Lock()
	defer s.authzStore.mu.Unlock()
	for _, r := range s.rows {
		for _, old := range r {
			if old.CallID == u.CallID {
				return false, nil
			}
		}
	}
	s.rows[runID] = append(s.rows[runID], u)
	return true, nil
}

func (s *usageStore) RunModelUsage(_ context.Context, runID uuid.UUID) (store.ModelUsageSummary, error) {
	s.authzStore.mu.Lock()
	defer s.authzStore.mu.Unlock()
	s.asked = append(s.asked, runID)
	var out store.ModelUsageSummary
	for _, u := range s.rows[runID] {
		i := -1
		for j, g := range out.Groups {
			if g.Vendor == u.Vendor && g.Host == u.Host && g.Model == u.Model {
				i = j
			}
		}
		if i < 0 {
			out.Groups = append(out.Groups, store.ModelUsageGroup{Vendor: u.Vendor, Host: u.Host, Model: u.Model})
			i = len(out.Groups) - 1
		}
		g := &out.Groups[i]
		g.Input, g.Output, g.CacheRead, g.CacheWrite, g.Reasoning = g.Input+u.Input, g.Output+u.Output, g.CacheRead+u.CacheRead, g.CacheWrite+u.CacheWrite, g.Reasoning+u.Reasoning
		g.Calls++
		switch u.State {
		case "uncounted":
			g.UncountedCalls++
			if g.UncountedReasons == nil {
				g.UncountedReasons = map[string]int64{}
			}
			g.UncountedReasons[u.Reason]++
		case "partial":
			g.PartialCalls++
		}
		t := time.Unix(1_700_000_000, 0).UTC()
		out.UpdatedAt = &t
	}
	return out, nil
}

func (s *usageStore) RunOpaqueTunnels(context.Context, uuid.UUID) (int64, error) {
	return s.tunnels, nil
}

func newUsageHarness(t *testing.T, shape ...func(*Config)) (*Server, *usageStore, *harness) {
	t.Helper()
	ast := newAuthzStore()
	us := &usageStore{authzStore: ast, rows: map[uuid.UUID][]egress.ModelUsage{}}
	h := newHarness(t)
	cfg := baseTestConfig(h, us)
	cfg.OIDC = &oidc.Authenticator{}
	for _, f := range shape {
		f(&cfg)
	}
	return New(cfg), us, h
}

func seedUsageRun(us *usageStore, createdBy string, provider string) uuid.UUID {
	return seedUsageRunIn(us, createdBy, provider, types.RunCompleted)
}

func seedUsageRunIn(us *usageStore, createdBy, provider string, state types.RunState) uuid.UUID {
	id := uuid.New()
	us.authzStore.mu.Lock()
	us.authzStore.runs[id] = types.AgentRun{ID: id, CreatedBy: createdBy, State: state, ModelProviderID: provider}
	us.authzStore.mu.Unlock()
	return id
}

// Who may read a run's token usage: its owner, and nobody else's member. A foreign run answers the same
// 404 a missing run does, and the store is never asked for the foreign run's rows.
func TestRunModelUsage_OwnerReadsForeignMemberGets404(t *testing.T) {
	srv, us, _ := newUsageHarness(t)
	id := seedUsageRun(us, "owner-sub", "")
	if _, err := us.InsertModelUsage(context.Background(), id, egress.ModelUsage{CallID: uuid.New(), Vendor: "anthropic", Host: "api.anthropic.com", Model: "claude-x", Input: 10, Output: 5, State: "counted"}, time.Now(), ""); err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/runs/" + id.String() + "/model-usage"

	mallory := ssoSession(t, "mallory-sub", "mallory@corp.example", oidc.RoleUser)
	w := doSSO(t, srv, http.MethodGet, path, mallory, "")
	if w.Code != http.StatusNotFound {
		t.Fatalf("non-owning member: code = %d, want 404; body=%s", w.Code, w.Body.String())
	}
	missing := doSSO(t, srv, http.MethodGet, "/api/v1/runs/"+uuid.NewString()+"/model-usage", mallory, "")
	if missing.Code != http.StatusNotFound || missing.Body.String() != w.Body.String() {
		t.Fatalf("a foreign run must answer as a missing run does: %d %q vs %d %q", w.Code, w.Body.String(), missing.Code, missing.Body.String())
	}
	if len(us.asked) != 0 {
		t.Fatalf("the store was read for a run the caller does not own: %v", us.asked)
	}

	owner := ssoSession(t, "owner-sub", "owner@corp.example", oidc.RoleUser)
	w = doSSO(t, srv, http.MethodGet, path, owner, "")
	if w.Code != http.StatusOK {
		t.Fatalf("owning member: code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var got struct {
		Totals struct {
			Input  int64 `json:"input_tokens"`
			Output int64 `json:"output_tokens"`
			Calls  int64 `json:"calls"`
		} `json:"totals"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || got.Totals.Input != 10 || got.Totals.Output != 5 || got.Totals.Calls != 1 {
		t.Fatalf("owner's totals: %+v %v (%s)", got, err, w.Body.String())
	}
}
