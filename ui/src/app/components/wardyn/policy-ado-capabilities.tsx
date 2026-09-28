/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The azure_devops_capabilities editor — a checklist beside the spec, the same
// shape as ToolRulesSection: it reads and writes the one document the textarea
// shows, so there is no second source of truth. Grouping, risk and labels come
// from lib/ado-capabilities.ts.
//
// DRAFT: layout and copy await the approved mock (mock-first law).
import type { RunPolicySpec } from "../../lib/types";
import { ADO_CAPABILITIES, ADO_CAPABILITY_GROUPS } from "../../lib/ado-capabilities";
import { Checkbox } from "../ui/checkbox";
import { Chip, SectionLabel } from "./primitives";
import { Mono } from "./code-block";

// Put the checked set back on the wire, in catalogue order. Nothing checked
// drops the key: absent and [] both mean "the provider row's default".
// A non-list value (parseSpec is a bare cast) reads as nothing checked.
export function withADOCapabilities(spec: RunPolicySpec, cap: string, on: boolean): RunPolicySpec {
  const current = new Set(Array.isArray(spec.azure_devops_capabilities) ? spec.azure_devops_capabilities : []);
  if (on) current.add(cap);
  else current.delete(cap);
  const next = { ...spec };
  const caps = ADO_CAPABILITIES.map((c) => c.cap).filter((c) => current.has(c));
  if (caps.length === 0) delete next.azure_devops_capabilities;
  else next.azure_devops_capabilities = caps;
  return next;
}

export function ADOCapabilitiesSection({
  spec,
  onSpecChange,
}: {
  spec: RunPolicySpec;
  onSpecChange: (next: RunPolicySpec) => void;
}) {
  const chosen = Array.isArray(spec.azure_devops_capabilities) ? spec.azure_devops_capabilities : [];
  return (
    <div className="rounded-lg border border-border p-3">
      <div className="mb-2 flex items-center gap-2">
        <SectionLabel>Azure DevOps capabilities</SectionLabel>
        <Chip tone="neutral" mono className="ml-auto">
          azure_devops_capabilities
        </Chip>
      </div>
      <p className="mb-3 text-xs leading-snug text-muted-foreground">
        What a run under this policy may do in Azure DevOps, in place of the provider&apos;s default.
        Only within the provider&apos;s ceiling: a run naming anything outside it is refused at launch.
        Leave all unchecked to keep the provider&apos;s default.
      </p>
      <div className="space-y-3">
        {ADO_CAPABILITY_GROUPS.map((g) => (
          <fieldset key={g.id}>
            <legend className="mb-1.5">
              <SectionLabel>{g.title}</SectionLabel>
            </legend>
            <ul className="grid gap-2 sm:grid-cols-2">
              {ADO_CAPABILITIES.filter((c) => c.group === g.id).map(({ cap, label, hint, highRisk }) => {
                const id = `ado-cap-${cap}`;
                return (
                  <li key={cap} className="flex items-start gap-2">
                    <Checkbox
                      id={id}
                      className="mt-0.5"
                      aria-describedby={hint ? `${id}-hint` : undefined}
                      checked={chosen.includes(cap)}
                      onCheckedChange={(v) => onSpecChange(withADOCapabilities(spec, cap, v === true))}
                    />
                    {/* The hint sits outside the label: inside it, "Rename, fork…"
                        joined the accessible name and answered a search for the
                        dialog's own "Name" field. */}
                    <div className="min-w-0 text-body leading-snug">
                      <label htmlFor={id}>
                        {label ?? <Mono>{cap}</Mono>}
                        {label && <span className="ml-1.5 font-mono text-meta text-muted-foreground">{cap}</span>}
                      </label>
                      {highRisk && (
                        <Chip tone="danger" className="ml-1.5">
                          High risk
                        </Chip>
                      )}
                      {hint && (
                        <span id={`${id}-hint`} className="block text-meta text-muted-foreground">
                          {hint}
                        </span>
                      )}
                    </div>
                  </li>
                );
              })}
            </ul>
          </fieldset>
        ))}
      </div>
    </div>
  );
}
