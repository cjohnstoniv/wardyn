/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Desktop group card (board M7-Knobs-B) in the Wardyn boundary's hard limits: one row per desktop
// direction, each Allowed or Blocked, under a header with the values at a glance. A direction the boundary
// blocks is blocked for every run (types.BoundarySpec.Desktop); one it allows is left to the layers below.
// Each direction's "Lock for teams below" adds desktop.<direction> to the boundary's locked_knobs, so no
// team below may set it otherwise; the header's group lock sets or clears all four.
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

/** The lock name of one direction; the bare "desktop" lock fixes all four. */
const lockName = (key: string) => `desktop.${key}`;
const WHOLE = "desktop";

/** Whether locks fix the direction key for teams below. */
export const desktopLocked = (locks: readonly string[], key: string) => locks.includes(WHOLE) || locks.includes(lockName(key));

/** locks with the direction keys locked or not: a whole-desktop lock is spelled out as its four directions. */
function withLocks(locks: readonly string[], keys: readonly string[], on: boolean): string[] {
  const spelled = locks.includes(WHOLE) ? [...locks.filter((l) => l !== WHOLE), ...C.ROWS.map((r) => lockName(r.key))] : [...locks];
  const names = new Set(keys.map(lockName));
  const out = spelled.filter((l) => !names.has(l));
  if (on) out.push(...names);
  return [...new Set(out)].sort();
}

/** The directions locks fix, e.g. "Copy out, Download", for the dry run. */
export function desktopLocksNow(locks: readonly string[] | undefined): string {
  const held = C.ROWS.filter((r) => desktopLocked(locks ?? [], r.key)).map((r) => r.short);
  return held.length > 0 ? held.join(", ") : C.NO_LOCKS;
}

/** The header's values at a glance, e.g. "Paste in allowed · Copy out blocked · …". */
export function desktopNow(d: DesktopControls | undefined): string {
  const rules = boundaryDesktopRules(d);
  return C.ROWS.map((r) => `${r.short} ${rules[r.key]}`).join(" · ");
}

export function DesktopGroupCard({
  value,
  locks,
  disabled,
  onChange,
  onLocksChange,
}: {
  value: DesktopControls | undefined;
  locks: readonly string[];
  disabled: boolean;
  onChange: (d: DesktopControls) => void;
  onLocksChange: (locks: string[]) => void;
}) {
  const rules = boundaryDesktopRules(value);
  const keys = C.ROWS.map((r) => r.key);
  const groupLocked = keys.every((k) => desktopLocked(locks, k));
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
        <label className="flex items-center gap-2 text-xs text-foreground">
          <input
            type="checkbox"
            disabled={disabled}
            checked={groupLocked}
            onChange={(e) => onLocksChange(withLocks(locks, keys, e.target.checked))}
          />
          {C.LOCK_GROUP}
        </label>
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
              <span className="text-xs text-muted-foreground">{C.BELOW}</span>
              <label className="flex items-center gap-2 text-xs text-foreground">
                <input
                  type="checkbox"
                  checked={desktopLocked(locks, r.key)}
                  onChange={(e) => onLocksChange(withLocks(locks, [r.key], e.target.checked))}
                />
                {C.LOCK_ROW}
              </label>
            </fieldset>
          </li>
        ))}
      </ul>
      <p className="border-t border-border px-4 py-3 text-xs text-muted-foreground">{C.ENFORCED}</p>
    </section>
  );
}
