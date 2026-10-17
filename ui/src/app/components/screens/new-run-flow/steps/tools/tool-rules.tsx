/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// A harness's tool rules: the policy's rows, locked, then the person's own.

import * as React from "react";
import { Lock } from "lucide-react";
import type { ToolRule } from "../../../../../lib/types/policy";
import type { ToolPolicyFact } from "../../../../../lib/types/new-run-access";
import { Button } from "../../../../ui/button";
import { NR_TOOLS } from "../../../../wardyn/copy/new-run/tools";
import { Chip } from "../../../../wardyn/primitives";
import { AddRuleDialog } from "./add-rule-dialog";
import { EFFECT_LABEL } from "./effect-choice";
import type { ToolEffect } from "../../../../../lib/tool-effect";

const ROW_CLASS = "flex flex-wrap items-center gap-2 rounded-lg border border-border bg-card px-2 py-1.5";
const EFFECT_TONE: Record<ToolEffect, "success" | "info" | "danger"> = { allow: "success", hold: "info", deny: "danger" };

export interface ToolRulesProps {
  fact: ToolPolicyFact;
  harnessName: string;
  personal: readonly ToolRule[];
  onAdd: (rule: ToolRule) => void;
  onRemove: (tool: string) => void;
}

export function ToolRules({ fact, harnessName, personal, onAdd, onRemove }: ToolRulesProps) {
  const headingId = React.useId();
  const [adding, setAdding] = React.useState(false);
  const label = (tool: string) => (tool === "*" ? NR_TOOLS.DEFAULT_LABEL : tool);
  return (
    <div className="flex flex-col gap-2">
      <h4 id={headingId} className="text-sm font-medium">{NR_TOOLS.RULES_LABEL}</h4>
      {(fact.policy_rules.length > 0 || personal.length > 0) && (
        <ul aria-labelledby={headingId} className="flex flex-col gap-1.5 text-sm">
          {fact.policy_rules.map((r) => (
            <li key={`policy:${r.tool}`} data-locked="" className={ROW_CLASS}>
              <Lock aria-hidden className="size-3.5 text-muted-foreground" />
              <span className="sr-only">{NR_TOOLS.ROW_LOCKED}</span>
              <span className="min-w-0 flex-1 font-mono [overflow-wrap:anywhere]">{label(r.tool)}</span>
              <Chip tone={EFFECT_TONE[r.effect as ToolEffect] ?? "neutral"}>{EFFECT_LABEL[r.effect as ToolEffect] ?? r.effect}</Chip>
            </li>
          ))}
          {personal.map((r) => (
            <li key={`mine:${r.tool}`} className={ROW_CLASS}>
              <span className="min-w-0 flex-1 font-mono [overflow-wrap:anywhere]">{r.tool}</span>
              {r.effect === "hold" && r.tool.startsWith("mcp__") && <span className="text-muted-foreground">{NR_TOOLS.MCP_ASKS}</span>}
              <Chip tone={EFFECT_TONE[r.effect]}>{EFFECT_LABEL[r.effect]}</Chip>
              <Button type="button" variant="ghost" size="sm" aria-label={`${NR_TOOLS.ROW_REMOVE} ${r.tool}`} onClick={() => onRemove(r.tool)}>
                {NR_TOOLS.ROW_REMOVE}
              </Button>
            </li>
          ))}
        </ul>
      )}
      <Button type="button" variant="outline" size="sm" className="w-fit" onClick={() => setAdding(true)}>{NR_TOOLS.ADD_RULE}</Button>
      {adding && (
        <AddRuleDialog
          fact={fact}
          harnessName={harnessName}
          existing={personal}
          onAdd={(rule) => { onAdd(rule); setAdding(false); }}
          onClose={() => setAdding(false)}
        />
      )}
    </div>
  );
}
