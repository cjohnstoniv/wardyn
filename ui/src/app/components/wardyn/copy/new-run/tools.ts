/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// New Run canon strings for step 5, Tools and access, byte for byte from docs/design/new-run-canon.md.
// Lazy-only: imported by the New Run flow and never re-exported through copy.ts, whose barrel is in the eager
// entry chunk. new-run-copy.test.ts keeps this file and the doc equal in both directions.

export const NR_TOOLS = {
  TITLE: "What else does it need?",
  LEDE:
    "Everything your earlier choices bring is listed with the access it adds. Add the CLIs, MCP servers, APIs and secrets the run needs, then finish any setup. Wardyn adds each tool's hosts and credentials for you, and your organisation's policy still applies at review.",
  INCLUDED_TITLE: "Already included",
  INCLUDED_BODY:
    "Added by your harness, repositories and drives. Open one to see the hosts and credentials it brings.",
  HARNESS_SETUP_TITLE: "Harness setup",
  HARNESS_SETUP_BODY:
    "Choose each harness's model and how its tool calls are handled. MCP servers you add below can be turned on per harness.",
  ADD_TITLE: "Add tools and access",
  ADD_BODY:
    "Give the run tools that aren't covered by a harness, repository or drive. Each one brings its own hosts and credentials.",
  CUSTOM_COMPONENT: "Custom component",
  CATEGORY_ARIA: "Category",
  CATEGORY_ALL: "All",
  CATEGORY_CLI: "CLIs",
  CATEGORY_MCP: "MCP servers",
  CATEGORY_API: "APIs",
  CATEGORY_SECRET: "Secrets and hosts",
  ADDED_HEADING: "Added to this run",
  FROM_ORG: "From your organisation",
  CARD_ADD: "Add",
  CARD_REMOVE: "Remove",
  ROW_REMOVE: "Remove",
  FROM_HARNESS: "Harness",
  FROM_REPOSITORY: "Repository",
  FROM_DRIVE: "Drive",
  FROM_COMMAND: "Command",
  FROM_CLI: "CLI",
  FROM_MCP: "MCP server",
  FROM_API: "API",
  FROM_SECRET: "Secret",
  FROM_HOST: "Host",
  FROM_CUSTOM: "Custom",
  SUB_STARTS: "Starts with the run",
  SUB_AVAILABLE: "Available inside the sandbox",
  SUB_COMMAND: "Runs on its own; no harness",
  SUB_NO_CREDENTIAL: "No credential",
  SUB_SAVED: "Saved to your components",
  SUB_RUN_ONLY: "This run only",
  NOTE_COMMAND: "Package registry access comes from your organisation's default policy.",
  NOTE_NONE_NEEDED: "No network access or credentials needed.",
  /** pending W-35: the recommended text until the owner answers. */
  REPO_SUB: "Via Wardyn's git broker · this run's repositories only",
  STATUS_NEEDS_SETUP: "Needs setup",
  STATUS_READY: "Ready",
  STATUS_HELD: "Held for approval",
  DETAILS_FOR: (name: string | number) => `Details for ${name}`,
  DETAIL_NETWORK: "Network",
  DETAIL_CREDENTIAL: "Credential",
  DETAIL_APPROVAL: "Approval",
  DETAIL_MCP: "MCP",
  DETAIL_IMAGE: "Image",
  DETAIL_NOTE: "Note",
  DETAIL_NONE: "None",
  DETAIL_MCP_BODY: "Turn it on for a harness under Harness setup.",
  /** pending W-34: the recommended text until the owner answers. */
  IMAGE_NO: "Not in this sandbox's image. The access still works if the tool is installed another way.",
  /** pending W-34: the recommended text until the owner answers. */
  IMAGE_UNKNOWN: "Wardyn can't tell whether this sandbox's image has it.",
  NO_HARNESSES:
    "No harnesses in this run. Add one on the Harnesses step if the sandbox should include an agent tool.",
  PROVIDER_LABEL: "Model provider",
  PROVIDER_CHOOSE: "Choose a model provider",
  PROVIDER_REQUIRED: "Required before launch.",
  PROVIDER_PROXY: "The proxy adds the provider's credential; the sandbox never holds it.",
  PROVIDER_SET: (name: string | number) => `Model provider set for ${name}.`,
  DEFAULT_LABEL: "When a tool has no rule",
  EFFECT_ALLOW: "Run it",
  EFFECT_HOLD: "Ask first",
  EFFECT_DENY: "Block it",
  /** pending W-30: the recommended text until the owner answers. */
  DEFAULT_HELP_GATED: "Ask first holds the call until an admin approves it.",
  /** pending W-30: the recommended text until the owner answers. */
  DEFAULT_HELP_OPERATOR: "Ask first holds the call until you or another admin approves it.",
  /** pending W-30: the recommended text until the owner answers. */
  DEFAULT_HELP_ATTENDED: "Ask first: Claude Code asks you in the terminal before it runs the tool.",
  /** pending W-30: the recommended text until the owner answers. */
  PROFILE_REQUIRES: (profile: string | number) =>
    `${profile} requires Ask first or stricter for tools with no rule.`,
  CODEX_HOLD_TIP: "Codex CLI can't pause a tool call for approval.",
  /** pending W-32: the recommended text until the owner answers. */
  CODEX_NOTE:
    "Codex CLI can't pause a tool call for approval, and Wardyn can't block its own tools. Rules for its MCP tools are enforced by the proxy.",
  RULES_LABEL: "Tool rules",
  ADD_RULE: "Add a tool rule",
  MCP_FOR_HARNESS: "MCP servers for this harness",
  MCP_FOLLOWS: "Calls to an MCP server follow this harness's tool rules, as mcp__<server>__<tool>.",
  MCP_NONE: "None added. Add an MCP server below and it appears here.",
  /** pending W-44: the recommended text until the owner answers. */
  MCP_ASKS: "The harness asks before an MCP tool runs; Wardyn does not hold the call.",
  RULE_TITLE: (name: string | number) => `Add a tool rule for ${name}`,
  /** pending W-31: the recommended text until the owner answers. */
  RULE_INTRO:
    "Name one tool exactly, for example WebFetch or mcp__jira__create_issue. Rules for parts of a command are coming soon.",
  RULE_TOOL: "Tool",
  /** pending W-31: the recommended text until the owner answers. */
  RULE_PLACEHOLDER: "WebFetch",
  RULE_EFFECT: "What happens",
  RULE_CONFIRM: "Add rule",
  RULE_ADDED: "Rule added.",
  RULE_ERR_TOOL: "Enter a tool name.",
  RULE_ERR_DUPLICATE: "That tool already has a rule.",
  SECRET_CARD_TITLE: "Add a secret",
  SECRET_CARD_BODY:
    "Give the run one of your saved secrets as an environment variable, a file, or a header the proxy adds.",
  SECRET_CARD_BUTTON: "Add secret",
  HOST_CARD_TITLE: "Allow a host",
  HOST_CARD_BODY:
    "Let the run reach one more host with no credential. Your organisation's ceiling still applies.",
  HOST_CARD_BUTTON: "Allow host",
  MCP_CARD_TITLE: "Add an MCP server",
  MCP_CARD_BODY: "Connect a remote MCP server by URL. Its token stays with the proxy.",
  MCP_CARD_BUTTON: "Add MCP server",
  CLI_GITHUB_BODY: "Actions and the REST API with gh; issue and pull request lists aren't supported yet.",
  CARD_ADDED: (name: string | number) => `${name} added. Its hosts and credential are included.`,
  COMPONENT_ADDED: "Added to this run.",
  COMPONENT_ADDED_NAMED: (name: string | number) => `${name} added to this run.`,
  COMPONENT_REMOVED: "Removed.",
  ADD_TO_RUN: "Add to run",
  /** pending W-39: the recommended text until the owner answers. */
  SECRET_INTRO:
    "Choose one of your saved secrets and how the run receives it. Prefer a request header: no copy is placed in the sandbox.",
  SECRET_LABEL: "Secret",
  SECRET_HELP: "Values are stored encrypted and never shown again. Add new secrets under Your account.",
  DELIVERY_LABEL: "Delivery",
  /** pending W-39: the recommended text until the owner answers. */
  DELIVERY_HEADER_OPTION: "Request header",
  /** pending W-39: the recommended text until the owner answers. */
  DELIVERY_ENV_OPTION: "Environment variable",
  /** pending W-39: the recommended text until the owner answers. */
  DELIVERY_FILE_OPTION: "File",
  /** pending W-39: the recommended text until the owner answers. */
  SECRET_HOST_LABEL: "Host",
  /** pending W-39: the recommended text until the owner answers. */
  SECRET_VARIABLE_LABEL: "Variable name",
  /** pending W-39: the recommended text until the owner answers. */
  SECRET_FILE_LABEL: "File name",
  /** pending W-39: the recommended text until the owner answers. */
  SECRET_FILE_PATH: (name: string | number) => `/run/wardyn/secrets/${name}`,
  SECRET_ERR_HOST: "Enter the host the proxy adds it for.",
  SECRET_ERR_VARIABLE: "Enter the variable name.",
  SECRET_ERR_FILE: "Enter the file name.",
  HOST_INTRO:
    "The run can reach this host with no credential. If your organisation's ceiling doesn't allow it, review shows why.",
  HOST_LABEL: "Host",
  HOST_PLACEHOLDER: "e.g. pypi.org or *.example.com",
  HOST_ERR: "Enter a host.",
  /** pending W-41: the recommended text until the owner answers. */
  MCP_INTRO:
    "A remote MCP server reached over HTTPS. If it needs a token, Wardyn adds it to each request; no copy is placed in the sandbox. Servers that need you to sign in are coming soon.",
  MCP_NAME: "Name",
  MCP_NAME_PLACEHOLDER: "e.g. Linear",
  MCP_URL: "Server URL",
  /** pending W-41: the recommended text until the owner answers. */
  MCP_URL_PLACEHOLDER: "https://mcp.example.com/mcp",
  MCP_TOKEN: "Token",
  MCP_NO_TOKEN: "No token",
  MCP_LOCAL_SOON: "Servers that run inside the sandbox (a local command) are Coming soon.",
  MCP_ERR_URL: "Enter the server URL.",
  CC_INTRO:
    "Bundle the hosts and secrets one tool needs, so you can reuse it. Saved components appear under Add tools and access next time.",
  CC_NAME: "Name",
  CC_NAME_PLACEHOLDER: "e.g. Billing service API",
  CC_HOSTS: "Hosts",
  CC_HOSTS_PLACEHOLDER: "billing.acme.internal, api.stripe.com",
  CC_SECRET: "Secret",
  CC_SECRET_NONE: "None",
  CC_SECRET_HELP: "Delivered by the proxy as a header unless you choose otherwise when editing it.",
  CC_SAVE: "Save to my components",
  CC_ERR: "Enter a name and at least one host.",
  /** pending W-40: the recommended text until the owner answers. */
  CC_HEADER_HOST: "Header host",
  /** pending W-40: the recommended text until the owner answers. */
  CC_MORE_OPTIONS: "More options",
  TOTALS_ARIA: "What this run can reach",
  NETWORK_TITLE: "Network",
  CREDENTIALS_TITLE: "Credentials",
  NO_HOSTS: "No hosts.",
  NO_CREDENTIALS: "No credentials.",
  FROM_ROW: (name: string | number) => `from ${name}`,
  FOR_ROW: (name: string | number) => `for ${name}`,
  NET_ALLOWED: "Allowed",
  NET_NEEDS_APPROVAL: "Needs approval",
  /** pending W-35: the recommended text until the owner answers. */
  NET_REQUESTED: "Requested",
  /** pending W-35: the recommended text until the owner answers. */
  NET_NOT_ALLOWED: "Not allowed",
  /** pending W-36: the recommended text until the owner answers. */
  NET_POLICY_DENY_REVIEW_YOU:
    "Anything else is refused the first time the run asks for it and raises a request. Once you approve it, the run can try again.",
  /** pending W-36: the recommended text until the owner answers. */
  NET_POLICY_DENY_REVIEW_ADMIN:
    "Anything else is refused the first time the run asks for it and raises a request. Once an admin approves it, the run can try again.",
  /** pending W-36: the recommended text until the owner answers. */
  NET_POLICY_ALWAYS_DENY: "Anything else is refused and logged.",
  NET_POLICY_HELD_YOU: "Anything else is held for your review the first time the run asks for it.",
  /** pending W-36: the recommended text until the owner answers. */
  NET_POLICY_HELD_ADMIN: "Anything else is held for an admin's review the first time the run asks for it.",
  /** pending W-36: the recommended text until the owner answers. */
  NET_POLICY_TIMEOUT: (n: string | number) => `If nobody decides within ${n} seconds, it's refused.`,
  /** pending W-36: the recommended text until the owner answers. */
  NET_POLICY_ALLOW_ALL: "Any other public host is allowed, except ones your organisation blocks.",
  NET_POLICY_CEILING: "Your organisation's ceiling still narrows this list at review.",
  /** pending W-36: the recommended text until the owner answers. */
  NET_POLICY_CHECKED: "Checked against your organisation's limits just now.",
  /** pending W-36: the recommended text until the owner answers. */
  NET_POLICY_PROFILE: (profile: string | number) => `${profile}'s ceiling still narrows this list at review.`,
  /** pending W-36: the recommended text until the owner answers. */
  NET_DECIDER: "An admin decides each new host.",
  /** pending W-33: the recommended text until the owner answers. */
  CREDENTIALS_POLICY:
    "Short-lived credentials are issued when the run first needs them and renewed while it runs. 'Proxy only' means no copy is placed in the sandbox.",
  DELIVERY_PROXY: "Proxy only",
  DELIVERY_ENV: "In the sandbox as an environment variable",
  DELIVERY_FILE: "In the sandbox as a file",
  /** pending W-33: the recommended text until the owner answers. */
  DELIVERY_SHORT_LIVED: "In the sandbox, short-lived",
  /** pending W-33: the recommended text until the owner answers. */
  DELIVERY_GIT_BROKER: "Via Wardyn's git broker",
  /** pending W-33: the recommended text until the owner answers. */
  DELIVERY_GIT_HELPER: "Given to git when it asks",
  /** pending W-33: the recommended text until the owner answers. */
  DELIVERY_AZURE: "Proxy only · renewed every hour or so",
  /** pending W-33: the recommended text until the owner answers. */
  DELIVERY_GITHUB:
    "Proxy only · acts as your organisation's Wardyn GitHub App, limited to this run's repositories",
  /** pending W-33: the recommended text until the owner answers. */
  DELIVERY_AWS: (session: string | number) =>
    `Your AWS role credentials are issued into the sandbox for up to ${session} at a time. Wardyn renews them while the run lives and stops when it ends; ones already issued stay valid at AWS until they expire.`,
  SETUP_TITLE: "Setup still needed",
  SETUP_BODY: "Finish these here, or review now and they appear as blockers before launch.",
  SETUP_ALL_SET: "Nothing else to set up.",
  SETUP_DONE: "Done",
  SETUP_SIGN_IN: "Sign in",
  SETUP_CHOOSE: "Choose",
  SETUP_ADD: "Add",
  SETUP_CHOOSE_PROVIDER: (name: string | number) => `Choose a model provider for ${name}`,
  /** pending W-38: the recommended text until the owner answers. */
  SETUP_SIGN_IN_TO: (provider: string | number) => `Sign in to ${provider}`,
  /** pending W-54: a stored sign-in's door, the recommended text until the owner answers. */
  SETUP_SIGN_IN_AGAIN: "Sign in again",
  SIGN_IN_INTRO:
    "You'll sign in with your own account in a new window. Wardyn keeps a refresh grant and issues the run a short-lived credential when it starts. The sandbox never sees your sign-in.",
  SIGN_IN_CONFIRM: "Continue to sign in",
  SIGNED_IN: "Signed in. The credential is issued when the run starts.",
  TOKEN_INTRO:
    "Paste the token once. It's stored encrypted as one of your secrets, and the proxy adds it to requests, so the sandbox never holds it.",
  TOKEN_LABEL: "Token",
  TOKEN_CONFIRM: "Save token",
  TOKEN_SAVED: "Token saved.",
  STICKY_ONE: "1 item still needs setup",
  STICKY_MANY: (n: string | number) => `${n} items still need setup`,
  STICKY_DONE: "Everything is set up",
  REVIEW_RUN: "Review run",
} as const;
