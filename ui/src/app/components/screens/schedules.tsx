/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Schedules: a person's repeating runs (/runs/schedules), or every person's in the Admin view
// (/admin/runs/schedules), with Pause, Resume and Delete. Only the owner resumes, so the Admin view has no Resume. A
// refused Resume shows the server's sentence under its row; one refused because scheduled runs were turned off since
// the page loaded also reads the setup status again, so the screen turns to its off form and never offers the click
// twice.

import * as React from "react";
import { Link } from "react-router-dom";
import { CalendarClock, Loader2 } from "lucide-react";
import { HttpError } from "../../lib/api/core";
import { runSchedules, type RunSchedule } from "../../lib/api/run-schedules";
import { getErrorMessage } from "../../lib/format";
import { dayTimeText, scheduledRuns, weekdayName, whenText } from "../../lib/schedule";
import { Button } from "../ui/button";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "../ui/table";
import {
  AlertDialog,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "../ui/alert-dialog";
import { Mono } from "../wardyn/code-block";
import { useConsoleMode } from "../wardyn/console-view";
import { useShellSetupStatus } from "../wardyn/model-access-context";
import { PageHeader } from "../wardyn/page-header";
import { Chip } from "../wardyn/primitives";
import { EmptyState, ErrorState, TableSkeleton } from "../wardyn/states";
import { PAUSE_REASON, RESUME_OFF_SENTENCE, RUN_SCHEDULE, RUN_SCHEDULE_REFUSAL } from "../wardyn/copy/run-schedules";
import { refusalSentence } from "./new-run-flow/launch/refusal-text";

const OFF_REASON = "scheduled_runs_off";
const EN_DASH = "–";

/** The outcome word of a schedule's last start. */
function outcomeWord(outcome: string): string {
  if (outcome === "booked") return RUN_SCHEDULE.OUTCOME_BOOKED;
  if (outcome === "missed") return RUN_SCHEDULE.OUTCOME_MISSED;
  if (outcome === "refused") return RUN_SCHEDULE.OUTCOME_REFUSED;
  if (outcome.startsWith("skipped:")) return RUN_SCHEDULE.OUTCOME_SKIPPED;
  if (outcome.startsWith("cancelled:")) return RUN_SCHEDULE.OUTCOME_CANCELLED;
  return outcome;
}

/** A pause reason as a sentence: the member's form, its form while scheduled runs are off, or the admin's. */
function PauseSentence({ reason, admin, off }: { reason: string; admin: boolean; off: boolean }) {
  const form = admin ? "admin" : off ? "off" : "member";
  const known = PAUSE_REASON[reason];
  if (known) return <>{known[form]}</>;
  const refused = reason.startsWith("refused:") ? reason.slice("refused:".length) : null;
  if (refused === null) return <Mono>{reason}</Mono>;
  const mapped = refusalSentence(refused);
  const tail = admin ? ` ${RUN_SCHEDULE.ONLY_OWNER}` : off ? "" : ` ${RUN_SCHEDULE.REFUSED_RESUME}`;
  return mapped ? (
    <>{`${RUN_SCHEDULE.PAUSED_REFUSED_LEAD} ${mapped}${tail}`}</>
  ) : (
    <>
      {`${RUN_SCHEDULE.PAUSED_REFUSED_LEAD} `}
      <Mono>{refused}</Mono>
      {`.${tail}`}
    </>
  );
}

/** A refused Resume's sentence: the server's own, or the reason's words when it sent none. */
function resumeRefusal(e: unknown): string {
  const message = getErrorMessage(e, "");
  if (e instanceof HttpError && !/\s/.test(message.trim())) {
    if (e.reason === OFF_REASON) return RESUME_OFF_SENTENCE;
    if (RUN_SCHEDULE_REFUSAL[e.reason]) return RUN_SCHEDULE_REFUSAL[e.reason];
  }
  return getErrorMessage(e);
}

export function SchedulesScreen() {
  const admin = useConsoleMode() === "admin";
  const { status, refresh } = useShellSetupStatus();
  const off = scheduledRuns(status) === false;
  const [rows, setRows] = React.useState<RunSchedule[] | null>(null);
  const [loadError, setLoadError] = React.useState<string | null>(null);
  // A refused Resume's sentence, under its row. It stays when the screen turns to its off form.
  const [rowError, setRowError] = React.useState<{ id: string; text: string } | null>(null);
  const [busy, setBusy] = React.useState<string | null>(null);
  const [toDelete, setToDelete] = React.useState<RunSchedule | null>(null);
  const [deleting, setDeleting] = React.useState(false);
  const [deleteError, setDeleteError] = React.useState<string | null>(null);
  // Pause and Resume swap in place: focus stays on the swapped button.
  const swapped = React.useRef<string | null>(null);
  const swapButtons = React.useRef(new Map<string, HTMLButtonElement>());

  const load = React.useCallback(async () => {
    try {
      setRows(await runSchedules.list(admin));
      setLoadError(null);
    } catch (e) {
      setLoadError(getErrorMessage(e));
    }
  }, [admin]);
  React.useEffect(() => {
    void load();
  }, [load]);
  React.useEffect(() => {
    if (!swapped.current) return;
    swapButtons.current.get(swapped.current)?.focus();
    swapped.current = null;
  });

  const replace = (next: RunSchedule) => setRows((all) => all?.map((r) => (r.id === next.id ? { ...next, owner: r.owner } : r)) ?? null);
  const act = async (sc: RunSchedule, verb: "pause" | "resume") => {
    setBusy(sc.id);
    setRowError(null);
    try {
      replace(await runSchedules[verb](sc.id));
      swapped.current = sc.id;
    } catch (e) {
      setRowError({ id: sc.id, text: resumeRefusal(e) });
      // The switch went off after this page read it: read it again, so every Resume turns unavailable.
      if (e instanceof HttpError && e.reason === OFF_REASON) void refresh();
    } finally {
      setBusy(null);
    }
  };
  const del = async (sc: RunSchedule) => {
    setDeleting(true);
    setDeleteError(null);
    try {
      await runSchedules.remove(sc.id);
      setRows((all) => all?.filter((r) => r.id !== sc.id) ?? null);
      setToDelete(null);
    } catch (e) {
      setDeleteError(getErrorMessage(e));
    } finally {
      setDeleting(false);
    }
  };

  const owner = (sc: RunSchedule) => (admin ? sc.owner : undefined);
  const bookedNote = admin ? RUN_SCHEDULE.BOOKED_NOTE_ADMIN : RUN_SCHEDULE.BOOKED_NOTE;
  const runHref = (id: string) => `${admin ? "/admin" : ""}/runs/${encodeURIComponent(id)}`;
  const cols = admin ? 7 : 6;

  return (
    <section className="flex flex-col gap-4">
      <PageHeader title={RUN_SCHEDULE.TITLE} description={admin ? RUN_SCHEDULE.LEDE_ADMIN : RUN_SCHEDULE.LEDE} />
      {off && (
        <p role="status" className="rounded-md border border-warning/30 bg-warning-subtle p-3 text-sm">
          {RUN_SCHEDULE.OFF_BANNER}
        </p>
      )}
      {loadError !== null && rows === null ? (
        <ErrorState message={loadError} onRetry={() => void load()} />
      ) : rows === null ? (
        <TableSkeleton rows={3} cols={cols} />
      ) : rows.length === 0 ? (
        <div className="overflow-hidden rounded-xl border border-border bg-card">
          <EmptyState icon={CalendarClock} title={RUN_SCHEDULE.EMPTY_TITLE} description={admin ? undefined : RUN_SCHEDULE.EMPTY_BODY} />
        </div>
      ) : (
        <>
          <div className="overflow-x-auto rounded-xl border border-border bg-card px-4">
            <Table className="table-fixed">
              <TableHeader>
                <TableRow className="hover:bg-transparent">
                  <TableHead className="w-[18%]">{RUN_SCHEDULE.COL_NAME}</TableHead>
                  {admin && <TableHead className="w-[14%]">{RUN_SCHEDULE.COL_OWNER}</TableHead>}
                  <TableHead className="w-[16%]">{RUN_SCHEDULE.COL_REPEATS}</TableHead>
                  <TableHead className="w-[17%]">{RUN_SCHEDULE.COL_NEXT}</TableHead>
                  <TableHead className="w-[17%]">{RUN_SCHEDULE.COL_LAST}</TableHead>
                  <TableHead className="w-[10%]">{RUN_SCHEDULE.COL_STATE}</TableHead>
                  <TableHead className="w-[14%]" />
                </TableRow>
              </TableHeader>
              <TableBody>
                {rows.map((sc) => {
                  const paused = sc.paused;
                  const reasonId = `schedule-${sc.id}-reason`;
                  const offId = `schedule-${sc.id}-off`;
                  const errorId = `schedule-${sc.id}-error`;
                  const error = rowError?.id === sc.id ? rowError.text : null;
                  const below = !!paused || error !== null;
                  return (
                    <React.Fragment key={sc.id}>
                      <TableRow className={below ? "border-b-0 hover:bg-transparent" : "hover:bg-transparent"}>
                        <TableCell className="whitespace-normal break-words align-top font-medium">{sc.name}</TableCell>
                        {admin && <TableCell className="whitespace-normal break-words align-top">{sc.owner}</TableCell>}
                        <TableCell className="whitespace-normal break-words align-top">
                          {RUN_SCHEDULE.RULE(sc.every, sc.time, weekdayName(sc.weekday ?? 0))}
                        </TableCell>
                        <TableCell className="whitespace-normal break-words align-top">
                          {paused || off ? EN_DASH : whenText(sc.next_at, sc.time_zone)}
                        </TableCell>
                        <TableCell className="whitespace-normal align-top">
                          {sc.last ? (
                            <div className="flex flex-col gap-0.5">
                              <span>{dayTimeText(sc.last.due_at, sc.time_zone)}</span>
                              <span className="text-muted-foreground">
                                {outcomeWord(sc.last.outcome)}
                                {sc.last.run_id && (
                                  <>
                                    {" • "}
                                    <Link className="text-primary underline-offset-4 hover:underline" to={runHref(sc.last.run_id)} aria-label={RUN_SCHEDULE.OPEN_LAST(sc.name, owner(sc))}>
                                      {RUN_SCHEDULE.OPEN_RUN}
                                    </Link>
                                  </>
                                )}
                              </span>
                            </div>
                          ) : (
                            EN_DASH
                          )}
                        </TableCell>
                        <TableCell className="align-top">
                          <Chip dot tone={paused ? "warning" : "success"}>{paused ? RUN_SCHEDULE.PAUSED : RUN_SCHEDULE.ACTIVE}</Chip>
                        </TableCell>
                        <TableCell className="align-top">
                          <div className="flex justify-end gap-1">
                            {!paused ? (
                              <Button
                                ref={(el) => void (el ? swapButtons.current.set(sc.id, el) : swapButtons.current.delete(sc.id))}
                                variant="ghost"
                                size="sm"
                                disabled={busy === sc.id}
                                aria-label={RUN_SCHEDULE.ROW_NAME(RUN_SCHEDULE.PAUSE, sc.name, owner(sc))}
                                onClick={() => void act(sc, "pause")}
                              >
                                {busy === sc.id && <Loader2 className="size-4 animate-spin" />}
                                {RUN_SCHEDULE.PAUSE}
                              </Button>
                            ) : admin ? null : off ? (
                              <Button
                                ref={(el) => void (el ? swapButtons.current.set(sc.id, el) : swapButtons.current.delete(sc.id))}
                                variant="ghost"
                                size="sm"
                                className="hatch-unavailable cursor-not-allowed text-muted-foreground"
                                aria-disabled="true"
                                aria-describedby={offId}
                                aria-label={RUN_SCHEDULE.ROW_NAME(RUN_SCHEDULE.RESUME, sc.name)}
                                onClick={(e) => e.preventDefault()}
                              >
                                {RUN_SCHEDULE.RESUME}
                              </Button>
                            ) : (
                              <Button
                                ref={(el) => void (el ? swapButtons.current.set(sc.id, el) : swapButtons.current.delete(sc.id))}
                                variant="ghost"
                                size="sm"
                                disabled={busy === sc.id}
                                aria-describedby={error !== null ? errorId : undefined}
                                aria-label={RUN_SCHEDULE.ROW_NAME(RUN_SCHEDULE.RESUME, sc.name)}
                                onClick={() => void act(sc, "resume")}
                              >
                                {busy === sc.id && <Loader2 className="size-4 animate-spin" />}
                                {RUN_SCHEDULE.RESUME}
                              </Button>
                            )}
                            <Button
                              variant="ghost"
                              size="sm"
                              aria-label={RUN_SCHEDULE.ROW_NAME(RUN_SCHEDULE.DELETE, sc.name, owner(sc))}
                              onClick={() => {
                                setDeleteError(null);
                                setToDelete(sc);
                              }}
                            >
                              {RUN_SCHEDULE.DELETE}
                            </Button>
                          </div>
                        </TableCell>
                      </TableRow>
                      {below && (
                        <TableRow className="hover:bg-transparent">
                          <TableCell colSpan={cols} className="whitespace-normal pt-0">
                            {paused && (
                              <p id={reasonId} className="flex items-baseline gap-2 text-sm text-muted-foreground">
                                <span aria-hidden="true" className="inline-block size-1.5 shrink-0 translate-y-[-1px] rounded-full bg-warning" />
                                <span>
                                  <PauseSentence reason={paused.reason} admin={admin} off={off} />
                                </span>
                              </p>
                            )}
                            {paused && off && !admin && (
                              <p id={offId} className="mt-1 pl-3.5 text-sm text-muted-foreground">
                                {RUN_SCHEDULE.RESUME_OFF_REASON}
                              </p>
                            )}
                            {error !== null && (
                              <p id={errorId} role="alert" className="mt-1 text-sm text-danger">
                                {error}
                              </p>
                            )}
                          </TableCell>
                        </TableRow>
                      )}
                    </React.Fragment>
                  );
                })}
              </TableBody>
            </Table>
          </div>
          <p className="text-xs text-muted-foreground">{bookedNote}</p>
        </>
      )}

      <AlertDialog open={!!toDelete} onOpenChange={(o) => !o && !deleting && setToDelete(null)}>
        <AlertDialogContent
          onOpenAutoFocus={(e) => {
            // First focus on Delete schedule, not Cancel.
            e.preventDefault();
            (e.currentTarget as HTMLElement).querySelector<HTMLButtonElement>("[data-delete-confirm]")?.focus();
          }}
        >
          <AlertDialogHeader>
            <AlertDialogTitle>{toDelete ? RUN_SCHEDULE.DELETE_TITLE(toDelete.name) : ""}</AlertDialogTitle>
            <AlertDialogDescription>{`${RUN_SCHEDULE.DELETE_BODY} ${bookedNote}`}</AlertDialogDescription>
          </AlertDialogHeader>
          {deleteError && (
            <p role="alert" className="text-sm text-danger">
              {deleteError}
            </p>
          )}
          <AlertDialogFooter>
            <Button variant="ghost" disabled={deleting} onClick={() => setToDelete(null)}>
              {RUN_SCHEDULE.CANCEL}
            </Button>
            <Button data-delete-confirm variant="destructive" disabled={deleting} onClick={() => toDelete && void del(toDelete)}>
              {deleting && <Loader2 className="size-4 animate-spin" />}
              {RUN_SCHEDULE.DELETE_CONFIRM}
            </Button>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </section>
  );
}
