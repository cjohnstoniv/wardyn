/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

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

// Shared kill-run confirm for the two places Kill is offered (the Runs board's
// row menu and Run Detail's header): an enforcement stop deserves the same
// question, in the same words, wherever it is fired from — the two copies had
// already been written out twice and were only kept in sync by hand.
// Controlled-only (like ConfirmEgressDialog) so a caller can render it as a
// SIBLING of a DropdownMenuContent — nested inside, it would unmount with the
// menu the moment the item is clicked.
export function KillRunDialog({
  runId,
  onOpenChange,
  onConfirm,
}: {
  // The run to kill; also doubles as the "is the dialog open" flag (null = closed).
  runId: string | null;
  onOpenChange: (open: boolean) => void;
  onConfirm: () => void;
}) {
  return (
    <AlertDialog open={!!runId} onOpenChange={onOpenChange}>
      <AlertDialogContent>
        <AlertDialogHeader>
          <AlertDialogTitle>Kill {runId}?</AlertDialogTitle>
          <AlertDialogDescription>
            This terminates the agent run, tears down the sandbox, and revokes any brokered
            credentials. Enforcement stop — it is recorded in the audit trail. This cannot be undone.
          </AlertDialogDescription>
        </AlertDialogHeader>
        <AlertDialogFooter>
          <AlertDialogCancel>Cancel</AlertDialogCancel>
          <AlertDialogAction
            onClick={onConfirm}
            className="bg-danger text-danger-foreground hover:bg-danger/90"
          >
            Kill run
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  );
}
