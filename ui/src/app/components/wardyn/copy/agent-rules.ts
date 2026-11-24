/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Agent rules section on Policies and its chip on the run's Policy tab. Every string is the approved mock's
// (mocks-approved/011-org-rules/STRINGS.md); the strings shared with the person's walls form, the wall presets and the
// governance note are reused from their own copy files, not repeated here.
export const AGENT_RULES = {
  TITLE: "Agent rules",
  LEDE: "Rules for one agent, applied to every run that includes it. They only narrow what a run's policy allows. A run with Your own tools isn't bound.",
  ADD: "Add agent rules",
  EMPTY_TITLE: "No agent rules yet",
  EMPTY_BODY: "An agent with no rules runs under each run's own policy. Add rules to narrow every run that includes the agent.",
  EDIT_TITLE: (name: string) => `Edit rules for ${name}`,
  EDIT_LEDE: (name: string) => `These rules only narrow a run. Every run that includes ${name} takes them. A run with Your own tools isn't bound.`,
  AGENT: "Agent",
  AGENT_FIXED: "The agent never changes on an edit. To rule another agent, add its rules.",
  // The person form's walls words in the organisation's voice (the form's copy override).
  SITES_ANY: "Any site the run's policy allows",
  SITES_ONLY_HINT: "The run still reaches what its own setup needs: repositories, model and tools.",
  UNLISTED_POLICY: "The policy's choice",
  TOOLS_NONE: "No tool rules. Runs keep their policy's.",
  INNER_LABEL: "Harness walls",
  INNER_HELP: "How the harness's sandbox and approvals are set for every run that includes it. A run that can't launch the harness with them is refused; it never starts looser.",
  INNER_SANDBOX: "Sandbox",
  INNER_NONE: "No requirement",
  INNER_APPROVALS: "Approvals",
  INNER_NEVER: "Rules never turn the harness's sandbox off or its approvals to none.",
  APPLIES_NOTE:
    "New runs take these rules. A running sandbox picks up denied sites, blocked clipboard and file directions, kept-out credentials and refused pushes the next time it starts or restarts.",
  SAVE: "Save rules",
  DELETE_TITLE: (name: string) => `Delete the rules for ${name}?`,
  DELETE_BODY: "Runs that start after the delete aren't narrowed by these rules. Running runs keep the rules they started with.",
  DELETE_CONFIRM: "Delete rules",
  RELOAD_BODY: "Someone changed these rules after you opened them, so this save was not made. Reload to edit the current version.",
  DELETE_RELOAD_BODY: "Someone changed these rules after you opened them, so they were not deleted. Reload to see the current version.",
  ADD_LEDE: "Choose the agent these rules apply to. An agent that already has rules can't be added again.",
  ADD_REASON: "Already has rules. Edit them instead.",
  LOAD_FAILED_TITLE: "Couldn't load agent rules",
  RUN_CHIP: (name: string) => `Agent rules: ${name}`,
} as const;
