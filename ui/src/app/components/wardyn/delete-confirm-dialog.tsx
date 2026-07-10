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

// Shared destructive delete-confirm dialog for the list screens (workspaces,
// policies, secrets): owns the busy spinner and the toast.success /
// toast.error(getErrorMessage) pair so all three report failures the same
// way instead of re-rolling the same try/toast/finally block.
export function DeleteConfirmDialog({
  name,
  entity,
  description,
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
  onOpenChange: (open: boolean) => void;
  onDelete: () => Promise<void>;
  // Called after a successful delete (e.g. clear selection + reload the list).
  onDeleted: () => void;
}) {
  const [deleting, setDeleting] = React.useState(false);

  const confirmDelete = async () => {
    setDeleting(true);
    try {
      await onDelete();
      toast.success(`${capitalize(entity)} “${name}” deleted`);
      onDeleted();
    } catch (e) {
      toast.error(`Failed to delete ${entity} “${name}”`, { description: getErrorMessage(e) });
    } finally {
      setDeleting(false);
    }
  };

  return (
    <AlertDialog open={!!name} onOpenChange={(o) => !o && onOpenChange(false)}>
      <AlertDialogContent>
        <AlertDialogHeader>
          <AlertDialogTitle>
            Delete {entity} “{name}”?
          </AlertDialogTitle>
          <AlertDialogDescription>{description}</AlertDialogDescription>
        </AlertDialogHeader>
        <AlertDialogFooter>
          <AlertDialogCancel>Cancel</AlertDialogCancel>
          <AlertDialogAction
            onClick={(e) => {
              e.preventDefault();
              confirmDelete();
            }}
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
