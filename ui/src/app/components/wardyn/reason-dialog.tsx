/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { Check, Loader2, X } from "lucide-react";
import type { ApprovalRequest, ApprovalScope } from "../../lib/types";
import { Button } from "../ui/button";
import { Input } from "../ui/input";
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
import { cn } from "../ui/utils";
import { useDeferredBusy } from "../../lib/use-deferred-busy";
import { useSecurityOperator } from "./operator-context";
import {
  ALWAYS_NEEDS_WORKSPACE,
  APPROVAL_SCOPE_HINT,
  APPROVAL_SCOPE_LABEL,
  APPROVAL_SCOPE_ORDER,
  DENY_SCOPE_HINT,
  DENY_SCOPE_LABEL,
  OPERATOR_ONLY_REASON,
  UNTIL_PRESETS,
} from "./copy";

// Shared approve/deny reason dialog (deny requires a reason for the audit
// trail). Kind-aware: approving authorizes the broker to MINT a short-lived
// scoped token — that copy only applies to a "credential" prompt, since an
// egress_domain/tool_call approve doesn't mint anything.
//
// egress_domain ALSO gets a decision-scope segmented control above the reason
// field (once / this run / until / always — see copy.ts's canon strings).
// Credential/tool_call approvals stay binary: the server 400s a scope on any
// other kind, so the control simply doesn't render for them and this dialog's
// scope stays at its "run" default, invisibly — see onSubmit below.
export function ReasonDialog({
  prompt,
  hasWorkspace = false,
  onClose,
  onSubmit,
}: {
  prompt: { id: string; action: "approve" | "deny"; kind: ApprovalRequest["kind"] } | null;
  // Whether THIS run resolves to an onboarded workspace — Always writes there,
  // so it's greyed out without one. Default false: a caller that forgets to
  // pass it shows Always disabled rather than offering a click the server
  // will 400. The standalone /approvals console page doesn't have the run in
  // hand and deliberately opts back IN (passes true) rather than inheriting
  // this default — see approvals.tsx's own comment at its mount.
  hasWorkspace?: boolean;
  onClose: () => void;
  // Resolves true when the decision committed (dialog closes itself via the
  // parent's onClose); false when it failed so we re-enable the button. scope
  // is always "run" for a non-egress_domain prompt (the control never
  // rendered, never changed) — callers use decisionArgs(scope, until) from
  // lib/types/approvals so a "run" decision keeps posting the old two-field
  // body instead of a needless-but-harmless third argument.
  onSubmit: (reason: string, scope: ApprovalScope, until?: string) => Promise<boolean>;
}) {
  // useSecurityOperator, not useOperator (0.7 §B): decision_scope=always is
  // gated by isSecurityOperator (approvals.go:604), in LOCKSTEP with
  // authorizeMemberDecision — same power, same tier. This dialog's ONLY
  // role-aware control is that scope.
  const securityOperator = useSecurityOperator();
  const [reason, setReason] = React.useState("");
  const [busy, setBusy] = React.useState(false);
  const { disabled: submitDisabled, showSpinner } = useDeferredBusy(busy);
  const [scope, setScope] = React.useState<ApprovalScope>("run");
  // ISO string once chosen; null until the operator picks a preset or a time.
  const [until, setUntil] = React.useState<string | null>(null);
  React.useEffect(() => {
    setReason("");
    setBusy(false);
    setScope("run");
    setUntil(null);
  }, [prompt]);

  const approve = prompt?.action === "approve";
  const showScope = prompt?.kind === "egress_domain";
  // Rule 6 (server-side, internal/api/approvals.go): always is
  // security-operator-only, on top of the workspace gate above. Both surfaces
  // that mount this dialog (approvals.tsx, run-detail.tsx) already know the
  // caller's tier via the same useSecurityOperator() hook — reading it here
  // once, rather than threading it through as a prop, keeps both callers'
  // code unchanged.
  const alwaysDisabled = !hasWorkspace || !securityOperator;
  const alwaysReason = !securityOperator ? OPERATOR_ONLY_REASON : ALWAYS_NEEDS_WORKSPACE;
  // Rule 2 (server-side): until demands an expiry. Mirrored here so the
  // confirm button can't submit a scope the server will 400.
  const untilMissing = scope === "until" && !until;

  // Always reset busy in finally so a failed approve/deny doesn't leave the
  // button spinning forever — the user can retry or cancel after the toast
  // reports the error.
  const submit = async () => {
    setBusy(true);
    try {
      await onSubmit(reason.trim(), scope, scope === "until" ? (until ?? undefined) : undefined);
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

        {showScope && (
          <div className="space-y-2 py-1" role="radiogroup" aria-label="Decision scope">
            {APPROVAL_SCOPE_ORDER.map((s) => {
              const disabled = s === "always" && alwaysDisabled;
              const label = approve ? APPROVAL_SCOPE_LABEL[s] : DENY_SCOPE_LABEL[s];
              const hint = disabled ? alwaysReason : approve ? APPROVAL_SCOPE_HINT[s] : DENY_SCOPE_HINT[s];
              return (
                <button
                  key={s}
                  type="button"
                  role="radio"
                  aria-checked={scope === s}
                  disabled={disabled}
                  onClick={() => setScope(s)}
                  className={cn(
                    "flex w-full flex-col items-start gap-0.5 rounded-lg border px-3 py-2 text-left text-sm transition-colors disabled:cursor-not-allowed disabled:opacity-50",
                    scope === s ? "border-primary bg-primary/5" : "border-border hover:border-border-strong",
                  )}
                >
                  <span className="font-medium text-foreground">{label}</span>
                  <span className="text-meta text-muted-foreground">{hint}</span>
                </button>
              );
            })}
            {scope === "until" && (
              <div className="flex flex-wrap items-center gap-1.5 pl-1 pt-0.5">
                {UNTIL_PRESETS.map((p) => (
                  <button
                    key={p.label}
                    type="button"
                    onClick={() => setUntil(new Date(Date.now() + p.ms).toISOString())}
                    className="rounded-full border border-border px-2.5 py-1 text-xs text-foreground hover:border-border-strong"
                  >
                    {p.label}
                  </button>
                ))}
                <Input
                  type="datetime-local"
                  aria-label="Pick a time"
                  className="h-7 w-auto flex-1 min-w-[10rem] text-xs"
                  onChange={(e) => setUntil(e.target.value ? new Date(e.target.value).toISOString() : null)}
                />
              </div>
            )}
          </div>
        )}

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
            disabled={submitDisabled || (!approve && !reason.trim()) || untilMissing}
            variant={approve ? "default" : "destructive"}
          >
            {showSpinner ? <Loader2 className="size-4 animate-spin" /> : approve ? <Check className="size-4" /> : <X className="size-4" />}
            {/* Approval does NOT mint — it AUTHORIZES the broker to mint later
                in a separate transaction. Don't over-claim "& mint". Never
                fold the scope into this label either — it stays exactly
                "Approve"/"Confirm deny" regardless of which scope is picked
                above (ui/e2e/approvals.spec.ts pins "Approve" exact:true). */}
            {approve ? "Approve" : "Confirm deny"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
