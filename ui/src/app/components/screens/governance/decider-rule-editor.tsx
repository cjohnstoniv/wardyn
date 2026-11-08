/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// One rule of a profile's Deciders section, opened in place. Every enable, disable and hide decision is a
// call to decider-rules.ts (internal/types/deciders.go as pure functions): the box offers no rule the
// server refuses, so a 400 stays the fallback. It edits the section's draft; nothing is saved here.
import * as React from "react";
import { X } from "lucide-react";
import type { DeciderKind, DeciderRule } from "../../../lib/api/deciders";
import { goDuration, goDurationMs } from "../../../lib/go-duration";
import { RUN_LIMIT_UNITS, RUN_LIMITS as RL } from "../../../lib/governance-copy";
import { Button } from "../../ui/button";
import { Checkbox } from "../../ui/checkbox";
import { Input } from "../../ui/input";
import { cn } from "../../ui/utils";
import { Chip } from "../../wardyn/primitives";
import { DECIDERS, DECIDER_KIND_LABEL } from "../../wardyn/copy/deciders";
import { DurationField } from "../../wardyn/duration-field";
import { Field } from "../../wardyn/form-primitives";
import {
  kindBlock,
  kindsHeldElsewhere,
  MAX_WHO,
  ownerAllowed,
  ruleProblems,
  scopeRowShown,
  secondHumanState,
  toggleKind,
} from "./decider-rules";

const KINDS = Object.keys(DECIDER_KIND_LABEL) as DeciderKind[];
const SCOPES = [
  { value: undefined, label: DECIDERS.SCOPE_NONE },
  { value: "once", label: DECIDERS.SCOPE_ONCE },
  { value: "until", label: DECIDERS.SCOPE_UNTIL },
  { value: "run", label: DECIDERS.SCOPE_RUN },
] as const;
const MS_PER_UNIT = { minute: 60_000, hour: 3_600_000 } as const;
const DURATION_UNITS = (["minute", "hour"] as const).map((one) => {
  const u = RUN_LIMIT_UNITS.find((x) => x.one === one)!;
  return { value: one, label: u.many };
});
type DurationUnit = keyof typeof MS_PER_UNIT;

type Patch = Partial<Record<keyof DeciderRule, unknown>>;

// What a change to the kinds leaves true: the second person and the owner follow the kinds, and the
// elevation-only fields go when elevation does.
function settle(rule: DeciderRule): DeciderRule {
  const next: DeciderRule = { ...rule };
  const second = secondHumanState(next);
  if (second === "hidden") delete next.second_human;
  else if (second === "forced" || second === "forced_download") next.second_human = true;
  const owner = ownerAllowed(next);
  if (!owner.allowed && owner.reason !== "second") next.who = next.who.filter((w) => w !== "owner");
  if (!next.kinds.includes("elevation")) {
    delete next.max_elevation;
    delete next.eligible_requesters;
    delete next.renew_after;
    delete next.ticket_pattern;
  }
  return next;
}

function withPatch(rule: DeciderRule, patch: Patch): DeciderRule {
  const next: Record<string, unknown> = { ...rule, ...patch };
  for (const k of Object.keys(next)) if (next[k] === undefined || next[k] === "") delete next[k];
  return settle(next as unknown as DeciderRule);
}

function initialDuration(go: string | undefined): { count: string; unit: DurationUnit } {
  const ms = go ? goDurationMs(go) : NaN;
  if (!(ms >= 0)) return { count: "", unit: "minute" };
  return ms > 0 && ms % MS_PER_UNIT.hour === 0 ? { count: String(ms / MS_PER_UNIT.hour), unit: "hour" } : { count: String(ms / MS_PER_UNIT.minute), unit: "minute" };
}

// A number box and unit beside each other, kept in the editor's own state so a half-typed number does not flip the unit.
function DurationRow({
  id,
  label,
  hint,
  error,
  value,
  onValue,
}: {
  id: string;
  label: string;
  hint: string;
  error: string | null;
  value: string | undefined;
  onValue: (go: string | undefined) => void;
}) {
  const [{ count, unit }, setState] = React.useState(() => initialDuration(value));
  const emit = (c: string, u: DurationUnit) => {
    const ms = Number(c) * MS_PER_UNIT[u];
    onValue(c === "" || !Number.isFinite(ms) || ms < 0 ? undefined : goDuration(ms));
  };
  return (
    <Field label={label} htmlFor={id} hint={hint} className="max-w-[28rem]">
      <DurationField
        id={id}
        count={count}
        unit={unit}
        units={DURATION_UNITS}
        onCount={(c) => {
          setState({ count: c, unit });
          emit(c, unit);
        }}
        onUnit={(u) => {
          setState({ count, unit: u as DurationUnit });
          emit(count, u as DurationUnit);
        }}
        unitLabel={RL.UNIT_PICKER_LABEL(label)}
        invalid={error !== null}
        inputClassName="max-w-[8rem] font-mono"
        selectClassName="w-[8rem]"
      />
      {error && <p className="mt-1 text-xs text-danger">{error}</p>}
    </Field>
  );
}

// A group name box with Add group, and the chips below it. `full` stops the add (eight entries).
function GroupAdder({ entries, full, onAdd, onRemove, placeholder }: { entries: string[]; full: boolean; onAdd: (entry: string) => void; onRemove: (entry: string) => void; placeholder: string }) {
  const [name, setName] = React.useState("");
  const add = () => {
    const entry = `group:${name.trim()}`;
    setName("");
    if (name.trim() && !entries.includes(entry)) onAdd(entry);
  };
  return (
    <div className="mt-2 flex flex-wrap items-center gap-2">
      {entries
        .filter((e) => e.startsWith("group:"))
        .map((e) => (
          <Chip key={e}>
            {DECIDERS.WHO_GROUP(e.slice("group:".length))}
            <button type="button" className="ml-1 rounded-sm outline-none focus-visible:ring-2 focus-visible:ring-ring" aria-label={`${DECIDERS.REMOVE} ${DECIDERS.WHO_GROUP(e.slice("group:".length))}`} onClick={() => onRemove(e)}>
              <X className="size-3" aria-hidden="true" />
            </button>
          </Chip>
        ))}
      <Input
        className="max-w-[16rem]"
        value={name}
        placeholder={placeholder}
        aria-label={placeholder}
        onChange={(e) => setName(e.target.value)}
        onKeyDown={(e) => {
          if (e.key === "Enter") {
            e.preventDefault();
            if (!full) add();
          }
        }}
      />
      <Button type="button" variant="outline" size="sm" disabled={full} onClick={add}>
        {DECIDERS.ADD_GROUP}
      </Button>
    </div>
  );
}

function CheckRow({ id, label, checked, disabled, reason, hint, onChange }: { id: string; label: string; checked: boolean; disabled?: boolean; reason?: string | null; hint?: string; onChange: (on: boolean) => void }) {
  return (
    <label htmlFor={id} className="flex items-start gap-2 text-xs text-foreground">
      <Checkbox id={id} className="mt-0.5" checked={checked} disabled={disabled} onCheckedChange={(v) => onChange(v === true)} />
      <span className="flex flex-col">
        <span>{label}</span>
        {hint && <span className="text-muted-foreground">{hint}</span>}
        {reason && <span className="text-muted-foreground">{reason}</span>}
      </span>
    </label>
  );
}

function SubHeading({ top, children }: { top?: boolean; children: React.ReactNode }) {
  return <p className={cn("text-sm font-medium text-foreground", top && "mt-4")}>{children}</p>;
}

export function DeciderRuleEditor({
  index,
  rule,
  draft,
  refusal,
  onChange,
  onDone,
  onRemove,
}: {
  index: number;
  rule: DeciderRule;
  /** Every rule of the draft, to see which kinds another rule holds. */
  draft: DeciderRule[];
  /** The refusal note for this rule, when the server named it. */
  refusal?: React.ReactNode;
  onChange: (rule: DeciderRule) => void;
  onDone: () => void;
  onRemove: () => void;
}) {
  const id = (s: string) => `decider-${index}-${s}`;
  const patch = (p: Patch) => onChange(withPatch(rule, p));
  const held = kindsHeldElsewhere(draft, index);
  const problems = ruleProblems(rule);
  const owner = ownerAllowed(rule);
  const second = secondHumanState(rule);
  const isElevation = rule.kinds.includes("elevation");
  const ownerReason = owner.allowed
    ? null
    : { cred: DECIDERS.OWNER_OFF_CRED, request: DECIDERS.OWNER_OFF_REQUEST, download: DECIDERS.OWNER_OFF_DOWNLOAD, second: DECIDERS.SECOND_WITH_OWNER }[owner.reason];
  const secondReason = { hidden: null, free: null, forced: DECIDERS.SECOND_FORCED, forced_download: DECIDERS.SECOND_FORCED_DOWNLOAD, blocked_by_owner: DECIDERS.SECOND_WITH_OWNER }[second];
  const setWho = (entry: string, on: boolean) => patch({ who: on ? [entry, ...rule.who] : rule.who.filter((w) => w !== entry) });
  const addEntry = (field: "who" | "eligible_requesters", entry: string) => patch({ [field]: [...(rule[field] ?? []), entry] });
  const dropEntry = (field: "who" | "eligible_requesters", entry: string) => patch({ [field]: (rule[field] ?? []).filter((w) => w !== entry) });

  return (
    <div
      role="group"
      data-testid={`decider-rule-editor-${index}`}
      aria-invalid={refusal ? true : undefined}
      className="rounded-lg border border-border bg-surface-2 p-4"
    >
      {refusal}

      <SubHeading>{DECIDERS.KINDS_LABEL}</SubHeading>
      <div className="mt-2 grid gap-2 sm:grid-cols-2 lg:grid-cols-3">
        {KINDS.map((k) => {
          const checked = rule.kinds.includes(k);
          const block = checked ? null : kindBlock(rule, k, held);
          const reason = block === null ? null : block.reason === "in_rule" ? DECIDERS.IN_RULE(block.n) : block.reason === "not_with" ? DECIDERS.NOT_WITH : DECIDERS.OWN_RULE;
          return (
            <CheckRow
              key={k}
              id={id(`kind-${k}`)}
              label={DECIDER_KIND_LABEL[k]}
              checked={checked}
              disabled={block !== null}
              reason={reason}
              onChange={(on) => onChange(settle(toggleKind(rule, k, on)))}
            />
          );
        })}
      </div>
      <p className="mt-2 text-xs text-muted-foreground">{DECIDERS.KINDS_HINT}</p>
      {isElevation && <p className="mt-1 text-xs text-muted-foreground">{DECIDERS.ELEVATION_OWN_RULE}</p>}

      <SubHeading top>{DECIDERS.DECIDED_BY}</SubHeading>
      <div className="mt-2 flex flex-col gap-2">
        <CheckRow
          id={id("owner")}
          label={DECIDERS.WHO_OWNER}
          checked={rule.who.includes("owner")}
          disabled={!owner.allowed}
          reason={ownerReason}
          hint={rule.kinds.includes("egress_domain") ? DECIDERS.OWNER_HINT_EGRESS : undefined}
          onChange={(on) => setWho("owner", on)}
        />
        <CheckRow id={id("admins")} label={DECIDERS.WHO_ADMINS} checked={rule.who.includes("unit_admins")} onChange={(on) => setWho("unit_admins", on)} />
      </div>
      <GroupAdder entries={rule.who} full={rule.who.length >= MAX_WHO} placeholder={DECIDERS.GROUP_PLACEHOLDER} onAdd={(e) => addEntry("who", e)} onRemove={(e) => dropEntry("who", e)} />
      <p className="mt-1 text-xs text-muted-foreground">{DECIDERS.COUNTER(rule.who.length)}</p>

      {second !== "hidden" && (
        <div className="mt-4">
          <CheckRow
            id={id("second")}
            label={DECIDERS.SECOND}
            checked={second !== "free" && second !== "blocked_by_owner" ? true : rule.second_human === true}
            disabled={second !== "free"}
            hint={DECIDERS.SECOND_HINT}
            reason={secondReason}
            onChange={(on) => patch({ second_human: on ? true : undefined })}
          />
        </div>
      )}

      {scopeRowShown(rule) && (
        <fieldset className="mt-4">
          <legend className="text-sm font-medium text-foreground">{DECIDERS.SCOPE_LABEL}</legend>
          <span className="mt-2 inline-flex overflow-hidden rounded-md border border-border text-xs">
            {SCOPES.map((s) => (
              <label key={s.label} className="cursor-pointer">
                <input type="radio" className="peer sr-only" name={id("scope")} checked={rule.max_scope === s.value} onChange={() => patch({ max_scope: s.value })} />
                <span className="block px-3 py-1.5 text-muted-foreground peer-checked:bg-primary peer-checked:text-primary-foreground peer-focus-visible:ring-2 peer-focus-visible:ring-ring">{s.label}</span>
              </label>
            ))}
          </span>
          <p className="mt-1 text-xs text-muted-foreground">{DECIDERS.ALWAYS_NEVER}</p>
        </fieldset>
      )}

      {isElevation && (
        <div className="mt-4 flex flex-col gap-4">
          <DurationRow
            id={id("max-elevation")}
            label={DECIDERS.ELEV_MAX_LABEL}
            hint={DECIDERS.ELEV_MAX_HINT}
            error={problems.includes("elev_max") ? DECIDERS.ELEV_MAX_ERROR : null}
            value={rule.max_elevation}
            onValue={(go) => patch({ max_elevation: go })}
          />
          <div>
            <SubHeading>{DECIDERS.ASK_LABEL}</SubHeading>
            <p className="mt-0.5 text-xs text-muted-foreground">{DECIDERS.ASK_HINT}</p>
            <GroupAdder
              entries={rule.eligible_requesters ?? []}
              full={(rule.eligible_requesters ?? []).length >= MAX_WHO}
              placeholder={DECIDERS.GROUP_PLACEHOLDER}
              onAdd={(e) => addEntry("eligible_requesters", e)}
              onRemove={(e) => dropEntry("eligible_requesters", e)}
            />
            {problems.includes("ask_required") && <p className="mt-1 text-xs text-danger">{DECIDERS.ASK_REQUIRED}</p>}
          </div>
          <DurationRow
            id={id("renew-after")}
            label={DECIDERS.RENEW_LABEL}
            hint={DECIDERS.RENEW_HINT}
            error={problems.includes("renew") ? DECIDERS.RENEW_ERROR : null}
            value={rule.renew_after}
            onValue={(go) => patch({ renew_after: go })}
          />
          <Field label={DECIDERS.TICKET_PATTERN_LABEL} htmlFor={id("ticket")} hint={DECIDERS.TICKET_PATTERN_HINT} className="max-w-[28rem]">
            <Input id={id("ticket")} className="font-mono" maxLength={256} value={rule.ticket_pattern ?? ""} onChange={(e) => patch({ ticket_pattern: e.target.value })} />
          </Field>
        </div>
      )}

      <div className="mt-4 flex items-center gap-3 border-t border-border pt-4">
        <Button type="button" size="sm" disabled={problems.length > 0 || rule.kinds.length === 0} onClick={onDone}>
          {DECIDERS.DONE}
        </Button>
        <Button type="button" variant="ghost" size="sm" onClick={onRemove}>
          {DECIDERS.REMOVE_RULE}
        </Button>
      </div>
    </div>
  );
}
