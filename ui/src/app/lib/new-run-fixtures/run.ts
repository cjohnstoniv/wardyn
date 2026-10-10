/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// M-NR-1: the Run panel's frames (/m-nr/run/…).
import { NEW_RUN_REASON, WORKSPACE_REFUSAL } from "../new-run-refusals";
import type { RunnerDraft } from "../run-contract-draft";
import { ALLOWED_IMAGES, agentComponent, noContract, preflight, preview, RESOURCES_ORG, resourcesFor, RUNNER_OFFLINE, RUNNER_ONLINE, RUNNER_SECOND, RUNNER_UNCLAIMED, selfDefined, type FixtureBody } from "./base";

const both = [RESOURCES_ORG, resourcesFor(RUNNER_ONLINE)];
const draft = (runner: RunnerDraft) => ({ ...noContract(), runner });

export const RUN_FIXTURES: FixtureBody[] = [
  { route: "run/p1-no-runner", note: "P1: no runner registered; My runner disabled with a link to My runner.", runners: [], preview: preview({ resources: [RESOURCES_ORG] }) },
  { route: "run/p1b-unclaimed", note: "P1b: a registered runner whose owner has not completed the claim.", runners: [RUNNER_UNCLAIMED], preview: preview({ resources: [RESOURCES_ORG] }) },
  { route: "run/p2-offline", note: "P2: the runner is offline; disabled with 'last seen'.", runners: [RUNNER_OFFLINE], preview: preview({ resources: [RESOURCES_ORG] }) },
  { route: "run/p3-ceiling", note: "P3: the person's profile does not allow runs on a runner.", runners: [RUNNER_ONLINE], ceilingDeniesLocal: { profile: "Contractors" }, preview: preview({ resources: [RESOURCES_ORG] }) },
  {
    route: "run/p4-grants",
    note: "P4: My runner selectable; the grant set refuses it, one issue per refused entry.",
    runners: [RUNNER_ONLINE],
    contract: draft({ placement: "local", runnerId: RUNNER_ONLINE.id }),
    preview: preview({
      resources: both,
      local_placement: [{ kind: "component", key: "git_provider:github:app", local_placeable: false, reason: "placement_credential" }],
    }),
    refusal: { status: 422, reason: "placement_credential", text: "Local placement needs the GitHub App token, which isn't available on your runner." },
  },
  {
    route: "run/p4-several",
    note: "P4: several refused entries, including a component the person defined (placement_component_self_defined).",
    runners: [RUNNER_ONLINE],
    contract: draft({ placement: "local", runnerId: RUNNER_ONLINE.id }),
    preview: preview({
      resources: both,
      components: [selfDefined("Internal API")],
      local_placement: [
        { kind: "component", key: "git_provider:github:app", local_placeable: false, reason: "placement_credential" },
        { kind: "component", key: "inline:0", local_placeable: false, reason: "placement_component_self_defined" },
      ],
    }),
  },
  { route: "run/p5-choose", note: "P5: two eligible placements and none chosen; no default (OD-8).", runners: [RUNNER_ONLINE], preview: preview({ resources: both }) },
  {
    route: "run/p6-single",
    note: "P6: only one placement is eligible; the server fills it.",
    runners: [],
    preview: preview({ resources: [RESOURCES_ORG] }),
    preflight: preflight({ resources: [RESOURCES_ORG] }),
  },
  {
    route: "run/p7-runners",
    note: "P7: more than one runner online; a runner picker, and runner_ambiguous when none is named.",
    runners: [RUNNER_ONLINE, RUNNER_SECOND],
    contract: draft({ placement: "local" }),
    preview: preview({ resources: [RESOURCES_ORG, resourcesFor(RUNNER_ONLINE), resourcesFor(RUNNER_SECOND, { cpu_millis: 8000, memory_mib: 32768 })] }),
    refusal: { status: 422, reason: "runner_ambiguous", text: "More than one of your runners is online. Choose which one this run uses." },
  },
  {
    route: "run/barrier-changed",
    note: "A placement change moved a touched Barrier pick to the strongest class the new placement can build.",
    runners: [RUNNER_ONLINE],
    contract: draft({ placement: "local", runnerId: RUNNER_ONLINE.id }),
    preview: preview({ resources: both }),
    preflight: preflight({ enforced_confinement_class: "CC2", resources: both }),
  },
  {
    route: "run/over-cap",
    note: "A touched CPU over the cap warns; the server clamps (A-S3).",
    runners: [RUNNER_ONLINE],
    contract: draft({ placement: "local", runnerId: RUNNER_ONLINE.id, cpus: 16 }),
    preview: preview({ resources: both }),
  },
  {
    route: "run/spec-resources",
    note: "The source policy sets resources, so the shown defaults are what an untouched run receives, not the platform default.",
    runners: [],
    preview: preview({
      spec: { allowed_domains: ["api.anthropic.com"], first_use_approval: "deny_with_review", min_confinement_class: "CC2", resources: { cpu_millis: 3000, memory_mib: 6144 } },
      resources: [{ ...RESOURCES_ORG, defaults: { cpu_millis: 3000, memory_mib: 6144 } }],
    }),
  },
  {
    route: "run/provider-line",
    note: "The read-only 'Model provider · {name} · Change' line, from the agent fact.",
    preview: preview({ components: [agentComponent()], resources: [RESOURCES_ORG] }),
  },
  {
    route: "run/runners-off",
    note: "Runners turned off: no Placement field, no rail section, no placeability chips.",
    runnersEnabled: false,
    preview: preview({ resources: [RESOURCES_ORG] }),
  },
  {
    route: "run/local-unavailable",
    note: "A server that cannot place on a runner yet refuses placement local with placement_unavailable.",
    runners: [RUNNER_ONLINE],
    contract: draft({ placement: "local", runnerId: RUNNER_ONLINE.id }),
    preview: preview({ resources: [RESOURCES_ORG] }),
    refusal: { status: 422, reason: "placement_unavailable", text: "Your own runner is not available: this server cannot place a run on a runner yet." },
  },
  {
    route: "run/field-unavailable",
    note: "A field the server cannot honour yet is refused with request_field_unavailable, never dropped.",
    preview: preview({ resources: [RESOURCES_ORG] }),
    refusal: { status: 422, reason: NEW_RUN_REASON.REQUEST_FIELD_UNAVAILABLE, text: "overrides: this server does not apply per-run overrides yet, so the run was not created." },
  },
  // The Runner tab's Image section: the resolved image, its source, and what Change offers.
  { route: "runner/image-agent", note: "The agent's own image; Change lists the organisation's allowed images.", preview: preview({ resources: [RESOURCES_ORG], allowed_images: ALLOWED_IMAGES }) },
  {
    route: "runner/image-workspace",
    note: "The primary workspace's base image: 'from payments'.",
    preview: preview({ resources: [RESOURCES_ORG], image: { ref: "ghcr.io/acme/dev:1", source: { kind: "workspace", name: "payments" } }, allowed_images: ALLOWED_IMAGES }),
  },
  {
    route: "runner/image-build",
    note: "The person's own build (the existing BYOI path).",
    preview: preview({ resources: [RESOURCES_ORG], image: { ref: "ghcr.io/ada/scratch:latest", source: { kind: "build", name: "your build" } }, allowed_images: ALLOWED_IMAGES }),
  },
  {
    route: "runner/image-org-allowed",
    note: "An image the person chose from the organisation's allowed list; sent as allowed_image.",
    contract: draft({ imageRef: ALLOWED_IMAGES[0].ref }),
    preview: preview({ resources: [RESOURCES_ORG], image: { ref: ALLOWED_IMAGES[0].ref, source: { kind: "org_allowed", name: ALLOWED_IMAGES[0].name } }, allowed_images: ALLOWED_IMAGES }),
  },
  { route: "runner/image-none-allowed", note: "The organisation allows no other image: Change is absent, Build an image remains.", preview: preview({ resources: [RESOURCES_ORG] }) },
  {
    route: "runner/image-conflict",
    note: "Two workspaces use different base images: the refusal is attributed to the image and shown here; remove one workspace.",
    preview: preview({ resources: [RESOURCES_ORG], allowed_images: ALLOWED_IMAGES }),
    refusal: { status: 422, reason: NEW_RUN_REASON.IMAGE_CONFLICT, text: WORKSPACE_REFUSAL.IMAGE_CONFLICT("payments", "docs") },
  },
  {
    route: "runner/image-unavailable",
    note: "A server that cannot apply an image choice yet refuses allowed_image (request_field_unavailable).",
    contract: draft({ imageRef: ALLOWED_IMAGES[0].ref }),
    preview: preview({ resources: [RESOURCES_ORG], allowed_images: ALLOWED_IMAGES }),
    refusal: { status: 422, reason: NEW_RUN_REASON.REQUEST_FIELD_UNAVAILABLE, text: "allowed_image: this server does not apply an image choice yet, so the run was not created." },
  },
];
