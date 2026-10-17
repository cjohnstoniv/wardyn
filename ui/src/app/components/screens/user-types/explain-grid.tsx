/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// "What this type gets" (design §2.6, packet A) over GET /permissions/explain
// (#739). Per kind: its default ("*") row, then one row per value a grant
// names or "Available to" restricts. The state is the server's answer; this
// file only names the value and adds what packet A draws beside it: the
// audience of a restricted value this type is not listed for, the wall note
// under a block, and Remove on a row written for this type.
import * as React from "react";
import { AlertTriangle, Loader2 } from "lucide-react";
import { HttpError, PendingChangeError } from "../../../lib/api/core";
import { permissions as api, type ExplainRow, type ExplainState } from "../../../lib/api/permissions";
import { AVAILABILITY } from "../../../lib/availability-copy";
import { getErrorMessage } from "../../../lib/format";
import { CAPABILITY_KINDS, KIND, PERM, type CapabilityKind } from "../../../lib/permissions-copy";
import type { CapabilityGrant } from "../../../lib/types";
import { useUserTypeName } from "../../../lib/use-user-types";
import { EXPLAIN, USER_TYPES as UT } from "../../../lib/user-types-copy";
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
import { Dialog, DialogContent, DialogHeader, DialogTitle } from "../../ui/dialog";
import { cn } from "../../ui/utils";
import { Mono } from "../../wardyn/code-block";
import { Chip } from "../../wardyn/primitives";
import { Note } from "../governance/display";
import { SubmittedNote } from "../governance/submitted-note";
import { AddGrantForm } from "../permissions";
import { useValueNames } from "./explain-names";

// Packet A: green for this type's own, red for a block, grey otherwise.
const STATE_TONE: Record<ExplainState, "neutral" | "success" | "danger"> = {
  everyone: "neutral",
  this_type: "success",
  blocked: "danger",
  admins_only: "neutral",
  not_available: "neutral",
};

const same = (a: string, b: string) => a.trim() === b.trim();

// G-5: "only A and B" (two), "only A, B and C" (three) name every audience —
// nothing is elided, so there is nothing more to disclose. Four or more names
// the first two and counts the rest; `elided` says so, and `full` (every
// audience, comma-joined) is then what an sr-only twin renders as real text —
// a keyboard or screen-reader user reaches it the same way a sighted mouse
// user reaches the truncated span's `title`, not through an aria-label a
// plain, non-focusable span can't expose (F1, FINAL-PR-907.md).
function onlyPhrase(who: string[]): { visible: string; full: string; elided: boolean } {
  const full = who.join(", ");
  if (who.length === 1) return { visible: EXPLAIN.ONLY(who[0]), full, elided: false };
  if (who.length === 2) return { visible: EXPLAIN.ONLY(EXPLAIN.WHO_LIST(who[0], who[1], "")), full, elided: false };
  if (who.length === 3) return { visible: EXPLAIN.ONLY(EXPLAIN.WHO_LIST(who[0], who[1], who[2])), full, elided: false };
  return { visible: EXPLAIN.ONLY(EXPLAIN.WHO_MORE(who[0], who[1], String(who.length - 2))), full, elided: true };
}

export function ExplainGrid({ subject, name, disabled }: { subject: string; name: string; disabled: boolean }) {
  const [rows, setRows] = React.useState<ExplainRow[] | null>(null);
  const [grants, setGrants] = React.useState<CapabilityGrant[]>([]);
  const [failed, setFailed] = React.useState<string | null>(null);
  const [toRemove, setToRemove] = React.useState<CapabilityGrant | null>(null);
  const [busy, setBusy] = React.useState(false);
  const [removeError, setRemoveError] = React.useState<string | null>(null);
  // A remove the server held for a second person (202): the grant is still there.
  const [submitted, setSubmitted] = React.useState(false);
  // G-7: which family's Add dialog is open, fixed to this subject and kind.
  const [addKind, setAddKind] = React.useState<CapabilityKind | null>(null);
  const typeName = useUserTypeName();
  const valueName = useValueNames();

  const load = React.useCallback(() => {
    let active = true;
    api
      .explainCapabilities("user_type", subject, [...CAPABILITY_KINDS])
      .then((res) => {
        if (!active) return;
        setRows(res.rows);
        setFailed(null);
      })
      // A 400 (a type deleted since the list loaded) carries the server's own
      // sentence, shown as sent under the failure line.
      .catch((e) => active && setFailed(e instanceof HttpError ? e.message : ""));
    // The grant rows only add Remove and "only …"; without them the grid
    // still answers.
    api
      .getPermissions()
      .then((snap) => active && setGrants(snap.grants))
      .catch(() => active && setGrants([]));
    return () => {
      active = false;
    };
  }, [subject]);
  React.useEffect(load, [load]);

  const remove = async (g: CapabilityGrant) => {
    setBusy(true);
    setRemoveError(null);
    setSubmitted(false);
    try {
      await api.deleteGrant(g.id);
      load();
    } catch (e) {
      if (e instanceof PendingChangeError) {
        setSubmitted(true);
        load();
        return;
      }
      setRemoveError(e instanceof HttpError ? e.message : getErrorMessage(e));
    } finally {
      setToRemove(null);
      setBusy(false);
    }
  };

  if (failed !== null) {
    return (
      <div className="text-body text-muted-foreground">
        <p className="flex items-center gap-2">
          <AlertTriangle className="size-3.5 shrink-0" />
          {UT.EXPLAIN_LOAD_FAILED}
        </p>
        {failed && <Note tone="red">{failed}</Note>}
      </div>
    );
  }
  if (!rows) {
    return (
      <p className="flex items-center gap-2 text-body text-muted-foreground">
        <Loader2 className="size-3.5 shrink-0 animate-spin" />
      </p>
    );
  }

  // The grant row this type wrote for exactly this cell — what Remove deletes.
  const ownGrant = (r: ExplainRow) =>
    grants.find(
      (g) => g.subject_type === "user_type" && g.subject === subject && g.capability === r.kind && same(g.value, r.value),
    );
  // Who a restricted value is listed for: the audiences of its allow rows.
  const onlyFor = (r: ExplainRow) =>
    grants
      .filter((g) => g.effect === "allow" && g.subject_type !== "all" && g.capability === r.kind && same(g.value, r.value))
      .map((g) =>
        g.subject_type === "user_type"
          ? AVAILABILITY.CHIP_TYPE(typeName(g.subject))
          : g.subject_type === "group"
            ? AVAILABILITY.CHIP_GROUP(g.subject)
            : AVAILABILITY.CHIP_USER(g.subject),
      );
  // Packet A draws the wall note once, under the first family with a block.
  const wallKind = CAPABILITY_KINDS.find((k) => rows.some((r) => r.kind === k && r.state === "blocked"));

  return (
    <div data-testid="explain-grid">
      {CAPABILITY_KINDS.map((kind) => {
        const list = rows.filter((r) => r.kind === kind);
        if (list.length === 0) return null;
        return (
          <div key={kind} className="border-t border-border py-2.5 first:border-t-0 first:pt-0">
            <div className="flex items-center justify-between gap-2">
              <h5 className="text-body font-medium text-foreground">{KIND[kind as CapabilityKind].label}</h5>
              <Button variant="ghost" size="sm" disabled={disabled} onClick={() => setAddKind(kind as CapabilityKind)}>
                {EXPLAIN.ADD}
              </Button>
            </div>
            {list.map((r) => {
              const own = ownGrant(r);
              // G-4: the server's own non-secret label (a git provider's kind
              // and organisation, a model provider's name) wins over a
              // client-side lookup, which wins over the raw value in mono.
              const named = r.label || valueName(r.kind, r.value);
              const only = r.restricted && r.state === "not_available" ? onlyFor(r) : [];
              const phrase = only.length > 0 ? onlyPhrase(only) : null;
              return (
                <div
                  key={r.value}
                  data-testid={`explain-row-${r.kind}-${r.value}`}
                  className={cn(
                    "flex flex-wrap items-center gap-x-2.5 gap-y-1.5 py-1 text-body",
                    r.state !== "this_type" && r.state !== "blocked" && "text-muted-foreground",
                  )}
                >
                  <Chip tone={STATE_TONE[r.state]}>{EXPLAIN.STATE[r.state]}</Chip>
                  <span>
                    {named ?? <Mono>{r.value}</Mono>}
                    {phrase && (
                      <>
                        {" · "}
                        {phrase.elided ? (
                          <>
                            {/* F1: the truncated text is a mouse-only hover
                                (title); it carries no accessible name of its
                                own (aria-hidden), so a keyboard or
                                screen-reader user reaches the real,
                                untruncated sentence below instead — never a
                                truncated one. */}
                            <span className="text-xs text-muted-foreground" title={phrase.full} aria-hidden="true">
                              {phrase.visible}
                            </span>
                            <span className="sr-only">{EXPLAIN.ONLY(phrase.full)}</span>
                          </>
                        ) : (
                          <span className="text-xs text-muted-foreground">{phrase.visible}</span>
                        )}
                      </>
                    )}
                  </span>
                  {own && (
                    <Button
                      variant="ghost"
                      size="sm"
                      disabled={disabled || busy}
                      onClick={() => setToRemove(own)}
                      aria-label={`${EXPLAIN.REMOVE} ${named ?? r.value}`}
                    >
                      {EXPLAIN.REMOVE}
                    </Button>
                  )}
                </div>
              );
            })}
            {kind === wallKind && (
              <Note tone="amber">
                <span>
                  <b>{EXPLAIN.WALL_HEAD}</b> {EXPLAIN.WALL_BODY}
                </span>
              </Note>
            )}
          </div>
        );
      })}
      {/* G-6: one true legend line; packet A's own second footer is true only
          now that G-7 gives the grid an add. */}
      <p className="mt-3 text-body text-muted-foreground">{EXPLAIN.LEGEND}</p>
      <p className="text-body text-muted-foreground">{EXPLAIN.FOOTER_OTHER_SIDE}</p>
      {removeError && (
        <Note tone="red" role="alert">
          {removeError}
        </Note>
      )}
      {submitted && <SubmittedNote />}

      {/* G-7: Permissions' own "Add a grant" dialog, Who and Capability fixed
          to this type and family. */}
      <Dialog open={addKind !== null} onOpenChange={(o) => !o && setAddKind(null)}>
        <DialogContent className="sm:max-w-lg">
          <DialogHeader>
            <DialogTitle>{PERM.ADD_TITLE}</DialogTitle>
          </DialogHeader>
          {addKind && (
            <AddGrantForm
              disabled={disabled}
              hideTitle
              fixed={{ subjectType: "user_type", subject, subjectLabel: name, kind: addKind }}
              onAdded={() => {
                setAddKind(null);
                load();
              }}
            />
          )}
        </DialogContent>
      </Dialog>

      {/* The Permissions screen's own confirmation, for the same grant row. */}
      <AlertDialog open={!!toRemove} onOpenChange={(o) => !o && setToRemove(null)}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>{PERM.REMOVE}</AlertDialogTitle>
            <AlertDialogDescription>{PERM.REMOVE_CONFIRM(name)}</AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>Cancel</AlertDialogCancel>
            <AlertDialogAction
              className="bg-danger text-danger-foreground hover:bg-danger/90"
              disabled={busy}
              onClick={(e) => {
                e.preventDefault();
                if (toRemove) void remove(toRemove);
              }}
            >
              {busy ? <Loader2 className="size-4 animate-spin" /> : null}
              {PERM.REMOVE}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}
