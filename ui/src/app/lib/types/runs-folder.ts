/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// GET /runs/{id}'s computer_folder: the run's folder held by the person's own runner, and, once the
// run has ended, what became of the changes that were not copied back and of the sandbox copy.

// What became of the changes still owed after the run ended: kept until a time, given up, or nothing owed.
export type ComputerFolderFate = "retained" | "given_up" | "complete";
// Only "deleted" is a confirmed deletion; the field is absent when the server has not confirmed one.
export type ComputerFolderSandboxCopy = "kept" | "deleted";
// The runner's last report of what was not copied back. unsynced is null when the runner never counted;
// paths holds at most 50 names and paths_more the count past them.
export interface ComputerFolderLastReport {
  unsynced: number | null;
  paths?: string[];
  paths_more?: number;
  at: string;
}

// internal/api/run_computer_folder.go computerFolderRunView. The path on that computer is never sent, only its name.
export interface RunComputerFolder {
  name: string;
  runner: string;
  remote_dir: string;
  direction: "both" | "in";
  // The first sync's: waiting, synced or failed (error says why).
  state: "waiting" | "synced" | "failed";
  error?: string;
  seed?: RunFolderPass;
  back?: RunFolderPass;
  // Absent on a server that does not report the fate.
  fate?: ComputerFolderFate;
  sandbox_copy?: ComputerFolderSandboxCopy;
  last_report?: ComputerFolderLastReport;
  give_up_at?: string;
}

// One pass as the runner counted it (types.FolderPassRecord).
export interface RunFolderPass {
  at: string;
  files: number;
  bytes: number;
  refused: { path: string; reason: string }[];
  error?: string;
}
