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

/**
 * connect() opens the connect popup and resolves once it closes: true if the
 * caller's own Azure DevOps state reads "live" by then, false otherwise (the
 * person closed the popup, or declined consent). It polls GET
 * /me/scm-access rather than trusting anything the popup's own page says —
 * that page is Microsoft's consent screen and then the daemon's own redirect
 * target, neither of which is this window's to instrument.
 *
 * blockedUrl is set when the browser refused to open the popup at all
 * (review finding F9): the caller renders a plain link to it instead.
 */
export function useAdoConnect(): { connecting: boolean; connect: () => Promise<boolean>; blockedUrl: string | null } {
  const [connecting, setConnecting] = React.useState(false);
  const [blockedUrl, setBlockedUrl] = React.useState<string | null>(null);
  const timerRef = React.useRef<number | null>(null);
  const mountedRef = React.useRef(true);

  // Stop polling on unmount (review finding F9) — an interval left running
  // past the screen it was raised on both leaks and can still call
  // scmAccess.getMine() for a caller who navigated away.
  React.useEffect(
    () => () => {
      mountedRef.current = false;
      if (timerRef.current !== null) window.clearInterval(timerRef.current);
    },
    [],
  );

  const connect = React.useCallback((): Promise<boolean> => {
    setBlockedUrl(null);
    setConnecting(true);
    return new Promise<boolean>((resolve) => {
      // about:blank FIRST, same-origin, then sever the opener reference and
      // navigate — not window.open(SIGNIN_URL, ...) directly (review finding
      // F9). Microsoft's sign-in page sets a Cross-Origin-Opener-Policy that
      // can sever an opener link formed AT a cross-origin navigation; forming
      // it here, one document that is still same-origin with this window,
      // and only THEN navigating the popup, is what keeps `popup.closed`
      // readable from this side through that hop.
      const popup = window.open("about:blank", "wardyn-ado-connect", "width=520,height=680");
      if (!popup) {
        setConnecting(false);
        setBlockedUrl(SIGNIN_URL);
        resolve(false);
        return;
      }
      popup.opener = null;
      popup.location.href = SIGNIN_URL;

      const finish = (ok: boolean) => {
        if (timerRef.current !== null) {
          window.clearInterval(timerRef.current);
          timerRef.current = null;
        }
        if (mountedRef.current) setConnecting(false);
        resolve(ok);
      };
      timerRef.current = window.setInterval(() => {
        if (popup.closed) {
          finish(false);
          return;
        }
        scmAccess
          .getMine()
          .then((rows) => {
            if (!mountedRef.current) return;
            if (rows.some((r) => r.state === "live")) {
              popup.close();
              finish(true);
            }
          })
          .catch(() => {
            /* transient — the next tick tries again while the popup stays open */
          });
      }, POLL_MS);
    });
  }, []);

  return { connecting, connect, blockedUrl };
}
