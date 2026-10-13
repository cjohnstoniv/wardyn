/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Person-created drives, under the user drives of the Add dialog's Choose step
// (the `createUserDrive` seam). It reads the create door from the catalogue:
// open, it offers "+ Create user drive" and a compact list of the person's own
// drives with "Delete drive"; closed, the same control disabled with the reason
// beside it; switched off (the default), nothing at all.

import * as React from "react";
import { createMyDrive, deleteMyDrive, type MeDrive, type MemberDriveCreate, type MemberDriveCreateReason } from "../../../../../lib/api/me-drives";
import { NR_FILES } from "../../../../wardyn/copy/new-run/files";
import { detachDrive } from "../../lenses/files-lens";
import { useNewRunDispatch, useNewRunDraft } from "../../new-run-route";
import { useAnnounce } from "../step-shell";
import { FILES_LINK, type AddDialogProps } from "./add-dialog";
import { CreateDriveDialog } from "./create-drive-dialog";
import { DeleteDriveConfirm } from "./delete-drive-confirm";
import { legacyIdOf } from "./file-items";
import { useDriveCatalogue } from "./file-sources";

type ArmContext = Parameters<NonNullable<AddDialogProps["createUserDrive"]>>[0];

/**
 * The sentence beside a closed door: the canon string for each reason of the server's closed set. `disabled` is the
 * switch being off, so the arm shows nothing for it; governance_unavailable and groups_snapshot_stale have no
 * canon string, so the control is closed with no sentence rather than one worded here.
 */
const REASON_TEXT: Record<MemberDriveCreateReason, ((c: MemberDriveCreate) => string | undefined) | undefined> = {
  disabled: undefined,
  denied_by_profile: (c) => (c.denied_by_profile ? NR_FILES.CREATE_DENIED(c.denied_by_profile) : undefined),
  limit_reached: (c) => NR_FILES.CREATE_LIMIT(c.count, c.limit),
  unsupported: () => NR_FILES.CREATE_UNSUPPORTED,
  delete_unavailable: () => NR_FILES.CREATE_NO_DELETE,
  governance_unavailable: () => undefined,
  groups_snapshot_stale: () => undefined,
};

const CLOSED_LINK = "font-semibold text-muted-foreground opacity-70 disabled:cursor-not-allowed";

export function CreateUserDrive({ choose, reload }: ArmContext) {
  const dispatch = useNewRunDispatch();
  const announce = useAnnounce();
  const { catalogue, reload: reloadOwn } = useDriveCatalogue(useNewRunDraft().intent.runner_pool_id);
  const [creating, setCreating] = React.useState(false);
  const [deleting, setDeleting] = React.useState<{ drive: MeDrive; opener: HTMLElement } | null>(null);
  const link = React.useRef<HTMLButtonElement>(null);
  // The row that held the delete is gone once the drive is, so focus goes to the create control instead.
  const deleted = React.useRef(false);
  const reasonId = React.useId();
  const legacyId = legacyIdOf(catalogue);

  const door = catalogue?.create;
  // Absent: not a person. Disabled: the switch is off, which is the default.
  if (!door || door.reason === "disabled") return null;
  const reasonText = door.allowed ? undefined : door.reason && REASON_TEXT[door.reason]?.(door);
  const own = (catalogue?.drives ?? []).filter((d) => d.owned);
  const refresh = () => {
    reload();
    reloadOwn();
  };

  return (
    <>
      {door.allowed ? (
        <button ref={link} type="button" className={FILES_LINK} onClick={() => setCreating(true)}>{NR_FILES.CREATE_USER_DRIVE}</button>
      ) : (
        <span className="inline-flex flex-wrap items-center gap-x-2 gap-y-1">
          <button type="button" disabled aria-describedby={reasonText ? reasonId : undefined} className={CLOSED_LINK}>{NR_FILES.CREATE_USER_DRIVE}</button>
          {reasonText && <span id={reasonId} className="text-sm text-muted-foreground">{reasonText}</span>}
        </span>
      )}
      {own.length > 0 && (
        <ul className="m-0 flex basis-full list-none flex-col gap-1.5 p-0" data-testid="nr-own-drives">
          {own.map((d) => (
            <li key={d.id} className="flex flex-wrap items-center justify-between gap-2 rounded-md border border-border px-3 py-1.5">
              <strong id={`nr-own-drive-${d.id}`} className="text-sm font-semibold">{d.name}</strong>
              <button
                type="button"
                className={FILES_LINK}
                aria-describedby={`nr-own-drive-${d.id}`}
                onClick={(e) => {
                  deleted.current = false;
                  setDeleting({ drive: d, opener: e.currentTarget });
                }}
              >
                {NR_FILES.DELETE_DRIVE}
              </button>
            </li>
          ))}
        </ul>
      )}
      <CreateDriveDialog
        open={creating}
        onOpenChange={setCreating}
        create={createMyDrive}
        returnFocusTo={link}
        onCreated={(drive) => {
          refresh();
          choose(drive.id);
          announce(NR_FILES.DRIVE_CREATED(drive.name));
        }}
      />
      {deleting && (
        <DeleteDriveConfirm
          name={deleting.drive.name}
          remove={() => deleteMyDrive(deleting.drive.id)}
          returnFocusTo={() => (deleted.current ? link.current : deleting.opener)}
          onClose={() => setDeleting(null)}
          onDeleted={() => {
            deleted.current = true;
            refresh();
            // A drive already attached to this run goes with it, and so does a start on it.
            dispatch({ type: "edit", apply: (i) => detachDrive(i, deleting.drive.id, legacyId) });
            announce(NR_FILES.DRIVE_DELETED(deleting.drive.name));
          }}
        />
      )}
    </>
  );
}
