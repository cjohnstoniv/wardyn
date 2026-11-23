/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Copy for the assistant: the Settings card, the person's list of providers it may use, and the one line
// that says why the assistant is not available. A {name} is filled in by the function that holds it.
// A string marked PROPOSED is not on the approved board yet.

export const ASSISTANT = {
  // ---- the unavailable line, one reason per state ----
  DRAFT_WITH_AI: "Draft with AI",
  OFF: "The assistant is off for your organisation.",
  OFF_LINK: "Turn it on in Settings",
  UNIT_DENIES: (provider: string) => `Your unit doesn't allow ${provider}, the assistant's model provider.`,
  NOT_CONNECTED: (provider: string) => `Connect your own credential for ${provider} to use the assistant.`,
  PROVIDER_ERROR: (provider: string) => `${provider} didn't answer. Your draft is kept. Try again in a moment.`,
  KIND_NEVER_SENT: (kind: string) => `Your organisation keeps ${kind} away from the assistant.`,
  TRY_AGAIN: "Try again",
  NO_PROVIDER: "No model provider can serve the assistant yet.", // PROPOSED
  AT_RUN_LIMIT: "You're at your run limit. End a run to start the assistant.", // PROPOSED

  // ---- Settings card ----
  TITLE: "AI assistant",
  LEAD: "Wardyn has no model and shares no credential for the assistant. You mark which providers on the list above may serve it; each person connects their own credential for one, and it is stored for them alone. The assistant drafts and explains; people save, propose and approve.",
  PURPOSES: "Purposes",
  COMPOSE: "Compose",
  COMPOSE_HINT: "Drafts runs, unit changes and templates from a chat.",
  REVIEW: "Review",
  REVIEW_HINT: "Explains a change waiting for approval, at the request of whoever is reading it. It never approves.", // PROPOSED
  PURPOSES_OFF: "Each purpose is off until you turn it on.",
  MAY_SERVE: "May serve the assistant",
  COL_PROVIDER: "Provider",
  COL_MODELS: "Models",
  LOCAL_NOTE: "A local model server keeps everything inside your network.",
  UNITS_DENY: "Units can deny a marked provider for their own people with their model provider deny list. They can't add one.",
  RUN_HISTORY: "Send run history as host counts",
  RUN_HISTORY_HINT: "Host, count and decision for the last 30 days of runs the person may read. Never prompts, output or files.",
  BUILD_FILES: "Build files the server may fetch",
  BUILD_FILES_LEAD: "Fixed by Wardyn, not a setting. Only for a repository draft, and only after a secret scan.",
  GROUP_CONTAINER: "Container and dev setup",
  GROUP_TOOLCHAIN: "Toolchain pins",
  GROUP_MANIFESTS: "Manifests",
  GROUP_LOCKFILES: "Lockfiles",
  // PROPOSED: the file names are drawn on the board, not listed as strings.
  FILES_CONTAINER: "Dockerfile, Containerfile, .devcontainer.json", // PROPOSED
  FILES_TOOLCHAIN: ".tool-versions, .nvmrc, .python-version, .ruby-version, mise.toml, rust-toolchain.toml, global.json, runtime.txt", // PROPOSED
  FILES_MANIFESTS: "package.json, go.mod, Cargo.toml, pyproject.toml, requirements.txt, Pipfile, composer.json, pom.xml, pubspec.yaml", // PROPOSED
  FILES_LOCKFILES:
    "package-lock.json, yarn.lock, pnpm-lock.yaml, go.sum, Cargo.lock, poetry.lock, pdm.lock, uv.lock, Pipfile.lock, Gemfile.lock, composer.lock, mix.lock, pubspec.lock", // PROPOSED
  BUILD_PROGRAMS:
    "Dockerfile and Containerfile are build programs, kept on purpose: the image is the point. Other build programs (Gradle build and settings files, mix.exs, Gemfile) are left out, as are files that often hold registry credentials (Maven settings.xml, .yarnrc.yml, .npmrc), compose files, CI files and Makefiles.",
  ALSO_SENT: "Also sent: these build files for a repository draft, and on review the change under review, labelled untrusted.",
  NEVER_SENT: "Never sent to the assistant",
  ADD_KIND: "Add a kind",
  REMOVE_KIND: (kind: string) => `Remove ${kind}`, // PROPOSED
  ALWAYS_ON: "Always on",
  REDACTION:
    "Policy text is cut to what the person may read, at most the security tier, and mount sources are never sent. People named in that text become Person A, Person B, put back only in the person's browser. What a person types in the chat is sent as they type it.",
  SAVE: "Save assistant settings",

  // A provider that cannot serve the assistant shows its reason and no checkboxes.
  CANNOT_SERVE: "This provider can't serve the assistant yet.", // PROPOSED
  BEDROCK: "Bedrock providers can't serve the assistant, because Wardyn can't inspect their prompts.", // PROPOSED
  DEFERS_INSPECTION: "This provider leaves prompt inspection to its gateway, so it can't serve the assistant.", // PROPOSED

  // A save that widens is held for approval and the card keeps showing what is stored.
  HELD: "Saved for approval. It applies when it is approved.", // PROPOSED
  HELD_OPEN: "Open the change", // PROPOSED
  SAVED_TOAST: "Assistant settings saved.", // PROPOSED
  SAVE_FAILED: "Couldn't save the assistant settings.", // PROPOSED
  LOAD_FAILED: "Couldn't load the assistant settings.", // PROPOSED

  // ---- a person's own list of providers ----
  MARKED: "Marked for the assistant",
  CONNECT_HINT:
    "The assistant uses your own credential for this provider. Wardyn's proxy adds it to the assistant's run; it never enters the sandbox.",
  DENIED_BY_UNIT: "Denied by your unit",
} as const;
