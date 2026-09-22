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
 * caller's own Azure DevOps state reads "live" by then, false otherwise (the
 * person closed the popup, or declined consent). It polls GET
 * /me/scm-access rather than trusting anything the popup's own page says —
 * that page is Microsoft's consent screen and then the daemon's own redirect
 * target, neither of which is this window's to instrument.
 *
 * blockedUrl is set when the browser refused to open the popup at all
 * (review finding F9): the caller renders a plain fallback link to it, and
 * connectFallback() starts the SAME poll (bounded, since there is no popup
 * handle to watch) once that link is used (review follow-up N1) — so the
 * dialog still advances when the person comes back connected.
 */
export function useAdoConnect(): {
  connecting: boolean;
  connect: () => Promise<boolean>;
  connectFallback: () => Promise<boolean>;
  blockedUrl: string | null;
} {
  const [connecting, setConnecting] = React.useState(false);
  const [blockedUrl, setBlockedUrl] = React.useState<string | null>(null);
  const timerRef = React.useRef<number | null>(null);
  // The in-flight poll's own resolver, so unmounting can settle its promise
  // (review follow-up N6) instead of leaving an awaiter hung forever.
  const resolveRef = React.useRef<((ok: boolean) => void) | null>(null);
  const mountedRef = React.useRef(true);

  React.useEffect(
    () => () => {
      mountedRef.current = false;
      if (timerRef.current !== null) window.clearInterval(timerRef.current);
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
  const poll = (shouldGiveUp: () => boolean, onLive?: () => void): Promise<boolean> => {
    setConnecting(true);
    return new Promise<boolean>((resolve) => {
      const finish = (ok: boolean) => {
        if (timerRef.current !== null) {
          window.clearInterval(timerRef.current);
          timerRef.current = null;
        }
        resolveRef.current = null;
        if (mountedRef.current) setConnecting(false);
        resolve(ok);
      };
      resolveRef.current = finish;
      timerRef.current = window.setInterval(() => {
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
  };

  const connect = React.useCallback((): Promise<boolean> => {
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
      setBlockedUrl(SIGNIN_URL);
      return Promise.resolve(false);
    }
    popup.opener = null;
    popup.location.href = SIGNIN_URL;
    return poll(
      () => popup.closed,
      () => popup.close(),
    );
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  // review follow-up N1: the blocked-popup fallback link opens sign-in in a
  // new tab the browser owns; this starts the SAME poll (bounded, see
  // FALLBACK_POLL_TIMEOUT_MS) so the dialog still advances on return.
  const connectFallback = React.useCallback((): Promise<boolean> => {
    const deadline = Date.now() + FALLBACK_POLL_TIMEOUT_MS;
    return poll(() => Date.now() > deadline);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  return { connecting, connect, connectFallback, blockedUrl };
}
