/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// What the Admin -> Runners page shows: the switch, every runner (revoked too, so the Active, Revoked and All
// filters are one read), the remote-hosted runners this server is set up with, and the unused tokens.
// Re-read silently every 30 s while the tab is visible and the console is signed in, and when the tab becomes
// visible again; a failed silent read keeps what the page already shows rather than blanking it.
import * as React from "react";
import { HttpError } from "../../../lib/api/core";
import { remoteRunners } from "../../../lib/api/runner-pools";
import { runners as runnersApi } from "../../../lib/api/runners";
import type { RemoteRunnerView } from "../../../lib/types/runner-pools";
import type { RunnerRegistrationToken, RunnerView } from "../../../lib/types/runners";
import { loadFailStatus, type ScreenStatus } from "../../wardyn/states";
import { useLoad } from "./use-load";

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

  const read = useLoad(
    {
      fetch: () => Promise.all([runnersApi.settings(), runnersApi.list("all"), runnersApi.tokens(), remoteOrNone()]),
      start: () => setData((d) => ({ ...d, status: "loading" })),
      ok: ([settings, rows, tokens, remote]) => setData({ status: "ready", enabled: settings.enabled, rows, remote, tokens }),
      fail: (e, silent) => {
        if (!silent) setData((d) => ({ ...d, status: loadFailStatus(e) }));
      },
    },
    POLL_MS,
  );

  const reload = React.useCallback(() => void read(true), [read]);
  return { ...data, reload };
}
