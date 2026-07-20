/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Shared presentational primitives for the New Run wizard steps. StepIndicator,
// Field, and OptionCard are lifted from the old new-run-dialog.tsx styling so the
// wizard matches the existing design-system / token conventions. DomainPillList
// renders the removable egress-domain pills.
import * as React from "react";
import { Check, X } from "lucide-react";
import { cn } from "../../ui/utils";
import { Label } from "../../ui/label";
import { WIZARD_STEPS, type WizardStepId } from "./wizard-types";

// Horizontal numbered step indicator with a connecting rail. The active step is
// primary-tinted; completed steps show a check. Generalized (steps? prop,
// defaulting to WIZARD_STEPS) so the first-run setup screen can reuse it with
// its own step id union without forking — the run-wizard call site (no `steps`
// prop) is unchanged and still infers T=WizardStepId from `current`.
export function StepIndicator<T extends string = WizardStepId>({
  current,
  onJump,
  steps = WIZARD_STEPS as unknown as { id: T; label: string }[],
}: {
  current: T;
  onJump?: (id: T) => void;
  steps?: { id: T; label: string }[];
}) {
  const currentIdx = steps.findIndex((s) => s.id === current);
  return (
    <ol className="flex items-center gap-1.5">
      {steps.map((step, i) => {
        const state = i < currentIdx ? "done" : i === currentIdx ? "active" : "todo";
        const clickable = onJump && i <= currentIdx;
        return (
          <React.Fragment key={step.id}>
            <li>
              <button
                type="button"
                disabled={!clickable}
                onClick={() => clickable && onJump?.(step.id)}
                className={cn(
                  "flex items-center gap-2 rounded-md px-1.5 py-1 text-xs transition-colors",
                  clickable && "hover:bg-accent",
                  !clickable && "cursor-default",
                )}
              >
                <span
                  className={cn(
                    "flex size-7 shrink-0 items-center justify-center rounded-full border text-[11px] font-semibold",
                    state === "active" && "border-primary bg-primary text-primary-foreground",
                    state === "done" && "border-primary bg-primary text-primary-foreground",
                    state === "todo" && "border-border text-muted-foreground",
                  )}
                >
                  {state === "done" ? <Check className="size-3" /> : i + 1}
                </span>
                <span
                  className={cn(
                    "hidden font-medium sm:inline",
                    state === "active" ? "text-foreground" : "text-muted-foreground",
                  )}
                >
                  {step.label}
                </span>
              </button>
            </li>
            {i < steps.length - 1 && (
              <li
                aria-hidden
                className={cn(
                  "h-0.5 w-3 shrink-0 sm:w-5",
                  i < currentIdx ? "bg-primary" : "bg-border",
                )}
              />
            )}
          </React.Fragment>
        );
      })}
    </ol>
  );
}

// A labelled form field with optional helper/hint text.
export function Field({
  label,
  htmlFor,
  hint,
  children,
  className,
}: {
  label: React.ReactNode;
  htmlFor?: string;
  hint?: React.ReactNode;
  children: React.ReactNode;
  className?: string;
}) {
  return (
    <div className={cn("space-y-2", className)}>
      <Label htmlFor={htmlFor}>{label}</Label>
      {children}
      {hint && <p className="text-[11px] leading-snug text-muted-foreground">{hint}</p>}
    </div>
  );
}

// A selectable card (radio-style) lifted from the old confinement-class picker.
export function OptionCard({
  selected,
  disabled,
  onClick,
  title,
  hint,
  className,
}: {
  selected: boolean;
  disabled?: boolean;
  onClick: () => void;
  title: React.ReactNode;
  hint?: React.ReactNode;
  className?: string;
}) {
  return (
    <button
      type="button"
      disabled={disabled}
      onClick={onClick}
      aria-pressed={selected}
      className={cn(
        "rounded-lg border p-2.5 text-left transition-colors",
        selected ? "border-primary bg-primary/10" : "border-border hover:border-border-strong",
        disabled && "cursor-not-allowed opacity-50 hover:border-border",
        className,
      )}
    >
      <div className="text-sm font-medium text-foreground">{title}</div>
      {hint && (
        <div className="mt-1 text-[11px] leading-snug text-muted-foreground">{hint}</div>
      )}
    </button>
  );
}

// A list of removable domain pills (used for custom egress domains + deny-list).
export function DomainPillList({
  domains,
  onRemove,
  tone = "neutral",
  emptyHint,
}: {
  domains: string[];
  onRemove: (domain: string) => void;
  tone?: "neutral" | "danger";
  emptyHint?: string;
}) {
  if (!domains.length) {
    return emptyHint ? (
      <p className="text-[11px] text-muted-foreground">{emptyHint}</p>
    ) : null;
  }
  return (
    <div className="flex flex-wrap gap-1.5">
      {domains.map((d) => (
        <span
          key={d}
          className={cn(
            "inline-flex items-center gap-1 rounded-md border px-2 py-0.5 font-mono text-[11px]",
            tone === "danger"
              ? "border-danger/25 bg-danger-subtle text-danger"
              : "border-border bg-surface-2 text-foreground",
          )}
        >
          {d}
          <button
            type="button"
            onClick={() => onRemove(d)}
            className="text-muted-foreground transition-colors hover:text-foreground"
            aria-label={`Remove ${d}`}
          >
            <X className="size-3" />
          </button>
        </span>
      ))}
    </div>
  );
}
