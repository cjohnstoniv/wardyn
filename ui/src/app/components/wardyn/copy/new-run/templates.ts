/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// New Run canon strings for the Templates screen, byte for byte from docs/design/new-run-canon.md.
// Lazy-only: imported by the New Run flow and never re-exported through copy.ts, whose barrel is in the eager
// entry chunk. new-run-copy.test.ts keeps this file and the doc equal in both directions.

export const NR_TEMPLATES = {
  EYEBROW: "Saved starting points",
  TITLE: "Templates",
  LEDE:
    "Start a new run from a saved setup. Using a template opens New run at review, where your organization's current policy and availability are checked again. A template never stores a run's title.",
  NEW_FROM_SCRATCH: "New run from scratch",
  SCOPE_FILTER_ARIA: "Who it's available to",
  KIND_FILTER_ARIA: "Kind",
  SEARCH_LABEL: "Search templates",
  SEARCH_PLACEHOLDER: "Search by name",
  LIST_ARIA: "Templates",
  SCOPE_ALL: "All",
  SCOPE_ME: "Only me",
  SCOPE_GROUPS: "Groups",
  SCOPE_ORG: "Organization",
  KIND_ALL: "All kinds",
  KIND_TEMPLATES: "Templates",
  KIND_POLICIES: "Policies",
  NOTE:
    "Save a template from a run's review page with Save as template or Save as policy. A policy holds only network, credential and access rules, and anything it leaves out comes from your organization's defaults.",
  COL_NAME: "Name",
  COL_AVAILABLE: "Available to",
  COL_INCLUDES: "Includes",
  COL_UPDATED: "Updated",
  COL_ACTIONS: "Actions",
  POLICY_BADGE: "Policy",
  NEEDS_SETUP: (label: string | number) => `Needs setup: ${label}`,
  GROUP_LABEL: (group: string | number) => `${group} group`,
  ORG_ADMINS: "Organization admins",
  /** pending W-24: the recommended text until the owner answers. */
  TEMPLATE_MANAGERS: "Template managers",
  PART_RUN: "Run",
  PART_WHERE: "Where it runs",
  PART_TOOLS: "Tools",
  PART_FILES: "Files",
  PART_ACCESS: "Access",
  PART_LIMITS: "Limits",
  REVISION: (n: string | number) => `Revision ${n}`,
  USE: "Use",
  RENAME: "Rename",
  DELETE: "Delete",
  /** Lead wording 2026-10-17 (FR-0120): console canon spells it Organization. */
  PUBLISH: "Publish to organization",
  /** Lead wording 2026-10-17 (FR-0120): the confirm that precedes Publish; its body is NR_SAVE.SHARED_DISCLOSURE. */
  PUBLISH_TITLE: (name: string | number) => `Publish ${name} to your organization?`,
  /** Lead wording 2026-10-17 (FR-0120): the success toast. */
  PUBLISHED: (name: string | number) => `${name} published to your organization.`,
  /** Lead wording 2026-10-17 (FR-0120): spelling only. */
  WHY_ORG: "Published by your organization. Only organization admins can change it.",
  WHY_GROUP: (scope: string | number) => `Published to ${scope}. Only its template managers can change it.`,
  GROUPS_UNVERIFIED: "Wardyn could not check which groups you belong to, so templates shared with a group may be missing. Sign in again to see them.",
  EMPTY_TITLE: "No templates yet",
  EMPTY_BODY: "When you review a run, choose Save as template to start from the same setup next time.",
  EMPTY_ACTION: "Start a new run",
  NO_MATCH_TITLE: "Nothing matches",
  NO_MATCH_BODY: "Try another filter or search term.",
  CLEAR_FILTERS: "Clear filters",
  RENAME_TITLE: "Rename template",
  RENAME_BODY:
    "The new name applies everywhere this template is offered. Runs already started from it keep their own titles.",
  NAME_LABEL: "Name",
  NAME_EMPTY: "Enter a name.",
  RENAME_CONFIRM: "Rename",
  RENAMED: (name: string | number) => `Renamed to ${name}.`,
  DELETE_TITLE: (name: string | number) => `Delete ${name}?`,
  DELETE_BODY_ME:
    "Only you can use it, so nobody else is affected. Runs already started from it keep running and keep their settings.",
  DELETE_BODY_SCOPE: (scope: string | number) =>
    `People in ${scope} can no longer start from it. Runs already started from it keep running and keep their settings.`,
  DELETE_CONFIRM: "Delete template",
  DELETED: (name: string | number) => `${name} deleted.`,
  CANCEL: "Cancel",
} as const;
