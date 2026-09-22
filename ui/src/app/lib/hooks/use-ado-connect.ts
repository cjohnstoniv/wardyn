/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Azure DevOps connect flow, driven from a POPUP rather than a top-level
// navigation (#386's launch door). The design's dialog-and-relaunch precedent
// (the 0.7.7 model_credential door) never leaves the page it was raised on,
// and this flow is a plain HTTP redirect (/api/v1/scm/azure-devops/signin,
// ado_entra.go) through Microsoft and back to the console — no device code,
// no in-app step of its own to render. A popup keeps that redirect off the
// caller's own window, so "this form comes back exactly as you left it"
// (ADO.LAUNCH_DIALOG_BODY) is true by construction: the form never left.
import * as React from "react";
import { scmAccess } from "../api/scm-access";

const POLL_MS = 1500;
const SIGNIN_URL = "/api/v1/scm/azure-devops/signin";
// The fallback link (review follow-up N1) opens a plain browser tab this
// hook has no handle on — nothing tells it the tab closed, so the poll it
// starts for that path is bounded instead, long enough for a real sign-in.
const FALLBACK_POLL_TIMEOUT_MS = 5 * 60 * 1000;

/**
 * connect() opens the connect popup and resolves once it closes: true if the
 * caller's own Azure DevOps state reads "live" by then, false if the person
 * closed the popup or declined consent, and null if the browser refused to
 * open the popup at all (review finding F1) — a blocked popup is neither a
 * connection nor a decline, so the caller must not treat it as one. It polls
 * GET /me/scm-access rather than trusting anything the popup's own page
 * says — that page is Microsoft's consent screen and then the daemon's own
 * redirect target, neither of which is this window's to instrument.
 *
 * blockedUrl is set alongside that null (review finding F9): the caller
 * renders a plain fallback link to it, and connectFallback() starts the SAME
 * poll (bounded, since there is no popup handle to watch) once that link is
 * used (review follow-up N1) — so the dialog still advances when the person
 * comes back connected.
 */
export function useAdoConnect(): {
  connecting: boolean;
  /** null means the popup was blocked outright — no connect/reject verdict
   *  ever happened, so the caller must keep asking rather than treat it as a
   *  declined connection (review finding F1). */
  connect: () => Promise<boolean | null>;
  connectFallback: () => Promise<boolean>;
  blockedUrl: string | null;
} {
  const [connecting, setConnecting] = React.useState(false);
  const [blockedUrl, setBlockedUrl] = React.useState<string | null>(null);
  // The in-flight poll's own resolver, so unmounting — or a fresh poll
  // starting, e.g. a second fallback-link click — can settle its promise and
  // clear its interval (review finding F2, follow-up N6) instead of leaving
  // an awaiter hung and its interval orphaned.
  const resolveRef = React.useRef<((ok: boolean) => void) | null>(null);
  const mountedRef = React.useRef(true);

  React.useEffect(
    () => () => {
      mountedRef.current = false;
      resolveRef.current?.(false);
      resolveRef.current = null;
    },
    [],
  );

  // The shared poll: ticks GET /me/scm-access until either a row reads
  // "live" (onLive fires, e.g. to close the popup, then resolves true) or
  // shouldGiveUp() says stop (resolves false). connect() gives up when the
  // popup closes; connectFallback() gives up after a bounded timeout, since
  // it has no popup to watch.
  const poll = React.useCallback((shouldGiveUp: () => boolean, onLive?: () => void): Promise<boolean> => {
    // Settle any poll already in flight (F2) before starting this one — a
    // second call (unmount, or the fallback link clicked again) must never
    // leave the previous interval running unobserved.
    resolveRef.current?.(false);
    setConnecting(true);
    return new Promise<boolean>((resolve) => {
      // Captured in THIS closure, not the shared ref: finish() below must
      // only ever clear the interval it started, never whichever one a
      // later poll() call has since put in the ref (F2's actual bug).
      let intervalId: number | null = null;
      const finish = (ok: boolean) => {
        if (intervalId !== null) {
          window.clearInterval(intervalId);
          intervalId = null;
        }
        if (resolveRef.current === finish) resolveRef.current = null;
        if (mountedRef.current) setConnecting(false);
        resolve(ok);
      };
      resolveRef.current = finish;
      intervalId = window.setInterval(() => {
        if (shouldGiveUp()) {
          finish(false);
          return;
        }
        scmAccess
          .getMine()
          .then((rows) => {
            if (!mountedRef.current) return;
            if (rows.some((r) => r.state === "live")) {
              onLive?.();
              finish(true);
            }
          })
          .catch(() => {
            /* transient — the next tick tries again */
          });
      }, POLL_MS);
    });
  }, []);

  const connect = React.useCallback((): Promise<boolean | null> => {
    setBlockedUrl(null);
    // about:blank FIRST, same-origin, then sever the opener reference and
    // navigate — not window.open(SIGNIN_URL, ...) directly. This is the
    // tabnabbing fix (reverse tabnabbing: a page we navigate to must not be
    // able to reach back via `window.opener` and redirect this window while
    // its sign-in runs unattended) — `rel="noopener"`'s own effect, done by
    // hand because `noopener` also drops the handle back to the popup this
    // hook needs to poll `.closed` on.
    const popup = window.open("about:blank", "wardyn-ado-connect", "width=520,height=680");
    if (!popup) {
      // null, not false (review finding F1): a blocked popup is neither a
      // connection nor a decline, and the launch door must keep the dialog
      // open — showing the fallback link — rather than close it as if the
      // person had answered.
      setBlockedUrl(SIGNIN_URL);
      return Promise.resolve(null);
    }
    popup.opener = null;
    popup.location.href = SIGNIN_URL;
    return poll(
      () => popup.closed,
      () => popup.close(),
    );
  }, [poll]);

  // review follow-up N1: the blocked-popup fallback link opens sign-in in a
  // new tab the browser owns; this starts the SAME poll (bounded, see
  // FALLBACK_POLL_TIMEOUT_MS) so the dialog still advances on return.
  const connectFallback = React.useCallback((): Promise<boolean> => {
    const deadline = Date.now() + FALLBACK_POLL_TIMEOUT_MS;
    return poll(() => Date.now() > deadline);
  }, [poll]);

  return { connecting, connect, connectFallback, blockedUrl };
}
