/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The delete dialog of a pool (mock M-POOLS v6 dialogs() "delete"). It lists what deleting does as bullets, so
// it is its own AlertDialog in the shape of ../confirm-dialog.tsx (focus opens on Cancel, a refusal stays inline
// with the server's sentence, the action is a destructive button). The server refuses a delete while a use
// policy is set ("clear the use policy first"); that sentence is the server's and shows here when it comes.
import * as React from "react";
import { Loader2 } from "lucide-react";
import { toast } from "sonner";
import { runnerPools } from "../../../../lib/api/runner-pools";
import { getErrorMessage } from "../../../../lib/format";
import type { RunnerPool } from "../../../../lib/types/runner-pools";
import { POOL_DIALOG, POOL_DRAWER } from "../../../wardyn/copy/runner-pools";
import {
  AlertDialog,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "../../../ui/alert-dialog";
import { Button } from "../../../ui/button";

/** What deleting does, in the mock's order; the count line only when the pool's slot counts are known. */
function bullets(mine: boolean, isDefault: boolean, waiting: number | undefined): string[] {
  return [
    waiting === undefined ? POOL_DIALOG.DELETE_WAITING : `${POOL_DIALOG.DELETE_WAITING} ${POOL_DIALOG.DELETE_WAITING_COUNT(waiting)}`,
    POOL_DIALOG.DELETE_RUNNING,
    mine ? POOL_DIALOG.DELETE_RUNNERS_OWN : POOL_DIALOG.DELETE_RUNNERS,
    ...(isDefault ? [mine ? POOL_DIALOG.DELETE_MY_DEFAULT : POOL_DIALOG.DELETE_ORG_DEFAULT] : []),
    ...(mine ? [] : [POOL_DIALOG.DELETE_SAVED_DEFAULT]),
  ];
}

export function DeletePoolDialog({
  pool,
  mine,
  isDefault,
  hasPolicy = false,
  waiting,
  onOpenChange,
  onDeleted,
}: {
  pool: RunnerPool | null;
  /** The caller owns the pool: it goes through their own route and the wording is about their runners. */
  mine: boolean;
  isDefault: boolean;
  /** The pool has a use policy: the server refuses the delete until it is cleared, so the dialog says so first. */
  hasPolicy?: boolean;
  /** Runs waiting for the pool, when its slot counts are known. */
  waiting?: number;
  onOpenChange: (open: boolean) => void;
  onDeleted: () => void;
}) {
  const [busy, setBusy] = React.useState(false);
  const [error, setError] = React.useState("");
  const open = pool !== null;
  React.useEffect(() => {
    if (open) setError("");
  }, [open]);
  if (!pool) return null;

  const run = async () => {
    setBusy(true);
    setError("");
    try {
      await (mine ? runnerPools.removeMine(pool.id) : runnerPools.remove(pool.id));
      onOpenChange(false);
      toast.success(POOL_DIALOG.DELETED(pool.name));
      onDeleted();
    } catch (e) {
      setError(getErrorMessage(e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <AlertDialog open onOpenChange={(o) => !busy && onOpenChange(o)}>
      <AlertDialogContent className="sm:max-w-md">
        <AlertDialogHeader>
          <AlertDialogTitle>{POOL_DIALOG.DELETE_TITLE(pool.name)}</AlertDialogTitle>
          <AlertDialogDescription asChild>
            <div className="space-y-2">
              <p>{POOL_DIALOG.DELETE_UNDONE}</p>
              <ul className="list-disc space-y-1 pl-4 text-body">
                {bullets(mine, isDefault, waiting).map((b) => (
                  <li key={b}>{b}</li>
                ))}
              </ul>
            </div>
          </AlertDialogDescription>
        </AlertDialogHeader>
        {(error || hasPolicy) && (
          <div role="alert" className="rounded-lg border border-danger bg-danger-subtle p-3 text-body text-foreground">
            {error || POOL_DIALOG.DELETE_BLOCKED}
          </div>
        )}
        <AlertDialogFooter>
          <AlertDialogCancel asChild>
            <Button variant="ghost" disabled={busy}>
              {POOL_DRAWER.CANCEL}
            </Button>
          </AlertDialogCancel>
          <Button variant="destructive" disabled={busy || hasPolicy} onClick={() => void run()}>
            {busy && <Loader2 className="size-3.5 animate-spin" />}
            {POOL_DIALOG.DELETE_CONFIRM}
          </Button>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  );
}
