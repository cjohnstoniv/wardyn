// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Per-file diff stat for a run's workspace — the Files-changed widget on the
// run-detail cockpit.
//
// WHY NOT workspacescan: that package is an ONBOARDING scanner (what a repo
// needs), not a differ, and it reads the host filesystem. This read has to
// answer "what has the agent changed so far", which is a question about the
// sandbox's own working tree, mid-run.
//
// WHY NOT the host filesystem: run.WorkspacePath is the HOST directory the
// workspace is bind-mounted from, so reading it from wardynd would work on
// docker-on-this-host and silently return nothing on k8s (where the mount is a
// PVC on another node) — the daemon must never assume it shares a filesystem
// with the sandbox.
//
// So: one ExecStream into the sandbox, running git. Substrate-agnostic — it
// works wherever the SSH gateway's exec channels already work.
package api

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

const (
	// runFilesTimeout bounds the WHOLE read (launch + both git invocations +
	// parse). The console POLLS this endpoint, so a sandbox whose git is wedged
	// must fail fast rather than pile up wardynd goroutines holding exec
	// streams open. ponytail: one flat deadline, not a per-phase budget —
	// upgrade path is a per-phase split if a slow-but-working repo ever trips
	// this (a huge untracked tree can make `git status` the long pole).
	runFilesTimeout = 5 * time.Second
	// runFilesMaxFiles caps how many rows we return. A refactor run can touch
	// thousands of files and neither the widget nor the JSON response wants
	// them; past the cap the response says so with truncated=true rather than
	// quietly returning a short list that reads like the whole truth.
	runFilesMaxFiles = 500
	// runFilesMaxOutput bounds the TOTAL bytes read off the exec stream (parse
	// AND the drain-to-EOF inside parseRunFiles). The 5s ctx above cannot reach
	// a hijacked docker stream (the client uses ctx for the dial only), so
	// without a byte bound a chatty sandbox parks this handler forever — the
	// exact hang the sibling endpoint's runResourcesMaxOutput already closes.
	runFilesMaxOutput = 512 << 10
	// runFilesMaxLine bounds ONE line of git output. A path is bounded by the
	// filesystem, but the sandbox's stdout is not something wardynd controls,
	// so the scanner gets an explicit ceiling instead of the 64KiB default —
	// and a line over it sets truncated (see parseRunFiles), never a silent drop.
	runFilesMaxLine = 1 << 20
	// runFilesSeparator (ASCII RS, 0x1e) divides the numstat section from the
	// porcelain section of the ONE script's stdout. It cannot occur in a path
	// git prints: git C-quotes control characters in both --numstat and
	// --porcelain output.
	runFilesSeparator = "\x1e"

	runFilesVCSGit  = "git"
	runFilesVCSNone = "none"
	// The script could not determine anything: git ran and failed for a reason
	// that is NOT "this is not a work tree" — missing from the image, a repo it
	// refuses to read. Distinct from "none" so the console can stop blaming the
	// workspace for a runner-image problem.
	runFilesVCSUnknown = "unknown"

	// runFilesExitNoWorkTree is the script's own exit code for "there is no git
	// work tree here", kept distinct from every other nonzero exit precisely so
	// the two can be told apart above.
	runFilesExitNoWorkTree = 3

	// runFilesUnsupportedMsg is the honest 501 reason for a runner with no
	// ExecStream primitive at all: there is no other way to read the sandbox's
	// working tree, so this is a capability statement, not a failure.
	runFilesUnsupportedMsg = "per-file diff stat needs sandbox exec, which this runner does not support"
)

// runFilesScript is the ONE exec behind this endpoint: the numstat section, a
// separator line, then the porcelain section, joined on path in Go.
//
// Two invocations rather than one because they answer different halves of the
// question — numstat has the +/− counts but never lists untracked files;
// porcelain has the status letter (and the untracked files) but no counts.
//
// Notes on the flags, none of which are cosmetic:
//
//   - safe.directory='*' — the workspace is a bind mount, so its owning uid on
//     the host frequently is not the sandbox user's. Without this git refuses
//     the repo outright ("detected dubious ownership") and we would report
//     vcs=none for a directory that is very much a git work tree. Read-only
//     commands only; this widens nothing about what the agent may do.
//   - core.quotepath=false — keeps non-ASCII paths readable instead of
//     \NNN-escaped. Control characters are still quoted (see runFilesSeparator).
//   - --no-renames — keeps the numstat path field a PLAIN path. With rename
//     detection on, git writes `old => new` (and brace-compacted variants)
//     into the path column, which no honest join-on-path survives. A rename
//     then shows as its delete+add pair, and porcelain's own `R old -> new`
//     row still labels the new path.
//
// W is the in-sandbox workspace dir, passed via ExecSpec.Env — never
// interpolated into this string. The `:-` default is not belt-and-braces: a
// substrate that dropped exec env would otherwise `cd ""`, fail, and report
// vcs=none for a workspace we never actually looked at.
//
// WHY IT SEARCHES rather than trusting one path: /home/agent/work is only the
// FALLBACK mount target (composerWorkspaceTarget) — a workspace source may set
// its own Target (workspace_run.go), and the run row does not carry the
// resolved path, so a hardcoded guess would report vcs=none for a real repo
// mounted somewhere else. That is the worst failure mode this endpoint has: not
// an error, but a confident "nothing changed". So it tries W, then the exec's
// OWN working directory (the image's WORKDIR, i.e. where the agent actually
// works), and — either way — REPORTS the directory it settled on, so a wrong
// path shows up in the UI as a named path instead of a silent empty list.
//
// A --repo run is the common case this search exists for: agent-run clones it
// to $W/<repo leaf> (deploy/images/common/agent-run-lib.sh), so W itself is a
// plain directory and only its CHILD is a work tree — the script therefore tries
// $W/$R (R = the repo leaf, from run.Repo) first, then W, then the exec's cwd,
// then every child of W. Before that ordering existed every repo run read as
// vcs=none at /home/agent/work: "nothing changed", confidently, for a sandbox
// that had a full clone one directory down.
//
// exit 3 means "there is no git work tree here" — a fact about the workspace,
// reported as 200 vcs=none. Any other nonzero exit lands in the same place
// (git missing from the image, a repo we cannot read): in every case we did
// not obtain a diff, and saying so beats inventing one.
const runFilesScript = `D=""
W="${W:-/home/agent/work}"
# Candidates, most specific first: the run's own repo clone (agent-run clones a
# --repo run to $W/<repo-leaf>, one level BELOW the mount target, so W itself is
# never a work tree for those runs), then W, then the exec's cwd, then any child
# of W that is a work tree (a workspace with repo sources lands them under W too).
set -- ${R:+"$W/$R"} "$W" .
for d in "$W"/*/; do [ -d "$d" ] && set -- "$@" "$d"; done
for c in "$@"; do
  [ -d "$c" ] || continue
  if (cd "$c" && git -c safe.directory='*' rev-parse --is-inside-work-tree >/dev/null 2>&1); then
    D=$(cd "$c" && pwd)
    break
  fi
done
if [ -z "$D" ]; then printf 'path=%s\n' "$W"; exit 3; fi
printf 'path=%s\n' "$D"
printf '\036\n'
cd "$D"
git -c safe.directory='*' -c core.quotepath=false diff --numstat --no-renames HEAD
printf '\036\n'
git -c safe.directory='*' -c core.quotepath=false status --porcelain
`

// runFileStat is ONE changed path in the run's workspace.
//
// Added/Deleted are POINTERS on purpose: a count we did not actually read —
// binary file, untracked file (numstat never lists one), unparsable field — is
// ABSENT from the JSON, never 0. "+0 −0" is a claim that the file changed by
// nothing, which for a binary asset the agent just rewrote is simply false.
type runFileStat struct {
	Path string `json:"path"`
	// Status is git's porcelain code with its position padding trimmed: "M",
	// "A", "D", "??", "MM"… Absent when the file appeared in the diff but not
	// in `git status` (rare, e.g. assume-unchanged) — an unknown status is
	// omitted rather than guessed at.
	Status  string `json:"status,omitempty"`
	Added   *int   `json:"added,omitempty"`
	Deleted *int   `json:"deleted,omitempty"`
	// Binary marks a file git declined to count lines for ("-" in numstat).
	Binary bool `json:"binary,omitempty"`
}

// runFilesResponse is the endpoint's body. Files is never null (the widget
// renders a list, and `null` is not an empty list); Truncated is never omitted
// — "we stopped counting" has to be visible in every response, including the
// ones where it is false.
type runFilesResponse struct {
	VCS   string        `json:"vcs"`
	Files []runFileStat `json:"files"`
	// Path is the in-sandbox directory actually inspected. Present on BOTH
	// outcomes on purpose: on vcs:"none" it is the evidence that turns "no repo
	// here" into "no repo AT THIS PATH", which is what an operator needs when a
	// workspace is mounted at a non-default target.
	Path      string `json:"path,omitempty"`
	Truncated bool   `json:"truncated"`
}

// handleRunFiles serves GET /api/v1/runs/{id}/files — the per-file diff stat of
// the run's workspace, read from INSIDE the sandbox (see the package comment).
//
// Owner-or-admin via getRunAuthorized: a foreign run gets the byte-identical
// 404 a missing run does (no existence oracle), same gate as GET /runs/{id}.
//
// Responses:
//
//	200 {"vcs":"git","truncated":false,"files":[
//	      {"path":"internal/api/run_files.go","status":"M","added":120,"deleted":4},
//	      {"path":"docs/logo.png","status":"A","binary":true},
//	      {"path":"scratch.txt","status":"??"}]}
//	200 {"vcs":"none","files":[],"truncated":false}   — not a git work tree
//	409  the run never got a sandbox (nothing to read)
//	501  the runner has no ExecStream primitive
//	500  the exec launched but the read failed
//
// AUDIT: failures only. The console polls this; a row per poll tick would bury
// the trail this project treats as its system of record. A successful read
// mints nothing, opens no network path, and changes no run state — there is
// nothing to non-repudiate.
func (s *Server) handleRunFiles(w http.ResponseWriter, r *http.Request) {
	id, ok := parseIDParam(w, r, "id", "run")
	if !ok {
		return
	}
	run, ok := s.getRunAuthorized(w, r, id)
	if !ok {
		return
	}
	if s.cfg.Runner == nil {
		writeError(w, http.StatusNotImplemented, runFilesUnsupportedMsg)
		return
	}
	// A finished run's sandbox is gone (finalize clears the ref on a clean
	// teardown; kill/idle-stop leave a stale one) — refuse crisply BEFORE the
	// ref check so both shapes get the same honest answer instead of a 500 +
	// an audit failure row per widget mount.
	if run.State.IsTerminal() {
		writeError(w, http.StatusConflict, "run has finished; its sandbox is gone (state="+string(run.State)+")")
		return
	}
	// A run that never dispatched (or whose sandbox was never recorded) has no
	// working tree to read. A crisp 409 beats handing an empty ref to the
	// driver and surfacing whatever error it invents — same shape as
	// handleAttachTicket's "not attachable" refusal.
	if run.SandboxRef == "" {
		writeError(w, http.StatusConflict, "run has no sandbox to read (state="+string(run.State)+")")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), runFilesTimeout)
	defer cancel()

	sess, err := s.cfg.Runner.ExecStream(ctx, run.SandboxRef, runner.ExecSpec{
		Argv: []string{"/bin/sh", "-c", runFilesScript},
		// R is the repo clone's directory name under W for a --repo run (the leaf
		// of org/name); empty otherwise. The script tries $W/$R before W itself.
		Env: []string{"W=" + composerWorkspaceTarget, "R=" + repoCloneLeaf(run.Repo)},
	})
	if err != nil {
		s.auditRunFilesFailure(r, id, err)
		if errors.Is(err, runner.ErrExecStreamUnsupported) {
			writeError(w, http.StatusNotImplemented, runFilesUnsupportedMsg+" ("+err.Error()+")")
			return
		}
		writeError(w, http.StatusInternalServerError, "read workspace files: "+err.Error())
		return
	}
	if sess == nil {
		writeError(w, http.StatusInternalServerError, "read workspace files: runner returned no exec session")
		return
	}
	defer func() {
		if sess.Close != nil {
			_ = sess.Close() // tears down ONLY this exec, never the sandbox
		}
	}()

	// STREAMING CONTRACT (runner.ExecSession's doc, and the reason
	// drainExecStderr exists in the SSH gateway): Stdout and Stderr are
	// UNBUFFERED io.Pipes fed by ONE demux goroutine — a single undrained
	// stderr byte blocks that goroutine, Stdout, AND Wait. git writes to stderr
	// on every path we care about (a non-repo, a missing HEAD), i.e. exactly
	// the cases this endpoint exists to report, so the drain starts BEFORE the
	// first Stdout read or the handler hangs on its most ordinary input.
	//
	// Discarded rather than captured: joining a capture goroutine would add a
	// second place this handler can block, and the exit code plus err already
	// carry everything the response says.
	if sess.Stderr != nil {
		go func() { _, _ = io.Copy(io.Discard, sess.Stderr) }()
	}

	inspectedPath, files, truncated := parseRunFiles(io.LimitReader(sess.Stdout, runFilesMaxOutput))

	if sess.Wait != nil {
		code, werr := sess.Wait()
		if werr != nil {
			// The exec itself broke (deadline, connection loss) — distinct from
			// "git said no", and a genuine failure of the read.
			s.auditRunFilesFailure(r, id, werr)
			writeError(w, http.StatusInternalServerError, "read workspace files: "+werr.Error())
			return
		}
		// exit 3 is the script's OWN "there is no git work tree here" signal.
		// Any OTHER nonzero exit means git ran and failed — most often it is
		// missing from the image entirely. Those are different facts and used
		// to collapse into the same one: the widget asserted "No git repository
		// at <path>", so an operator re-mounted their workspace to fix an image
		// problem. vcs:"unknown" says only what we know, which is that we could
		// not tell.
		if code == runFilesExitNoWorkTree {
			// A FACT about the workspace, not a failure: 200, an honest empty
			// list, no audit row. NAME the directory we looked in — a bare
			// "not a git repository" is indistinguishable from "we looked in
			// the wrong place", and the mount target is configurable per
			// workspace source.
			writeJSON(w, http.StatusOK, runFilesResponse{
				VCS: runFilesVCSNone, Files: []runFileStat{}, Path: inspectedPath,
			})
			return
		}
		if code != 0 {
			writeJSON(w, http.StatusOK, runFilesResponse{
				VCS: runFilesVCSUnknown, Files: []runFileStat{}, Path: inspectedPath,
			})
			return
		}
	}
	writeJSON(w, http.StatusOK, runFilesResponse{
		VCS: runFilesVCSGit, Files: files, Path: inspectedPath, Truncated: truncated,
	})
}

// repoCloneLeaf is the directory agent-run clones a run's repo into under the
// workspace mount target: the last path element of "org/name" (or of a URL),
// minus a ".git" suffix. Empty for a run with no repo.
func repoCloneLeaf(repo string) string {
	repo = strings.TrimSuffix(strings.TrimRight(strings.TrimSpace(repo), "/"), ".git")
	if i := strings.LastIndex(repo, "/"); i >= 0 {
		repo = repo[i+1:]
	}
	return repo
}

// auditRunFilesFailure records the FAILURE-only run.files audit row (see
// handleRunFiles' doc for why success is silent).
func (s *Server) auditRunFilesFailure(r *http.Request, runID uuid.UUID, err error) {
	s.recordAudit(r.Context(), s.auditEvent(&runID, actorTypeFromRequest(r), principalFromRequest(r),
		"run.files", runID.String(), "failure", mustJSON(map[string]any{"error": err.Error()})))
}

// parseRunFiles reads runFilesScript's stdout — the numstat section, the
// separator, then the porcelain section — and joins the two on path, keeping
// numstat's order and appending porcelain-only paths (untracked files) after it.
//
// It ALWAYS drains stdout to EOF, including after the cap is hit or the scanner
// gives up on an over-long line: an undrained pipe blocks the demux goroutine
// and therefore Wait, which is the same hang the stderr drain above avoids.
func parseRunFiles(stdout io.Reader) (path string, files []runFileStat, truncated bool) {
	files = []runFileStat{}
	if stdout == nil {
		return "", files, false
	}
	defer func() { _, _ = io.Copy(io.Discard, stdout) }()

	byPath := make(map[string]int)
	// Three sections, separated by runFilesSeparator: the inspected path, the
	// numstat rows, then the porcelain rows.
	section := 0
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, bufio.MaxScanTokenSize), runFilesMaxLine)
	for sc.Scan() {
		line := sc.Text()
		if line == runFilesSeparator {
			section++
			continue
		}
		if section == 0 {
			path = strings.TrimPrefix(line, "path=")
			continue
		}
		var (
			f  runFileStat
			ok bool
		)
		if section >= 2 {
			f, ok = parsePorcelainLine(line)
		} else {
			f, ok = parseNumstatLine(line)
		}
		if !ok {
			continue
		}
		if i, seen := byPath[f.Path]; seen {
			mergeRunFileStat(&files[i], f)
			continue
		}
		if len(files) >= runFilesMaxFiles {
			// Keep scanning (the pipe must reach EOF) but stop collecting, and
			// say so — a short list presented as complete is the lie here.
			truncated = true
			continue
		}
		byPath[f.Path] = len(files)
		files = append(files, f)
	}
	if sc.Err() != nil {
		// A line over runFilesMaxLine (or a read error) stops the scan with
		// output still unread: rows were dropped, so admit it.
		truncated = true
	}
	return path, files, truncated
}

// parseNumstatLine parses one `git diff --numstat` row: "<added>\t<deleted>\t<path>".
// A binary file is "-\t-\t<path>" — git declined to count lines, which is
// reported as binary:true with NO counts, never as +0/−0.
func parseNumstatLine(line string) (runFileStat, bool) {
	parts := strings.SplitN(line, "\t", 3)
	if len(parts) != 3 {
		return runFileStat{}, false
	}
	path := strings.TrimSpace(parts[2])
	if path == "" {
		return runFileStat{}, false
	}
	// Either side being "-" means git refused to count this file's lines.
	if parts[0] == "-" || parts[1] == "-" {
		return runFileStat{Path: path, Binary: true}, true
	}
	return runFileStat{Path: path, Added: parseRunFileCount(parts[0]), Deleted: parseRunFileCount(parts[1])}, true
}

// parseRunFileCount returns nil for anything it cannot read as a count, so an
// unreadable field lands in the JSON as ABSENT rather than as a confident zero.
func parseRunFileCount(s string) *int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n < 0 {
		return nil
	}
	return &n
}

// parsePorcelainLine parses one `git status --porcelain` row: two status
// columns (staged, worktree), a space, then the path — "?? scratch.txt",
// " M main.go", "R  old.go -> new.go".
//
// A rename row is joined on its NEW path: that is the path --no-renames
// numstat reports the additions against. The old path keeps whatever numstat
// said about it and simply carries no status letter.
func parsePorcelainLine(line string) (runFileStat, bool) {
	if len(line) < 4 {
		return runFileStat{}, false
	}
	code := strings.TrimSpace(line[:2])
	path := strings.TrimSpace(line[3:])
	if strings.HasPrefix(code, "R") || strings.HasPrefix(code, "C") {
		if i := strings.LastIndex(path, " -> "); i >= 0 {
			path = strings.TrimSpace(path[i+len(" -> "):])
		}
	}
	if code == "" || path == "" {
		return runFileStat{}, false
	}
	return runFileStat{Path: path, Status: code}, true
}

// mergeRunFileStat folds a porcelain row onto the numstat row for the same path
// (the join). Only fields the incoming row actually carries are copied — a
// section that says nothing about a count must never blank one the other
// section did report.
func mergeRunFileStat(dst *runFileStat, src runFileStat) {
	if src.Status != "" {
		dst.Status = src.Status
	}
	if src.Added != nil {
		dst.Added = src.Added
	}
	if src.Deleted != nil {
		dst.Deleted = src.Deleted
	}
	if src.Binary {
		dst.Binary = true
	}
}
