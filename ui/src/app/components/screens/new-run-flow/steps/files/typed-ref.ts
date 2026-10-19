/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Which repository refs the person typed rather than picked from a listed response (#2107). A typed ref is
// not checked against the remote until the run starts. The fact is UI-only, kept in the draft's
// `guided.refText` (never serialised), and counts only while the intent still carries the very same ref.
import type { GuidedFacts, RunIntent } from "../../draft/run-intent";

export const refTextKey = (workspaceId: string, repo: string): string => `${workspaceId}/${repo}`;

/** The text recorded for `key`, or the record without it when the text is empty (a pick, or a cleared field). */
export function withRefText(refText: Record<string, string>, key: string, text: string): Record<string, string> {
  const t = text.trim();
  const next = { ...refText };
  if (t) next[key] = t;
  else delete next[key];
  return next;
}

/** Whether the ref the intent carries for this repository is one the person typed. */
export function isTypedRef(guided: GuidedFacts, intent: RunIntent, workspaceId?: string, repo?: string): boolean {
  if (!workspaceId || !repo) return false;
  const typed = guided.refText[refTextKey(workspaceId, repo)]?.trim();
  if (!typed) return false;
  const ref = intent.workspaces?.find((w) => w.workspace_id === workspaceId)?.repos?.find((r) => r.repo === repo)?.ref;
  return ref === typed;
}
