/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Add a tool rule: one tool named exactly, and what happens to it. The three
// effects are greyed per tool the person types, the way the server will refuse
// them, and Codex CLI takes MCP tools only.

import * as React from "react";
import type { ToolEffect } from "../../../../../lib/tool-effect";
import type { ToolRule } from "../../../../../lib/types/policy";
import type { ToolPolicyFact } from "../../../../../lib/types/new-run-access";
import { Button } from "../../../../ui/button";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "../../../../ui/dialog";
import { Input } from "../../../../ui/input";
import { Label } from "../../../../ui/label";
import { NR_SHARED } from "../../../../wardyn/copy/new-run/shared";
import { NR_TOOLS } from "../../../../wardyn/copy/new-run/tools";
import { EffectChoice } from "./effect-choice";
import { ruleChoices } from "./tool-facts";

export interface AddRuleDialogProps {
  fact: ToolPolicyFact;
  harnessName: string;
  /** The person's own rules already on this harness: a tool has at most one. */
  existing: readonly ToolRule[];
  onAdd: (rule: ToolRule) => void;
  onClose: () => void;
}

export function AddRuleDialog({ fact, harnessName, existing, onAdd, onClose }: AddRuleDialogProps) {
  const toolId = React.useId();
  const effectLabelId = React.useId();
  const [typed, setTyped] = React.useState("");
  const [picked, setPicked] = React.useState<ToolEffect>();
  const [submitted, setSubmitted] = React.useState(false);

  const tool = typed.trim();
  const choices = React.useMemo(() => ruleChoices(fact, harnessName, tool), [fact, harnessName, tool]);
  // What was picked stands only while it is still offered for this tool.
  const effect = choices.find((c) => c.effect === picked && c.available)?.effect ?? choices.find((c) => c.available)?.effect;
  const duplicate = existing.some((r) => r.tool === tool);
  const error = !submitted ? "" : !tool ? NR_TOOLS.RULE_ERR_TOOL : duplicate ? NR_TOOLS.RULE_ERR_DUPLICATE : "";
  // A hold on an MCP tool is answered by the harness, not by Wardyn (W-44).
  const notes = tool.startsWith("mcp__") ? { hold: NR_TOOLS.MCP_ASKS } : undefined;

  const add = () => {
    setSubmitted(true);
    if (tool && !duplicate && effect) onAdd({ tool, effect });
  };

  return (
    <Dialog open onOpenChange={(open) => !open && onClose()}>
      <DialogContent onOpenAutoFocus={(e) => { e.preventDefault(); document.getElementById(toolId)?.focus(); }}>
        <DialogHeader>
          <DialogTitle>{NR_TOOLS.RULE_TITLE(harnessName)}</DialogTitle>
          <DialogDescription>{NR_TOOLS.RULE_INTRO}</DialogDescription>
        </DialogHeader>
        <div className="flex flex-col gap-2">
          <Label htmlFor={toolId}>{NR_TOOLS.RULE_TOOL}</Label>
          <Input
            id={toolId}
            value={typed}
            placeholder={NR_TOOLS.RULE_PLACEHOLDER}
            autoComplete="off"
            spellCheck={false}
            aria-invalid={error ? true : undefined}
            aria-describedby={error ? `${toolId}-err` : undefined}
            onChange={(e) => { setTyped(e.target.value); setSubmitted(false); }}
            onKeyDown={(e) => { if (e.key === "Enter") add(); }}
          />
          {error && <p id={`${toolId}-err`} role="alert" className="text-sm text-danger">{error}</p>}
        </div>
        <div className="flex flex-col gap-2">
          <span id={effectLabelId} className="text-sm font-medium">{NR_TOOLS.RULE_EFFECT}</span>
          <EffectChoice name={`${toolId}-effect`} labelledBy={effectLabelId} value={effect} choices={choices} onChange={setPicked} notes={notes} />
        </div>
        <DialogFooter>
          <Button type="button" variant="ghost" onClick={onClose}>{NR_SHARED.CANCEL}</Button>
          <Button type="button" disabled={!effect} onClick={add}>{NR_TOOLS.RULE_CONFIRM}</Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
