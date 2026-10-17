/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The words of New Run's Access rows (#1914): one row per component the run
// carries. Imported directly by the lazy New Run screen and never re-exported
// through copy.ts, which is in the eager entry chunk. Each sentence says no more
// than the server's fact does (internal/api's componentFact), and none names a
// secret: the server sends none to a member for an organisation-provided one.

export const ACCESS_ROWS = {
  /** The list's accessible name. */
  LIST_LABEL: "Access for this run",

  /** The status chip on a row. */
  STATUS: {
    ready: "Ready",
    needs_input: "Needs input",
    unavailable: "Unavailable",
    refused: "Refused",
    unknown: "Not checked",
  },

  /** What names a row. */
  TITLE: {
    github: "GitHub",
    azure_devops: "Azure DevOps",
    custom_unnamed: "Custom component",
  },

  /** Why the row is there, by the fact's `reason`. */
  REASON: {
    org: "Provided by your organisation.",
    self: "One you saved.",
    inline: "Added for this run only.",
    workspace: "Needed for the repositories in this run's workspace.",
    agent: "For the agent you chose.",
  },

  /** A row whose status is `unavailable`: nothing the person can supply would make it ready. */
  UNAVAILABLE: {
    custom: "Not ready: a secret your organisation provides isn't stored yet. Ask your admin.",
    git_provider: "This account has no personal connection to make here.",
  },

  /** The sentence on a row that holds Launch, and the link above Launch that names it. */
  ISSUE_NEEDS_INPUT: (name: string) => `${name} needs input before this run can launch.`,
  ISSUE_REFUSED: (name: string) => `${name} was refused, so this run can't launch.`,

  /** Under a row, whatever the run's own shape makes true of it. */
  DISCLOSURE: {
    SELF_DEFINED: "This run reaches destinations you added.",
    HEADER: "Wardyn decrypts this connection to add the header.",
    RESIDENT: "Placed inside the sandbox for the whole run; it can't be taken back while the run lasts.",
    CAP_L1: "Tool calls will wait for your approval.",
    CAP_L0: "Unattended runs are refused by your organisation.",
    HIGH_RISK: "Rated high risk in Review.",
    VAULT_FLOOR: "Runs in the strongest sandbox.",
  },

  /** The open row's parts. */
  BODY: {
    HOSTS: "Reaches",
    SECRETS: "Secrets",
    CONFIG: "Settings",
    NEEDS: "Still needed",
    ORG: "Organisation",
    REPOS: "Repositories",
    /** One secret: how it is delivered. */
    SECRET_DELIVERY: {
      header: "Sent as a header",
      env: "Set as an environment variable",
      file: "Written to a file",
    },
    SECRET_SHARED: "Provided by your admin",
    SECRET_OWN: "Yours",
    ADD_SECRET: "Add it on the Secrets page",
  },

  /** How a Git provider row reaches the repository, by the fact's `lane`. */
  LANE: {
    app: "Uses your organisation's GitHub App.",
    pat: "Uses a stored personal access token.",
    ssh: "Uses a stored SSH key.",
    entra: "Uses your Azure DevOps sign-in.",
    direct: "No credential from Wardyn; the run reaches the host as itself.",
    none: "Nothing in this run gives it a way to sign in here.",
  },
} as const;

// The Add control and the custom-component dialog (#1914). Same rules as above:
// lazy, never re-exported through copy.ts, no more than the server does. A
// header secret is "no copy placed" (docs/CREDENTIALS.md); env and file put the
// value inside the sandbox for the whole run.

export const ADD_ACCESS = {
  BUTTON: "Add access",
  TITLE: "Add access to this run",
  LEAD: "Pick what this run may reach. Nothing is added until you choose it.",
  LOADING: "Loading what you can add.",
  LOAD_FAILED: "Couldn't load what you can add.",
  CUSTOM: "Custom component",
  CUSTOM_HINT: "Name the hosts a service lives at and the secrets it needs.",
  MINE: "Your saved components",
  ORG: "Provided by your organisation",
  NOT_ALLOWED: "Your organisation doesn't let you define your own components.",
  ADD: "Add",
  ADDED: "Added",
  FULL: (max: number) => `A run can carry at most ${max} components.`,
  REACHES: (hosts: string[]) => (hosts.length === 0 ? "No hosts" : hosts.join(", ")),
  CLOSE: "Close",
  REMOVE: "Remove from this run",
  REMOVE_NAMED: (name: string) => `Remove ${name} from this run`,
} as const;

export const CUSTOM_COMPONENT = {
  TITLE_NEW: "Custom component",
  TITLE_EDIT: "Edit component",
  LEAD: "A component names the hosts a service lives at and the secrets it needs. Secret values are never typed here: you add them on the Secrets page.",

  /** Where the definition goes. */
  KEEP: "Use it",
  KEEP_RUN: "For this run only",
  KEEP_SAVE: "Save to reuse",
  KEEP_SAVE_HINT: "Saved under your name. You can edit or delete it under Your account.",

  NAME: "Name",
  NAME_HINT_RUN: "Optional for this run only. It labels the row.",
  NAME_HINT_SAVE: "What you will pick it by. Two of yours can't share a name.",
  HOSTS: "Hosts it reaches",
  HOSTS_HINT: "One per line, such as api.example.com or *.example.com. Names only: an IP address is refused. Add :443 to allow one port.",

  SECRETS: "Secrets",
  SECRETS_HINT: "Name a secret you have stored. Its value stays on the Secrets page and is never shown here.",
  ADD_SECRET: "Add a secret",
  REMOVE_SECRET: (n: number) => `Remove secret ${n}`,
  SECRET_NAME: "Stored secret name",
  DELIVERY: "How it reaches the run",
  DELIVERY_HEADER: "Request header",
  DELIVERY_ENV: "Environment variable",
  DELIVERY_FILE: "File",
  DELIVERY_HEADER_HINT: "Wardyn adds it to requests to one host, over HTTPS only. No copy is placed in the sandbox.",
  DELIVERY_ENV_HINT: "Set in the sandbox for the whole run. Anything running there can read it, and it can't be taken back while the run lasts.",
  DELIVERY_FILE_HINT: "Written to a file under /run/wardyn/secrets/ for the whole run. Anything running there can read it, and it can't be taken back while the run lasts.",
  HEADER_HOST: "Host",
  HEADER_HOST_HINT: "One of the hosts above, without a port. The header goes to that host's standard HTTPS port.",
  HEADER_HOST_NONE: "Add a host above first.",
  HEADER_NAME: "Header",
  HEADER_NAME_PLACEHOLDER: "Authorization",
  HEADER_FORMAT: "Value",
  HEADER_FORMAT_PLACEHOLDER: "Bearer %s",
  HEADER_FORMAT_HINT: "Write %s where the secret goes. Leave header and value blank for Authorization: Bearer followed by the secret.",
  VAR: "Variable name",
  VAR_HINT: "Upper-case letters, digits and underscores. Names such as PATH and WARDYN_* are refused.",
  FILE: "File name",
  FILE_HINT: "A name, not a path: lower-case letters, digits, '.', '_' and '-'.",

  SETTINGS: "Settings",
  SETTINGS_HINT: "Plain, non-secret values set as environment variables in the sandbox.",
  ADD_SETTING: "Add a setting",
  REMOVE_SETTING: (n: number) => `Remove setting ${n}`,
  SETTING_NAME: "Name",
  SETTING_VALUE: "Value",

  SAVE: "Save",
  ADD_TO_RUN: "Add to this run",
  CANCEL: "Cancel",
  /** Printed at the top of a refused form, above the field sentences it names. */
  REFUSED: "This component can't be saved as written. Nothing you typed has been removed.",
  REFUSED_RUN: "This component can't be added as written. Nothing you typed has been removed.",

  /** What a save answer says is still needed (D18). */
  SAVED: (name: string) => `${name} is saved.`,
  NEEDS_TITLE: "Needs input before a run can launch",
  NEEDS_SECRET: (name: string) => `Secret ${name} isn't stored yet.`,
  NEEDS_ADD: "Add it on the Secrets page",
  DONE: "Done",

  /** One sentence per rule the form checks, each naming the field's own value. */
  ERR: {
    NAME_EMPTY: "Give it a name.",
    NAME_SHAPE: (max: number) => `The name must be 1 to ${max} characters on one line.`,
    HOST_EMPTY: "A host line is empty.",
    HOST_URL: (h: string) => `"${h}" must be a bare host, not a URL.`,
    HOST_WILDCARD: (h: string) => `"${h}": a "*" is only allowed as a leading "*.".`,
    HOST_PORT: (h: string) => `"${h}": the port must be a number from 1 to 65535.`,
    HOST_ADDRESS: (h: string) => `"${h}" must be a DNS name, not an IP address.`,
    HOST_CHARSET: (h: string) => `"${h}" may use only ASCII letters, digits, '-' and '.'. Write an international name as punycode (xn--).`,
    HOST_SPELLING: (h: string, canon: string) => `Write "${h}" as "${canon}".`,
    HOST_TWICE: (h: string) => `"${h}" is listed twice.`,
    HOSTS_COUNT: (n: number, max: number) => `${n} hosts is over the limit of ${max}.`,
    SECRETS_COUNT: (n: number, max: number) => `${n} secrets is over the limit of ${max}.`,
    SECRET_NAME_EMPTY: "Name the stored secret.",
    SECRET_NAME_SHAPE: (n: string) => `"${n}" isn't a valid secret name: lower-case letters, digits, '.', '_' and '-'.`,
    RESIDENT_OFF: "Your organisation has turned off environment-variable and file delivery. Use a request header.",
    HEADER_HOST: "Pick the host the header goes to, from the hosts above.",
    HEADER_NAME: (n: string) => `"${n}" isn't a valid header name.`,
    FORMAT_VERB: (f: string) => `"${f}" must contain %s exactly once, and no other %.`,
    FORMAT_TEXT: (max: number) => `The value must be printable text of at most ${max} bytes.`,
    VAR_EMPTY: "Name the variable.",
    ENV_TOO_LONG: (max: number) => `Must be at most ${max} characters.`,
    ENV_SHAPE: (n: string) => `"${n}" must use upper-case letters, digits and underscores, and not start with a digit.`,
    ENV_WARDYN: (n: string) => `"${n}" is reserved: WARDYN_* configures the sandbox itself.`,
    ENV_RESERVED: (n: string) => `"${n}" is reserved: it decides how programs in the sandbox start or reach the network.`,
    FILE_SHAPE: (f: string) => `"${f}" must be a file name of lower-case letters, digits, '_', '.' and '-', starting with a letter or digit, at most 63 characters.`,
    SAME_PLACE: (n: number) => `This delivers to the same place as secret ${n}.`,
    CONFIG_COUNT: (n: number, max: number) => `${n} settings is over the limit of ${max}.`,
    SETTING_TWICE: (n: string) => `"${n}" is listed twice.`,
    SETTING_VALUE: (max: number) => `The value must be printable text on one line of at most ${max} bytes.`,
    SETTING_IS_VAR: (n: string) => `"${n}" is also the variable a secret is delivered in.`,
  },
} as const;

/** Your account: the card of saved components. */
export const MY_COMPONENTS = {
  TITLE: "Your components",
  SUMMARY: (n: number) => (n === 0 ? "None saved" : n === 1 ? "1 saved" : `${n} saved`),
  LEAD: "Components you saved to reuse in runs. A run only gets one when you add it on New run.",
  NEW: "New component",
  EDIT: "Edit",
  DELETE: "Delete",
  EMPTY: "You haven't saved any components.",
  NOT_ALLOWED: "Your organisation doesn't let you define your own components, so you can't save new ones. You can still delete the ones below.",
  LOAD_FAILED: "Couldn't load your components.",
  RETRY: "Retry",
  DELETE_BODY: "Runs that already started with it keep what they were given. New runs can no longer add it.",
  REACHES: (n: number) => (n === 1 ? "1 host" : `${n} hosts`),
  SECRETS: (n: number) => (n === 0 ? "no secrets" : n === 1 ? "1 secret" : `${n} secrets`),
} as const;

/** The Secrets page for a person who is not an administrator: their own rows. */
export const OWN_SECRETS = {
  EMPTY: "Add an API key or access token so your runs can use it by name. It is stored under your name, and the value is write-only: it is never shown again, not even to you.",
  FROM_ADMIN: "Provided by your admin",
  FROM_ADMIN_TITLE: "Your admin stored this secret. You can't change or remove it here.",
} as const;
