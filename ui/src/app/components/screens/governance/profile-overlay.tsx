/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The composition half of the profile editor (0.8.6, mock packet M3 S5): the
// base picker, the per-field overlay rows and the read-only effective view.
//
// An overlay field is OPT-IN. A row's Narrow switch adds its key to the overlay
// object and turning it off deletes the key, so an untouched field is ABSENT on
// the wire, never an empty list (an empty list narrows to nothing, and for
// methods the server refuses it). Keys the console has no row for are left in
// the overlay exactly as they were loaded.
//
// Every product string comes from governance-copy.ts. The only literals here
// are the field names themselves (wire keys, rendered mono) and On/Off.
import * as React from "react";
import type { CeilingOverlay, GovernanceLimits, GovernanceProfile } from "../../../lib/api/governance";
import { GOVERNANCE as GOV } from "../../../lib/governance-copy";
import type { ConfinementClass } from "../../../lib/types";
import { CC_ORDER } from "../../../lib/types";
import { Input } from "../../ui/input";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "../../ui/select";
import { CC_META } from "../../wardyn/cc-meta";
import { Chip } from "../../wardyn/primitives";
import { Switch } from "../../wardyn/form-primitives";
import { Segmented } from "../permissions";
import { limitChips } from "./limit-chips";

// ---- the base graph -------------------------------------------------------

export const MAX_DEPTH = 3;
export const BASE_NONE = "none";
export const BASE_DEPLOYMENT = "deployment";

const byId = (profiles: GovernanceProfile[]) => new Map(profiles.map((p) => [p.id, p]));

// Profiles on the path from p to the deployment, p included. A visited guard
// keeps a corrupt cycle from looping the render.
function depthOf(p: GovernanceProfile, all: Map<string, GovernanceProfile>): number {
  const seen = new Set<string>();
  let n = 0;
  for (let cur: GovernanceProfile | undefined = p; cur && !seen.has(cur.id); cur = cur.base_profile_id ? all.get(cur.base_profile_id) : undefined) {
    seen.add(cur.id);
    n++;
  }
  return n;
}

// Profiles on the longest path from `id` down through its children, itself included.
function heightOf(id: string, profiles: GovernanceProfile[], seen = new Set<string>()): number {
  if (seen.has(id)) return 0;
  seen.add(id);
  const kids = profiles.filter((p) => p.base_profile_id === id);
  return 1 + Math.max(0, ...kids.map((k) => heightOf(k.id, profiles, seen)));
}

export type BaseOption = { profile: GovernanceProfile; hint?: string };

// Every profile but self as a candidate base. One that is based on self (a
// descendant) or would push the chain past MAX_DEPTH is listed but carries its
// hint and is disabled.
export function baseOptions(self: GovernanceProfile | null, profiles: GovernanceProfile[]): BaseOption[] {
  const all = byId(profiles);
  const selfHeight = self ? heightOf(self.id, profiles) : 1;
  return profiles
    .filter((p) => p.id !== self?.id)
    .map((profile) => {
      const seen = new Set<string>();
      let descendant = false;
      for (let cur: GovernanceProfile | undefined = profile; cur && !seen.has(cur.id); cur = cur.base_profile_id ? all.get(cur.base_profile_id) : undefined) {
        seen.add(cur.id);
        if (self && cur.id === self.id) descendant = true;
      }
      if (descendant) return { profile, hint: GOV.BASE_DESCENDANT };
      if (depthOf(profile, all) + selfHeight > MAX_DEPTH) return { profile, hint: GOV.BASE_TOO_DEEP };
      return { profile };
    });
}

export function childrenOf(id: string, profiles: GovernanceProfile[]): GovernanceProfile[] {
  return profiles.filter((p) => p.base_profile_id === id);
}

// What the list's chip names: the base profile, or the deployment ceiling.
export function baseName(p: GovernanceProfile, profiles: GovernanceProfile[]): string | null {
  if (!p.overlay) return null;
  if (!p.base_profile_id) return GOV.BASE_DEPLOYMENT;
  return profiles.find((x) => x.id === p.base_profile_id)?.name ?? GOV.BASE_DEPLOYMENT;
}

export function BaseChip({ profile, profiles }: { profile: GovernanceProfile; profiles: GovernanceProfile[] }) {
  const name = baseName(profile, profiles);
  if (name === null) return null;
  return (
    <div className="mt-1">
      <Chip tone="neutral">{GOV.BASE_CHIP(name)}</Chip>
    </div>
  );
}

export function BasePicker({
  value,
  options,
  disabled,
  onChange,
}: {
  value: string;
  options: BaseOption[];
  disabled: boolean;
  onChange: (next: string) => void;
}) {
  return (
    <Select value={value} disabled={disabled} onValueChange={onChange}>
      <SelectTrigger id="governance-profile-base" className="max-w-[28rem]">
        <SelectValue />
      </SelectTrigger>
      <SelectContent>
        <SelectItem value={BASE_NONE}>{GOV.BASE_NONE}</SelectItem>
        <SelectItem value={BASE_DEPLOYMENT}>{GOV.BASE_DEPLOYMENT}</SelectItem>
        {options.map(({ profile, hint }) => (
          <SelectItem key={profile.id} value={profile.id} disabled={!!hint} title={hint}>
            {profile.name}
            {hint && <span className="block text-meta text-muted-foreground">{hint}</span>}
          </SelectItem>
        ))}
      </SelectContent>
    </Select>
  );
}

// ---- overlay rows ---------------------------------------------------------

type Kind = "list" | "number" | "false" | "true" | "barrier";
type Field = { key: string; kind: Kind; label: string; mono: boolean };

const ceilingField = (key: string, kind: Kind): Field => ({ key, kind, label: key, mono: true });

// The ceiling fields a composed profile can narrow from the console, in the
// order the Ceiling section lists them. A key outside this list is carried
// through untouched.
export const CEILING_FIELDS: Field[] = [
  ceilingField("allowed_domains", "list"),
  ceilingField("denied_domains", "list"),
  ceilingField("allowed_methods", "list"),
  ceilingField("min_confinement_class", "barrier"),
  ceilingField("allow_all_egress", "false"),
  ceilingField("git_push_any_branch", "false"),
  ceilingField("auto_stop_after_sec", "number"),
];

export const LIMIT_FIELDS: Field[] = [
  { key: "deny_task_mode_exec", kind: "true", label: GOV.LIMIT_EXEC_LABEL, mono: false },
  { key: "deny_interactive", kind: "true", label: GOV.LIMIT_INTERACTIVE_LABEL, mono: false },
  { key: "deny_user_drive", kind: "true", label: GOV.LIMIT_DRIVE_LABEL, mono: false },
  { key: "max_concurrent_runs", kind: "number", label: GOV.LIMIT_CONCURRENT_LABEL, mono: false },
  { key: "max_ephemeral_disk_mib", kind: "number", label: GOV.LIMIT_EPHEMERAL_LABEL, mono: false },
  { key: "max_drive_size_mib", kind: "number", label: GOV.LIMIT_DRIVE_SIZE_LABEL, mono: false },
];

type Bag = Record<string, unknown>;

const onOff = (v: unknown) => (v ? "On" : "Off");

// The inherited value as the row prints it after "Inherited:".
function shown(kind: Kind, v: unknown): string {
  if (kind === "list") return Array.isArray(v) && v.length > 0 ? v.join(", ") : GOV.LIMITS_NONE;
  if (kind === "number") return typeof v === "number" && v > 0 ? String(v) : GOV.LIMITS_NONE;
  if (kind === "barrier") return typeof v === "string" && v in CC_META ? CC_META[v as ConfinementClass].label : GOV.LIMITS_NONE;
  return onOff(v);
}

// What a row holds the moment Narrow is switched on: the inherited value for a
// list, number or barrier (so the admin edits down from it), and the narrowing
// direction for a boolean. Methods fall back to GET, since an empty list is refused.
function seed(f: Field, inherited: unknown): unknown {
  switch (f.kind) {
    case "false":
      return false;
    case "true":
      return true;
    case "number":
      return typeof inherited === "number" && inherited > 0 ? inherited : 1;
    case "barrier":
      return typeof inherited === "string" ? inherited : "CC1";
    default:
      return Array.isArray(inherited) && inherited.length > 0 ? inherited : f.key === "allowed_methods" ? ["GET"] : [];
  }
}

export function OverlayRows({
  fields,
  value,
  inherited,
  disabled,
  onChange,
  idPrefix,
}: {
  fields: Field[];
  value: Bag;
  inherited: Bag;
  disabled: boolean;
  onChange: (next: Bag) => void;
  idPrefix: string;
}) {
  const set = (key: string, v: unknown) => onChange({ ...value, [key]: v });
  const clear = (key: string) => {
    const { [key]: _drop, ...rest } = value;
    onChange(rest);
  };
  return (
    <div>
      {fields.map((f) => {
        const on = f.key in value;
        const id = `${idPrefix}-${f.key}`;
        return (
          <div key={f.key} className="mt-3 border-t border-border pt-3 first-of-type:border-t-0" data-testid={id}>
            <div className="flex items-start gap-3">
              <Switch
                checked={on}
                disabled={disabled}
                label={`${f.label} ${GOV.OVERLAY_NARROW}`}
                className="mt-0.5"
                onChange={(next) => (next ? set(f.key, seed(f, inherited[f.key])) : clear(f.key))}
              />
              <div className="min-w-0 flex-1">
                <p className={f.mono ? "font-mono text-body font-medium text-foreground" : "text-body font-medium text-foreground"}>{f.label}</p>
                <p className="mt-0.5 text-xs text-muted-foreground">
                  {GOV.OVERLAY_INHERITED(shown(f.kind, inherited[f.key]))}
                </p>
                {on && <OverlayValue field={f} id={id} value={value[f.key]} disabled={disabled} onChange={(v) => set(f.key, v)} />}
              </div>
            </div>
          </div>
        );
      })}
    </div>
  );
}

function OverlayValue({
  field,
  id,
  value,
  disabled,
  onChange,
}: {
  field: Field;
  id: string;
  value: unknown;
  disabled: boolean;
  onChange: (v: unknown) => void;
}) {
  if (field.kind === "false" || field.kind === "true") return null;
  if (field.kind === "number") {
    return (
      <Input
        id={`${id}-value`}
        aria-label={field.label}
        type="number"
        min={0}
        className="mt-2 max-w-[12rem] font-mono"
        disabled={disabled}
        value={typeof value === "number" ? value : ""}
        onChange={(e) => onChange(Math.max(0, Math.trunc(Number(e.target.value)) || 0))}
      />
    );
  }
  if (field.kind === "barrier") {
    return (
      <div className="mt-2">
        <Segmented
          value={(typeof value === "string" ? value : "CC1") as ConfinementClass}
          disabled={disabled}
          onChange={onChange}
          options={CC_ORDER.map((cc) => ({ value: cc, label: CC_META[cc].label }))}
        />
      </div>
    );
  }
  return <ListValue id={id} label={field.label} value={Array.isArray(value) ? (value as string[]) : []} disabled={disabled} onChange={onChange} />;
}

// One entry per line. The text is kept locally so a blank line being typed is
// not eaten; the overlay gets the trimmed, non-empty lines.
function ListValue({
  id,
  label,
  value,
  disabled,
  onChange,
}: {
  id: string;
  label: string;
  value: string[];
  disabled: boolean;
  onChange: (v: string[]) => void;
}) {
  const [text, setText] = React.useState(value.join("\n"));
  return (
    <textarea
      id={`${id}-value`}
      aria-label={label}
      rows={Math.min(6, Math.max(2, text.split("\n").length))}
      className="mt-2 w-full max-w-[28rem] rounded-md border border-border bg-input-background px-3 py-2 font-mono text-xs"
      disabled={disabled}
      value={text}
      onChange={(e) => {
        setText(e.target.value);
        onChange(
          e.target.value
            .split("\n")
            .map((l) => l.trim())
            .filter(Boolean),
        );
      }}
    />
  );
}

// ---- the effective view ---------------------------------------------------

// Read-only: the API's `effective`, as of the last save. Where the profile is
// composed, what binds is the base narrowed here.
export function EffectiveView({ profile }: { profile: GovernanceProfile }) {
  const eff = profile.effective;
  if (!eff) return null;
  const ceiling = eff.ceiling as unknown as Bag;
  const chips = limitChips(eff.limits as GovernanceLimits);
  return (
    <section className="mt-6" data-testid="governance-profile-effective">
      <h4 className="text-body font-medium text-foreground">{GOV.EFFECTIVE_TITLE}</h4>
      <p className="mt-0.5 max-w-[82ch] text-body text-muted-foreground">{GOV.EFFECTIVE_LEAD}</p>
      {eff.error ? (
        <p className="mt-3 font-mono text-xs text-danger">{eff.error}</p>
      ) : (
        <>
          <dl className="mt-3 grid max-w-[82ch] grid-cols-[minmax(0,14rem)_1fr] gap-x-4 gap-y-1.5 text-xs">
            {CEILING_FIELDS.map((f) => (
              <React.Fragment key={f.key}>
                <dt className="font-mono text-muted-foreground">{f.label}</dt>
                <dd className="font-mono text-foreground" data-testid={`governance-effective-${f.key}`}>
                  {shown(f.kind, ceiling[f.key])}
                </dd>
              </React.Fragment>
            ))}
          </dl>
          <div className="mt-3 flex flex-wrap items-center gap-1.5" data-testid="governance-effective-limits">
            {chips.length > 0 ? chips : GOV.LIMITS_NONE}
          </div>
          {(eff.warnings ?? []).map((w) => (
            <p key={w} className="mt-2 font-mono text-xs text-warning">
              {w}
            </p>
          ))}
        </>
      )}
    </section>
  );
}

// ---- wire -----------------------------------------------------------------

export const asOverlay = (b: Bag): CeilingOverlay => b as CeilingOverlay;

// The refusal heading for a server reason, or undefined when the console has
// none for it. The server's own sentence renders under it, verbatim.
export function composeRefusalTitle(reason: string): string | undefined {
  switch (reason) {
    case "governance_overlay_invalid":
      return GOV.REFUSED_OVERLAY_INVALID;
    case "governance_profile_cycle":
      return GOV.REFUSED_CYCLE;
    case "governance_profile_depth":
      return GOV.REFUSED_DEPTH;
    case "governance_overlay_unsatisfiable":
      return GOV.REFUSED_UNSATISFIABLE;
    default:
      return undefined;
  }
}
