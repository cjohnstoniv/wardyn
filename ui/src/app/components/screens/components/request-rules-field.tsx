/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The per-component request rules, for both editors (the organisation's and a person's own). A rule narrows
// what a run's request to one of the component's exact hosts may do; the first rule that matches decides.
// While the deployment has request rules off, a component that still holds rules shows a count and one
// Remove action, and a component without rules shows nothing: the server accepts only a save that removes.
import * as React from "react";
import { ArrowDown, ArrowUp, CircleAlert, Plus, X } from "lucide-react";
import type { HostRule, HostRuleEffect } from "../../../lib/types";
import { Button } from "../../ui/button";
import { Input } from "../../ui/input";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "../../ui/select";
import { Field } from "../../wardyn/form-primitives";
import { Chip } from "../../wardyn/primitives";
import { Segmented } from "../../wardyn/segmented";
import { MAX_REQUEST_RULES, type RuleRefusal } from "./component-editor-model";

export const REQUEST_RULES = {
  LABEL: "Request rules",
  HINT: "A request to a host that has rules is refused unless a rule allows it. Rules are checked top to bottom, and the first that matches decides. Rules only narrow: they never allow a request the run's policy refuses.",
  FORMAT_HINT: "Separate methods and paths with spaces. A path matches itself and everything below it.",
  EMPTY: "No rules. Requests to the hosts above are not narrowed by this component.",
  ADD: "Add a rule",
  COUNT: (n: number) => `${n} of ${MAX_REQUEST_RULES} rules`,
  AT_LIMIT: `${MAX_REQUEST_RULES} of ${MAX_REQUEST_RULES} rules. Remove one to add another.`,
  NO_HOST: "Rules go on a host named exactly above, with no port or port 443. Add one first.",
  RULE: (n: number) => `Rule ${n}`,
  REFUSED: "Refused. The reason is below the rules.",
  HOST: "Host",
  HOST_PLACEHOLDER: "Choose a host",
  EFFECT: "Effect",
  ALLOW: "Allow",
  HOLD: "Hold",
  DENY: "Deny",
  METHODS: "Methods",
  METHODS_PLACEHOLDER: "Any method",
  PATHS: "Paths",
  PATHS_PLACEHOLDER: "Any path",
  HOLD_HINT: "Hold waits for a person to decide, and their answer stands for the rest of the run. Refused if nobody decides in time.",
  HOST_OF: (n: number) => `Host for rule ${n}`,
  EFFECT_OF: (n: number) => `Effect for rule ${n}`,
  METHODS_OF: (n: number) => `Methods for rule ${n}`,
  PATHS_OF: (n: number) => `Paths for rule ${n}`,
  UP: (n: number) => `Move rule ${n} up`,
  DOWN: (n: number) => `Move rule ${n} down`,
  REMOVE: (n: number) => `Remove rule ${n}`,
  OFF_COUNT: (n: number) => (n === 1 ? "1 rule" : `${n} rules`),
  OFF_CHIP: "Off",
  OFF_SENTENCE:
    "Request rules are off on this deployment, so a run that uses this component is refused while it has rules. Remove them to use the component again.",
  REMOVE_ALL: "Remove rules",
  REMOVED: "Rules removed. Save the component to apply.",
} as const;

const EFFECTS: { value: HostRuleEffect; label: string }[] = [
  { value: "allow", label: REQUEST_RULES.ALLOW },
  { value: "hold", label: REQUEST_RULES.HOLD },
  { value: "deny", label: REQUEST_RULES.DENY },
];

/** The DOM id of a rule's control, so a refused form can focus the one a sentence names. */
export const ruleDomId = (prefix: string, index: number, part: NonNullable<RuleRefusal["part"]>): string => `${prefix}-${index}-${part}`;

interface Row {
  // Stable React key; never sent.
  key: number;
  host: string;
  effect: HostRuleEffect;
  // The text as typed, so a space the person has just typed is not swallowed by the list it makes.
  methods: string;
  paths: string;
}

let nextRowKey = 1;

const words = (text: string): string[] => text.split(/\s+/).filter(Boolean);
const rowOf = (r: HostRule): Row => ({ key: nextRowKey++, host: r.host, effect: r.effect, methods: (r.methods ?? []).join(" "), paths: (r.paths ?? []).join(" ") });
const ruleOf = (r: Row): HostRule => {
  const methods = words(r.methods);
  const paths = words(r.paths);
  return { host: r.host, ...(methods.length > 0 && { methods }), ...(paths.length > 0 && { paths }), effect: r.effect };
};

function RuleCard({
  n,
  row,
  hosts,
  count,
  idPrefix,
  failing,
  errorId,
  onChange,
  onMove,
  onRemove,
}: {
  n: number;
  row: Row;
  hosts: string[];
  count: number;
  idPrefix: string;
  failing: RuleRefusal | null;
  errorId: string;
  onChange: (patch: Partial<Row>) => void;
  onMove: (by: -1 | 1) => void;
  onRemove: () => void;
}) {
  const i = n - 1;
  // A host taken out of Hosts after the rule was written stays shown, so the refusal can name it.
  const choices = row.host && !hosts.includes(row.host) ? [...hosts, row.host] : hosts;
  const invalid = (part: NonNullable<RuleRefusal["part"]>) => (failing?.part === part ? ({ "aria-invalid": true, "aria-describedby": errorId } as const) : {});
  return (
    <div
      role="group"
      aria-label={REQUEST_RULES.RULE(n)}
      className={`min-w-0 space-y-3 rounded-lg border p-3 ${failing ? "border-danger/60" : "border-border"}`}
      data-testid="request-rule"
    >
      <div className="flex items-center gap-2 text-sm font-medium text-foreground">
        <span>{REQUEST_RULES.RULE(n)}</span>
        {failing && (
          <span className="flex items-center gap-1 text-xs font-normal text-danger">
            <CircleAlert className="size-3.5 shrink-0" aria-hidden="true" />
            {REQUEST_RULES.REFUSED}
          </span>
        )}
      </div>
      <div className="grid gap-3 sm:grid-cols-2">
        <Field label={REQUEST_RULES.HOST} htmlFor={ruleDomId(idPrefix, i, "host")} className="min-w-0">
          <Select value={row.host} onValueChange={(host) => onChange({ host })}>
            <SelectTrigger id={ruleDomId(idPrefix, i, "host")} aria-label={REQUEST_RULES.HOST_OF(n)} {...invalid("host")}>
              <SelectValue placeholder={REQUEST_RULES.HOST_PLACEHOLDER} />
            </SelectTrigger>
            <SelectContent>
              {choices.map((h) => (
                <SelectItem key={h} value={h}>
                  {h}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        </Field>
        <Field label={REQUEST_RULES.EFFECT} className="@container min-w-0">
          <div className="flex items-center gap-2">
            <div role="group" id={ruleDomId(idPrefix, i, "effect")} tabIndex={-1} aria-label={REQUEST_RULES.EFFECT_OF(n)} className="outline-none" {...invalid("effect")}>
              <Segmented value={row.effect} options={EFFECTS} onChange={(effect) => onChange({ effect })} />
            </div>
            <Button type="button" variant="ghost" size="icon" aria-label={REQUEST_RULES.UP(n)} disabled={i === 0} onClick={() => onMove(-1)}>
              <ArrowUp className="size-4" />
            </Button>
            <Button type="button" variant="ghost" size="icon" aria-label={REQUEST_RULES.DOWN(n)} disabled={i === count - 1} onClick={() => onMove(1)}>
              <ArrowDown className="size-4" />
            </Button>
            <Button type="button" variant="ghost" size="icon" aria-label={REQUEST_RULES.REMOVE(n)} onClick={onRemove}>
              <X className="size-4" />
            </Button>
          </div>
        </Field>
      </div>
      <div className="grid gap-3 sm:grid-cols-2">
        <Field label={REQUEST_RULES.METHODS} htmlFor={ruleDomId(idPrefix, i, "methods")} className="min-w-0">
          <Input
            id={ruleDomId(idPrefix, i, "methods")}
            aria-label={REQUEST_RULES.METHODS_OF(n)}
            value={row.methods}
            placeholder={REQUEST_RULES.METHODS_PLACEHOLDER}
            onChange={(e) => onChange({ methods: e.target.value.toUpperCase() })}
            className="font-mono"
            autoComplete="off"
            spellCheck={false}
            {...invalid("methods")}
          />
        </Field>
        <Field label={REQUEST_RULES.PATHS} htmlFor={ruleDomId(idPrefix, i, "paths")} className="min-w-0">
          <Input
            id={ruleDomId(idPrefix, i, "paths")}
            aria-label={REQUEST_RULES.PATHS_OF(n)}
            value={row.paths}
            placeholder={REQUEST_RULES.PATHS_PLACEHOLDER}
            onChange={(e) => onChange({ paths: e.target.value })}
            className="font-mono"
            autoComplete="off"
            spellCheck={false}
            {...invalid("paths")}
          />
        </Field>
      </div>
      {row.effect === "hold" && <p className="text-xs leading-snug text-muted-foreground">{REQUEST_RULES.HOLD_HINT}</p>}
    </div>
  );
}

function Refusal({ id, text }: { id: string; text: string | null }) {
  if (!text) return null;
  return (
    <p id={id} role="alert" tabIndex={-1} className="text-xs leading-snug text-danger outline-none">
      {text}
    </p>
  );
}

function RuleEditor({
  rules,
  hosts,
  onChange,
  idPrefix,
  refusal,
  failing,
  disabled,
}: {
  rules: HostRule[];
  hosts: string[];
  onChange: (rules: HostRule[]) => void;
  idPrefix: string;
  refusal: string | null;
  failing: RuleRefusal | null;
  disabled?: boolean;
}) {
  const [rows, setRows] = React.useState<Row[]>(() => rules.map(rowOf));
  const errorId = `${idPrefix}-error`;
  // An edit moves or changes the rule a sentence named, so its mark goes; the sentence stays until the next save.
  const [cleared, setCleared] = React.useState<string | null>(null);
  React.useEffect(() => {
    if (!refusal) setCleared(null);
  }, [refusal]);
  const marked = cleared === refusal ? null : failing;
  const emit = (next: Row[]) => {
    setCleared(refusal);
    setRows(next);
    onChange(next.map(ruleOf));
  };
  const patch = (key: number, p: Partial<Row>) => emit(rows.map((r) => (r.key === key ? { ...r, ...p } : r)));
  const move = (at: number, by: -1 | 1) => {
    const next = [...rows];
    [next[at], next[at + by]] = [next[at + by], next[at]];
    emit(next);
  };
  const full = rows.length >= MAX_REQUEST_RULES;
  const noHost = hosts.length === 0;
  const addNote = full ? REQUEST_RULES.AT_LIMIT : noHost ? REQUEST_RULES.NO_HOST : REQUEST_RULES.COUNT(rows.length);
  return (
    <fieldset disabled={disabled} className="min-w-0 space-y-3">
      <legend className="text-sm font-medium text-foreground">{REQUEST_RULES.LABEL}</legend>
      <p className="text-xs text-muted-foreground">{REQUEST_RULES.HINT}</p>
      <p className="text-xs text-muted-foreground">{REQUEST_RULES.FORMAT_HINT}</p>
      {rows.map((row, i) => (
        <RuleCard
          key={row.key}
          n={i + 1}
          row={row}
          hosts={hosts}
          count={rows.length}
          idPrefix={idPrefix}
          failing={marked?.index === i ? marked : null}
          errorId={errorId}
          onChange={(p) => patch(row.key, p)}
          onMove={(by) => move(i, by)}
          onRemove={() => emit(rows.filter((r) => r.key !== row.key))}
        />
      ))}
      {rows.length === 0 && !noHost && <p className="text-xs text-muted-foreground">{REQUEST_RULES.EMPTY}</p>}
      <div className="flex flex-wrap items-center gap-3">
        <Button
          type="button"
          variant="outline"
          size="sm"
          disabled={full || noHost}
          onClick={() => emit([...rows, { key: nextRowKey++, host: "", effect: "allow", methods: "", paths: "" }])}
        >
          <Plus className="size-3.5" />
          {REQUEST_RULES.ADD}
        </Button>
        <span className="text-xs text-muted-foreground">{addNote}</span>
      </div>
      <Refusal id={errorId} text={refusal} />
    </fieldset>
  );
}

export function RequestRulesField({
  on,
  rules,
  hadRules,
  hosts,
  onChange,
  idPrefix,
  refusal = null,
  failing = null,
  disabled,
}: {
  /** The deployment has request rules on (the setup status's features.component_request_rules); null until the status has answered. */
  on: boolean | null;
  /** The draft's rules. */
  rules: HostRule[];
  /** The stored component carries rules: with the switch off, emptying them is still shown as done. */
  hadRules: boolean;
  /** The component's exact hosts a rule may name, as the Hosts field stands. */
  hosts: string[];
  onChange: (rules: HostRule[]) => void;
  /** Prefix of the ids the field's controls and its sentence take. */
  idPrefix: string;
  /** The server's sentence about the rules, shown under the field. */
  refusal?: string | null;
  /** The rule and part the sentence names. */
  failing?: RuleRefusal | null;
  /** A save is in flight. */
  disabled?: boolean;
}) {
  const errorId = `${idPrefix}-error`;
  // The draft still carries the stored rules, so a save before the answer sends them unchanged.
  if (on === null) return <Refusal id={errorId} text={refusal} />;
  if (on) return <RuleEditor rules={rules} hosts={hosts} onChange={onChange} idPrefix={idPrefix} refusal={refusal} failing={failing} disabled={disabled} />;
  if (rules.length === 0 && !hadRules) return <Refusal id={errorId} text={refusal} />;
  return (
    <fieldset disabled={disabled} className="min-w-0 space-y-2">
      <legend className="text-sm font-medium text-foreground">{REQUEST_RULES.LABEL}</legend>
      {rules.length === 0 ? (
        <p className="text-xs text-muted-foreground">{REQUEST_RULES.REMOVED}</p>
      ) : (
        <div className="space-y-2 rounded-lg border border-border p-3">
          <div className="flex items-center gap-2 text-sm text-foreground">
            <span>{REQUEST_RULES.OFF_COUNT(rules.length)}</span>
            <Chip tone="warning">{REQUEST_RULES.OFF_CHIP}</Chip>
          </div>
          <p className="text-xs leading-snug text-muted-foreground">{REQUEST_RULES.OFF_SENTENCE}</p>
          <Button type="button" variant="outline" size="sm" onClick={() => onChange([])}>
            {REQUEST_RULES.REMOVE_ALL}
          </Button>
        </div>
      )}
      <Refusal id={errorId} text={refusal} />
    </fieldset>
  );
}
