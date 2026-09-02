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
      {hint && <p className="text-xs leading-snug text-muted-foreground">{hint}</p>}
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
        <div className="mt-1 text-xs leading-snug text-muted-foreground">{hint}</div>
      )}
    </button>
  );
}

// A two-state switch. Hoisted here from governance/profile-editor.tsx's
// LimitRow the day a third screen wanted one — which is what that file's own
// note said to do — so the governance limits, the drive editor's Writable, the
// allocation form's Enabled and /permissions' per-kind enforcement are all the
// SAME control, not four copies of twenty lines.
//
// It replaces no @radix-ui/react-switch: cb351ba9 dropped ui/switch.tsx AND
// that dependency as never-imported, and a native button with role="switch" is
// the same accessible contract the suites assert, in ten lines and no
// dependency, following the aria-checked pattern Segmented and OptionCard use.
//
// role="switch" is load-bearing beyond semantics: it is what keeps a checked
// switch (which paints bg-primary) out of the "exactly one teal BUTTON per
// screen" counts that both the unit and e2e suites run.
export function Switch({
  checked,
  onChange,
  disabled,
  /** The switch's accessible name — the field's own label, never new copy. */
  label,
  className,
}: {
  checked: boolean;
  onChange: (next: boolean) => void;
  disabled?: boolean;
  label: string;
  className?: string;
}) {
  return (
    <button
      type="button"
      role="switch"
      aria-checked={checked}
      aria-label={label}
      disabled={disabled}
      onClick={() => onChange(!checked)}
      className={cn(
        "inline-flex h-[1.15rem] w-8 shrink-0 items-center rounded-full border border-transparent p-[1px] transition-colors outline-none focus-visible:ring-[3px] focus-visible:ring-ring disabled:cursor-not-allowed disabled:opacity-50",
        checked ? "bg-primary" : "bg-muted",
        className,
      )}
    >
      <span
        aria-hidden="true"
        className={cn(
          "pointer-events-none block size-4 rounded-full bg-card shadow-sm transition-transform",
          checked ? "translate-x-[calc(100%-2px)]" : "translate-x-0",
        )}
      />
    </button>
  );
}
