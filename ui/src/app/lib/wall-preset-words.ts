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

const isRead = (c: string) => c.endsWith("_read") || c === "identity";

/** Both forge capability lists name reads only (and at least one list is named). */
export const readsOnly = (s: PersonalWallsSpec): boolean => {
  const lists = [s.github_capabilities, s.azure_devops_capabilities].filter((l): l is string[] => !!l);
  return lists.length > 0 && lists.every((l) => l.every(isRead));
};

/** The directions a spec blocks, in the order the form lists them. */
export const blockedTransfers = (s: PersonalWallsSpec): string[] =>
  Object.entries(NR_WALLS.APPLY_TRANSFERS).filter(([dir]) => s.desktop?.[dir as keyof NonNullable<PersonalWallsSpec["desktop"]>] === "blocked").map(([, word]) => word);

/** The short chips of a row: each knob the preset names, none but "Narrows nothing" for the empty spec. */
export function presetChips(s: PersonalWallsSpec): string[] {
  const chips: string[] = [];
  if (s.allowed_domains) chips.push(s.allowed_domains.length > 0 ? W.CHIP_SITES(s.allowed_domains.length) : W.CHIP_SETUP_ONLY);
  if (s.denied_domains?.length) chips.push(W.CHIP_DENIED(s.denied_domains.length));
  if (s.first_use_approval === "always_deny") chips.push(W.CHIP_UNLISTED_REFUSED);
  const blocked = blockedTransfers(s);
  if (blocked.length === Object.keys(NR_WALLS.APPLY_TRANSFERS).length) chips.push(W.CHIP_NO_TRANSFERS);
  else if (blocked.length > 0) chips.push(W.CHIP_NO_TRANSFER(blocked.join(", ")));
  if (readsOnly(s)) chips.push(W.CHIP_READ_ONLY);
  if (s.push_rules?.deny_all === true) chips.push(W.CHIP_EVERY_PUSH);
  if (s.min_confinement_class) chips.push(W.CHIP_BARRIER(CC_META[s.min_confinement_class]?.label ?? s.min_confinement_class));
  if (s.withheld_grants?.length) chips.push(W.CHIP_CREDENTIALS(s.withheld_grants.length));
  return chips.length > 0 ? chips : [W.CHIP_OPEN];
}

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
export type FormSection = "sites" | "never" | "unlisted" | "barrier" | "clipboard" | "credentials" | "pushes";

const SECTION_OF_PATH: Record<string, FormSection | "name" | "description"> = {
  allowed_domains: "sites",
  allow_all_egress: "sites",
  denied_domains: "never",
  first_use_approval: "unlisted",
  min_confinement_class: "barrier",
  desktop: "clipboard",
  withheld_grants: "credentials",
  "push_rules.deny_all": "pushes",
  name: "name",
  description: "description",
};

/**
 * The part a wall preset refusal names: the section of the form its wire path belongs to, and the server's own
 * sentence under it. A path the form has no section for (a capability list, tool rules, recording) maps to none,
 * and the sentence shows above the buttons. The server prefixes a write's sentence with "invalid wall preset: ".
 */
export function sectionOfRefusal(message: string): { section: FormSection | "name" | "description" | null; text: string } {
  const text = message.replace(/^invalid wall preset: /, "");
  // A path, then a colon ("denied_domains[1]: ...") or a space ("name is required"); indexes are dropped.
  const path = /^[a-z_]+(?:\[\d+\])?(?:\.[a-z_]+)?(?:\[\d+\])?(?=[: ])/.exec(text)?.[0].replace(/\[\d+\]/g, "") ?? "";
  return { section: SECTION_OF_PATH[path] ?? SECTION_OF_PATH[path.split(".")[0]] ?? null, text };
}
