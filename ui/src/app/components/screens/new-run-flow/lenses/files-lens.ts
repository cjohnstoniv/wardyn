/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Owns workspaces, drive, start_folder and no_repositories_or_drives (and
// resources.disk_mib once its TS twin lands with the main drive carrier).

import type { RunIntent } from "../draft/run-intent";
import { withKeys } from "./lens-keys";
import { issue, type StepIssue, type StepValidator } from "./step-issues";

const attachmentCount = (intent: RunIntent): number => (intent.workspaces?.length ?? 0) + (intent.drive?.enabled ? 1 : 0);

export function attachWorkspace(intent: RunIntent, workspaceId: string): RunIntent {
  if (intent.workspaces?.some((w) => w.workspace_id === workspaceId)) return intent;
  return withKeys(intent, {
    workspaces: [...(intent.workspaces ?? []), { workspace_id: workspaceId }],
    no_repositories_or_drives: undefined,
  });
}

export function detachWorkspace(intent: RunIntent, workspaceId: string): RunIntent {
  const workspaces = (intent.workspaces ?? []).filter((w) => w.workspace_id !== workspaceId);
  const startsThere = intent.start_folder?.kind === "attachment" && intent.start_folder.attachment === workspaceId;
  return withKeys(intent, { workspaces: workspaces.length ? workspaces : undefined, start_folder: startsThere ? undefined : intent.start_folder });
}

/** Choosing where the run starts never attaches anything. */
export const setStartsHere = (intent: RunIntent, attachment: string): RunIntent =>
  withKeys(intent, { start_folder: { kind: "attachment", attachment } });

export const chooseNothing = (intent: RunIntent): RunIntent =>
  withKeys(intent, { no_repositories_or_drives: true, workspaces: undefined, drive: undefined, start_folder: undefined });

export const filesIssues: StepValidator = (intent) => {
  const out: StepIssue[] = [];
  const attached = attachmentCount(intent);
  if (!attached && intent.no_repositories_or_drives !== true) out.push(issue("files", "files_choice_required"));
  if (attached && !intent.start_folder) out.push(issue("files", "starts_here_required"));
  return out;
};
