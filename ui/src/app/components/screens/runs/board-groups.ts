/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Runs list's shared naming helpers — headline fallback, the repo/
// workspace label, and the short id. No JSX, so run-context-row.tsx (an
// approval card, not the board) can share the same headline rule without
// pulling in anything board-shaped.
//
// #1197 L3: the attention rule that used to live here (approvalSignals /
// runAttention / needsAttention / needsYou / groupWaitBreakdown) is GONE —
// folded into the server (internal/types/attention.go's RunAttention,
// projected by GET /runs?view=), which is the one truth now. See
// screens/runs/runs-model.ts for the row/section rule that replaced it, and
// FINAL-PR-1230.md for why moving it server-side was the right call. Title
// grouping (titleGroups) is also gone: the Runs landing page groups by need
// then time (design.md H-6), not by title — "Group by Title" is an L4 filter
// option, not a standing grouping rule.
import type { AgentRun } from "../../../lib/types";

// The last rung of the headline chain, and the repo slot's stand-in — both
// name what the run actually is rather than rendering a blank or a dash.
// "Ephemeral scratch — no repo" is the New Run screen's own workspace string
// (new-run-screen.tsx), so the board and the form agree on what "no workspace"
// is called.
export const INTERACTIVE_HEADLINE = "Interactive session";
export const NO_REPO = "Ephemeral scratch — no repo";

function basename(path: string | undefined): string {
  const trimmed = (path ?? "").replace(/\/+$/, "");
  return trimmed.split("/").pop() ?? "";
}

/**
 * What one row calls itself, with honest fallbacks all the way down. Nothing
 * here is invented: each rung is a fact the run carries, and "Interactive
 * session" is claimed only for a run that IS one — a nameless non-interactive
 * run still gets the dash rather than a flattering guess.
 */
export function rowHeadline(run: AgentRun): string {
  return (
    (run.title ?? "").trim() ||
    run.task ||
    basename(run.workspace_path) ||
    (run.interactive ? INTERACTIVE_HEADLINE : "—")
  );
}

/** The meta line's workspace slot: the repo, else the workspace it mounted,
 *  else the honest note that there is neither. Only a real path gets mono
 *  type — the fallback is a phrase. */
export function repoLabel(run: AgentRun): { text: string; mono: boolean } {
  const text = run.repo || run.workspace_path || "";
  return text ? { text, mono: true } : { text: NO_REPO, mono: false };
}

// shortId (the Run ID column) was removed here — #1197 L3 review F12: the
// Run ID moved to the run page (design.md §4); nothing on this page reads it.
