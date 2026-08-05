// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// workspace_build.go — the wizard's BUILD step endpoints. Building the
// recommended/custom image used to happen lazily INSIDE a session launch,
// which froze the "Verify" click behind minutes of invisible work. Building
// is its own process now: POST kicks it asynchronously (single-flight per
// workspace), GET reports it honestly, and the session launch finds the image
// already cached (resolveWorkspaceImage's hash check) so it starts fast.

// buildState is one workspace's in-flight/last build. In-memory by design:
// a build is bound to THIS daemon's docker socket, so tracker state has
// nothing durable to say across restarts — after one, GET falls back to
// what the workspace row proves (a cached image ref, or nothing).
type buildState struct {
	Building  bool      `json:"building"`
	Image     string    `json:"image,omitempty"`
	Error     string    `json:"error,omitempty"`
	StartedAt time.Time `json:"started_at,omitempty"`
}

type buildTracker struct {
	mu sync.Mutex
	by map[uuid.UUID]*buildState
}

func (t *buildTracker) get(id uuid.UUID) buildState {
	t.mu.Lock()
	defer t.mu.Unlock()
	if st, ok := t.by[id]; ok {
		return *st
	}
	return buildState{}
}

// begin claims the single-flight slot; ok=false when a build is already
// running for this workspace.
func (t *buildTracker) begin(id uuid.UUID, now time.Time) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.by == nil {
		t.by = map[uuid.UUID]*buildState{}
	}
	if st, ok := t.by[id]; ok && st.Building {
		return false
	}
	t.by[id] = &buildState{Building: true, StartedAt: now}
	return true
}

func (t *buildTracker) finish(id uuid.UUID, image, errMsg string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.by == nil {
		t.by = map[uuid.UUID]*buildState{}
	}
	t.by[id] = &buildState{Building: false, Image: image, Error: errMsg}
}

// buildResponse is what both handlers return: the tracker's view unioned with
// what the workspace row already proves.
type buildResponse struct {
	// State: "building" | "done" | "failed" | "none" (nothing built yet) |
	// "nothing_to_build" (an explicit image boots verbatim).
	State     string    `json:"state"`
	Image     string    `json:"image,omitempty"`
	Detail    string    `json:"detail,omitempty"`
	StartedAt time.Time `json:"started_at,omitempty"`
}

// resolveBuildView derives the honest view for GET/POST responses.
func (s *Server) resolveBuildView(ws types.Workspace) buildResponse {
	// An explicit image choice boots verbatim — there is nothing to build.
	if b := ws.BaseImage; b != nil && b.Kind != "recommended" && b.Kind != "custom" && b.Image != "" {
		return buildResponse{State: "nothing_to_build", Image: b.Image,
			Detail: "this image boots as-is — no build involved"}
	}
	st := s.builds.get(ws.ID)
	switch {
	case st.Building:
		return buildResponse{State: "building", StartedAt: st.StartedAt}
	case st.Error != "":
		return buildResponse{State: "failed", Detail: st.Error}
	case st.Image != "":
		return buildResponse{State: "done", Image: st.Image}
	}
	if prof, ok := workspaceProfile(ws); ok && ws.ImageRef != "" && ws.BuiltProfileHash == prof.ProfileHash() {
		return buildResponse{State: "done", Image: ws.ImageRef}
	}
	if s.cfg.ImageBuilder == nil {
		return buildResponse{State: "none",
			Detail: "devcontainer builds are not enabled on this host — sessions boot the stock agent image"}
	}
	return buildResponse{State: "none"}
}

// handleGetWorkspaceBuild reports the workspace's image-build state.
//
//	GET /api/v1/workspaces/{id}/build
func (s *Server) handleGetWorkspaceBuild(w http.ResponseWriter, r *http.Request) {
	id, ok := parseIDParam(w, r, "id", "workspace")
	if !ok {
		return
	}
	ws, ok := s.getWorkspaceOr404(w, r, id)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, s.resolveBuildView(ws))
}

// handleBuildWorkspace kicks the workspace's image build asynchronously —
// single-flight per workspace, detached from the request (a closed tab must
// not cancel a build). 202 while it runs; the GET above (and the run.build
// audit trail) carry the outcome. resolveWorkspaceImage is reused wholesale,
// so the cache/hash/audit semantics are exactly the run path's — a session
// launched after a successful build hits the cache and starts immediately.
//
//	POST /api/v1/workspaces/{id}/build
func (s *Server) handleBuildWorkspace(w http.ResponseWriter, r *http.Request) {
	id, ok := parseIDParam(w, r, "id", "workspace")
	if !ok {
		return
	}
	ws, ok := s.getWorkspaceOr404(w, r, id)
	if !ok {
		return
	}
	view := s.resolveBuildView(ws)
	if view.State == "nothing_to_build" || view.State == "done" {
		writeJSON(w, http.StatusOK, view)
		return
	}
	if s.cfg.ImageBuilder == nil {
		writeJSON(w, http.StatusOK, view) // honest "none" + detail; not an error
		return
	}
	now := s.cfg.Now().UTC()
	if !s.builds.begin(ws.ID, now) {
		writeJSON(w, http.StatusAccepted, buildResponse{State: "building", StartedAt: s.builds.get(ws.ID).StartedAt})
		return
	}
	buildID := uuid.New() // audit correlation for a build with no run
	go func() {
		ctx := context.WithoutCancel(r.Context())
		built, okBuild := s.resolveWorkspaceImage(ctx, buildID, ws)
		if !okBuild {
			// resolveWorkspaceImage audited the specific failure; the tracker
			// carries the honest headline for the wizard's poll.
			s.builds.finish(ws.ID, "", "build failed — see the run.build audit trail for the reason")
			return
		}
		s.builds.finish(ws.ID, built, "")
	}()
	writeJSON(w, http.StatusAccepted, buildResponse{State: "building", StartedAt: now})
}
