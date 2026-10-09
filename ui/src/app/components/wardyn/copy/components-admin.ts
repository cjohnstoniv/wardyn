/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Admin copy for custom components (#1914): the organisation's catalog, its
// editor, the Settings card, the Permissions switch and the person-erasure scope.
// Lazy by construction: only the screens that read it import it, and none of
// them is in the entry chunk (bundle-split.test.ts). The strings are owner-review
// drafts. Each claims what the server does and no more (docs/CREDENTIALS.md,
// internal/types/site_config.go ComponentSettings).

export const COMPONENTS_ADMIN = {
  // ---- catalog (/admin/components) ----
  TITLE: "Components",
  LEAD: "Services you give runs access to: the hosts they reach and the secrets they use. A new component is available to nobody until you choose who may use it.",
  ADD: "Add component",
  EMPTY_TITLE: "No components yet",
  EMPTY_BODY: "A component names the hosts a service lives at and the secrets it needs. Add one, then choose who may use it.",
  COL_NAME: "Name",
  COL_REACHES: "Reaches",
  COL_SECRETS: "Secrets",
  COL_AVAILABLE: "Available to",
  N_HOSTS: (n: number) => (n === 1 ? "1 host" : `${n} hosts`),
  N_SECRETS: (n: number) => (n === 0 ? "None" : n === 1 ? "1 secret" : `${n} secrets`),
  AVAIL_NOBODY: "Nobody yet",
  AVAIL_EVERYONE: "Everyone",
  AVAIL_ONLY: (n: number) => `Only ${n} listed`,
  AVAIL_UNKNOWN: "Couldn't load",
  EDIT: "Edit",
  EDIT_ARIA: (name: string) => `Edit ${name}`,
  WHO: "Choose who",
  WHO_ARIA: (name: string) => `Choose who may use ${name}`,
  DELETE: "Delete",
  DELETE_ARIA: (name: string) => `Delete ${name}`,
  DELETE_TITLE: (name: string) => `Delete ${name}?`,
  DELETE_BODY:
    "People can no longer add it to a run. A run that already used it keeps its record, but can't be revived, restarted or extended once the component is gone. This can't be undone.",
  DELETE_CONFIRM: "Delete component",
  CANCEL: "Cancel",
  SAVED_TOAST: "Component saved.",
  DELETED_TOAST: "Component deleted.",
  DELETE_FAILED: "Couldn't delete the component.",

  // The "Available to" control for a component (availability-control.tsx).
  NOBODY_YET: "Nobody yet. Add a person, group or user type below to let them use this component.",
  ADD_FIRST: "Add at least one person, group or user type before choosing Only, or nobody could use this.",
  ONLY_HINT:
    "Only these — people not listed can't add this component to a run. Runs already going aren't affected.",

  // ---- editor ----
  EDITOR_NEW: "Add a component",
  EDITOR_EDIT: "Edit component",
  NAME: "Name",
  NAME_HINT: "People you make it available to see this name. Up to 64 characters.",
  HOSTS: "Hosts",
  HOSTS_HINT: "One per line, such as api.example.com or *.example.com. A run that uses this component can reach these hosts.",
  SECRETS_LABEL: "Secrets",
  SECRETS_HINT: "Named here, stored on the Secrets page. The value is never part of the component.",
  ADD_SECRET: "Add a secret",
  REMOVE_SECRET: (n: number) => `Remove secret ${n}`,
  SECRET_NAME: "Secret name",
  WHOSE: "Whose value",
  WHOSE_OWN: "Each person's own",
  WHOSE_SHARED: "Provided by your organisation",
  WHOSE_OWN_HINT: "Each person stores a secret of this name. A run that uses the component needs it.",
  WHOSE_SHARED_HINT: "Uses the secret of this name that you store. People who use the component never see the value.",
  WHOSE_SHARED_HEADER_ONLY: "Only a request header can use a secret you provide.",
  DELIVERY: "How it reaches the run",
  DEL_HEADER: "Request header",
  DEL_ENV: "Environment variable",
  DEL_FILE: "File",
  DEL_HEADER_HINT: "Wardyn decrypts the connection to add the header. The sandbox never holds the value.",
  DEL_RESIDENT_HINT: "Inside the sandbox for the whole run, and can't be revoked.",
  HEADER_HOST: "Host",
  HEADER_HOST_HINT: "One of the hosts above, without a port. The header goes to that host's standard HTTPS port.",
  HEADER_HOST_NONE: "Add a host above first.",
  HEADER_NAME: "Header",
  HEADER_NAME_PLACEHOLDER: "Authorization",
  HEADER_FORMAT: "Value",
  HEADER_FORMAT_PLACEHOLDER: "Bearer %s",
  HEADER_FORMAT_HINT: "Write %s where the secret goes. Leave header and value blank for Authorization: Bearer followed by the secret.",
  PLAIN_HTTP: "Also send over plain HTTP",
  PLAIN_HTTP_HINT: "Without this, the header is sent only over HTTPS. Plain HTTP puts the secret on the network unencrypted.",
  VAR: "Variable name",
  VAR_HINT: "Upper-case letters, digits and underscores. Names Wardyn manages, such as PATH and WARDYN_*, are refused.",
  FILE: "File name",
  FILE_HINT: "A name, not a path. Written under /run/wardyn/secrets/ in the sandbox.",
  CONFIG_LABEL: "Settings",
  CONFIG_HINT: "Plain values, not secrets. Each is set as an environment variable in the run.",
  ADD_SETTING: "Add a setting",
  REMOVE_SETTING: (n: number) => `Remove setting ${n}`,
  SETTING_NAME: "Name",
  SETTING_VALUE: "Value",
  SAVE: "Save component",

  // ---- Settings card ----
  SETTINGS_TITLE: "Custom components",
  SETTINGS_LEAD: "How runs that use custom components are held, and what a component may do.",
  CAP_LABEL: "Unattended runs with custom components",
  CAP_HINT: "Applies to a run that uses a component the person defined themselves. Components you add on the Components page are not affected.",
  CAP_NONE: "No restriction (default)",
  CAP_NONE_HINT: "The run is marked as reaching destinations the person added, for them before launch and in the audit log. Nothing is held or refused.",
  CAP_L1: "Hold tool calls",
  CAP_L1_HINT:
    "An unattended run's tool calls wait for approval. Shell-command runs, startup commands and auto-approved tools are refused, and so is an agent that can't hold tool calls.",
  CAP_L0: "Never",
  CAP_L0_HINT: "Unattended runs are refused. Someone has to be at the terminal.",
  RESIDENT_LABEL: "Allow environment and file delivery",
  RESIDENT_HINT:
    "Lets a component put a secret inside the sandbox as an environment variable or a file, for the whole run, with no way to revoke it. When off, components can only add a secret as a request header, and a run that uses one with a variable or file is refused.",
  VAULT_LABEL: "Require the strongest sandbox for custom credentials",
  VAULT_HINT:
    "When on, a run that sends a component's header credential to a host outside the standard coding-agent hosts needs the strongest sandbox class. Off by default.",
  WHO_LABEL: "Who may define their own components",
  WHO_BODY: "Everyone, by default. Change it on the Permissions page.",
  WHO_CTA: "Open Permissions",
  MANAGE_LABEL: "Components you provide",
  MANAGE_BODY: "The organisation's components, and who may use each.",
  MANAGE_CTA: "Open Components",
  SETTINGS_SAVE: "Save",
  SETTINGS_SAVED_TOAST: "Custom component settings saved.",
  SETTINGS_SAVE_FAILED: "Couldn't save these settings.",
  SETTINGS_LOAD_FAILED: "Couldn't load these settings.",
  SETTINGS_RETRY: "Retry",
  SETTINGS_SAVED_ELSEWHERE:
    "Someone else changed these settings while you were editing. The values shown are theirs. Make your change again and save.",
  SUMMARY_CAP: { "": "Unattended runs: no restriction", L1: "Unattended runs: tool calls held", L0: "Unattended runs: never" } as Record<string, string>,
  SUMMARY_RESIDENT_OFF: "Variable and file delivery off",
  SUMMARY_VAULT_ON: "Strongest sandbox required",

  // ---- Permissions switch row (custom_component) ----
  DEFINE_LABEL: "Custom components",
  DEFINE_BLURB: "Whether a person may define their own components, for one run or saved for later.",
  DEFINE_ON_CHIP: "Allowed",
  DEFINE_OFF_CHIP: "Turned off",
  DEFINE_ON: "Everyone signed in may define their own components. Turn this off to stop it for everyone.",
  DEFINE_OFF: "Nobody may define their own components. Components you add and make available still work.",
  DEFINE_ALLOW_CHIP: "By allow only",
  DEFINE_ALLOW:
    "The rule for SSH keys and API tokens is enforced, so only people with an allow for custom_component may define their own components.",
  DEFINE_SWITCH: "Allow people to define their own components",
  DEFINE_FAILED: "Failed to save the rule",

  // ---- person erasure (erase-data-dialog.tsx) ----
  ERASE_SCOPE_LABEL: "Saved components",
  ERASE_SCOPE_HINT:
    "The components they saved, and what each run recorded about the ones they defined. Each run keeps a row with its content removed.",
} as const;
