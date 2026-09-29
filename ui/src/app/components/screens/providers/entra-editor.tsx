/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Azure DevOps row's Entra section: tenant and client, the REST toggle,
// the capability ceiling and the default profile. It edits only the row's
// `entra` block and hands the next row to git-tab.tsx's onUpdate, so the
// screen's one whole-document Save is still the only write.
//
// Grouping and the High-risk flag come from lib/ado-capabilities.ts; every
// string from workspace-providers-copy.ts.
import { useId, type ReactNode } from "react";
import type { ADOEntraConfig, GitProvider } from "../../../lib/api/providers";
import { ADO_CAPABILITIES, ADO_CAPABILITY_GROUPS } from "../../../lib/ado-capabilities";
import { ADO_CAP_COPY, ADO_ENTRA_EDITOR as E, ADO_GROUP_COPY } from "../../../lib/workspace-providers-copy";
import { Checkbox } from "../../ui/checkbox";
import { Input } from "../../ui/input";
import { Field, Switch } from "../../wardyn/form-primitives";
// clsx, not cn: tailwind-merge doesn't know the text-meta/text-body size
// tokens, so cn() drops them whenever a text colour class is merged in.
import { clsx } from "clsx";

// An empty default_profile reads as ["read"] on the server
// (ADOEntraConfig.Profile), so the checklist shows that.
function effectiveDefault(cfg: ADOEntraConfig): string[] {
  return cfg.default_profile && cfg.default_profile.length > 0 ? cfg.default_profile : ["read"];
}

// The defaults that sit outside the ceiling — the one thing the server refuses
// this section for that the section itself can create. gitRowInvalid withholds
// Save on any.
export function entraDefaultsOffCeiling(cfg: ADOEntraConfig | undefined): string[] {
  if (!cfg) return [];
  const ceiling = cfg.capability_ceiling ?? [];
  return effectiveDefault(cfg).filter((c) => !ceiling.includes(c));
}

// The list with `cap` toggled, in catalogue order.
function toggled(list: string[], cap: string, on: boolean): string[] {
  const next = new Set(list);
  if (on) next.add(cap);
  else next.delete(cap);
  return ADO_CAPABILITIES.map((c) => c.cap).filter((c) => next.has(c));
}

const nameOf = (cap: string) => ADO_CAP_COPY[cap]?.name ?? cap;

function RiskBadge() {
  return (
    <span className="ml-1.5 inline-flex items-center rounded-full bg-danger-subtle px-1.5 py-px text-[10px] font-bold uppercase tracking-wide text-danger">
      {E.HIGH_RISK_BADGE}
    </span>
  );
}

// One capability row. `locked` is the off-ceiling default: a red crossed box
// and a struck-through name, the reason in the hover tooltip, and a disabled
// checkbox kept for assistive tech.
function CapRow({
  id,
  cap,
  checked,
  disabled,
  locked,
  bad,
  tip,
  consequence,
  onChange,
}: {
  id: string;
  cap: string;
  checked: boolean;
  disabled: boolean;
  locked?: boolean;
  bad?: boolean;
  tip?: string;
  consequence?: boolean;
  onChange: (on: boolean) => void;
}) {
  const info = ADO_CAPABILITIES.find((c) => c.cap === cap);
  // The testid stays the bare `id`; the DOM id also carries this editor's own
  // prefix, so two Azure DevOps rows on one screen never share a label target.
  const domId = `${useId()}${id}`;
  return (
    <div
      className={clsx("grid grid-cols-[20px_1fr] gap-2 py-1.5", locked && "cursor-not-allowed", bad && "rounded-md bg-danger-subtle")}
      title={tip}
      data-testid={id}
    >
      {locked ? (
        <span className="relative mt-0.5 inline-flex size-4 items-center justify-center rounded border-[1.5px] border-danger bg-danger-subtle text-[11px] font-bold leading-none text-danger">
          <span aria-hidden="true">✕</span>
          <input type="checkbox" id={domId} className="sr-only" aria-label={nameOf(cap)} checked={false} disabled readOnly />
        </span>
      ) : (
        <Checkbox
          id={domId}
          className="mt-0.5"
          aria-label={nameOf(cap)}
          checked={checked}
          disabled={disabled}
          onCheckedChange={(v) => onChange(v === true)}
        />
      )}
      <label htmlFor={domId} className="min-w-0 leading-snug">
        <span
          className={clsx(
            "text-body font-medium",
            bad ? "text-danger" : checked ? "text-foreground" : "text-muted-foreground",
            locked && "line-through decoration-danger/70",
          )}
        >
          {nameOf(cap)}
          {info?.highRisk && <RiskBadge />}
        </span>
        {consequence && ADO_CAP_COPY[cap] && (
          <span className="block text-meta text-muted-foreground">{ADO_CAP_COPY[cap].consequence}</span>
        )}
      </label>
    </div>
  );
}

function Groups({
  label,
  ceilingView,
  children,
}: {
  label: string;
  ceilingView: boolean;
  children: (groupId: string) => ReactNode;
}) {
  return (
    <div role="group" aria-label={label} className="mt-2 space-y-2.5">
      {ADO_CAPABILITY_GROUPS.map((g) => {
        const risk = g.id === "high_risk";
        const copy = ADO_GROUP_COPY[g.id];
        return (
          <div key={g.id} className={clsx("overflow-hidden rounded-lg border", risk ? "border-danger" : "border-border")}>
            <div className={clsx("flex flex-wrap items-baseline gap-2 px-3 py-2", risk ? "bg-danger-subtle" : "bg-muted/40")}>
              <span className={clsx("text-body font-semibold", risk && "text-danger")}>
                {ceilingView ? copy.name : risk ? E.HIGH_RISK_BADGE : copy.name}
              </span>
              {ceilingView && <span className={clsx("text-meta", risk ? "text-danger" : "text-muted-foreground")}>{copy.lead}</span>}
            </div>
            {ceilingView && risk && (
              <p className="border-t border-dashed border-danger bg-danger-subtle px-3 py-1.5 text-meta text-danger">{E.HIGH_RISK_WARN}</p>
            )}
            <div className="divide-y divide-border px-3">{children(g.id)}</div>
          </div>
        );
      })}
    </div>
  );
}

export function EntraEditor({
  row,
  operator,
  onUpdate,
}: {
  row: GitProvider;
  operator: boolean;
  onUpdate: (next: GitProvider) => void;
}) {
  const uid = useId();
  const cfg: ADOEntraConfig = row.entra ?? { tenant_id: "", client_id: "" };
  const set = (patch: Partial<ADOEntraConfig>) => onUpdate({ ...row, entra: { ...cfg, ...patch } });
  const ceiling = cfg.capability_ceiling ?? [];
  const defaults = effectiveDefault(cfg);
  const offCeiling = entraDefaultsOffCeiling(cfg);
  const inGroup = (groupId: string) => ADO_CAPABILITIES.filter((c) => c.group === groupId);

  return (
    <div className="space-y-5 border-t border-border pt-4" data-testid="entra-editor">
      <div>
        <h4 className="text-sm font-medium text-foreground">{E.SECTION_TITLE}</h4>
        <p className="mt-0.5 text-body text-muted-foreground">{E.SECTION_LEAD}</p>
        <div className="mt-3 grid gap-4 sm:grid-cols-2">
          <Field label={E.FIELD_TENANT} hint={operator ? E.FIELD_TENANT_HINT : undefined} htmlFor={`${uid}entra-tenant`}>
            <Input
              id={`${uid}entra-tenant`}
              className="font-mono"
              disabled={!operator}
              value={cfg.tenant_id}
              onChange={(e) => set({ tenant_id: e.target.value.trim() })}
            />
          </Field>
          <Field label={E.FIELD_CLIENT} hint={operator ? E.FIELD_CLIENT_HINT : undefined} htmlFor={`${uid}entra-client`}>
            <Input
              id={`${uid}entra-client`}
              className="font-mono"
              disabled={!operator}
              value={cfg.client_id}
              onChange={(e) => set({ client_id: e.target.value.trim() })}
            />
          </Field>
        </div>
        <div className="mt-4 flex items-start gap-2.5">
          {/* Absent means on (ADOEntraConfig.rest_api). */}
          <Switch checked={cfg.rest_api !== false} disabled={!operator} label={E.REST_TOGGLE} onChange={(on) => set({ rest_api: on })} />
          <span>
            <span className="block text-body font-medium text-foreground">{E.REST_TOGGLE}</span>
            {operator && <span className="block text-meta text-muted-foreground">{E.REST_TOGGLE_HINT}</span>}
          </span>
        </div>
      </div>

      <div>
        <h4 className="text-sm font-medium text-foreground">{E.CEILING_TITLE}</h4>
        {operator && <p className="mt-0.5 text-body text-muted-foreground">{E.CEILING_LEAD}</p>}
        <Groups label={E.CEILING_TITLE} ceilingView={operator}>
          {(groupId) =>
            inGroup(groupId).map(({ cap }) => (
              <CapRow
                key={cap}
                id={`entra-ceiling-${cap}`}
                cap={cap}
                checked={ceiling.includes(cap)}
                disabled={!operator}
                consequence={operator}
                onChange={(on) => set({ capability_ceiling: toggled(ceiling, cap, on) })}
              />
            ))
          }
        </Groups>
      </div>

      <div>
        <h4 className="text-sm font-medium text-foreground">{E.DEFAULT_TITLE}</h4>
        {operator && <p className="mt-0.5 text-body text-muted-foreground">{E.DEFAULT_LEAD}</p>}
        <Groups label={E.DEFAULT_TITLE} ceilingView={false}>
          {(groupId) =>
            inGroup(groupId).map(({ cap, highRisk }) => {
              const onCeiling = ceiling.includes(cap);
              const isDefault = defaults.includes(cap);
              // Off the ceiling and not a default: locked. Off the ceiling but
              // still a default (the ceiling was just narrowed): left
              // uncheckable and flagged, so the admin can clear it.
              const locked = !onCeiling && !isDefault;
              const bad = !onCeiling && isDefault;
              return (
                <CapRow
                  key={cap}
                  id={`entra-default-${cap}`}
                  cap={cap}
                  checked={isDefault}
                  disabled={!operator || locked}
                  locked={locked}
                  bad={bad}
                  tip={locked ? E.DEFAULT_OFF_CEILING_TIP : highRisk && !isDefault ? E.DEFAULT_HIGH_RISK_TIP : undefined}
                  onChange={(on) => set({ default_profile: toggled(defaults, cap, on) })}
                />
              );
            })
          }
        </Groups>
        {offCeiling.length > 0 && (
          <div role="alert" className="mt-2.5 rounded-lg bg-danger-subtle px-3 py-2 text-body text-danger">
            <b className="font-semibold">{E.ERROR_TITLE}</b>
            {offCeiling.map((cap) => (
              <span key={cap}> {E.ERROR_DEFAULT_OFF_CEILING(nameOf(cap))}</span>
            ))}
          </div>
        )}
        {operator && (
          <p className="mt-2.5 rounded-lg bg-muted/40 px-3 py-2 text-body text-muted-foreground">
            <b className="font-semibold">{E.PER_RUN_POINTER_LEAD}</b> {E.PER_RUN_POINTER}
          </p>
        )}
      </div>

      {!operator && <p className="rounded-lg bg-muted/40 px-3 py-2 text-body text-muted-foreground">{E.READ_ONLY_NOTE}</p>}
    </div>
  );
}
