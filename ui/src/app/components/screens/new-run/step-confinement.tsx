/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Step 4 — Barrier + lifecycle. The barrier picker is gated by what the runner
// actually supports (health().confinement_classes); genuinely-unavailable tiers
// (e.g. Vault with no Kata runtime registered) render disabled with the launch-time fact.
// Lifecycle is never-reap (auto for interactive => -1) or an explicit auto-stop
// window.
import * as React from "react";
import { Check, ShieldCheck } from "lucide-react";
import { Button } from "../../ui/button";
import { Input } from "../../ui/input";
import { RadioGroup, RadioGroupItem } from "../../ui/radio-group";
import { Label } from "../../ui/label";
import { cn } from "../../ui/utils";
import { ConfinementChip } from "../../wardyn/primitives";
import { BarrierStrengthStrip } from "../../wardyn/barrier-strength-strip";
import { CC_META, CC_ORDER, CONFINEMENT_CONSTANT_NOTE } from "../../wardyn/cc-meta";
import { TierMatrixDialog } from "../../wardyn/tier-matrix";
import { RESIDUAL_PREFIX } from "../../wardyn/copy";
import { StatusChip } from "../../wardyn/status-chip";
import { Field } from "./step-shell";
import type { ConfinementClass } from "../../../lib/types";
import type { Lifecycle, WizardState } from "./wizard-types";

// HIGH fix (confinement labels): CC1/CC2/CC3 select ONLY the isolation
// SUBSTRATE — credential brokering, egress filtering, and approvals are
// governed by the policy, independent of the class. The honest wording is the
// single source of truth in wardyn/cc-meta.ts (shared with ConfinementChip's
// tooltip); this step only adds the scenario-picking guidance and picker
// chrome. Users never see the CC1/CC2/CC3 wire code outside the chip's / this
// card's hover tooltip (operators only). CC_ORDER is cc-meta's canonical
// weakest->strongest ladder — imported, never redeclared here.

// "Pick this tier when…" guidance — leads each card, ahead of the residual-risk
// line. Scenario framing, not a repeat of cc-meta's protects/doesntProtect
// sentences.
const TIER_GUIDANCE: Record<ConfinementClass, string> = {
  CC1: "Trying things out, or the code is your own. Quickest start — works on any host.",
  CC2: "Real work on real repos. Closes the Fence's holes — nothing touches your kernel.",
  CC3: "Untrusted code, or secrets nearby. Its own hardware-walled VM and kernel.",
};

// Launch-time fact only: the runner's health probe doesn't offer this tier for
// THIS run. Cause-neutral on purpose — whether that's missing hardware or just
// an uninstalled runtime is Getting Started's job to diagnose (it has the
// /dev/kvm probe); claiming a hardware cause here would lie on a KVM-capable
// host that merely lacks the runtime.
const UNAVAILABLE_REASON: Record<ConfinementClass, string> = {
  CC1: "No Fence substrate on this runner.",
  CC2: "No Wall (gVisor) runtime on this runner — set it up in Getting started.",
  CC3: "No Vault (Kata microVM) runtime on this runner — set it up in Getting started.",
};

export function StepConfinement({
  state,
  patch,
  availableClasses,
  minClass,
}: {
  state: WizardState;
  patch: (p: Partial<WizardState>) => void;
  // The set of confinement classes the runner reports as available. null means
  // the health probe hasn't resolved — render "Checking…", never a definitive
  // hardware reason for a tier we haven't actually probed. Probed-but-empty
  // falls back to allowing only CC1 (the local floor).
  availableClasses: string[] | null;
  // Policy floor (e.g. a composer proposal's min_confinement_class): a run can
  // request an equal-or-STRONGER tier than its policy minimum, never a weaker one
  // (the server 422s a weaker request). Tiers below this render disabled with a
  // clear reason instead of letting the operator build an unlaunchable run.
  minClass?: ConfinementClass;
}) {
  const probing = availableClasses === null;
  const available = new Set(
    probing ? CC_ORDER : availableClasses.length ? availableClasses : ["CC1"],
  );
  // "Compare all three" opens the pricing-table matrix (E1) — detail on demand,
  // never a replacement for the picker above.
  const [showMatrix, setShowMatrix] = React.useState(false);

  return (
    <div className="space-y-5">
      <Field
        label="Barrier"
        hint="How strongly this run is walled off from your machine. Hover a tier for how it works — egress, credentials, approvals, and audit apply at every tier regardless of which you pick."
      >
        <div className="grid grid-cols-1 gap-2.5 sm:grid-cols-3">
          {CC_ORDER.map((cc) => {
            const meta = CC_META[cc];
            const belowFloor = !!minClass && CC_ORDER.indexOf(cc) < CC_ORDER.indexOf(minClass);
            const enabled = available.has(cc) && !belowFloor;
            const disabledReason = belowFloor
              ? `Below this run's minimum barrier — needs ${CC_META[minClass].label} or stronger.`
              : UNAVAILABLE_REASON[cc];
            const selected = state.confinementClass === cc;
            return (
              <button
                key={cc}
                type="button"
                disabled={!enabled}
                aria-pressed={selected}
                onClick={() => patch({ confinementClass: cc })}
                title={meta.mechanism}
                className={cn(
                  "relative flex flex-col rounded-lg border p-3 text-left transition-colors",
                  selected
                    ? "border-primary bg-primary/5 ring-2 ring-primary/40"
                    : "border-border bg-card hover:border-border-strong",
                  !enabled && "cursor-not-allowed opacity-60",
                )}
              >
                <div className="flex items-center gap-2">
                  <ConfinementChip value={cc} />
                  {selected && (
                    <span className="ml-auto flex size-4 shrink-0 items-center justify-center rounded-full bg-primary text-primary-foreground">
                      <Check className="size-2.5" />
                    </span>
                  )}
                  {probing && !selected && (
                    <span className="ml-auto">
                      <StatusChip status="checking" />
                    </span>
                  )}
                  {!enabled && (
                    <span className="ml-auto">
                      <StatusChip status="unavailable" reason={disabledReason} />
                    </span>
                  )}
                </div>
                <p className="mt-2.5 text-[12px] leading-snug text-foreground/80">
                  {TIER_GUIDANCE[cc]}
                </p>
                <p className="mt-2 text-[11px] leading-snug text-muted-foreground">
                  <span className="font-semibold text-foreground/80">{RESIDUAL_PREFIX}</span>{" "}
                  {meta.doesntProtect}
                </p>
                {!enabled && (
                  <p className="mt-2 text-[11px] leading-snug text-danger">
                    {disabledReason}
                  </p>
                )}
                <div className="mt-3">
                  <BarrierStrengthStrip tier={cc} muted={!enabled} />
                </div>
              </button>
            );
          })}
        </div>
        <p className="mt-3 flex items-start gap-2 text-[11px] leading-snug text-muted-foreground">
          <ShieldCheck className="mt-0.5 size-3.5 shrink-0 text-primary" />
          {CONFINEMENT_CONSTANT_NOTE}
        </p>
        <Button size="sm" variant="link" className="mt-1 h-auto p-0 text-xs" onClick={() => setShowMatrix(true)}>
          Compare all three →
        </Button>
        <TierMatrixDialog open={showMatrix} onOpenChange={setShowMatrix} />
      </Field>

      <Field
        label="How long may it run?"
        hint={
          state.mode === "interactive"
            ? "Interactive runs come up idle — keeping it running keeps the sandbox alive for you to attach."
            : "Auto-stop reaps the sandbox after the window elapses."
        }
      >
        <RadioGroup
          value={state.lifecycle}
          onValueChange={(v) => patch({ lifecycle: v as Lifecycle })}
          className="gap-2"
        >
          <label className="flex items-center gap-2.5 rounded-lg border border-border p-2.5">
            <RadioGroupItem value="never" id="lc-never" />
            <Label htmlFor="lc-never" className="flex-1 cursor-pointer">
              Keep running until I stop it
            </Label>
            <span className="font-mono text-[10.5px] text-muted-foreground">
              auto_stop_after_sec
            </span>
          </label>
          <label className="flex items-center gap-2.5 rounded-lg border border-border p-2.5">
            <RadioGroupItem value="auto" id="lc-auto" />
            <div className="flex flex-1 items-center gap-2">
              <Label htmlFor="lc-auto" className="cursor-pointer">
                Auto-stop after
              </Label>
              <Input
                type="number"
                min={1}
                value={state.autoStopMinutes}
                onChange={(e) =>
                  patch({ autoStopMinutes: Number(e.target.value), lifecycle: "auto" })
                }
                disabled={state.lifecycle !== "auto"}
                className="w-20 font-mono"
              />
              <span className="text-sm text-muted-foreground">min</span>
            </div>
            <span className="font-mono text-[10.5px] text-muted-foreground">
              auto_stop_after_sec
            </span>
          </label>
        </RadioGroup>
      </Field>
    </div>
  );
}
