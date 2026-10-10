/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// M-NR-1: the Workspaces panel's frames (/m-nr/workspaces/…). The refusal
// sentences are the server's, in the console's bytes (new-run-refusals.ts).
import { NEW_RUN_REASON, WORKSPACE_REFUSAL } from "../new-run-refusals";
import { adoComponent, githubComponent, preview, RESOURCES_ORG, type FixtureBody, type FixtureWorkspace } from "./base";

const payments: FixtureWorkspace = { id: "ws-payments", name: "payments", target: "/home/agent/work" };
const docs: FixtureWorkspace = { id: "ws-docs", name: "docs", target: "/home/agent/docs" };
const base = { preview: preview({ resources: [RESOURCES_ORG] }) };
const refusal = (reason: string, text: string) => ({ status: 422, reason, text });

export const WORKSPACE_FIXTURES: FixtureBody[] = [
  { route: "workspaces/one", note: "One entry: primary, no Make primary, Remove present.", workspaces: [payments], ...base },
  { route: "workspaces/scratch", note: "Ephemeral scratch primary: the picker and Attach another only.", workspaces: [{ id: "scratch", name: "Ephemeral scratch — no repo", scratch: true }], ...base },
  { route: "workspaces/two", note: "Two entries; the second has Make primary and Remove.", workspaces: [payments, docs], ...base },
  { route: "workspaces/make-primary", note: "After Make primary: the entry moved to index 0; the order is meaningful on the wire (workspaces[0]).", workspaces: [docs, payments], ...base },
  { route: "workspaces/remove", note: "A removed entry clears its issues; a Git section it alone caused goes (or stays inactive when edited).", workspaces: [payments], ...base },
  { route: "workspaces/attach", note: "Attach another appends the entry with a sibling target prefilled when the default collides (NR-Q6).", workspaces: [payments, { ...docs, target: "/home/agent/docs" }], ...base },
  { route: "workspaces/attach-none", note: "Every workspace the person can use is attached.", workspaces: [payments, docs], ...base },
  {
    route: "workspaces/equal",
    note: "Equal targets across two workspaces (OD-3).",
    workspaces: [payments, { ...docs, target: "/home/agent/work" }],
    refusal: refusal(NEW_RUN_REASON.WORKSPACE_TARGET_OVERLAP, WORKSPACE_REFUSAL.OVERLAP_EQUAL("/home/agent/work", "payments")),
    ...base,
  },
  {
    route: "workspaces/nested",
    note: "A target inside another workspace's target.",
    workspaces: [payments, { ...docs, target: "/home/agent/work/docs" }],
    refusal: refusal(NEW_RUN_REASON.WORKSPACE_TARGET_OVERLAP, WORKSPACE_REFUSAL.OVERLAP_NESTED("/home/agent/work/docs", "payments", "/home/agent/work")),
    ...base,
  },
  {
    route: "workspaces/drive",
    note: "A target on the drive's fixed path, drive on.",
    workspaces: [{ ...payments, target: "/home/agent/drive" }],
    drive: true,
    refusal: refusal(NEW_RUN_REASON.WORKSPACE_TARGET_OVERLAP, WORKSPACE_REFUSAL.OVERLAP_EQUAL("/home/agent/drive", "your drive")),
    ...base,
  },
  {
    route: "workspaces/drive-toggled-last",
    note: "The drive was switched on last, so the issue sits on the colliding entry.",
    workspaces: [{ ...payments, target: "/home/agent/drive/src" }],
    drive: true,
    refusal: refusal(NEW_RUN_REASON.WORKSPACE_TARGET_OVERLAP, WORKSPACE_REFUSAL.OVERLAP_NESTED("/home/agent/drive/src", "your drive", "/home/agent/drive")),
    ...base,
  },
  {
    route: "workspaces/fixed-secondary",
    note: "A fixed secondary source collides with the edited target; the issue is on the other entry.",
    workspaces: [{ ...payments, otherSourceTargets: ["/home/agent/data"] }, { ...docs, target: "/home/agent/data" }],
    refusal: refusal(NEW_RUN_REASON.WORKSPACE_TARGET_OVERLAP, WORKSPACE_REFUSAL.OVERLAP_EQUAL("/home/agent/data", "payments")),
    ...base,
  },
  {
    route: "workspaces/fixed-both",
    note: "Both colliding paths are fixed: the issue is under the later entry's picker; Remove is the remedy.",
    workspaces: [{ ...payments, otherSourceTargets: ["/home/agent/data"] }, { ...docs, target: undefined, otherSourceTargets: ["/home/agent/data"] }],
    refusal: refusal(NEW_RUN_REASON.WORKSPACE_TARGET_OVERLAP, WORKSPACE_REFUSAL.OVERLAP_EQUAL("/home/agent/data", "payments")),
    ...base,
  },
  { route: "workspaces/clone-collision", note: "A clone whose default target collides with an attached workspace.", workspaces: [payments, { ...docs, target: "/home/agent/work" }], refusal: refusal(NEW_RUN_REASON.WORKSPACE_TARGET_OVERLAP, WORKSPACE_REFUSAL.OVERLAP_EQUAL("/home/agent/work", "payments")), ...base },
  {
    route: "workspaces/pin",
    note: "Two workspaces pin different model providers.",
    workspaces: [{ ...payments, pin: "Anthropic API" }, { ...docs, pin: "Bedrock" }],
    refusal: refusal(NEW_RUN_REASON.WORKSPACE_PIN_CONFLICT, WORKSPACE_REFUSAL.PIN_CONFLICT("payments", "Anthropic API", "docs", "Bedrock")),
    ...base,
  },
  {
    route: "workspaces/image",
    note: "Two workspaces use different base images. The refusal is about the image, so it is shown in the Runner tab's Image section, not here (see runner/image-conflict).",
    workspaces: [{ ...payments, baseImage: "ghcr.io/acme/dev:1" }, { ...docs, baseImage: "ghcr.io/acme/dev:2" }],
    refusal: refusal(NEW_RUN_REASON.IMAGE_CONFLICT, WORKSPACE_REFUSAL.IMAGE_CONFLICT("payments", "docs")),
    ...base,
  },
  {
    route: "workspaces/ado-org",
    note: "Two Azure DevOps organisations in one run.",
    workspaces: [{ ...payments, adoOrg: "globex" }, { ...docs, adoOrg: "initech" }],
    preview: preview({ resources: [RESOURCES_ORG], components: [adoComponent(), githubComponent()] }),
    refusal: refusal(NEW_RUN_REASON.WORKSPACE_ADO_ORG_CONFLICT, WORKSPACE_REFUSAL.ADO_ORG_CONFLICT("payments", "globex", "docs", "initech")),
  },
  {
    route: "workspaces/optional",
    note: "A workspace's optional requirements as checkboxes (enabled_optional).",
    workspaces: [{ ...payments, optional: ["egress:api.stripe.com", "secret:stripe-key", "write:/home/agent/work"], enabledOptional: ["egress:api.stripe.com"] }],
    ...base,
  },
  { route: "workspaces/read-only-forced", note: "A workspace that grants no writes shows a sentence instead of the read-only checkbox.", workspaces: [{ ...payments, noWrites: true, readOnly: true }], ...base },
];
