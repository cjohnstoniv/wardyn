/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// AI Run Composer — the "Proposed Setup" review screen: the single most
// consequential decision surface in the console, so the honesty rules are strict.
// It is a pure projection of the CLAMPED inline_policy + the DETERMINISTIC
// risk_assessment (never the model's self-assessment):
//   • a header of neutral identity FACTS (agent, barrier, mode, repo, lifecycle),
//     each shown ONCE (chip discipline C2) — the barrier via ConfinementChip so the
//     wire code CCx / invariants never leak to the user (D4);
//   • the CAN / CAN'T split (<ComposeQuickReview>) — capabilities amber, guarantees
//     teal (D2);
//   • the honest clamp notices (warnings) and a collapsible "view the exact policy"
//     raw JSON (C7) — humane content is primary, JSON is one click away;
//   • the deterministic risk grade with RISK_ATTRIBUTION (D8) — ONLY a HIGH grade
//     gates launch behind an explicit acknowledgment;
//   • the real launch / adjust / cancel actions, wired to the actual proposal.
import * as React from "react";
import {
  ChevronDown,
  Code2,
  GitBranch,
  KeyRound,
  Loader2,
  RefreshCw,
  Rocket,
  Settings2,
  ShieldCheck,
  Sparkles,
  Timer,
  TriangleAlert,
  WandSparkles,
} from "lucide-react";
import { Button } from "../../ui/button";
import { Checkbox } from "../../ui/checkbox";
import { cn } from "../../ui/utils";
import { YamlBlock } from "../../wardyn/code-block";
import { AgentBadge, Chip, ConfinementChip, RiskBadge } from "../../wardyn/primitives";
import { StatusChip } from "../../wardyn/status-chip";
import type { StatusKind } from "../../wardyn/copy";
import { RISK_ATTRIBUTION, RUN_MODE, SETUP_RESIDENCY_NOTE } from "../../wardyn/copy";
import { ComposeQuickReview, whyRisky } from "./compose-quick-review";
import { AskPopover } from "./ask-popover";
import { parseMissingSecret } from "./run-warnings";
import type { ComposeResponse, RiskItem, RiskLevel, SetupItem } from "../../../lib/types";

export function ComposeReview({
  result,
  setupItems,
  interactive,
  acknowledged,
  launching,
  launchError,
  onInteractiveChange,
  onAcknowledge,
  onApproveLaunch,
  onAddSecret,
  onFixWorkspace,
  onEditInWizard,
  onCancel,
}: {
  result: ComposeResponse;
  // The setup checklist to render — ALREADY merged with any client-side "re-flip"
  // (decision 9: no recheck endpoint in v1, so a just-fixed item is flipped from
  // data the dialog already has, not re-derived here). Absent/empty renders no
  // checklist section — never a crash against an older server.
  setupItems?: SetupItem[];
  // The run mode the user will launch with. Seeded from the composer's proposal
  // but operator-overridable here (the composer ADVISES; the human decides).
  interactive: boolean;
  acknowledged: boolean;
  launching: boolean;
  // A create-run failure, surfaced INLINE here (not a toast) so the operator keeps
  // the proposal and can fix it in place. null when there's no error.
  launchError?: string | null;
  onInteractiveChange: (v: boolean) => void;
  onAcknowledge: (v: boolean) => void;
  onApproveLaunch: () => void;
  // Open the Add-secret dialog for a secret the launch error OR a setup-checklist
  // item named as missing (same 926da19 plumbing — a second caller, never a
  // re-generalization of it).
  onAddSecret?: (name: string) => void;
  // Kick off a (re-)scan for a setup-checklist item's `scan_workspace` fix.
  onFixWorkspace?: (workspaceId: string) => void;
  onEditInWizard: () => void;
  onCancel: () => void;
}) {
  const { proposed, risk_assessment, overall_risk, summary, warnings, model_notes, llm_access } =
    result;
  const { run, inline_policy } = proposed;

  // The llm_access checklist item's fix (if any) drives the no-model-access
  // banner's action button below — same underlying reconcileLLMAccess verdict as
  // `llm_access`, just carrying a structured fix instead of prose (compose_setup.go's
  // consistency guarantee: the two can never disagree).
  const llmAccessFix = React.useMemo(
    () => setupItems?.find((i) => i.kind === "llm_access")?.fix,
    [setupItems],
  );

  const highItems = React.useMemo(
    () => risk_assessment.filter((r) => r.risk_level === "high"),
    [risk_assessment],
  );
  // Launch is gated only when there ARE high-risk items to acknowledge (D8: only a
  // HIGH grade forces the gate; Medium/Low launch without one). Collapsing nothing
  // can bypass it — the ack section is always rendered when needsAck.
  const needsAck = highItems.length > 0;
  // No-model-access (B3): the run will launch but its first model call 404s. It gets
  // its OWN distinct, high-contrast callout (the fix for "indistinguishable from a
  // benign clamp notice") but is NON-BLOCKING — the launch gate stays reserved for
  // HIGH-risk config (the deliberate "only HIGH gates launch" invariant), and the
  // operator is never trapped. Mirrors the manual wizard's non-blocking warning.
  const noModelAccess = !!llm_access && !llm_access.provisioned;
  const launchDisabled = launching || (needsAck && !acknowledged);

  // A create-run failure that names a not-yet-stored secret — extract the name
  // (shared with the manual wizard, run-warnings.ts) so we can offer a one-click
  // "add it" and keep the fix on-panel.
  const missingSecret = React.useMemo(() => parseMissingSecret(launchError), [launchError]);

  const why = React.useMemo(() => whyRisky(risk_assessment), [risk_assessment]);
  const autoStop = autoStopLabel(inline_policy.auto_stop_after_sec);

  return (
    <div className="space-y-5">
      {/* --- header: orientation + title + neutral identity facts (each once) --- */}
      <div className="space-y-3">
        <div className="flex items-center gap-2">
          <span className="label-eyebrow inline-flex items-center gap-1.5">
            <Sparkles className="size-3 text-primary" aria-hidden="true" />
            AI Run Composer
          </span>
          <StepDots />
          <span className="text-[11.5px] text-muted-foreground">
            Describe · Clarify · <span className="text-foreground">Review</span>
          </span>
        </div>

        <div>
          <h2 className="text-lg font-semibold leading-snug tracking-tight text-foreground">
            {run.task || "Proposed run"}
          </h2>
          {summary && <p className="mt-1 text-sm text-muted-foreground">{summary}</p>}
        </div>

        <div className="flex flex-wrap items-center gap-2">
          <AgentBadge agent={run.agent} />
          <ConfinementChip value={inline_policy.min_confinement_class} />
          <ModeToggle interactive={interactive} onChange={onInteractiveChange} disabled={launching} />
          {run.repo && (
            <Chip tone="neutral" mono>
              <GitBranch className="size-3" />
              {run.repo}
            </Chip>
          )}
          {run.devcontainer_repo && (
            <Chip tone="neutral" mono title="Devcontainer">
              {run.devcontainer_repo}
            </Chip>
          )}
          {autoStop && (
            <Chip tone="neutral">
              <Timer className="size-3" />
              {autoStop}
            </Chip>
          )}
        </div>
      </div>

      {/* --- can / can't split, derived from the clamped policy --- */}
      <ComposeQuickReview inline_policy={inline_policy} />

      {/* --- setup checklist: doctor-style per-requirement readiness, computed
          from the SAME final clamped spec as everything else above — never the
          model's self-report. Between the CAN/CAN'T split and the honest clamp
          notices below (non-blocking everywhere new — decision 4). --- */}
      {setupItems && setupItems.length > 0 && (
        <SetupChecklist items={setupItems} onAddSecret={onAddSecret} onFixWorkspace={onFixWorkspace} />
      )}

      {/* --- model-access verdict (B3): a "this run will do nothing" blocker gets its
          OWN destructive callout + acknowledgement, never buried in the amber clamp
          list; a positive verdict is a quiet reassurance. --- */}
      {noModelAccess && llm_access && (
        <div
          className="flex items-start gap-2 rounded-lg border border-danger/40 bg-danger-subtle p-3 text-xs leading-relaxed text-danger"
          data-testid="no-model-access"
        >
          <TriangleAlert className="mt-0.5 size-3.5 shrink-0" aria-hidden="true" />
          <div>
            <span className="font-semibold">No model access — this run will do nothing.</span>
            <p className="mt-1 text-foreground/90">{llm_access.note}</p>
            {llmAccessFix?.action === "add_secret" && llmAccessFix.secret_name && onAddSecret && (
              <Button
                size="sm"
                variant="outline"
                className="mt-2 gap-1.5"
                onClick={() => onAddSecret(llmAccessFix.secret_name!)}
              >
                <KeyRound className="size-3.5" /> Add the “{llmAccessFix.secret_name}” secret
              </Button>
            )}
          </div>
        </div>
      )}
      {llm_access?.provisioned && (
        <p className="flex items-start gap-2 text-xs leading-relaxed text-success">
          <ShieldCheck className="mt-0.5 size-3.5 shrink-0" aria-hidden="true" />
          {llm_access.note}
        </p>
      )}

      {/* --- honest clamp notices (the grader's real warnings) --- */}
      {warnings && warnings.length > 0 && (
        <div className="flex items-start gap-2 rounded-lg border border-warning/30 bg-warning-subtle p-3 text-xs leading-relaxed text-warning">
          <WandSparkles className="mt-0.5 size-3.5 shrink-0" aria-hidden="true" />
          <div>
            <span className="font-medium">Tightened by policy:</span>
            <ul className="mt-1 list-disc space-y-0.5 pl-4">
              {warnings.map((w, i) => (
                <li key={i}>{w}</li>
              ))}
            </ul>
          </div>
        </div>
      )}

      {/* --- the model's OWN advisory notes (M7): untrusted prose, clearly NOT policy --- */}
      {model_notes && model_notes.length > 0 && (
        <div className="flex items-start gap-2 rounded-lg border border-border bg-muted/40 p-3 text-xs leading-relaxed text-muted-foreground">
          <WandSparkles className="mt-0.5 size-3.5 shrink-0" aria-hidden="true" />
          <div>
            <span className="font-medium">Model notes (advisory — not enforced):</span>
            <ul className="mt-1 list-disc space-y-0.5 pl-4">
              {model_notes.map((n, i) => (
                <li key={i}>{n}</li>
              ))}
            </ul>
          </div>
        </div>
      )}

      {/* --- exact policy, one click away (C7): humane content is primary, JSON secondary --- */}
      <details className="group rounded-lg border border-border bg-card">
        <summary className="flex cursor-pointer list-none items-center gap-2 p-3 text-xs text-muted-foreground hover:text-foreground">
          <Code2 className="size-3.5" aria-hidden="true" />
          View the exact policy that will be enforced
          <ChevronDown
            className="ml-auto size-3.5 transition-transform group-open:rotate-180"
            aria-hidden="true"
          />
        </summary>
        <div className="border-t border-border p-3">
          <YamlBlock value={inline_policy} />
          <p className="mt-2 text-[11px] leading-snug text-muted-foreground">
            The lists above are derived from this policy — the one that will actually be enforced —
            not from the model&apos;s description of itself.
          </p>
        </div>
      </details>

      {/* --- deterministic risk grade + HIGH-only acknowledgment gate (D8) --- */}
      <RiskPanel
        overallRisk={overall_risk}
        why={why}
        highItems={highItems}
        needsAck={needsAck}
        acknowledged={acknowledged}
        onAcknowledge={onAcknowledge}
      />

      {/* --- escalation-only help (advisory; no authority over the proposal) --- */}
      <AskPopover context={{ step: "review", proposalSummary: summary }} />

      {/* --- launch error (inline): a failed create-run keeps the operator on the
          proposal with the reason + a one-click fix, instead of a corner toast. --- */}
      {launchError && (
        <div
          className="space-y-2.5 rounded-lg border border-danger/40 bg-danger-subtle p-3 text-xs leading-relaxed text-danger"
          data-testid="launch-error"
        >
          <div className="flex items-start gap-2">
            <TriangleAlert className="mt-0.5 size-3.5 shrink-0" aria-hidden="true" />
            <div>
              <span className="font-semibold">Couldn&apos;t launch this run.</span>
              <p className="mt-1 text-foreground/90">{launchError}</p>
            </div>
          </div>
          {missingSecret && onAddSecret && (
            <Button
              size="sm"
              variant="outline"
              className="gap-1.5"
              onClick={() => onAddSecret(missingSecret)}
            >
              <KeyRound className="size-3.5" /> Add the “{missingSecret}” secret
            </Button>
          )}
        </div>
      )}

      {/* --- actions, wired to the real proposal --- */}
      <div className="flex items-center gap-2 border-t border-border pt-4">
        <Button variant="ghost" onClick={onCancel} disabled={launching}>
          Cancel
        </Button>
        <Button variant="outline" onClick={onEditInWizard} disabled={launching} className="gap-1.5">
          <Settings2 className="size-4" />
          Edit in wizard
        </Button>
        <Button onClick={onApproveLaunch} disabled={launchDisabled} className="ml-auto gap-1.5">
          {launching ? <Loader2 className="size-4 animate-spin" /> : <Rocket className="size-4" />}
          Approve &amp; launch
        </Button>
      </div>
    </div>
  );
}

// Deterministic risk grade (D8): tone escalates with the grade, RISK_ATTRIBUTION
// makes clear Wardyn's rules graded this — not the model — and ONLY a HIGH grade
// gates launch behind an explicit acknowledgment. The high items list the grader's
// plain-language rationales (never the raw wire field / invariant ref — D4).
function RiskPanel({
  overallRisk,
  why,
  highItems,
  needsAck,
  acknowledged,
  onAcknowledge,
}: {
  overallRisk: RiskLevel;
  why: string[];
  highItems: RiskItem[];
  needsAck: boolean;
  acknowledged: boolean;
  onAcknowledge: (v: boolean) => void;
}) {
  const tone =
    overallRisk === "high"
      ? "border-danger/40 bg-danger-subtle"
      : overallRisk === "medium"
        ? "border-warning/30 bg-warning-subtle"
        : "border-border bg-muted/30";
  return (
    <div className={cn("space-y-2 rounded-xl border p-4", tone)}>
      <div className="flex flex-wrap items-center gap-2">
        <span className="text-sm font-medium text-foreground">Risk:</span>
        <RiskBadge level={overallRisk} />
        {why[0] && <span className="text-[12.5px] text-foreground">{why[0]}</span>}
      </div>
      {/* Non-exhaustive on purpose — the deterministic grader (composer/risk.go)
          has more HIGH triggers (weakest barrier tier, never-reap, …) and this
          copy must never claim to enumerate them. */}
      <p className="text-[11px] leading-relaxed text-muted-foreground">
        {RISK_ATTRIBUTION} High-graded choices — such as the weakest barrier tier, allow-all
        egress, host-writable mounts, or write-capable credentials — require an explicit
        acknowledgment before launch.
      </p>

      {needsAck && (
        <div className="mt-1 border-t border-danger/30 pt-3" data-testid="high-risk-section">
          <div className="flex items-center gap-2 text-danger">
            <TriangleAlert className="size-4" aria-hidden="true" />
            <span className="text-sm font-semibold">High-risk configuration</span>
          </div>
          <ul className="mt-2 list-disc space-y-0.5 pl-4 text-xs text-danger">
            {highItems.map((item, i) => (
              <li key={`${item.field}-${i}`}>{item.rationale}</li>
            ))}
          </ul>
          <label className="mt-3 flex items-start gap-2">
            <Checkbox
              id="ack-high-risk"
              checked={acknowledged}
              onCheckedChange={(c) => onAcknowledge(c === true)}
              className="mt-0.5"
            />
            <span className="text-xs text-foreground">
              I understand the high-risk items above and want to launch this run anyway.
            </span>
          </label>
        </div>
      )}
    </div>
  );
}

// Doctor-style setup readiness (decision 2/3/4): each row is label+detail |
// StatusChip | action button — the same DOM shape as the Getting-started
// funnel's AccessRow (llm-access.tsx), so the vocabulary never drifts
// between the two surfaces even though this one can't share the component (it
// projects a proposal's SetupItem[], not the boot-time SetupStatus). Item text
// (label/detail/required_by) is server-derived but rendered as plain React text
// nodes ONLY — never dangerouslySetInnerHTML — the same caution as everywhere
// else a field could in principle be influenced by the analyzer's output.
function SetupChecklist({
  items,
  onAddSecret,
  onFixWorkspace,
}: {
  items: SetupItem[];
  onAddSecret?: (name: string) => void;
  onFixWorkspace?: (workspaceId: string) => void;
}) {
  return (
    <div className="space-y-2">
      <span className="label-eyebrow">Setup checklist</span>
      <ul className="space-y-2">
        {items.map((item) => (
          <SetupChecklistRow
            key={item.id}
            item={item}
            onAddSecret={onAddSecret}
            onFixWorkspace={onFixWorkspace}
          />
        ))}
      </ul>
    </div>
  );
}

// v1 is declared-present, never live-verified (decision 3) — "satisfied" maps to
// the same green "ready" chip used everywhere else, but the label override below
// says "Configured", never "Ready"/"Verified", so the checklist can't be misread
// as a live health probe. Any status this build doesn't recognize degrades to the
// neutral "unverified" chip rather than crashing.
function setupStatusKind(status: string): StatusKind {
  if (status === "satisfied") return "ready";
  if (status === "missing") return "needs-setup";
  return "unverified";
}

function SetupChecklistRow({
  item,
  onAddSecret,
  onFixWorkspace,
}: {
  item: SetupItem;
  onAddSecret?: (name: string) => void;
  onFixWorkspace?: (workspaceId: string) => void;
}) {
  const fix = item.fix;
  // Only a MISSING llm_access/secret gets the destructive treatment (the run will
  // do nothing, or 422s at launch — H1); every other gap (workspace/repo_credential/
  // egress/backend/config_pair, or any "unverified" item) stays a plain card — its
  // StatusChip tone (amber for needs-setup, neutral for unverified) already carries
  // the signal, so the card itself never needs to escalate too (decision 4). backend
  // and config_pair are host/config state, not a credential absence, so a missing
  // one is never destructive even though it blocks the SAME way a missing secret does.
  const destructive = item.status === "missing" && (item.kind === "llm_access" || item.kind === "secret");
  const residencyNote = item.residency ? SETUP_RESIDENCY_NOTE[item.residency] : undefined;
  return (
    <li
      className={cn(
        "flex flex-wrap items-center gap-3 rounded-xl border p-3.5",
        destructive ? "border-danger/40 bg-danger-subtle" : "border-border bg-card",
      )}
      data-testid={`setup-item-${item.id}`}
    >
      <div className="min-w-[200px] flex-1">
        <div className="text-sm font-semibold text-foreground">{item.label}</div>
        <p className="mt-0.5 text-xs leading-snug text-muted-foreground">Required by {item.required_by}</p>
        {item.detail && <p className="mt-0.5 text-xs leading-snug text-muted-foreground">{item.detail}</p>}
        {residencyNote && (
          <p className="mt-0.5 text-[11px] leading-snug text-muted-foreground/70">{residencyNote}</p>
        )}
      </div>
      <StatusChip
        status={setupStatusKind(item.status)}
        label={item.status === "satisfied" ? "Configured" : undefined}
      />
      {fix?.action === "add_secret" && fix.secret_name && onAddSecret && (
        <Button
          size="sm"
          variant="outline"
          className="shrink-0 gap-1.5"
          onClick={() => onAddSecret(fix.secret_name!)}
        >
          <KeyRound className="size-3.5" /> Add secret
        </Button>
      )}
      {fix?.action === "scan_workspace" && fix.workspace_id && onFixWorkspace && (
        <Button
          size="sm"
          variant="outline"
          className="shrink-0 gap-1.5"
          onClick={() => onFixWorkspace(fix.workspace_id!)}
        >
          <RefreshCw className="size-3.5" /> Scan workspace
        </Button>
      )}
    </li>
  );
}

// Three-dot progress cue (Describe · Clarify · Review) — orientation only.
function StepDots() {
  return (
    <span className="flex items-center gap-1" aria-hidden="true">
      <span className="size-1.5 rounded-full bg-muted-foreground/40" />
      <span className="size-1.5 rounded-full bg-muted-foreground/40" />
      <span className="h-1.5 w-4 rounded-full bg-primary" />
    </span>
  );
}

// Inline segmented control to choose Interactive vs Autonomous — the composer
// ADVISES a mode, the operator DECIDES here. Labels come verbatim from RUN_MODE
// (D3): the pair is Interactive / Autonomous, never Batch/Background.
function ModeToggle({
  interactive,
  onChange,
  disabled,
}: {
  interactive: boolean;
  onChange: (v: boolean) => void;
  disabled?: boolean;
}) {
  const options: { v: boolean; label: string; blurb: string }[] = [
    { v: true, label: RUN_MODE.interactive.label, blurb: RUN_MODE.interactive.blurb },
    { v: false, label: RUN_MODE.autonomous.label, blurb: RUN_MODE.autonomous.blurb },
  ];
  return (
    <div
      role="radiogroup"
      aria-label="Run mode"
      className="inline-flex rounded-md border border-border p-0.5"
    >
      {options.map((opt) => {
        const active = interactive === opt.v;
        return (
          <button
            key={opt.label}
            type="button"
            role="radio"
            aria-checked={active}
            title={opt.blurb}
            disabled={disabled}
            onClick={() => onChange(opt.v)}
            className={cn(
              "rounded px-2 py-0.5 text-xs font-medium transition-colors disabled:opacity-50",
              active ? "bg-primary/15 text-primary" : "text-muted-foreground hover:text-foreground",
            )}
          >
            {opt.label}
          </button>
        );
      })}
    </div>
  );
}

// A human auto-stop label ("Auto-stops after 2h" / "… 30 min"). Never-reap (-1)
// and platform-default (undefined) return null — the never-reap caution is
// surfaced instead as a can-line in the CAN column.
function autoStopLabel(sec?: number): string | null {
  if (typeof sec !== "number" || sec <= 0) return null;
  const hours = sec / 3600;
  if (Number.isInteger(hours)) return `Auto-stops after ${hours}h`;
  return `Auto-stops after ${Math.round(sec / 60)} min`;
}
