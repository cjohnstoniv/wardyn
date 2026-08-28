/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { Check } from "lucide-react";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "../ui/alert-dialog";
import { Checkbox } from "../ui/checkbox";
import { Mono } from "./code-block";

// Shared untrusted-content confirm for approving egress hosts. Every entry
// point that widens a workspace's allowed egress from a suggested/observed/
// detected host — today the workspace detail screen's session promote AND its
// per-host approve, both writing egress:<host> requirement rows — must ask
// this same question first, since the host name itself came from untrusted
// content (the workspace's own files, or a run's denied egress) and, once
// approved, is reachable by every future run that mounts the workspace.
export function ConfirmEgressDialog({
  hosts,
  selectable,
  onOpenChange,
  onConfirm,
}: {
  // null/empty = closed. One host = singular copy; more = bulk copy + list.
  hosts: string[] | null;
  // BULK ONLY: turn the read-only list into a checkbox list so the operator
  // can approve a SUBSET of what a recording observed instead of the whole
  // thing wholesale. Deliberately not offered on the single-host path — a
  // one-checkbox list next to an "Approve host" button is noise, and that
  // flow's subset is already the host itself.
  selectable?: boolean;
  onOpenChange: (open: boolean) => void;
  // Receives what the operator actually approved: the checked subset when
  // `selectable`, otherwise the full `hosts` list.
  onConfirm: (approved: string[]) => void;
}) {
  const open = !!hosts && hosts.length > 0;
  const bulk = (hosts?.length ?? 0) > 1;
  const single = hosts?.[0];
  const checkboxes = !!selectable && bulk;
  // Reseeded whenever a different host set opens the dialog — the one instance
  // is shared by several call sites (workspace-detail's promote AND approve),
  // so a stale selection from the previous open must never carry over.
  const key = (hosts ?? []).join(",");
  const [selected, setSelected] = React.useState<Set<string>>(() => new Set(hosts ?? []));
  const [seededFor, setSeededFor] = React.useState(key);
  if (seededFor !== key) {
    setSeededFor(key);
    setSelected(new Set(hosts ?? []));
  }
  const approved = checkboxes ? (hosts ?? []).filter((h) => selected.has(h)) : (hosts ?? []);

  return (
    <AlertDialog open={open} onOpenChange={onOpenChange}>
      <AlertDialogContent>
        <AlertDialogHeader>
          <AlertDialogTitle>
            {bulk ? `Approve egress to ${hosts!.length} hosts?` : `Approve egress to ${single}?`}
          </AlertDialogTitle>
          <AlertDialogDescription>
            {bulk ? "These hosts aren't" : "This host isn't"} approved for this workspace yet — they
            came from untrusted content (the workspace&apos;s own files, or a run&apos;s denied egress).
            Approving allows every future run that mounts this workspace to reach {bulk ? "them" : "it"}.
          </AlertDialogDescription>
        </AlertDialogHeader>
        {bulk && (
          <ul className="scroll-thin max-h-40 space-y-1 overflow-y-auto rounded-md border border-border bg-muted/40 px-3 py-2">
            {hosts!.map((h) => (
              <li key={h} className="flex items-center gap-2">
                {checkboxes && (
                  <Checkbox
                    id={`confirm-egress-${h}`}
                    checked={selected.has(h)}
                    onCheckedChange={(v) =>
                      setSelected((prev) => {
                        const next = new Set(prev);
                        if (v === true) next.add(h);
                        else next.delete(h);
                        return next;
                      })
                    }
                    aria-label={`Approve ${h}`}
                  />
                )}
                <label htmlFor={checkboxes ? `confirm-egress-${h}` : undefined} className={checkboxes ? "cursor-pointer" : undefined}>
                  <Mono className="text-foreground">{h}</Mono>
                </label>
              </li>
            ))}
          </ul>
        )}
        <AlertDialogFooter>
          <AlertDialogCancel>Cancel</AlertDialogCancel>
          <AlertDialogAction
            disabled={approved.length === 0}
            onClick={(e) => {
              e.preventDefault();
              onConfirm(approved);
            }}
          >
            <Check className="size-4" />{" "}
            {checkboxes
              ? `Approve ${approved.length} host${approved.length === 1 ? "" : "s"}`
              : bulk
                ? "Approve hosts"
                : "Approve host"}
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  );
}
