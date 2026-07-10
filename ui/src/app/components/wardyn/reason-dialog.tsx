/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { Check, Loader2, X } from "lucide-react";
import type { ApprovalRequest } from "../../lib/types";
import { Button } from "../ui/button";
import { Label } from "../ui/label";
import { Textarea } from "../ui/textarea";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "../ui/dialog";

// Shared approve/deny reason dialog (deny requires a reason for the audit
// trail). Kind-aware: approving authorizes the broker to MINT a short-lived
// scoped token — that copy only applies to a "credential" prompt, since an
// egress_domain/tool_call approve doesn't mint anything.
export function ReasonDialog({
  prompt,
  onClose,
  onSubmit,
}: {
  prompt: { id: string; action: "approve" | "deny"; kind: ApprovalRequest["kind"] } | null;
  onClose: () => void;
  // Resolves true when the decision committed (dialog closes itself via the
  // parent's onClose); false when it failed so we re-enable the button.
  onSubmit: (reason: string) => Promise<boolean>;
}) {
  const [reason, setReason] = React.useState("");
  const [busy, setBusy] = React.useState(false);
  React.useEffect(() => {
    setReason("");
    setBusy(false);
  }, [prompt]);

  const approve = prompt?.action === "approve";
  // Always reset busy in finally so a failed approve/deny doesn't leave the
  // button spinning forever — the user can retry or cancel after the toast
  // reports the error.
  const submit = async () => {
    setBusy(true);
    try {
      await onSubmit(reason.trim());
    } finally {
      setBusy(false);
    }
  };

  return (
    <Dialog open={!!prompt} onOpenChange={(o) => !o && onClose()}>
      <DialogContent className="sm:max-w-md">
        <DialogHeader>
          <DialogTitle>{approve ? "Approve request" : "Deny request"}</DialogTitle>
          <DialogDescription>
            {/* Approving authorizes the mint — the broker mints the short-lived
                scoped token later, in a separate transaction, and only for a
                credential prompt. Don't imply a token is minted at approval
                time, and don't claim a mint for egress_domain/tool_call. */}
            {approve && prompt?.kind === "credential"
              ? "This authorizes the broker to mint a short-lived scoped token bound to the run identity. Add a reason for the audit trail."
              : "Record a reason for the audit trail. This decision is immutable."}
          </DialogDescription>
        </DialogHeader>
        <div className="space-y-2 py-1">
          <Label htmlFor="reason">Reason {approve && <span className="text-muted-foreground">(optional)</span>}</Label>
          <Textarea
            id="reason"
            value={reason}
            onChange={(e) => setReason(e.target.value)}
            rows={3}
            placeholder={approve ? "Verified scope is minimal and time-boxed…" : "Domain not on allowlist…"}
          />
        </div>
        <DialogFooter>
          <Button variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button
            onClick={submit}
            disabled={busy || (!approve && !reason.trim())}
            variant={approve ? "default" : "destructive"}
          >
            {busy ? <Loader2 className="size-4 animate-spin" /> : approve ? <Check className="size-4" /> : <X className="size-4" />}
            {/* Approval does NOT mint — it AUTHORIZES the broker to mint later
                in a separate transaction. Don't over-claim "& mint". */}
            {approve ? "Approve" : "Confirm deny"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
