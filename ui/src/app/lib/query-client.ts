/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The console's one TanStack Query client. Screens pass it to useQuery as the second argument, so no provider wraps
// the app or a test. usePoll still drives every background re-read (in-flight drop, hidden tab, signed-out hold), so
// this client never polls, never re-reads on focus or reconnect, and never retries: a refused or failed read is the
// screen's own error state, as before. gcTime 0: a read dies with the last screen that shows it, as component state did.
import { QueryClient } from "@tanstack/react-query";

export const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      retry: false,
      refetchOnWindowFocus: false,
      refetchOnReconnect: false,
      refetchInterval: false,
      staleTime: 0,
      gcTime: 0,
      // No provider mounts this client, so its online tracking never starts. "always" pins today's rule: a read is
      // sent, and fails or answers; it never waits for the browser to say it is online.
      networkMode: "always",
    },
  },
});
