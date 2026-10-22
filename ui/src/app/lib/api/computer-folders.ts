/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// A folder on the person's computer for a run elsewhere (internal/api/run_computer_folder.go): GET
// /me/runners/{id}/folders lists the folders the person's own folder-only runner shares. Only folder
// names, file counts and sizes cross; a refusal throws core.ts's HttpError with the server's reason.
import { asJson, wfetch } from "./core";

/** One folder in a listing (internal/runnerwire FolderEntry). `more`: counting stopped early, so files and bytes are at least these. */
export interface FolderEntry {
  name: string;
  path: string;
  files: number;
  bytes: number;
  more?: boolean;
}

/** What a run would get from the folder (internal/runnerwire FolderSurvey). */
export interface FolderSurvey {
  files: number;
  bytes: number;
  /** Top-level names never sent: .git, and what the organisation's deny list names. */
  left_out?: string[];
  /** Paths an ignore file (.gitignore, .wardynignore) leaves out. */
  ignored: number;
}

/** GET /me/runners/{id}/folders (internal/api computerFolderView). An empty `path` is the runner's roots. */
export interface FolderList {
  path: string;
  folders: FolderEntry[];
  survey?: FolderSurvey;
  runner: string;
}

/** Reasons the routes answer (internal/api/reasons.go). */
export const COMPUTER_FOLDERS_OFF = "computer_folders_off";
export const COMPUTER_RUNNER_OFFLINE = "computer_runner_offline";
export const COMPUTER_RUNNER_NO_FOLDERS = "computer_runner_no_folders";
export const COMPUTER_FOLDER_REFUSED = "computer_folder_refused";

export const computerFolders = {
  list: (runnerId: string, path = "", survey = false) => {
    const q = new URLSearchParams();
    if (path) q.set("path", path);
    if (survey) q.set("survey", "1");
    const qs = q.toString();
    return wfetch(`/me/runners/${encodeURIComponent(runnerId)}/folders${qs ? `?${qs}` : ""}`, { method: "GET" }).then((r) => asJson<FolderList>(r));
  },
};
