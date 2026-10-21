/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// M9, the local model server (#2957): the editor's fields, the provider list's
// extra lines and the run Overview's "Local models" card. Every string is
// byte-for-byte from the owner-approved M9 boards (2026-10-20). A pure-data
// module, like model-providers-copy.ts, so a Playwright spec can import it.

import { LOCAL_MODEL_LIMITS } from "./types/site";

export const LOCAL_MODEL_EDITOR = {
  DESCRIPTION: "A model server you run on a laptop or a GPU node. Runs reach it only through the run proxy.",
  BASE_URL_HINT: "The server's root, such as http://host.docker.internal:11434. No path.",
  TOKEN_SWITCH: "This server asks for a token",
  TOKEN_OFF_HINT: "Leave this off if the server needs no sign-in. Nobody then has anything to connect.",
  MODELS: "Models runs may use",
  MODELS_NONE: "No models yet.",
  MODELS_PLACEHOLDER: "Add a model name, then Enter",
  MODELS_ADD: "Add",
  MODELS_COUNT: (n: number) => `${n} of ${LOCAL_MODEL_LIMITS.models}`,
  MODELS_HINT: "Type each name exactly as the server lists it, tag included. Runs can use these models and no others.",
  // Not in the board: the chip's remove button needs a name.
  MODEL_REMOVE: (name: string) => `Remove ${name}`,
  LIMITS: "Limits for each request",
  CONTEXT: "Longest context",
  REPLY: "Longest reply",
  KEEP_ALIVE: "Keep a model loaded, seconds",
  LIMITS_HINT: "Empty means the number shown. Context and reply go up to 1,048,576. Keep loaded goes up to 86,400.",
  MODEL_HINT: "One of the models above.",
  MODEL_HINT_CLAUDE: "One of the models above. Claude Code uses it for every model tier.",
} as const;

export const LOCAL_MODEL_LIST = {
  PROVIDES_NONE: "No token. Runs reach it through the run proxy.",
  CHIP_NO_TOKEN: "No token needed",
  ADDRESS: (host: string, models: number) => `${host} · ${models} ${models === 1 ? "model" : "models"}`,
  EMPTY_EXTRA: "Local model server is one of the kinds. Until you add one, runs behave exactly as they do today.",
} as const;

// "Up to 30 minutes": the longest a run may keep a model loaded.
function keepFor(seconds: number): string {
  const unit = (n: number, one: string) => `${n} ${one}${n === 1 ? "" : "s"}`;
  if (seconds % 3600 === 0) return unit(seconds / 3600, "hour");
  if (seconds % 60 === 0) return unit(seconds / 60, "minute");
  return unit(seconds, "second");
}

export const LOCAL_MODEL_CARD = {
  TITLE: "Local models",
  // The provider as it is now: the run's own list was fixed at dispatch and is not on the run record.
  LEDE: "The provider's current models.",
  RUN_KEEPS: "The run keeps the models it launched with.",
  // "Claude Code and Codex CLI ask for this" / "Claude Code asks for this".
  ASKED_FOR: (agents: string[]) => `${agents.join(" and ")} ${agents.length === 1 ? "asks" : "ask"} for this`,
  NOT_ASKED_FOR: "Allowed, not asked for",
  SERVER: "Server",
  SERVER_NOTE: "Reached only through this run's proxy.",
  CONTEXT: "Longest context",
  REPLY: "Longest reply",
  TOKENS: (n: number) => `${n.toLocaleString("en-US")} tokens`,
  KEEP_ALIVE: "Keeps a model loaded",
  KEEP_FOR: (seconds: number) => `Up to ${keepFor(seconds)}`,
  NO_MANAGEMENT: "A run can't pull, push, create, copy or delete a model.",
} as const;
