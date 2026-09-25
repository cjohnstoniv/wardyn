/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { Plus, Trash2 } from "lucide-react";
import type { RunPolicySpec, ToolEffect, ToolRule } from "../../lib/types";
import { TOOL_EFFECTS, TOOL_RULE_DEFAULT, toolRulesProblem } from "../../lib/types";
import { Button } from "../ui/button";
import { Input } from "../ui/input";
import { cn } from "../ui/utils";
import { Chip, SectionLabel } from "./primitives";

/* ---------- tool rules ---------- */

// The wire words stay mono and verbatim (allow/hold/deny); these are their past
// tense, and only the rail's one-line summary uses them.
export const EFFECT_PAST: Record<ToolEffect, string> = { allow: "allowed", hold: "held", deny: "denied" };

// The effect's semantic tint. Not decoration: an effect is a run/approval state
// word, so it takes the state palette (CONSOLE-RULES §2) and never a metal.
const EFFECT_TONE: Record<ToolEffect, string> = {
  allow: "text-success",
  hold: "text-warning",
  deny: "text-danger",
};

// Split the flat wire list into what the editor renders as two different things:
// the named rules (addable, removable) and the "*" default (neither).
//
// An absent "*" is `hold`: ToolEffectFor returns ok=false for an unmatched tool,
// and the caller then raises a human approval — which is exactly what hold does.
// So the default row can state `hold` honestly whether or not the document
// spells the rule out.
//
// A non-list `tool_rules` reads as no rules: parseSpec is a bare cast, so the
// textarea can hand this a string or an object, and the section has to render
// the refusal toolRulesProblem returns for it — throwing here would hit the
// route's ErrorBoundary and take the operator's draft with it.
export function splitToolRules(rules: readonly ToolRule[] | undefined): {
  named: ToolRule[];
  defaultEffect: ToolEffect;
  explicitDefault: boolean;
} {
  const all = Array.isArray(rules) ? rules : [];
  const fallback = all.find((r) => r.tool === TOOL_RULE_DEFAULT);
  return {
    named: all.filter((r) => r.tool !== TOOL_RULE_DEFAULT),
    defaultEffect: fallback?.effect ?? "hold",
    explicitDefault: !!fallback,
  };
}

// Put the two halves back on the wire.
//
// The "*" rule is written when the document already spelled it out, or when the
// default says something other than `hold` — an implicit hold and an explicit
// one behave identically, so emitting it into every policy would add a no-op
// line to documents that never asked for one. An empty result drops the key
// entirely: `tool_rules: []` and no key at all mean the same thing, and the
// shorter one is what a policy written before this field looks like.
function withToolRules(
  spec: RunPolicySpec,
  named: readonly ToolRule[],
  defaultEffect: ToolEffect,
  keepDefault: boolean,
): RunPolicySpec {
  const rules = keepDefault
    ? [...named, { tool: TOOL_RULE_DEFAULT, effect: defaultEffect }]
    : [...named];
  const next = { ...spec };
  if (rules.length === 0) delete next.tool_rules;
  else next.tool_rules = rules;
  return next;
}

// The effect picker. A native <select>: three fixed wire words, one per row, and
// the current value has to carry its own tint — the same reason the Title field
// takes a native <datalist> rather than a combobox library.
function EffectSelect({
  value,
  label,
  onChange,
}: {
  value: ToolEffect;
  label: string;
  onChange: (e: ToolEffect) => void;
}) {
  return (
    <select
      aria-label={label}
      value={value}
      onChange={(e) => onChange(e.target.value as ToolEffect)}
      className={cn(
        "border-input bg-input-background focus-visible:border-ring focus-visible:ring-ring dark:bg-input/30",
        "h-8 w-full rounded-md border px-2 font-mono text-body font-medium outline-none focus-visible:ring-[3px]",
        EFFECT_TONE[value],
      )}
    >
      {TOOL_EFFECTS.map((e) => (
        <option key={e} value={e} className="text-foreground">
          {e}
        </option>
      ))}
    </select>
  );
}

const RULE_GRID = "grid grid-cols-[minmax(0,1fr)_9rem_2rem] items-center gap-2.5";

// The tool_rules editor — a section beside the spec, not a second JSON blob.
// It reads and writes the SAME document the textarea shows (every edit
// re-serialises it), so there is one source of truth and no sync to get wrong.
export function ToolRulesSection({
  spec,
  onSpecChange,
}: {
  spec: RunPolicySpec;
  onSpecChange: (next: RunPolicySpec) => void;
}) {
  const { named, defaultEffect, explicitDefault } = splitToolRules(spec.tool_rules);
  // Mirrors the server's own refusals so the operator sees them here rather
  // than as a 400 after Save. The server is still the gate — this is advisory,
  // exactly like the JSON parse above it.
  const problem = toolRulesProblem(spec.tool_rules ?? []);
  const write = (nextNamed: ToolRule[], nextDefault: ToolEffect) =>
    onSpecChange(
      withToolRules(spec, nextNamed, nextDefault, explicitDefault || nextDefault !== "hold"),
    );
  const patchNamed = (i: number, patch: Partial<ToolRule>) =>
    write(
      named.map((r, n) => (n === i ? { ...r, ...patch } : r)),
      defaultEffect,
    );

  return (
    <div className="rounded-lg border border-border p-3">
      <div className="mb-2 flex items-center gap-2">
        <SectionLabel>Tool rules</SectionLabel>
        <Chip tone="neutral" mono className="ml-auto">
          tool_rules
        </Chip>
      </div>
      <p className="mb-3 text-xs leading-snug text-muted-foreground">
        Consulted only for a run whose tool approvals are held. A rule narrows what a human is
        asked about — it never widens what the agent may do. Evaluated outside the sandbox, so the
        agent cannot rewrite the rules that govern it.
      </p>

      <div className={cn(RULE_GRID, "border-b border-border pb-1.5")}>
        <SectionLabel>Tool</SectionLabel>
        <SectionLabel>Effect</SectionLabel>
        <span />
      </div>

      {named.map((rule, i) => (
        <div key={i} className={cn(RULE_GRID, "border-b border-border py-2")}>
          {/* No maxLength: the cap is 64 bytes (Go's len) and the attribute
              counts UTF-16 units, so it cannot express this one — it would
              silently swallow the 65th keystroke of a legal ASCII name while
              still letting a 40-character CJK name (120 bytes) through. The
              refusal below is the honest surface for the cap. */}
          <Input
            aria-label={`Tool ${i + 1}`}
            value={rule.tool}
            spellCheck={false}
            placeholder="Bash"
            onChange={(e) => patchNamed(i, { tool: e.target.value })}
            className="h-8 font-mono text-body"
          />
          <EffectSelect
            value={rule.effect}
            label={`Effect ${i + 1}`}
            onChange={(effect) => patchNamed(i, { effect })}
          />
          <Button
            type="button"
            variant="ghost"
            size="icon"
            className="size-8"
            aria-label={`Remove rule ${i + 1}`}
            onClick={() =>
              write(
                named.filter((_, n) => n !== i),
                defaultEffect,
              )
            }
          >
            <Trash2 className="size-4" />
          </Button>
        </div>
      ))}

      {/* The default row: not removable, and it is what keeps an added rule
          from ever widening — everything unlisted still stops for a human
          unless the operator deliberately says otherwise. */}
      <div className={cn(RULE_GRID, "py-2")}>
        <div className="flex h-8 items-center rounded-md border border-dashed border-input bg-muted px-3 font-mono text-body text-muted-foreground">
          {TOOL_RULE_DEFAULT}
        </div>
        <EffectSelect value={defaultEffect} label="Default effect" onChange={(e) => write(named, e)} />
        <span />
      </div>
      <p className="text-xs leading-snug text-muted-foreground">
        The effect for any tool no rule names. It cannot be removed — leave it on{" "}
        <span className="font-mono text-foreground">hold</span> to keep today&apos;s behaviour for
        everything unlisted.
      </p>

      <Button
        type="button"
        variant="ghost"
        size="sm"
        className="mt-2 px-1"
        onClick={() => write([...named, { tool: "", effect: "hold" }], defaultEffect)}
      >
        <Plus className="size-4" /> Add rule
      </Button>

      {problem && (
        <p role="alert" className="mt-1 text-xs text-danger">
          {problem}
        </p>
      )}
    </div>
  );
}
