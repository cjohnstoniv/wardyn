/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// One read loop for a waiting sign-in, shared by the run page's strip and the
// Runs list row (approved 088 mock, M2 D4/D7). It asks GET /runs/{id}/sign-in
// on open and every 5 s. Reading continues until the first `waiting` answer
// during the run's first two minutes (a page opened seconds after launch sees
// `not_waiting` before the code is drawn); once `waiting` was seen, the next
// `not_waiting` ends the loop; past two minutes (counted from the run's own
// start, not from page open) the next `not_waiting` ends it either way. The
// first failed read stops the loop until `retry`, and keeps the last answer.
import * as React from "react";
import { runSignIn } from "../../../lib/api/run-sign-in";
import type { RunSignIn } from "../../../lib/types";

export const SIGN_IN_POLL_MS = 5000;
/** How long after the run's start a `not_waiting` answer still keeps the loop going. */
export const SIGN_IN_GRACE_MS = 2 * 60 * 1000;
/** The first read shows nothing until it has been in flight this long. */
export const SIGN_IN_CHECKING_MS = 1000;

export interface RunSignInRead {
  /** The latest `waiting` answer, kept through a failed read; null once it is not waiting. */
  waiting: Required<RunSignIn> | null;
  /** The sign-in was waiting and has stopped: approved, failed or expired. */
  ended: boolean;
  /** A read failed; polling is stopped until `retry`. */
  failed: boolean;
  /** The first read has been in flight for over a second. */
  checking: boolean;
  retry: () => void;
}

export function useRunSignIn(runId: string, createdAt: string, enabled: boolean): RunSignInRead {
  const [waiting, setWaiting] = React.useState<Required<RunSignIn> | null>(null);
  const [ended, setEnded] = React.useState(false);
  const [failed, setFailed] = React.useState(false);
  const [answered, setAnswered] = React.useState(false);
  const [checking, setChecking] = React.useState(false);
  const [attempt, setAttempt] = React.useState(0);
  const seenWaiting = React.useRef(false);
  const startedMs = Date.parse(createdAt);

  React.useEffect(() => {
    if (!enabled) return;
    let cancelled = false;
    let timer: ReturnType<typeof setTimeout> | undefined;
    const tick = async () => {
      let res: RunSignIn;
      try {
        res = await runSignIn.get(runId);
      } catch {
        if (!cancelled) setFailed(true);
        return;
      }
      if (cancelled) return;
      setFailed(false);
      setAnswered(true);
      if (res.state === "waiting") {
        seenWaiting.current = true;
        // The strip's text changes only with the code or the link: keep the
        // same object while both are unchanged, so a 5 s read re-renders nothing.
        setWaiting((prev) =>
          prev && prev.user_code === res.user_code && prev.verification_url === res.verification_url
            ? prev
            : { state: "waiting", user_code: res.user_code ?? "", verification_url: res.verification_url ?? "" },
        );
        timer = setTimeout(() => void tick(), SIGN_IN_POLL_MS);
        return;
      }
      setWaiting(null);
      if (seenWaiting.current) {
        setEnded(true);
        return;
      }
      if (Date.now() - startedMs > SIGN_IN_GRACE_MS) return;
      timer = setTimeout(() => void tick(), SIGN_IN_POLL_MS);
    };
    void tick();
    return () => {
      cancelled = true;
      if (timer) clearTimeout(timer);
    };
  }, [runId, enabled, startedMs, attempt]);

  React.useEffect(() => {
    if (!enabled || answered || failed) {
      setChecking(false);
      return;
    }
    const t = setTimeout(() => setChecking(true), SIGN_IN_CHECKING_MS);
    return () => clearTimeout(t);
  }, [enabled, answered, failed]);

  const retry = React.useCallback(() => {
    setFailed(false);
    setAttempt((n) => n + 1);
  }, []);
  return { waiting: enabled ? waiting : null, ended: enabled && ended, failed: enabled && failed, checking, retry };
}
