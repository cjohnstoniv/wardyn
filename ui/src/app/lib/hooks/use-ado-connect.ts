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

/**
 * connect() opens the connect popup and resolves once it closes: true if the
 * caller's own Azure DevOps state reads "live" by then, false otherwise (the
 * person closed the popup, or declined consent). It polls GET
 * /me/scm-access rather than trusting anything the popup's own page says —
 * that page is Microsoft's consent screen and then the daemon's own redirect
 * target, neither of which is this window's to instrument.
 */
export function useAdoConnect(): { connecting: boolean; connect: () => Promise<boolean> } {
  const [connecting, setConnecting] = React.useState(false);

  const connect = React.useCallback((): Promise<boolean> => {
    setConnecting(true);
    return new Promise<boolean>((resolve) => {
      const popup = window.open("/api/v1/scm/azure-devops/signin", "wardyn-ado-connect", "width=520,height=680");
      if (!popup) {
        // Popup blocked — nothing to poll and nothing this hook can repair.
        setConnecting(false);
        resolve(false);
        return;
      }
      const finish = (ok: boolean) => {
        window.clearInterval(timer);
        setConnecting(false);
        resolve(ok);
      };
      const timer = window.setInterval(() => {
        if (popup.closed) {
          finish(false);
          return;
        }
        scmAccess
          .getMine()
          .then((a) => {
            if (a.state === "live") {
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

  return { connecting, connect };
}
