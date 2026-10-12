/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// A harness's tool rules: the policy's rows, locked, then the person's own. A
// locked row says it is enforced only when the run's lane consults it.

import * as React from "react";
import { Lock } from "lucide-react";
import type { ToolRule } from "../../../../../lib/types/policy";
import type { ToolPolicyFact } from "../../../../../lib/types/new-run-access";
import { Button } from "../../../../ui/button";
import { NR_TOOLS } from "../../../../wardyn/copy/new-run/tools";
import { AddRuleDialog } from "./add-rule-dialog";
import { EFFECT_LABEL } from "./effect-choice";
import { lockedRowEnforced, type LockedRow } from "./tool-facts";
import type { ToolEffect } from "../../../../../lib/tool-effect";

// OWNER-WORDING W-43: the brief's own words until the owner answers; not yet in the canon.
const NOT_ENFORCED = "Not enforced on this run";

export interface ToolRulesProps {
  fact: ToolPolicyFact;
  harnessName: string;
  personal: readonly ToolRule[];
  /** The server has answered exactly this body. Until it has, no row claims to be enforced or not. */
  answered: boolean;
  onAdd: (rule: ToolRule) => void;
  onRemove: (tool: string) => void;
}

export function ToolRules({ fact, harnessName, personal, answered, onAdd, onRemove }: ToolRulesProps) {
  const headingId = React.useId();
  const [adding, setAdding] = React.useState(false);
  const label = (tool: string) => (tool === "*" ? NR_TOOLS.DEFAULT_LABEL : tool);
  return (
    <div className="flex flex-col gap-2">
      <h4 id={headingId} className="text-sm font-medium">{NR_TOOLS.RULES_LABEL}</h4>
      {(fact.policy_rules.length > 0 || personal.length > 0) && (
        <ul aria-labelledby={headingId} className="flex flex-col gap-1 text-sm">
          {fact.policy_rules.map((r) => (
            <li key={`policy:${r.tool}`} data-locked="" className="flex flex-wrap items-center gap-2">
              <Lock aria-hidden className="size-3.5 text-muted-foreground" />
              <span className="font-mono [overflow-wrap:anywhere]">{label(r.tool)}</span>
              <span>{EFFECT_LABEL[r.effect as ToolEffect] ?? r.effect}</span>
              {!answered ? (
                <span className="text-muted-foreground">{NR_TOOLS.NET_REQUESTED}</span>
              ) : (
                !lockedRowEnforced(fact, r as LockedRow) && <span className="text-muted-foreground">{NOT_ENFORCED}</span>
              )}
            </li>
          ))}
          {personal.map((r) => (
            <li key={`mine:${r.tool}`} className="flex flex-wrap items-center gap-2">
              <span className="font-mono [overflow-wrap:anywhere]">{r.tool}</span>
              <span>{EFFECT_LABEL[r.effect]}</span>
              {r.effect === "hold" && r.tool.startsWith("mcp__") && <span className="text-muted-foreground">{NR_TOOLS.MCP_ASKS}</span>}
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
