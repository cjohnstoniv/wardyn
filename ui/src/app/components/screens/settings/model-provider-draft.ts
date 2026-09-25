/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The provider editor's form state and its two translations: a stored
// ModelProvider into a draft, and a draft back into the record PUT
// /model-providers stores. Pure, so the rules the packet settled (the id made
// from the name, every compatible agent ticked, rule 8) are testable without
// rendering the dialog.
import { IMPOSSIBLE } from "../../../lib/integrations";
import { MODEL_PROVIDERS } from "../../../lib/model-providers-copy";
import type { ModelProvider, ModelProviderKind, ProviderHarness } from "../../../lib/types/site";
import { agentCapabilityFor } from "../providers/agents-tab";

// The kinds #537 builds, in the kind step's order (decision 1); #538 adds
// Amazon Bedrock and Claude subscription.
export const EDITOR_KINDS = ["anthropic_api_key", "openai_api_key", "custom_endpoint"] as const;
export type EditorKind = (typeof EDITOR_KINDS)[number];

// One catalog agent a provider can serve (SetupStatus.harnesses without the
// no-managed-auth rows).
export interface HarnessRow {
  id: string;
  display: string;
}

export interface HarnessDraft {
  ticked: boolean;
  model: string;
  path: string;
}

export interface ProviderDraft {
  kind: ModelProviderKind;
  name: string;
  baseUrl: string;
  authHeader: string;
  // Shown with "{token}" where the wire format carries "%s".
  authFormat: string;
  harnesses: Record<string, HarnessDraft>;
}

export const isEndpoint = (kind: ModelProviderKind) => kind === "custom_endpoint";

// The vendor host a key kind's route-through re-points (providerPublicHosts).
export const vendorHost = (kind: ModelProviderKind) =>
  kind === "openai_api_key" ? "api.openai.com" : "api.anthropic.com";

// The catalog's own reason this kind can't drive this agent, or undefined.
export function incompatibleReason(kind: ModelProviderKind, harness: string): string | undefined {
  const cap = agentCapabilityFor(harness);
  return cap && (IMPOSSIBLE as Record<string, Partial<Record<string, string>>>)[kind]?.[cap];
}

const toDisplayFormat = (f: string) => f.replace("%s", "{token}");
const toWireFormat = (f: string) => f.replace("{token}", "%s");

// A new provider: named after its kind (packet A, QA-5), every compatible agent
// ticked, and a custom endpoint's header scheme at the server's own defaults.
export function newDraft(kind: EditorKind, harnesses: HarnessRow[]): ProviderDraft {
  return {
    kind,
    name: MODEL_PROVIDERS.KIND[kind],
    baseUrl: "",
    authHeader: "Authorization",
    authFormat: "Bearer {token}",
    harnesses: Object.fromEntries(
      harnesses.map((h) => [h.id, { ticked: !incompatibleReason(kind, h.id), model: "", path: "" }]),
    ),
  };
}

// An existing provider's draft. Its stored agents are kept even when the
// catalog read is missing one, so a save never drops an agent it did not show.
export function draftFrom(p: ModelProvider, harnesses: HarnessRow[]): ProviderDraft {
  const rows: Record<string, HarnessDraft> = {};
  for (const h of harnesses) rows[h.id] = { ticked: false, model: "", path: "" };
  for (const h of p.harnesses ?? []) rows[h.harness] = { ticked: true, model: h.model ?? "", path: h.path ?? "" };
  return {
    kind: p.kind,
    name: p.name ?? "",
    baseUrl: p.base_url ?? "",
    authHeader: p.auth?.header ?? "Authorization",
    authFormat: toDisplayFormat(p.auth?.format ?? "Bearer %s"),
    harnesses: rows,
  };
}

// The provider id is made from the name at first save and fixed after it
// (decision 3): the server's grammar ([a-z0-9._-], at most 64), and a numeric
// suffix when another provider already holds it.
export function providerIdFrom(name: string, taken: string[]): string {
  const base =
    name
      .toLowerCase()
      .replace(/[^a-z0-9._-]+/g, "-")
      .replace(/^-+|-+$/g, "")
      .slice(0, 60) || "provider";
  let id = base;
  for (let n = 2; taken.includes(id); n++) id = `${base}-${n}`;
  return id;
}

// The record a Save writes. `base` is the stored provider when editing: its id
// stays, and anything the editor doesn't show (turned off, a per-agent header
// scheme) is carried through untouched.
export function providerFrom(draft: ProviderDraft, base: ModelProvider | null, taken: string[]): ModelProvider {
  const endpoint = isEndpoint(draft.kind);
  const name = draft.name.trim();
  const harnesses: ProviderHarness[] = Object.entries(draft.harnesses)
    .filter(([, h]) => h.ticked)
    .map(([id, h]) => ({
      ...base?.harnesses?.find((x) => x.harness === id),
      harness: id,
      model: h.model.trim() || undefined,
      path: endpoint ? h.path.trim() : undefined,
    }));
  return {
    ...base,
    id: base?.id ?? providerIdFrom(name || MODEL_PROVIDERS.KIND[draft.kind], taken),
    name,
    kind: draft.kind,
    base_url: draft.baseUrl.trim() || undefined,
    auth: endpoint
      ? { header: draft.authHeader.trim() || undefined, format: toWireFormat(draft.authFormat.trim()) || undefined }
      : undefined,
    harnesses,
  };
}

const normURL = (u?: string) => (u ?? "").trim().replace(/\/$/, "");
const header = (p: ModelProvider) => (isEndpoint(p.kind) ? p.auth?.header?.trim() || "Authorization" : "");

// Rule 8, the server's providerAddressChanged: where requests go, or how each
// person's credential is sent, differs — which deletes everyone's credential
// for the provider (E9).
export function addressChanged(a: ModelProvider, b: ModelProvider): boolean {
  if (normURL(a.base_url) !== normURL(b.base_url) || header(a) !== header(b)) return true;
  return (a.harnesses ?? []).some((ha) =>
    (b.harnesses ?? []).some(
      (hb) =>
        hb.harness === ha.harness &&
        ((ha.path ?? "").trim() !== (hb.path ?? "").trim() || (ha.auth_header ?? "") !== (hb.auth_header ?? "")),
    ),
  );
}
