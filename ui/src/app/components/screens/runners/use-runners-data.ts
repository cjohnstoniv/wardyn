/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// What the Admin -> Runners page shows: the switch, every runner (revoked too, so the Active, Revoked and All
// filters are one read), the remote-hosted runners this server is set up with, and the unused tokens.
// Re-read silently every 30 s while the tab is visible and the console is signed in, and when the tab becomes
// visible again; a failed silent read keeps what the page already shows rather than blanking it.
import * as React from "react";
import { useQuery } from "@tanstack/react-query";
import { HttpError } from "../../../lib/api/core";
import { remoteRunners } from "../../../lib/api/runner-pools";
import { runners as runnersApi } from "../../../lib/api/runners";
import type { RemoteRunnerView } from "../../../lib/types/runner-pools";
import type { RunnerRegistrationToken, RunnerView } from "../../../lib/types/runners";
import { queryClient } from "../../../lib/query-client";
import { usePoll } from "../../../lib/use-poll";
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

const KEY = ["runners", "page"];
const NO_ROWS: RunnerView[] = [];
const NO_REMOTE: RemoteRunnerView[] = [];
const NO_TOKENS: RunnerRegistrationToken[] = [];

export function useRunnersData(): RunnersData & { reload: () => void } {
  const q = useQuery(
    {
      queryKey: KEY,
      queryFn: () => Promise.all([runnersApi.settings(), runnersApi.list("all"), runnersApi.tokens(), remoteOrNone()]),
    },
    queryClient,
  );
  const { refetch } = q;
  // With rows on screen a poll tick joins a read still in flight; a reload after a change starts a new read that
  // replaces it. With nothing shown yet a tick replaces the read in flight, so a first read that hangs is not
  // waited on for good (TanStack joins a read with no data instead of cancelling it).
  const tick = React.useCallback(
    () =>
      queryClient.getQueryData(KEY) === undefined
        ? queryClient.cancelQueries({ queryKey: KEY, exact: true }).then(() => refetch())
        : refetch({ cancelRefetch: false }),
    [refetch],
  );
  usePoll(tick, POLL_MS, false);
  const reload = React.useCallback(() => void refetch(), [refetch]);

  // The read dies with the last screen that shows it: a remount shows the skeleton, not the last answer.
  React.useEffect(
    () => () => {
      if (queryClient.getQueryCache().find({ queryKey: KEY, exact: true })?.getObserversCount() === 0) {
        queryClient.removeQueries({ queryKey: KEY, exact: true });
      }
    },
    [],
  );

  // The last failure stays on screen while a re-read is in flight; only a read that answers replaces it.
  const [lastFail, setLastFail] = React.useState<ScreenStatus | null>(null);
  React.useEffect(() => {
    if (q.data) setLastFail(null);
    else if (q.isError) setLastFail(loadFailStatus(q.error));
  }, [q.data, q.isError, q.error]);

  if (q.data) {
    const [settings, rows, tokens, remote] = q.data;
    return { status: "ready", enabled: settings.enabled, rows, remote, tokens, reload };
  }
  return {
    status: q.isError ? loadFailStatus(q.error) : (lastFail ?? "loading"),
    enabled: true,
    rows: NO_ROWS,
    remote: NO_REMOTE,
    tokens: NO_TOKENS,
    reload,
  };
}
