/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { Loader2 } from "lucide-react";
import { toast } from "sonner";
import { HttpError } from "../../lib/api/core";
import { getErrorMessage } from "../../lib/format";
import { Button } from "../ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "../ui/dialog";

// Shared delete-in-use dialog for the Tier-1/Tier-2 library screens (sources
// library, base-image catalog): busy/inUseDetail state, a 409 -> named-users
// detail, and the "Detach everywhere & delete" escape — the same flow, only
// the API call and two nouns ever differed between the two call sites.
export function DeleteInUseDialog<T extends { id: string; name: string }>({
  target,
  onOpenChange,
  onDeleted,
  description,
  inUseHint,
  onDelete,
  removedFrom,
  errorNoun,
}: {
  target: T | null;
  onOpenChange: (o: boolean) => void;
  onDeleted: () => void;
  description: React.ReactNode;
  // The extra sentence shown under the server's 409 detail once it's in use —
  // what the force-delete's detach actually does for THIS entity.
  inUseHint: React.ReactNode;
  onDelete: (target: T, force: boolean) => Promise<void>;
  // "the library" / "the catalog" — completes the success toast.
  removedFrom: string;
  // "source" / "image" — completes the error toast.
  errorNoun: string;
}) {
  const [busy, setBusy] = React.useState(false);
  const [inUseDetail, setInUseDetail] = React.useState<string | null>(null);

  React.useEffect(() => {
    setBusy(false);
    setInUseDetail(null);
  }, [target]);

  const attempt = async (force: boolean) => {
    if (!target) return;
    setBusy(true);
    try {
      await onDelete(target, force);
      toast.success(`"${target.name}" removed from ${removedFrom}`);
      onDeleted();
      onOpenChange(false);
    } catch (e) {
      // A forced (detach-everywhere) attempt can 409 too — deterministically,
      // when detaching would leave some workspace with zero attachments
      // (STORE-1) — so this can't be gated on `!force`: without that, the
      // second failure fell into the generic toast while the dialog kept
      // showing the FIRST attempt's (now stale) detail and its "Detach
      // everywhere & delete" button, silently re-offering an action that
      // deterministically fails again.
      if (e instanceof HttpError && e.status === 409) {
        setInUseDetail(getErrorMessage(e));
      } else {
        toast.error(`Failed to delete ${errorNoun}`, { description: getErrorMessage(e) });
      }
    } finally {
      setBusy(false);
    }
  };

  return (
    <Dialog open={!!target} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>Delete “{target?.name}”?</DialogTitle>
          <DialogDescription>{description}</DialogDescription>
        </DialogHeader>
        {inUseDetail && (
          <div className="space-y-1.5 rounded-lg border border-warning/30 bg-warning-subtle p-2.5">
            <p className="text-xs leading-snug text-warning">{inUseDetail}</p>
            <p className="text-[0.6875rem] leading-snug text-warning/90">{inUseHint}</p>
          </div>
        )}
        <DialogFooter>
          <Button variant="ghost" onClick={() => onOpenChange(false)}>
            Cancel
          </Button>
          {inUseDetail ? (
            <Button variant="destructive" onClick={() => void attempt(true)} disabled={busy}>
              {busy && <Loader2 className="size-4 animate-spin" />}
              Detach everywhere & delete
            </Button>
          ) : (
            <Button variant="destructive" onClick={() => void attempt(false)} disabled={busy}>
              {busy && <Loader2 className="size-4 animate-spin" />}
              Delete
            </Button>
          )}
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
