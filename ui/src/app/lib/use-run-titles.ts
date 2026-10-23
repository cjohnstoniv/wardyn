/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { runs as runsApi } from "./api/runs";

// #3036: run titles for a list's run references, read once from the runs list. A failed or partial read only
// leaves the id alone after "Run"; the rows never wait on it.
export function useRunTitles(): ReadonlyMap<string, string> {
  const [titles, setTitles] = React.useState<ReadonlyMap<string, string>>(new Map());
  React.useEffect(() => {
    let live = true;
    void Promise.resolve()
      .then(() => runsApi.listRuns())
      .then((rs) => {
        if (live) setTitles(new Map(rs.map((r) => [r.id, r.title?.trim() || r.task])));
      })
      .catch(() => {});
    return () => {
      live = false;
    };
  }, []);
  return titles;
}
