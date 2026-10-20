/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Owns workspaces, drive, drives, start_folder, no_repositories_or_drives and
// the main drive's size (resources.disk_mib).

import { START_FOLDER_DRIVE, type DriveAttachment, type RequestedResources } from "../../../../lib/types/new-run-contract";
import type { RunIntent } from "../draft/run-intent";
import { withKeys } from "./lens-keys";
import { issue, type StepIssue, type StepValidator } from "./step-issues";

/** Seam: DS3-S2 adds disk_mib to the request twin; this alias goes when it does. */
export type SizedResources = RequestedResources & { disk_mib?: number };
type WorkspaceEntry = NonNullable<RunIntent["workspaces"]>[number];

const attachmentCount = (intent: RunIntent): number =>
  (intent.workspaces?.length ?? 0) + (intent.drive?.enabled ? 1 : 0) + (intent.drives?.length ?? 0);

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

/**
 * Whether this repository holds the starting folder. A workspace start that names a repository is that one;
 * with none named the server opens the first mounted target, so `firstMounted` (this is the first repository
 * the run clones from the workspace, no folder coming before it) decides.
 */
export function startsAt(intent: RunIntent, workspaceId: string, repo: string, firstMounted: boolean): boolean {
  const f = intent.start_folder;
  if (f?.kind !== "attachment" || f.attachment !== workspaceId) return false;
  return f.repo ? f.repo === repo : firstMounted;
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
  // An unnamed start follows the first repository still cloned, so only a start that names this one is cleared.
  const startsThere = startsAt(intent, workspaceId, repo, false);
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

/** Under 0.1 GiB one decimal would read a valid size as zero, so the field shows whole MiB there instead. */
export const underTenthGiB = (mib: number): boolean => mib * 10 < MIB_PER_GIB;

/** One decimal, the way an admin's sub-GiB cap reads; a cap rounds down so "up to" is never overstated. */
export const mibToGibText = (mib: number, rounding: "round" | "floor" = "round"): string =>
  String(Math[rounding]((mib / MIB_PER_GIB) * 10) / 10);

export type SizeUnit = "GiB" | "MiB";

/** The main drive field's text and unit for a stored size: GiB with one decimal from 0.1 GiB up, whole MiB under it. */
export const sizeField = (mib: number, rounding: "round" | "floor" = "round"): { text: string; unit: SizeUnit } =>
  underTenthGiB(mib) ? { text: String(mib), unit: "MiB" } : { text: mibToGibText(mib, rounding), unit: "GiB" };

/** A typed GiB size (0.1 steps) as whole MiB, or undefined when it is not a positive number. */
export function parseGiB(text: string): number | undefined {
  const gib = Number(text.trim());
  return text.trim() !== "" && Number.isFinite(gib) && gib > 0 ? Math.max(1, Math.round(gib * MIB_PER_GIB)) : undefined;
}

/** The field's text in its unit as whole MiB, or undefined when it is not a positive number. */
export function parseSize(text: string, unit: SizeUnit): number | undefined {
  if (unit === "GiB") return parseGiB(text);
  const mib = Number(text.trim());
  return text.trim() !== "" && Number.isFinite(mib) && mib > 0 ? Math.max(1, Math.round(mib)) : undefined;
}

/** What a typed size becomes. A size over the cap is the cap, and says so. */
export function commitSize(text: string, capMib: number | undefined, unit: SizeUnit = "GiB"): { mib?: number; clamped: boolean } {
  const mib = parseSize(text, unit);
  if (mib === undefined) return { clamped: false };
  return capMib !== undefined && mib > capMib ? { mib: capMib, clamped: true } : { mib, clamped: false };
}

export const chooseNothing = (intent: RunIntent): RunIntent =>
  withKeys(intent, { no_repositories_or_drives: true, workspaces: undefined, drive: undefined, drives: undefined, start_folder: undefined });

/** Takes the "only the main drive" choice back: the step is then unsettled until something is added. */
export const clearNothing = (intent: RunIntent): RunIntent => withKeys(intent, { no_repositories_or_drives: undefined });

/**
 * Moves an older `drive: {enabled}` into `drives[]` as the catalogue's legacy drive, so the request never
 * carries both (400 drive_selection_conflict). A start on the legacy drive moves with it. With no legacy
 * drive in the catalogue the flag asks for a drive the server does not have, and it is dropped.
 */
export function liftLegacyDrive(intent: RunIntent, legacyId: string | undefined): RunIntent {
  if (!intent.drive) return intent;
  const entry: DriveAttachment | undefined = intent.drive.enabled && legacyId
    ? { drive_id: legacyId, ...(intent.drive.read_only ? { read_only: true } : {}) } : undefined;
  const drives = [...(intent.drives ?? []).filter((d) => d.drive_id !== legacyId), ...(entry ? [entry] : [])];
  const f = intent.start_folder;
  const startsLegacy = f?.kind === "attachment" && f.attachment === START_FOLDER_DRIVE;
  const start_folder = startsLegacy
    ? entry ? { kind: "attachment" as const, drive_id: entry.drive_id, ...(f.subpath ? { subpath: f.subpath } : {}) } : undefined
    : f;
  return withKeys(intent, { drive: undefined, drives: drives.length ? drives : undefined, start_folder });
}

/** Attaches one catalogue drive by id. Never touches the starting folder. */
export function attachDrive(intent: RunIntent, driveId: string, legacyId: string | undefined, readOnly = false): RunIntent {
  const lifted = liftLegacyDrive(intent, legacyId);
  const rest = (lifted.drives ?? []).filter((d) => d.drive_id !== driveId);
  return withKeys(lifted, { drives: [...rest, readOnly ? { drive_id: driveId, read_only: true } : { drive_id: driveId }], no_repositories_or_drives: undefined });
}

/** Detaches one drive; a start on it is cleared. */
export function detachDrive(intent: RunIntent, driveId: string, legacyId: string | undefined): RunIntent {
  const lifted = liftLegacyDrive(intent, legacyId);
  const drives = (lifted.drives ?? []).filter((d) => d.drive_id !== driveId);
  const startsThere = lifted.start_folder?.kind === "attachment" && lifted.start_folder.drive_id === driveId;
  return withKeys(lifted, { drives: drives.length ? drives : undefined, start_folder: startsThere ? undefined : lifted.start_folder });
}

/** Narrows an attached drive to read-only for this run, or takes the narrowing off. */
export function setDriveReadOnly(intent: RunIntent, driveId: string, readOnly: boolean, legacyId: string | undefined): RunIntent {
  const lifted = liftLegacyDrive(intent, legacyId);
  if (!lifted.drives?.some((d) => d.drive_id === driveId)) return lifted;
  return withKeys(lifted, { drives: lifted.drives.map((d) => (d.drive_id === driveId ? (readOnly ? { drive_id: driveId, read_only: true } : { drive_id: driveId }) : d)) });
}

/** The run starts in one attached drive. Never attaches it. */
export const setStartsAtDrive = (intent: RunIntent, driveId: string, legacyId: string | undefined): RunIntent =>
  withKeys(liftLegacyDrive(intent, legacyId), { start_folder: { kind: "attachment", drive_id: driveId } });

/**
 * Attaches one workspace for its folders only: `repos: []` clones none of its repositories, and `target`
 * (a workspace with exactly one folder) places it. Never touches the starting folder.
 */
export function attachFolder(intent: RunIntent, workspaceId: string, target?: string): RunIntent {
  if (intent.workspaces?.some((w) => w.workspace_id === workspaceId)) return intent;
  return withKeys(intent, {
    workspaces: [...(intent.workspaces ?? []), { workspace_id: workspaceId, repos: [], ...(target ? { target } : {}) }],
    no_repositories_or_drives: undefined,
  });
}

/** Sets or clears where one attached workspace appears (its one repository or folder). */
export function setWorkspaceTarget(intent: RunIntent, workspaceId: string, target: string | undefined): RunIntent {
  const entry = entryOf(intent, workspaceId);
  if (!entry) return intent;
  const next = { ...entry, target };
  if (!target) delete next.target;
  return replaceEntry(intent, next);
}

/** Narrows one attached workspace to read-only for this run, or takes the narrowing off. */
export function setWorkspaceReadOnly(intent: RunIntent, workspaceId: string, readOnly: boolean): RunIntent {
  const entry = entryOf(intent, workspaceId);
  if (!entry) return intent;
  const next = { ...entry, read_only: true };
  if (!readOnly) delete (next as { read_only?: boolean }).read_only;
  return replaceEntry(intent, next);
}

/** Sets or clears the starting folder's subpath, keeping where it starts. */
export function setStartSubpath(intent: RunIntent, subpath: string | undefined): RunIntent {
  const f = intent.start_folder;
  if (f?.kind !== "attachment") return intent;
  const next = { ...f, subpath };
  if (!subpath) delete next.subpath;
  return withKeys(intent, { start_folder: next });
}

export const filesIssues: StepValidator = (intent) => {
  const out: StepIssue[] = [];
  const attached = attachmentCount(intent);
  // A folder from the person's computer (local_folder) is a choice too: it lands on the main drive.
  if (!attached && !intent.local_folder && intent.no_repositories_or_drives !== true) out.push(issue("files", "files_choice_required"));
  if (attached && !intent.start_folder) out.push(issue("files", "starts_here_required"));
  return out;
};
