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

// One extra header row (custom_endpoint only). `placeholder` is the row's own value placeholder, shown while
// the value is empty: UI-only, never sent.
export interface HeaderDraft {
  name: string;
  value: string;
  placeholder?: string;
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
  // bedrock_bearer only: bedrock.base_url, the gateway address a Bedrock key row routes through.
  bedrockBaseUrl: string;
  ssoStartUrl: string;
  ssoAccountId: string;
  ssoRoleName: string;
  // local_model only. `useToken` is the "This server asks for a token" switch (auth is
  // sent only while it is on); the three limits are text, empty meaning the default.
  useToken: boolean;
  // local_model only: local.allow_on_laptop_runners, the "Let runs on a person's own computer…" switch.
  laptopRunners: boolean;
  models: string[];
  maxContext: string;
  maxOutput: string;
  keepAlive: string;
  // gateway_identity: the "Send the person and run to this gateway" switch and its Token audience. Named
  // apart from the editor's own `audience`, which is who the provider is available to.
  gatewayOn: boolean;
  gatewayAudience: string;
  // gateway_identity.inspection: the "Let this gateway check prompts" switch.
  gatewayInspect: boolean;
  // The address, audience and inspection the stored row sends identity with, or null when it does not: what
  // a Base URL edit compares against, and what turning it off is a change from.
  gatewaySaved: { baseUrl: string; audience: string; inspect: boolean } | null;
  // custom_endpoint only: how people connect. Off is "Paste a token"; on is "Sign in", and the fields below
  // are the stored `oidc` block's. The block's authorize_url and token_url are not here: a save carries them.
  oidcOn: boolean;
  oidcIssuer: string;
  oidcClientId: string;
  // The operator secret holding the client's secret; "" is a public client.
  oidcSecret: string;
  oidcScopes: string[];
  // "Match the person to their console sign-in": on is identity_binding "console", off is "none".
  oidcBound: boolean;
  // custom_endpoint only: extra_headers, the non-secret headers a gateway routes on.
  headers: HeaderDraft[];
  harnesses: Record<string, HarnessDraft>;
}

// The address a provider's gateway is reached at: a Bedrock row's bedrock.base_url, any other row's base_url.
export const providerGatewayAddress = (p: ModelProvider) => (isBedrock(p.kind) ? (p.bedrock?.base_url ?? "") : (p.base_url ?? ""));
// The same address as the draft has it now.
export const draftGatewayAddress = (d: ProviderDraft) => (isBedrock(d.kind) ? d.bedrockBaseUrl : d.baseUrl);
// The kinds that may send run identity to a gateway. A Bedrock sign-in row may only where the install signs
// AWS requests at its proxy (the editor asks the server's answer): SigV4 signs the Host.
export const gatewayKind = (kind: ModelProviderKind) =>
  kind === "anthropic_api_key" || kind === "openai_api_key" || kind === "custom_endpoint" || kind === "bedrock_bearer" || kind === "bedrock_sso";

export const isEndpoint = (kind: ModelProviderKind) => kind === "custom_endpoint";
export const isLocal = (kind: ModelProviderKind) => kind === "local_model";

// The vendor host a key kind's route-through re-points (providerPublicHosts).
export const vendorHost = (kind: ModelProviderKind) =>
  kind === "openai_api_key" ? "api.openai.com" : "api.anthropic.com";

// Where a provider's calls go when it has no gateway: the vendor's host, or a Bedrock row's regional runtime host.
export const directHost = (d: ProviderDraft) =>
  isBedrock(d.kind) ? `bedrock-runtime.${d.region.trim() || "us-east-1"}.amazonaws.com` : vendorHost(d.kind);

// The provides-line under the dialog title (§2.2, per kind) — the editor's own
// wording, distinct from the list row's compact providesLine (model-providers-
// copy.ts), which drops the trailing period for a table cell.
export function editorProvidesLine(kind: ModelProviderKind, signIn = false): string {
  if (signIn && isEndpoint(kind)) return PROVIDER_EDITOR.OIDC.PROVIDES;
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
    bedrockBaseUrl: "",
    ssoStartUrl: "",
    ssoAccountId: "",
    ssoRoleName: "",
    useToken: false,
    laptopRunners: false,
    models: [],
    maxContext: "",
    maxOutput: "",
    keepAlive: "",
    gatewayOn: false,
    gatewayAudience: "",
    gatewayInspect: false,
    gatewaySaved: null,
    oidcOn: false,
    oidcIssuer: "",
    oidcClientId: "",
    oidcSecret: "",
    oidcScopes: [],
    oidcBound: true,
    headers: [],
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
    bedrockBaseUrl: p.bedrock?.base_url ?? "",
    ssoStartUrl: p.bedrock?.sso_start_url ?? "",
    ssoAccountId: p.bedrock?.sso_account_id ?? "",
    ssoRoleName: p.bedrock?.sso_role_name ?? "",
    useToken: !!p.auth,
    laptopRunners: !!p.local?.allow_on_laptop_runners,
    models: p.local?.models ?? [],
    maxContext: p.local?.max_context ? String(p.local.max_context) : "",
    maxOutput: p.local?.max_output ? String(p.local.max_output) : "",
    keepAlive: p.local?.keep_alive_seconds ? String(p.local.keep_alive_seconds) : "",
    gatewayOn: !!p.gateway_identity,
    gatewayAudience: p.gateway_identity?.audience ?? "",
    gatewayInspect: p.gateway_identity?.inspection === "gateway",
    gatewaySaved: p.gateway_identity
      ? { baseUrl: providerGatewayAddress(p), audience: p.gateway_identity.audience, inspect: p.gateway_identity.inspection === "gateway" }
      : null,
    oidcOn: !!p.oidc,
    oidcIssuer: p.oidc?.issuer ?? "",
    oidcClientId: p.oidc?.client_id ?? "",
    oidcSecret: p.oidc?.client_secret_ref ?? "",
    oidcScopes: p.oidc?.scopes ?? [],
    oidcBound: p.oidc?.identity_binding !== "none",
    headers: (p.extra_headers ?? []).map((h) => ({ name: h.name, value: h.value })),
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
  // Trimmed as the server trims them; the UI-only placeholder never goes. None, or any other kind, is no key.
  const headers = endpoint ? draft.headers.map((h) => ({ name: h.name.trim(), value: h.value.trim() })) : [];
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
    // Absent is off; a stored field the editor doesn't show rides along inside it.
    gateway_identity: draft.gatewayOn && gatewayKind(draft.kind)
      ? { ...base?.gateway_identity, audience: draft.gatewayAudience.trim(), inspection: draft.gatewayInspect && !isBedrock(draft.kind) ? "gateway" : undefined }
      : undefined,
    // The stored block's authorize_url and token_url ride along inside it: dropping one would change the
    // sign-in's address and delete everyone's sign-in. Binding "console" is also the absent value.
    oidc:
      endpoint && draft.oidcOn
        ? {
            ...base?.oidc,
            issuer: draft.oidcIssuer.trim(),
            client_id: draft.oidcClientId.trim(),
            client_secret_ref: draft.oidcSecret || undefined,
            scopes: draft.oidcScopes,
            identity_binding: draft.oidcBound ? undefined : "none",
          }
        : undefined,
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
          allow_on_laptop_runners: draft.laptopRunners || undefined,
        }
      : undefined,
    // Anything the editor doesn't show (a custom Bedrock endpoint override) is
    // carried through untouched, same rule as the endpoint kind's per-harness
    // auth_header/auth_format above.
    bedrock: bedrock
      ? {
          ...base?.bedrock,
          region: draft.region.trim() || undefined,
          base_url: draft.bedrockBaseUrl.trim() || undefined,
          sso_start_url: sso ? draft.ssoStartUrl.trim() || undefined : undefined,
          sso_account_id: sso ? draft.ssoAccountId.trim() || undefined : undefined,
          sso_role_name: sso ? draft.ssoRoleName.trim() || undefined : undefined,
        }
      : undefined,
    extra_headers: headers.length > 0 ? headers : undefined,
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
// laptopBaseUrlRefused: the switch is on and Base URL names a host other than host.docker.internal, which
// the server refuses (mp400LocalLaptopHost); the editor says so under Base URL before Save.
export function laptopBaseUrlRefused(draft: ProviderDraft): boolean {
  if (!isLocal(draft.kind) || !draft.laptopRunners || !draft.baseUrl.trim()) return false;
  try {
    return new URL(draft.baseUrl.trim()).hostname.toLowerCase().replace(/\.$/, "") !== "host.docker.internal";
  } catch {
    return true;
  }
}

// The server's oidcAddress (internal/api/endpoint_oidc.go): "" for a provider with no sign-in block, else the
// authority, the client, the scopes and the binding. Compared as the server compares them: nothing trimmed, the
// scopes sorted but never de-duplicated (a repeat is a change), and an absent binding read as "console".
const oidcAddress = (p: ModelProvider): string =>
  p.oidc
    ? JSON.stringify([
        p.oidc.issuer,
        p.oidc.authorize_url ?? "",
        p.oidc.token_url ?? "",
        p.oidc.client_id,
        [...(p.oidc.scopes ?? [])].sort(),
        p.oidc.identity_binding || "console",
      ])
    : "";

// How the sign-in block differs between two versions of one provider, when it does: added (Paste a token to
// Sign in), removed (the other way), or edited. Only the digest's terms count, as for addressChanged.
export type OIDCChange = "to_sign_in" | "to_paste" | "edited";
export function oidcChange(a: ModelProvider, b: ModelProvider): OIDCChange | null {
  if (oidcAddress(a) === oidcAddress(b)) return null;
  return !a.oidc ? "to_sign_in" : !b.oidc ? "to_paste" : "edited";
}

// A name in the case the server stores it (Go's CanonicalMIMEHeaderKey): a name with a character a header
// cannot carry stays as typed, for the server's own refusal.
const canonicalHeader = (n: string) =>
  /^[!#$%&'*+.^_`|~0-9A-Za-z-]+$/.test(n)
    ? n
        .split("-")
        .map((w) => w.charAt(0).toUpperCase() + w.slice(1).toLowerCase())
        .join("-")
    : n;

// The server's extraHeadersAddress: the rows in name order, `name:value` a line each, after the server's
// own trimming and canonical case. No headers is "".
const extraHeadersAddress = (p: ModelProvider): string =>
  (p.extra_headers ?? [])
    .map((h) => ({ name: canonicalHeader(h.name.trim()), value: h.value.trim() }))
    .sort((x, y) => (x.name < y.name ? -1 : x.name > y.name ? 1 : 0))
    .map((h) => `${h.name}:${h.value}\n`)
    .join("");

export function addressChanged(a: ModelProvider, b: ModelProvider): boolean {
  if (
    a.kind !== b.kind ||
    extraHeadersAddress(a) !== extraHeadersAddress(b) ||
    oidcAddress(a) !== oidcAddress(b) ||
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
export type RefusedField = "base_url" | "gateway_audience" | "models" | "max_context" | "max_output" | "keep_alive_seconds";

export function refusedField(message: string | null): RefusedField | null {
  if (!message) return null;
  // The provider id is quoted at the sentence's start and may spell a field's name ("audience", "local.models"),
  // so only what follows it is read. A refusal without that opening is read whole.
  const rest = message.replace(/^.*?model_providers: "[a-z0-9._-]*": /s, "");
  if (rest.includes("model must be one of local.models")) return null; // a harness's, see refusedHarness
  // The audience's refusals, matched at their fixed opening: the duplicate one quotes what the admin typed.
  if (/^(gateway_identity[.:] ?audience|moved to a new address, so gateway_identity\.audience)/.test(rest)) return "gateway_audience";
  if (rest.includes("base_url")) return "base_url";
  if (rest.includes("local.models")) return "models";
  return (["max_context", "max_output", "keep_alive_seconds"] as const).find((f) => rest.includes(`local.${f}`)) ?? null;
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
