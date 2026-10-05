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
// Every string is from CHANGES (governance-copy.ts); this file adds none.
import * as React from "react";
import { Inbox } from "lucide-react";
import { toast } from "sonner";
import { HttpError } from "../../../lib/api/core";
import { governance as api } from "../../../lib/api/governance";
import { getErrorMessage, relativeTime } from "../../../lib/format";
import { CHANGES } from "../../../lib/governance-copy";
import type { GovernanceChange } from "../../../lib/types";
import { Button } from "../../ui/button";
import { Input } from "../../ui/input";
import { Sheet, SheetContent, SheetDescription, SheetHeader, SheetTitle } from "../../ui/sheet";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "../../ui/table";
import { Mono } from "../../wardyn/code-block";
import { Field } from "../../wardyn/form-primitives";
import { usePrincipal } from "../../wardyn/operator-context";
import { EmptyState } from "../../wardyn/states";
import { Note } from "./display";

// "Profile · Edit": the kind and the op, each in the table's own vocabulary. One this console predates
// shows its raw name rather than nothing.
export function changeLabel(c: GovernanceChange): string {
  return `${CHANGES.KIND[c.target_kind] ?? c.target_kind} · ${CHANGES.OP[c.op] ?? c.op}`;
}

// What a change is about, as the reviewer reads it. A profile's key is its id, which says nothing, so its
// name is read from the server's own diff (the stored row on an edit or a delete, the proposed one on a
// create). Every other kind's key is already its natural key.
export function changeTarget(c: GovernanceChange): string {
  if (c.target_kind === "governance_profile") {
    for (const side of [c.diff.after, c.diff.before]) {
      const name = (side as { name?: unknown } | undefined)?.name;
      if (typeof name === "string" && name) return name;
    }
  }
  return c.target_key;
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
  const p = (c.diff.after as { profile?: unknown } | undefined)?.profile;
  return p && typeof p === "object" ? (p as Record<string, unknown>) : null;
}

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
  return (
    <section className="mt-4 overflow-hidden rounded-xl border border-border bg-card">
      <div className="px-6 pt-5">
        <p className="max-w-[82ch] text-body text-muted-foreground">{CHANGES.LEAD}</p>
      </div>
      {loadError && (
        <div className="px-6">
          <Note tone="red" role="alert">
            <Mono className="text-inherit">{loadError}</Mono>
          </Note>
        </div>
      )}
      <div className="mt-4">
        {changes.length === 0 ? (
          // No action: a change reaches this list only by being proposed at a write site.
          <EmptyState icon={Inbox} title={CHANGES.EMPTY_TITLE} description={CHANGES.EMPTY_BODY} />
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
              {changes.map((c) => (
                <TableRow key={c.id} className="cursor-pointer" onClick={() => setOpen(c)}>
                  <TableCell className="font-medium">
                    <button type="button" className="text-left hover:underline" onClick={() => setOpen(c)}>
                      {changeLabel(c)}
                    </button>
                  </TableCell>
                  <TableCell>
                    <Mono className="text-foreground">{changeTarget(c)}</Mono>
                  </TableCell>
                  <TableCell>{c.proposed_by}</TableCell>
                  <TableCell className="whitespace-nowrap text-muted-foreground" title={c.proposed_at}>
                    {relativeTime(c.proposed_at)}
                  </TableCell>
                  <TableCell className="whitespace-nowrap text-muted-foreground" title={c.expires_at}>
                    {relativeTime(c.expires_at)}
                  </TableCell>
                </TableRow>
              ))}
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

  const own = !!change && !!principal && change.proposed_by === principal;
  const profile = change ? assignedProfile(change) : null;

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
      <SheetContent className="scroll-thin w-full gap-0 overflow-y-auto p-0 sm:max-w-[680px]" style={{ maxWidth: "98vw" }}>
        {change && (
          <>
            <SheetHeader className="border-b border-border p-5">
              <SheetTitle className="flex flex-wrap items-baseline gap-x-2">
                {changeLabel(change)}
                <Mono className="text-foreground">{changeTarget(change)}</Mono>
              </SheetTitle>
              <SheetDescription>
                {CHANGES.META(change.proposed_by, relativeTime(change.proposed_at), relativeTime(change.expires_at))}
              </SheetDescription>
            </SheetHeader>

            <div className="space-y-4 p-5">
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

              {own && <Note>{CHANGES.OWN_NOTE}</Note>}
              {refused && (
                <Note tone="red" role="alert">
                  <b className="font-semibold">{CHANGES.DECIDE_REFUSED_TITLE}</b>
                  <Mono className="text-inherit">{refused}</Mono>
                </Note>
              )}

              <div className="flex justify-end gap-2">
                <Button variant="outline" disabled={busy} onClick={() => void decide(false)}>
                  {CHANGES.REJECT}
                </Button>
                <Button disabled={busy || own} onClick={() => void decide(true)}>
                  {CHANGES.APPROVE}
                </Button>
              </div>
            </div>
          </>
        )}
      </SheetContent>
    </Sheet>
  );
}
