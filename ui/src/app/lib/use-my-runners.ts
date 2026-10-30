/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { runners as runnersApi } from "./api/runners";
import type { RunnerView } from "./types/runners";

export interface MyRunners {
  /** The last list that was read; null until the first read answers. A failed read leaves it as it was. */
  runners: RunnerView[] | null;
  /** The latest read failed (an error, or the 403 while runners are off): says nothing about whether the person has runners. */
  failed: boolean;
}

/** The caller's own runners (GET /me/runners), read each time `enabled` turns on. */
export function useMyRunners(enabled = true): MyRunners {
  const [state, setState] = React.useState<MyRunners>({ runners: null, failed: false });
  React.useEffect(() => {
    if (!enabled) return;
    let alive = true;
    runnersApi.listMine().then(
      (runners) => alive && setState({ runners, failed: false }),
      () => alive && setState((prev) => ({ ...prev, failed: true })),
    );
    return () => {
      alive = false;
    };
  }, [enabled]);
  return state;
}

/** A runner a folder can be put on: the caller's own, claimed and not revoked. */
export const isFolderRunner = (r: RunnerView): boolean => r.state === "claimed" && !r.revoked_at && r.owner_kind !== "org";
