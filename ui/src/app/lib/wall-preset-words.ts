/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Wall presets in the console's words: the knob chips on a row, the audience a preset is for, and which form section
// a server sentence names. Shared by the Settings card and the New Run choice.
import { CC_META } from "../components/wardyn/cc-meta";
import { NR_WALLS } from "../components/wardyn/copy/personal-walls";
import { WALL_PRESETS as W } from "../components/wardyn/copy/wall-presets";
import type { PersonalWallsSpec } from "./api/walls";
import type { WallPreset } from "./types/wall-presets";

/** A read capability: the console's own test, the one the built-in Read-only keeps. */
export const isRead = (c: string) => c.endsWith("_read") || c === "identity";

/** Both forge capability lists are named and name reads only: the one case "Read capabilities only" is true. */
export const readsOnly = (s: PersonalWallsSpec): boolean =>
  !!s.github_capabilities && !!s.azure_devops_capabilities && s.github_capabilities.every(isRead) && s.azure_devops_capabilities.every(isRead);

/** What a spec says of each forge it names a list for, when it is not the both-lists case: reads only, or a list that still holds writes. */
export function forgeLimits(s: PersonalWallsSpec): { forge: "github" | "ado"; reads: boolean; n: number }[] {
  if (readsOnly(s)) return [];
  const one = (forge: "github" | "ado", list?: string[]) => (list ? [{ forge, reads: list.every(isRead), n: list.length }] : []);
  return [...one("github", s.github_capabilities), ...one("ado", s.azure_devops_capabilities)];
}

/** The directions a spec blocks, in the order the form lists them. */
export const blockedTransfers = (s: PersonalWallsSpec): string[] =>
  Object.entries(NR_WALLS.APPLY_TRANSFERS).filter(([dir]) => s.desktop?.[dir as keyof NonNullable<PersonalWallsSpec["desktop"]>] === "blocked").map(([, word]) => word);

/**
 * The short chips of a row: each knob the preset names. "Narrows nothing" only for a spec that names no limit at all,
 * so a knob with no chip of its own can never read as nothing.
 */
export function presetChips(s: PersonalWallsSpec): string[] {
  const chips: string[] = [];
  if (s.allowed_domains) chips.push(s.allowed_domains.length > 0 ? W.CHIP_SITES(s.allowed_domains.length) : W.CHIP_SETUP_ONLY);
  if (s.allow_all_egress === false) chips.push(W.CHIP_NO_ALLOW_ALL);
  if (s.denied_domains?.length) chips.push(W.CHIP_DENIED(s.denied_domains.length));
  if (s.first_use_approval === "always_deny") chips.push(W.CHIP_UNLISTED_REFUSED);
  else if (s.first_use_approval === "deny_with_review") chips.push(W.CHIP_UNLISTED_REVIEW);
  else if (s.first_use_approval === "wait_for_review") chips.push(W.CHIP_UNLISTED_HELD);
  const blocked = blockedTransfers(s);
  if (blocked.length === Object.keys(NR_WALLS.APPLY_TRANSFERS).length) chips.push(W.CHIP_NO_TRANSFERS);
  else if (blocked.length > 0) chips.push(W.CHIP_NO_TRANSFER(blocked.join(", ")));
  if (readsOnly(s)) chips.push(W.CHIP_READ_ONLY);
  for (const f of forgeLimits(s)) chips.push(f.reads ? W.CHIP_FORGE_READ(FORGE_NAME[f.forge]) : W.CHIP_FORGE_LIMITED(FORGE_NAME[f.forge], f.n));
  if (s.tool_rules?.length) chips.push(W.CHIP_TOOL_RULES(s.tool_rules.length));
  if (s.push_rules?.deny_all === true) chips.push(W.CHIP_EVERY_PUSH);
  if (s.push_rules?.deny_paths?.length) chips.push(W.CHIP_PUSH_PATHS(s.push_rules.deny_paths.length));
  if (s.recording) chips.push(W.CHIP_RECORDING);
  if (s.auto_stop_after_sec) chips.push(W.CHIP_AUTO_STOP);
  if (s.min_confinement_class) chips.push(W.CHIP_BARRIER(CC_META[s.min_confinement_class]?.label ?? s.min_confinement_class));
  if (s.withheld_grants?.length) chips.push(W.CHIP_CREDENTIALS(s.withheld_grants.length));
  return chips.length > 0 ? chips : [W.CHIP_OPEN];
}

export const FORGE_NAME = { github: "GitHub", ado: "Azure DevOps" } as const;

/** Who a preset is for, in the words Review and the pill use; a group says its label, else its id. */
export function audienceOf(p: Pick<WallPreset, "scope" | "group_id">, groupLabels?: Record<string, string>): string {
  switch (p.scope) {
    case "builtin":
      return W.AUD_BUILT_IN;
    case "org":
      return W.AUD_ORG;
    case "group":
      return W.AUD_GROUP(groupLabels?.[p.group_id ?? ""] ?? p.group_id ?? "");
    default:
      return W.AUD_YOURS;
  }
}

/** The form sections a server sentence can name, by the wire path it starts with. */
export type FormSection =
  | "sites"
  | "never"
  | "unlisted"
  | "barrier"
  | "clipboard"
  | "credentials"
  | "github"
  | "ado"
  | "tools"
  | "pushes"
  | "recording"
  | "idle";

const SECTION_OF_PATH: Record<string, FormSection | "name" | "description"> = {
  allowed_domains: "sites",
  allow_all_egress: "sites",
  denied_domains: "never",
  first_use_approval: "unlisted",
  min_confinement_class: "barrier",
  desktop: "clipboard",
  withheld_grants: "credentials",
  github_capabilities: "github",
  azure_devops_capabilities: "ado",
  tool_rules: "tools",
  "push_rules.deny_all": "pushes",
  "push_rules.deny_paths": "pushes",
  recording: "recording",
  auto_stop_after_sec: "idle",
  name: "name",
  description: "description",
};

/**
 * The part a walls or wall preset refusal names: the section of the form its wire path belongs to, and the server's own
 * sentence under it. A sentence that names no path of the form maps to none, and shows above the buttons. The server
 * prefixes a write's sentence with "invalid wall preset: " (a preset) or "invalid walls: " (Your walls).
 */
export function sectionOfRefusal(message: string): { section: FormSection | "name" | "description" | null; text: string } {
  const text = message.replace(/^invalid (?:wall preset|walls): /, "");
  // A path, then a colon ("denied_domains[1]: ...") or a space ("name is required"); indexes are dropped.
  const path = /^[a-z_]+(?:\[\d+\])?(?:\.[a-z_]+)?(?:\[\d+\])?(?=[: ])/.exec(text)?.[0].replace(/\[\d+\]/g, "") ?? "";
  return { section: SECTION_OF_PATH[path] ?? SECTION_OF_PATH[path.split(".")[0]] ?? null, text };
}

/** A stored idle stop as the form shows it: whole minutes in minutes, anything else in seconds, so it is always exact. */
export const idleDisplay = (sec: number): { n: number; unit: "seconds" | "minutes" } =>
  sec % 60 === 0 ? { n: sec / 60, unit: "minutes" } : { n: sec, unit: "seconds" };

/** The audit rule_source of a push refused because the policy refuses every push. */
export const PUSH_DENY_ALL_SOURCE = "brokered:git:push-deny-all";

/**
 * The name to print under a deny-all refusal when the run's wall preset is what refused every push: the policy view's
 * wall_preset says the preset added deny_all (added.push_rules). A personal preset a non-owner reads has no name, so none.
 */
export function presetRefusingEveryPush(p: { name?: string; added?: Record<string, string[]> } | undefined): string | undefined {
  return p?.name && p.added?.push_rules?.includes("deny_all") ? p.name : undefined;
}
