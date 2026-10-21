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
import { RUN_REF } from "./copy/run-ref";
import { RunRef, shortRunId } from "./run-ref";

// Shared kill-run confirm for the two places Kill is offered (the Runs board's
// row menu and Run Detail's header): an enforcement stop deserves the same
// question, in the same words, wherever it is fired from — the two copies had
// already been written out twice and were only kept in sync by hand.
// Controlled-only (like ConfirmEgressDialog) so a caller can render it as a
// SIBLING of a DropdownMenuContent — nested inside, it would unmount with the
// menu the moment the item is clicked.
export function KillRunDialog({
  runId,
  title,
  onOpenChange,
  onConfirm,
}: {
  // The run to kill; also doubles as the "is the dialog open" flag (null = closed).
  runId: string | null;
  // What the page calls the run (its title, else its task). Named in the question with the id beneath it;
  // absent, the id stands in so the question still names the run.
  title?: string;
  onOpenChange: (open: boolean) => void;
  // Called with the id the dialog is showing, so the kill can only ever hit
  // the run the title names.
  onConfirm: (runId: string) => void;
}) {
  // First line, bounded: a run named by its task can be a paragraph.
  const line = (title ?? "").trim().split("\n")[0];
  const name = line.length > 80 ? `${line.slice(0, 79)}…` : line;
  return (
    <AlertDialog open={!!runId} onOpenChange={onOpenChange}>
      <AlertDialogContent>
        <AlertDialogHeader>
          <AlertDialogTitle className="break-words">{name ? `Kill "${name}"?` : RUN_REF.killRun(shortRunId(runId ?? ""))}</AlertDialogTitle>
          <AlertDialogDescription>
            {name && runId && (
              <>
                <RunRef runId={runId} />.{" "}
              </>
            )}
            This terminates the agent run, tears down the sandbox, and revokes any brokered
            credentials. The kill is recorded in the audit trail. This cannot be undone.
          </AlertDialogDescription>
        </AlertDialogHeader>
        <AlertDialogFooter>
          <AlertDialogCancel>Cancel</AlertDialogCancel>
          <AlertDialogAction
            onClick={() => runId && onConfirm(runId)}
            className="bg-danger text-danger-foreground hover:bg-danger/90"
          >
            Kill run
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  );
}
