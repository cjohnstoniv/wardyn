/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Review's one ordered blocker list, in the order the design fixes:
// local issues, unsupported document fields, the template's unmet setup, the
// server's refusal, missing setup items, then a stale or unchecked result.

import { NR_DOCUMENT } from "../../../wardyn/copy/new-run/document";
import { NR_FILES } from "../../../wardyn/copy/new-run/files";
import { NR_HARNESSES } from "../../../wardyn/copy/new-run/harnesses";
import { NR_REVIEW } from "../../../wardyn/copy/new-run/review";
import { NR_RUN } from "../../../wardyn/copy/new-run/run";
import { NR_SHAPE } from "../../../wardyn/copy/new-run/shape";
import { NR_TOOLS } from "../../../wardyn/copy/new-run/tools";
import { NR_TEMPLATES } from "../../../wardyn/copy/new-run/templates";
import type { SetupItem } from "../../../../lib/types/preflight";
import type { TemplateSetupNeed } from "../../../../lib/types/templates";
import {
  FIELD_HOME_OVERRIDES, FIELD_OF_REASON, groupOfPath, homeOfPath, homeOfReason, REVIEW_GROUP_OF_FIELD, type FieldHome, type ReviewGroupId,
} from "../draft/field-homes";
import type { NewRunLocation } from "../draft/new-run-url";
import type { RunIntent } from "../draft/run-intent";
import { APP_PROBLEM_CODES, type AppProblemCode, type StepIssue, type StepIssueCode } from "../lenses/step-issues";
import { appIssueText } from "../steps/run/app-issue-text";

export type BlockerKind = "local" | "gateway_off" | "unsupported" | "template_setup" | "template_unread" | "refusal" | "setup_item" | "stale";

export interface Blocker {
  id: string;
  kind: BlockerKind;
  text: string;
  /** Where the fix lives; absent when it lives on Review itself. */
  to?: NewRunLocation;
  /** The one field it names, when it names one: only that field's Review group shows it. */
  field?: string;
}

/** The screen a field's fix lives on. Review and excluded fields have none to link to. */
export function locationOfHome(home: FieldHome | undefined): NewRunLocation | undefined {
  if (!home || home === "review" || home === "excluded") return undefined;
  return home === "document" ? { screen: "document", format: "yaml" } : { screen: "step", step: home };
}

/** Every local issue code has a sentence of its own; a port row's problems name the name or port, so appIssueText builds them. */
const ISSUE_TEXT: Record<Exclude<StepIssueCode, AppProblemCode>, string> = {
  shape_required: NR_SHAPE.SHAPE_REQUIRED,
  harness_required: NR_HARNESSES.HARNESS_REQUIRED,
  task_required: NR_HARNESSES.TASK_ERROR,
  command_required: NR_RUN.COMMAND_REQUIRED,
  pool_required: NR_RUN.POOL_HELP,
  app_required: NR_RUN.PORT_REQUIRED,
  main_drive_size_required: NR_FILES.SIZE_ERROR,
  files_choice_required: NR_FILES.CHOOSE_FILES,
  starts_here_required: NR_FILES.STARTS_HERE_REQUIRED,
  doc_invalid: NR_DOCUMENT.DOC_INVALID,
};

const isAppProblem = (code: StepIssueCode): code is AppProblemCode => (APP_PROBLEM_CODES as readonly StepIssueCode[]).includes(code);

/** "inline_policy.model_provider" as "Model provider": the field named, not a sentence about it. */
const fieldLabel = (field: string): string => {
  const words = field.replace(/^inline_policy\./, "").replace(/_/g, " ");
  return words.charAt(0).toUpperCase() + words.slice(1);
};

/** True when the draft now specifies the field a template left for the person to set. */
export function specifies(intent: RunIntent, field: string): boolean {
  const record = intent as Record<string, unknown>;
  const policy = field.startsWith("inline_policy.") ? (intent.inline_policy as Record<string, unknown> | undefined) : undefined;
  return (policy ? policy[field.slice("inline_policy.".length)] : record[field]) !== undefined;
}

export interface BlockerInput {
  issues: readonly StepIssue[];
  unsupported: readonly string[];
  templateNeeds: readonly TemplateSetupNeed[];
  /** The template's needs are unread or could not be read: fail closed with the stale line. */
  templateUnread?: boolean;
  /** The server's refusal of this draft: its own sentence and reason. */
  refusal: { text: string; reason: string } | null;
  setupItems: readonly SetupItem[];
  /** A local MCP server on for a harness that cannot apply the rules it is under: the run would be refused. */
  localMcp?: readonly { harness: string; server: string }[];
  /** The harness's display name; its id until the roster answers. */
  harnessLabel?: (id: string) => string;
  /** An application server is drafted and this server's web app gateway is off, so its ports could never be opened. */
  gatewayOff?: boolean;
  /** The result for this draft is missing or older than a minute. */
  stale: boolean;
}

export function buildBlockers(input: BlockerInput): Blocker[] {
  const out: Blocker[] = [];
  for (const i of input.issues) {
    out.push({ id: `local:${i.code}`, kind: "local", text: isAppProblem(i.code) ? appIssueText({ ...i, code: i.code }) : ISSUE_TEXT[i.code], to: locationOfHome(i.home) });
  }
  for (const path of input.unsupported) {
    out.push({ id: `unsupported:${path}`, kind: "unsupported", text: NR_REVIEW.BLOCKER_UNSUPPORTED(path), to: locationOfHome(homeOfPath(path) ?? "document"), field: path });
  }
  for (const need of input.templateNeeds) {
    out.push({ id: `template:${need.field}`, kind: "template_setup", text: NR_TEMPLATES.NEEDS_SETUP(fieldLabel(need.field)), to: locationOfHome(homeOfPath(need.field)), field: need.field });
  }
  if (input.templateUnread) out.push({ id: "template:unread", kind: "template_unread", text: NR_REVIEW.BLOCKER_STALE });
  for (const c of input.localMcp ?? []) {
    const name = input.harnessLabel?.(c.harness) ?? c.harness;
    out.push({ id: `local_mcp:${c.harness}:${c.server}`, kind: "refusal", text: NR_TOOLS.MCP_REFUSED(name, c.server), to: { screen: "step", step: "tools" } });
  }
  // The server's own refusal of the same conflict says it in other words; the one above stands for it.
  if (input.refusal && !(input.localMcp?.length && input.refusal.reason === "request_field_unavailable")) {
    const field = (FIELD_OF_REASON as Record<string, string | undefined>)[input.refusal.reason];
    out.push({ id: "refusal", kind: "refusal", text: input.refusal.text, to: locationOfHome(homeOfReason(input.refusal.reason)), ...(field && { field }) });
  }
  for (const item of input.setupItems) {
    out.push({ id: `setup:${item.id}`, kind: "setup_item", text: item.detail || item.label, to: { screen: "step", step: "tools" } });
  }
  if (input.gatewayOff) {
    out.push({ id: "gateway_off", kind: "gateway_off", text: NR_REVIEW.BLOCKER_GATEWAY_OFF, to: { screen: "step", step: "run" }, field: "inline_policy.ui_apps" });
  }
  // A stale result only matters when nothing else already holds Launch.
  if (input.stale && out.length === 0) out.push({ id: "stale", kind: "stale", text: NR_REVIEW.BLOCKER_STALE });
  return out;
}

/** Every path with a home: the registry fields and the subpaths homed apart from their field. */
const HOMED_PATHS = [...Object.keys(REVIEW_GROUP_OF_FIELD), ...Object.keys(FIELD_HOME_OVERRIDES)];

/**
 * The Review groups a blocker's fix lives under: the group of the one field it names, else every group
 * showing a field the step it links to edits.
 */
export function groupsBlockedBy(blockers: readonly Blocker[]): Set<ReviewGroupId> {
  const out = new Set<ReviewGroupId>();
  for (const b of blockers) {
    const step = b.to?.screen === "step" ? b.to.step : undefined;
    const groups = b.field ? [groupOfPath(b.field)] : HOMED_PATHS.filter((p) => step && homeOfPath(p) === step).map(groupOfPath);
    for (const g of groups) if (g) out.add(g);
  }
  return out;
}

/** The Azure DevOps sign-in item: its Tools-step door lives in the policy preview, not in the preflight alone. */
const isAdoSignIn = (i: SetupItem): boolean => i.kind === "repo_credential" && i.fix?.action === "sign_in" && i.fix.target === "azure_devops";

/**
 * Missing setup items of a result: the checklist and the access block's own, once each.
 * The Azure DevOps sign-in item holds Launch only when the policy preview shows the same
 * item, so the Tools step it links to has its door; otherwise Launch stays open and the
 * launch refusal opens that door.
 */
export function missingSetup(
  result: { setup_items?: SetupItem[]; access?: { setup: SetupItem[] } } | null,
  preview?: { access?: { setup: SetupItem[] } } | null,
): SetupItem[] {
  const seen = new Set<string>();
  return [...(result?.setup_items ?? []), ...(result?.access?.setup ?? [])].filter((i) => {
    if (i.status !== "missing" || seen.has(i.id)) return false;
    if (isAdoSignIn(i) && !preview?.access?.setup.some((p) => p.id === i.id && p.status === "missing")) return false;
    seen.add(i.id);
    return true;
  });
}
