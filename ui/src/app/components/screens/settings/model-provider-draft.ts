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
import { LOCAL_MODEL_EDITOR } from "../../../lib/local-model-copy";
import { MODEL_PROVIDERS, PROVIDER_EDITOR } from "../../../lib/model-providers-copy";
import type { ModelProvider, ModelProviderKind, ProviderHarness } from "../../../lib/types/site";
import { agentCapabilityFor } from "../providers/agents-tab";

// The six kinds the editor draws, in the kind step's order (mp-packet-B.html
// §"Case (a) first", and M9's one added row): Claude subscription, Amazon
// Bedrock, Anthropic API key, OpenAI API key, Your own endpoint, Local model
// server. "Amazon Bedrock" is ONE kind-step option
// that starts a bedrock_sso draft — the "How people sign in" toggle inside the
// form (#538, E3) is what reaches bedrock_bearer; that kind is never a kind-
// step option of its own; editing a stored bedrock_bearer provider reaches the
// form directly via draftFrom, bypassing this list.
export const EDITOR_KINDS = [
  "anthropic_subscription",
  "bedrock_sso",
  "anthropic_api_key",
  "openai_api_key",
  "custom_endpoint",
  "local_model",
] as const;
export type EditorKind = (typeof EDITOR_KINDS)[number];

export const isBedrock = (kind: ModelProviderKind) => kind === "bedrock_sso" || kind === "bedrock_bearer";

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
  // Bedrock kinds only (bedrock_sso, bedrock_bearer) — mp.Bedrock's fields.
  region: string;
  ssoStartUrl: string;
  ssoAccountId: string;
  ssoRoleName: string;
  // local_model only. `useToken` is the "This server asks for a token" switch (auth is
  // sent only while it is on); the three limits are text, empty meaning the default.
  useToken: boolean;
  models: string[];
  maxContext: string;
  maxOutput: string;
  keepAlive: string;
  harnesses: Record<string, HarnessDraft>;
}

export const isEndpoint = (kind: ModelProviderKind) => kind === "custom_endpoint";
export const isLocal = (kind: ModelProviderKind) => kind === "local_model";

// The vendor host a key kind's route-through re-points (providerPublicHosts).
export const vendorHost = (kind: ModelProviderKind) =>
  kind === "openai_api_key" ? "api.openai.com" : "api.anthropic.com";

// The provides-line under the dialog title (§2.2, per kind) — the editor's own
// wording, distinct from the list row's compact providesLine (model-providers-
// copy.ts), which drops the trailing period for a table cell.
export function editorProvidesLine(kind: ModelProviderKind): string {
  switch (kind) {
    case "bedrock_sso":
      return PROVIDER_EDITOR.PROVIDES_SSO;
    case "anthropic_subscription":
      return PROVIDER_EDITOR.PROVIDES_CLAUDE;
    case "custom_endpoint":
      return PROVIDER_EDITOR.PROVIDES_TOKEN;
    case "local_model":
      return LOCAL_MODEL_EDITOR.DESCRIPTION;
    default:
      return PROVIDER_EDITOR.PROVIDES_KEY;
  }
}

// The catalog's own reason this kind can't drive this agent, or undefined.
// bedrock_sso and bedrock_bearer both fold to IMPOSSIBLE's single "bedrock"
// row (lib/integrations.ts's AiType has no separate entry for either) — cast
// as a loose Record rather than AiType so custom_endpoint (no IMPOSSIBLE row
// at all) still falls through to undefined instead of a type error.
export function incompatibleReason(kind: ModelProviderKind, harness: string): string | undefined {
  const cap = agentCapabilityFor(harness);
  const key = isBedrock(kind) ? "bedrock" : kind;
  return cap && (IMPOSSIBLE as Record<string, Partial<Record<string, string>>>)[key]?.[cap];
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
    region: "",
    ssoStartUrl: "",
    ssoAccountId: "",
    ssoRoleName: "",
    useToken: false,
    models: [],
    maxContext: "",
    maxOutput: "",
    keepAlive: "",
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
    region: p.bedrock?.region ?? "",
    ssoStartUrl: p.bedrock?.sso_start_url ?? "",
    ssoAccountId: p.bedrock?.sso_account_id ?? "",
    ssoRoleName: p.bedrock?.sso_role_name ?? "",
    useToken: !!p.auth,
    models: p.local?.models ?? [],
    maxContext: p.local?.max_context ? String(p.local.max_context) : "",
    maxOutput: p.local?.max_output ? String(p.local.max_output) : "",
    keepAlive: p.local?.keep_alive_seconds ? String(p.local.keep_alive_seconds) : "",
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

// A limit field: empty is the default (absent), anything else goes to the server as typed, so its
// "must be between" answer is the one the admin reads.
const limit = (v: string): number | undefined => (v.trim() === "" ? undefined : Number(v));

// The record a Save writes. `base` is the stored provider when editing: its id
// stays, and anything the editor doesn't show (turned off, a per-agent header
// scheme) is carried through untouched.
export function providerFrom(draft: ProviderDraft, base: ModelProvider | null, taken: string[]): ModelProvider {
  const endpoint = isEndpoint(draft.kind);
  const local = isLocal(draft.kind);
  const bedrock = isBedrock(draft.kind);
  const sso = draft.kind === "bedrock_sso";
  const name = draft.name.trim();
  const harnesses: ProviderHarness[] = Object.entries(draft.harnesses)
    .filter(([, h]) => h.ticked)
    .map(([id, h]) => ({
      ...base?.harnesses?.find((x) => x.harness === id),
      harness: id,
      // A local model's agent asks for one of the listed models; the first when none was picked.
      model: h.model.trim() || (local ? draft.models[0] : undefined),
      path: endpoint ? h.path.trim() : undefined,
    }));
  return {
    ...base,
    id: base?.id ?? providerIdFrom(name || MODEL_PROVIDERS.KIND[draft.kind], taken),
    name,
    kind: draft.kind,
    base_url: draft.baseUrl.trim() || undefined,
    auth:
      endpoint || (local && draft.useToken)
        ? { header: draft.authHeader.trim() || undefined, format: toWireFormat(draft.authFormat.trim()) || undefined }
        : undefined,
    local: local
      ? {
          ...base?.local,
          models: draft.models,
          max_context: limit(draft.maxContext),
          max_output: limit(draft.maxOutput),
          keep_alive_seconds: limit(draft.keepAlive),
        }
      : undefined,
    // Anything the editor doesn't show (a custom Bedrock endpoint override) is
    // carried through untouched, same rule as the endpoint kind's per-harness
    // auth_header/auth_format above.
    bedrock: bedrock
      ? {
          ...base?.bedrock,
          region: draft.region.trim() || undefined,
          sso_start_url: sso ? draft.ssoStartUrl.trim() || undefined : undefined,
          sso_account_id: sso ? draft.ssoAccountId.trim() || undefined : undefined,
          sso_role_name: sso ? draft.ssoRoleName.trim() || undefined : undefined,
        }
      : undefined,
    harnesses,
  };
}

const normURL = (u?: string) => (u ?? "").trim().replace(/\/$/, "");
const header = (p: ModelProvider) =>
  isEndpoint(p.kind) || (isLocal(p.kind) && p.auth) ? p.auth?.header?.trim() || "Authorization" : "";

// Rule 8, the server's providerAddressChanged: where requests go, or how each
// person's credential is sent, differs — which deletes everyone's credential
// for the provider (E9). E9's colfoot names "Bedrock's region or endpoint"
// alongside the base URL/path/header this already covered; the AWS access
// portal and its pin are identity, not address, so they don't belong here.
//
// A kind change is the same fact by a different door: it is only reachable
// today via E3's "How people sign in" toggle (bedrock_sso <-> bedrock_bearer —
// every other kind is fixed after first save, decision 3), and the server
// mints a fresh UID on any kind change (assignModelProviderUIDs), which
// orphans every credential given under the old one. Owner ruling 2026-09-25
// (PR #1038 review): the existing confirm covers it verbatim — no new string,
// no new dialog.
export function addressChanged(a: ModelProvider, b: ModelProvider): boolean {
  if (
    a.kind !== b.kind ||
    normURL(a.base_url) !== normURL(b.base_url) ||
    header(a) !== header(b) ||
    normURL(a.bedrock?.region) !== normURL(b.bedrock?.region) ||
    normURL(a.bedrock?.base_url) !== normURL(b.bedrock?.base_url)
  ) {
    return true;
  }
  return (a.harnesses ?? []).some((ha) =>
    (b.harnesses ?? []).some(
      (hb) =>
        hb.harness === ha.harness &&
        ((ha.path ?? "").trim() !== (hb.path ?? "").trim() || (ha.auth_header ?? "") !== (hb.auth_header ?? "")),
    ),
  );
}

// Which field a refused save's own sentence is about (M9: the field it names is
// outlined beside the red card). Read off the server's wording, so a refusal the
// editor doesn't recognise outlines nothing and the card still says all of it.
export type RefusedField = "base_url" | "models" | "max_context" | "max_output" | "keep_alive_seconds";

export function refusedField(message: string | null): RefusedField | null {
  if (!message) return null;
  if (message.includes("model must be one of local.models")) return null; // a harness's, see refusedHarness
  if (message.includes("base_url")) return "base_url";
  if (message.includes("local.models")) return "models";
  return (["max_context", "max_output", "keep_alive_seconds"] as const).find((f) => message.includes(`local.${f}`)) ?? null;
}

// The agent whose Model the server refused ("codex-cli: model must be one of local.models").
export function refusedHarness(message: string | null): string | null {
  return message?.match(/: ([a-z0-9-]+): model must be one of local\.models/)?.[1] ?? null;
}

// Whether the refusal names this chip, at the position the server quotes a model name: 'local.models:
// "x" is not a model name' and 'local.models lists "x" twice' (mp400LocalModel, mp400LocalModelDup).
// Not anywhere in the sentence: the provider id is quoted at its start and may equal a chip's name.
export const refusedModel = (message: string | null, name: string): boolean =>
  !!message && (message.includes(`local.models: "${name}" is not`) || message.includes(`local.models lists "${name}" twice`));
