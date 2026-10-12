/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";

export type Loaded<T> = { status: "loading" } | { status: "error"; message: string } | { status: "ready"; value: T };

/** One read on mount, aborted on unmount. `load` must be stable. */
export function useLoaded<T>(load: (signal: AbortSignal) => Promise<T>): Loaded<T> {
  const [state, setState] = React.useState<Loaded<T>>({ status: "loading" });
  React.useEffect(() => {
    const controller = new AbortController();
    load(controller.signal).then(
      (value) => {
        if (!controller.signal.aborted) setState({ status: "ready", value });
      },
      (error: unknown) => {
        if (!controller.signal.aborted) setState({ status: "error", message: error instanceof Error ? error.message : String(error) });
      },
    );
    return () => controller.abort();
  }, [load]);
  return state;
}
