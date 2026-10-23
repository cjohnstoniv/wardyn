/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Desktop group card (board M7-Knobs-B) in the Wardyn boundary's hard limits: one row per desktop
// direction, each Allowed or Blocked, under a header with the values at a glance. A direction the boundary
// blocks is blocked for every run (types.BoundarySpec.Desktop); one it allows is left to the layers below.
import type { DesktopControls, TransferRule } from "../../../lib/types/policy";
import { BOUNDARY_DESKTOP as C } from "../../../lib/boundary-copy";

const RULES: TransferRule[] = ["allowed", "blocked"];

/**
 * The rule the boundary holds each direction to, as the server's meet reads it: a boundary that names no
 * desktop bounds nothing, so every direction is allowed; a named desktop's unset direction is that
 * direction's default (paste in and upload allowed, copy out and download blocked), the stricter value.
 */
export function boundaryDesktopRules(d: DesktopControls | undefined): Required<DesktopControls> {
  if (!d) return { paste_in: "allowed", copy_out: "allowed", upload: "allowed", download: "allowed" };
  return {
    paste_in: d.paste_in ?? "allowed",
    copy_out: d.copy_out ?? "blocked",
    upload: d.upload ?? "allowed",
    download: d.download ?? "blocked",
  };
}

/** The header's values at a glance, e.g. "Paste in allowed · Copy out blocked · …". */
export function desktopNow(d: DesktopControls | undefined): string {
  const rules = boundaryDesktopRules(d);
  return C.ROWS.map((r) => `${r.short} ${rules[r.key]}`).join(" · ");
}

export function DesktopGroupCard({
  value,
  disabled,
  onChange,
}: {
  value: DesktopControls | undefined;
  disabled: boolean;
  onChange: (d: DesktopControls) => void;
}) {
  const rules = boundaryDesktopRules(value);
  return (
    <section aria-labelledby="boundary-desktop-title" data-testid="boundary-desktop" className="rounded-lg border border-border">
      <header className="flex flex-col gap-1 border-b border-border px-4 py-3">
        <div className="flex flex-wrap items-baseline gap-2">
          <h3 id="boundary-desktop-title" className="text-sm font-medium text-foreground">
            {C.TITLE}
          </h3>
          <span className="text-xs text-muted-foreground">{C.COUNT(C.ROWS.length)}</span>
        </div>
        <p className="text-xs text-muted-foreground">{C.APPLIES}</p>
        <p className="flex flex-wrap gap-x-3 gap-y-1 text-xs" data-testid="boundary-desktop-now">
          <span className="font-medium text-foreground">{C.NOW}</span>
          {C.ROWS.map((r) => (
            <span key={r.key} className="text-muted-foreground">
              {r.short} <span className={rules[r.key] === "blocked" ? "font-medium text-foreground" : ""}>{rules[r.key]}</span>
            </span>
          ))}
        </p>
      </header>
      <ul>
        {C.ROWS.map((r) => (
          <li key={r.key} className="flex flex-wrap items-center gap-x-4 gap-y-2 border-b border-border px-4 py-3 last:border-b-0">
            <fieldset disabled={disabled} className="flex flex-wrap items-center gap-x-4 gap-y-2">
              <legend className="sr-only">{r.label}</legend>
              <span aria-hidden="true" className="min-w-[13rem] text-sm font-medium text-foreground">
                {r.label}
              </span>
              <span className="inline-flex overflow-hidden rounded-md border border-border text-xs">
                {RULES.map((v) => (
                  <label key={v} className="cursor-pointer">
                    <input
                      type="radio"
                      className="peer sr-only"
                      name={`boundary-desktop-${r.key}`}
                      value={v}
                      checked={rules[r.key] === v}
                      onChange={() => onChange({ ...rules, [r.key]: v })}
                    />
                    <span className="block px-3 py-1.5 text-muted-foreground peer-checked:bg-primary peer-checked:text-primary-foreground peer-focus-visible:ring-2 peer-focus-visible:ring-ring peer-disabled:cursor-not-allowed peer-disabled:opacity-60">
                      {v === "allowed" ? C.ALLOWED : C.BLOCKED}
                    </span>
                  </label>
                ))}
              </span>
            </fieldset>
            <span className="text-xs text-muted-foreground">{C.BELOW}</span>
          </li>
        ))}
      </ul>
      <p className="border-t border-border px-4 py-3 text-xs text-muted-foreground">{C.ENFORCED}</p>
    </section>
  );
}
