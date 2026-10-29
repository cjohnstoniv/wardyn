/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { useModelAccessDoor } from "../../../../wardyn/model-access-context";
import { SERVICE_PAUSE_WARNING } from "../../../../wardyn/copy/run-service";
import { intentToWire } from "../../draft/intent-to-wire";
import type { RunIntent } from "../../draft/run-intent";
import { useDraftIdentity, useIntentChecks } from "../../draft/use-intent-checks";
import { useLocalIssues } from "../../new-run-route";

const NO_AUTO_PREFLIGHT = { local: false, backendArm: false, modelArm: false };

/**
 * Whether the server's policy preview for this draft says the service would be paused while it waits for requests.
 * A draft with no service sends nothing, so only a run that keeps itself running asks.
 */
export function useServicePauses(intent: RunIntent): boolean {
  const issues = useLocalIssues();
  const identity = useDraftIdentity();
  const door = useModelAccessDoor();
  const body = React.useMemo(() => (intent.service && !issues.length ? intentToWire(intent).body : null), [intent, issues.length]);
  const checks = useIntentChecks({
    body, selectionKey: "run-service", identity, externalRevision: "0", sourceRefreshPending: false,
    autoCheck: NO_AUTO_PREFLIGHT, doorOpen: door.open, adoDoorOpen: false,
  });
  return !!intent.service && !!checks.preview.result?.warnings?.includes(SERVICE_PAUSE_WARNING);
}
