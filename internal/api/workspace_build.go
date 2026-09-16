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
	// Key is the workspace's image cache key AT THE MOMENT THIS BUILD STARTED
	// (Server.workspaceBuiltImageKey) — what the build was building, not what
	// it produced. The `done` half of a stale entry is answered by the row, but
	// a FAILED build has no ref for the row to disagree with, so without this a
	// failure outlived every invalidation: after a rescan moved the profile, GET
	// still reported the previous composition's error until someone clicked
	// Build again or wardynd restarted. Never serialised — it is an internal
	// freshness fact, not something a caller can act on.
	Key string `json:"-"`
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

// drop forgets everything this process remembers about id's build. Called by
// every invalidator of the built image (B4-F2): the tracker is a CACHE of a
// build, so once the thing it was caching is gone — the workspace edited, the
// row deleted — its memory is not a stale answer to be outranked later, it is
// an answer to a question nobody can ask any more. Without it, a failed build's
// Error also outlived the composition that produced it, so an edited workspace
// reported `failed` with the old build's reason forever.
func (t *buildTracker) drop(id uuid.UUID) {
	t.mu.Lock()
	defer t.mu.Unlock()
	// NEVER under a live build: the entry IS the single-flight claim, so
	// dropping one whose goroutine is still inside the image builder hands the
	// slot back and the next Build click starts a SECOND envbuilder run for one
	// workspace — both finishing into this tracker and the same cache columns.
	// Keeping it costs nothing: the invalidator has already cleared the row, so
	// whatever that build finishes with cannot answer `done` either way.
	if st, ok := t.by[id]; ok && st.Building {
		return
	}
	delete(t.by, id)
}

// bindKey records which composition the in-flight build is building. Separate
// from begin (rather than a parameter on it) because begin's job is the
// single-flight claim and only ONE caller — the route that actually launches a
// build — knows the workspace well enough to answer.
func (t *buildTracker) bindKey(id uuid.UUID, key string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if st, ok := t.by[id]; ok {
		st.Key = key
	}
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
	var key string
	if st, ok := t.by[id]; ok {
		log, key = st.Log, st.Key
	}
	t.by[id] = &buildState{Building: false, Image: image, Error: errMsg, Log: log, Key: key}
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
	State  string `json:"state"`
	Image  string `json:"image,omitempty"`
	Detail string `json:"detail,omitempty"`
	// StartedAt is a pointer (WIRE-6): encoding/json's omitempty is a no-op for
	// a struct, so a plain time.Time shipped the zero instant
	// ("0001-01-01T00:00:00Z") on every non-"building" response instead of
	// omitting the field the TS WorkspaceBuildState declares optional/absent.
	StartedAt *time.Time `json:"started_at,omitempty"`
	// Log is the build's output tail (see buildState.Log) — present only while
	// the tracker actually knows about a build THIS process ran; a cache-hit
	// "done" resolved from the workspace row's own ImageRef never populates it
	// (nothing was streamed this process — no log to show).
	Log []string `json:"log,omitempty"`
}

// resolveBuildView derives the honest view for GET/POST responses.
//
// No ctx: the cache-hit branch compares the stored BuiltProfileHash against
// the SAME expression resolveWorkspaceImage wrote it with (p.CacheKey(), not
// a bare ProfileHash()) — both are pure functions of the profile now that the
// standard agent-tool install is unconditional rather than derived from the
// workspace's named integrations.
// The `tier` argument is the READER's projection tier (workspaceReadTierFor),
// and it is a required argument rather than a field on the workspace for the
// same reason dispatchCeiling is: this response is fed by getWorkspaceReadable,
// so a plain member and a foreign security admin reach it for an OPERATOR-owned
// workspace, and two of the branches below answer with ws.BaseImage.Image — the
// operator's authored registry coordinate, exactly the datum
// redactWorkspaceForRead blanks on GET /workspaces{,/{id}}. A new call site
// cannot compile without deciding.
//
// The BUILT image (st.Image / ws.ImageRef) is NOT blanked: it is the image the
// member's own run against this workspace actually executes, kept for the same
// reason redactWorkspaceForRead keeps image_ref. Log IS blanked below the full
// tier — a build log is the operator's build output and reproduces the base
// ref verbatim in its FROM line.
func (s *Server) resolveBuildView(ws types.Workspace, tier workspaceReadTier) buildResponse {
	authoredImage := func(ref string) string {
		if tier == workspaceReadFull {
			return ref
		}
		return ""
	}
	buildLog := func(l []string) []string {
		if tier == workspaceReadFull {
			return l
		}
		return nil
	}
	// B4-F1: the builder's OWN error is the fourth Detail this function can
	// answer, and the only one that is not fixed prose — it quotes the
	// operator's authored base-image coordinate verbatim in the pull/FROM line
	// that failed, which is exactly the datum redactWorkspaceForRead blanks on
	// GET /workspaces{,/{id}} and the Log below is already blanked for. The
	// three STATIC Details stay at every tier: a member launching against this
	// workspace needs to know the host has no builder wired
	// (TestF287_BuildKeepsWhatTheMemberNeeds pins that they do).
	builderError := func(msg string) string {
		if tier == workspaceReadFull {
			return msg
		}
		return ""
	}
	// An explicit image CHOICE (registry/byo) boots verbatim ONLY once an image
	// builder has wrapped it with the agent runtime — resolveWorkspaceImage's
	// FinalizeBase call, the SAME wrap a devcontainer build needs. W7-S1-2: this
	// used to report "nothing_to_build ... no build involved" unconditionally,
	// which is false on a builder-less host (the default bare binary AND every
	// Helm/k8s install unless WARDYN_ENVBUILD is set) — a run against this
	// workspace is refused there (runs_create.go's wsRefs door), not booted
	// as-is. Gate on the builder so a builder-less host instead falls through
	// to the same honest report "custom" already gets below.
	if b := ws.BaseImage; b != nil && b.Kind != "recommended" && b.Kind != "custom" && b.Image != "" && s.cfg.ImageBuilder != nil {
		return buildResponse{State: "nothing_to_build", Image: authoredImage(b.Image),
			Detail: "this image boots as-is — no build involved"}
	}
	key, keyed := s.workspaceBuiltImageKey(ws)
	st := s.builds.get(ws.ID)
	switch {
	case st.Building:
		return buildResponse{State: "building", StartedAt: &st.StartedAt, Log: buildLog(st.Log)}
	case st.Error != "" && st.Key == key:
		// Only about the composition it actually ran against — see buildState.Key.
		return buildResponse{State: "failed", Detail: builderError(st.Error), Log: buildLog(st.Log)}
	}
	// THE ROW DECIDES WHETHER ANYTHING IS BUILT; THE TRACKER ONLY REMEMBERS HOW
	// (B4-F2). The tracker used to answer `done` from its own st.Image before
	// anything consulted the row, so an image invalidated underneath it — a PUT
	// that removeStaleImage'd the ref and cleared the cache columns, a rescan
	// that moved the profile hash — still read `done` with a ref no run would
	// ever resolve, and POST /build short-circuited on that same view, leaving
	// the workspace unbuildable until wardynd restarted. The row is what
	// resolveWorkspaceImage actually consults, so it is what this reports; the
	// tracker contributes only the LOG, and only while it is talking about the
	// very ref the row names.
	if keyed && ws.ImageRef != "" && ws.BuiltProfileHash == key {
		done := buildResponse{State: "done", Image: ws.ImageRef}
		if st.Image == ws.ImageRef {
			done.Log = buildLog(st.Log)
		}
		return done
	}
	if s.cfg.ImageBuilder == nil {
		if b := ws.BaseImage; b != nil && b.Kind != "recommended" && b.Image != "" {
			// Explicit base image (registry/byo/custom), no builder wired: unlike
			// the generic "sessions boot the stock agent image" fallback below,
			// this workspace's chosen image is refused outright at run creation
			// rather than silently substituted (PARITY-4, runs_create.go).
			return buildResponse{State: "none", Image: authoredImage(b.Image),
				Detail: "the sandbox image builder is not wired on this host, so this base image cannot be wrapped with the agent runtime — a run against this workspace is REFUSED, not silently substituted; set WARDYN_ENVBUILD on a wardynd built with -tags docker to enable it, or drop the base image"}
		}
		return buildResponse{State: "none",
			Detail: "devcontainer builds are not enabled on this host — sessions boot the stock agent image"}
	}
	return buildResponse{State: "none"}
}

// workspaceBuiltImageKey is the cache key resolveWorkspaceImage
// (workspace_run_image.go) would compare ws.BuiltProfileHash against on the
// next launch — the one fact that decides whether the row's cached ImageRef is
// still the image a run gets. ok=false when no lane could have keyed on
// anything: no builder to wrap an explicit base image, or no readable profile.
//
// It mirrors that function's lane SELECTION rather than restating its keys: an
// explicit base image keys on (kind, ref), a repo primary source carrying its
// own devcontainer on (clone URL, ref), everything else on the scanned
// profile's CacheKey.
//
// THE EXPLICIT-IMAGE LANE IS NOT DEAD CODE, although resolveBuildView answers
// "nothing_to_build" above for most of it: that branch excludes kind "custom",
// which is exactly the composition that falls through to here with a byoi key
// on the row. Keying it on the profile instead reported `none` for a workspace
// that had built perfectly, in-process AND after a restart, and made every
// Build click re-run the whole resolve. The builder gate keeps the honest
// builder-less refusal below (W7-S1-2) — with no builder that base image is
// REFUSED at run creation, so a cached wrap for it is not "done" here.
//
// Reporting these lanes from the ROW is also what lets their built images read
// `done` at all after a restart: neither key is the profile's, so the old
// profile-only comparison could never match one and the in-memory tracker was
// the only thing that ever said done for them.
func (s *Server) workspaceBuiltImageKey(ws types.Workspace) (string, bool) {
	if b := ws.BaseImage; b != nil && b.Kind != "recommended" && strings.TrimSpace(b.Image) != "" {
		if s.cfg.ImageBuilder == nil {
			return "", false
		}
		return byoiCacheKey(b.Kind, b.Image), true
	}
	p, ok := workspaceProfile(ws)
	if !ok {
		return "", false
	}
	if url := repoOwnDevcontainerURL(ws, p); url != "" {
		return repoDevcontainerCacheKey(url, ws.Sources[0].Ref), true
	}
	return p.CacheKey(), true
}

// handleGetWorkspaceBuild reports the workspace's image-build state.
//
//	GET /api/v1/workspaces/{id}/build
func (s *Server) handleGetWorkspaceBuild(w http.ResponseWriter, r *http.Request) {
	id, ok := parseIDParam(w, r, "id", "workspace")
	if !ok {
		return
	}
	ws, ok := s.getWorkspaceReadable(w, r, id)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, s.resolveBuildView(ws, s.workspaceReadTierFor(r, ws)))
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
	ws, ok := s.getWorkspaceAuthorized(w, r, id)
	if !ok {
		return
	}
	view := s.resolveBuildView(ws, s.workspaceReadTierFor(r, ws))
	if view.State == "nothing_to_build" || view.State == "done" {
		writeJSON(w, http.StatusOK, view)
		return
	}
	// This route is on the owner-or-admin router, so a MEMBER reaches it — and
	// past this point resolveWorkspaceImage clones the workspace's repo
	// SERVER-SIDE (repoOwnDevcontainerURL -> envbuilder). A workspace onboarded
	// before a provider row narrowed it, or owned by a member whose grant was
	// taken away, must not keep that clone running through the wizard's Build
	// step. Sited after the two early returns above, which clone nothing.
	if s.admitRepoSources(w, r, repoSourceLocators(ws.Sources)...) {
		return
	}
	if s.denyMemberWorkspaceProviders(w, r, "workspaces.source_provider", repoSourceLocators(ws.Sources)...) {
		return
	}
	if s.cfg.ImageBuilder == nil {
		writeJSON(w, http.StatusOK, view) // honest "none" + detail; not an error
		return
	}
	now := s.cfg.Now().UTC()
	if !s.builds.begin(ws.ID, now) {
		already := s.builds.get(ws.ID)
		writeJSON(w, http.StatusAccepted, buildResponse{State: "building", StartedAt: &already.StartedAt})
		return
	}
	// What this build is building (buildState.Key): resolved from the SAME
	// expression the reader compares the row against, so a rescan landing a new
	// profile retires this attempt's outcome instead of reporting it against a
	// composition it never saw.
	buildKey, _ := s.workspaceBuiltImageKey(ws)
	s.builds.bindKey(ws.ID, buildKey)
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
	writeJSON(w, http.StatusAccepted, buildResponse{State: "building", StartedAt: &now})
}
