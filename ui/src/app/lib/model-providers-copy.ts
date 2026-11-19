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
  // Gateway presets and extra headers on a new Your own endpoint provider. A preset prefills fields and is
  // never recorded.
  PRESET_LABEL: "Start from a gateway",
  PRESET: {
    litellm: "LiteLLM",
    portkey_self_hosted: "Portkey self-hosted",
    portkey_hosted: "Portkey hosted",
    openrouter: "OpenRouter",
    other: "Other",
  },
  PRESET_HINT:
    "Fills in the fields below for that gateway and keeps what an earlier one filled. Change any of them; nothing records which one you picked.",
  HEADERS_LABEL: "Extra headers (optional)",
  HEADER_NAME_PLACEHOLDER: "Name",
  HEADER_VALUE_PLACEHOLDER: "Value",
  HEADERS_ADD: "Add header",
  HEADERS_REMOVE: "Remove",
  HEADERS_HINT:
    "Sent to your gateway on every request, for a gateway that routes on a header. Stored in clear, so never a credential: that goes in the auth header.",
  HEADERS_CAP: "At most 8 headers.",
  PORTKEY_SLUG_PLACEHOLDER: "@<slug>",
  PORTKEY_SLUG_HINT: "Your Portkey provider's slug, starting with @.",
  OPENROUTER_MODEL_PLACEHOLDER: "openai/…",
  OPENROUTER_MODEL_HINT: "OpenRouter names a model vendor first, for example openai/…",
  NAME: "Name",
  NAME_HINT: "What people see when they choose it.",
  ROUTE_THROUGH: "Route through a gateway (optional)",
  ROUTE_THROUGH_HINT: (host: string) =>
    `Send requests to your gateway instead of ${host}. Each person's key goes with them, and they are told where it goes.`,
  // A sign-in row has no key to send: the person's AWS sign-in stays with Wardyn.
  ROUTE_THROUGH_HINT_SSO: (host: string) => `Send requests to your gateway instead of ${host}.`,
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
  // How people connect to your own endpoint: paste a token (the default) or sign in at the endpoint's own
  // authority. The refusals the server sends for the sign-in block are shown as sent, never reworded here.
  OIDC: {
    CHOICE: "How people connect",
    PASTE: "Paste a token",
    SIGN_IN: "Sign in",
    SELECTED: "Selected",
    PROVIDES: "Each person signs in with their own account. You set up the connection once, below.",
    CHOICE_HINT: "Wardyn keeps each person's sign-in, renews it for their runs, and never puts it in the sandbox.",
    GROUP: "Sign-in service",
    ISSUER: "Issuer",
    ISSUER_HINT: "Where people sign in. Wardyn finds the rest from this address. https only.",
    CLIENT_ID: "Client ID",
    CLIENT_ID_HINT: "The client you registered for Wardyn at your sign-in service.",
    CLIENT_SECRET: "Client secret (optional)",
    CLIENT_SECRET_HINT:
      "Leave this empty for a public client. Otherwise choose a secret stored on the Secrets page.",
    CLIENT_SECRET_NONE: "No secret",
    CLIENT_SECRET_MISSING: (secret: string) =>
      `No secret named ${secret} is stored yet. Sign-in fails until you add it on the Secrets page.`,
    SCOPES: "Scopes",
    SCOPES_HINT: "openid and offline_access are always added. Add at least one scope your endpoint needs.",
    SCOPES_NONE: "No scopes yet.",
    SCOPES_PLACEHOLDER: "Add a scope, then Enter",
    SCOPE_REMOVE: (name: string) => `Remove ${name}`,
    CALLBACK: "Callback URL to register",
    CALLBACK_HINT: "Add this as a redirect URL on the client. It is the same for every provider.",
    COPY_CALLBACK: "Copy callback URL",
    BIND: "Match the person to their console sign-in",
    BIND_ON: "A person can connect only the account that is theirs in Wardyn. The issuer above must be the service people sign in to Wardyn with.",
    BIND_OFF:
      "Wardyn won't check that the account a person signs in with is theirs. It records the issuer and account id of each connection, so you can see which account was connected. This is no stricter than people pasting a token.",
    CONFIRM_TITLE: (name: string) => `Change how ${name} signs people in?`,
    CONFIRM_BODY: (n: number) =>
      `The sign-ins ${n} people made were for the old issuer, client, scopes or account matching, so they are deleted. Everyone who connected signs in again.`,
    CONFIRM_BODY_ONE:
      "The sign-in 1 person made was for the old issuer, client, scopes or account matching, so it is deleted. They sign in again.",
    // The note under the choice and the Save confirmation's body, one string in both places.
    TO_SIGN_IN: (n: number) =>
      `Saving this change deletes the tokens ${n} people added for this provider. Everyone must sign in before starting another run with it.`,
    TO_SIGN_IN_ONE:
      "Saving this change deletes the token 1 person added for this provider. They must sign in before starting another run with it.",
    TO_PASTE: (n: number) =>
      `Saving this change deletes the sign-ins ${n} people made for this provider. Everyone must add a token before starting another run with it.`,
    TO_PASTE_ONE:
      "Saving this change deletes the sign-in 1 person made for this provider. They must add a token before starting another run with it.",
  },
  // Run identity on a gateway (mock boards M1 and M2, their verdicts of 2026-11-02 winning over the
  // frames). Not in the canon doc: model-providers-copy.test.ts pins only the frozen rows.
  GATEWAY: {
    SWITCH: "Send the person and run to this gateway",
    DISCLOSURE:
      "When on, each model call to this gateway carries a signed token naming the person, the run and the runner.",
    // The Bedrock sign-in row's own sentence: its token rides with an AWS signature the gateway could resend.
    DISCLOSURE_BEDROCK_SSO:
      "When on, each Bedrock call carries a token naming the person, run and runner, plus an AWS signature. The gateway can resend the signed request while AWS still accepts it, but cannot change it.",
    CLAIMS_LINK: "See everything the token says",
    CLAIMS_URL: "https://github.com/cjohnstoniv/wardyn/blob/main/docs/operations/ai-gateway.md#the-claims-a-gateway-receives",
    CHIP: "Token format 1",
    MAY_CHANGE: "Within token format 1 claims are only added. The key address is stable through 0.11, and a format 2 is announced a minor release ahead.",
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
    COPIED: "Copied",
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
    INSPECT_NOT_BEDROCK: "Not available for Amazon Bedrock yet. The run's policy alone decides how Wardyn checks Bedrock prompts.",
    INSPECT_SEALED:
      "After you save this, existing runs stop starting new gateway calls within 10 minutes. Create new runs to use the change.",
    ADDRESS_CHANGED:
      "You changed where this provider sends requests. Enter the audience again for the new address. After saving, existing runs cannot refresh their gateway tokens. Create new runs to use the new address.",
    SIGNER_VAR: "WARDYN_AWS_SIGV4_AT_PROXY",
    NOT_AVAILABLE: {
      anthropic_subscription: "Not available for Claude subscriptions yet.",
      // The editor shows the variable name as code (SIGNER_VAR).
      bedrock_sso: "Needs Wardyn to sign AWS requests at its proxy. Ask your operator to turn on WARDYN_AWS_SIGV4_AT_PROXY.",
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
    SIGN_IN: "Each person signs in with their own account",
  },
  CHIP_UNBOUND: "Not bound to sign-in",
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
export function providesLine(kind: ModelProviderKind, signsIn = false): string {
  if (signsIn) return MODEL_PROVIDERS.PROVIDES.SIGN_IN;
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
