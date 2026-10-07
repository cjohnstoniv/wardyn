/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { workspaces as workspacesApi } from "./api/workspaces";
import type { Workspace } from "./types";

// useWorkspaceList — the onboarded-workspace list every LAUNCH surface needs (the
// New Run dialog, the manual wizard, the Getting-started Workspaces step), plus
// the pending_scan kick those surfaces share.
//
// A failed fetch degrades to an EMPTY list rather than an error state: each of
// these surfaces offers "Add workspace" inline, so "we couldn't ask" and "you
// have none" lead to the same next action. The Workspaces SCREEN deliberately
// does NOT use this — it tracks loading/error/ready separately so a failed fetch
// never renders as a confident "no workspaces".
export function useWorkspaceList(scope = "") {
  const currentScope = React.useRef(scope);
  currentScope.current = scope;
  const sequence = React.useRef(0);
  const [loadedScope, setLoadedScope] = React.useState(scope);
  React.useEffect(() => () => { sequence.current++; }, []);
  const [workspaces, setWorkspaces] = React.useState<Workspace[]>([]);
  // Starts true: nothing has been fetched yet, so the empty initial list must not
  // be rendered as a confirmed "none".
  const [loading, setLoading] = React.useState(true);
  // Additive (member Getting Started's "inline note on error"): every existing
  // caller degrades to an empty list on a failed fetch and never reads this, so
  // it changes nothing for them — a caller that wants to tell "fetch failed"
  // apart from "genuinely none" (rather than treat both alike, per the doc
  // comment above) can now do so without a second fetch of its own.
  const [error, setError] = React.useState(false);

  // `clear` empties the list first — a dialog re-opening must not show the
  // previous session's workspaces while the new fetch is in flight.
  const reload = React.useCallback((clear = false) => {
    if (clear) setWorkspaces([]);
    setLoading(true);
    setError(false);
    const started = ++sequence.current;
    const owner = currentScope.current;
    const current = () => started === sequence.current && owner === currentScope.current;
    return workspacesApi
      .listWorkspaces()
      .then((rows) => { if (current()) setWorkspaces(rows); })
      .catch(() => {
        if (!current()) return;
        setWorkspaces([]);
        setError(true);
      })
      .finally(() => { if (current()) { setLoadedScope(owner); setLoading(false); } });
  }, []);

  // Refresh, kick a best-effort (re-)scan, then refresh again once it settles: a
  // local dir reaches "ready" inline, a repo launches its governed scan run — so
  // the inline path isn't left stuck in pending_scan. A failed scan is swallowed;
  // the workspace is onboarded either way and the operator can re-scan from the
  // Workspaces screen. Resolves after the scan settles, so a caller can chain
  // (e.g. re-run preflight).
  const scanAndReload = React.useCallback(
    (id: string) => {
      reload();
      return workspacesApi
        .scanWorkspace(id)
        .catch(() => {})
        .finally(reload);
    },
    [reload],
  );

  return { workspaces: loadedScope === scope ? workspaces : [], loading: loadedScope !== scope || loading, error, reload, scanAndReload };
}
