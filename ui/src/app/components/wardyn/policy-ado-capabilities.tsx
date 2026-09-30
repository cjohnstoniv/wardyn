/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The policy editor's "Azure DevOps access" section — azure_devops_capabilities
// as a grouped checklist beside the spec, the same shape as ToolRulesSection: it
// reads and writes the one document the textarea shows, so there is no second
// source of truth. Layout and copy are the owner-approved mocks' (mock-08,
// Member · State 4; the per-area packet, Member · State 3): one group per Azure
// DevOps area, and a High-risk row sits inside its area with the badge and a
// red left edge.
//
// A capability off the row's ceiling renders LOCKED — a red crossed box and a
// struck-through name — because a policy naming it is refused at launch. When
// the ceiling is unknown (no Azure DevOps row, or /setup/status unreadable)
// nothing is locked: dispatch is still the gate.
import type { RunPolicySpec } from "../../lib/types";
import { ADO_CAPABILITIES, ADO_CAPABILITY_GROUPS, type ADOCapabilityInfo } from "../../lib/ado-capabilities";
import { ADO_ACCESS, adoCapName, adoGroupName } from "../../lib/ado-access-copy";
import { Checkbox } from "../ui/checkbox";
// clsx, not cn: tailwind-merge doesn't know the text-meta/text-body size
// tokens, so clsx() drops them whenever a text colour class is merged in.
import { clsx } from "clsx";
import { SectionLabel } from "./primitives";
import { HighRiskBadge } from "./ado-access-summary";

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

function CapabilityRow({
  info,
  checked,
  locked,
  onToggle,
}: {
  info: ADOCapabilityInfo;
  checked: boolean;
  locked: boolean;
  onToggle: (on: boolean) => void;
}) {
  const id = `ado-cap-${info.cap}`;
  const name = adoCapName(info.cap);
  // A locked capability a stored policy already names stays uncheckable, so the
  // person can take it out; one it does not name cannot be put in.
  const inert = locked && !checked;
  return (
    <li
      className={clsx(
        "flex items-start gap-2.5 border-b border-border py-2 last:border-b-0",
        info.highRisk && "-ml-3 border-l-[3px] border-l-danger pl-[9px]",
        inert && "cursor-not-allowed",
      )}
      data-high-risk={info.highRisk || undefined}
      title={locked ? ADO_ACCESS.LOCKED : undefined}
      aria-disabled={inert || undefined}
    >
      {inert && (
        <span
          aria-hidden="true"
          className="mt-0.5 inline-flex size-4 shrink-0 items-center justify-center rounded-[4px] border-[1.5px] border-danger bg-danger-subtle text-meta font-bold leading-none text-danger"
        >
          ✕
        </span>
      )}
      <Checkbox
        id={id}
        className={clsx("mt-0.5", inert && "sr-only")}
        disabled={inert}
        checked={checked}
        onCheckedChange={(v) => onToggle(v === true)}
      />
      <label htmlFor={id} className={clsx("min-w-0 text-body leading-snug", inert && "cursor-not-allowed")}>
        <span
          className={clsx(
            "font-medium",
            checked ? "text-foreground" : "text-muted-foreground",
            locked && "line-through decoration-danger/70",
          )}
        >
          {name}
        </span>
        {info.highRisk && (
          <>
            {" "}
            <HighRiskBadge className="ml-1 align-middle" />
          </>
        )}
        {locked && (
          <>
            {" "}
            <span className="sr-only">{ADO_ACCESS.LOCKED}</span>
          </>
        )}
      </label>
    </li>
  );
}

export function ADOCapabilitiesSection({
  spec,
  onSpecChange,
  ceiling,
}: {
  spec: RunPolicySpec;
  onSpecChange: (next: RunPolicySpec) => void;
  /** The Azure DevOps row's capability_ceiling; undefined = unknown, nothing locked. */
  ceiling?: readonly string[];
}) {
  const chosen = Array.isArray(spec.azure_devops_capabilities) ? spec.azure_devops_capabilities : [];
  return (
    <div className="rounded-lg border border-border p-3">
      <SectionLabel>{ADO_ACCESS.SECTION_TITLE}</SectionLabel>
      <p className="mt-1 text-xs leading-snug text-muted-foreground">{ADO_ACCESS.SECTION_LEAD}</p>
      <p className="mt-2.5 flex flex-wrap items-baseline gap-2 rounded-lg bg-danger-subtle px-3 py-2 text-xs text-danger">
        <HighRiskBadge />
        <span>{ADO_ACCESS.HIGH_RISK_WARN_MEMBER}</span>
      </p>
      <div className="mt-1">
        {ADO_CAPABILITY_GROUPS.map((g) => {
          const title = adoGroupName(g.id);
          return (
            <fieldset key={g.id} className="mt-2.5 overflow-hidden rounded-lg border border-border">
              <legend className="sr-only">{title}</legend>
              <div aria-hidden="true" className="bg-muted px-3 py-2 text-xs font-semibold">
                {title}
              </div>
              <ul className="px-3">
                {ADO_CAPABILITIES.filter((c) => c.group === g.id).map((info) => (
                  <CapabilityRow
                    key={info.cap}
                    info={info}
                    checked={chosen.includes(info.cap)}
                    locked={ceiling !== undefined && !ceiling.includes(info.cap)}
                    onToggle={(on) => onSpecChange(withADOCapabilities(spec, info.cap, on))}
                  />
                ))}
              </ul>
            </fieldset>
          );
        })}
      </div>
    </div>
  );
}
