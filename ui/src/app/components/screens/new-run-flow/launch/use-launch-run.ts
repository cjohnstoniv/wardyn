/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Create from the Name dialog: one request, held to the person who pressed
// Launch. A refusal comes back as its sentence, with the policy it names when it
// names one, for the dialog to show, and goes to `onRefused` so the credential
// doors can open; the dialog and its typed text stay as they were.

import * as React from "react";
import { getAuthGeneration, HttpError, isSignedOutHold } from "../../../../lib/api/core";
import { runs } from "../../../../lib/api/runs";
import { getErrorMessage } from "../../../../lib/format";
import type { CreateRunResult } from "../../../../lib/types/run-create";
import { useDeferredBusy } from "../../../../lib/use-deferred-busy";
import type { DraftIdentity } from "../draft/use-intent-checks";
import type { CreateRunWire } from "../draft/intent-to-wire";

/**
 * Whether a create is out. It is held by the owner of the dialog, not the form: Cancel unmounts the form with
 * the request still pending, and a form that began again would let a second Launch make a second run.
 */
export interface PendingLaunch {
  inFlight: React.MutableRefObject<boolean>;
  creating: boolean;
  setCreating: (creating: boolean) => void;
}

export function usePendingLaunch(): PendingLaunch {
  const inFlight = React.useRef(false);
  const [creating, setCreating] = React.useState(false);
  return { inFlight, creating, setCreating };
}

export function useLaunchRun(identity: DraftIdentity, onCreated: (created: CreateRunResult) => void, onRefused: (e: unknown) => void, held?: PendingLaunch) {
  const own = usePendingLaunch();
  const { inFlight, creating, setCreating } = held ?? own;
  const [refusal, setRefusal] = React.useState<string | null>(null);
  const [policy, setPolicy] = React.useState<HttpError["policy"]>();
  const { showSpinner: spinning } = useDeferredBusy(creating);
  const latest = React.useRef({ identity, onCreated, onRefused });
  latest.current = { identity, onCreated, onRefused };
  const mounted = React.useRef(true);
  React.useEffect(() => {
    mounted.current = true;
    return () => {
      mounted.current = false;
    };
  }, []);

  const launch = async (body: CreateRunWire) => {
    const { principal, resolved, authGeneration } = latest.current.identity;
    if (inFlight.current || !resolved || !principal || authGeneration !== getAuthGeneration() || isSignedOutHold()) return;
    const auth = getAuthGeneration();
    const still = () => latest.current.identity.principal === principal && auth === getAuthGeneration();
    inFlight.current = true;
    setRefusal(null);
    setPolicy(undefined);
    setCreating(true);
    try {
      const created = await runs.createRunWire(body);
      // A run that was created is shown even when the dialog is gone; one made for another person never is.
      if (still()) latest.current.onCreated(created);
    } catch (e) {
      if (!still() || !mounted.current) return;
      setRefusal(getErrorMessage(e));
      setPolicy(e instanceof HttpError ? e.policy : undefined);
      latest.current.onRefused(e);
    } finally {
      inFlight.current = false;
      setCreating(false);
    }
  };

  return { launch, creating, spinning, refusal, policy };
}
