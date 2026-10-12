/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// What the drives section writes to the draft: the person's one drive is the
// request's `drive`. Choosing where the run starts never attaches anything, and
// unmounting the drive that holds the start clears it.

import { START_FOLDER_DRIVE } from "../../../../../lib/types/new-run-contract";
import type { RunIntent } from "../../draft/run-intent";
import { withKeys } from "../../lenses/lens-keys";

/** Whether the person's drive is mounted. */
export const legacyMounted = (intent: RunIntent): boolean => intent.drive?.enabled === true;

const legacyStarts = (intent: RunIntent): boolean =>
  intent.start_folder?.kind === "attachment" && intent.start_folder.attachment === START_FOLDER_DRIVE;

/** Mounts or unmounts the drive. Unmounted sends no `drive` at all. */
export function mountLegacy(intent: RunIntent, on: boolean): RunIntent {
  if (on) return withKeys(intent, { drive: { enabled: true }, no_repositories_or_drives: undefined });
  return withKeys(intent, { drive: undefined, start_folder: legacyStarts(intent) ? undefined : intent.start_folder });
}

/** Narrows the mounted drive to read-only for this run; a no-op while it is not mounted. */
export const setLegacyReadOnly = (intent: RunIntent, readOnly: boolean): RunIntent =>
  legacyMounted(intent) ? withKeys(intent, { drive: readOnly ? { enabled: true, read_only: true } : { enabled: true } }) : intent;

export const startsAtLegacy = (intent: RunIntent): boolean => legacyMounted(intent) && legacyStarts(intent);

export const setStartsAtLegacy = (intent: RunIntent): RunIntent =>
  withKeys(intent, { start_folder: { kind: "attachment", attachment: START_FOLDER_DRIVE } });

/** Whether the person has chosen nothing for the step yet, which is the only time a default may be chosen for them. */
export const nothingChosen = (intent: RunIntent): boolean =>
  !intent.workspaces?.length && !intent.drive && !intent.start_folder && intent.no_repositories_or_drives === undefined;
