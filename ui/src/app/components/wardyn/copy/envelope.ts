/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The platform envelope card on Governance > Profiles, and the two chips in the Profiles table. The
// strings are the approved board's, character for character. LAZY: reached only through the governance
// screen, never re-exported from the eager copy.ts barrel.
export const ENVELOPE = {
  TITLE: "Platform envelopes",
  TITLE_UNIT_ADMIN: "Your platform's envelope",
  LEAD: "A platform is a profile and the profiles below it. Security names the groups approved for it. Inside an envelope, a unit admin's change applies directly when it stays within the platform's ceiling and limits.",
  LEAD_UNIT_ADMIN:
    "Security set the envelope for your platform. Inside it, your changes below the platform apply directly when they stay within its ceiling and limits.",
  HELP: "A profile is a governance unit.",
  COL_PLATFORM: "Platform",
  COL_GROUPS: "Approved groups",
  COL_SET_BY: "Set by",
  NO_GROUPS: "No groups approved",
  CHIP_ROOT: "Envelope root",
  CHIP_IN: (root: string) => `In envelope · ${root}`,
  SET: "Set an envelope",
  EDIT: "Edit groups",
  REMOVE: "Remove",
  CANCEL: "Cancel",
  SAVE_GROUPS: "Save approved groups",
  SAVE_ENVELOPE: "Save envelope",
  EDIT_TITLE: (platform: string) => `Approved groups for "${platform}"`,
  EDIT_LEAD:
    "Groups of people your identity provider sends. A unit admin can add one of these groups to a profile in this platform without a second person. A group sits on one envelope only.",
  ADD_LABEL: "Add a group",
  ADD_PLACEHOLDER: "Group name",
  ROOT_LABEL: "Platform root",
  ROOT_HINT: "The profile the platform starts at. Every profile below it is inside the envelope.",
  GROUPS_LABEL: "Approved groups (one per line)",
  GROUPS_HINT: "Leave empty to approve none yet. A group sits on one envelope only.",
  REFUSED_TITLE: "Couldn't save the envelope",
  REMOVE_TITLE: (platform: string) => `Remove the envelope from "${platform}"?`,
  REMOVE_BODY: (platform: string) =>
    `Changes below ${platform} will need a second person again, and a unit admin above it can decide changes to ${platform} itself. Groups already assigned stay assigned.`,
  REMOVE_CONFIRM: "Remove envelope",
  APPLIES_TITLE: "Applies directly",
  APPLIES: (platform: string) => [
    `Changing a profile below ${platform}, while it stays within ${platform}'s ceiling and limits.`,
    `Adding a group from the approved list to a profile in ${platform}.`,
    `Moving an approved group's assignment between two profiles in ${platform}.`,
  ],
  SECOND_TITLE: "Needs a second person",
  SECOND: (platform: string) => [
    "Removing a lock, changing a contact, or changing which assignment picks a person's profile.",
    `Moving a profile out of ${platform}, or deleting an assignment.`,
    "Adding a person: as before.",
    "Adding a group that isn't on the approved list: refused if the person isn't already in it, held for a second person if they are.",
  ],
  SECURITY_TITLE: "Only security can decide",
  SECURITY_ONLY: (platform: string) =>
    `Any change to ${platform} itself, and who administers it. If you propose one, it is held for security; you still see it under Changes, can add a note, and can withdraw it.`,
  ROOT_ADMIN_TITLE: "You administer the platform's root.",
  ROOT_ADMIN_BODY: (platform: string) =>
    `Changes to ${platform} itself, and to who administers it, are decided by security only. Changes below it follow the lists below.`,
  // Copy only: the console cannot write to a platform's root as a unit admin, so no screen mounts this note.
  HELD_ROOT_TITLE: (platform: string) => `Your change to ${platform} is held for security`,
  HELD_ROOT_BODY: "You can follow it under Changes, add a note, or withdraw it. Nobody else in your platform can see it.",
  HELD_ROOT_LINK: "View in Changes",
  SET_BY_SECURITY: "Security",
  EMPTY_TITLE: "No platform envelopes.",
  EMPTY_BODY:
    "An envelope lets a platform's unit admins change their own profiles and add approved groups without a second person. Set one on a profile that has others below it.",
} as const;
