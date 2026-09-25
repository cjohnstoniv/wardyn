// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/internal/workspacescan"
)

// workspace_envcode.go owns env-as-code: turning a workspace's CURRENT scanned
// profile into committable files (.devcontainer/*, AGENTS.md, the
// artifact-registry redirect stubs) and, for a local_dir composition, writing
// them into the host source tree. Split out of workspaces.go when that file
// crossed its size cap; the grouping is the real seam — the read endpoint, the
// write endpoint and the shared generator must never drift, and the host-write
// half carries its own os.Root symlink-escape argument that has nothing to do
// with workspace CRUD. Generation itself lives in internal/workspacescan
// (EmitEnvAsCode); nothing here is persisted.

// handleWriteEnvAsCode generates committable env-as-code from the workspace's
// CURRENT scanned profile (base + language features + artifact-registry
// redirects, plus an AGENTS.md documenting the detected toolchain/commands) and
// writes it into the host source dir — the host-write half of env-as-code
// generation. LOCAL-DIR ONLY: a repo-only workspace has no host path to
// write into (regenerate + commit yourself via
// GET /workspaces/{id}/env-as-code, which stays the read path regardless of
// composition). Writes to the FIRST local_dir source when the workspace has
// more than one.
func (s *Server) handleWriteEnvAsCode(w http.ResponseWriter, r *http.Request) {
	id, ok := parseIDParam(w, r, "id", "workspace")
	if !ok {
		return
	}
	ws, ok := s.getWorkspaceOr404(w, r, id)
	if !ok {
		return
	}
	localDirs := workspaceSourcesOfType(ws, types.WorkspaceSourceTypeLocalDir)
	if len(localDirs) == 0 {
		writeError(w, http.StatusUnprocessableEntity,
			"env-as-code can only be written to disk for a workspace with a local_dir source (a repo/ephemeral-only "+
				"workspace has no host path — use GET /workspaces/{id}/env-as-code and commit the files yourself)")
		return
	}
	files, ok := s.envAsCodeFor(w, r, ws)
	if !ok {
		return
	}
	skipped, werr := writeEnvAsCode(localDirs[0].Path, files)
	if werr != nil {
		writeServerError(w, r, "write env-as-code", werr)
		return
	}
	// written_files must name only what was actually written — a skipped key
	// (an operator file writeEnvAsCode refused to clobber) staying in this map
	// would tell the caller it was overwritten when it was not.
	for _, rel := range skipped {
		delete(files, rel)
	}
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		"workspace.envcode.write", id.String(), "success",
		auditWorkspaceData(r, ws.OwnedBy, map[string]any{"files": len(files), "skipped": len(skipped)})))
	writeJSON(w, http.StatusOK, map[string]any{"written_files": files, "skipped_files": skipped})
}

// envAsCodeFor generates the committable env-as-code for a workspace from its
// CURRENT scanned profile. Shared by handleWriteEnvAsCode and
// handleGetEnvAsCode so the two generations can never drift. It writes its own
// error response (422 when the workspace has no scanned profile, 500 when the
// generator fails) and returns ok=false, mirroring getWorkspaceOr404.
func (s *Server) envAsCodeFor(w http.ResponseWriter, r *http.Request, ws types.Workspace) (map[string]string, bool) {
	profile, ok := workspaceProfile(ws)
	if !ok {
		writeError(w, http.StatusUnprocessableEntity, "workspace has no scanned profile to emit from")
		return nil, false
	}
	// Fold the operator-wide artifact-registry redirects (URL-only) into the
	// committable output so an exported workspace pulls from the corp mirror.
	// Best-effort: a store error / no site-config just omits them.
	var artifactBases map[string]string
	if s.cfg.Store != nil {
		if sc, scErr := s.cfg.Store.GetSiteConfig(r.Context()); scErr == nil {
			artifactBases = artifactBaseURLs(sc)
		}
	}
	// baseRef: the SAME "explicit non-recommended choice" predicate
	// resolveWorkspaceImage uses (workspace_run.go) — WITHOUT it every export
	// described the generic devcontainer base regardless of what this
	// workspace's own registry/custom/byo pick actually boots.
	var baseRef string
	if b := ws.BaseImage; b != nil && b.Kind != "recommended" && strings.TrimSpace(b.Image) != "" {
		baseRef = b.Image
	}
	files, gerr := workspacescan.EmitEnvAsCode(profile, artifactBases, baseRef)
	if gerr != nil {
		writeServerError(w, r, "generate env-as-code", gerr)
		return nil, false
	}
	return files, true
}

// handleGetEnvAsCode re-generates the committable env-as-code for a workspace.
// Finalize hands these files back exactly once, and a repo workspace has nowhere
// on the host to write them, so this is how the operator gets them to COMMIT.
// Nothing is persisted: the files are deterministic from stored state.
//
// Owner-or-super, not member-readable, because of the emitted CONTENT: the
// `FROM <base_image.image>` registry coordinate redactWorkspaceForRead blanks,
// the site-config artifact redirects (artifactBaseURLs) GET /site-config
// withholds from members, and the setup commands. A committable response has no
// useful projection. getWorkspaceAuthorized's population is exactly
// workspaceReadFull, with the right refusals (403 for an operator-owned row, the
// byte-identical 404 for another member's).
func (s *Server) handleGetEnvAsCode(w http.ResponseWriter, r *http.Request) {
	id, ok := parseIDParam(w, r, "id", "workspace")
	if !ok {
		return
	}
	ws, ok := s.getWorkspaceAuthorized(w, r, id)
	if !ok {
		return
	}
	files, ok := s.envAsCodeFor(w, r, ws)
	if !ok {
		return
	}
	// Same key finalize returns, so a client renders either response identically.
	writeJSON(w, http.StatusOK, map[string]any{"emitted_files": files})
}

// writeEnvAsCode writes generated env-as-code files under root, returning the
// subset of keys it left untouched because they already existed. Paths are
// the fixed, safe outputs of EmitEnvAsCode.
// Every write goes through os.Root, which refuses to traverse or land on a
// symlink escaping root: the sandbox agent (and any imported repo) can write
// this tree, so `<root>/AGENTS.md -> ~/.bashrc` would otherwise truncate an
// operator file. The lexical check is only a cheap first gate against `..`.
// workspacescan.EnvAsCodeDockerfilePath is special-cased: .devcontainer/Dockerfile
// is where an operator already keeps their OWN Dockerfile, so an existing one is
// left alone and reported UNLESS byte-identical to what Wardyn would write now
// (genAgentToolDockerfile is pure, so that is Wardyn's own stub) — keying on
// existence alone would permanently block the regenerate path for this file.
func writeEnvAsCode(rootPath string, files map[string]string) ([]string, error) {
	cleanRoot := filepath.Clean(rootPath)
	root, err := os.OpenRoot(cleanRoot)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	var skipped []string
	for rel, content := range files {
		dst := filepath.Join(cleanRoot, filepath.FromSlash(rel))
		if !strings.HasPrefix(dst, cleanRoot+string(filepath.Separator)) {
			return nil, fmt.Errorf("refusing to write outside workspace: %s", rel)
		}
		relPath := filepath.FromSlash(rel)
		if dir := filepath.Dir(relPath); dir != "." {
			if err := root.MkdirAll(dir, 0o755); err != nil {
				return nil, fmt.Errorf("refusing to write %s: %w", rel, err)
			}
		}
		flag := os.O_WRONLY | os.O_CREATE | os.O_TRUNC
		if rel == workspacescan.EnvAsCodeDockerfilePath {
			// The EXCL guard is needed only when a pre-existing file can't
			// already be PROVEN to be Wardyn's own: byte-identical to what
			// would be written right now (genAgentToolDockerfile is a pure
			// function of tools, so only Wardyn's own previous stub can
			// match). A read error — including "does not exist" — falls
			// through to the guard, the safe default: create fresh, or fail
			// closed into the EEXIST-skip path below rather than guess.
			if existing, rerr := root.ReadFile(relPath); rerr != nil || string(existing) != content {
				flag = os.O_WRONLY | os.O_CREATE | os.O_EXCL
			}
		}
		f, err := root.OpenFile(relPath, flag, 0o644)
		if err != nil {
			if os.IsExist(err) {
				skipped = append(skipped, rel)
				continue
			}
			return nil, fmt.Errorf("refusing to write %s: %w", rel, err)
		}
		_, werr := f.WriteString(content)
		cerr := f.Close()
		if werr != nil {
			return nil, werr
		}
		if cerr != nil {
			return nil, cerr
		}
	}
	slices.Sort(skipped)
	return skipped, nil
}
