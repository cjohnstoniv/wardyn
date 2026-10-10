/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Every route the 0.9 New Run packet draws, as data. A UI lane tests against
// `newRunFixture("run/p4-grants")` rather than hand-building a response.
import { INFO_FIXTURE } from "./info";
import { ACCESS_FIXTURES } from "./access";
import { inTab, type NewRunFixture } from "./base";
import { POLICY_FIXTURES } from "./policy";
import { RUN_FIXTURES } from "./run";
import { WORKSPACE_FIXTURES } from "./workspaces";
import { NEW_RUN_REASON } from "../new-run-refusals";

export type { FixtureBody, FixtureProvenance, FixtureTab, FixtureRefusal, FixtureWorkspace, NewRunFixture } from "./base";

export const NEW_RUN_FIXTURES: readonly NewRunFixture[] = [
  ...inTab("info", [INFO_FIXTURE]),
  ...inTab("workspaces", WORKSPACE_FIXTURES.filter((f) => f.refusal?.reason !== NEW_RUN_REASON.IMAGE_CONFLICT)),
  ...inTab("runner", [...RUN_FIXTURES, ...WORKSPACE_FIXTURES.filter((f) => f.refusal?.reason === NEW_RUN_REASON.IMAGE_CONFLICT)]),
  ...inTab("access", ACCESS_FIXTURES),
  ...inTab("policy", POLICY_FIXTURES),
];

/** One fixture by its route (without the /m-nr/ prefix); throws on a route that is not drawn. */
export function newRunFixture(route: string): NewRunFixture {
  const found = NEW_RUN_FIXTURES.find((f) => f.route === route);
  if (!found) throw new Error(`no New Run fixture for ${route}`);
  return found;
}
