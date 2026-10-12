/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Owns workspaces, drive, start_folder, no_repositories_or_drives and the main
// drive's size (resources.disk_mib).

import type { RequestedResources } from "../../../../lib/types/new-run-contract";
import type { RunIntent } from "../draft/run-intent";
import { withKeys } from "./lens-keys";
import { issue, type StepIssue, type StepValidator } from "./step-issues";

/** Seam: DS3-S2 adds disk_mib to the request twin; this alias goes when it does. */
export type SizedResources = RequestedResources & { disk_mib?: number };
type WorkspaceEntry = NonNullable<RunIntent["workspaces"]>[number];

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

/** Choosing where the run starts never attaches anything. `repo` names the clone inside a workspace. */
export const setStartsHere = (intent: RunIntent, attachment: string, repo?: string): RunIntent =>
  withKeys(intent, { start_folder: { kind: "attachment", attachment, ...(repo ? { repo } : {}) } });

/** Whether this repository holds the starting folder; a workspace folder with no repo named is its only repository. */
export function startsAt(intent: RunIntent, workspaceId: string, repo: string, repoCount: number): boolean {
  const f = intent.start_folder;
  if (f?.kind !== "attachment" || f.attachment !== workspaceId) return false;
  return f.repo ? f.repo === repo : repoCount === 1;
}

const replaceEntry = (intent: RunIntent, entry: WorkspaceEntry): RunIntent =>
  withKeys(intent, { workspaces: (intent.workspaces ?? []).map((w) => (w.workspace_id === entry.workspace_id ? entry : w)) });

const entryOf = (intent: RunIntent, workspaceId: string): WorkspaceEntry | undefined =>
  intent.workspaces?.find((w) => w.workspace_id === workspaceId);

/** The repositories an entry clones; an entry with no list clones every one of the workspace's. */
const picksOf = (entry: WorkspaceEntry, all: readonly string[]) => entry.repos ?? all.map((repo) => ({ repo }));

/** Whether a repository is cloned: its workspace is attached and its repos list is absent (every one) or names it. */
export function repoIncluded(intent: RunIntent, workspaceId: string, repo: string): boolean {
  const entry = entryOf(intent, workspaceId);
  return !!entry && (!entry.repos || entry.repos.some((r) => r.repo === repo));
}

/** Includes one repository. An entry with no repos list already clones them all. Never touches the starting folder. */
export function includeRepo(intent: RunIntent, workspaceId: string, repo: string): RunIntent {
  const entry = entryOf(intent, workspaceId);
  if (!entry) {
    return withKeys(intent, {
      workspaces: [...(intent.workspaces ?? []), { workspace_id: workspaceId, repos: [{ repo }] }],
      no_repositories_or_drives: undefined,
    });
  }
  if (!entry.repos || entry.repos.some((r) => r.repo === repo)) return intent;
  return replaceEntry(intent, { ...entry, repos: [...entry.repos, { repo }] });
}

/** Leaves one repository out. The last one detaches the workspace, since an empty repos list means "all". */
export function excludeRepo(intent: RunIntent, workspaceId: string, repo: string, all: readonly string[]): RunIntent {
  const entry = entryOf(intent, workspaceId);
  if (!entry) return intent;
  const rest = picksOf(entry, all).filter((r) => r.repo !== repo);
  if (!rest.length) return detachWorkspace(intent, workspaceId);
  const startsThere = startsAt(intent, workspaceId, repo, all.length);
  return withKeys(replaceEntry(intent, { ...entry, repos: rest }), { start_folder: startsThere ? undefined : intent.start_folder });
}

/** Sets or clears one included repository's ref. */
export function setRepoRef(intent: RunIntent, workspaceId: string, repo: string, ref: string | undefined, all: readonly string[]): RunIntent {
  const entry = entryOf(intent, workspaceId);
  if (!entry) return intent;
  return replaceEntry(intent, { ...entry, repos: picksOf(entry, all).map((r) => (r.repo === repo ? (ref ? { repo, ref } : { repo }) : r)) });
}

export const mainDriveMiB = (intent: RunIntent): number | undefined => (intent.resources as SizedResources | undefined)?.disk_mib;

/** Sets the main drive's size in whole MiB, or clears it. Other resources are kept. */
export function setMainDrive(intent: RunIntent, mib: number | undefined): RunIntent {
  const next: SizedResources = { ...intent.resources };
  if (mib === undefined) delete next.disk_mib;
  else next.disk_mib = mib;
  return withKeys(intent, { resources: Object.keys(next).length ? next : undefined });
}

const MIB_PER_GIB = 1024;

/** One decimal, the way an admin's sub-GiB cap reads; a cap rounds down so "up to" is never overstated. */
export const mibToGibText = (mib: number, rounding: "round" | "floor" = "round"): string =>
  String(Math[rounding]((mib / MIB_PER_GIB) * 10) / 10);

/** A typed GiB size (0.1 steps) as whole MiB, or undefined when it is not a positive number. */
export function parseGiB(text: string): number | undefined {
  const gib = Number(text.trim());
  return text.trim() !== "" && Number.isFinite(gib) && gib > 0 ? Math.max(1, Math.round(gib * MIB_PER_GIB)) : undefined;
}

/** What a typed size becomes. A size over the cap is the cap, and says so. */
export function commitSize(text: string, capMib: number | undefined): { mib?: number; clamped: boolean } {
  const mib = parseGiB(text);
  if (mib === undefined) return { clamped: false };
  return capMib !== undefined && mib > capMib ? { mib: capMib, clamped: true } : { mib, clamped: false };
}

export const chooseNothing = (intent: RunIntent): RunIntent =>
  withKeys(intent, { no_repositories_or_drives: true, workspaces: undefined, drive: undefined, start_folder: undefined });

export const filesIssues: StepValidator = (intent) => {
  const out: StepIssue[] = [];
  const attached = attachmentCount(intent);
  if (!attached && intent.no_repositories_or_drives !== true) out.push(issue("files", "files_choice_required"));
  if (attached && !intent.start_folder) out.push(issue("files", "starts_here_required"));
  return out;
};
