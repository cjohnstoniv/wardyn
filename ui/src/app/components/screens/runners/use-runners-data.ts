/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// What the Admin -> Runners page shows: the switch, every runner (revoked too, so the Active, Revoked and All
// filters are one read), the remote-hosted runners this server is set up with, and the unused tokens.
// Re-read silently every 30 s while the tab is visible and when it becomes visible again; a failed silent
// read keeps what the page already shows rather than blanking it.
import * as React from "react";
import { HttpError } from "../../../lib/api/core";
import { remoteRunners } from "../../../lib/api/runner-pools";
import { runners as runnersApi } from "../../../lib/api/runners";
import type { RemoteRunnerView } from "../../../lib/types/runner-pools";
import type { RunnerRegistrationToken, RunnerView } from "../../../lib/types/runners";
import { loadFailStatus, type ScreenStatus } from "../../wardyn/states";

export const POLL_MS = 30_000;

export interface RunnersData {
  status: ScreenStatus;
  enabled: boolean;
  rows: RunnerView[];
  remote: RemoteRunnerView[];
  tokens: RunnerRegistrationToken[];
}

// A server with no pool storage answers the remote-hosted runners 501: the page then shows people's
// runners alone, as it did before pools.
const remoteOrNone = () =>
  remoteRunners.list().catch((e) => {
    if (e instanceof HttpError && e.status === 501) return [] as RemoteRunnerView[];
    throw e;
  });

export function useRunnersData(): RunnersData & { reload: () => void } {
  const [data, setData] = React.useState<RunnersData>({ status: "loading", enabled: true, rows: [], remote: [], tokens: [] });
  const seq = React.useRef(0);

  // A read that was in flight when the page went away must not set state.
  const abandon = React.useCallback(() => {
    seq.current++;
  }, []);

  const read = React.useCallback((silent: boolean) => {
    const mine = ++seq.current;
    if (!silent) setData((d) => ({ ...d, status: "loading" }));
    Promise.all([runnersApi.settings(), runnersApi.list("all"), runnersApi.tokens(), remoteOrNone()])
      .then(([settings, rows, tokens, remote]) => {
        if (mine === seq.current) setData({ status: "ready", enabled: settings.enabled, rows, remote, tokens });
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
