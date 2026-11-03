/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Wall presets: the Settings card, the New Run choice and the run page's words. The strings are the jointly
// approved boards' (WP-1, WP-2, WP-3). The word "preset" also names a launch preset on the run page, so every
// string about a wall preset says "wall preset" in full where it could be read as the other.
import type { PersonalWallsSpec } from "../../../lib/api/walls";

export const WALL_PRESETS = {
  TITLE: "Wall presets",
  SUMMARY: "Named sets of walls you pick when you start a run. They only narrow what your organisation allows, and only for that run.",
  BUILT_IN: "Built in",
  ORGANISATION: "Organisation",
  GROUP_OF: (label: string | number) => `${label} group`,
  YOURS: "Yours",
  META_SHARED: (version: string | number, who: string | number, when: string | number) => `Version ${version} · saved by ${who}, ${when}`,
  META_PLAIN: (version: string | number) => `Version ${version}`,
  EDIT: "Edit",
  DELETE: "Delete",
  COPY_TO_MINE: "Copy to mine",
  NEW_PRESET: "New preset",
  SHOW: "Show what it narrows",
  HIDE: "Hide",
  // Knob chips (short) on a row.
  CHIP_SETUP_ONLY: "Only setup sites",
  CHIP_SITES: (n: string | number) => `${n} listed sites`,
  CHIP_DENIED: (n: string | number) => `${n} sites never reached`,
  CHIP_UNLISTED_REFUSED: "Unlisted sites refused",
  CHIP_NO_TRANSFERS: "No desktop transfers",
  CHIP_NO_TRANSFER: (what: string) => `No ${what}`,
  CHIP_EVERY_PUSH: "Every push refused",
  CHIP_READ_ONLY: "Read capabilities only",
  CHIP_FORGE_READ: (forge: string) => `${forge}: read only`,
  CHIP_FORGE_LIMITED: (forge: string, n: string | number) => `${forge}: ${n} ${n === 1 ? "capability" : "capabilities"}`,
  CHIP_NO_ALLOW_ALL: "No allow-all network",
  CHIP_UNLISTED_REVIEW: "Unlisted sites: ask for review",
  CHIP_UNLISTED_HELD: "Unlisted sites: held for review",
  CHIP_TOOL_RULES: (n: string | number) => `${n} tool ${n === 1 ? "rule" : "rules"}`,
  CHIP_PUSH_PATHS: (n: string | number) => `${n} push ${n === 1 ? "path" : "paths"} refused`,
  CHIP_RECORDING: "Recording kept",
  CHIP_AUTO_STOP: "Idle stop",
  CHIP_BARRIER: (cls: string | number) => `Barrier at least ${cls}`,
  CHIP_CREDENTIALS: (n: string | number) => `${n} credential ${n === 1 ? "restriction" : "restrictions"}`,
  CHIP_OPEN: "Narrows nothing",
  // The detail list of a row's disclosure.
  DETAIL_GITHUB: "GitHub keeps only",
  DETAIL_ADO: "Azure DevOps keeps only",
  DETAIL_NOT_NARROWED: "Not narrowed",
  // The form dialog.
  DIALOG_NEW: "New wall preset",
  DIALOG_EDIT: "Edit wall preset",
  DIALOG_COPY: "Copy to mine",
  DESCRIPTION: "Description",
  OPTIONAL: "Optional",
  DESCRIPTION_PLACEHOLDER: "What it is for",
  HELP_ONLY_ME: "Only you can pick it. Organisation admins and group template managers publish shared presets.",
  HELP_GROUPS: "You can also publish to the groups you manage.",
  HELP_ORG: "Everyone in the organisation can pick an organisation preset.",
  HELP_AUDIENCE_FIXED: "The audience never changes on an edit; copy it to move it.",
  EDITING_VERSION: (v: string | number) => `Editing version ${v}.`,
  COPY_SUFFIX: (name: string) => `${name} (copy)`,
  CARRIED: (knobs: string) => `Also kept, set from the CLI: ${knobs}. Change them with wardyn walls presets set.`,
  CARRIED_GITHUB: (n: string | number, reads: boolean) => `GitHub keeps only ${n} ${reads ? "read " : ""}${n === 1 ? "capability" : "capabilities"}`,
  CARRIED_ADO: (n: string | number, reads: boolean) => `Azure DevOps keeps only ${n} ${reads ? "read " : ""}${n === 1 ? "capability" : "capabilities"}`,
  CARRIED_TOOL_RULES: (n: string | number) => `${n} tool ${n === 1 ? "rule" : "rules"}`,
  CARRIED_PUSH_PATHS: "push path rules",
  CARRIED_RECORDING: "a recording floor",
  CARRIED_AUTO_STOP: "an idle stop",
  SAVE: "Save preset",
  CANCEL: "Cancel",
  SAVED: "Saved. Pick it when you start a run. Runs already started keep what they had.",
  RELOAD: "Reload",
  COPY_OPEN: "Open narrows nothing, so this copy has no limits yet. Add at least one before you save.",
  DELETE_TITLE: (name: string) => `Delete ${name}?`,
  DELETE_BODY: "Runs already launched with it keep it. A queued run that names it ends when it would start, and asks to be started again.",
  DELETE_CONFIRM: "Delete preset",
  // Not on the boards: the card's own failure and accessibility words.
  LOAD_FAILED: "Couldn't load wall presets.",
  RETRY: "Retry",
  SAVE_FAILED: "Couldn't save the wall preset.",
  DELETE_FAILED: "Couldn't delete the wall preset.",
  NAME: "Name",
  AUDIENCE: "Available to",
  ONLY_ME: "Only me",
  // A form section a server sentence names, in the section's own words: the sentence follows it.
  SECTION_NAME: "Name",
  // New Run, Tools step.
  CONTROL: "Wall preset",
  CONTROL_HELP: "Narrows this run only, on top of your walls. Open keeps your policy as it stands.",
  MORE: "More presets",
  PRESET_LINE: (name: string, clauses: string) => `The ${name} wall preset applies: ${clauses}.`,
  PRESET_LINE_NONE: (name: string) => `The ${name} wall preset applies.`,
  CLAUSE_EVERY_PUSH: "every push refused",
  CLAUSE_READ_ONLY: "read capabilities only",
  CLAUSE_FORGE_READ: (forge: string) => `${forge} read only`,
  CLAUSE_UNLISTED_REFUSED: "anything unlisted refused",
  PILL: (name: string, audience: string) => `${name} · ${audience}`,
  REVIEW_LINE: (name: string, audience: string) => `Wall preset: ${name} (${audience})`,
  AUD_BUILT_IN: "built in",
  AUD_ORG: "organisation",
  AUD_GROUP: (label: string | number) => `${label} group`,
  AUD_YOURS: "yours",
  UNAVAILABLE_UNNAMED: "The chosen wall preset",
  CLEAR_PRESET: (name: string) => `Clear ${name}`,
  UNAVAILABLE: (name: string) => `${name} is no longer available. Choose another wall preset to continue.`,
  SAVE_AS_NOTE: "A wall preset is chosen at each launch, so it is not saved with the template.",
  PUSHES_UNSTOPPABLE: "Pushes can't be stopped on this install.",
  CHANGE: "Change",
  ROW_TOOLS: "Tools",
  ROW_PUSHES: "Pushes",
  PREVIEW_PENDING: "The effective policy shows again once you choose a wall preset and the preview is checked.",
  // Pushes values (Review's Effective policy card and the run's policy document).
  PUSH_EVERY_REFUSED: "Every push refused",
  PUSH_NO_WRITE: "No pushes (no write access)",
  // Run page.
  RUN_LINE: (name: string, audience: string, version: string | number) => `Wall preset: ${name} (${audience}, version ${version}).`,
  RUN_LINE_PERSONAL: "Wall preset: a personal wall preset.",
  CHANGE_HEADING: (name: string) => `Wall preset: ${name}`,
  CHANGE_HEADING_PERSONAL: "A personal wall preset",
  CHIP: (name: string) => `Wall preset: ${name}`,
  // The Why card's wall_preset arm.
  WHY_SET_BY: (name: string) => `Wall preset: ${name}`,
  WHY_SET_BY_PERSONAL: "A personal wall preset",
  WHY_CHOSEN_BY: "Chosen by",
  WHY_CAN_CHANGE_OWN: "Not on this run. Launch a new run without it.",
  WHY_CAN_CHANGE_OTHER: "Not on this run. The owner can launch a new run with a different preset.",
  APPROVAL_OWN: (name: string) => `This run's ${name} wall preset blocks `,
  APPROVAL_OWN_AFTER: ". Launch a new run without it.",
  APPROVAL_OTHER: (owner: string, name: string) => `${owner}'s run uses the ${name} wall preset, which blocks `,
  APPROVAL_OTHER_AFTER: ".",
  PUSH_REFUSED_LABEL: "Refused: every push",
  PUSH_REFUSED_BY: (name: string) => `Refused by this run's ${name} wall preset.`,
  QUEUED_CHANGED: (name: string, queued: string | number, now: string | number) =>
    `The run was queued with the ${name} wall preset, version ${queued}. It was at version ${now} when the run would have started.`,
} as const;

const isRead = (c: string) => c.endsWith("_read") || c === "identity";

/** The knobs a form has no control for, as the Also kept row names them; none for a spec that carries none. */
export function carriedKnobWords(carried: PersonalWallsSpec): string[] {
  const W = WALL_PRESETS;
  const out: string[] = [];
  const gh = carried.github_capabilities;
  if (gh) out.push(W.CARRIED_GITHUB(gh.length, gh.every(isRead)));
  const ado = carried.azure_devops_capabilities;
  if (ado) out.push(W.CARRIED_ADO(ado.length, ado.every(isRead)));
  if (carried.tool_rules?.length) out.push(W.CARRIED_TOOL_RULES(carried.tool_rules.length));
  if (carried.push_rules && Object.keys(carried.push_rules).length > 0) out.push(W.CARRIED_PUSH_PATHS);
  if (carried.recording) out.push(W.CARRIED_RECORDING);
  if (carried.auto_stop_after_sec) out.push(W.CARRIED_AUTO_STOP);
  return out;
}
