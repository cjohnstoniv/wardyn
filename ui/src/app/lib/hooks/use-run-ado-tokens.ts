/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Azure DevOps tokens a run held, oldest first (GET /runs/{id}/ado-tokens).
// A run that holds none, a daemon without the route, or a caller who may not
// read it all read as []: the run page then draws nothing for Azure DevOps
// rather than a claim it cannot back. While `live` it re-reads on an interval,
// since renewal, a pause and an approval each change the list.
import * as React from "react";
import { adoPat } from "../api/ado-pat";
import type { ADORunToken } from "../types/ado-pat";

const POLL_MS = 10_000;

export function useRunAdoTokens(runId: string, live: boolean, enabled = true): ADORunToken[] {
  const [tokens, setTokens] = React.useState<ADORunToken[]>([]);
  React.useEffect(() => {
    if (!enabled) return;
    let alive = true;
    const read = () =>
      adoPat
        .runTokens(runId)
        .then((t) => alive && setTokens(Array.isArray(t) ? t : []))
        .catch(() => alive && setTokens((prev) => prev));
    void read();
    if (!live) return () => void (alive = false);
    const id = window.setInterval(() => void read(), POLL_MS);
    return () => {
      alive = false;
      window.clearInterval(id);
    };
  }, [runId, live, enabled]);
  return tokens;
}
