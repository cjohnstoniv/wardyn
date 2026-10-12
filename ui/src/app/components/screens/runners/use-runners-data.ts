/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// What the Admin -> Runners page shows: the switch, every runner (revoked too, so the Active, Revoked and All
// filters are one read) and the unused tokens. Re-read silently every 30 s while the tab is visible and when
// it becomes visible again; a failed silent read keeps what the page already shows rather than blanking it.
import * as React from "react";
import { runners as runnersApi } from "../../../lib/api/runners";
import type { RunnerRegistrationToken, RunnerView } from "../../../lib/types/runners";
import { loadFailStatus, type ScreenStatus } from "../../wardyn/states";

export const POLL_MS = 30_000;

export interface RunnersData {
  status: ScreenStatus;
  enabled: boolean;
  rows: RunnerView[];
  tokens: RunnerRegistrationToken[];
}

export function useRunnersData(): RunnersData & { reload: () => void } {
  const [data, setData] = React.useState<RunnersData>({ status: "loading", enabled: true, rows: [], tokens: [] });
  const seq = React.useRef(0);

  // A read that was in flight when the page went away must not set state.
  const abandon = React.useCallback(() => {
    seq.current++;
  }, []);

  const read = React.useCallback((silent: boolean) => {
    const mine = ++seq.current;
    if (!silent) setData((d) => ({ ...d, status: "loading" }));
    Promise.all([runnersApi.settings(), runnersApi.list("all"), runnersApi.tokens()])
      .then(([settings, rows, tokens]) => {
        if (mine === seq.current) setData({ status: "ready", enabled: settings.enabled, rows, tokens });
      })
      .catch((e) => {
        if (mine === seq.current && !silent) setData((d) => ({ ...d, status: loadFailStatus(e) }));
      });
  }, []);

  React.useEffect(() => {
    read(false);
    const tick = () => document.visibilityState === "visible" && read(true);
    const id = setInterval(tick, POLL_MS);
    document.addEventListener("visibilitychange", tick);
    return () => {
      clearInterval(id);
      document.removeEventListener("visibilitychange", tick);
      abandon();
    };
  }, [read, abandon]);

  const reload = React.useCallback(() => read(true), [read]);
  return { ...data, reload };
}
