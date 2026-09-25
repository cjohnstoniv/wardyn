/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import type { ModelProviderKind } from "./types/site";

// #536 — Settings → Model providers (the admin list), #537 — the key and
// endpoint kinds' editor, and #538 — the Bedrock and Claude subscription
// kinds, byte-for-byte from docs/design/model-providers-canon.md (approved
// mock packet MP-A, the #537 packet of 2026-09-25, and #538's own section
// transcribed from mp-packet-B.html's frozen strings). A pure-data module so
// Playwright specs can import it; connection-cards.tsx cannot be (it pulls in
// xterm's CSS). The editor's Codex/Claude/Bedrock "can't drive it" reasons are
// lib/integrations.ts's INTEGRATIONS.X_* rows, reused verbatim; its AWS
// sign-in labels are lib/workspace-providers-copy.ts's AGENTS/AGENTS_DRAFT.

// Reused as the Model provider card's lede (connection-cards.tsx S.MODEL_LEDE).
export const MODEL_LEDE = "Agent runs need one. Governed commands don't.";

// The provider editor (#537, #538). PROVIDES_CLAUDE is packet B's line; the
// list reuses it for a Claude subscription row because §5.1 gives none
// (packet MP-A, A3).
export const PROVIDER_EDITOR = {
  PROVIDES_CLAUDE: "Each person signs in with their own Claude subscription.",
  KIND_TITLE: "What kind of model provider?",
  PROVIDES_KEY: "Each person adds their own key.",
  PROVIDES_TOKEN: "Each person adds their own token. You set how it is sent.",
  // #538 (Bedrock and Claude subscription kinds) — from mp-packet-B.html's own
  // frozen strings table (owner-approved 2026-09-22), transcribed into
  // docs/design/model-providers-canon.md's #538 section.
  HOW_PEOPLE_SIGN_IN: "How people sign in",
  PROVIDES_SSO:
    "Each person signs in with one click. You set up your organisation's IAM Identity Center connection once, below.",
  IDC_GROUP: "AWS IAM Identity Center",
  // Flagged in the #538 report: not in any shipped copy module (the packet
  // calls it "Reused" without naming a source; connection-cards.tsx labels
  // the same legacy Bedrock setting "Region" in prose, but no Field constant
  // existed to pin against). Owner-approved as canon 2026-09-25 (PR #1038).
  REGION: "Region",
  SSO_SETUP_HINT: "These make each person's sign-in one click.",
  MODEL_HINT_BEDROCK: "An inference profile id.",
  CLAUDE_IMAGE_MISSING:
    "Claude subscriptions need the Claude Code sign-in image, which this install hasn't built yet. See Operations → Claude sign-in image.",
  NAME: "Name",
  NAME_HINT: "What people see when they choose it.",
  ROUTE_THROUGH: "Route through a gateway (optional)",
  ROUTE_THROUGH_HINT: (host: string) =>
    `Send requests to your gateway instead of ${host}. Each person's key goes with them, and they are told where it goes.`,
  BASE_URL: "Base URL",
  BASE_URL_HINT: "https only. Wardyn sends this provider's requests here.",
  AUTH_HEADER: "Auth header",
  VALUE_FORMAT: "Value format",
  USE_WITH: "Use with",
  MODEL: "Model",
  MODEL_HINT: "Leave empty for the agent's own default.",
  PATH: "Path",
  PATH_HINT_CLAUDE: "Where your endpoint serves the Anthropic Messages API for Claude Code, e.g. /anthropic.",
  PATH_HINT_CODEX: "Where your endpoint serves the OpenAI Responses API for Codex CLI, e.g. /v1.",
  CANCEL: "Cancel",
  SAVE: "Save",
  REMOVE: "Remove",
  SAVED_TOAST: "Provider saved.",
  DELETE_TITLE: (name: string) => `Remove ${name}?`,
  DELETE_BODY: "Runs that chose it are refused until they choose another. Everyone's tokens for it are deleted.",
  DELETE_BODY_KEY: "Runs that chose it are refused until they choose another. Everyone's keys for it are deleted.",
  DELETE_BLOCKED: (name: string, harness: string) => `${name} is the default for ${harness} — choose another default first.`,
  ADDRESS_TITLE: (name: string) => `Change where ${name} sends requests?`,
  ADDRESS_BODY: (n: number) =>
    `The tokens ${n} people added were given for the old address, so they are deleted. Everyone who connected adds theirs again.`,
  ADDRESS_BODY_ONE: "The token 1 person added was given for the old address, so it is deleted. They add it again.",
  ADDRESS_BODY_KEY: (n: number) =>
    `The keys ${n} people added were given for the old address, so they are deleted. Everyone who connected adds theirs again.`,
  ADDRESS_BODY_KEY_ONE: "The key 1 person added was given for the old address, so it is deleted. They add it again.",
} as const;

export const PROVIDERS = {
  SAVE_REFUSED_TITLE_ONE: "This provider can't be saved as written",
} as const;

export const MODEL_PROVIDERS = {
  TITLE: "Model providers",
  ADD_CTA: "Add model provider",
  EMPTY_TITLE: "No model providers yet",
  EMPTY_BODY: "Agent runs need one. Add the kinds your organisation uses — each person connects their own.",
  KIND: {
    anthropic_subscription: "Claude subscription",
    bedrock_sso: "Amazon Bedrock",
    bedrock_bearer: "Amazon Bedrock",
    anthropic_api_key: "Anthropic API key",
    openai_api_key: "OpenAI API key",
    custom_endpoint: "Your own endpoint",
  } satisfies Record<ModelProviderKind, string>,
  PROVIDES: {
    SSO: "Each person signs in with one click",
    TOKEN: "Each person adds their own token",
    KEY: "Each person adds their own key",
  },
  USED_BY: (harnesses: string[]) => `Used by ${harnesses.join(", ")}`,
  // One harness, or both ("Default for Claude Code and Codex CLI", A4).
  CHIP_DEFAULT_FOR: (harnesses: string[]) => `Default for ${harnesses.join(" and ")}`,
  CONNECTED: (n: number) =>
    n === 0 ? "No one has connected yet" : n === 1 ? "Connected by 1 person" : `Connected by ${n} people`,
  CHIP_OFF: "Off",
  OFF_LINE: "Runs can't choose it.",
  OFF_STILL_DEFAULT: (harness: string) =>
    `Off — it's still the default for ${harness}, so those runs are refused until you choose another default.`,
  UNUSED: "Not used by any agent — tick one under Use with.",
  HARNESS_UNSERVED: (harness: string) =>
    `${harness} is turned on, but no model provider is set up for it. Its runs launch without model access.`,
  // Not in the packet (it draws no failed read): the console's FETCH_FAILED
  // pattern (workspace-providers-copy.ts, governance-copy.ts).
  FETCH_FAILED_TITLE: "Couldn't load model providers",
  FETCH_FAILED_BODY:
    "Something went wrong reaching the server. The providers already saved still apply — this list just can't show them right now.",
} as const;

// What each person provides, per kind (§2.2). A Bedrock bearer provider takes
// each person's own Bedrock API key, so it reads as a key.
export function providesLine(kind: ModelProviderKind): string {
  switch (kind) {
    case "anthropic_subscription":
      return PROVIDER_EDITOR.PROVIDES_CLAUDE;
    case "bedrock_sso":
      return MODEL_PROVIDERS.PROVIDES.SSO;
    case "custom_endpoint":
      return MODEL_PROVIDERS.PROVIDES.TOKEN;
    default:
      return MODEL_PROVIDERS.PROVIDES.KEY;
  }
}
