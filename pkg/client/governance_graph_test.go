// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client_test

// governance_graph_test.go pins ApplyGovernance over a composed profile graph: bases are written
// before the profiles composed on them, every graph reference is remapped to the target's ids, a
// repeat apply writes nothing, a pending base defers its children, and prune deletes descendants
// before bases. graphFake enforces what the server does: a base must exist, and a base with a
// child cannot be deleted (409).

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

type graphFake struct {
	mu       sync.Mutex
	profiles []client.GovernanceProfile
	hold     map[string]bool // profile names whose create/update is answered 202
	writes   []string        // "METHOD name-or-id", in arrival order
	deleted  []string        // profile names, in delete order
}

func (g *graphFake) byID(id uuid.UUID) (int, bool) {
	for i, p := range g.profiles {
		if p.ID == id {
			return i, true
		}
	}
	return 0, false
}

func (g *graphFake) client(t *testing.T) *client.Client {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		p := r.URL.Path
		switch {
		case r.Method == http.MethodGet && p == "/api/v1/governance":
			writeJSON(w, http.StatusOK, client.GovernanceDocument{Profiles: g.profiles, Assignments: []client.GovernanceAssignment{}})
		case (r.Method == http.MethodPost && p == "/api/v1/governance/profiles") ||
			(r.Method == http.MethodPut && strings.HasPrefix(p, "/api/v1/governance/profiles/")):
			var req struct {
				client.GovernanceProfileRequest
				Effective json.RawMessage `json:"effective"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Errorf("decode: %v", err)
			}
			if req.Effective != nil {
				t.Errorf("effective was written back for %q", req.Name)
			}
			g.writes = append(g.writes, r.Method+" "+req.Name)
			if g.hold[req.Name] {
				writeJSON(w, http.StatusAccepted, pendingChange("upsert", "governance_profile", req.Name))
				return
			}
			prof := client.GovernanceProfile{ID: uuid.New(), Name: req.Name, Ceiling: req.Ceiling, Limits: req.Limits}
			if r.Method == http.MethodPut {
				id, _ := uuid.Parse(strings.TrimPrefix(p, "/api/v1/governance/profiles/"))
				i, ok := g.byID(id)
				if !ok {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				prof.ID = id
				defer func(i int) { g.profiles[i] = prof }(i)
			}
			if string(req.BaseProfileID) != "" && string(req.BaseProfileID) != "null" {
				var id uuid.UUID
				if err := json.Unmarshal(req.BaseProfileID, &id); err != nil {
					t.Errorf("base_profile_id: %v", err)
				}
				if _, ok := g.byID(id); !ok {
					writeJSON(w, http.StatusBadRequest, map[string]string{"error": "base does not exist"})
					return
				}
				prof.BaseProfileID = &id
			}
			if string(req.Overlay) != "" && string(req.Overlay) != "null" {
				prof.Overlay = &types.CeilingOverlay{}
				_ = json.Unmarshal(req.Overlay, prof.Overlay)
			}
			if string(req.OverlayLimits) != "" && string(req.OverlayLimits) != "null" {
				prof.OverlayLimits = &types.LimitsOverlay{}
				_ = json.Unmarshal(req.OverlayLimits, prof.OverlayLimits)
			}
			if r.Method == http.MethodPost {
				g.profiles = append(g.profiles, prof)
			}
			writeJSON(w, http.StatusOK, client.GovernanceProfileResponse{Profile: prof})
		case r.Method == http.MethodDelete && strings.HasPrefix(p, "/api/v1/governance/profiles/"):
			id, _ := uuid.Parse(strings.TrimPrefix(p, "/api/v1/governance/profiles/"))
			i, ok := g.byID(id)
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			for _, c := range g.profiles {
				if c.BaseProfileID != nil && *c.BaseProfileID == id {
					writeJSON(w, http.StatusConflict, map[string]string{"error": "profile is a base"})
					return
				}
			}
			g.deleted = append(g.deleted, g.profiles[i].Name)
			g.writes = append(g.writes, "DELETE "+g.profiles[i].Name)
			g.profiles = append(g.profiles[:i], g.profiles[i+1:]...)
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return &client.Client{BaseURL: srv.URL, Token: testToken, HTTPClient: srv.Client()}
}

func ptr[T any](v T) *T { return &v }

// sourceGraph is a three-level graph exported in REVERSE order: leaf, mid, root, with ids that exist
// only in the source install.
func sourceGraph() client.GovernanceDocument {
	rootID, midID, leafID := uuid.New(), uuid.New(), uuid.New()
	ts := time.Time{}
	return client.GovernanceDocument{Profiles: []client.GovernanceProfile{
		{ID: leafID, Name: "leaf", BaseProfileID: &midID, Overlay: &types.CeilingOverlay{AutoStopAfterSec: ptr(60)},
			OverlayLimits: &types.LimitsOverlay{MaxConcurrentRuns: ptr(1)}, CreatedAt: ts},
		{ID: midID, Name: "mid", BaseProfileID: &rootID, Overlay: &types.CeilingOverlay{AutoStopAfterSec: ptr(120)}},
		{ID: rootID, Name: "root", Limits: client.GovernanceLimits{MaxConcurrentRuns: 4}},
	}}
}

// shape strips the per-install ids so two exports compare by name.
func shape(doc client.GovernanceDocument) map[string][4]any {
	name := map[uuid.UUID]string{}
	for _, p := range doc.Profiles {
		name[p.ID] = p.Name
	}
	out := map[string][4]any{}
	for _, p := range doc.Profiles {
		base := ""
		if p.BaseProfileID != nil {
			base = name[*p.BaseProfileID]
		}
		out[p.Name] = [4]any{base, p.Overlay, p.OverlayLimits, p.Limits}
	}
	return out
}

func TestApplyGovernance_GraphReversedOrderRoundTripsAndRepeatsAsNoop(t *testing.T) {
	ctx := context.Background()
	src := sourceGraph()
	// ApplyGovernance rewrites the document it is given in place; keep the source's own ids and shape.
	raw, _ := json.Marshal(src)
	var srcCopy client.GovernanceDocument
	_ = json.Unmarshal(raw, &srcCopy)
	wantShape := shape(src)
	g := &graphFake{}
	c := g.client(t)

	got, err := c.ApplyGovernance(ctx, src, false)
	if err != nil {
		t.Fatalf("ApplyGovernance: %v", err)
	}
	if want := []string{"POST root", "POST mid", "POST leaf"}; !reflect.DeepEqual(g.writes, want) {
		t.Errorf("write order = %v, want %v", g.writes, want)
	}
	if !reflect.DeepEqual(shape(got), wantShape) {
		t.Errorf("re-exported graph = %+v, want %+v", shape(got), wantShape)
	}
	for _, p := range got.Profiles {
		for _, s := range srcCopy.Profiles {
			if p.BaseProfileID != nil && *p.BaseProfileID == s.ID {
				t.Errorf("%q kept the source install's base id %s", p.Name, s.ID)
			}
		}
	}

	g.writes = nil
	if _, err := c.ApplyGovernance(ctx, got, false); err != nil {
		t.Fatalf("repeat ApplyGovernance: %v", err)
	}
	if len(g.writes) != 0 {
		t.Errorf("repeat apply wrote %v, want nothing", g.writes)
	}
	// The source document, ids and all, is a no-op against the install it just built.
	if _, err := c.ApplyGovernance(ctx, srcCopy, false); err != nil {
		t.Fatalf("source re-apply: %v", err)
	}
	if len(g.writes) != 0 {
		t.Errorf("source-id re-apply wrote %v, want nothing", g.writes)
	}
}

func TestApplyGovernance_PendingBaseDefersItsChildren(t *testing.T) {
	src := sourceGraph()
	g := &graphFake{hold: map[string]bool{"root": true}}
	c := g.client(t)

	res, err := c.ApplyGovernanceResult(context.Background(), src, false)
	if err != nil {
		t.Fatalf("ApplyGovernanceResult: %v", err)
	}
	if len(res.Pending) != 1 {
		t.Fatalf("pending = %d, want 1", len(res.Pending))
	}
	want := []client.GovernanceDeferredWrite{{Profile: "mid", Base: "root"}, {Profile: "leaf", Base: "mid"}}
	if !reflect.DeepEqual(res.Deferred, want) {
		t.Errorf("deferred = %+v, want %+v", res.Deferred, want)
	}
	if want := []string{"POST root"}; !reflect.DeepEqual(g.writes, want) {
		t.Errorf("writes = %v, want only the held base", g.writes)
	}
}

func TestApplyGovernance_PruneDeletesDescendantsBeforeBases(t *testing.T) {
	src := sourceGraph()
	g := &graphFake{}
	c := g.client(t)
	if _, err := c.ApplyGovernance(context.Background(), src, false); err != nil {
		t.Fatalf("seed: %v", err)
	}
	g.writes = nil

	// An empty document with prune removes the whole graph; listing order puts the base first.
	if _, err := c.ApplyGovernance(context.Background(), client.GovernanceDocument{}, true); err != nil {
		t.Fatalf("prune: %v", err)
	}
	if want := []string{"leaf", "mid", "root"}; !reflect.DeepEqual(g.deleted, want) {
		t.Errorf("delete order = %v, want %v", g.deleted, want)
	}
}
