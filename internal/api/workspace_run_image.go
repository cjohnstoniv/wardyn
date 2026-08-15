// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"strings"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/internal/workspacescan"
)

// workspaceProfile decodes a workspace's opaque profile blob into the scanner's
// WorkspaceProfile. Returns ok=false when there is no profile yet (unscanned) or
// it is malformed.
func workspaceProfile(ws types.Workspace) (workspacescan.WorkspaceProfile, bool) {
	if len(ws.Profile) == 0 {
		return workspacescan.WorkspaceProfile{}, false
	}
	var p workspacescan.WorkspaceProfile
	if err := json.Unmarshal(ws.Profile, &p); err != nil {
		return workspacescan.WorkspaceProfile{}, false
	}
	return p, true
}

// byoiCacheKey and repoDevcontainerCacheKey namespace the SAME shared
// ImageRef/BuiltProfileHash cache columns the generated-devcontainer lane
// uses (WorkspaceProfile.CacheKey) so all three build lanes can share one
// per-workspace cache slot without colliding: switching a workspace between
// a byoi base image, a repo's own devcontainer, and the generated toolchain
// image always busts the cache (the hash's own lane prefix differs), rather
// than one lane reading a stale image built by a different lane.

// byoiCacheKey keys the FinalizeBase wrap on (kind, base ref) — W20-W20-record-image-3.
func byoiCacheKey(kind, image string) string {
	sum := sha256.Sum256([]byte("byoi|" + kind + "|" + image))
	return hex.EncodeToString(sum[:])
}

// repoDevcontainerCacheKey keys a repo's own devcontainer build on (clone URL,
// ref) — W20-W20-record-image-3. Like the generated-devcontainer lane, this
// does not detect the repo's CONTENT changing at a fixed ref (a branch head
// moving) — the same honesty ceiling profile-hash caching already accepts.
func repoDevcontainerCacheKey(url, ref string) string {
	sum := sha256.Sum256([]byte("repo-devcontainer|" + url + "|" + ref))
	return hex.EncodeToString(sum[:])
}

// repoDevcontainerImageCaveats is W20-W20-record-image-6: when the session's
// image comes from the repo's OWN devcontainer (resolveWorkspaceImage's
// repo-own-devcontainer lane, source="repo-devcontainer"), that image was
// built AS-IS from the repo's devcontainer file — it never bakes claude-code
// (resolveWorkspaceImage's own doc comment: "deliberate, not an oversight").
// The Record pane tells the operator to drive the agent in this sandbox and
// wires model credentials for it regardless, so a silent absence reads as a
// broken session rather than an image that legitimately doesn't carry the
// CLI. Uses the SAME predicate resolveWorkspaceImage's repo-devcontainer
// branch gates on (repoOwnDevcontainerURL), so the caveat fires exactly when
// that lane will actually be selected.
func repoDevcontainerImageCaveats(ws types.Workspace) []string {
	// An explicit BaseImage choice takes precedence over this lane in
	// resolveWorkspaceImage (checked first) — mirror that here so the caveat
	// never fires for a session that will actually run the operator's chosen
	// base image instead.
	if b := ws.BaseImage; b != nil && b.Kind != "recommended" && strings.TrimSpace(b.Image) != "" {
		return nil
	}
	p, ok := workspaceProfile(ws)
	if !ok || repoOwnDevcontainerURL(ws, p) == "" {
		return nil
	}
	return []string{
		"This workspace's image is built from the repo's OWN devcontainer, not Wardyn's generated one — " +
			"claude-code is present only if the repo's devcontainer installs it itself.",
	}
}

// cachedImageStillPresent guards every cache-hit branch below
// (W20-W20-record-image-5): a cached image_ref the daemon no longer actually
// has (pruned, host replaced, a different daemon this control plane now
// talks to) used to be a PERMANENT dead end — every launch "hit" the cache,
// dispatched the missing ref, and failed at "no such image" with no
// automatic recovery; the only reset was clearing the row by hand. When the
// wired Runner can answer (runner.ImageChecker — the docker substrate),
// consult it and treat "not present" as a cache MISS so resolveWorkspaceImage
// falls through to a rebuild. A Runner that cannot answer (unwired, or a
// substrate with no local cache notion) is the SAME "unknown" this function
// already treated as true before this fix existed — fail-open, matching
// resolveWorkspaceImage's posture everywhere else.
func (s *Server) cachedImageStillPresent(ctx context.Context, ref string) bool {
	ic, ok := s.cfg.Runner.(runner.ImageChecker)
	if !ok {
		return true
	}
	present, err := ic.ImagePresent(ctx, ref)
	if err != nil {
		return true
	}
	return present
}

// resolveWorkspaceImage returns the sandbox image for a run driven by its PRIMARY
// onboarded workspace, or ok=false to fall through to the convention image.
// Order (all fail-OPEN — any failure returns ok=false + convention image, never
// blocks the run):
//   - an explicit BaseImage CHOICE on the workspace ("registry"/"byo"/"custom") →
//     use it (see below);
//   - a REPO PRIMARY source (Sources[0]) whose profile HasDevcontainer → build
//     the repo's own devcontainer;
//   - a cached generated image still valid for the current profile hash → reuse;
//   - else generate a devcontainer for the detected toolchain, build it, and cache
//     image_ref + built_profile_hash on the workspace for reuse.
//
// logSink, when non-nil, receives the build's output as it happens (the
// wizard Build step's in-memory ring); every OTHER caller passes nil, which
// falls back to the ImageBuilder's own default (wardynd's slog) unchanged.
//
// It audits its own build success/failure against runID.
func (s *Server) resolveWorkspaceImage(ctx context.Context, runID uuid.UUID, primary types.Workspace, logSink io.Writer) (string, bool) {
	buildAudit := func(outcome string, extra map[string]any) {
		extra["workspace_id"] = primary.ID.String()
		s.recordAudit(ctx, s.auditEvent(&runID, types.ActorSystem, "wardynd", "run.build",
			runID.String(), outcome, mustJSON(extra)))
	}

	// An explicit base-image CHOICE takes precedence over everything below.
	// "recommended" (Wardyn's own convention image for the detected stack) —
	// like a nil BaseImage — falls through to the devcontainer/generated path
	// instead of a fixed ref.
	if b := primary.BaseImage; b != nil && b.Kind != "recommended" && strings.TrimSpace(b.Image) != "" {
		if s.cfg.ImageBuilder == nil {
			// PARITY-4: the workspace_id door hard-400s a base_image with no builder
			// wired (validateImageBuildRequest, re-run after the seed); the UI door has
			// no such gate, so at minimum AUDIT that the operator's chosen base image
			// was DROPPED for the convention image rather than swapping it silently.
			// (Fully closing the door = the UI sending workspace_id — later UI batch.)
			buildAudit("skipped", map[string]any{
				"source": "base_image:" + b.Kind, "base": b.Image,
				"reason": "no image builder wired; base image dropped for the convention image",
			})
			return "", false
		}
		// Cache the wrap PER (workspace, base ref) — W20-W20-record-image-3: a
		// record/replay session used to re-wrap the SAME base image on every
		// single session launch (the old tag embedded runID, guaranteeing a
		// cache miss even when nothing about the base image changed), turning
		// every record/verify click into a multi-minute rebuild. byoiCacheKey
		// namespaces the shared ImageRef/BuiltProfileHash cache columns so a
		// workspace switching FROM a byoi/repo-devcontainer/generated lane (or
		// to a different base ref) always busts the cache instead of reading a
		// stale image built for a different source.
		byoiHash := byoiCacheKey(b.Kind, b.Image)
		if primary.ImageRef != "" && primary.BuiltProfileHash == byoiHash && s.cachedImageStillPresent(ctx, primary.ImageRef) {
			buildAudit("success", map[string]any{"source": "base_image:" + b.Kind, "base": b.Image, "image": primary.ImageRef, "cache_hit": true})
			return primary.ImageRef, true
		}
		// Wrap the operator's base image the SAME way the workspace_id door does
		// (PARITY-4): FinalizeBase copies the runner tools + agent-run in and tags it
		// wardyn-byoi/<runid>, which ALSO arms dispatch's fail-closed harness selftest
		// (runs_dispatch.go keys it off the wardyn-byoi/ prefix). Before this a UI-door
		// run launched the raw image with no agent-run and failed with an opaque exec
		// error, while the CLI wrapped + selftest-gated the identical workspace.
		// "custom" Steps are not layered here (no builder method layers Dockerfile
		// lines on a base yet) — same as the req.Image path's own verbatim wrap.
		outTag := "wardyn-byoi/" + runID.String() + ":latest"
		built, berr := s.cfg.ImageBuilder.FinalizeBase(ctx, b.Image, outTag, logSink)
		if berr != nil {
			buildAudit("failure", map[string]any{"source": "base_image:" + b.Kind, "base": b.Image, "error": berr.Error()})
			return "", false
		}
		if s.cfg.Store != nil {
			if _, uerr := s.cfg.Store.SetWorkspaceBuiltImage(ctx, primary.ID, built, byoiHash); uerr != nil {
				// Non-fatal: the image built and is usable now; caching just missed.
				buildAudit("success", map[string]any{"source": "base_image:" + b.Kind, "base": b.Image, "image": built, "cache_warn": uerr.Error()})
				return built, true
			}
		}
		buildAudit("success", map[string]any{"source": "base_image:" + b.Kind, "base": b.Image, "image": built})
		return built, true
	}

	if s.cfg.ImageBuilder == nil {
		return "", false
	}

	p, ok := workspaceProfile(primary)
	if !ok {
		return "", false // unscanned/malformed → convention image
	}

	// A repo PRIMARY source (Sources[0]) carrying its OWN devcontainer: respect
	// it, build from the repo — UNLESS the source is an SSH URL. The image
	// builder (envbuilder) clones with no minted key / known_hosts / :443
	// ProxyCommand — only the agent-run sandbox has that wiring — so an SSH
	// devcontainer build would fail auth. Fall through to a generated toolchain
	// image; agent-run still clones the repo itself using the run's ssh_key
	// grant (the repo's own devcontainer is just not built in v1).
	//
	// This lane never bakes the standard agent-tool install: it builds the
	// repo's own devcontainer file(s) verbatim rather than rewriting them to
	// point at the generated Dockerfile gen.go's baseOrBuild would otherwise
	// layer a RUN onto (see its package comment — the two mechanisms that
	// WOULD add one without rewriting the operator's own devcontainer, a
	// lifecycle hook and the vendor's own devcontainer feature, were both
	// tried against a real build and rejected). claude-code is therefore
	// silently absent from this image unless the operator's own devcontainer
	// happens to install it — deliberate, not an oversight.
	if url := repoOwnDevcontainerURL(primary, p); url != "" {
		repoSrc := primary.Sources[0]
		// Cache per (repo URL, ref) — the same W20-W20-record-image-3 fix as the
		// byoi branch above: this lane used to rebuild the repo's OWN devcontainer
		// on every session (a fixed tag, but no cache-hit check before building
		// it), even though nothing about the repo ref had changed between sessions.
		repoHash := repoDevcontainerCacheKey(url, repoSrc.Ref)
		if primary.ImageRef != "" && primary.BuiltProfileHash == repoHash && s.cachedImageStillPresent(ctx, primary.ImageRef) {
			buildAudit("success", map[string]any{"source": "repo-devcontainer", "image": primary.ImageRef, "cache_hit": true})
			return primary.ImageRef, true
		}
		tag := "wardyn-workspace/" + primary.ID.String() + ":devcontainer"
		if built, err := s.cfg.ImageBuilder.BuildDevcontainer(ctx, url, repoSrc.Ref, tag, logSink); err == nil {
			if s.cfg.Store != nil {
				if _, uerr := s.cfg.Store.SetWorkspaceBuiltImage(ctx, primary.ID, built, repoHash); uerr != nil {
					buildAudit("success", map[string]any{"source": "repo-devcontainer", "image": built, "cache_warn": uerr.Error()})
					return built, true
				}
			}
			buildAudit("success", map[string]any{"source": "repo-devcontainer", "image": built})
			return built, true
		} else {
			buildAudit("failure", map[string]any{"source": "repo-devcontainer", "error": err.Error()})
			return "", false
		}
	}
	if len(primary.Sources) > 0 && primary.Sources[0].Type == types.WorkspaceSourceTypeRepo && p.HasDevcontainer {
		if _, ssh := sshCloneHost(primary.Sources[0].Source); ssh {
			buildAudit("skipped", map[string]any{"source": "repo-devcontainer", "reason": "ssh-source-not-buildable-by-image-builder"})
		}
	}

	hash := p.CacheKey()
	// Reuse a cached generated image when the profile is unchanged.
	if primary.ImageRef != "" && primary.BuiltProfileHash == hash && s.cachedImageStillPresent(ctx, primary.ImageRef) {
		return primary.ImageRef, true
	}

	// Generate a devcontainer for the detected toolchain (the standard
	// agent-tool install rides unconditionally — gen.go) and build it.
	files, gerr := workspacescan.GenerateDevcontainer(p)
	if gerr != nil {
		buildAudit("failure", map[string]any{"source": "generated-devcontainer", "error": gerr.Error()})
		return "", false
	}
	tag := "wardyn-workspace/" + primary.ID.String() + ":" + hash[:12]
	built, berr := s.cfg.ImageBuilder.BuildFromDevcontainerFiles(ctx, files, tag, logSink)
	if berr != nil {
		buildAudit("failure", map[string]any{"source": "generated-devcontainer", "error": berr.Error()})
		return "", false
	}
	// Cache the built image on the workspace for reuse by later runs — a SCOPED
	// write: the previous full-row UpdateWorkspace here replayed a stale
	// pre-launch snapshot over every concurrently-persisted async column
	// (active_run_id, record_results, verify state).
	if _, uerr := s.cfg.Store.SetWorkspaceBuiltImage(ctx, primary.ID, built, hash); uerr != nil {
		// Non-fatal: the image built and is usable now; caching just missed.
		buildAudit("success", map[string]any{"source": "generated-devcontainer", "image": built, "cache_warn": uerr.Error()})
		return built, true
	}
	buildAudit("success", map[string]any{"source": "generated-devcontainer", "image": built})
	return built, true
}

// repoOwnDevcontainerURL returns the clone URL resolveWorkspaceImage builds
// FROM when ws's primary source is a repo carrying its own devcontainer
// (HasDevcontainer) — or "" when that lane does not apply: no repo primary,
// no devcontainer, an SSH source (the image builder cannot clone one — see
// resolveWorkspaceImage's comment), or an unparseable source.
func repoOwnDevcontainerURL(ws types.Workspace, p workspacescan.WorkspaceProfile) string {
	if len(ws.Sources) == 0 || ws.Sources[0].Type != types.WorkspaceSourceTypeRepo || !p.HasDevcontainer {
		return ""
	}
	repoSrc := ws.Sources[0]
	if _, ssh := sshCloneHost(repoSrc.Source); ssh {
		return ""
	}
	return repoCloneURL(repoSrc.Source)
}

// workspaceRunImage is the image a verify/record run executes in: the
// workspace's BUILT devcontainer image (built now if needed), falling back to
// the convention agent image when no builder is configured.
func (s *Server) workspaceRunImage(ctx context.Context, runID uuid.UUID, ws types.Workspace) string {
	// W20-W20-record-image-4: bound the build the same way the other three
	// doors do (runs_create.go) — the record/verify launch callers detach
	// from request cancellation before reaching here (context.WithoutCancel),
	// so without its OWN deadline this build could run indefinitely, holding
	// the workspace's serial import-step claim well past the reaper's grace
	// (undispatchedGrace = 2*imageBuildTimeout).
	buildCtx, cancel := context.WithTimeout(ctx, imageBuildTimeout)
	defer cancel()
	if built, ok := s.resolveWorkspaceImage(buildCtx, runID, ws, nil); ok {
		return built
	}
	return agentImage("claude-code", s.cfg.AgentImages)
}
