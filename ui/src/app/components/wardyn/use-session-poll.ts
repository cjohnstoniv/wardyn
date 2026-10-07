/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { HttpError, onAuthChange, setToken, wfetch } from "../../lib/api/core";
import type { Me } from "../../lib/api/health";

type Session = Me | "unauthed" | "unreachable";
type Status = "idle" | "waiting" | "blocked" | "closed" | "unreachable" | "rejected";

interface Wait {
  over: () => boolean;
  onOver: () => void;
  onLive?: () => void;
  keepPolling?: boolean;
}

async function readSession(signal: AbortSignal): Promise<Session> {
  try {
    const res = await wfetch("/me", { signal });
    if (res.ok) return (await res.json()) as Me;
    await res.text().catch(() => "");
    return "unreachable";
  } catch (e) {
    return e instanceof HttpError && e.status === 401 ? "unauthed" : "unreachable";
  }
}

/** Serialize the layer's /me reads across renewal, cancellation and token checks. */
export function useSessionPoll(accept: (me: Me) => boolean) {
  const [status, setStatus] = React.useState<Status>("idle");
  const [busy, setBusy] = React.useState(false);
  const acceptRef = React.useRef(accept);
  acceptRef.current = accept;
  const interval = React.useRef<number | undefined>();
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
    void readSession(controller.signal).then((session) => {
      if (started === generation.current) receive(session);
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
    onAuthChange(refresh);
    window.addEventListener("focus", refresh);
    document.addEventListener("visibilitychange", visible);
    return () => {
      onAuthChange(null);
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

  const submitToken = (token: string) => {
    stopPoll();
    setBusy(true);
    setStatus("idle");
    setToken(token);
    tickRef.current = () => read((session) => {
      stopPoll();
      setBusy(false);
      if (session === "unauthed") setStatus("rejected");
      else if (session === "unreachable") setStatus("unreachable");
      else acceptRef.current(session);
    });
    pending.current = true;
    tickRef.current();
  };

  return { status, setStatus, busy, startPoll, stopPoll, submitToken };
}
