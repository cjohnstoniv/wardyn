/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Runner tab's pool selector states (owner amendment, runner pools): one pool,
// several, an empty Self-Hosted catalogue, a pool with none of the person's own
// runners, and the refused default. Preview data only: no server answers these
// yet, and every sentence is the one the server will send.
import type { RunnerDraft } from "../run-contract-draft";
import { RUNNER_POOL_REFUSAL, RUNNER_POOL_REFUSAL_REASON } from "../runner-pool-refusals";
import type { ResolvedRunnerPool, RunnerPoolChoice } from "../types/runner-pools";
import { noContract, preview, RESOURCES_ORG, type FixtureBody } from "./base";

const BUILD_FARM: RunnerPoolChoice = { id: "a1111111-1111-4111-8111-111111111111", name: "Build farm", hosting_type: "remote_provided", availability: "available" };
const GPU_FARM: RunnerPoolChoice = { id: "a2222222-2222-4222-8222-222222222222", name: "GPU farm", hosting_type: "remote_provided", availability: "available" };
const TEAM_LAPTOPS: RunnerPoolChoice = { id: "b1111111-1111-4111-8111-111111111111", name: "Team laptops", hosting_type: "self_hosted", availability: "available" };
const NO_OWN_RUNNER: RunnerPoolChoice = {
  ...TEAM_LAPTOPS,
  availability: "unavailable",
  reason: RUNNER_POOL_REFUSAL_REASON.NO_OWN_RUNNER,
};

const resolved = (pool: RunnerPoolChoice, selection: ResolvedRunnerPool["selection"], revision = 3): ResolvedRunnerPool => ({
  id: pool.id,
  name: pool.name,
  hosting_type: pool.hosting_type,
  revision,
  selection,
});
const draft = (runner: RunnerDraft) => ({ ...noContract(), runner });
const answer = (pools: RunnerPoolChoice[], runner_pool?: ResolvedRunnerPool) =>
  preview({ resources: [RESOURCES_ORG], runner_pools: pools, ...(runner_pool ? { runner_pool } : {}) });

export const POOL_FIXTURES: FixtureBody[] = [
  {
    route: "runner/pool-single",
    note: "One Remote Provided pool, seeded by the organisation default; the selector is still shown.",
    preview: answer([BUILD_FARM], resolved(BUILD_FARM, "organisation_default")),
    contract: draft({ poolId: BUILD_FARM.id }),
  },
  {
    route: "runner/pool-personal-default",
    note: "Two Remote Provided pools; the person's own default beats the organisation's.",
    preview: answer([BUILD_FARM, GPU_FARM], resolved(GPU_FARM, "personal_default")),
    contract: draft({ poolId: GPU_FARM.id }),
  },
  {
    route: "runner/pool-self-empty",
    note: "Self-Hosted with no permitted pool: the empty area carries the setup link, and Self-Hosted stays selectable.",
    preview: answer([]),
  },
  {
    route: "runner/pool-self-no-own-runner",
    note: "A Self-Hosted pool exists but none of the person's own runners is in it; never another person's.",
    preview: answer([NO_OWN_RUNNER]),
    refusal: { status: 422, reason: "runner_pool_no_eligible_member", text: RUNNER_POOL_REFUSAL.NO_OWN_RUNNER(TEAM_LAPTOPS.name) },
  },
  {
    route: "runner/pool-default-unavailable",
    note: "The person's saved default pool is gone: replacement is explicit, never a silent fall-through.",
    preview: answer([BUILD_FARM, GPU_FARM]),
    refusal: { status: 422, reason: "runner_pool_default_unavailable", text: RUNNER_POOL_REFUSAL.DEFAULT_UNAVAILABLE_PERSONAL() },
  },
  {
    route: "runner/pool-required",
    note: "No explicit pool and no default of any kind: the person must choose.",
    preview: answer([BUILD_FARM, GPU_FARM]),
    refusal: { status: 422, reason: "runner_pool_required", text: RUNNER_POOL_REFUSAL.REQUIRED_ANY() },
  },
];
