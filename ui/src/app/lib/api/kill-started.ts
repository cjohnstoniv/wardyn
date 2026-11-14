/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { HttpError } from "./core";

// A kill sent with notStarted (runs.killRun), refused because the run has started: nothing was ended. Its own module so
// a screen whose tests mock lib/api/runs still tells this refusal apart. Go: reasonRunKillStarted.
export const RUN_KILL_STARTED_REASON = "run_kill_started";

export const isKillStartedRefusal = (e: unknown): boolean => e instanceof HttpError && e.reason === RUN_KILL_STARTED_REASON;
