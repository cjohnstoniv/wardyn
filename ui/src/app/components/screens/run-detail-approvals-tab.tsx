/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Approvals tab (this run's approvals) — split out of run-detail.tsx (#181
// review finding 3) to keep that file under the 1000-line file-size gate once
// this tab grew a third kind-specific card (PushContentCard, alongside
// AdoCapabilityCard). Same split shape run-detail-summary-header.tsx /
// run-detail-command-bar.tsx already use.
import * as React from "react";
import { Check, ShieldCheck } from "lucide-react";
import {
  canDecideApproval,
  isAdoCapabilityRequest,
  isAdoConsentRequest,
  isPushContentRequest,
  type ApprovalRequest,
  type DecisionOptions,
  type RunDetail,
} from "../../lib/types";
import { relativeTime } from "../../lib/format";
import { Button } from "../ui/button";
import { Label } from "../ui/label";
import { ApprovalKindChip, ApprovalStateBadge, Chip } from "../wardyn/primitives";
import { JsonBlock } from "../wardyn/code-block";
import { EmptyState } from "../wardyn/states";
import { AdoCapabilityCard } from "../wardyn/ado-capability-card";
import { PushContentCard } from "../wardyn/push-content-card";
import { PUSH } from "../wardyn/copy/push";
import { usePrincipal, useSecurityOperator } from "../wardyn/operator-context";
import { SECURITY_ONLY_REASON, approvalScopeBadge } from "../wardyn/copy";

export function ApprovalsTab({
  approvals,
  run,
  onDecide,
  onAdoDecide,
  onPushDecide,
}: {
  approvals: ApprovalRequest[];
  // Round 2 (F2) — this page always has the run loaded by the time this tab
  // can render (see run-detail.tsx's own `status`/Tabs gate), so unlike
  // screens/approvals.tsx's per-row RunContextRow fetch, there is no
  // loading/error tri-state to thread here: it's always the real thing.
  run: RunDetail;
  onDecide: (id: string, action: "approve" | "deny", kind: ApprovalRequest["kind"]) => void;
  onAdoDecide: (id: string, approve: boolean, opts: [DecisionOptions]) => Promise<void>;
  onPushDecide: (id: string, approve: boolean) => Promise<void>;
}) {
  // useSecurityOperator (0.7 §B): the only thing this reads is
  // canDecideApproval, which mirrors authorizeMemberDecision's early return
  // for the security tier (approvals.go:392).
  const securityOperator = useSecurityOperator();
  const principal = usePrincipal();
  // The id of the row currently deciding, and which of its two actions
  // (#458) — see AdoCapabilityCard's own `busy` doc for why a single
  // boolean isn't enough.
  const [adoBusy, setAdoBusy] = React.useState<{ id: string; action: "approve" | "deny" } | null>(null);
  const [pushBusyId, setPushBusyId] = React.useState<string | null>(null);
  if (approvals.length === 0) {
    return (
      <div className="rounded-xl border border-border bg-card">
        <EmptyState
          icon={ShieldCheck}
          title="No approvals for this run"
          description="Credential, egress, and tool-call requests for this run will appear here."
        />
      </div>
    );
  }
  return (
    <div className="flex max-w-3xl flex-col gap-3">
      {approvals.map((a) => {
        // S10 round 2 (F2) — an Azure DevOps escalation (or its Entra-consent
        // chain) gets AdoCapabilityCard, not this tab's generic row: it needs
        // an explicit decision_scope (never the bodyless decide onDecide's
        // ReasonDialog path produces) and the ownership-aware decidability
        // rule canDecideApproval doesn't model.
        //
        // N1 (round 2) — gated on PENDING: `approvals` here is EVERY state
        // this run's approvals ever reached (unlike approvals.tsx's
        // pendingItems / live-approvals.tsx's pending, both already PENDING-
        // only), so a DECIDED Azure DevOps row reaches this map too. Without
        // the state check it rendered live Approve/Deny buttons — and, on an
        // ended run, a false "nothing to allow" — over a row nobody can act
        // on any more. A decided row falls through to the generic branch
        // below, whose `scopeBadge` (approvalScopeBadge, extended F11) reads
        // "Allowed once"/"Allowed for this run" for it.
        if ((isAdoCapabilityRequest(a) || isAdoConsentRequest(a)) && a.state === "PENDING") {
          return (
            <AdoCapabilityCard
              key={a.id}
              item={a}
              securityOperator={securityOperator}
              viewerPrincipal={principal}
              run={run}
              busy={adoBusy?.id === a.id ? adoBusy.action : null}
              onApprove={async (opts) => {
                setAdoBusy({ id: a.id, action: "approve" });
                await onAdoDecide(a.id, true, opts);
                setAdoBusy(null);
              }}
              onDeny={async (opts) => {
                setAdoBusy({ id: a.id, action: "deny" });
                await onAdoDecide(a.id, false, opts);
                setAdoBusy(null);
              }}
            />
          );
        }
        // #181 (review finding 3) — a held push gets its own card too, for
        // the SAME reason ADO's does two branches up: the generic row's
        // JsonBlock below would render the raw requested_scope, exposing
        // acts_as (a credential reference) and, for an Azure DevOps REST
        // push, the request-body digest under the wire key "commits".
        if (isPushContentRequest(a) && a.state === "PENDING") {
          return (
            <PushContentCard
              key={a.id}
              item={a}
              securityOperator={securityOperator}
              run={run}
              busy={pushBusyId === a.id}
              onApprove={async () => {
                setPushBusyId(a.id);
                await onPushDecide(a.id, true);
                setPushBusyId(null);
              }}
              onDeny={async () => {
                setPushBusyId(a.id);
                await onPushDecide(a.id, false);
                setPushBusyId(null);
              }}
            />
          );
        }
        const pending = a.state === "PENDING";
        // Owner-scoped page (getRunAuthorized) — canDecideApproval only needs
        // the KIND question: egress_domain is a member act on an owned run,
        // credential/tool_call stay admin-OR-security-admin-only (see its doc).
        const canDecide = canDecideApproval(securityOperator, a.kind);
        const scopeBadge = approvalScopeBadge(a);
        return (
          <div key={a.id} className="rounded-xl border border-border bg-card p-4">
            <div className="flex flex-wrap items-center gap-2">
              <ApprovalKindChip kind={a.kind} />
              <ApprovalStateBadge state={a.state} />
              <span className="ml-auto text-xs text-muted-foreground" title={a.requested_at}>
                requested {relativeTime(a.requested_at)}
              </span>
            </div>
            {/* #181 (review finding 3) — a DECIDED push_content row (the only
                state shape left once the PENDING branch above has already
                returned) never shows the raw scope either: same exposure,
                one decision later. A short, field-by-field summary instead —
                acts_as_label only, never the raw acts_as or the wire's
                "commits" key, which is a request-body digest on an Azure
                DevOps REST push, not an object id. */}
            {isPushContentRequest(a) ? (
              <dl className="mt-3 grid grid-cols-[max-content_1fr] gap-x-3 gap-y-1 text-sm">
                <dt className="text-muted-foreground">{PUSH.FIELD_REPOSITORY}</dt>
                <dd className="font-mono text-xs text-foreground">{a.requested_scope.repo}</dd>
                <dt className="text-muted-foreground">{PUSH.FIELD_BRANCH}</dt>
                <dd className="font-mono text-xs text-foreground">{a.requested_scope.branch}</dd>
                <dt className="text-muted-foreground">{PUSH.FIELD_ACTS_AS}</dt>
                <dd className="text-xs text-foreground">{a.requested_scope.acts_as_label || "—"}</dd>
              </dl>
            ) : (
              <div className="mt-3">
                <Label className="text-meta uppercase tracking-wide text-muted-foreground">
                  Requested scope
                </Label>
                <JsonBlock value={a.requested_scope} className="mt-1.5" />
              </div>
            )}
            {a.decided_by && (
              <div className="mt-2 text-xs text-muted-foreground">
                Decided by <span className="text-foreground">{a.decided_by}</span>
                {/* Scope badge — the console's DecidedRow shows the same
                    fact; without it here the cockpit would show a decided
                    egress row and the console would show it grew a scope,
                    for the SAME approval. */}
                {scopeBadge && <> · {scopeBadge}</>}
                {a.reason && <> · {a.reason}</>}
              </div>
            )}
            {pending && (
              <div className="mt-3 flex items-center justify-end gap-2 border-t border-border pt-3">
                {/* Gated on the SECURITY tier (canDecideApproval reads
                    securityOperator, admin-OR-security-admin), not plain
                    isOperator — SECURITY_ONLY_REASON says so; OPERATOR_ONLY_
                    REASON here would undersell who this control actually
                    admits. */}
                {!canDecide && <Chip tone="neutral">{SECURITY_ONLY_REASON}</Chip>}
                <Button
                  variant="outline"
                  size="sm"
                  onClick={() => onDecide(a.id, "deny", a.kind)}
                  disabled={!canDecide}
                >
                  Deny
                </Button>
                <Button
                  size="sm"
                  variant="info"
                  onClick={() => onDecide(a.id, "approve", a.kind)}
                  disabled={!canDecide}
                >
                  <Check className="size-4" /> Approve
                </Button>
              </div>
            )}
          </div>
        );
      })}
    </div>
  );
}
