/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { Loader2, Trash2 } from "lucide-react";
import { toast } from "sonner";
import { getErrorMessage } from "../../lib/format";
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
import { useOperator } from "./operator-context";
import { OPERATOR_ONLY_REASON } from "./copy";

// Shared destructive delete-confirm dialog for four callers: Secrets,
// Policies, Workspaces and the workspace detail page. It owns the busy
// spinner, the toast.success / toast.error(getErrorMessage) pair and the
// one-delete-per-confirm guard. Other screens keep bespoke dialogs.
export function DeleteConfirmDialog({
  name,
  entity,
  description,
  allowed,
  onOpenChange,
  onDelete,
  onDeleted,
}: {
  // The entity's display name; also doubles as the "is the dialog open" flag
  // (null/empty = closed).
  name: string | null;
  // Lowercase noun used in the title/button ("workspace" / "policy" / "secret").
  entity: string;
  description: React.ReactNode;
  // F5-F1/X3-F2: whether THIS caller may confirm THIS delete. Defaults to
  // `operator` (today's behaviour, unchanged) so every existing caller
  // (secret/policy/SCM-host/credential — all operator-only) needs no change.
  // A caller with an ownership concept (workspaces) passes
  // `useCanMutate(ws.owned_by)` instead, so a member can delete a row they
  // own without this dialog knowing anything about workspaces.
  allowed?: boolean;
  onOpenChange: (open: boolean) => void;
  onDelete: () => Promise<void>;
  // Called after a successful delete (e.g. clear selection + reload the list).
  onDeleted: () => void;
}) {
  const [deleting, setDeleting] = React.useState(false);
  // A ref, not the state above: two clicks in one tick both see deleting ===
  // false until React re-renders, so only a synchronous flag stops the second.
  const inFlight = React.useRef(false);
  // The four callers' deletes route through this dialog, so gating it here is
  // the chokepoint for them; a screen with a bespoke dialog gates its own.
  const operator = useOperator();
  const canConfirm = allowed ?? operator;

  const confirmDelete = async () => {
    if (inFlight.current) return;
    inFlight.current = true;
    setDeleting(true);
    try {
      await onDelete();
      toast.success(`${capitalize(entity)} “${name}” deleted`);
      onDeleted();
    } catch (e) {
      toast.error(`Failed to delete ${entity} “${name}”`, { description: getErrorMessage(e) });
    } finally {
      inFlight.current = false;
      setDeleting(false);
    }
  };

  return (
    <AlertDialog open={!!name} onOpenChange={(o) => !o && !inFlight.current && onOpenChange(false)}>
      <AlertDialogContent>
        <AlertDialogHeader>
          <AlertDialogTitle>
            Delete {entity} “{name}”?
          </AlertDialogTitle>
          <AlertDialogDescription>{description}</AlertDialogDescription>
          {/* Visible, not hover-only — a viewer sees why before they even reach
              the (disabled) confirm button, not after a failed click. */}
          {!canConfirm && (
            <p id="delete-confirm-operator-reason" className="text-xs font-medium text-warning">
              {OPERATOR_ONLY_REASON}
            </p>
          )}
        </AlertDialogHeader>
        <AlertDialogFooter>
          <AlertDialogCancel disabled={deleting}>Cancel</AlertDialogCancel>
          <AlertDialogAction
            onClick={(e) => {
              e.preventDefault();
              void confirmDelete();
            }}
            disabled={!canConfirm || deleting}
            aria-describedby={canConfirm ? undefined : "delete-confirm-operator-reason"}
            className="bg-danger text-danger-foreground hover:bg-danger/90"
          >
            {deleting ? <Loader2 className="size-4 animate-spin" /> : <Trash2 className="size-4" />}
            Delete {entity}
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  );
}

function capitalize(s: string): string {
  return s.charAt(0).toUpperCase() + s.slice(1);
}
