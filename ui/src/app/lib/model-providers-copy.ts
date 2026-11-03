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
// sign-in labels are lib/workspace-providers-copy.ts's AGENTS/AGENTS_EXTRA.

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
  REGION_BEARER: "AWS region",
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
  SAVE: "Save provider",
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
  // Run identity on a gateway (mock boards M1 and M2, their verdicts of 2026-11-02 winning over the
  // frames). Not in the canon doc: model-providers-copy.test.ts pins only the frozen rows.
  GATEWAY: {
    SWITCH: "Send the person and run to this gateway",
    DISCLOSURE:
      "When on, each model call to this gateway carries a signed token naming the person, the run and the runner.",
    CLAIMS_LINK: "See everything the token says",
    CLAIMS_URL: "https://github.com/cjohnstoniv/wardyn/blob/main/docs/operations/ai-gateway.md#the-claims-a-gateway-receives",
    CHIP: "Experimental until 0.11",
    MAY_CHANGE: "The token format and the key address may change before 0.11.",
    AUDIENCE: "Token audience",
    AUDIENCE_HINT: "The value your gateway looks for in each token. Use a different one for each provider.",
    ISSUER: "Issuer",
    ISSUER_HINT: "Your gateway checks that each token came from here.",
    ISSUER_DEFAULT: "This is the default name. Two Wardyn installs that both use it look the same to one gateway.",
    KEY_ADDRESS: "Key address",
    KEY_ADDRESS_HINT: "Your gateway reads the public key here to check each token.",
    KEY_ADDRESS_OTHER: "If your gateway reaches Wardyn at another address, use that address with this path.",
    KEY_ADDRESS_UNSAVED: "Available after identity is saved; it can take up to 30 seconds.",
    COPY: "Copy",
    COPY_ISSUER: "Copy issuer",
    COPY_KEY_ADDRESS: "Copy key address",
    OFF_NOTE:
      "After you save this, existing runs stop starting new gateway calls within 10 minutes. Create new runs to use this provider with identity off.",
    NEEDS_ADDRESS: (host: string) =>
      `Add a gateway address above to turn this on. Without one, calls go straight to ${host}.`,
    ADDRESS_CLEARED: "This needs a gateway address. Add one, or turn this off.",
    // gateway_identity.inspection: who checks the prompts sent to this gateway.
    INSPECT_HEADING: "Inspection",
    INSPECT_SWITCH: "Let this gateway check prompts",
    INSPECT_OFF: "Turn this on if your gateway already checks prompts, so Wardyn doesn't check them twice.",
    INSPECT_ON:
      "Wardyn keeps the run's secret-value check when that check is enabled by its policy. Other prompt checks are left to this gateway.",
    INSPECT_SEALED:
      "After you save this, existing runs stop starting new gateway calls within 10 minutes. Create new runs to use the change.",
    ADDRESS_CHANGED:
      "You changed where this provider sends requests. Enter the audience again for the new address. After saving, existing runs cannot refresh their gateway tokens. Create new runs to use the new address.",
    NOT_AVAILABLE: {
      anthropic_subscription: "Not available for Claude subscriptions yet.",
      bedrock_sso: "Not available when people sign in with SSO. Use a bearer key row, or add the gateway as your own endpoint.",
      local_model: "Not available for a local model server, which is not a gateway.",
    },
  },
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
    local_model: "Local model server",
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
    case "local_model": // with a token; the list says so for one without (LOCAL_MODEL_LIST.PROVIDES_NONE)
      return MODEL_PROVIDERS.PROVIDES.TOKEN;
    default:
      return MODEL_PROVIDERS.PROVIDES.KEY;
  }
}
