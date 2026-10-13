/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Azure DevOps launch door: opened by a git_credential refusal at Launch,
// closable without launching. The popup and its poll belong to useAdoLaunchDoor;
// this only asks.

import { Loader2 } from "lucide-react";
import { ADO } from "../../../../lib/ado-entra-copy";
import { PEOPLE } from "../../../../lib/people-access-copy";
import { Button, buttonVariants } from "../../../ui/button";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "../../../ui/dialog";
import type { useAdoLaunchDoor } from "../checks/use-ado-launch-door";

export function AdoLaunchDialog({ dialog }: { dialog: ReturnType<typeof useAdoLaunchDoor>["dialog"] }) {
  return (
    <Dialog open={dialog.open} onOpenChange={(open) => !open && dialog.onCancel()}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>{ADO.LAUNCH_DIALOG_TITLE}</DialogTitle>
          <DialogDescription>{ADO.LAUNCH_DIALOG_BODY(dialog.org)}</DialogDescription>
        </DialogHeader>
        {dialog.blockedUrl && (
          <p className="text-xs text-muted-foreground">
            {ADO.CONNECT_POPUP_BLOCKED}{" "}
            <a
              href={dialog.blockedUrl}
              target="_blank"
              rel="noopener noreferrer"
              className={buttonVariants({ variant: "outline", size: "sm" })}
              onClick={dialog.onFallbackClick}
            >
              {ADO.CONNECT_POPUP_OPEN}
            </a>
          </p>
        )}
        <DialogFooter>
          <Button type="button" variant="outline" onClick={dialog.onCancel}>
            {PEOPLE.CANCEL}
          </Button>
          <Button type="button" onClick={dialog.onConfirm} disabled={dialog.connecting}>
            <Loader2 className={dialog.connecting ? "size-4 animate-spin" : "size-4 animate-spin invisible"} />
            {ADO.CONNECT_CTA}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
