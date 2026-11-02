/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The drives people made for themselves, on the admin Drives screen (board J4, "Admin view → User drives"):
// GET /drives/member, which is the super admin's alone. A disk kept on a Mac names the Mac. The registry above lists
// only what an admin registered, so a Mac disk never appears there. Paged by the server's own total.
//
// Delete is DELETE /drives/member/{id} behind the same confirm as the registry's. A disk drive whose Mac is offline
// stays "Deleting" until it returns, so the row says so after the refresh rather than claiming it is gone.
import * as React from "react";
import { Loader2 } from "lucide-react";
import {
  drives as api,
  type MemberDriveAdminItem,
} from "../../../lib/api/drives";
import { runners as runnersApi } from "../../../lib/api/runners";
import { HttpError } from "../../../lib/api/core";
import { getErrorMessage } from "../../../lib/format";
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
import { MAC_DRIVES } from "../../wardyn/copy/mac-drives";
import { MEMBER_DRIVES as T } from "../../wardyn/copy/member-drives";
import { NR_FILES } from "../../wardyn/copy/new-run/files";
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
  const [macs, setMacs] = React.useState<Record<string, string>>({});
  const [toDelete, setToDelete] = React.useState<MemberDriveAdminItem | null>(null);
  const [deleteError, setDeleteError] = React.useState<string | null>(null);
  const [busy, setBusy] = React.useState(false);

  // The first page replaces what is shown (after a delete the list is read again from the top); a later page adds to it.
  const load = React.useCallback((offset: number) => {
    setMore(offset > 0);
    return Promise.resolve()
      .then(() => api.listMemberDrives(offset))
      .then((page) => {
        setRows((prev) => (offset > 0 ? [...prev, ...page.drives] : page.drives));
        setTotal(page.total);
        setStatus("ready");
      })
      // A security admin reads this screen but not this list: the tier's answer is a section that is not there, not an error.
      .catch((e) => setStatus(e instanceof HttpError && e.status === 403 ? "hidden" : "error"))
      .finally(() => setMore(false));
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

  const del = async (d: MemberDriveAdminItem) => {
    setBusy(true);
    try {
      await api.deleteMemberDrive(d.id);
      setToDelete(null);
      await load(0);
    } catch (e) {
      // The substrate's own refusal, as the server worded it.
      setDeleteError(getErrorMessage(e));
    } finally {
      setBusy(false);
    }
  };

  if (status === "loading" || status === "hidden" || (status === "ready" && total === 0)) return null;

  return (
    <section data-testid="member-drives" className="mt-6 overflow-clip rounded-xl border border-border bg-card">
      <div className="px-6 pt-5">
        <h2 className="text-sm font-medium text-foreground">{T.TITLE}</h2>
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
                  <TableHead className="w-[22%]">{SCIM.COL_DRIVE}</TableHead>
                  <TableHead className="w-[22%]">{POOLS.COL_OWNER}</TableHead>
                  <TableHead className="w-[14%]">{MAC_DRIVES.D15}</TableHead>
                  <TableHead className="w-[14%]">{MAC_DRIVES.D09}</TableHead>
                  <TableHead className="w-[9%]">{DRIVES.COL_SIZE}</TableHead>
                  <TableHead className="w-[11%]">{POOLS.COL_STATE}</TableHead>
                  <TableHead className="w-[8%]" />
                </TableRow>
              </TableHeader>
              <TableBody>
                {rows.map((d) => (
                  <TableRow key={d.id}>
                    <TableCell className="truncate font-medium" title={d.name}>{d.name}</TableCell>
                    <TableCell className="truncate" title={d.owner}>{d.owner}</TableCell>
                    <TableCell>{storageText(d.backend) ?? <Mono>{d.backend}</Mono>}</TableCell>
                    <TableCell data-testid="member-drive-mac" className="truncate">
                      {d.home_runner_id ? (macs[d.home_runner_id] ?? "") : T.NONE}
                    </TableCell>
                    <TableCell>{driveSizeLabel(d.size_mib) ?? T.NONE}</TableCell>
                    <TableCell>
                      <Chip tone={d.deleting ? "warning" : "success"}>{d.deleting ? T.STATE_DELETING : T.STATE_READY}</Chip>
                    </TableCell>
                    <TableCell>
                      <div className="flex justify-end">
                        <Button
                          variant="ghost"
                          size="sm"
                          aria-label={`${DRIVES.DELETE} ${d.name}`}
                          onClick={() => {
                            setDeleteError(null);
                            setToDelete(d);
                          }}
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

      <AlertDialog open={!!toDelete} onOpenChange={(o) => !o && setToDelete(null)}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>{toDelete ? NR_FILES.DELETE_CONFIRM(toDelete.name) : ""}</AlertDialogTitle>
            <AlertDialogDescription className="sr-only">{toDelete?.owner}</AlertDialogDescription>
          </AlertDialogHeader>
          {deleteError && (
            <Note tone="red" role="alert">
              <span>{deleteError}</span>
            </Note>
          )}
          <AlertDialogFooter>
            <AlertDialogCancel>{PEOPLE.CANCEL}</AlertDialogCancel>
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
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </section>
  );
}
