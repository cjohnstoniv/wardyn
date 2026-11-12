/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Governance screen's Changes tab and its drawer (0.8.6 four-eyes, mock packet M3 S2-S3): the covered
// governance writes held for a second person, the server's own diff of each, and Approve / Reject.
//
// Two rules, not styling:
//
//  1. THE DIFF IS THE SERVER'S. `diff.before`, `diff.after` and `diff.changed` are rendered as they came;
//     the console never computes a diff or looks a profile up. An assignment change reads the profile it
//     assigns from the diff's own embedded summary, as that profile stood at proposal.
//  2. THE SERVER DECIDES WHO MAY APPROVE. Approve is disabled on the viewer's own proposal as a courtesy
//     and the server remains the authority: a 403 or 409 is shown as its own sentence, verbatim, under the
//     frozen heading.
//
// Every string is from CHANGES (governance-copy.ts) or CHANGE_NOTE (wardyn/copy/change-note.ts); this file
// adds none.
import * as React from "react";
import { Inbox } from "lucide-react";
import { useNavigate } from "react-router-dom";
import { toast } from "sonner";
import { HttpError } from "../../../lib/api/core";
import { governance as api } from "../../../lib/api/governance";
import { runnerPools } from "../../../lib/api/runner-pools";
import { absoluteTime, formatDuration, getErrorMessage, relativeTime } from "../../../lib/format";
import { goDurationMs } from "../../../lib/go-duration";
import { CHANGES, GOVERNANCE } from "../../../lib/governance-copy";
import type { GovernanceChange } from "../../../lib/types";
import { Button } from "../../ui/button";
import { Input } from "../../ui/input";
import { Sheet, SheetContent, SheetDescription, SheetHeader, SheetTitle } from "../../ui/sheet";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "../../ui/table";
import { cn } from "../../ui/utils";
import { Mono } from "../../wardyn/code-block";
import { CHANGE_NOTE } from "../../wardyn/copy/change-note";
import { IMG } from "../../wardyn/copy/image-request";
import { changeTargetRef, splitImageRef } from "../elevation/image-ref";
import { Field } from "../../wardyn/form-primitives";
import { usePrincipal, useSecurityOperator } from "../../wardyn/operator-context";
import { EmptyState } from "../../wardyn/states";
import { useRovingRadio } from "../../wardyn/use-roving-radio";
import { ChangeNoteForm } from "./change-note-form";
import { Note } from "./display";

// "Profile · Edit": the kind and the op, each in the table's own vocabulary. One this console predates
// shows its raw name rather than nothing.
export function changeLabel(c: GovernanceChange): string {
  return `${CHANGES.KIND[c.target_kind] ?? c.target_kind} · ${CHANGES.OP[c.op] ?? c.op}`;
}

// What a change is about, as the reviewer reads it. A profile's key is its id, which says nothing, so its
// name is read from the server's own diff (the stored row on an edit or a delete, the proposed one on a
// create), as is a runner trust change's runner. Every other kind's key is already its natural key.
export function changeTarget(c: GovernanceChange): string {
  // A runner's key is its id; the diff carries its name on both sides.
  if (c.target_kind === "governance_unit" || c.target_kind === "runner_trust") {
    for (const side of [c.diff.after, c.diff.before]) {
      const name = (side as { name?: unknown } | undefined)?.name;
      if (typeof name === "string" && name) return name;
    }
  }
  return c.target_key;
}

// The organization runner a held removal takes out of its pool. The payload never reaches the console, so it
// is the one id the diff's runner_ids lose; null for every other change. A pool's key is its id.
function removedOrgRunner(c: GovernanceChange): string | null {
  if (c.target_kind !== "runner_pool_use_policy") return null;
  const ids = (side: unknown) => ((side as { runner_ids?: unknown } | undefined)?.runner_ids ?? []) as string[];
  const after = ids(c.diff.after);
  return ids(c.diff.before).find((id) => !after.includes(id)) ?? null;
}

// The names the removal rows read: each such change's pool and runner. A name that cannot be read (or has
// not arrived) shows its id, so the row is never blank.
function useRemovalNames(changes: GovernanceChange[]): Record<string, { pool: string; runner: string }> {
  const [names, setNames] = React.useState<Record<string, { pool: string; runner: string }>>({});
  const asked = React.useRef(new Set<string>());
  React.useEffect(() => {
    for (const c of changes) {
      const runner = removedOrgRunner(c);
      if (!runner || asked.current.has(c.id)) continue;
      asked.current.add(c.id);
      Promise.all([runnerPools.get(c.target_key), runnerPools.members(c.target_key)])
        .then(([pool, members]) => {
          const m = members.find((x) => x.kind === "self_hosted" && x.id === runner);
          setNames((n) => ({ ...n, [c.id]: { pool: pool.name, runner: m?.name ?? runner } }));
        })
        .catch(() => {});
    }
  }, [changes]);
  return names;
}

// One value of a diff cell. Strings read as they are, an absent value as DIFF_UNSET, anything else as JSON.
function cell(v: unknown): React.ReactNode {
  if (v === undefined || v === null) return <span className="text-muted-foreground">{CHANGES.DIFF_UNSET}</span>;
  return <Mono className="whitespace-pre-wrap break-words text-foreground">{typeof v === "string" ? v : JSON.stringify(v)}</Mono>;
}

// The value at a dotted path of `root`. The server's changedPaths names an array as one leaf and a
// whole-value change "(root)".
function valueAt(root: unknown, path: string): unknown {
  if (path === "(root)") return root;
  let cur: unknown = root;
  for (const key of path.split(".")) {
    if (cur === null || typeof cur !== "object") return undefined;
    cur = (cur as Record<string, unknown>)[key];
  }
  return cur;
}

// The profile an assignment change assigns, from the diff's own `after.profile`.
function assignedProfile(c: GovernanceChange): Record<string, unknown> | null {
  const p = (c.diff.after as { unit?: unknown } | undefined)?.unit;
  return p && typeof p === "object" ? (p as Record<string, unknown>) : null;
}

// An elevation request's person, unit and length, read from the server's diff (`after`). Null when a field is
// missing or the length is not a Go duration, so the row and drawer fall back to the generic ones.
function elevationOf(c: GovernanceChange): { person: string; unit: string; length: string } | null {
  if (c.target_kind !== "governance_elevation") return null;
  const a = c.diff.after as { unit_name?: unknown; subject?: unknown; duration?: unknown } | undefined;
  if (typeof a?.unit_name !== "string" || typeof a.subject !== "string" || typeof a.duration !== "string") return null;
  if (!a.unit_name || !a.subject) return null;
  const ms = goDurationMs(a.duration);
  return Number.isFinite(ms) ? { person: a.subject, unit: a.unit_name, length: formatDuration(ms, "words") } : null;
}

// An image request's person, the reference and the unit it was asked under, read from the server's diff (`after`).
// Null when a field is missing or the reference has no digest, so the row and drawer fall back to the generic ones.
function imageOf(c: GovernanceChange): { person: string; ref: string; unit: string } | null {
  if (c.target_kind !== "governance_image_request") return null;
  const a = c.diff.after as { image?: unknown; unit_name?: unknown; subject?: unknown } | undefined;
  if (typeof a?.image !== "string" || typeof a.unit_name !== "string" || typeof a.subject !== "string") return null;
  if (!a.unit_name || !a.subject || !splitImageRef(a.image).digest) return null;
  return { person: a.subject, ref: a.image, unit: a.unit_name };
}

// A person asking for something, not proposing a change: the kinds whose proposer's Reject is a withdrawal.
function isRequest(c: GovernanceChange): boolean {
  return c.target_kind === "governance_elevation" || c.target_kind === "governance_image_request";
}

// The proposer's reason, cut with an ellipsis, then the ticket in mono.
function NoteLine({ note }: { note: NonNullable<GovernanceChange["note"]> }) {
  return (
    <p className="flex items-baseline gap-1.5 text-xs font-normal text-muted-foreground">
      <span className="max-w-[48ch] truncate">{note.reason}</span>
      {note.ticket && (
        <>
          {" "}
          <Mono>{note.ticket}</Mono>
        </>
      )}
    </p>
  );
}

// "Who can decide": the all/only-security pill group, the look of the runners screen's filter.
function DeciderFilter({ onlySecurity, onChange }: { onlySecurity: boolean; onChange: (only: boolean) => void }) {
  const options = [
    { only: false, label: CHANGE_NOTE.FILTER_ALL },
    { only: true, label: CHANGE_NOTE.FILTER_SECURITY },
  ];
  const group = useRovingRadio(options.length, onlySecurity ? 1 : 0, (i) => onChange(options[i].only));
  return (
    <div role="radiogroup" aria-label={CHANGE_NOTE.FILTER_LABEL} className="flex gap-1.5" {...group.containerProps}>
      {options.map((o, i) => {
        const { tabIndex, radioRef } = group.itemProps(i);
        return (
          <button
            key={o.label}
            ref={radioRef}
            tabIndex={tabIndex}
            type="button"
            role="radio"
            aria-checked={onlySecurity === o.only}
            onClick={() => onChange(o.only)}
            className={cn(
              "rounded-full border px-2.5 py-0.5 text-xs",
              onlySecurity === o.only ? "border-primary text-foreground" : "border-border text-muted-foreground",
            )}
          >
            {o.label}
          </button>
        );
      })}
    </div>
  );
}

const NONE: GovernanceChange[] = [];

export function ChangesTab({
  changes,
  loadError,
  onChanged,
}: {
  changes: GovernanceChange[];
  /** The server's message when the list could not be read; null when it was. */
  loadError: string | null;
  onChanged: () => void;
}) {
  const [open, setOpen] = React.useState<GovernanceChange | null>(null);
  const navigate = useNavigate();
  const securityOperator = useSecurityOperator();
  // "Only security can decide" is a request of its own: the list rows do not say who could decide them, so the
  // server answers which ones are security's alone. It is read again whenever the parent reloads the list.
  const [onlySecurity, setOnlySecurity] = React.useState(false);
  const [filtered, setFiltered] = React.useState<GovernanceChange[] | null>(null);
  const [filterError, setFilterError] = React.useState<string | null>(null);
  React.useEffect(() => {
    if (!onlySecurity) return;
    let live = true;
    api
      .listChanges({ approver: "security" })
      .then((list) => {
        if (!live) return;
        setFiltered(list);
        setFilterError(null);
      })
      .catch((e) => live && setFilterError(getErrorMessage(e)));
    return () => {
      live = false;
    };
  }, [onlySecurity, changes]);
  const chooseFilter = (only: boolean) => {
    setOnlySecurity(only);
    setFiltered(null);
    setFilterError(null);
  };
  const shown = onlySecurity ? filtered : changes;
  const shownError = onlySecurity ? filterError : loadError;
  const removalNames = useRemovalNames(shown ?? NONE);
  const hasList = changes.length > 0 || (filtered?.length ?? 0) > 0;
  return (
    <section className="mt-4 overflow-hidden rounded-xl border border-border bg-card">
      {/* The lead describes the list, so it rides with the list; with nothing waiting the empty block
          below says the same thing once (#2907). */}
      {(hasList || (securityOperator && onlySecurity)) && (
        <div className="flex flex-wrap items-start justify-between gap-3 px-6 pt-5">
          {hasList && <p className="max-w-[82ch] text-body text-muted-foreground">{CHANGES.LEAD}</p>}
          {securityOperator && <DeciderFilter onlySecurity={onlySecurity} onChange={chooseFilter} />}
        </div>
      )}
      {shownError && (
        <div className="px-6 pt-5">
          <Note tone="red" role="alert">
            <Mono className="text-inherit">{shownError}</Mono>
          </Note>
        </div>
      )}
      <div className={shown && shown.length > 0 ? "mt-4" : undefined}>
        {shown === null ? null : shown.length === 0 && onlySecurity ? (
          <div data-testid="changes-empty-filtered">
            <EmptyState
              size="compact"
              icon={Inbox}
              title={CHANGE_NOTE.FILTERED_EMPTY_TITLE}
              description={CHANGE_NOTE.FILTERED_EMPTY_BODY}
              action={
                <Button variant="outline" size="sm" onClick={() => chooseFilter(false)}>
                  {CHANGE_NOTE.FILTERED_EMPTY_ACTION}
                </Button>
              }
            />
          </div>
        ) : shown.length === 0 ? (
          // A change reaches this list only by being proposed at a write site, so the one door is the audit
          // trail, where the decided ones are.
          <div data-testid="changes-empty">
            <EmptyState
              size="compact"
              icon={Inbox}
              title={CHANGES.EMPTY_TITLE}
              description={CHANGES.EMPTY_BODY}
              action={
                <Button
                  variant="outline"
                  size="sm"
                  data-testid="changes-empty-audit"
                  onClick={() => navigate("/admin/audit")}
                >
                  {CHANGES.EMPTY_CTA}
                </Button>
              }
            />
          </div>
        ) : (
          <Table>
            <TableHeader>
              <TableRow className="hover:bg-transparent">
                <TableHead>{CHANGES.COL_CHANGE}</TableHead>
                <TableHead>{CHANGES.COL_TARGET}</TableHead>
                <TableHead>{CHANGES.COL_BY}</TableHead>
                <TableHead>{CHANGES.COL_PROPOSED}</TableHead>
                <TableHead>{CHANGES.COL_EXPIRES}</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {shown.map((c) => {
                const gone = removedOrgRunner(c);
                const elevation = elevationOf(c);
                const img = imageOf(c);
                return (
                  <TableRow key={c.id} className="cursor-pointer" onClick={() => setOpen(c)}>
                    <TableCell className="font-medium">
                      <button type="button" className="text-left hover:underline" onClick={() => setOpen(c)}>
                        {gone
                          ? CHANGES.ORG_RUNNER_REMOVE(removalNames[c.id]?.runner ?? gone, removalNames[c.id]?.pool ?? c.target_key)
                          : changeLabel(c)}
                      </button>
                      {gone && (
                        <p className="text-xs font-normal text-muted-foreground">{CHANGES.SUPER_ADMIN_DECIDES}</p>
                      )}
                      {elevation ? (
                        <p className="text-xs font-normal text-muted-foreground">
                          {CHANGE_NOTE.ELEVATION_LINE(elevation.length, c.note?.ticket)}
                        </p>
                      ) : img ? (
                        <p className="text-xs font-normal text-muted-foreground">{IMG.CHANGE_LINE(c.note?.ticket)}</p>
                      ) : (
                        c.note && <NoteLine note={c.note} />
                      )}
                    </TableCell>
                    <TableCell>
                      {elevation ? (
                        CHANGE_NOTE.ELEVATION_TARGET(elevation.person, elevation.unit)
                      ) : img ? (
                        <Mono className="text-foreground">{IMG.CHANGE_TARGET(img.person, changeTargetRef(img.ref))}</Mono>
                      ) : (
                        <Mono className="text-foreground">{changeTarget(c)}</Mono>
                      )}
                    </TableCell>
                    <TableCell>{c.proposed_by}</TableCell>
                    <TableCell className="whitespace-nowrap text-muted-foreground" title={absoluteTime(c.proposed_at)}>
                      {relativeTime(c.proposed_at)}
                    </TableCell>
                    <TableCell className="whitespace-nowrap text-muted-foreground" title={absoluteTime(c.expires_at)}>
                      {relativeTime(c.expires_at)}
                    </TableCell>
                  </TableRow>
                );
              })}
            </TableBody>
          </Table>
        )}
      </div>
      <ChangeDrawer
        // Keyed by change so a reason typed for one never carries to the next.
        key={open?.id ?? ""}
        change={open}
        onClose={() => setOpen(null)}
        onChanged={onChanged}
      />
    </section>
  );
}

export function ChangeDrawer({
  change,
  onClose,
  onChanged,
}: {
  change: GovernanceChange | null;
  onClose: () => void;
  onChanged: () => void;
}) {
  const principal = usePrincipal();
  const [reason, setReason] = React.useState("");
  const [busy, setBusy] = React.useState(false);
  const [refused, setRefused] = React.useState<string | null>(null);
  // The change as the note form last saved it: the list behind the drawer reloads, this drawer keeps its own copy.
  const [noted, setNoted] = React.useState<GovernanceChange | null>(null);
  const [editing, setEditing] = React.useState(false);
  // Whether only security can decide this change, read from the change itself: the list does not carry approvers.
  const [securityOnly, setSecurityOnly] = React.useState(false);

  const own = !!change && !!principal && change.proposed_by === principal;
  const shown = noted ?? change;
  const profile = shown ? assignedProfile(shown) : null;
  const elevation = shown ? elevationOf(shown) : null;
  const img = shown ? imageOf(shown) : null;
  const withdrawal = own && !!shown && isRequest(shown);
  const hasNote = !!(shown?.note?.reason || shown?.note?.ticket);
  const id = change?.id;

  React.useEffect(() => {
    if (!own || !id) return;
    let live = true;
    api
      .getChange(id)
      .then((c) => live && setSecurityOnly(!!c.approvers?.length && c.approvers.every((a) => a.kind === "security")))
      .catch(() => {});
    return () => {
      live = false;
    };
  }, [own, id]);

  const decide = async (approve: boolean) => {
    if (!change) return;
    setBusy(true);
    setRefused(null);
    try {
      if (approve) await api.approveChange(change.id);
      else await api.rejectChange(change.id, reason.trim());
      toast.success(approve ? CHANGES.TOAST_APPROVED : CHANGES.TOAST_REJECTED);
      onChanged();
      onClose();
    } catch (e) {
      setRefused(e instanceof HttpError ? e.message : getErrorMessage(e));
      // A 409 means the change is no longer what was reviewed (stale, expired, or decided already): the
      // list behind the drawer is out of date.
      if (e instanceof HttpError && e.status === 409) onChanged();
    } finally {
      setBusy(false);
    }
  };

  return (
    <Sheet open={!!change} onOpenChange={(o) => !o && onClose()}>
      <SheetContent className="scroll-thin w-full gap-0 overflow-y-auto p-0 sm:max-w-[680px]">
        {change && (
          <>
            <SheetHeader className="border-b border-border p-5 pr-10">
              <SheetTitle className="flex min-w-0 flex-wrap items-baseline gap-x-2 break-words">
                <span className="min-w-0 [overflow-wrap:anywhere]">{changeLabel(change)}</span>
                <Mono className="min-w-0 text-foreground [overflow-wrap:anywhere]">
                  {elevation
                    ? CHANGE_NOTE.ELEVATION_TARGET(elevation.person, elevation.unit)
                    : img
                      ? IMG.CHANGE_TARGET(img.person, changeTargetRef(img.ref))
                      : changeTarget(change)}
                </Mono>
              </SheetTitle>
              <SheetDescription>
                {CHANGES.META(change.proposed_by, relativeTime(change.proposed_at), relativeTime(change.expires_at))}
              </SheetDescription>
            </SheetHeader>

            <div className="space-y-4 p-5">
              {elevation ? (
                // A request reads as three plain rows; the full before and after stays below.
                <Table>
                  <TableBody>
                    {[
                      [CHANGE_NOTE.ELEV_PERSON, elevation.person],
                      [CHANGE_NOTE.ELEV_UNIT, elevation.unit],
                      [CHANGE_NOTE.ELEV_LENGTH, CHANGE_NOTE.ELEV_LENGTH_VALUE(elevation.length)],
                    ].map(([label, value]) => (
                      <TableRow key={label}>
                        <TableCell className="text-muted-foreground">{label}</TableCell>
                        <TableCell>{value}</TableCell>
                      </TableRow>
                    ))}
                  </TableBody>
                </Table>
              ) : img ? (
                <Table>
                  <TableBody>
                    {[
                      [IMG.ROW_PERSON, img.person],
                      [IMG.ROW_IMAGE, <Mono key="n" className="text-foreground">{splitImageRef(img.ref).name}</Mono>],
                      [IMG.ROW_DIGEST, <Mono key="d" className="break-all text-foreground">{splitImageRef(img.ref).digest}</Mono>],
                      [IMG.ROW_ASKED_UNDER, IMG.ASKED_UNDER(img.unit)],
                      [IMG.ROW_WRITES, IMG.WRITES],
                    ].map(([label, value]) => (
                      <TableRow key={String(label)}>
                        <TableCell className="align-top text-muted-foreground">{label}</TableCell>
                        <TableCell>{value}</TableCell>
                      </TableRow>
                    ))}
                  </TableBody>
                </Table>
              ) : (
                <Table>
                  <TableHeader>
                    <TableRow className="hover:bg-transparent">
                      <TableHead>{CHANGES.DIFF_FIELD}</TableHead>
                      <TableHead>{CHANGES.DIFF_BEFORE}</TableHead>
                      <TableHead>{CHANGES.DIFF_AFTER}</TableHead>
                    </TableRow>
                  </TableHeader>
                  <TableBody>
                    {change.diff.changed.map((path) => (
                      <TableRow key={path}>
                        <TableCell className="align-top">
                          <Mono className="text-foreground">{path}</Mono>
                        </TableCell>
                        <TableCell className="align-top">{cell(valueAt(change.diff.before, path))}</TableCell>
                        <TableCell className="align-top">{cell(valueAt(change.diff.after, path))}</TableCell>
                      </TableRow>
                    ))}
                  </TableBody>
                </Table>
              )}

              <details>
                <summary className="cursor-pointer text-body text-muted-foreground">{CHANGES.DIFF_FULL}</summary>
                <div className="mt-2 grid gap-2 sm:grid-cols-2">
                  {[change.diff.before, change.diff.after].map((side, i) => (
                    <pre
                      key={i}
                      className="scroll-thin max-h-72 overflow-auto rounded-lg bg-muted p-3 font-mono text-xs"
                    >
                      {side === undefined ? CHANGES.DIFF_UNSET : JSON.stringify(side, null, 2)}
                    </pre>
                  ))}
                </div>
              </details>

              {profile && (
                <section data-testid="change-assigned-profile">
                  <h4 className="text-body font-medium text-foreground">
                    {CHANGES.ASSIGNED_PROFILE}
                    {typeof profile.name === "string" && <span className="ml-2 font-normal">{profile.name}</span>}
                  </h4>
                  <pre className="scroll-thin mt-1 max-h-72 overflow-auto rounded-lg bg-muted p-3 font-mono text-xs">
                    {JSON.stringify({ ceiling: profile.ceiling, limits: profile.limits, error: profile.error }, null, 2)}
                  </pre>
                </section>
              )}

              {/* R1-0098: the notes a direct save would have shown, for the person approving, between the diff and the
                  reason (after the profile an assignment points at). Same amber note and heading the profile screen puts them
                  under after a save. */}
              {change.diff.notes && change.diff.notes.length > 0 && (
                <Note tone="amber">
                  <b className="font-semibold">{GOVERNANCE.OMISSION_TITLE}</b>
                  {change.diff.notes.map((n) => (
                    <Mono key={n} className="text-inherit">
                      {n}
                    </Mono>
                  ))}
                </Note>
              )}

              {/* The proposer's note, between the diff (and its notes) and the decider's own reason. */}
              {editing ? (
                <section className="space-y-2">
                  <h4 className="text-body font-medium text-foreground">{CHANGE_NOTE.WHY_TITLE}</h4>
                  <ChangeNoteForm
                    change={shown!}
                    onSaved={(c) => {
                      setNoted(c);
                      setEditing(false);
                      onChanged();
                    }}
                    onCancel={() => setEditing(false)}
                    onGone={(status) => {
                      onChanged();
                      if (status === 404) onClose();
                    }}
                  />
                </section>
              ) : (
                <>
                  {hasNote && (
                    <section data-testid="change-note" className="space-y-1">
                      <h4 className="text-body font-medium text-foreground">{CHANGE_NOTE.WHY_TITLE}</h4>
                      {shown?.note?.reason && (
                        <p className="whitespace-pre-wrap break-words text-body text-foreground">{shown.note.reason}</p>
                      )}
                      {shown?.note?.ticket && (
                        <p className="text-xs text-muted-foreground">
                          {CHANGE_NOTE.TICKET(shown.note.ticket).label} <Mono className="break-all">{shown.note.ticket}</Mono>
                        </p>
                      )}
                    </section>
                  )}
                  {own && (
                    <div className="flex flex-wrap items-center gap-3">
                      <Button variant="outline" size="sm" onClick={() => setEditing(true)}>
                        {hasNote ? CHANGE_NOTE.EDIT : CHANGE_NOTE.ADD}
                      </Button>
                      {!hasNote && <span className="text-xs text-muted-foreground">{CHANGE_NOTE.NO_NOTE_HINT}</span>}
                    </div>
                  )}
                </>
              )}
              {!editing && !withdrawal && (
                <Field label={CHANGES.REASON_LABEL} htmlFor="governance-change-reason" hint={CHANGES.REASON_HINT}>
                  <Input
                    id="governance-change-reason"
                    value={reason}
                    onChange={(e) => setReason(e.target.value)}
                    disabled={busy}
                    maxLength={512}
                    autoComplete="off"
                  />
                </Field>
              )}

              {own &&
                (withdrawal ? (
                  <Note>{CHANGE_NOTE.OWN_REQUEST}</Note>
                ) : securityOnly ? (
                  <Note>
                    <b className="font-semibold">{CHANGE_NOTE.OWN_SECURITY_TITLE}</b>
                    <span>{CHANGE_NOTE.OWN_SECURITY_BODY}</span>
                  </Note>
                ) : (
                  <Note>{CHANGES.OWN_NOTE}</Note>
                ))}
              {refused && (
                <Note tone="red" role="alert">
                  <b className="font-semibold">{CHANGES.DECIDE_REFUSED_TITLE}</b>
                  <Mono className="text-inherit">{refused}</Mono>
                </Note>
              )}

              {!editing && (
                <div className="flex justify-end gap-2">
                  <Button variant="outline" disabled={busy} onClick={() => void decide(false)}>
                    {withdrawal ? CHANGE_NOTE.WITHDRAW_REQUEST : CHANGES.REJECT}
                  </Button>
                  <Button disabled={busy || own} onClick={() => void decide(true)}>
                    {CHANGES.APPROVE}
                  </Button>
                </div>
              )}
            </div>
          </>
        )}
      </SheetContent>
    </Sheet>
  );
}
