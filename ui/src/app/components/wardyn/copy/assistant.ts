/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Copy for the assistant: the Settings card, the person's list of providers it may use, and the one line
// that says why the assistant is not available. A {name} is filled in by the function that holds it.

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
  NO_PROVIDER: "No model provider can serve the assistant yet.",
  AT_RUN_LIMIT: "You're at your run limit. End a run to start the assistant.",

  // ---- Settings card ----
  TITLE: "AI assistant",
  LEAD: "Wardyn has no model and shares no credential for the assistant. You mark which providers on the list above may serve it; each person connects their own credential for one, and it is stored for them alone. The assistant drafts and explains; people save, propose and approve.",
  PURPOSES: "Purposes",
  COMPOSE: "Compose",
  COMPOSE_HINT: "Drafts runs, unit changes and templates from a chat.",
  REVIEW: "Review",
  REVIEW_HINT: "Explains a change waiting for approval when the person viewing it asks. It never approves.",
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
  FILES_CONTAINER: "Dockerfile, Containerfile, .devcontainer.json",
  FILES_TOOLCHAIN: ".tool-versions, .nvmrc, .python-version, .ruby-version, mise.toml, rust-toolchain.toml, global.json, runtime.txt",
  FILES_MANIFESTS: "package.json, go.mod, Cargo.toml, pyproject.toml, requirements.txt, Pipfile, composer.json, pom.xml, pubspec.yaml",
  FILES_LOCKFILES:
    "package-lock.json, yarn.lock, pnpm-lock.yaml, go.sum, Cargo.lock, poetry.lock, pdm.lock, uv.lock, Pipfile.lock, Gemfile.lock, composer.lock, mix.lock, pubspec.lock",
  BUILD_PROGRAMS:
    "Dockerfile and Containerfile are build programs, kept on purpose: the image is the point. Other build programs (Gradle build and settings files, mix.exs, Gemfile) are left out, as are files that often hold registry credentials (Maven settings.xml, .yarnrc.yml, .npmrc), compose files, CI files and Makefiles.",
  ALSO_SENT: "Also sent: these build files for a repository draft, and on review the change under review, labelled untrusted.",
  NEVER_SENT: "Never sent to the assistant",
  ADD_KIND: "Add a kind",
  REMOVE_KIND: (kind: string) => `Remove ${kind}`,
  ALWAYS_ON: "Always on",
  REDACTION:
    "Policy text is cut to what the person may read, at most the security tier, and mount sources are never sent. People named in that text become Person A, Person B, put back only in the person's browser. What a person types in the chat is sent as they type it.",
  SAVE: "Save assistant settings",

  // A provider that cannot serve the assistant shows its reason and no checkboxes.
  CANNOT_SERVE: "This provider can't serve the assistant yet.",
  BEDROCK: "Bedrock providers can't serve the assistant, because Wardyn can't inspect their prompts.",
  DEFERS_INSPECTION: "This provider leaves prompt inspection to its gateway, so it can't serve the assistant.",

  SAVED_TOAST: "Assistant settings saved.",
  SAVE_FAILED: "Couldn't save the assistant settings.",
  LOAD_FAILED: "Couldn't load the assistant settings.",

  // ---- a person's own list of providers ----
  MARKED: "Marked for the assistant",
  CONNECT_HINT:
    "The assistant uses your own credential for this provider. Wardyn's proxy adds it to the assistant's run; it never enters the sandbox.",
  DENIED_BY_UNIT: "Denied by your unit",

  // ---- the composer: what will be sent ----
  COMPOSER_TITLE: "AI composer",
  PREVIEW_TITLE: "What will be sent",
  PREVIEW_LEAD: (provider: string) =>
    `This is what ${provider} receives with your first message. Policy text is cut to what you may read, at most the security tier; people named in it become Person A, Person B. What you type is sent as you type it.`,
  // The harness's own additions, shown under the lead.
  PREVIEW_ADDITION:
    "Wardyn adds its own instructions and list of tools. Later answers may read more of the kinds of data shown here through Wardyn's read-only tools, never anything on the Never sent list.",
  // The label on a section whose text the model is told to read as data.
  UNTRUSTED: "Untrusted",
  NEVER_SENT_HEADING: "Never sent",
  NEVER_SECRETS: "Secret values",
  NEVER_CREDENTIALS: "Credential values",
  NEVER_SOURCE_FILES: "Source files",
  NEVER_AUDIT: "Audit trails",
  NEVER_MOUNTS: "Mount sources",
  NEVER_ROWS: "Per-run or per-person rows",
  KINDS_KEPT_AWAY: (kinds: string) => `Kinds your organisation keeps away: ${kinds}`,
  START: "Start",
  // Shown above the preview again after the records or the serving provider changed.
  PREVIEW_CHANGED: "What will be sent changed. Check it again.",

  // ---- the composer: starting ----
  STARTING: "Starting assistant…",
  RUN_POLICY: (provider: string) =>
    `The assistant runs as a run under your name, with a policy Wardyn writes: no mounts, no workspace, egress only to ${provider}, the harness's built-in tools off, and only Wardyn's read-only tools. Prompt inspection is always on, and its token use is counted.`,
  OPEN_RUN: "Open the assistant's run",
  FETCHING: "Fetching build files…",
  FETCH_LINE: "Only names on the fixed list, with your git access, secret-scanned before the assistant starts.",

  // ---- the composer: the chat and the draft ----
  PROVIDER_CHIP: (provider: string) => `Drafting with ${provider}`,
  CHAT_PLACEHOLDER: "Describe what the run needs…",
  SEND: "Send",
  PART_PROPOSED: "Proposed",
  DRAFT_LEAD: "The editor holds your draft. The assistant proposes parts; nothing is in the draft until you accept it.",
  LEFT_OUT: (host: string, unit: string) =>
    `Left out ${host}: the ceiling for unit ${unit} doesn't allow it, so a run could never reach it. The rest of the draft is as proposed.`,
  // The chat's end control, and what a collapsed tool line opens to.
  END: "End",
  ARGUMENTS: "Arguments",
  TOKENS_USED: "Tokens used",

  // ---- the composer: the tool trail and its audit fields ----
  TOOL_CALLS: "Tool calls",
  DRY_RUN: "Dry run · read-only",
  AUDIT_PERSON: "Person",
  AUDIT_PURPOSE: "Purpose",
  AUDIT_MODEL: "Model",
  AUDIT_PROMPT_DIGEST: "Prompt digest",
  AUDIT_OUTPUT_DIGEST: "Output digest",
  AUDIT_TOOLS: "Tools called",
  AUDIT_LINKED: "Linked change",
  TRANSCRIPT_KEPT: "The full transcript is kept with the assistant's run, under normal run retention.",

  // ---- the composer: 390 px ----
  DRAFT_BAR: (lines: number, left: number) => `Draft · ${lines} lines · ${left} left out`,
  SHOW_DRAFT: "Show draft",
  // Closes the full-height draft sheet.
  HIDE_DRAFT: "Back to chat",

  // ---- the composer: ending ----
  // The wording when a session has ended, and the way back to the first step.
  ENDED: "The assistant has ended this conversation. Your draft is kept.",
  START_AGAIN: "Start again",
  // The relay could not offer the model its tools.
  TOOLS_UNAVAILABLE: "The assistant can't use its tools on this server yet, so it stopped.",
} as const;
