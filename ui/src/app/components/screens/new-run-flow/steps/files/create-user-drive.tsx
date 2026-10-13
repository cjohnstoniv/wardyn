/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Person-created drives, under the user drives of the Add dialog's Choose step
// (the `createUserDrive` seam). It reads the create door from the catalogue:
// open, it offers "+ Create user drive"; closed, the same control disabled with
// the reason beside it; switched off (the default), no create control at all.
// The person's own drives list with "Delete drive" whatever the door says: the
// server never gates a delete on it, and a person can always remove their own data.

import * as React from "react";
import { createMyDrive, deleteMyDrive, type MeDrive, type MemberDriveCreate, type MemberDriveCreateReason } from "../../../../../lib/api/me-drives";
import { MEMBER } from "../../../../../lib/governance-copy";
import { DRIVE_MEMBER } from "../../../../../lib/user-drives-copy";
import { NR_FILES } from "../../../../wardyn/copy/new-run/files";
import { NR_SHARED } from "../../../../wardyn/copy/new-run/shared";
import { detachDrive } from "../../lenses/files-lens";
import { useNewRunDispatch, useNewRunDraft } from "../../new-run-route";
import { useAnnounce } from "../step-shell";
import { FILES_LINK, type AddDialogProps } from "./add-dialog";
import { CreateDriveCard } from "./create-drive-card";
import { DeleteDriveConfirm } from "./delete-drive-confirm";
import { legacyIdOf } from "./file-items";
import { useDriveCatalogue } from "./file-sources";

type ArmContext = Parameters<NonNullable<AddDialogProps["createUserDrive"]>>[0];

/**
 * The sentence beside a closed door: the canon string for each reason of the server's closed set. `disabled` is the
 * switch being off, so the arm shows no create control for it. The two reasons with no CREATE_* string show the
 * step's existing sentence for the same cause.
 */
const REASON_TEXT: Record<MemberDriveCreateReason, ((c: MemberDriveCreate) => string | undefined) | undefined> = {
  disabled: undefined,
  denied_by_profile: (c) => (c.denied_by_profile ? NR_FILES.CREATE_DENIED(c.denied_by_profile) : undefined),
  limit_reached: (c) => NR_FILES.CREATE_LIMIT(c.count, c.limit),
  unsupported: () => NR_FILES.CREATE_UNSUPPORTED,
  delete_unavailable: () => NR_FILES.CREATE_NO_DELETE,
  governance_unavailable: () => DRIVE_MEMBER.NR_GOVERNANCE_UNAVAILABLE,
  groups_snapshot_stale: () => MEMBER.DENIED_STALE_GROUPS,
};

const CLOSED_LINK = "font-semibold text-muted-foreground opacity-70 disabled:cursor-not-allowed";

export function CreateUserDrive({ choose, reload }: ArmContext) {
  const dispatch = useNewRunDispatch();
  const announce = useAnnounce();
  const { catalogue, reload: reloadOwn } = useDriveCatalogue(useNewRunDraft().intent.runner_pool_id);
  const [creating, setCreating] = React.useState(false);
  const [deleting, setDeleting] = React.useState<{ drive: MeDrive; opener: HTMLElement } | null>(null);
  const link = React.useRef<HTMLButtonElement>(null);
  // Focus goes back to the create control when the card closes, and when a delete leaves no row to return to.
  const refocus = React.useRef(false);
  // A delete just finished: where focus goes once the drive has left the lists.
  const deleted = React.useRef(false);
  const [afterDelete, setAfterDelete] = React.useState<{ id: string; name: string; dialog: HTMLElement | null } | null>(null);
  const reasonId = React.useId();
  const legacyId = legacyIdOf(catalogue);
  React.useEffect(() => {
    if (!creating && refocus.current) {
      refocus.current = false;
      link.current?.focus();
    }
  }, [creating]);
  React.useEffect(() => {
    if (!afterDelete || deleting || catalogue?.drives.some((d) => d.id === afterDelete.id)) return;
    setAfterDelete(null);
    if (link.current) return link.current.focus();
    // No create link on screen: the next own drive's Delete, else the first option, else Back; never the dialog itself.
    const root = afterDelete.dialog;
    const radio = Array.from(root?.querySelectorAll<HTMLInputElement>('input[type="radio"]:not(:disabled)') ?? [])
      .find((r) => !r.closest("label")?.textContent?.startsWith(afterDelete.name));
    const back = Array.from(root?.querySelectorAll<HTMLButtonElement>("button") ?? []).find((b) => b.textContent === NR_SHARED.BACK);
    (root?.querySelector<HTMLElement>("[data-own-delete]") ?? radio ?? back)?.focus();
  }, [afterDelete, deleting, catalogue]);
  const closeCard = React.useCallback(() => {
    refocus.current = true;
    setCreating(false);
  }, []);
  // The prototype holds Continue while the form is open, and the seam can only clear a pick, so the pick is
  // read back off the list (the one checked option, found by its unique name) and put back on Cancel.
  const held = React.useRef<string | null>(null);
  const dialogRoot = React.useRef<HTMLElement | null>(null);
  const openCard = () => {
    const root = (dialogRoot.current = link.current?.closest<HTMLElement>('[role="dialog"]') ?? null);
    const name = root?.querySelector<HTMLInputElement>('input[type="radio"]:checked')?.closest("label")?.querySelector("strong")?.textContent;
    const same = (catalogue?.drives ?? []).filter((d) => d.section === "user" && d.name === name);
    // A pick that cannot be named once is left alone, and Continue stays live.
    if (name && same.length !== 1) return setCreating(true);
    held.current = same[0]?.id ?? null;
    choose("");
    setCreating(true);
  };
  const cancelCard = React.useCallback(() => {
    const root = dialogRoot.current;
    if (held.current && !root?.querySelector('input[type="radio"]:checked')) choose(held.current);
    held.current = null;
    closeCard();
  }, [choose, closeCard]);

  const door = catalogue?.create;
  // No door: not a person. Disabled: the switch is off, which is the default.
  const offersCreate = !!door && door.reason !== "disabled";
  const own = (catalogue?.drives ?? []).filter((d) => d.owned);
  if (!offersCreate && own.length === 0) return null;
  const reasonText = door && !door.allowed && door.reason ? REASON_TEXT[door.reason]?.(door) : undefined;
  const refresh = () => {
    reload();
    reloadOwn();
  };

  return (
    <>
      {offersCreate && !creating && (door.allowed ? (
        <button ref={link} type="button" className={FILES_LINK} onClick={openCard}>{NR_FILES.CREATE_USER_DRIVE}</button>
      ) : (
        <span className="inline-flex flex-wrap items-center gap-x-2 gap-y-1">
          <button type="button" disabled aria-describedby={reasonText ? reasonId : undefined} className={CLOSED_LINK}>{NR_FILES.CREATE_USER_DRIVE}</button>
          {reasonText && <span id={reasonId} className="text-sm text-muted-foreground">{reasonText}</span>}
        </span>
      ))}
      {creating && (
        <CreateDriveCard
          create={createMyDrive}
          onClose={cancelCard}
          onCreated={(drive) => {
            held.current = null;
            refresh();
            choose(drive.id);
            announce(NR_FILES.DRIVE_CREATED(drive.name));
            closeCard();
          }}
        />
      )}
      {own.length > 0 && (
        <ul className="m-0 flex basis-full list-none flex-col gap-1 p-0" data-testid="nr-own-drives">
          {own.map((d) => (
            <li key={d.id} className="flex items-center justify-between gap-2 text-[13px] text-muted-foreground">
              <span id={`nr-own-drive-${d.id}`}>{d.name}</span>
              <button
                type="button"
                className="font-semibold text-primary hover:underline"
                aria-describedby={`nr-own-drive-${d.id}`}
                data-own-delete={d.id}
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
      {deleting && (
        <DeleteDriveConfirm
          name={deleting.drive.name}
          remove={() => deleteMyDrive(deleting.drive.id)}
          returnFocusTo={() => (deleted.current ? null : deleting.opener)}
          onClose={() => setDeleting(null)}
          onDeleted={() => {
            deleted.current = true;
            setAfterDelete({ id: deleting.drive.id, name: deleting.drive.name, dialog: deleting.opener.closest<HTMLElement>('[role="dialog"]') });
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
