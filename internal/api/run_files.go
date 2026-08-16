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

import "net/http"

// handleRunFiles serves GET /api/v1/runs/{id}/files.
//
// CONTRACT (lane A1 — fill this in):
//
//   - Gate: parseIDParam + s.getRunAuthorized (owner-or-admin; a foreign run
//     404s). Follow handleAttachTicket (attach_ticket.go:113) exactly.
//
//   - Read: s.cfg.Runner.ExecStream(ctx, run.SandboxRef, runner.ExecSpec{
//     Argv: []string{"/bin/sh", "-c", script}, Env: []string{"W=" + workdir}})
//     with a script that emits `git diff --numstat HEAD`, a \036 separator,
//     then `git status --porcelain`. Precedent: sshgateway_channels.go:527.
//
//   - STREAMING CONTRACT: drain Stderr CONCURRENTLY with Stdout. ExecSession's
//     own doc is explicit — one undrained stderr byte blocks the demux
//     goroutine, Stdout, AND Wait. This is the single easiest way to hang the
//     handler; start the stderr drain before the first Stdout read.
//
//   - Bound it: a context deadline (a few seconds), and a cap of 500 files with
//     an explicit `truncated` flag. Never silently drop rows.
//
//   - Honesty:
//     binary files report "-"/"-" in numstat  -> {"binary": true}, never +0/-0.
//     not a git work tree (git exits non-zero) -> 200 {"vcs":"none","files":[]}.
//     errors.Is(err, runner.ErrExecStreamUnsupported) -> 501 with that reason.
//     A missing number is absent, never zero.
//
//   - Audit: record run.files only on FAILURE. A read the console polls should
//     not write an audit row per tick.
func (s *Server) handleRunFiles(w http.ResponseWriter, r *http.Request) {
	// ponytail: honest 501 until lane A1 lands — the widget already renders an
	// unsupported state, so an unbuilt endpoint degrades instead of lying.
	writeError(w, http.StatusNotImplemented, "per-file diff stat is not implemented on this build")
}
