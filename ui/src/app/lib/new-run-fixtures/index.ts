/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Every route the 0.9 New Run packet draws, as data. A UI lane tests against
// `newRunFixture("run/p4-grants")` rather than hand-building a response.
import { ACCESS_FIXTURES } from "./access";
import type { NewRunFixture } from "./base";
import { POLICY_FIXTURES } from "./policy";
import { RUN_FIXTURES } from "./run";
import { WORKSPACE_FIXTURES } from "./workspaces";

export type { FixtureProvenance, FixtureRefusal, FixtureWorkspace, NewRunFixture } from "./base";

export const NEW_RUN_FIXTURES: readonly NewRunFixture[] = [...RUN_FIXTURES, ...WORKSPACE_FIXTURES, ...ACCESS_FIXTURES, ...POLICY_FIXTURES];

/** One fixture by its route (without the /m-nr/ prefix); throws on a route that is not drawn. */
export function newRunFixture(route: string): NewRunFixture {
  const found = NEW_RUN_FIXTURES.find((f) => f.route === route);
  if (!found) throw new Error(`no New Run fixture for ${route}`);
  return found;
}
