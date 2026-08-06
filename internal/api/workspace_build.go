// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/internal/workspacescan"
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
	// Log is the bounded tail of this build's output (maxBuildLogLines), fed by
	// buildLogWriter. Same in-memory-only caveat as the rest of buildState.
	Log []string `json:"log,omitempty"`
}

type buildTracker struct {
	mu sync.Mutex
	by map[uuid.UUID]*buildState
}

func (t *buildTracker) get(id uuid.UUID) buildState {
	t.mu.Lock()
	defer t.mu.Unlock()
	st, ok := t.by[id]
	if !ok {
		return buildState{}
	}
	cp := *st
	// Defensive copy: Log's backing array is still owned by the tracker and a
	// concurrent appendLog (another goroutine, same lock) must never mutate
	// what a caller reads AFTER we unlock.
	if st.Log != nil {
		cp.Log = append([]string(nil), st.Log...)
	}
	return cp
}

// begin claims the single-flight slot; ok=false when a build is already
// running for this workspace. A fresh buildState means a fresh (empty) Log —
// a retried build's pane starts clean, not appended to the failed attempt's.
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
	// Carry the accumulated Log over into the terminal state — the log IS the
	// debugging story for a failure, so it must survive Building true->false.
	var log []string
	if st, ok := t.by[id]; ok {
		log = st.Log
	}
	t.by[id] = &buildState{Building: false, Image: image, Error: errMsg, Log: log}
}

const (
	// maxBuildLogLines bounds each build's in-memory log ring: oldest lines
	// drop once output exceeds it. Generous enough to carry a real failure's
	// context, small enough that a runaway build can't grow the tracker
	// unbounded.
	maxBuildLogLines = 500
	// maxBuildLogLineLen clamps one STORED line — a single absurdly long
	// line (or non-line-oriented output) must not bloat the ring or the
	// /build response payload.
	maxBuildLogLineLen = 500
	// maxBuildLogBufBytes bounds buildLogWriter's internal partial-line
	// buffer: a chunk this large with no newline yet is flushed as its own
	// line and the buffer reset, rather than growing without bound.
	maxBuildLogBufBytes = 8 << 10 // 8KiB
)

// ansiCSI matches a CSI escape sequence (ESC '[' params final-byte) — the
// color/cursor codes build tools commonly emit (colorized compiler output,
// progress redraws). A cheap guard, not a full ANSI parser: good enough to
// keep them out of a plain-text log pane.
var ansiCSI = regexp.MustCompile("\x1b\\[[0-9;]*[a-zA-Z]")

// appendLog bounds-appends one line to id's ring, under the tracker's own
// lock (buildState carries no lock of its own — see the package doc).
// ponytail: keyed by WORKSPACE id like the tracker itself, not a per-build
// id — a fast Retry can show a trailing line or two from the build it
// replaced while the old container's log stream finishes closing. Not worth
// a separate per-build correlation id for a debug-aid pane.
func (t *buildTracker) appendLog(id uuid.UUID, line string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	st, ok := t.by[id]
	if !ok {
		return // no build known for id (finished + reaped, or never begun) — drop it
	}
	st.Log = append(st.Log, line)
	if over := len(st.Log) - maxBuildLogLines; over > 0 {
		st.Log = st.Log[over:]
	}
}

// buildLogWriter line-splits build output into id's ring on the tracker. Used
// as the build's LogSink so the wizard's Build step can show the real output
// instead of a bare spinner (see cmd/wardynd/envbuild_docker.go's adapter,
// which tees this with wardynd's own slog so operator logs don't regress).
type buildLogWriter struct {
	t   *buildTracker
	id  uuid.UUID
	buf []byte
}

func (w *buildLogWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		w.emit(w.buf[:i])
		w.buf = w.buf[i+1:]
	}
	if len(w.buf) > maxBuildLogBufBytes {
		w.emit(w.buf)
		w.buf = w.buf[:0]
	}
	return len(p), nil
}

// emit stores one line: ANSI/CSI escapes stripped, trimmed, and clamped so
// one absurd line can't bloat the ring or the /build response payload.
func (w *buildLogWriter) emit(raw []byte) {
	line := strings.TrimSpace(ansiCSI.ReplaceAllString(string(raw), ""))
	if len(line) > maxBuildLogLineLen {
		line = line[:maxBuildLogLineLen]
	}
	if line != "" {
		w.t.appendLog(w.id, line)
	}
}

var _ io.Writer = (*buildLogWriter)(nil)

// buildResponse is what both handlers return: the tracker's view unioned with
// what the workspace row already proves.
type buildResponse struct {
	// State: "building" | "done" | "failed" | "none" (nothing built yet) |
	// "nothing_to_build" (an explicit image boots verbatim).
	State     string    `json:"state"`
	Image     string    `json:"image,omitempty"`
	Detail    string    `json:"detail,omitempty"`
	StartedAt time.Time `json:"started_at,omitempty"`
	// Log is the build's output tail (see buildState.Log) — present only while
	// the tracker actually knows about a build THIS process ran; a cache-hit
	// "done" resolved from the workspace row's own ImageRef never populates it
	// (nothing was streamed this process — no log to show).
	Log []string `json:"log,omitempty"`
}

// resolveBuildView derives the honest view for GET/POST responses.
//
// ctx is needed only for the cache-hit branch: it re-derives the workspace's
// named agent tools so the stored BuiltProfileHash is compared against the
// SAME expression resolveWorkspaceImage wrote it with (p.CacheKey(tools), not
// a bare ProfileHash()). Keying the reader on anything else makes this branch
// unreachable by construction — which reports state:"none" for a workspace
// that IS built as soon as s.builds (in-memory, per-process) no longer knows
// about it: after any restart, or from a second replica.
func (s *Server) resolveBuildView(ctx context.Context, ws types.Workspace) buildResponse {
	// An explicit image choice boots verbatim — there is nothing to build.
	if b := ws.BaseImage; b != nil && b.Kind != "recommended" && b.Kind != "custom" && b.Image != "" {
		return buildResponse{State: "nothing_to_build", Image: b.Image,
			Detail: "this image boots as-is — no build involved"}
	}
	st := s.builds.get(ws.ID)
	switch {
	case st.Building:
		return buildResponse{State: "building", StartedAt: st.StartedAt, Log: st.Log}
	case st.Error != "":
		return buildResponse{State: "failed", Detail: st.Error, Log: st.Log}
	case st.Image != "":
		return buildResponse{State: "done", Image: st.Image, Log: st.Log}
	}
	if prof, ok := workspaceProfile(ws); ok && ws.ImageRef != "" {
		tools := workspacescan.AgentToolsForIntegrationTypes(s.namedIntegrationTypes(ctx, ws))
		if ws.BuiltProfileHash == prof.CacheKey(tools) {
			return buildResponse{State: "done", Image: ws.ImageRef}
		}
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
	writeJSON(w, http.StatusOK, s.resolveBuildView(r.Context(), ws))
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
	view := s.resolveBuildView(r.Context(), ws)
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
		logSink := &buildLogWriter{t: &s.builds, id: ws.ID}
		built, okBuild := s.resolveWorkspaceImage(ctx, buildID, ws, logSink)
		if !okBuild {
			// resolveWorkspaceImage audited the specific failure under buildID —
			// read it back so the wizard shows the REAL reason, not a pointer
			// at the audit trail.
			detail := "build failed — see the run.build audit trail for the reason"
			if evs, qerr := s.cfg.Store.QueryAuditEvents(ctx, buildID, 5); qerr == nil {
				for _, ev := range evs {
					if ev.Action != "run.build" || ev.Outcome != "failure" {
						continue
					}
					var d struct {
						Error string `json:"error"`
					}
					if json.Unmarshal(ev.Data, &d) == nil && d.Error != "" {
						detail = d.Error
					}
					break
				}
			}
			s.builds.finish(ws.ID, "", detail)
			return
		}
		s.builds.finish(ws.ID, built, "")
	}()
	writeJSON(w, http.StatusAccepted, buildResponse{State: "building", StartedAt: now})
}
