/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { useShellSetupStatus } from "./model-access-context";
import { agentLabel } from "./primitives";

/** A harness id as the console names it: the server's roster name, else the console's own, else the id. */
export function useAgentName(): (id: string) => string {
  const { status } = useShellSetupStatus();
  return React.useCallback(
    (id) => status?.harnesses?.find((h) => h.id === id)?.display ?? agentLabel(id) ?? id,
    [status],
  );
}
