/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { workspaces as workspacesApi } from "../../../lib/api/workspaces";
import { HttpError } from "../../../lib/api/core";
import type { Workspace } from "../../../lib/types";

/**
 * A host-list save the server refused as stale (412: a decision or edit landed after this page
 * read the workspace) changed nothing. Reload the workspace so the list shows what the server
 * holds; the refusal's own message is already on screen. A failed reload keeps the page as it was.
 */
export async function reloadIfStale(e: unknown, id: string, onWorkspaceUpdated: (w: Workspace) => void): Promise<void> {
  if (!(e instanceof HttpError) || e.status !== 412) return;
  try {
    const fresh = await workspacesApi.getWorkspace(id);
    if (fresh) onWorkspaceUpdated(fresh);
  } catch {
    // The refusal is already shown; the next load picks the list up.
  }
}
