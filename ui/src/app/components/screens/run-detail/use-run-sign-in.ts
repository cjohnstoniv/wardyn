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
import { useOperatorResolved, usePrincipal } from "../../wardyn/operator-context";

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

const EMPTY_READ = { waiting: null, ended: false, failed: false, checking: false };

export function useRunSignIn(runId: string, createdAt: string, enabled: boolean): RunSignInRead {
  const principal = usePrincipal();
  const resolved = useOperatorResolved();
  const scope = React.useMemo(() => ({ runId, createdAt, principal, enabled, resolved }), [runId, createdAt, principal, enabled, resolved]);
  const [state, setState] = React.useState<Omit<RunSignInRead, "retry"> & { scope: typeof scope }>({ ...EMPTY_READ, scope });
  const inFlight = React.useRef(false);
  const nextRead = React.useRef<(() => void) | null>(null);
  const retryRef = React.useRef<(() => void) | null>(null);

  React.useEffect(() => {
    setState({ ...EMPTY_READ, scope });
    if (!scope.enabled || !scope.resolved || !scope.principal) return;
    let cancelled = false;
    let stopped = false;
    let seenWaiting = false;
    let answered = false;
    let generation = 0;
    let controller: AbortController | null = null;
    let timer: ReturnType<typeof setTimeout> | undefined;
    let checkingTimer: ReturnType<typeof setTimeout> | undefined;
    const checking = () => {
      clearTimeout(checkingTimer);
      if (!answered) checkingTimer = setTimeout(() => setState((s) => ({ ...s, checking: true })), SIGN_IN_CHECKING_MS);
    };
    const tick = async () => {
      if (cancelled || stopped) return;
      if (inFlight.current) {
        nextRead.current = () => void tick();
        return;
      }
      inFlight.current = true;
      const started = generation;
      controller = new AbortController();
      try {
        const res = await runSignIn.get(scope.runId, controller.signal);
        if (cancelled || started !== generation) return;
        answered = true;
        clearTimeout(checkingTimer);
        if (res.state === "waiting") {
          seenWaiting = true;
          setState((s) => {
            // Identical answers keep both the object and the live text unchanged.
            if (s.waiting && s.waiting.user_code === res.user_code && s.waiting.verification_url === res.verification_url && !s.failed) return s;
            return { scope, waiting: { state: "waiting", user_code: res.user_code ?? "", verification_url: res.verification_url ?? "" }, ended: false, failed: false, checking: false };
          });
        } else {
          setState({ scope, waiting: null, ended: seenWaiting, failed: false, checking: false });
          stopped = seenWaiting || Date.now() - Date.parse(scope.createdAt) > SIGN_IN_GRACE_MS;
        }
        if (!stopped) timer = setTimeout(() => void tick(), SIGN_IN_POLL_MS);
      } catch {
        if (cancelled || started !== generation) return;
        stopped = true;
        clearTimeout(checkingTimer);
        setState((s) => ({ ...s, failed: true, checking: false }));
      } finally {
        inFlight.current = false;
        controller = null;
        const next = nextRead.current;
        nextRead.current = null;
        next?.();
      }
    };
    const refresh = () => {
      if (stopped) return;
      generation++;
      controller?.abort();
      clearTimeout(timer);
      void tick();
    };
    const visible = () => { if (document.visibilityState === "visible") refresh(); };
    retryRef.current = () => {
      stopped = false;
      setState((s) => ({ ...s, failed: false, checking: false }));
      checking();
      refresh();
    };
    checking();
    void tick();
    window.addEventListener("focus", refresh);
    document.addEventListener("visibilitychange", visible);
    return () => {
      cancelled = true;
      controller?.abort();
      clearTimeout(timer);
      clearTimeout(checkingTimer);
      nextRead.current = null;
      retryRef.current = null;
      window.removeEventListener("focus", refresh);
      document.removeEventListener("visibilitychange", visible);
    };
  }, [scope]);

  const retry = React.useCallback(() => retryRef.current?.(), []);
  // An effect reset alone exposes the previous person's code for one render.
  return { ...(state.scope === scope ? state : EMPTY_READ), retry };
}
