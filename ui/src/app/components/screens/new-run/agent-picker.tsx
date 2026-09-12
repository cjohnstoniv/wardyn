/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The New Run screen's Agent picker (§5c.4), split out of new-run-screen.tsx
// (the file-size gate) the way new-run-rail.tsx/workspace-card.tsx were.
//
// Options come from SetupStatus.harnesses. Roster-unknown (harnesses
// undefined — the fetch never landed, or failed) keeps today's two catalog
// literals and marks nothing unavailable: "unknown stays unknown — never
// claim a missing model path on a blip." Once the roster arrives, every
// catalog row renders — a disabled one WITH AGENTS.UNAVAILABLE as its reason,
// never hidden.
import type { SetupHarnessTool } from "../../../lib/types";
import { AGENTS } from "../../../lib/workspace-providers-copy";
import { Field } from "../../wardyn/form-primitives";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "../../ui/select";
import type { WizardAgent } from "./wizard-types";

export function AgentPicker({
  value,
  harnesses,
  onChange,
}: {
  value: WizardAgent;
  harnesses: SetupHarnessTool[] | undefined;
  onChange: (agent: WizardAgent) => void;
}) {
  const options: { id: string; label: string; disabled?: boolean }[] = harnesses
    ? harnesses.map((h) => ({ id: h.id, label: h.display, disabled: h.enabled === false }))
    : [
        { id: "claude-code", label: "Claude Code" },
        { id: "codex-cli", label: "Codex CLI" },
      ];

  return (
    <Field label="Agent" htmlFor="nr-agent">
      <Select value={value} onValueChange={(v) => onChange(v as WizardAgent)}>
        <SelectTrigger id="nr-agent">
          <SelectValue />
        </SelectTrigger>
        <SelectContent>
          {options.map((o) => (
            <SelectItem key={o.id} value={o.id} disabled={o.disabled}>
              {o.label}
              {o.disabled && <span className="text-muted-foreground"> — {AGENTS.UNAVAILABLE}</span>}
            </SelectItem>
          ))}
        </SelectContent>
      </Select>
    </Field>
  );
}
