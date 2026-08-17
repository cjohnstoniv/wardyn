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
// writes it into the host source dir — the host-write half of what used to be
// handleFinalizeWorkspace's optional emit, extracted on its own now that there
// is no more "finalize to ready" step. LOCAL-DIR ONLY: a repo-only workspace
// has no host path to write into (regenerate + commit yourself via
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
		writeError(w, http.StatusInternalServerError, "write env-as-code: "+werr.Error())
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
		mustJSON(map[string]any{"files": len(files), "skipped": len(skipped)})))
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
	// workspace's own registry/custom/byo pick actually boots (WSPIPE-9).
	var baseRef string
	if b := ws.BaseImage; b != nil && b.Kind != "recommended" && strings.TrimSpace(b.Image) != "" {
		baseRef = b.Image
	}
	files, gerr := workspacescan.EmitEnvAsCode(profile, artifactBases, baseRef)
	if gerr != nil {
		writeError(w, http.StatusInternalServerError, "generate env-as-code: "+gerr.Error())
		return nil, false
	}
	return files, true
}

// handleGetEnvAsCode re-generates the committable env-as-code for a workspace.
// Finalize hands these files back exactly once, in its response body, and a repo
// workspace has nowhere on the host to write them — so without this the content
// the operator is meant to COMMIT dies with the import dialog. Nothing is
// persisted: the files are deterministic from stored state, so this reflects a
// later re-scan or setup-command edit rather than a finalize-time snapshot.
func (s *Server) handleGetEnvAsCode(w http.ResponseWriter, r *http.Request) {
	id, ok := parseIDParam(w, r, "id", "workspace")
	if !ok {
		return
	}
	ws, ok := s.getWorkspaceOr404(w, r, id)
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
// the fixed, safe outputs of EmitEnvAsCode (.devcontainer/devcontainer.json,
// AGENTS.md, plus any artifact-redirect config like .npmrc/.cargo/config.toml).
//
// Every write goes through os.Root, which resolves each path component INSIDE
// the kernel and refuses to traverse or land on a symlink escaping root. A
// lexical filepath.Join check cannot do this: the tree we write into is exactly
// the tree the sandbox agent (and any imported repo — git carries symlinks) can
// write to, so `<root>/AGENTS.md -> ~/.bashrc` would otherwise be FOLLOWED and
// truncate an operator file, wardynd running as the operator in host mode. The
// lexical check stays as a cheap first gate against a `..` in a generated key.
//
// workspacescan.EnvAsCodeDockerfilePath is special-cased: every OTHER emitted
// key is Wardyn's own narrow, regenerate-on-demand output (the card's own
// copy promises "regenerate after a rescan or a requirements change" for
// devcontainer.json/AGENTS.md, and the artifact-redirect stubs are one-line
// registry pointers with no plausible hand-authored equivalent) — but
// .devcontainer/Dockerfile is exactly where an operator using devcontainers
// already puts their OWN hand-written Dockerfile, unrelated to Wardyn. A
// pre-existing file there is protected UNLESS its content is byte-identical
// to what Wardyn would write right now — genAgentToolDockerfile is a pure
// function of tools, so that can only be Wardyn's own previously-emitted
// stub, never an operator's coincidence — in which case it is refreshed like
// every other key, not reported skipped. Keying the guard on existence alone
// would make the SECOND "Write into the directory" click always find the
// FIRST click's own stub in the way, permanently closing the regenerate path
// for this one file and falsifying the card's "won't include the agent CLI
// unless you add that yourself" copy. Only a Dockerfile whose content
// actually differs — genuinely the operator's — is left alone and reported.
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
