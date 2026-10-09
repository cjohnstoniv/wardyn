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
