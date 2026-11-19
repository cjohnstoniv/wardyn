/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { HttpError, identityCheckSettled, isSignedOutHold, onAuthChange, serverSentence, setToken, wfetch } from "../../lib/api/core";
import type { Me } from "../../lib/api/health";

type Session = Me | "unauthed" | "unreachable";
type Status = "idle" | "waiting" | "blocked" | "closed" | "unreachable" | "rejected";

interface Wait {
  over: () => boolean;
  onOver: () => void;
  onLive?: () => void;
  keepPolling?: boolean;
}

/** Why the last /me read gave no session: the status Wardyn answered (0 when nothing answered) and the
 *  sentence its answer named ("" when none). */
export type Outage = { status: number; said: string };
const NO_ANSWER: Outage = { status: 0, said: "" };

async function readSession(signal: AbortSignal, heard: (outage: Outage) => void): Promise<Session> {
  let res: Response;
  try {
    res = await wfetch("/me", { signal });
  } catch (e) {
    return e instanceof HttpError && e.status === 401 ? "unauthed" : "unreachable";
  }
  if (res.ok) {
    try {
      return (await res.json()) as Me;
    } catch (e) {
      // Only a body that does not parse is an answer; a body read that broke off or ran out of time is none.
      if (e instanceof SyntaxError) heard({ status: res.status, said: "" });
      return "unreachable";
    }
  }
  heard({ status: res.status, said: await serverSentence(res) });
  return "unreachable";
}

/** Serialize the layer's /me reads across renewal, cancellation and token checks. */
export function useSessionPoll(accept: (me: Me) => boolean) {
  const [status, setStatus] = React.useState<Status>("idle");
  const [busy, setBusy] = React.useState(false);
  const [outage, setOutage] = React.useState<Outage>(NO_ANSWER);
  const acceptRef = React.useRef(accept);
  acceptRef.current = accept;
  const interval = React.useRef<number | undefined>(undefined);
  const generation = React.useRef(0);
  const inFlight = React.useRef<AbortController | null>(null);
  const pending = React.useRef(false);
  const tickRef = React.useRef<(() => void) | null>(null);

  const invalidate = React.useCallback(() => {
    generation.current++;
    inFlight.current?.abort();
  }, []);
  const stopPoll = React.useCallback(() => {
    window.clearInterval(interval.current);
    interval.current = undefined;
    tickRef.current = null;
    pending.current = false;
    setBusy(false);
    invalidate();
  }, [invalidate]);

  const read = React.useCallback((receive: (session: Session) => void) => {
    if (inFlight.current) return;
    pending.current = false;
    const controller = new AbortController();
    inFlight.current = controller;
    const started = generation.current;
    let heard = NO_ANSWER;
    void readSession(controller.signal, (o) => (heard = o)).then((session) => {
      if (started !== generation.current) return;
      if (session === "unreachable") setOutage(heard);
      receive(session);
      // A write held for this check goes (or is refused) once /me has said who is signed in, or that no one
      // is. An error is not an answer: it leaves the write to the hold's bound, which refuses it.
      if (session !== "unreachable") identityCheckSettled();
    }).finally(() => {
      inFlight.current = null;
      if (pending.current) tickRef.current?.();
    });
  }, []);

  React.useEffect(() => {
    const refresh = () => {
      invalidate();
      if (!tickRef.current) return;
      pending.current = true;
      tickRef.current();
    };
    const visible = () => { if (document.visibilityState === "visible") refresh(); };
    const unsubscribe = onAuthChange(refresh);
    window.addEventListener("focus", refresh);
    document.addEventListener("visibilitychange", visible);
    return () => {
      unsubscribe();
      window.removeEventListener("focus", refresh);
      document.removeEventListener("visibilitychange", visible);
      stopPoll();
    };
  }, [invalidate, stopPoll]);

  const startPoll = React.useCallback((wait: Wait | null, until?: number, onEnd?: () => void) => {
    stopPoll();
    setBusy(false);
    if (wait) setStatus("waiting");
    let over = false;
    const expired = () => {
      if (!until || Date.now() <= until) return false;
      stopPoll();
      onEnd?.();
      return true;
    };
    const observeClose = () => {
      if (over || !wait?.over()) return false;
      over = true;
      invalidate();
      pending.current = true;
      return true;
    };
    const tick = () => {
      if (expired()) return;
      observeClose();
      read((session) => {
        if (expired() || observeClose()) return;
        if (typeof session === "object" && acceptRef.current(session)) {
          wait?.onLive?.();
          stopPoll();
        } else if (over) {
          wait?.onOver();
          if (!wait?.keepPolling) stopPoll();
        } else if (wait) {
          setStatus(session === "unreachable" ? "unreachable" : "waiting");
        }
      });
    };
    tickRef.current = tick;
    interval.current = window.setInterval(tick, 1500);
    // A replaced read must settle before the new generation can read again.
    pending.current = inFlight.current !== null;
    if (!wait) tick();
  }, [invalidate, read, stopPoll]);

  // A quiet auth invalidation gets one read, then focus/visibility/auth events can retry an outage.
  const checkOnce = React.useCallback(() => {
    stopPoll();
    tickRef.current = () => {
      if (isSignedOutHold()) { stopPoll(); return; }
      read((session) => {
        if (isSignedOutHold() || session === "unauthed" || (typeof session === "object" && acceptRef.current(session))) stopPoll();
      });
    };
    pending.current = inFlight.current !== null;
    tickRef.current();
  }, [read, stopPoll]);

  // Another tab's auth event reached a tab that is signed out and not waiting on a sign-in of its own:
  // one read of who is signed in now, then the focus and visibility events can retry an outage.
  const recheck = React.useCallback(() => {
    if (tickRef.current) return;
    tickRef.current = () => read((session) => {
      if (session === "unauthed" || (typeof session === "object" && acceptRef.current(session))) stopPoll();
    });
    pending.current = inFlight.current !== null;
    tickRef.current();
  }, [read, stopPoll]);

  const submitToken = (token: string) => {
    stopPoll();
    setBusy(true);
    setStatus("idle");
    setToken(token);
    tickRef.current = () => read((session) => {
      setBusy(false);
      // A live answer can wait on another auth mutation; its settlement must be able to retry.
      if (typeof session === "object" && !acceptRef.current(session)) return;
      stopPoll();
      if (session === "unauthed") setStatus("rejected");
      else if (session === "unreachable") setStatus("unreachable");
    });
    pending.current = true;
    tickRef.current();
  };

  return { status, setStatus, busy, outage, startPoll, stopPoll, checkOnce, recheck, submitToken };
}
