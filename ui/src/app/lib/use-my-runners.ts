/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { runners as runnersApi } from "./api/runners";
import type { RunnerView } from "./types/runners";

/**
 * The caller's own runners (GET /me/runners), read once; null until the read answers. A failed read, or the
 * 403 while personal runners are off, is an empty list: a name the list lacks is shown by its short id.
 */
export function useMyRunners(enabled = true): RunnerView[] | null {
  const [list, setList] = React.useState<RunnerView[] | null>(null);
  React.useEffect(() => {
    if (!enabled) return;
    let alive = true;
    runnersApi.listMine().then(
      (rs) => alive && setList(rs),
      () => alive && setList([]),
    );
    return () => {
      alive = false;
    };
  }, [enabled]);
  return list;
}

/** A runner a folder can be put on: the caller's own, claimed and not revoked. */
export const isFolderRunner = (r: RunnerView): boolean => r.state === "claimed" && !r.revoked_at && r.owner_kind !== "org";
