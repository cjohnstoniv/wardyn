/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The two reads behind New run's "Run in" choice: the person's units (GET /policies/default, no run_in, so the
// home unit's answer) and their requests (GET /governance/requests). The Run step reads them when it opens and
// Review reads them again when it checks.

import * as React from "react";
import { requests } from "../../../../../lib/api/governance-requests";
import { policies, type DefaultPolicyContext } from "../../../../../lib/api/policies";
import type { GovernanceRequestList } from "../../../../../lib/types/governance-requests";
import { runInChoices, type RunInChoices } from "./run-in-model";

export interface RunInRead {
  status: "loading" | "ready" | "failed";
  choices?: RunInChoices;
  /** The person's requests; null when they could not be read, which is not a failure of the whole. */
  list?: GovernanceRequestList | null;
  /** The clock at the read, so an elevation that has ended since is not offered. */
  now: number;
  reload: () => void;
}

interface Answer {
  contexts: DefaultPolicyContext[] | undefined;
  list: GovernanceRequestList | null;
  now: number;
}

export type LoadRunIn = () => Promise<Answer>;

export const loadRunIn: LoadRunIn = async () => {
  const [policy, list] = await Promise.all([policies.getDefaultPolicy(), requests.list().catch(() => null)]);
  return { contexts: policy.contexts, list, now: Date.now() };
};

/** An answer in hand stays while a reload is out, and a failed reload does not replace it. */
export function useRunIn(load: LoadRunIn = loadRunIn): RunInRead {
  const [asked, setAsked] = React.useState(0);
  const [answer, setAnswer] = React.useState<Answer>();
  const [failed, setFailed] = React.useState(false);
  React.useEffect(() => {
    let live = true;
    load().then(
      (a) => {
        if (!live) return;
        setAnswer(a);
        setFailed(false);
      },
      () => live && setFailed(true),
    );
    return () => {
      live = false;
    };
  }, [load, asked]);
  const reload = React.useCallback(() => setAsked((n) => n + 1), []);
  const choices = React.useMemo(() => (answer ? runInChoices(answer.contexts, answer.list, answer.now) : undefined), [answer]);
  if (answer) return { status: "ready", choices, list: answer.list, now: answer.now, reload };
  return { status: failed ? "failed" : "loading", now: Date.now(), reload };
}
