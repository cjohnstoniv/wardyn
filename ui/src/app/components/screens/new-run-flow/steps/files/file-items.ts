/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The one list the Files step shows: every repository, drive and folder the
// draft attaches, read back from the draft and the server's sources, plus the
// writes the Add / Edit dialog and the cards make. The draft stays the truth;
// a card is only a view of it.

import { DENIED } from "../../../../../lib/permissions-copy";
import type { MeDrive, MeDriveCatalogue } from "../../../../../lib/api/me-drives";
import type { RunAttachment } from "../../../../../lib/types/new-run-contract";
import { START_FOLDER_DRIVE } from "../../../../../lib/types/new-run-contract";
import type { Workspace } from "../../../../../lib/types/workspaces";
import { NR_FILES } from "../../../../wardyn/copy/new-run/files";
import type { RunIntent } from "../../draft/run-intent";
import {
  attachDrive, attachFolder, detachDrive, detachWorkspace, excludeRepo, includeRepo, repoIncluded, setDriveReadOnly, setRepoRef,
  setStartsAtDrive, setStartsHere, setWorkspaceReadOnly, setWorkspaceTarget, startsAt,
} from "../../lenses/files-lens";
import { withKeys } from "../../lenses/lens-keys";
import type { RepoRow } from "./files-seams";

export type FileKind = "repo" | "userDrive" | "sharedDrive" | "folder" | "computer";
export const KIND_ORDER: readonly FileKind[] = ["repo", "userDrive", "sharedDrive", "folder", "computer"];

/** One workspace's host folders (its `local_dir` sources), attached together with none of its repositories. */
export interface FolderSource {
  key: string;
  workspaceId: string;
  name: string;
  hostPath?: string;
  /** Where it appears when the person does not move it. */
  defaultPath: string;
  /** Exactly one folder: the request may place it (`workspaces[].target`). */
  single: boolean;
  writable: boolean;
  unavailable?: string;
}

const baseName = (path: string): string => path.replace(/[/\\]+$/, "").split(/[/\\]/).pop() || path;
const slug = (text: string): string => text.trim().toLowerCase().replace(/[^a-z0-9]+/g, "-").replace(/^-|-$/g, "");

/** Owner ruling 2026-10-13: a folder appears at /home/agent/folders/<name> unless its source names a place. */
export const folderPath = (name: string): string => `/home/agent/folders/${slug(name) || "folder"}`;

export function folderSources(workspaces: readonly Workspace[]): FolderSource[] {
  return workspaces.flatMap((w) => {
    const dirs = (w.sources ?? []).filter((s) => s.type === "local_dir");
    if (!dirs.length) return [];
    const first = dirs[0];
    const name = dirs.length === 1 && first.path ? baseName(first.path) : w.name;
    return [{
      key: `folder:${w.id}`, workspaceId: w.id, name, hostPath: first.path,
      defaultPath: first.target || folderPath(name), single: dirs.length === 1,
      writable: dirs.every((s) => s.writable !== false),
      unavailable: w.available_to_you === false ? DENIED.WORKSPACE_NOT_AVAILABLE : undefined,
    }];
  });
}

const BACKEND: Record<MeDrive["backend"], string> = {
  docker_volume: NR_FILES.FAMILY_DOCKER_VOLUME,
  host_path: NR_FILES.FAMILY_HOST_FOLDER,
  k8s_pvc: NR_FILES.FAMILY_CLUSTER_VOLUME,
  k8s_pvc_static: NR_FILES.FAMILY_CLUSTER_VOLUME,
};
const SOURCE: Record<MeDrive["source"], string> = { personal: NR_FILES.SOURCE_PERSONAL, team: NR_FILES.SOURCE_TEAM, organisation: NR_FILES.SOURCE_ORG };

/** The chips a drive is stored on: where it comes from, then its storage. */
export const driveChips = (d: MeDrive): string[] => [SOURCE[d.source], BACKEND[d.backend]];
export const driveKind = (d: MeDrive): FileKind => (d.section === "shared" ? "sharedDrive" : "userDrive");
export const legacyIdOf = (catalogue: MeDriveCatalogue | null): string | undefined => catalogue?.drives.find((d) => d.legacy)?.id;

/** One card. Exactly one of repo (workspaceId + repo), folder (workspaceId) and drive (driveId) refs is set. */
export interface FileItem {
  key: string;
  kind: FileKind;
  name: string;
  path?: string;
  /** A repository's branch as the draft or its source names it. */
  branch?: string;
  readOnly: boolean;
  /** Read-only because the drive or folder itself is, not by this run's choice. */
  locked: boolean;
  /** The request may place it: a repository its workspace clones alone, or a workspace's one folder. */
  pathEditable: boolean;
  storedOn: string[];
  hostPath?: string;
  starts: boolean;
  workspaceId?: string;
  repo?: string;
  driveId?: string;
  /** Read for a repository's other repositories of the same workspace. */
  siblings?: readonly string[];
}

export interface ItemSources {
  rows: readonly RepoRow[];
  folders: readonly FolderSource[];
  catalogue: MeDriveCatalogue | null;
  attachments: readonly RunAttachment[];
}

const LEGACY_KEY = "drive:legacy";

export function fileItems(intent: RunIntent, src: ItemSources): FileItem[] {
  const f = intent.start_folder?.kind === "attachment" ? intent.start_folder : undefined;
  const out: FileItem[] = [];
  for (const entry of intent.workspaces ?? []) {
    const ws = entry.workspace_id;
    const rows = src.rows.filter((r) => r.workspaceId === ws && repoIncluded(intent, ws, r.repo));
    const folder = src.folders.find((s) => s.workspaceId === ws);
    for (const r of rows) {
      const ref = entry.repos?.find((x) => x.repo === r.repo)?.ref;
      const alone = rows.length === 1 && !folder;
      const served = src.attachments.find((a) => a.kind === "repository" && a.workspace_id === ws && a.repo === r.repo)?.target;
      out.push({
        key: r.key, kind: "repo", name: r.name, path: (alone && entry.target) || served || r.path, branch: ref ?? r.ref,
        readOnly: false, locked: false, pathEditable: alone, storedOn: [], starts: startsAt(intent, ws, r.repo, r.siblings.length),
        workspaceId: ws, repo: r.repo, siblings: r.siblings,
      });
    }
    if (folder) {
      const served = src.attachments.find((a) => a.kind === "folder" && a.workspace_id === ws)?.target;
      const alone = folder.single && rows.length === 0;
      out.push({
        key: folder.key, kind: "folder", name: folder.name, path: (alone && entry.target) || served || folder.defaultPath,
        readOnly: !folder.writable || entry.read_only === true, locked: !folder.writable, pathEditable: alone,
        storedOn: [NR_FILES.FAMILY_HOST_FOLDER], hostPath: folder.hostPath,
        starts: f?.attachment === ws && !f.repo && !f.drive_id && rows.length === 0, workspaceId: ws,
      });
    }
  }
  const drives = src.catalogue?.drives ?? [];
  const legacy = drives.find((d) => d.legacy);
  if (intent.drive?.enabled) {
    out.push({
      key: legacy ? `drive:${legacy.id}` : LEGACY_KEY, kind: "userDrive", name: legacy?.name ?? NR_FILES.KIND_USER_DRIVE,
      path: legacy?.target ?? src.attachments.find((a) => a.kind === "drive" && !a.drive_id)?.target,
      readOnly: intent.drive.read_only === true || legacy?.writable === false, locked: legacy?.writable === false, pathEditable: false,
      storedOn: legacy ? driveChips(legacy) : [], starts: f?.attachment === START_FOLDER_DRIVE, driveId: legacy?.id,
    });
  }
  for (const a of intent.drives ?? []) {
    const d = drives.find((x) => x.id === a.drive_id);
    const served = src.attachments.find((x) => x.kind === "drive" && x.drive_id === a.drive_id);
    out.push({
      key: `drive:${a.drive_id}`, kind: d ? driveKind(d) : "userDrive", name: d?.name ?? served?.name ?? a.drive_id,
      path: d?.target ?? served?.target, readOnly: a.read_only === true || d?.writable === false, locked: d?.writable === false,
      pathEditable: false, storedOn: d ? driveChips(d) : [], starts: f?.drive_id === a.drive_id, driveId: a.drive_id,
    });
  }
  return out;
}

/** What the dialog saves for one item. */
export interface ItemChoice {
  kind: FileKind;
  /** RepoRow.key, FolderSource.key or the drive id. */
  sourceKey: string;
  path: string;
  /** The path the source shows by default; a path equal to it is not sent. */
  defaultPath?: string;
  branch?: string;
  readOnly: boolean;
}

type Apply = (intent: RunIntent) => RunIntent;

/** Starts the run in an item. Never attaches anything. */
export function startIn(item: FileItem, legacyId: string | undefined): Apply {
  return (i) => {
    if (item.driveId) return setStartsAtDrive(i, item.driveId, legacyId);
    if (item.key === LEGACY_KEY) return withKeys(i, { start_folder: { kind: "attachment", attachment: START_FOLDER_DRIVE } });
    return setStartsHere(i, item.workspaceId!, item.repo);
  };
}

/** The key the item a choice saves will have. */
export const choiceKey = (c: Pick<ItemChoice, "kind" | "sourceKey">): string =>
  c.kind === "userDrive" || c.kind === "sharedDrive" ? `drive:${c.sourceKey}` : c.sourceKey;

/**
 * Adds or edits one item. Editing to another source takes the old one out first. A path is sent only
 * where the request may place the item, and for a repository only when it differs from the server's own.
 */
export function saveChoice(choice: ItemChoice, src: ItemSources, editing?: FileItem): Apply {
  const legacyId = legacyIdOf(src.catalogue);
  return (intent) => {
    const moved = editing && editing.key !== choiceKey(choice);
    let i = moved ? removeItem(editing, src)(intent) : intent;
    if (choice.kind === "repo") {
      const row = src.rows.find((r) => r.key === choice.sourceKey);
      if (!row) return intent;
      i = setRepoRef(includeRepo(i, row.workspaceId, row.repo), row.workspaceId, row.repo, choice.branch, row.siblings);
      const alone = fileItems(i, src).find((x) => x.key === row.key)?.pathEditable;
      return setWorkspaceTarget(i, row.workspaceId, alone && choice.path !== choice.defaultPath ? choice.path : undefined);
    }
    if (choice.kind === "folder") {
      const folder = src.folders.find((f) => f.key === choice.sourceKey);
      if (!folder) return intent;
      i = attachFolder(i, folder.workspaceId);
      const alone = fileItems(i, src).find((x) => x.key === folder.key)?.pathEditable;
      i = setWorkspaceTarget(i, folder.workspaceId, alone ? choice.path : undefined);
      return setWorkspaceReadOnly(i, folder.workspaceId, choice.readOnly && folder.writable);
    }
    const d = src.catalogue?.drives.find((x) => x.id === choice.sourceKey);
    if (!d) return intent;
    const narrow = choice.readOnly && d.writable;
    return editing && !moved ? setDriveReadOnly(i, d.id, narrow, legacyId) : attachDrive(i, d.id, legacyId, narrow);
  };
}

/** Removes one item. The last repository of a workspace that also brings folders keeps the folders. */
export function removeItem(item: FileItem, src: ItemSources): Apply {
  const legacyId = legacyIdOf(src.catalogue);
  return (intent) => {
    if (item.driveId) return detachDrive(intent, item.driveId, legacyId);
    if (item.key === LEGACY_KEY) {
      const startsThere = intent.start_folder?.attachment === START_FOLDER_DRIVE;
      return withKeys(intent, { drive: undefined, start_folder: startsThere ? undefined : intent.start_folder });
    }
    const ws = item.workspaceId!;
    if (item.kind === "folder") return detachWorkspace(intent, ws);
    const hasFolder = src.folders.some((f) => f.workspaceId === ws);
    const left = fileItems(intent, src).filter((x) => x.kind === "repo" && x.workspaceId === ws && x.key !== item.key);
    if (hasFolder && !left.length) {
      const startsThere = intent.start_folder?.attachment === ws && intent.start_folder.repo === item.repo;
      const entry = intent.workspaces!.find((w) => w.workspace_id === ws)!;
      return withKeys(intent, {
        workspaces: intent.workspaces!.map((w) => (w === entry ? { workspace_id: ws, repos: [] } : w)),
        start_folder: startsThere ? undefined : intent.start_folder,
      });
    }
    return excludeRepo(intent, ws, item.repo!, item.siblings ?? [item.repo!]);
  };
}

/** The paths the request may hold: under /home/agent (the server refuses anything else), with no "..". */
export const PATH_SHAPE = /^\/home\/agent\/[^/]/;

/** Why a path cannot be saved, against the other items' paths; "" when it can. */
export function pathError(path: string, items: readonly FileItem[], selfKey?: string): string {
  const clean = path.trim().replace(/\/+$/, "");
  if (!PATH_SHAPE.test(clean) || clean.split("/").some((p) => p === "..")) return NR_FILES.PATH_INVALID;
  const clash = items.find((x) => x.key !== selfKey && x.path && (x.path === clean || x.path.startsWith(`${clean}/`) || clean.startsWith(`${x.path}/`)));
  return clash ? NR_FILES.PATH_TAKEN(clash.name, clash.path!) : "";
}
