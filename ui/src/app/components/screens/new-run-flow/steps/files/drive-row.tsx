/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The person's one drive from /me.user_drive: a mount checkbox (off by default),
// what it is, a read-only toggle shown only while a writable drive is mounted,
// and the shared Starts here radio.

import * as React from "react";
import { Checkbox } from "../../../../ui/checkbox";
import { makeMono } from "../../../../wardyn/code-block";
import type { MeUserDrive } from "../../../../../lib/api/health";
import { DRIVE_MEMBER } from "../../../../../lib/user-drives-copy";
import { driveModeWord, driveSizeLabel } from "../../../../../lib/user-drives-display";
import { StartsHere } from "./starts-here";

const withDriveTarget = makeMono(["/home/agent/drive"]);

/** The Starts here radio of a row; the id goes to the row a held Continue focuses. */
export interface StartsProps {
  checked: boolean;
  onSelect: () => void;
  id?: string;
}

export function LegacyDriveRow({ drive, mounted, readOnly, onMount, onReadOnly, starts }: {
  drive: MeUserDrive;
  mounted: boolean;
  readOnly: boolean;
  onMount: (on: boolean) => void;
  onReadOnly: (on: boolean) => void;
  starts: StartsProps;
}) {
  const nameId = React.useId();
  // The mode of the mount this run gets: a writable drive narrowed for this run reads as read-only.
  const writable = drive.writable && !(mounted && readOnly);
  const size = driveSizeLabel(drive.size_mib);
  const mode = driveModeWord(writable);
  const hint = size ? DRIVE_MEMBER.NR_HINT(drive.name, size, mode) : DRIVE_MEMBER.NR_HINT_NOSIZE(drive.name, mode);
  return (
    <div data-testid="nr-drive" className="flex flex-col gap-2 py-3">
      <div className="flex flex-wrap items-start gap-x-4 gap-y-2">
        <Checkbox id="nr-drive-mount" className="mt-0.5" checked={mounted} onCheckedChange={(v) => onMount(v === true)} />
        <div className="min-w-0 flex-1">
          <label id={nameId} htmlFor="nr-drive-mount" className="text-sm font-medium">{DRIVE_MEMBER.NR_CHECKBOX}</label>
          <p className="text-sm text-muted-foreground">
            {withDriveTarget(hint)} {writable ? DRIVE_MEMBER.NR_RW_NOTE : DRIVE_MEMBER.NR_RO_NOTE}
          </p>
        </div>
        <StartsHere nameId={nameId} id={starts.id} included={mounted} checked={mounted && starts.checked} onSelect={starts.onSelect} />
      </div>
      {mounted && drive.writable && (
        <label htmlFor="nr-drive-readonly" className="ml-6 flex items-center gap-2 text-sm">
          <Checkbox id="nr-drive-readonly" checked={readOnly} onCheckedChange={(v) => onReadOnly(v === true)} />
          {DRIVE_MEMBER.NR_READONLY_TOGGLE}
        </label>
      )}
    </div>
  );
}
