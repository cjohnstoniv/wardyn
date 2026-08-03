/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Integrations copy canon + structured metadata — verbatim transcription of
// the approved mock export's `T` (copy canon) and `CAPS` (capability-line
// notes) from mockup/wardyn-integrations.js, plus the structured metadata the
// later Integrations pages (list/add/detail) will render from: which category
// a type belongs to, what each AI type's picker card says, how a credential's
// residency reads as a chip, and which capabilities are flatly impossible for
// a given type (never a toggle — a stated fact, sourced from CAPS' own `fact`
// lines below). Pure TS — no React, no fetch, no DOM.
//
// Three layers, per the mock's own header: credential (dumb material) →
// integration (named, configured) → capabilities (ON / OFF / impossible-as-
// fact). There is no test-connect anywhere by design (T.FOOTNOTE).

// ============================ COPY CANON (verbatim) ============================
export const T = {
  LEDE: "Named connections to systems outside Wardyn — model providers, git hosts, artifact mirrors, your corporate proxy. Wardyn runs without any of them.",
  FOOTNOTE:
    "Wardyn doesn't test-connect. Everything here is what's stored and what Wardyn can see locally — except the GitHub App's ref-confinement row, which really does ask GitHub.",
  STORE_NOTE: "Wardyn stores this — it doesn't dial the provider to check it.",
  EMPTY_TITLE: "No integrations",
  EMPTY_BODY:
    "Wardyn runs anything without any of this — add an integration when a run or a Wardyn feature needs one. Governed commands, interactive runs, and terminal recordings need none.",
  EMPTY_AI:
    "None. Runs work without a model — add one to have a coding agent drive a run, or to use Wardyn's own AI features.",
  EMPTY_SCM: "None. Public repos clone without any credential.",
  EMPTY_MIRROR: "None. Runs fetch from the public registries.",
  EMPTY_PROXY: "None. The sandbox reaches the internet through wardyn-proxy directly.",
  PROXY_BANNER:
    "A corporate proxy was detected on this host. Until it's connected, sandboxes may not reach anything — add the Host proxy integration first.",
  X_KEY_CODEX: "Codex CLI speaks the OpenAI API only — an Anthropic key can't drive it. Not a setting.",
  X_SUB_CODEX: "Codex CLI speaks the OpenAI API only — a Claude login can't drive it. Not a setting.",
  X_BEDROCK_CODEX: "Codex CLI speaks the OpenAI API only — Bedrock can't drive it. Not a setting.",
  X_OPENAI_CLAUDE: "Claude Code speaks the Anthropic API only — an OpenAI key can't drive it. Not a setting.",
  X_SUB_DIRECT:
    "A subscription token is accepted only for Claude-Code-shaped requests; anything else comes back 429. That's Anthropic's gate, not a Wardyn setting.",
  X_AZURE_HARNESS:
    "Neither agent tool can be pointed at an Azure OpenAI deployment. Azure powers Wardyn's own features only.",
  X_AZURE_DIRECT: "No sandbox lane exists — Azure is called from the control plane only.",
  BEDROCK_FEATURES:
    "Wardyn's own features reach Bedrock through the AWS credential chain — the same lane this integration uses.",
  LAW: "A tool is what the image carries. An integration is what it connects through. Wardyn never installs tools into your image — it only wires them at run time.",
  CACHE_CAVEAT: "Answers are cached for 5 minutes — Re-check may return the cached one.",
  WRITE_ONLY: "The store is write-only: the value can't be read back.",
  KEY_FIELD_HINT: "write-only — it can be replaced or removed, never read back",
  MANAGED_LINE: "One login in a sandbox; the token is injected proxy-side and the sandbox holds only an inert sentinel.",
  HOSTCLI_LINE: "Uses the ~/.claude login on this host, mounted read-only into the run.",
  SEALED_NOTE:
    "Sealed control plane — wardynd runs in a container, so it can only see a ~/.claude that's mounted into it. The managed login avoids the problem entirely.",
  LANE_SWITCH: "One integration; the lane is switchable later.",
  VIEWER_HINT: "Operator role required",
  VIEWER_LINE:
    "You're a viewer — everything here is readable; adding, rotating, defaults and deletion need an operator.",
  STEP_LEDE:
    "Optional. Wardyn runs governed commands, interactive runs, and recordings with nothing connected. Add an integration when a run or a Wardyn feature needs one.",
  BLAST: [
    "Agent runs that resolve the server default lose model access — their first model call fails.",
    "Wardyn's Composer loses its backend — ‘Describe your task’ disappears from New Run.",
    "3 workspaces pin this integration; their runs fall back to the server default.",
    "The stored secret anthropic-api-key is not deleted — remove it under Secrets.",
  ],
  CAT_AI: "Powers a coding agent's model calls, or Wardyn's own AI features. Skip if you run governed commands or drive runs yourself.",
  CAT_SCM: "Lets runs clone from a git host. Skip if your repos are public.",
  CAT_MIRROR: "Redirects npm/pip/cargo/maven/go/nuget to a corporate mirror. Skip if public registries are reachable.",
  CAT_PROXY: "Chains wardyn-proxy through your corporate proxy. Skip if the sandbox reaches the internet directly.",
  TY_KEY: "Drives Claude Code, direct API calls, and Wardyn's features. Never resident.",
  TY_OPENAI: "Drives Codex CLI, direct API calls, and Wardyn's features.",
  TY_AZURE: "Powers Wardyn's own AI features only. Neither agent tool can be pointed at an Azure deployment.",
};

// ============================ CAPABILITY-LINE NOTES (verbatim) ============================
// mockup/wardyn-integrations.js's `CAPS` — one capability table per AI
// credential type, rendered as ON (note) / OFF (never seen here — the mock has
// no off-but-fixable row for these five) / impossible-as-fact (`fact`, always
// one of T's X_* lines above). `sub`'s Wardyn-features row depends on whether
// the subscription is the sealed host-CLI lane (hostCli) — the only
// capability row that varies by anything other than the type itself.
export interface CapabilityRow {
  label: string;
  /** Present (true) when this capability works today. */
  on?: boolean;
  /** This is the type's CURRENT default lane for the capability (informational). */
  def?: boolean;
  /** Selecting this credential type WOULD become the default for the capability. */
  makeDefault?: boolean;
  /** Shown when `on` — what actually happens. */
  note?: string;
  /** Shown instead of on/note — a flat, unconditional impossibility (verbatim T.X_*). */
  fact?: string;
}

export const CAPS = {
  key(): CapabilityRow[] {
    return [
      { label: "Claude Code", on: true, def: true, note: "Claude Code runs call Anthropic with this key." },
      { label: "Codex CLI", fact: T.X_KEY_CODEX },
      { label: "Direct API calls", on: true, note: "The proxy injects x-api-key into calls the sandbox makes itself." },
      { label: "Wardyn features", on: true, def: true, note: "Composer and review use it from the control plane." },
    ];
  },
  // Optional (defaults falsy, like the managed/non-sealed lane) so this and the
  // other four CAPS.* functions share one capabilityPreview call shape below.
  sub(hostCli?: boolean): CapabilityRow[] {
    return [
      { label: "Claude Code", on: true, makeDefault: true, note: "Claude Code signs in with the captured session." },
      { label: "Codex CLI", fact: T.X_SUB_CODEX },
      { label: "Direct API calls", fact: T.X_SUB_DIRECT },
      {
        label: "Wardyn features",
        on: !hostCli,
        note: hostCli
          ? "Opt-in — off until you switch it on."
          : "Composer sends Claude-Code-shaped requests, so the token is accepted.",
      },
    ];
  },
  bedrock(): CapabilityRow[] {
    return [
      { label: "Claude Code", on: true, makeDefault: true, note: "Claude Code calls Bedrock through the AWS chain." },
      { label: "Codex CLI", fact: T.X_BEDROCK_CODEX },
      { label: "Direct API calls", on: true, note: "SigV4-signed calls from the sandbox go out through the active lane." },
      { label: "Wardyn features", on: true, note: T.BEDROCK_FEATURES },
    ];
  },
  openai(): CapabilityRow[] {
    return [
      { label: "Claude Code", fact: T.X_OPENAI_CLAUDE },
      { label: "Codex CLI", on: true, note: "Codex CLI runs call OpenAI with this key." },
      { label: "Direct API calls", on: true, note: "The proxy injects Authorization into calls the sandbox makes itself." },
      { label: "Wardyn features", on: true, note: "Composer and review use it from the control plane." },
    ];
  },
  azure(): CapabilityRow[] {
    return [
      { label: "Claude Code · Codex CLI", fact: T.X_AZURE_HARNESS },
      { label: "Direct API calls", fact: T.X_AZURE_DIRECT },
      { label: "Wardyn features", on: true, note: "Composer and review call your Azure deployment from the control plane." },
    ];
  },
};

// ============================ STRUCTURED METADATA (this module's own design) ============================
// Everything below is NOT a mock transcription — it's metadata later
// list/add/detail pages will index into, built ON TOP of the verbatim T/CAPS
// canon above (referenced, never re-typed) so the two can't drift apart.

// ---- Categories (mockup's AddCategory radio cards) ----
export type IntegrationCategory = "ai_provider" | "scm_host" | "artifact_mirror" | "host_proxy";

export interface CategoryMeta {
  /** lucide-react export name — a pure module can't import the component itself. */
  icon: string;
  title: string;
  /** Verbatim T.CAT_* — when a run/feature doesn't need this category. */
  skipIfLine: string;
}

export const CATEGORY_META: Record<IntegrationCategory, CategoryMeta> = {
  ai_provider: { icon: "Sparkles", title: "AI provider", skipIfLine: T.CAT_AI },
  scm_host: { icon: "GitBranch", title: "SCM host", skipIfLine: T.CAT_SCM },
  artifact_mirror: { icon: "Box", title: "Artifact mirror", skipIfLine: T.CAT_MIRROR },
  host_proxy: { icon: "Network", title: "Host proxy", skipIfLine: T.CAT_PROXY },
};

// ---- AI credential types (mockup's AddTypeAI radio cards) ----
export type AiType = "anthropic_api_key" | "anthropic_subscription" | "bedrock" | "openai_api_key" | "azure_openai";

export interface AiTypeMeta {
  title: string;
  /** T.TY_* verbatim where the mock defines one; otherwise the mock's own
   *  collapsed-card description (still verbatim, just not a T.* constant). */
  desc: string;
  /** The capability-table rows a later page renders as a preview. `sub`'s
   *  hostCli param only matters for anthropic_subscription; the others ignore
   *  any argument. */
  capabilityPreview: (hostCli?: boolean) => CapabilityRow[];
}

export const AI_TYPES: Record<AiType, AiTypeMeta> = {
  anthropic_api_key: { title: "Anthropic API key", desc: T.TY_KEY, capabilityPreview: CAPS.key },
  anthropic_subscription: {
    title: "Claude subscription",
    desc: "A claude.ai login instead of a key.",
    capabilityPreview: CAPS.sub,
  },
  bedrock: {
    title: "AWS Bedrock",
    desc: "Claude models through your AWS account.",
    capabilityPreview: CAPS.bedrock,
  },
  openai_api_key: { title: "OpenAI API key", desc: T.TY_OPENAI, capabilityPreview: CAPS.openai },
  azure_openai: { title: "Azure OpenAI", desc: `${T.TY_AZURE} Key or Entra.`, capabilityPreview: CAPS.azure },
};

// ---- Residency (mockup's resChip kinds, generalized into a label+tone+tooltip
// dictionary any integration's credential can point at) ----
// control_plane is the mock's 4th resChip kind ("cp" — "control-plane side"),
// used only by Azure OpenAI (AddAzure's residencyBlock): distinct from `varies`
// (which means "more than one lane exists") — Azure has exactly one lane, and
// it simply never touches the sandbox at all.
export type ResidencyKind =
  | "proxy_injected"
  | "brokered_mint"
  | "resident_mount"
  | "resident_env"
  | "control_plane"
  | "varies";

export interface ResidencyMeta {
  label: string;
  tone: "success" | "warning" | "neutral";
  tooltip: string;
}

export const RESIDENCY_META: Record<ResidencyKind, ResidencyMeta> = {
  proxy_injected: {
    label: "proxy-injected",
    tone: "success",
    tooltip: "Held by Wardyn's store; the egress proxy injects it into outbound calls. The sandbox never holds it.",
  },
  brokered_mint: {
    label: "brokered",
    tone: "success",
    tooltip: "A short-lived, scoped credential is minted per run. Nothing long-lived is ever stored in the sandbox.",
  },
  resident_mount: {
    label: "resident",
    tone: "warning",
    tooltip: "Mounted into the sandbox as a file — the process running there can read it.",
  },
  resident_env: {
    label: "resident",
    tone: "warning",
    tooltip: "Set as an environment variable in the sandbox — the process running there can read it.",
  },
  control_plane: {
    label: "control-plane side",
    tone: "neutral",
    tooltip: "Called from Wardyn's control plane only — no sandbox lane exists for it.",
  },
  varies: {
    label: "varies by lane",
    tone: "neutral",
    tooltip: "This integration has more than one lane; residency depends on which one is active.",
  },
};

// Lane sub-choices for the two multi-lane AI types. Named to match the wire
// vocabulary already in use elsewhere (WorkspaceLLMCredMode uses "managed" for
// this exact subscription case — lib/types/workspaces.ts).
export type SubscriptionLane = "managed" | "resident_host";
export interface SubscriptionLaneMeta {
  title: string;
  residency: ResidencyKind;
  /** Verbatim T.MANAGED_LINE / T.HOSTCLI_LINE. */
  tooltip: string;
}
export const SUBSCRIPTION_LANE_META: Record<SubscriptionLane, SubscriptionLaneMeta> = {
  managed: { title: "Managed by Wardyn (container login)", residency: "proxy_injected", tooltip: T.MANAGED_LINE },
  resident_host: { title: "Host CLI login", residency: "resident_mount", tooltip: T.HOSTCLI_LINE },
};

export type BedrockLane = "bearer" | "sso" | "aws_dir" | "static";
export interface BedrockLaneMeta {
  title: string;
  residency: ResidencyKind;
  extra?: string;
}
export const BEDROCK_LANE_META: Record<BedrockLane, BedrockLaneMeta> = {
  bearer: { title: "Bearer token", residency: "proxy_injected" },
  sso: { title: "AWS SSO", residency: "resident_mount", extra: "containerized login" },
  aws_dir: { title: "Host ~/.aws profile", residency: "resident_mount", extra: "boot config" },
  // Raw access keys are exported as environment variables, not a mounted file.
  static: { title: "Access keys", residency: "resident_env" },
};

// ---- Impossible-as-fact map (never a toggle) ----
// Derived straight from CAPS' own `fact` rows above (type → capability →
// verbatim T.X_* reason) so the impossibility text can't drift from the
// capability table it's read out of.
export type AiCapability = "claude_code" | "codex_cli" | "direct_api" | "wardyn_features";

export const IMPOSSIBLE: Partial<Record<AiType, Partial<Record<AiCapability, string>>>> = {
  anthropic_api_key: { codex_cli: T.X_KEY_CODEX },
  anthropic_subscription: { codex_cli: T.X_SUB_CODEX, direct_api: T.X_SUB_DIRECT },
  bedrock: { codex_cli: T.X_BEDROCK_CODEX },
  openai_api_key: { claude_code: T.X_OPENAI_CLAUDE },
  azure_openai: { claude_code: T.X_AZURE_HARNESS, codex_cli: T.X_AZURE_HARNESS, direct_api: T.X_AZURE_DIRECT },
};

// ---- Tools tab (mockup's Harnesses frame / toolRow calls, verbatim) ----
// A tool is installable client software the image carries; an integration is
// what it connects through (T.LAW) — this copy is the "How Wardyn wires it" /
// "Image" text for each of the Tools tab's six rows. Not part of CAPS: these
// are fixed mechanism descriptions, not a capability matrix.
export type ToolRowId = "git" | "package_managers" | "claude_code" | "codex_cli" | "gh_cli" | "own_tools";
export interface ToolRowCopy {
  wire: string;
  image?: string;
  /** git's own trailing note ("In every Wardyn image.") — distinct from `image`,
   *  which names a REQUIREMENT the other rows' images must satisfy. */
  extra?: string;
}
export const TOOLS: Record<ToolRowId, ToolRowCopy> = {
  git: {
    wire: "Clone and push rerouted through the broker (App), a credential helper (PAT), or a key file (SSH) — set up at run start.",
    extra: "In every Wardyn image.",
  },
  package_managers: {
    wire: "Per-tool config files generated at run start (.npmrc, pip.conf, …); the mirror token is injected proxy-side.",
  },
  claude_code: {
    wire: "Sign-in injected proxy-side — a subscription sentinel or a key; never resident.",
    image: "Must carry the Claude Code CLI — Wardyn's built-in agent image does.",
  },
  codex_cli: {
    wire: "API key only — Wardyn has no Codex login capture.",
    image: "Must carry the Codex CLI.",
  },
  gh_cli: {
    wire: "Recognized on the host, never wired. Its token is broad; Wardyn never imports it — use an SCM host integration instead.",
  },
  own_tools: {
    wire: "Whatever your image carries. Wardyn wires nothing; your tools authenticate however they like. The self-test still runs, and governed commands skip all wiring entirely.",
  },
};
