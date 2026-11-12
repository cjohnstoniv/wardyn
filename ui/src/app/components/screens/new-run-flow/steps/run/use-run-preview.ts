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
import { checkFailureText, useDraftIdentity, useIntentChecks } from "../../draft/use-intent-checks";
import { appsOf } from "../../lenses/apps-lens";
import { useLocalIssues } from "../../new-run-route";

const NO_AUTO_PREFLIGHT = { local: false, backendArm: false, modelArm: false };

/** The preview refused an inbound port: capability_feature is shared by the feature doors, so the refusal's `feature` says which. */
export function isInboundRefusal(inbound: boolean, error: unknown): boolean {
  return inbound && error instanceof HttpError && error.reason === "capability_feature" && error.feature === "inbound";
}

/** The preview's refusal as the window reads it: the field it names and its sentence. */
export function previewRefusal(error: unknown): { field: string; text: string } | null {
  return error instanceof HttpError ? { field: error.field, text: checkFailureText(error) } : null;
}

export interface RunPreview {
  /** The server's policy preview says the service would be paused while it waits for requests. */
  pauses: boolean;
  /** The server refused a port the draft marks callable from outside: this person may not declare one. */
  inboundRefused: boolean;
  /** The draft asks for a window and the server is being asked about it. */
  windowChecking: boolean;
  /** The preview's refusal, for the window's field-keyed clear. */
  refusal: { field: string; text: string } | null;
  /** The server's sentence refusing a field of the service (a 400 naming a service.* field), as it said it. */
  serviceRefusal?: string;
}

/** The server's own words for a refused service field: a run_mode_invalid whose field is under service. */
export function serviceRefusalOf(service: boolean, error: unknown): string | undefined {
  return service && error instanceof HttpError && error.reason === "run_mode_invalid" && error.field.startsWith("service.") ? error.message : undefined;
}

/**
 * What the server's policy preview answers about the Run step's own fields. A draft with no service, no inbound port
 * and no window on the Mac sends nothing, so only a run that keeps itself running, opens a port to callers or asks
 * for a window asks. With a window, every change on the step (a pool, a level, a harness) is checked with it.
 */
export function useRunPreview(intent: RunIntent): RunPreview {
  const issues = useLocalIssues();
  const identity = useDraftIdentity();
  const door = useModelAccessDoor();
  const inbound = appsOf(intent).some((a) => a.inbound);
  const asks = !!(intent.service || inbound || intent.desktop_window);
  const body = React.useMemo(() => (asks && !issues.length ? intentToWire(intent).body : null), [intent, asks, issues.length]);
  const checks = useIntentChecks({
    body, selectionKey: "run-service", identity, externalRevision: "0", sourceRefreshPending: false,
    autoCheck: NO_AUTO_PREFLIGHT, doorOpen: door.open, adoDoorOpen: false,
  });
  const error = checks.preview.error;
  return {
    pauses: !!intent.service && !!checks.preview.result?.warnings?.includes(SERVICE_PAUSE_WARNING),
    inboundRefused: isInboundRefusal(inbound, error),
    // Asked, or waiting out the debounce: neither an answer nor a refusal for this draft yet.
    windowChecking: !!intent.desktop_window && body !== null && (checks.preview.busy || (!checks.preview.current && error === null)),
    refusal: previewRefusal(error),
    serviceRefusal: serviceRefusalOf(!!intent.service, error),
  };
}
