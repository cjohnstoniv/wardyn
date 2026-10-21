/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Approvals tab (this run's approvals) — split out of run-detail.tsx to keep
// that file under the 1000-line file-size gate. Requests waiting on a person
// render as the shared ApprovalCard (the Approvals screen's own card); decided
// ones are one-line History rows.
import { ShieldCheck } from "lucide-react";
import {
  isAdoCapabilityRequest,
  isAdoConsentRequest,
  isTerminalRunState,
  type ApprovalRequest,
  type DecisionOptions,
  type RunDetail,
} from "../../lib/types";
import { SectionLabel } from "../wardyn/primitives";
import { EmptyState } from "../wardyn/states";
import { useConsoleMode } from "../wardyn/console-view";
import { ReauthRow } from "../wardyn/live-approvals-rows";
import { APPROVALS_TAB } from "../wardyn/copy/approvals";
import { RUNS_ROW_WORD } from "../wardyn/copy/runs-landing";
import { useRunAdoTokens } from "../../lib/hooks/use-run-ado-tokens";
import { ApprovalCard, DecidedRow } from "./approvals-cards";

export function ApprovalsTab({
  approvals,
  run,
  onDecide,
  onAdoDecide,
  onPushDecide,
  viewerBlocked = false,
}: {
  approvals: ApprovalRequest[];
  // This page always has the run loaded by the time this tab can render
  // (see run-detail.tsx's own `status`/Tabs gate), so the cards take it as-is.
  run: RunDetail;
  onDecide: (id: string, action: "approve" | "deny", kind: ApprovalRequest["kind"]) => void;
  onAdoDecide: (id: string, approve: boolean, opts: [DecisionOptions]) => Promise<void>;
  onPushDecide: (id: string, approve: boolean) => Promise<void>;
  /** The pending decisions are ones this viewer cannot make (viewerBlockedOnDecision). */
  viewerBlocked?: boolean;
}) {
  const adminView = useConsoleMode() === "admin";
  // A run that holds Azure DevOps tokens widens one when access is approved.
  const adoTokens = useRunAdoTokens(run.id, !isTerminalRunState(run.state), approvals.some(isAdoCapabilityRequest));
  if (approvals.length === 0) {
    return (
      <div data-testid="run-approvals-empty" className="max-w-3xl rounded-xl border border-border bg-card">
        <EmptyState
          icon={ShieldCheck}
          title="No approvals for this run"
          description="Credential, egress, and tool-call requests for this run will appear here."
        />
      </div>
    );
  }
  const waiting = approvals.filter((a) => a.state === "PENDING");
  // The Runs list's wording, when the pending decisions are an admin's to make.
  const waitingLabel = viewerBlocked ? RUNS_ROW_WORD.WAITING_FOR_ADMIN : APPROVALS_TAB.WAITING;
  const history = approvals.filter((a) => a.state !== "PENDING");
  return (
    <div className="flex max-w-3xl flex-col gap-5">
      {waiting.length > 0 && (
        <section aria-label={waitingLabel} className="space-y-3">
          <SectionLabel>{waitingLabel}</SectionLabel>
          {waiting.map((a) =>
            a.kind === "credential_reauth" && !isAdoConsentRequest(a) ? (
              // A door, not a decision: no tier can approve or deny this kind (the server answers 409), so the pair
              // is absent, not disabled. The same row the live strip and /approvals offer, so the three cannot drift.
              <div
                key={a.id}
                className="rounded-xl border border-border bg-card p-4"
                data-testid="approvals-tab-reauth"
              >
                <ReauthRow request={a} runId={run.id} adminView={adminView} />
              </div>
            ) : (
              <ApprovalCard
                key={a.id}
                item={a}
                caps={null}
                run={run}
                tokenWidens={adoTokens.length > 0}
                onAct={(action) => onDecide(a.id, action, a.kind)}
                onAdoDecide={onAdoDecide}
                onPushDecide={onPushDecide}
              />
            ),
          )}
        </section>
      )}
      {history.length > 0 && (
        <section aria-label={APPROVALS_TAB.HISTORY} className="space-y-3">
          <SectionLabel>{APPROVALS_TAB.HISTORY}</SectionLabel>
          <div className="overflow-hidden rounded-xl border border-border bg-card">
            {history.map((a) => (
              <DecidedRow key={a.id} item={a} showRun={false} />
            ))}
          </div>
        </section>
      )}
    </div>
  );
}
