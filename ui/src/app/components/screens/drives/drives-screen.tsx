/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The /drives screen (0.7) — the admin's drive registry: what storage a run can
// mount at /home/agent/drive, and who gets it. SUPER-only, with NO nav item:
// it is reached from the Workspaces header's outline button, the setup
// Workspaces step's card and the Settings card, all three of which render for
// an operator only (docs/design/user-drives-prompt.md §5 #7, §6). Nothing
// LINKS a security admin here — but a URL is a URL, and GET /drives is
// operatorOnly, so their READ is a 403. That is a tier refusal, not a transport
// failure, and the screen answers it as one (see `status`).
//
// EVERY user-visible string here comes from user-drives-copy.ts (§7, frozen)
// plus the subject/priority/preview vocabulary already frozen in
// permissions-copy.ts, people-access-copy.ts and governance-copy.ts (§7.1,
// referenced and never re-frozen). This file adds no copy of its own.
//
// Three structural rules, not styling:
//
//  1. ONE `default` button on the screen at any moment (CONSOLE-RULES §6).
//     Allocate at rest; Save drive while the editor is open (the allocation
//     form collapses and takes its teal with it); New drive when there are no
//     drives at all and the empty state carries the action that fills it.
//  2. DELETE IS TWO DIFFERENT REFUSALS. The list already knows grant_count, so
//     at count > 0 the dialog opens PRE-FILLED with the restriction and its
//     confirm disabled — there is nothing to attempt. The server's 409 stays
//     authoritative for the race that count cannot see, and that path is
//     COUNT-FREE: the client believed the count was zero, and the shipped 409
//     carries no n and must not grow one.
//  3. A SIZE IS NEVER WARNED ABOUT, ONLY STATED. Wardyn enforces no drive's
//     size itself; HONESTY says so once under the table and every rendered size
//     carries its ENFORCEMENT_* gloss, so the honesty is on each number.
import * as React from "react";
import { AlertTriangle, HardDrive, Loader2 } from "lucide-react";
import { HttpError } from "../../../lib/api/core";
import {
  drives as api,
  enforcementFor,
  isManagedBackend,
  type UserDriveListItem,
  type UserDrivesSnapshot,
} from "../../../lib/api/drives";
import { getErrorMessage } from "../../../lib/format";
import { ACCESS_STATE, DRIVES, PEOPLE } from "../../../lib/user-drives-copy";
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
import { useOperator } from "../../wardyn/operator-context";
import { PageHeader } from "../../wardyn/page-header";
import { Chip, OperatorOnlyHint } from "../../wardyn/primitives";
import { EmptyState, TableSkeleton } from "../../wardyn/states";
import { AllocationsBlock } from "./allocations";
import { DriveEditor } from "./drive-editor";
import { Note, enforcementGloss, modeText, modeTone, noteClass, question, sizeText, withMono } from "./display";

const EMPTY: UserDrivesSnapshot = { drives: [], grants: [], host_roots_configured: false, runner_target: "" };

export function DrivesScreen() {
  // The SUPER tier, not the security one: a drive names a host path or a
  // cluster storage class, and "never the host" is what separates the two admin
  // tiers. Mirrors the operatorOnly route group exactly. UX only — the
  // middleware is what refuses a write.
  const operator = useOperator();
  const [snap, setSnap] = React.useState<UserDrivesSnapshot>(EMPTY);
  const [status, setStatus] = React.useState<"loading" | "error" | "forbidden" | "ready">("loading");
  // null = closed; {drive: null} = a new drive; {drive: d} = editing d.
  const [editing, setEditing] = React.useState<{ drive: UserDriveListItem | null } | null>(null);
  const [toDelete, setToDelete] = React.useState<UserDriveListItem | null>(null);
  const [deleteError, setDeleteError] = React.useState<{ title?: string; message: string } | null>(null);
  const [busy, setBusy] = React.useState(false);

  // ponytail: no setStatus("loading") here. `load` is also the post-write
  // refresh, and flipping back to loading would unmount the add form under
  // whoever just typed in it — the skeleton belongs to the FIRST read.
  const load = React.useCallback(() => {
    api
      .getDrives()
      .then((s) => {
        setSnap(s);
        setStatus("ready");
      })
      // A 403 is the TIER, not the network: GET /drives is operatorOnly, so a
      // security admin who typed this URL is refused the read itself. Rendering
      // FETCH_FAILED_* there would call an authorization answer a server
      // hiccup, and offer a Retry that returns the same 403 forever.
      .catch((e) => setStatus(e instanceof HttpError && e.status === 403 ? "forbidden" : "error"));
  }, []);
  React.useEffect(load, [load]);

  const openEditor = (drive: UserDriveListItem | null) => setEditing({ drive });

  const openDelete = (d: UserDriveListItem) => {
    setDeleteError(null);
    setToDelete(d);
  };

  const del = async (d: UserDriveListItem) => {
    setBusy(true);
    try {
      await api.deleteDrive(d.id);
      setToDelete(null);
      load();
    } catch (e) {
      // The 409 is the ON DELETE RESTRICT refusal and it is the authority on
      // the race: another admin allocated this drive since the list was read.
      const restricted = e instanceof HttpError && e.status === 409 ? e : null;
      setDeleteError(
        restricted
          ? { title: DRIVES.DELETE_RESTRICT_TITLE, message: restricted.message }
          : { message: getErrorMessage(e) },
      );
      // It is the authority on the COUNT too, and that is what this re-read is
      // for. `grant_count` said nobody — which is the whole reason this dialog
      // opened unrestricted — and the server has just said otherwise, so the
      // row the table shows is now known to be wrong and is re-read. The OPEN
      // dialog is deliberately left as it is: §7.4 draws the race post-attempt,
      // COUNT-FREE, with the console's heading over the server's text and the
      // confirm STILL ENABLED (the allocation may be gone by the retry). What
      // the re-read ends is the loop AFTER it — without it the row keeps saying
      // ALLOCATED_NONE and every later Delete on it re-opens the same doomed
      // confirm, where now the next one opens PRE-FILLED from the true count
      // with its confirm disabled and nothing to attempt.
      if (restricted) load();
    } finally {
      setBusy(false);
    }
  };

  const editorOpen = editing !== null;
  const noDrives = status === "ready" && snap.drives.length === 0;
  // The screen's ONE `default` button (CONSOLE-RULES §6, prompt §4). This file
  // decides exactly one of the three arms — the empty state's New drive — so
  // exactly one bit is derived here. The other two are held where they render
  // and cannot co-occur with this one: the editor's Save exists only while the
  // editor is open, and the allocation form's Allocate only once there are
  // drives to allocate, which is the negation of `noDrives`.
  const newIsTeal = noDrives && !editorOpen;

  const deleteCount = toDelete?.grant_count ?? 0;
  const [deleteHead, deleteBody] = question(toDelete ? DRIVES.DELETE_CONFIRM(toDelete.name) : "");

  return (
    <div className="mx-auto max-w-[1120px] px-6 py-6">
      <PageHeader title={DRIVES.TITLE} description={withMono(DRIVES.LEAD)} />

      {status === "forbidden" ? (
        /* The tier, said as a tier: OPERATOR_ONLY_REASON, the same sentence
           every other operator-only control carries, and no Retry — retrying a
           403 returns a 403. Mock state 9's note and §5 #7 hide the card and
           both entry points from this caller; this is what is left when they
           arrive at the URL anyway. */
        <div className="mt-6">
          <OperatorOnlyHint />
        </div>
      ) : status === "error" ? (
        <section className="mt-6 overflow-hidden rounded-xl border border-border bg-card">
          {/* Distinct from empty, and it says so: allocations that already exist
              keep binding every run — this list just cannot show them. */}
          <EmptyState
            icon={AlertTriangle}
            title={DRIVES.FETCH_FAILED_TITLE}
            description={DRIVES.FETCH_FAILED_BODY}
            action={
              <Button variant="outline" size="sm" onClick={load}>
                {ACCESS_STATE.FETCH_FAILED_RETRY}
              </Button>
            }
          />
        </section>
      ) : (
        <>
          <section className="mt-6 overflow-hidden rounded-xl border border-border bg-card">
            <div className="flex flex-wrap items-start justify-between gap-3 px-6 pt-5">
              <div>
                <h2 className="text-sm font-medium text-foreground">{DRIVES.DRIVES_TITLE}</h2>
                <p className="mt-1 max-w-[82ch] text-body text-muted-foreground">{DRIVES.DRIVES_LEAD}</p>
              </div>
              {/* With no drives the empty state carries this action instead, so
                  the screen never offers two New drive buttons. */}
              {!noDrives && (
                <Button variant="outline" disabled={!operator || editorOpen} onClick={() => openEditor(null)}>
                  {DRIVES.NEW_CTA}
                </Button>
              )}
            </div>

            <div className="mt-4">
              {status === "loading" ? (
                <TableSkeleton rows={3} cols={7} />
              ) : noDrives ? (
                <EmptyState
                  icon={HardDrive}
                  title={DRIVES.EMPTY_TITLE}
                  description={DRIVES.EMPTY_BODY}
                  action={
                    <Button
                      variant={newIsTeal ? "default" : "outline"}
                      disabled={!operator || editorOpen}
                      onClick={() => openEditor(null)}
                    >
                      {DRIVES.NEW_CTA}
                    </Button>
                  }
                />
              ) : (
                <>
                  <Table>
                    <TableHeader>
                      <TableRow className="hover:bg-transparent">
                        <TableHead>{DRIVES.COL_NAME}</TableHead>
                        <TableHead>{DRIVES.COL_BACKEND}</TableHead>
                        <TableHead>{DRIVES.COL_SIZE}</TableHead>
                        <TableHead>{DRIVES.COL_MODE}</TableHead>
                        <TableHead>{DRIVES.COL_RECLAIM}</TableHead>
                        <TableHead>{DRIVES.COL_ALLOCATED}</TableHead>
                        <TableHead className="w-[140px]" />
                      </TableRow>
                    </TableHeader>
                    <TableBody>
                      {snap.drives.map((d) => (
                        <TableRow key={d.id}>
                          {/* A drive name is a human-chosen label, never mono. */}
                          <TableCell className="font-medium">{d.name}</TableCell>
                          <TableCell>
                            {/* The kind chip over the backend's wire value: the
                                WHO-over-WHAT two-glyph rule, applied to an
                                object instead of an actor. */}
                            <span className="flex flex-wrap items-center gap-2">
                              <Chip tone="neutral">
                                {isManagedBackend(d.backend) ? DRIVES.KIND_MANAGED : DRIVES.KIND_SHARE}
                              </Chip>
                              <Mono>{d.backend}</Mono>
                            </span>
                          </TableCell>
                          <TableCell>
                            {sizeText(d.size_mib)}
                            {/* The gloss on every number, not only in the note. */}
                            <span className="mt-0.5 block text-meta text-muted-foreground">
                              {enforcementGloss(enforcementFor(d.backend))}
                            </span>
                          </TableCell>
                          <TableCell>
                            <Chip tone={modeTone(d.writable)}>{modeText(d.writable)}</Chip>
                          </TableCell>
                          <TableCell>
                            {d.reclaim === "delete" ? DRIVES.RECLAIM_DELETE : DRIVES.RECLAIM_RETAIN}
                          </TableCell>
                          <TableCell>
                            {d.grant_count === 0 ? DRIVES.ALLOCATED_NONE : DRIVES.ALLOCATED_COUNT(d.grant_count)}
                          </TableCell>
                          <TableCell>
                            <div className="flex justify-end gap-1">
                              <Button
                                variant="outline"
                                size="sm"
                                disabled={!operator || editorOpen}
                                onClick={() => openEditor(d)}
                                aria-label={`${DRIVES.EDIT} ${d.name}`}
                              >
                                {DRIVES.EDIT}
                              </Button>
                              <Button
                                variant="ghost"
                                size="sm"
                                disabled={!operator || editorOpen}
                                onClick={() => openDelete(d)}
                                aria-label={`${DRIVES.DELETE} ${d.name}`}
                              >
                                {DRIVES.DELETE}
                              </Button>
                            </div>
                          </TableCell>
                        </TableRow>
                      ))}
                    </TableBody>
                  </Table>
                  {/* Once, under the table (Q1) — never a per-number warning and
                      never a second wording of "we do not enforce this". */}
                  <div className="px-6 pb-5">
                    <Note>{withMono(DRIVES.HONESTY)}</Note>
                  </div>
                </>
              )}
            </div>

            {/* Opens IN PLACE, under the list it edits — not a dialog. The
                allocation form below collapses for exactly this span. */}
            {editing && (
              <DriveEditor
                key={editing.drive?.id ?? ""}
                drive={editing.drive}
                runnerTarget={snap.runner_target}
                hostRootsConfigured={snap.host_roots_configured}
                onCancel={() => setEditing(null)}
                onSaved={() => {
                  setEditing(null);
                  load();
                }}
              />
            )}
          </section>

          {/* Only once the read has landed AND there is something to allocate:
              "No allocations yet" over an unloaded snapshot is a confident
              empty state that is a false claim, and over a registry with no
              drives it is an answer to a question nobody can ask yet (mock
              state 1 is the header and the empty state, nothing else). The
              drives card above holds the skeleton for both. */}
          {status === "ready" && snap.drives.length > 0 && (
            <AllocationsBlock
              drives={snap.drives}
              grants={snap.grants}
              collapsed={editorOpen}
              onChanged={load}
            />
          )}
        </>
      )}

      <AlertDialog open={!!toDelete} onOpenChange={(o) => !o && setToDelete(null)}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>{deleteHead}</AlertDialogTitle>
            {/* ONE Description per dialog — it is what Radix points
                aria-describedby at, so the note styling travels to it rather
                than wrapping it in a div a <p> may not contain. */}
            {deleteCount > 0 ? (
              // PRE-FILLED from the count the list already shows: there is
              // nothing to attempt, so the confirm below is disabled rather
              // than left to fail. This is the ONE path that names a count.
              <AlertDialogDescription className={noteClass("red")}>
                <b className="font-semibold">{DRIVES.DELETE_RESTRICT_TITLE}</b>
                <span>{DRIVES.DELETE_RESTRICT_BODY(toDelete?.name ?? "", deleteCount)}</span>
              </AlertDialogDescription>
            ) : (
              <AlertDialogDescription>{deleteBody}</AlertDialogDescription>
            )}
          </AlertDialogHeader>
          {/* The race, post-attempt: count-free, the server's own message. */}
          {deleteError && (
            <Note tone="red" role="alert">
              {deleteError.title && <b className="font-semibold">{deleteError.title}</b>}
              {/* The server's own sentence, PLAIN: it is prose that quotes a
                  wire fact, not a literal, and monoing the whole of it would
                  claim otherwise. */}
              <span>{deleteError.message}</span>
            </Note>
          )}
          <AlertDialogFooter>
            <AlertDialogCancel>{PEOPLE.CANCEL}</AlertDialogCancel>
            <AlertDialogAction
              className="bg-danger text-danger-foreground hover:bg-danger/90"
              disabled={deleteCount > 0 || busy}
              onClick={(e) => {
                e.preventDefault();
                if (toDelete) del(toDelete);
              }}
            >
              {busy ? <Loader2 className="size-4 animate-spin" /> : null}
              {DRIVES.DELETE}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}
