/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The new New Run's fixtures, keyed by the approved mock's scenarios plus the
// Tools and access prototype's. One file per wire area, each typed by the real
// wire twin; the step lane that reads an area owns its file.

export const NEW_RUN_SCENARIOS = ["member", "governed", "full", "offline", "noKvm", "yaml", "admin", "interactive2", "app"] as const;
export type NewRunScenario = (typeof NEW_RUN_SCENARIOS)[number];

/** One answer per scenario: a scenario without one fails typecheck. */
export type ByScenario<T> = Record<NewRunScenario, T>;

export const everyScenario = <T>(value: T, over: Partial<ByScenario<T>> = {}): ByScenario<T> =>
  ({ ...Object.fromEntries(NEW_RUN_SCENARIOS.map((s) => [s, value])), ...over }) as ByScenario<T>;
