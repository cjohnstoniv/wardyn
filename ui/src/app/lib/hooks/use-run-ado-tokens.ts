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
import { usePoll } from "../use-poll";
import type { ADORunToken } from "../types/ado-pat";

const POLL_MS = 10_000;

export function useRunAdoTokens(runId: string, live: boolean, enabled = true): ADORunToken[] {
  const [tokens, setTokens] = React.useState<ADORunToken[]>([]);
  // Bumped when the run, `live` or `enabled` changes and on unmount, so a read still in flight then sets nothing.
  const gen = React.useRef(0);
  const read = () => {
    const mine = gen.current;
    return adoPat
      .runTokens(runId)
      .then((t) => mine === gen.current && setTokens(Array.isArray(t) ? t : []))
      .catch(() => mine === gen.current && setTokens((prev) => prev));
  };
  const abandon = () => void gen.current++;
  React.useEffect(() => {
    if (!enabled) return;
    void read();
    return abandon;
  }, [runId, live, enabled]); // eslint-disable-line react-hooks/exhaustive-deps -- read is rebuilt each render and reads runId
  usePoll(read, POLL_MS, !live || !enabled);
  return tokens;
}
