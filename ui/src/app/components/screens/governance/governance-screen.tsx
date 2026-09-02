/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Governance screen (0.7) — the security admin's acting surface for named,
// assignable ceilings. Its sidebar entry sits between Policies and Permissions
// so the three read as one narrowing sequence: the ceiling for everyone, the
// ceilings assigned over it, then the grants layered inside one (§O Q1).
//
// EVERY user-visible string here comes from a copy module — governance-copy.ts
// (docs/design/governance-prompt.md §7, frozen), plus the subject/preview
// vocabulary already frozen in permissions-copy.ts and people-access-copy.ts
// (§7.1, referenced and never re-frozen). This file adds no copy of its own, so
// the shipped wording cannot drift from the reviewed mock
// (docs/design/governance-mock/index.html).
//
// Three structural rules, not styling:
//
//  1. ONE `default` button on the screen at any moment (CONSOLE-RULES §6).
//     Assign at rest; Save profile while the editor is open (the add form
//     collapses and takes its teal with it); New profile when there are no
//     profiles at all and the empty state carries the action that fills it.
//  2. DELETE IS TWO DIFFERENT REFUSALS. The list already knows the assignment
//     count, so at count > 0 the dialog opens PRE-FILLED with the restriction
//     and its confirm disabled — no attempt to make. The server's 409 stays
//     authoritative for the race that count cannot see, and that path is
//     COUNT-FREE: the client believed the count was zero, and the shipped 409
//     carries no n.
//  3. THE SERVER COMPOSES ITS OWN PROSE. The omission warnings and the
//     grant-bound refusal render verbatim, in response order, under the
//     console's frozen heading — never re-worded, never joined into a sentence.
import * as React from "react";
import { AlertTriangle, Loader2, ShieldCheck } from "lucide-react";
import { HttpError } from "../../../lib/api/core";
import { governance as api, type GovernanceProfile, type GovernanceSnapshot } from "../../../lib/api/governance";
import { getErrorMessage, relativeTime } from "../../../lib/format";
import { GOVERNANCE as GOV } from "../../../lib/governance-copy";
import { ACCESS_STATE, PEOPLE } from "../../../lib/people-access-copy";
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
import { useSecurityOperator } from "../../wardyn/operator-context";
import { PageHeader } from "../../wardyn/page-header";
import { Chip } from "../../wardyn/primitives";
import { SafetyMeter } from "../../wardyn/safety-meter";
import { EmptyState, TableSkeleton } from "../../wardyn/states";
import { AssignmentsBlock } from "./assignments";
import { Note, noteClass, question, withMono } from "./display";
import { ProfileEditor } from "./profile-editor";

const EMPTY: GovernanceSnapshot = { profiles: [], assignments: [] };

export function GovernanceScreen() {
  // The SECURITY tier, not the super-admin one: authoring profiles is what the
  // securityOps route group gates server-side, and this mirrors that predicate
  // exactly. UX only — the middleware is what refuses a write.
  const securityOperator = useSecurityOperator();
  const [snap, setSnap] = React.useState<GovernanceSnapshot>(EMPTY);
  const [status, setStatus] = React.useState<"loading" | "error" | "ready">("loading");
  // null = closed; {profile: null} = a new profile; {profile: p} = editing p.
  const [editing, setEditing] = React.useState<{ profile: GovernanceProfile | null } | null>(null);
  const [toDelete, setToDelete] = React.useState<GovernanceProfile | null>(null);
  const [deleteError, setDeleteError] = React.useState<{ title?: string; message: string } | null>(null);
  const [omission, setOmission] = React.useState<string[]>([]);
  const [busy, setBusy] = React.useState(false);

  // ponytail: no setStatus("loading") here. `load` is also the post-write
  // refresh, and flipping back to loading would unmount the add form under
  // whoever just typed in it — the skeleton belongs to the FIRST read, which is
  // what the initial state already says.
  const load = React.useCallback(() => {
    api
      .getGovernance()
      .then((s) => {
        setSnap(s);
        setStatus("ready");
      })
      .catch(() => setStatus("error"));
  }, []);
  React.useEffect(load, [load]);

  const assignedCount = React.useCallback(
    (p: GovernanceProfile) => snap.assignments.filter((a) => a.profile_id === p.id).length,
    [snap.assignments],
  );

  const openEditor = (profile: GovernanceProfile | null) => {
    setOmission([]);
    setEditing({ profile });
  };

  const openDelete = (p: GovernanceProfile) => {
    setDeleteError(null);
    setToDelete(p);
  };

  const del = async (p: GovernanceProfile) => {
    setBusy(true);
    try {
      await api.deleteProfile(p.id);
      setToDelete(null);
      load();
    } catch (e) {
      // The 409 is the ON DELETE RESTRICT refusal, and it is the authority on
      // the race: another admin assigned this profile since the list was read.
      setDeleteError(
        e instanceof HttpError && e.status === 409
          ? { title: GOV.DELETE_RESTRICT_TITLE, message: e.message }
          : { message: getErrorMessage(e) },
      );
    } finally {
      setBusy(false);
    }
  };

  const editorOpen = editing !== null;
  const noProfiles = status === "ready" && snap.profiles.length === 0;
  // The screen's single `default` button, derived in one place so two can never
  // co-occur (CONSOLE-RULES §6, prompt §4).
  const teal: "save" | "new" | "assign" = editorOpen ? "save" : noProfiles ? "new" : "assign";

  const deleteCount = toDelete ? assignedCount(toDelete) : 0;
  const [deleteHead, deleteBody] = question(toDelete ? GOV.DELETE_CONFIRM(toDelete.name) : "");

  return (
    <div className="mx-auto max-w-[1120px] px-6 py-6">
      <PageHeader title={GOV.TITLE} description={GOV.LEAD} />

      {status === "error" ? (
        <section className="mt-6 overflow-hidden rounded-xl border border-border bg-card">
          {/* Distinct from empty, and it says so: the profiles already assigned
              keep binding every run — this list just cannot show them. */}
          <EmptyState
            icon={AlertTriangle}
            title={GOV.FETCH_FAILED_TITLE}
            description={GOV.FETCH_FAILED_BODY}
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
                <h2 className="text-sm font-medium text-foreground">{GOV.PROFILES_TITLE}</h2>
                <p className="mt-1 max-w-[82ch] text-body text-muted-foreground">{GOV.PROFILES_LEAD}</p>
              </div>
              {/* With no profiles the empty state carries this action instead,
                  so the screen never offers two New profile buttons — and this
                  one is never the teal, because a list with rows in it always
                  has an Assign or a Save profile that is. */}
              {!noProfiles && (
                <Button
                  variant="outline"
                  disabled={!securityOperator || editorOpen}
                  onClick={() => openEditor(null)}
                >
                  {GOV.NEW_CTA}
                </Button>
              )}
            </div>

            {/* Q6's adopted variant: the server's omission list, rendered after
                a successful save, verbatim and in response order — never
                blocking, because narrowing by omission IS what a profile is
                for. Each warning is its own item; they are never joined. */}
            {omission.length > 0 && (
              <div className="px-6">
                <Note tone="amber">
                  <b className="font-semibold">{GOV.OMISSION_TITLE}</b>
                  {omission.map((w) => (
                    <Mono key={w} className="text-inherit">
                      {w}
                    </Mono>
                  ))}
                </Note>
              </div>
            )}

            <div className="mt-4">
              {status === "loading" ? (
                <TableSkeleton rows={3} cols={6} />
              ) : noProfiles ? (
                <EmptyState
                  icon={ShieldCheck}
                  title={GOV.EMPTY_TITLE}
                  // withMono, like every other governance surface that renders
                  // a frozen literal: WARDYN_DEFAULT_POLICY is an env var, and
                  // §7's backtick-mono rule says it looks like one EVERYWHERE
                  // it appears, not only where the container happened to
                  // accept a node.
                  description={withMono(GOV.EMPTY_BODY)}
                  action={
                    <Button
                      variant={teal === "new" ? "default" : "outline"}
                      disabled={!securityOperator || editorOpen}
                      onClick={() => openEditor(null)}
                    >
                      {GOV.NEW_CTA}
                    </Button>
                  }
                />
              ) : (
                <Table>
                  <TableHeader>
                    <TableRow className="hover:bg-transparent">
                      <TableHead>{GOV.COL_NAME}</TableHead>
                      <TableHead>{GOV.COL_ASSIGNED}</TableHead>
                      <TableHead>{GOV.COL_LIMITS}</TableHead>
                      <TableHead>{GOV.COL_GRADE}</TableHead>
                      <TableHead>{GOV.COL_UPDATED}</TableHead>
                      <TableHead className="w-[140px]" />
                    </TableRow>
                  </TableHeader>
                  <TableBody>
                    {snap.profiles.map((p) => {
                      const n = assignedCount(p);
                      return (
                        <TableRow key={p.id}>
                          {/* A profile name is a human-chosen label, never mono. */}
                          <TableCell className="font-medium">{p.name}</TableCell>
                          <TableCell>{n === 0 ? GOV.ASSIGNED_NONE : GOV.ASSIGNED_COUNT(n)}</TableCell>
                          <TableCell>
                            <span className="flex flex-wrap items-center gap-1.5">
                              {!p.limits.deny_task_mode_exec &&
                                !p.limits.deny_interactive &&
                                !p.limits.deny_user_drive &&
                                GOV.LIMITS_NONE}
                              {p.limits.deny_task_mode_exec && <Chip tone="neutral">{GOV.LIMIT_EXEC_LABEL}</Chip>}
                              {p.limits.deny_interactive && (
                                <Chip tone="neutral">{GOV.LIMIT_INTERACTIVE_LABEL}</Chip>
                              )}
                              {/* The user-drive door's chip, beside the other
                                  two (user-drives mock, state 6). */}
                              {p.limits.deny_user_drive && <Chip tone="neutral">{GOV.LIMIT_DRIVE_LABEL}</Chip>}
                            </span>
                          </TableCell>
                          <TableCell>
                            {/* The shipped meter and its own vocabulary — the
                                grade is semantic, not metal, and nothing here
                                recolours it.
                                ponytail: one debounced POST /policies/grade per
                                row. Profile lists are short; batch it if a
                                deployment ever makes that false. */}
                            <SafetyMeter spec={p.ceiling} />
                          </TableCell>
                          <TableCell className="whitespace-nowrap text-muted-foreground" title={p.updated_at}>
                            {relativeTime(p.updated_at)}
                          </TableCell>
                          <TableCell>
                            <div className="flex justify-end gap-1">
                              <Button
                                variant="outline"
                                size="sm"
                                disabled={!securityOperator || editorOpen}
                                onClick={() => openEditor(p)}
                                aria-label={`${GOV.EDIT} ${p.name}`}
                              >
                                {GOV.EDIT}
                              </Button>
                              <Button
                                variant="ghost"
                                size="sm"
                                disabled={!securityOperator || editorOpen}
                                onClick={() => openDelete(p)}
                                aria-label={`${GOV.DELETE} ${p.name}`}
                              >
                                {GOV.DELETE}
                              </Button>
                            </div>
                          </TableCell>
                        </TableRow>
                      );
                    })}
                  </TableBody>
                </Table>
              )}
            </div>

            {/* Opens IN PLACE, under the list it edits — not a dialog. The add
                form below collapses for exactly this span. */}
            {editing && (
              <ProfileEditor
                key={editing.profile?.id ?? ""}
                profile={editing.profile}
                disabled={!securityOperator}
                onCancel={() => setEditing(null)}
                onSaved={(warnings) => {
                  setEditing(null);
                  setOmission(warnings);
                  load();
                }}
              />
            )}
          </section>

          {/* Only once the read has landed: "No assignments yet" over an
              unloaded snapshot is a confident empty state that is a false
              claim (CONSOLE-RULES §10). The profiles card above holds the
              skeleton for both. */}
          {status === "ready" && (
            <AssignmentsBlock
              snapshot={snap}
              disabled={!securityOperator}
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
              // than left to fail. This is the ONE path that names a count —
              // the count is knowable only here.
              <AlertDialogDescription className={noteClass("red")}>
                <b className="font-semibold">{GOV.DELETE_RESTRICT_TITLE}</b>
                <span>{GOV.DELETE_RESTRICT_BODY(toDelete?.name ?? "", deleteCount)}</span>
              </AlertDialogDescription>
            ) : (
              <AlertDialogDescription>{deleteBody}</AlertDialogDescription>
            )}
          </AlertDialogHeader>
          {/* The race, post-attempt: count-free, the server's own message. */}
          {deleteError && (
            <Note tone="red" role="alert">
              {deleteError.title && <b className="font-semibold">{deleteError.title}</b>}
              <Mono className="text-inherit">{deleteError.message}</Mono>
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
              {GOV.DELETE}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}
