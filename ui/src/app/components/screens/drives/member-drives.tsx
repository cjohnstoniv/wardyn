/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The drives people made for themselves, on the admin Drives screen (board J4, "Admin view → User drives"):
// GET /drives/member, which is the super admin's alone. A disk kept on a Mac names the Mac. The registry above lists
// only what an admin registered, so a Mac disk never appears there. Paged by the server's own total.
//
// Delete is DELETE /drives/member/{id} behind the same confirm as the registry's. A disk drive whose Mac is offline
// stays "Deleting" until it returns, so the row says so after the refresh rather than claiming it is gone. One whose
// Mac is no longer registered loses only its record (outcome record_only): the confirm stays up with the sentence a
// person gets for their own drive, since the row is gone and its disk is not, even when that was the last drive. A
// disk on a Mac gets a confirm that says both cases before the click. While the delete is in flight the
// confirm cannot be closed, and an answer for a confirm that was closed since never touches a newer one.
//
// A person's own team drive (#3459) shows its chips, its unit and a "Members can write" switch. The switch opens a
// confirm (on Cancel when it widens access, on its button when it narrows), keeps its state until the PUT answers 200,
// and a refusal stays in the confirm with the server's sentence.
import * as React from "react";
import { Loader2 } from "lucide-react";
import {
  drives as api,
  type MemberDriveAdminItem,
} from "../../../lib/api/drives";
import { runners as runnersApi } from "../../../lib/api/runners";
import { HttpError } from "../../../lib/api/core";
import { getErrorMessage } from "../../../lib/format";
import { DRIVE_REFUSAL } from "../../../lib/drive-refusals";
import { DRIVES, PEOPLE, ACCESS_STATE } from "../../../lib/user-drives-copy";
import { SCIM } from "../../../lib/scim-copy";
import { driveSizeLabel } from "../../../lib/user-drives-display";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "../../ui/alert-dialog";
import { Button } from "../../ui/button";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "../../ui/table";
import { Mono } from "../../wardyn/code-block";
import { Switch } from "../../wardyn/form-primitives";
import { MAC_DRIVES } from "../../wardyn/copy/mac-drives";
import { MEMBER_DRIVES as T } from "../../wardyn/copy/member-drives";
import { NR_FILES } from "../../wardyn/copy/new-run/files";
import { NR_SHARED } from "../../wardyn/copy/new-run/shared";
import { POOL_UNIT } from "../../wardyn/copy/pool-unit";
import { POOLS } from "../../wardyn/copy/runner-pools";
import { Chip } from "../../wardyn/primitives";
import { Note } from "./display";

// Docker and cluster volumes keep the names the person sees in New Run; a disk kept on a Mac is MAC-D16.
function storageText(b: MemberDriveAdminItem["backend"]): string | null {
  switch (b) {
    case "mac_disk": return MAC_DRIVES.D16;
    case "docker_volume": return NR_FILES.FAMILY_DOCKER_VOLUME;
    case "k8s_pvc": return NR_FILES.FAMILY_CLUSTER_VOLUME;
    default: return null;
  }
}

export function MemberDrives() {
  const [rows, setRows] = React.useState<MemberDriveAdminItem[]>([]);
  const [total, setTotal] = React.useState(0);
  const [status, setStatus] = React.useState<"loading" | "ready" | "error" | "hidden">("loading");
  const [more, setMore] = React.useState(false);
  // Each first page starts a new list. A later page still out for the list it replaced is dropped, and a later page
  // joins only rows of the length it was asked after: either would show a row twice and then skip one.
  const listGen = React.useRef(0);
  const [macs, setMacs] = React.useState<Record<string, string>>({});
  const [toDelete, setToDelete] = React.useState<MemberDriveAdminItem | null>(null);
  const [deleteError, setDeleteError] = React.useState<string | null>(null);
  const [busy, setBusy] = React.useState(false);
  // The sentence for a delete that removed only the record; the confirm then shows it and a close button.
  const [recordOnly, setRecordOnly] = React.useState<string | null>(null);
  // Counts every open and close of the confirm, so a delete's answer acts only on the confirm it was sent from.
  const openCount = React.useRef(0);
  // The write-bit confirm: the drive, the value asked for and the switch that opened it.
  const [toWrite, setToWrite] = React.useState<{ drive: MemberDriveAdminItem; on: boolean; opener: HTMLElement | null } | null>(null);
  const [writeBusy, setWriteBusy] = React.useState(false);
  const [writeError, setWriteError] = React.useState<string | null>(null);
  const writeCancel = React.useRef<HTMLButtonElement>(null);
  const writeAction = React.useRef<HTMLButtonElement>(null);
  // The opener again, for the close: the dialog closes on the render where toWrite is already null.
  const writeOpener = React.useRef<HTMLElement | null>(null);

  // The first page replaces what is shown (after a delete the list is read again from the top); a later page adds to it.
  const load = React.useCallback((offset: number) => {
    const gen = offset > 0 ? listGen.current : ++listGen.current;
    setMore(offset > 0);
    return Promise.resolve()
      .then(() => api.listMemberDrives(offset))
      .then((page) => {
        if (gen !== listGen.current) return;
        setRows((prev) => (offset === 0 ? page.drives : prev.length === offset ? [...prev, ...page.drives] : prev));
        setTotal(page.total);
        setStatus("ready");
      })
      // A security admin reads this screen but not this list: the tier's answer is a section that is not there, not an error.
      .catch((e) => {
        if (gen === listGen.current) setStatus(e instanceof HttpError && e.status === 403 ? "hidden" : "error");
      })
      .finally(() => {
        if (gen === listGen.current) setMore(false);
      });
  }, []);
  React.useEffect(() => {
    void load(0);
  }, [load]);

  // The Macs the disk drives are kept on, by name: asked for only when the list holds one.
  const showMacs = rows.some((d) => d.home_runner_id);
  React.useEffect(() => {
    if (!showMacs) return;
    runnersApi.list("all").then((rs) => setMacs(Object.fromEntries(rs.map((r) => [r.id, r.name])))).catch(() => setMacs({}));
  }, [showMacs]);

  const openDelete = (d: MemberDriveAdminItem | null) => {
    openCount.current += 1;
    setDeleteError(null);
    setRecordOnly(null);
    setToDelete(d);
  };

  // The switch keeps its state: the row changes only when the PUT answers 200.
  const openWrite = (d: MemberDriveAdminItem, on: boolean) => {
    const label = T.TEAM_WRITE_SWITCH(d.name);
    const opener = Array.from(document.querySelectorAll<HTMLElement>('[role="switch"]')).find((el) => el.getAttribute("aria-label") === label) ?? null;
    setWriteError(null);
    writeOpener.current = opener;
    setToWrite({ drive: d, on, opener });
  };

  const write = async () => {
    if (!toWrite) return;
    setWriteBusy(true);
    setWriteError(null);
    try {
      const item = await api.setMemberTeamDriveWrite(toWrite.drive.id, toWrite.on);
      setRows((prev) => prev.map((r) => (r.id === item.id ? item : r)));
      setToWrite(null);
    } catch (e) {
      setWriteError(getErrorMessage(e));
    } finally {
      setWriteBusy(false);
    }
  };

  const del = async (d: MemberDriveAdminItem) => {
    const open = openCount.current;
    setBusy(true);
    try {
      const res = await api.deleteMemberDrive(d.id);
      if (open === openCount.current) {
        if (res.outcome === "record_only") {
          const mac = d.home_runner_id ? macs[d.home_runner_id] : undefined;
          setRecordOnly(mac ? DRIVE_REFUSAL.MEMBER_RECORD_ONLY(mac) : T.RECORD_ONLY_UNNAMED);
        } else {
          openDelete(null);
        }
      }
    } catch (e) {
      // The substrate's own refusal, as the server worded it.
      if (open === openCount.current) setDeleteError(getErrorMessage(e));
      return;
    } finally {
      setBusy(false);
    }
    await load(0);
  };

  // The record-only sentence outlives the list: deleting the last drive empties it, and the confirm must stay up.
  if (!recordOnly && (status === "loading" || status === "hidden" || (status === "ready" && total === 0))) return null;

  return (
    <section data-testid="member-drives" className="mt-6 overflow-clip rounded-xl border border-border bg-card">
      <div className="px-6 pt-5">
        <h2 className="text-sm font-medium text-foreground">{T.TITLE}</h2>
        {rows.some((d) => d.team) && (
          <div className="mt-3">
            <Note tone="amber">
              <span>{DRIVES.TEAM_ONE_HOST}</span>
            </Note>
          </div>
        )}
      </div>
      {status === "error" ? (
        <div className="flex flex-wrap items-center gap-3 px-6 py-4">
          <p className="text-sm text-muted-foreground">{DRIVES.FETCH_FAILED_TITLE}</p>
          <Button variant="outline" size="sm" onClick={() => void load(0)}>
            {ACCESS_STATE.FETCH_FAILED_RETRY}
          </Button>
        </div>
      ) : (
        <>
          <div className="mt-4">
            <Table className="table-fixed">
              <TableHeader>
                <TableRow className="hover:bg-transparent">
                  <TableHead className="w-[17%]">{SCIM.COL_DRIVE}</TableHead>
                  <TableHead className="w-[15%]">{POOLS.COL_OWNER}</TableHead>
                  <TableHead className="w-[11%]">{MAC_DRIVES.D15}</TableHead>
                  <TableHead className="w-[9%]">{MAC_DRIVES.D09}</TableHead>
                  <TableHead className="w-[7%]">{DRIVES.COL_SIZE}</TableHead>
                  <TableHead className="w-[9%]">{POOLS.COL_STATE}</TableHead>
                  <TableHead className="w-[11%]">{POOL_UNIT.LABEL}</TableHead>
                  <TableHead className="w-[13%]">{DRIVES.FIELD_TEAM_WRITE}</TableHead>
                  <TableHead className="w-[8%]" />
                </TableRow>
              </TableHeader>
              <TableBody>
                {rows.map((d) => (
                  <TableRow key={d.id}>
                    <TableCell className="truncate font-medium" title={d.name}>
                      {d.team ? (
                        <span className="flex flex-col items-start gap-1.5">
                          {d.name}
                          <span className="flex flex-wrap gap-1.5">
                            <Chip tone="neutral">{DRIVES.TEAM_CHIP}</Chip>
                            <Chip tone="warning">{DRIVES.TEAM_ONE_HOST_CHIP}</Chip>
                          </span>
                        </span>
                      ) : (
                        d.name
                      )}
                    </TableCell>
                    <TableCell className="truncate" title={d.owner}>{d.owner}</TableCell>
                    <TableCell>{storageText(d.backend) ?? <Mono>{d.backend}</Mono>}</TableCell>
                    <TableCell data-testid="member-drive-mac" className="truncate">
                      {d.home_runner_id ? (macs[d.home_runner_id] ?? "") : T.NONE}
                    </TableCell>
                    <TableCell>{driveSizeLabel(d.size_mib) ?? T.NONE}</TableCell>
                    <TableCell>
                      <Chip tone={d.deleting ? "warning" : "success"}>{d.deleting ? T.STATE_DELETING : T.STATE_READY}</Chip>
                    </TableCell>
                    <TableCell data-testid="member-drive-unit" className="truncate">
                      {d.team_unit_name ? d.team_unit_name : d.team_unit_id ? <Mono>{d.team_unit_id}</Mono> : T.NONE}
                    </TableCell>
                    <TableCell data-testid="member-drive-write">
                      {d.team && d.team_unit_id ? (
                        <span className="inline-flex items-center gap-2">
                          <Switch checked={!!d.members_can_write} onChange={(on) => openWrite(d, on)} label={T.TEAM_WRITE_SWITCH(d.name)} />
                          <span aria-hidden="true" className="text-sm text-muted-foreground">
                            {d.members_can_write ? T.TEAM_WRITE_ON : T.TEAM_WRITE_OFF}
                          </span>
                        </span>
                      ) : (
                        T.NONE
                      )}
                    </TableCell>
                    <TableCell>
                      <div className="flex justify-end">
                        <Button
                          variant="ghost"
                          size="sm"
                          aria-label={`${DRIVES.DELETE} ${d.name}`}
                          onClick={() => openDelete(d)}
                        >
                          {DRIVES.DELETE}
                        </Button>
                      </div>
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </div>
          {rows.length < total && (
            <div className="flex flex-wrap items-center gap-3 px-6 py-4">
              <p className="text-sm text-muted-foreground">{T.SHOWING(rows.length, total)}</p>
              <Button variant="outline" size="sm" disabled={more} onClick={() => void load(rows.length)}>
                {more ? <Loader2 className="size-4 animate-spin" /> : null}
                {T.SHOW_MORE}
              </Button>
            </div>
          )}
        </>
      )}

      <AlertDialog open={!!toDelete} onOpenChange={(o) => !o && !busy && openDelete(null)}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle className={recordOnly ? "sr-only" : undefined}>
              {recordOnly
                ? NR_FILES.DELETE_DRIVE
                : toDelete
                  ? toDelete.home_runner_id
                    ? T.DELETE_CONFIRM_MAC(toDelete.name)
                    : toDelete.team && toDelete.team_unit_id
                      ? NR_FILES.DELETE_CONFIRM_TEAM(toDelete.name)
                      : NR_FILES.DELETE_CONFIRM(toDelete.name)
                  : ""}
            </AlertDialogTitle>
            <AlertDialogDescription className="sr-only">{toDelete?.owner}</AlertDialogDescription>
          </AlertDialogHeader>
          {recordOnly && (
            <Note tone="amber" role="status">
              <span>{recordOnly}</span>
            </Note>
          )}
          {deleteError && (
            <Note tone="red" role="alert">
              <span>{deleteError}</span>
            </Note>
          )}
          <AlertDialogFooter>
            {recordOnly ? (
              <AlertDialogCancel>{NR_SHARED.CLOSE_DIALOG}</AlertDialogCancel>
            ) : (
              <>
                <AlertDialogCancel disabled={busy}>{PEOPLE.CANCEL}</AlertDialogCancel>
                <AlertDialogAction
                  className="bg-danger text-danger-foreground hover:bg-danger/90"
                  disabled={busy}
                  onClick={(e) => {
                    e.preventDefault();
                    if (toDelete) void del(toDelete);
                  }}
                >
                  {busy ? <Loader2 className="size-4 animate-spin" /> : null}
                  {DRIVES.DELETE}
                </AlertDialogAction>
              </>
            )}
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>

      <AlertDialog open={!!toWrite} onOpenChange={(o) => !o && !writeBusy && setToWrite(null)}>
        <AlertDialogContent
          onOpenAutoFocus={(e) => {
            // Allowing writing widens access, so it opens on Cancel; making it read-only opens on its button.
            e.preventDefault();
            (toWrite?.on ? writeCancel : writeAction).current?.focus();
          }}
          onCloseAutoFocus={(e) => {
            const el = writeOpener.current;
            if (el?.isConnected) {
              e.preventDefault();
              el.focus();
            }
          }}
        >
          <AlertDialogHeader>
            <AlertDialogTitle>
              {toWrite ? (toWrite.on ? T.TEAM_WRITE_ALLOW_TITLE(toWrite.drive.name) : T.TEAM_WRITE_STOP_TITLE(toWrite.drive.name)) : ""}
            </AlertDialogTitle>
            <AlertDialogDescription>
              {toWrite
                ? toWrite.on
                  ? T.TEAM_WRITE_ALLOW_BODY(toWrite.drive.team_unit_name || "its unit")
                  : T.TEAM_WRITE_STOP_BODY(toWrite.drive.owner)
                : ""}
            </AlertDialogDescription>
          </AlertDialogHeader>
          {writeError && (
            <Note tone="red" role="alert">
              <span>{writeError}</span>
            </Note>
          )}
          <AlertDialogFooter>
            <AlertDialogCancel ref={writeCancel} disabled={writeBusy}>{PEOPLE.CANCEL}</AlertDialogCancel>
            <AlertDialogAction
              ref={writeAction}
              disabled={writeBusy}
              onClick={(e) => {
                e.preventDefault();
                void write();
              }}
            >
              {writeBusy ? <Loader2 className="size-4 animate-spin" /> : null}
              {toWrite?.on ? T.TEAM_WRITE_ALLOW : T.TEAM_WRITE_STOP}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </section>
  );
}
