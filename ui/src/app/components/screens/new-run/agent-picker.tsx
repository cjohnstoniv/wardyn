/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The New Run screen's Agent picker (§5c.4), split out of new-run-screen.tsx
// (the file-size gate) the way new-run-rail.tsx/workspace-card.tsx were.
//
// Options come from SetupStatus.harnesses. Roster-unknown (harnesses
// undefined — the fetch never landed, or failed) falls back to WIZARD_AGENTS
// and marks nothing unavailable: "unknown stays unknown — never claim a
// missing model path on a blip." Once the roster arrives, every catalog row
// renders — a disabled one WITH AGENTS.UNAVAILABLE as its reason, never hidden.
//
// WIZARD_AGENTS, not a second pair of literals: the fallback listed
// claude-code/codex-cli only, so a cloned BYOA run (`agent: "none"`, label
// "Your own tools") painted an EMPTY trigger — Radix has no item to read the
// selected value from — and no way back to the value the run would actually
// launch with. For the same reason the CURRENT value is appended whenever the
// options don't already carry it, which also covers a roster that omits it.
import type { SetupHarnessTool } from "../../../lib/types";
import { AGENTS } from "../../../lib/workspace-providers-copy";
import { Field } from "../../wardyn/form-primitives";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "../../ui/select";
import { agentLabel, WIZARD_AGENTS, type WizardAgent } from "./wizard-types";

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
    : WIZARD_AGENTS.map((id) => ({ id, label: agentLabel(id) }));
  // The selected value ALWAYS has an item: without one the trigger renders
  // nothing at all, which is the screen claiming no agent for the agent the run
  // will use. Never disabled — this is the run's own value, not an offer.
  if (!options.some((o) => o.id === value)) options.push({ id: value, label: agentLabel(value) });

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
