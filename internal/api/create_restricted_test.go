// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// restrictOrderStore is a store that holds policies and records, at the moment CreatePolicy runs, whether the
// policy's id was already restricted: the order a create restricted first must show.
type restrictOrderStore struct {
	*capStore
	createErr error
	setErr    error
	calls     int
	sawLocked bool
	lastID    string
}

func (s *restrictOrderStore) CreatePolicy(_ context.Context, p types.RunPolicy) (types.RunPolicy, error) {
	s.calls++
	s.lastID = p.ID.String()
	s.sawLocked = s.restricted[capPolicy][s.lastID]
	if s.createErr != nil {
		return types.RunPolicy{}, s.createErr
	}
	return p, nil
}

func (s *restrictOrderStore) SetCapabilityRestriction(ctx context.Context, kind, value string, restricted bool, by string) error {
	if s.setErr != nil {
		return s.setErr
	}
	return s.capStore.SetCapabilityRestriction(ctx, kind, value, restricted, by)
}

const createPolicyBody = `{"name":"payments","spec":{"min_confinement_class":"CC2","first_use_approval":"always_deny"}}`

func availabilityAudits(t *testing.T, audit *recRecorder) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, ev := range audit.snapshot() {
		if ev.Action != "capability.availability.write" {
			continue
		}
		var d map[string]any
		if err := json.Unmarshal(ev.Data, &d); err != nil {
			t.Fatal(err)
		}
		out = append(out, d)
	}
	return out
}

// A policy created with ?restricted=true is restricted before CreatePolicy runs and stays so; one created
// without it is written as before; a misspelled flag is refused before anything is written.
func TestCreatePolicy_RestrictedIsRestrictedBeforeTheRowExists(t *testing.T) {
	t.Run("restricted=true", func(t *testing.T) {
		st := &restrictOrderStore{capStore: &capStore{}}
		h := newHarness(t)
		srv := New(baseTestConfig(h, st))
		w := do(t, srv, http.MethodPost, "/api/v1/policies?restricted=true", adminToken, createPolicyBody)
		if w.Code != http.StatusCreated {
			t.Fatalf("POST = %d %s", w.Code, w.Body)
		}
		var created types.RunPolicy
		if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
			t.Fatal(err)
		}
		if !st.sawLocked {
			t.Error("CreatePolicy ran before the policy was restricted")
		}
		if !st.restricted[capPolicy][created.ID.String()] {
			t.Error("the policy is not restricted after the create")
		}
		audits := availabilityAudits(t, h.audit)
		if len(audits) != 1 || audits[0]["restricted"] != true || audits[0]["value"] != created.ID.String() || audits[0]["kind"] != capPolicy {
			t.Errorf("availability audits = %v, want one restricted row for %s", audits, created.ID)
		}
	})
	t.Run("no flag", func(t *testing.T) {
		st := &restrictOrderStore{capStore: &capStore{}}
		h := newHarness(t)
		srv := New(baseTestConfig(h, st))
		if w := do(t, srv, http.MethodPost, "/api/v1/policies", adminToken, createPolicyBody); w.Code != http.StatusCreated {
			t.Fatalf("POST = %d %s", w.Code, w.Body)
		}
		if st.sawLocked || len(st.restricted[capPolicy]) != 0 {
			t.Errorf("a plain create restricted something: saw %v, rows %v", st.sawLocked, st.restricted)
		}
		if audits := availabilityAudits(t, h.audit); len(audits) != 0 {
			t.Errorf("a plain create wrote availability audits: %v", audits)
		}
	})
	t.Run("a misspelled flag", func(t *testing.T) {
		st := &restrictOrderStore{capStore: &capStore{}}
		srv := New(baseTestConfig(newHarness(t), st))
		w := do(t, srv, http.MethodPost, "/api/v1/policies?restricted=yes", adminToken, createPolicyBody)
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), reasonPolicyRequestInvalid) || !strings.Contains(w.Body.String(), "restricted") {
			t.Fatalf("POST = %d %s, want 400 %s naming restricted", w.Code, w.Body, reasonPolicyRequestInvalid)
		}
		if st.calls != 0 {
			t.Error("CreatePolicy was called")
		}
	})
}

// A create that cannot be restricted writes nothing, and one that fails after the restriction lifts it.
func TestCreatePolicy_RestrictedFailureLeavesNothing(t *testing.T) {
	t.Run("the create conflicts", func(t *testing.T) {
		st := &restrictOrderStore{capStore: &capStore{}, createErr: store.ErrConflict}
		srv := New(baseTestConfig(newHarness(t), st))
		w := do(t, srv, http.MethodPost, "/api/v1/policies?restricted=true", adminToken, createPolicyBody)
		if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), reasonPolicyNameConflict) {
			t.Fatalf("POST = %d %s, want 409 %s", w.Code, w.Body, reasonPolicyNameConflict)
		}
		if st.calls != 1 || st.restricted[capPolicy][st.lastID] {
			t.Errorf("after the conflict: calls %d, restricted %v; want 1 call and no row for %s", st.calls, st.restricted, st.lastID)
		}
	})
	t.Run("the create fails", func(t *testing.T) {
		st := &restrictOrderStore{capStore: &capStore{}, createErr: errors.New("db down")}
		srv := New(baseTestConfig(newHarness(t), st))
		if w := do(t, srv, http.MethodPost, "/api/v1/policies?restricted=true", adminToken, createPolicyBody); w.Code != http.StatusInternalServerError {
			t.Fatalf("POST = %d %s, want 500", w.Code, w.Body)
		}
		if st.restricted[capPolicy][st.lastID] {
			t.Error("the restriction stayed after a failed create")
		}
	})
	for name, st := range map[string]*restrictOrderStore{
		"the restriction write fails": {capStore: &capStore{}, setErr: errors.New("db down")},
		"the restriction read fails":  {capStore: &capStore{restrictErr: errors.New("db down")}},
	} {
		t.Run(name, func(t *testing.T) {
			srv := New(baseTestConfig(newHarness(t), st))
			if w := do(t, srv, http.MethodPost, "/api/v1/policies?restricted=true", adminToken, createPolicyBody); w.Code != http.StatusInternalServerError {
				t.Fatalf("POST = %d %s, want 500", w.Code, w.Body)
			}
			if st.calls != 0 {
				t.Error("CreatePolicy was called after the restriction failed")
			}
		})
	}
}

// restrictingSiteStore is the site-config fake with capability restrictions kept in a map, and a record of
// whether each model provider the saved document holds was already restricted when it was written.
type restrictingSiteStore struct {
	*fakeSiteConfigStore
	restricted  map[string]map[string]bool
	lockedAtPut map[string]bool
}

func (s *restrictingSiteStore) ListCapabilityRestrictions(context.Context) (map[string]map[string]bool, error) {
	return s.restricted, nil
}

func (s *restrictingSiteStore) SetCapabilityRestriction(_ context.Context, kind, value string, on bool, _ string) error {
	if s.restricted[kind] == nil {
		s.restricted[kind] = map[string]bool{}
	}
	if on {
		s.restricted[kind][value] = true
	} else {
		delete(s.restricted[kind], value)
	}
	return nil
}

func (s *restrictingSiteStore) PutSiteConfig(ctx context.Context, cfg types.SiteConfig) (types.SiteConfig, error) {
	if cfg.ModelProviders != nil {
		for _, p := range cfg.ModelProviders.Providers {
			s.lockedAtPut[p.ID] = s.restricted[capModelProvider][p.ID]
		}
	}
	return s.fakeSiteConfigStore.PutSiteConfig(ctx, cfg)
}

func newRestrictingSiteStore() *restrictingSiteStore {
	return &restrictingSiteStore{
		fakeSiteConfigStore: &fakeSiteConfigStore{},
		restricted:          map[string]map[string]bool{},
		lockedAtPut:         map[string]bool{},
	}
}

func customEndpointRow(id string) string {
	return `{"id":"` + id + `","kind":"custom_endpoint","base_url":"https://` + id + `.corp.example/llm",` +
		`"harnesses":[{"harness":"claude-code","path":"/anthropic"}]}`
}

func providersBody(ids ...string) string {
	rows := make([]string, len(ids))
	for i, id := range ids {
		rows[i] = customEndpointRow(id)
	}
	return `{"providers":[` + strings.Join(rows, ",") + `]}`
}

// ?restricted=true restricts each provider the save adds before the block is written, leaves a stored one alone,
// and refuses a save that adds none. Without the flag nothing is restricted, and {} still clears the block.
func TestPutModelProviders_RestrictedRestrictsWhatTheSaveAdds(t *testing.T) {
	seeded := func(t *testing.T) (*Server, *restrictingSiteStore, *recRecorder) {
		t.Helper()
		st := newRestrictingSiteStore()
		h := newHarness(t)
		srv, audit := New(baseTestConfig(h, st)), h.audit
		if w := do(t, srv, http.MethodPut, "/api/v1/model-providers", adminToken, providersBody("p")); w.Code != http.StatusOK {
			t.Fatalf("seed PUT = %d %s", w.Code, w.Body)
		}
		st.lockedAtPut = map[string]bool{}
		return srv, st, audit
	}
	t.Run("adds q beside the stored p", func(t *testing.T) {
		srv, st, audit := seeded(t)
		w := do(t, srv, http.MethodPut, "/api/v1/model-providers?restricted=true", adminToken, providersBody("p", "q"))
		if w.Code != http.StatusOK {
			t.Fatalf("PUT = %d %s", w.Code, w.Body)
		}
		if !st.lockedAtPut["q"] {
			t.Error("q was not restricted when the document was written")
		}
		if st.restricted[capModelProvider]["p"] || !st.restricted[capModelProvider]["q"] {
			t.Errorf("restrictions = %v, want q only", st.restricted[capModelProvider])
		}
		audits := availabilityAudits(t, audit)
		if len(audits) != 1 || audits[0]["value"] != "q" || audits[0]["restricted"] != true {
			t.Errorf("availability audits = %v, want one row for q", audits)
		}
	})
	t.Run("adds nothing", func(t *testing.T) {
		srv, st, _ := seeded(t)
		w := do(t, srv, http.MethodPut, "/api/v1/model-providers?restricted=true", adminToken, providersBody("p"))
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), reasonSiteConfigInvalid) || !strings.Contains(w.Body.String(), "a provider this save adds") {
			t.Fatalf("PUT = %d %s, want 400 %s naming a provider this save adds", w.Code, w.Body, reasonSiteConfigInvalid)
		}
		if len(st.restricted[capModelProvider]) != 0 || len(st.lockedAtPut) != 0 {
			t.Errorf("a refused save changed state: restrictions %v, writes %v", st.restricted, st.lockedAtPut)
		}
	})
	t.Run("no flag", func(t *testing.T) {
		srv, st, _ := seeded(t)
		if w := do(t, srv, http.MethodPut, "/api/v1/model-providers", adminToken, providersBody("p", "q")); w.Code != http.StatusOK {
			t.Fatalf("PUT = %d %s", w.Code, w.Body)
		}
		if len(st.restricted[capModelProvider]) != 0 {
			t.Errorf("restrictions = %v, want none", st.restricted)
		}
	})
	t.Run("the clear form", func(t *testing.T) {
		srv, st, _ := seeded(t)
		if w := do(t, srv, http.MethodPut, "/api/v1/model-providers", adminToken, `{}`); w.Code != http.StatusOK {
			t.Fatalf("PUT {} = %d %s", w.Code, w.Body)
		}
		if st.cfg.ModelProviders != nil {
			t.Errorf("model_providers = %+v, want cleared", st.cfg.ModelProviders)
		}
	})
	t.Run("the clear form with the flag", func(t *testing.T) {
		srv, st, _ := seeded(t)
		w := do(t, srv, http.MethodPut, "/api/v1/model-providers?restricted=true", adminToken, `{}`)
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), reasonSiteConfigInvalid) || !strings.Contains(w.Body.String(), "a provider this save adds") {
			t.Fatalf("PUT {} = %d %s, want 400 %s naming a provider this save adds", w.Code, w.Body, reasonSiteConfigInvalid)
		}
		if st.cfg.ModelProviders == nil || len(st.lockedAtPut) != 0 {
			t.Error("a refused clear changed the stored block")
		}
	})
}

// A save that fails after the restriction lifts the rows it wrote, and leaves one that was already there.
func TestPutModelProviders_RestrictedFailureLeavesNothing(t *testing.T) {
	st := newRestrictingSiteStore()
	srv := New(baseTestConfig(newHarness(t), st))
	st.putErr = errors.New("db down")
	st.restricted[capModelProvider] = map[string]bool{"r": true}
	w := do(t, srv, http.MethodPut, "/api/v1/model-providers?restricted=true", adminToken, providersBody("q", "r"))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("PUT = %d %s, want 500", w.Code, w.Body)
	}
	if st.restricted[capModelProvider]["q"] {
		t.Error("q stayed restricted after the failed save")
	}
	if !st.restricted[capModelProvider]["r"] {
		t.Error("r was restricted before the request and must stay so")
	}
}
