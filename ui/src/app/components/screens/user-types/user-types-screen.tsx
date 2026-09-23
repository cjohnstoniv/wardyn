/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The User types screen (0.8, UT-7a) — the security admin's acting surface
// for org-defined kinds of person (user-types-design.md rev 4 §2.6/§2.7,
// approved decision packets mock-08/user-types-packet-{a,b}.html).
//
// Same idiom as the Governance screen (screens/governance/governance-screen
// .tsx): list + in-place editor, one `default` button on the screen at any
// moment (CONSOLE-RULES §6), delete refused with the server's own message
// when the type is still named by a role mapping, a grant, an assignment, a
// drive grant or a run (the five-source guard, UT-1 §7).
import * as React from "react";
import { AlertTriangle, Loader2, UsersRound } from "lucide-react";
import { HttpError } from "../../../lib/api/core";
import { userTypes as api } from "../../../lib/api/user-types";
import { governance as governanceApi, type GovernanceSnapshot } from "../../../lib/api/governance";
import { getErrorMessage, relativeTime } from "../../../lib/format";
import { USER_TYPES as UT } from "../../../lib/user-types-copy";
import type { UserType } from "../../../lib/types";
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
import { useSecurityOperator } from "../../wardyn/operator-context";
import { PageHeader } from "../../wardyn/page-header";
import { Chip } from "../../wardyn/primitives";
import { EmptyState, TableSkeleton, loadFailStatus, type ScreenStatus } from "../../wardyn/states";
import { SECURITY_ONLY_REASON } from "../../wardyn/copy";
import { Note, question } from "../governance/display";
import { UserTypeEditor } from "./user-type-editor";

export function UserTypesScreen() {
  const securityOperator = useSecurityOperator();
  const [types, setTypes] = React.useState<UserType[]>([]);
  const [status, setStatus] = React.useState<ScreenStatus>("loading");
  const [governance, setGovernance] = React.useState<GovernanceSnapshot | null>(null);
  // null = closed; {type: null} = a new type; {type: t} = editing t.
  const [editing, setEditing] = React.useState<{ type: UserType | null } | null>(null);
  const [toDelete, setToDelete] = React.useState<UserType | null>(null);
  const [deleteError, setDeleteError] = React.useState<string | null>(null);
  const [busy, setBusy] = React.useState(false);

  // ponytail: no setStatus("loading") here (governance-screen.tsx's own
  // note) — `load` doubles as the post-write refresh, and flipping back to
  // loading would unmount the editor under whoever just typed in it.
  const load = React.useCallback(() => {
    api
      .listUserTypes()
      .then((list) => {
        setTypes(list);
        setStatus("ready");
      })
      .catch((e) => setStatus(loadFailStatus(e)));
    // Best-effort: a failed governance read degrades the editor's Ceiling
    // section to "unassigned" rather than blocking the whole screen — the
    // list itself never depends on it.
    governanceApi.getGovernance().then(setGovernance).catch(() => setGovernance(null));
  }, []);
  React.useEffect(load, [load]);

  const openEditor = (type: UserType | null) => setEditing({ type });
  const openDelete = (t: UserType) => {
    setDeleteError(null);
    setToDelete(t);
  };

  const del = async (t: UserType) => {
    setBusy(true);
    try {
      await api.deleteUserType(t.id);
      setToDelete(null);
      load();
    } catch (e) {
      setDeleteError(e instanceof HttpError ? e.message : getErrorMessage(e));
    } finally {
      setBusy(false);
    }
  };

  const editorOpen = editing !== null;
  const noTypes = status === "ready" && types.length === 0;
  const teal: "save" | "new" = editorOpen ? "save" : "new";

  const [deleteHead, deleteBody] = question(toDelete ? UT.DELETE_CONFIRM(toDelete.name) : "");

  return (
    <div className="mx-auto max-w-[1120px] px-6 py-6">
      <PageHeader title={UT.TITLE} description={UT.LEAD} />

      {status === "forbidden" ? (
        <p className="mt-6 text-sm text-muted-foreground">{SECURITY_ONLY_REASON}</p>
      ) : status === "error" ? (
        <section className="mt-6 overflow-hidden rounded-xl border border-border bg-card">
          <EmptyState
            icon={AlertTriangle}
            title={UT.FETCH_FAILED_TITLE}
            description={UT.FETCH_FAILED_BODY}
            action={
              <Button variant="outline" size="sm" onClick={load}>
                Retry
              </Button>
            }
          />
        </section>
      ) : (
        <section className="mt-6 overflow-hidden rounded-xl border border-border bg-card">
          <div className="flex flex-wrap items-start justify-between gap-3 px-6 pt-5">
            <div />
            {!noTypes && (
              <Button
                variant={teal === "new" ? "default" : "outline"}
                disabled={!securityOperator || editorOpen}
                onClick={() => openEditor(null)}
              >
                {UT.NEW_CTA}
              </Button>
            )}
          </div>

          <div className="mt-4">
            {status === "loading" ? (
              <TableSkeleton rows={3} cols={5} />
            ) : noTypes ? (
              <EmptyState
                icon={UsersRound}
                title={UT.EMPTY_TITLE}
                description={UT.EMPTY_BODY}
                action={
                  <Button
                    variant="default"
                    disabled={!securityOperator || editorOpen}
                    onClick={() => openEditor(null)}
                  >
                    {UT.NEW_CTA}
                  </Button>
                }
              />
            ) : (
              <Table>
                <TableHeader>
                  <TableRow className="hover:bg-transparent">
                    <TableHead>{UT.COL_NAME}</TableHead>
                    <TableHead>{UT.COL_DESCRIPTION}</TableHead>
                    <TableHead>{UT.COL_PRIORITY}</TableHead>
                    <TableHead>{UT.COL_UPDATED}</TableHead>
                    <TableHead className="w-[140px]" />
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {types.map((t) => (
                    <TableRow key={t.id}>
                      <TableCell className="font-medium">
                        <span className="flex items-center gap-2">
                          {t.name}
                          {t.built_in && <Chip tone="neutral">{UT.BUILT_IN_BADGE}</Chip>}
                        </span>
                      </TableCell>
                      <TableCell className="max-w-[36ch] truncate text-muted-foreground" title={t.description}>
                        {t.description}
                      </TableCell>
                      <TableCell>{t.priority}</TableCell>
                      <TableCell className="whitespace-nowrap text-muted-foreground" title={t.updated_at}>
                        {relativeTime(t.updated_at)}
                      </TableCell>
                      <TableCell>
                        <div className="flex justify-end gap-1">
                          <Button
                            variant="outline"
                            size="sm"
                            disabled={!securityOperator || editorOpen}
                            onClick={() => openEditor(t)}
                            aria-label={`${UT.EDIT} ${t.name}`}
                          >
                            {UT.EDIT}
                          </Button>
                          <Button
                            variant="ghost"
                            size="sm"
                            disabled={!securityOperator || editorOpen || t.built_in}
                            onClick={() => openDelete(t)}
                            aria-label={`${UT.DELETE} ${t.name}`}
                            title={t.built_in ? UT.DELETE_BUILTIN : undefined}
                          >
                            {UT.DELETE}
                          </Button>
                        </div>
                      </TableCell>
                    </TableRow>
                  ))}
                </TableBody>
              </Table>
            )}
          </div>

          {/* Opens IN PLACE, under the list it edits — not a dialog. */}
          {editing && (
            <UserTypeEditor
              key={editing.type?.id ?? ""}
              type={editing.type}
              disabled={!securityOperator}
              governance={governance}
              onCancel={() => setEditing(null)}
              onSaved={() => {
                setEditing(null);
                load();
              }}
            />
          )}
        </section>
      )}

      <AlertDialog open={!!toDelete} onOpenChange={(o) => !o && setToDelete(null)}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>{deleteHead}</AlertDialogTitle>
            <AlertDialogDescription>{deleteBody}</AlertDialogDescription>
          </AlertDialogHeader>
          {/* Count-free (governance-screen.tsx's own precedent): the five-
              source guard's count is knowable only server-side here — there is
              no cheap client-side count across role mappings, grants,
              assignments, drive grants and runs the way Governance's single
              assignments table gives it for free. */}
          {deleteError && (
            <Note tone="red" role="alert">
              {deleteError}
            </Note>
          )}
          <AlertDialogFooter>
            <AlertDialogCancel>Cancel</AlertDialogCancel>
            <AlertDialogAction
              className="bg-danger text-danger-foreground hover:bg-danger/90"
              disabled={busy}
              onClick={(e) => {
                e.preventDefault();
                if (toDelete) del(toDelete);
              }}
            >
              {busy ? <Loader2 className="size-4 animate-spin" /> : null}
              {UT.DELETE}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}
