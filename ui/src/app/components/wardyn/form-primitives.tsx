/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Shared form primitives. These lived in screens/new-run/step-shell.tsx, which
// made them look wizard-specific — but Field is used by policies, secrets and
// ssh-keys, none of which are wizards, and OptionCard is the radio-card shape
// the 0.5 console is built on. Moved here so the New Run wizard can be deleted
// without taking three unrelated screens with it.
//
// StepIndicator deliberately did NOT come along: its only two callers are the
// New Run and Add-workspace wizards, both of which are being replaced by single
// screens, so it dies with them rather than outliving its purpose here.
import * as React from "react";
import { X } from "lucide-react";
import { cn } from "../ui/utils";
import { Label } from "../ui/label";

// A labelled form field with optional helper/hint text.
export function Field({
  label,
  htmlFor,
  hint,
  required,
  children,
  className,
}: {
  label: React.ReactNode;
  htmlFor?: string;
  hint?: React.ReactNode;
  /** Marks the label with a visible "*" — pair with `required` on the actual
   *  control so the requirement is never conveyed by the marker's color alone. */
  required?: boolean;
  children: React.ReactNode;
  className?: string;
}) {
  return (
    <div className={cn("space-y-2", className)}>
      {/* The "*" is a sibling of Label, not inside it, so it never joins the
          label's accessible name (getByLabelText("Name") stays exact) — the
          native `required` attribute on the control is what AT announces. */}
      <div className="flex items-center gap-1">
        <Label htmlFor={htmlFor}>{label}</Label>
        {required && (
          <span className="text-muted-foreground" aria-hidden="true">
            *
          </span>
        )}
      </div>
      {children}
      {hint && <p className="text-[0.6875rem] leading-snug text-muted-foreground">{hint}</p>}
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
        <div className="mt-1 text-[0.6875rem] leading-snug text-muted-foreground">{hint}</div>
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
      <p className="text-[0.6875rem] text-muted-foreground">{emptyHint}</p>
    ) : null;
  }
  return (
    <div className="flex flex-wrap gap-1.5">
      {domains.map((d) => (
        <span
          key={d}
          className={cn(
            "inline-flex items-center gap-1 rounded-md border px-2 py-0.5 font-mono text-[0.6875rem]",
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
