/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The multi-provider console's strings (epic #551). Every value is a row of
// docs/design/model-providers-mock/canon.html (owner-approved 2026-09-25),
// byte for byte, under the row's own id — a copy change is a canon change.
// The Codex/Claude "can't drive it" reasons are NOT here: they are
// lib/integrations.ts's INTEGRATIONS.X_* rows, reused verbatim.
import type { ModelProviderKind } from "./types/site";

export const MODEL_PROVIDERS = {
  ADD_CTA: "Add model provider",
  KIND: {
    anthropic_api_key: "Anthropic API key",
    openai_api_key: "OpenAI API key",
    custom_endpoint: "Your own endpoint",
    anthropic_subscription: "Claude subscription",
    bedrock_sso: "Amazon Bedrock",
    bedrock_bearer: "Amazon Bedrock",
  } satisfies Record<ModelProviderKind, string>,
};

export const PROVIDER_EDITOR = {
  KIND_TITLE: "What kind of model provider?",
  PROVIDES_KEY: "Each person adds their own key.",
  PROVIDES_TOKEN: "Each person adds their own token. You set how it is sent.",
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
};

export const PROVIDERS = {
  SAVE_REFUSED_TITLE_ONE: "This provider can't be saved as written",
};
