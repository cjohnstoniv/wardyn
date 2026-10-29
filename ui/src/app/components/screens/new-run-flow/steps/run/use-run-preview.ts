/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { HttpError } from "../../../../../lib/api/core";
import { useModelAccessDoor } from "../../../../wardyn/model-access-context";
import { SERVICE_PAUSE_WARNING } from "../../../../wardyn/copy/run-service";
import { intentToWire } from "../../draft/intent-to-wire";
import type { RunIntent } from "../../draft/run-intent";
import { useDraftIdentity, useIntentChecks } from "../../draft/use-intent-checks";
import { appsOf } from "../../lenses/apps-lens";
import { useLocalIssues } from "../../new-run-route";

const NO_AUTO_PREFLIGHT = { local: false, backendArm: false, modelArm: false };

/** The server's sentence for the inbound feature refusal (inboundDeclaredRefusal): its reason, capability_feature, is shared with the other feature doors, and the wire carries no target. */
export const INBOUND_REFUSAL = "Inbound apps aren't turned on for you.";

export interface RunPreview {
  /** The server's policy preview says the service would be paused while it waits for requests. */
  pauses: boolean;
  /** The server refused a port the draft marks callable from outside: this person may not declare one. */
  inboundRefused: boolean;
}

/**
 * What the server's policy preview answers about the Run step's own fields. A draft with no service and no inbound port
 * sends nothing, so only a run that keeps itself running or opens a port to callers asks.
 */
export function useRunPreview(intent: RunIntent): RunPreview {
  const issues = useLocalIssues();
  const identity = useDraftIdentity();
  const door = useModelAccessDoor();
  const inbound = appsOf(intent).some((a) => a.inbound);
  const body = React.useMemo(() => ((intent.service || inbound) && !issues.length ? intentToWire(intent).body : null), [intent, inbound, issues.length]);
  const checks = useIntentChecks({
    body, selectionKey: "run-service", identity, externalRevision: "0", sourceRefreshPending: false,
    autoCheck: NO_AUTO_PREFLIGHT, doorOpen: door.open, adoDoorOpen: false,
  });
  const error = checks.preview.error;
  return {
    pauses: !!intent.service && !!checks.preview.result?.warnings?.includes(SERVICE_PAUSE_WARNING),
    inboundRefused: inbound && error instanceof HttpError && error.reason === "capability_feature" && error.message.startsWith(INBOUND_REFUSAL),
  };
}
