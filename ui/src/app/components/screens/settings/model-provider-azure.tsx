/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Azure Foundry provider's form: its Endpoint, the one Route it serves, the sign-in prerequisite for that route,
// the private-endpoint note, and Use with. Each person signs in with Microsoft, so there is no Base URL, Auth or
// gateway block. Drawn by the 011-foundry-m7 mock (F3 to F7); every string is lib/model-providers-copy.ts's (M7).
import * as React from "react";
import { ChevronDown, ChevronRight } from "lucide-react";
import { PROVIDER_EDITOR } from "../../../lib/model-providers-copy";
import { Checkbox } from "../../ui/checkbox";
import { Input } from "../../ui/input";
import { RadioGroup, RadioGroupItem } from "../../ui/radio-group";
import { cn } from "../../ui/utils";
import { Field, fieldHintId } from "../../wardyn/form-primitives";
import { Notice } from "../../wardyn/notice";
import {
  AZURE_FAST_HARNESS,
  AZURE_HARNESS_ROUTE,
  AZURE_ROUTE_PATH,
  azureRefusedField,
  incompatibleReason,
  type AzureRoute,
  type HarnessDraft,
  type HarnessRow,
  type ProviderDraft,
} from "./model-provider-draft";

// The resource each route's sign-in is granted for (the server's audience per route, without /.default).
export const AZURE_RESOURCE: Record<AzureRoute, string> = {
  openai_v1: "https://cognitiveservices.azure.com",
  anthropic: "https://ai.azure.com",
};

const ROUTES: AzureRoute[] = ["openai_v1", "anthropic"];

// The sentence with each of `names` set in code type.
function withCode(text: string, names: string[]): React.ReactNode[] {
  const pattern = new RegExp(`(${names.map((n) => n.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")).join("|")})`);
  return text.split(pattern).map((part, i) =>
    names.includes(part) ? (
      <code key={i} className="font-mono">
        {part}
      </code>
    ) : (
      part
    ),
  );
}

// Moving to a route keeps the agent that route serves ticked and unticks the one it cannot serve.
const retick = (harnesses: ProviderDraft["harnesses"], route: AzureRoute): ProviderDraft["harnesses"] =>
  Object.fromEntries(
    Object.entries(harnesses).map(([id, h]) => [id, AZURE_HARNESS_ROUTE[id] ? { ...h, ticked: AZURE_HARNESS_ROUTE[id] === route } : h]),
  );

export function AzureFields({
  draft,
  set,
  refused,
  refusedAt,
  rows,
}: {
  draft: ProviderDraft;
  set: (patch: Partial<ProviderDraft>) => void;
  refused: string | null;
  refusedAt: string | null;
  rows: HarnessRow[];
}) {
  const bad = azureRefusedField(refused, refusedAt);
  const [privateOpen, setPrivateOpen] = React.useState(false);
  const route = draft.azureRoute;
  const resource = AZURE_RESOURCE[route];
  const setHarness = (id: string, patch: Partial<HarnessDraft>) =>
    set({ harnesses: { ...draft.harnesses, [id]: { ...draft.harnesses[id], ...patch } } });

  return (
    <>
      <Field label={PROVIDER_EDITOR.NAME} htmlFor="mp-name" hint={PROVIDER_EDITOR.NAME_HINT}>
        <Input id="mp-name" value={draft.name} onChange={(e) => set({ name: e.target.value })} />
      </Field>

      <Field label={PROVIDER_EDITOR.ENDPOINT} htmlFor="mp-azure-endpoint" hint={PROVIDER_EDITOR.ENDPOINT_HINT} required>
        <Input
          id="mp-azure-endpoint"
          required
          className="font-mono"
          placeholder="https://"
          aria-invalid={bad === "azure_endpoint" || undefined}
          value={draft.azureEndpoint}
          onChange={(e) => set({ azureEndpoint: e.target.value })}
        />
      </Field>

      <Field label={PROVIDER_EDITOR.ROUTE} htmlFor="mp-azure-route" hint={PROVIDER_EDITOR.ROUTE_HINT}>
        {/* The two-button segmented pair (M7), over the Radix radio group: each button is a label around its radio. */}
        <RadioGroup
          id="mp-azure-route"
          aria-label={PROVIDER_EDITOR.ROUTE}
          aria-describedby={fieldHintId("mp-azure-route")}
          className="flex flex-wrap gap-2"
          value={route}
          onValueChange={(v) => set({ azureRoute: v as AzureRoute, harnesses: retick(draft.harnesses, v as AzureRoute) })}
        >
          {ROUTES.map((r) => (
            <label
              key={r}
              className={cn(
                "relative inline-flex cursor-pointer items-center rounded-md border px-3 py-1.5 font-mono text-body hover:bg-accent has-[[data-state=checked]]:bg-secondary has-[button:focus-visible]:ring-[3px] has-[button:focus-visible]:ring-ring",
                bad === "azure_route" ? "border-destructive" : "border-border",
              )}
            >
              <RadioGroupItem value={r} className="absolute size-px opacity-0" aria-invalid={bad === "azure_route" || undefined} />
              {AZURE_ROUTE_PATH[r]}
            </label>
          ))}
        </RadioGroup>
      </Field>

      <Notice tone="info">{withCode(PROVIDER_EDITOR.PREREQ(resource), [resource])}</Notice>

      <div>
        <button
          type="button"
          aria-expanded={privateOpen}
          aria-controls="mp-azure-private"
          onClick={() => setPrivateOpen(!privateOpen)}
          className="flex items-center gap-2 text-body"
        >
          {privateOpen ? <ChevronDown className="size-4 text-muted-foreground" aria-hidden /> : <ChevronRight className="size-4 text-muted-foreground" aria-hidden />}
          {PROVIDER_EDITOR.PRIVATE_TITLE}
        </button>
        {privateOpen && (
          <p id="mp-azure-private" className="mt-2 pl-6 text-xs leading-snug text-muted-foreground">
            {withCode(PROVIDER_EDITOR.PRIVATE_BODY, ["internal_hosts", "upstream_proxy_no_proxy"])}
          </p>
        )}
      </div>

      <fieldset className="space-y-2">
        <legend className="text-sm font-medium">{PROVIDER_EDITOR.USE_WITH}</legend>
        {rows
          .filter((h) => AZURE_HARNESS_ROUTE[h.id])
          .map((h) => (
            <AzureHarness key={h.id} harness={h} draft={draft} onChange={(patch) => setHarness(h.id, patch)} />
          ))}
      </fieldset>
    </>
  );
}

// One agent under Use with: its Deployment, and for Claude Code on its own route a Fast deployment. An agent the
// route cannot serve is disabled with its reason (the label dims; the reason keeps full contrast), never hidden.
function AzureHarness({
  harness,
  draft,
  onChange,
}: {
  harness: HarnessRow;
  draft: ProviderDraft;
  onChange: (patch: Partial<HarnessDraft>) => void;
}) {
  const row = draft.harnesses[harness.id];
  const reason = incompatibleReason(draft.kind, harness.id, { route: draft.azureRoute, display: harness.display });
  const needsModel = row.ticked && !row.model.trim();
  const [open, setOpen] = React.useState(true);
  const expanded = row.ticked && (open || needsModel);
  const base = `mp-h-${harness.id}`;
  const fast = harness.id === AZURE_FAST_HARNESS && draft.azureRoute === "anthropic";
  const need = AZURE_HARNESS_ROUTE[harness.id];

  return (
    <div className={cn("rounded-lg border border-border", reason && "hatch-unavailable")}>
      <div className="flex items-start gap-2 px-3 py-2">
        <Checkbox id={base} className="mt-0.5" checked={row.ticked} disabled={!!reason} onCheckedChange={(v) => onChange({ ticked: v === true })} />
        <div className="min-w-0 flex-1">
          <label htmlFor={base} className={cn("text-body font-normal", reason && "opacity-60")}>
            {harness.display}
          </label>
          {reason && need && <p className="mt-0.5 text-xs text-muted-foreground">{withCode(reason, [AZURE_ROUTE_PATH[need]])}</p>}
        </div>
        {row.ticked && (
          <button
            type="button"
            aria-label={harness.display}
            aria-expanded={expanded}
            aria-controls={`${base}-body`}
            // An empty required Deployment stays open: collapsing would unmount the input its `required` check lives on.
            disabled={needsModel}
            onClick={() => setOpen(!expanded)}
            className="text-muted-foreground disabled:opacity-50"
          >
            {expanded ? <ChevronDown className="size-4" /> : <ChevronRight className="size-4" />}
          </button>
        )}
      </div>
      {expanded && (
        <div id={`${base}-body`} className="grid gap-4 border-t border-border p-3 sm:grid-cols-2">
          <Field label={PROVIDER_EDITOR.DEPLOYMENT} htmlFor={`${base}-model`} hint={PROVIDER_EDITOR.DEPLOYMENT_HINT} required>
            <Input id={`${base}-model`} required className="font-mono" value={row.model} onChange={(e) => onChange({ model: e.target.value })} />
          </Field>
          {fast && (
            <Field label={PROVIDER_EDITOR.FAST_DEPLOYMENT} htmlFor={`${base}-fast`} hint={PROVIDER_EDITOR.FAST_DEPLOYMENT_HINT}>
              <Input id={`${base}-fast`} className="font-mono" value={row.fastModel ?? ""} onChange={(e) => onChange({ fastModel: e.target.value })} />
            </Field>
          )}
        </div>
      )}
    </div>
  );
}

// A save that landed with advisories: the server's own sentences, to check before people sign in.
export function SavedWarning({ warnings }: { warnings: string[] }) {
  return (
    <Notice tone="warning" live title={PROVIDER_EDITOR.SAVED_WARNING_TITLE}>
      {warnings.map((w) => (
        <p key={w}>{w}</p>
      ))}
    </Notice>
  );
}
