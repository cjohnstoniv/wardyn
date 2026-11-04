/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// "Needs you" notices (#2963, mock M8-Banner-A): a takeover request anywhere in the caller's runs, or a hold
// on one of their own runs that waits on a named site, raises a console toast that stays until they act, and — only if they allowed browser
// notifications (opt-in; this never asks) and the tab is in the background — a browser
// notification. Both carry the agent's reason and the run's name, never a screenshot or a value
// from the screen. The list is the server's own scope (a member sees their runs only).
import * as React from "react";
import { useNavigate } from "react-router-dom";
import { toast } from "sonner";
import { approvals as approvalsApi } from "../../lib/api/approvals";
import { runs as runsApi } from "../../lib/api/runs";
import { isHeld, runHeadline } from "../../lib/types";
import { runPath, useConsoleMode } from "./console-view";
import { NEEDS_YOU } from "./copy/needs-you";
import { usePrincipal } from "./operator-context";

const POLL_MS = 15_000;
const SEEN_KEY = "wardyn.needs-you.seen";

function seenIds(): Set<string> {
  try {
    return new Set(JSON.parse(window.sessionStorage.getItem(SEEN_KEY) ?? "[]") as string[]);
  } catch {
    return new Set();
  }
}

function remember(ids: Set<string>) {
  try {
    window.sessionStorage.setItem(SEEN_KEY, JSON.stringify([...ids].slice(-200)));
  } catch {
    // Without storage a reload shows the notice again; nothing else depends on it.
  }
}

export function NeedsYouNotices() {
  const navigate = useNavigate();
  const view = useConsoleMode();
  const principal = usePrincipal();
  React.useEffect(() => {
    let alive = true;
    const seen = seenIds();
    const check = async () => {
      let rows;
      try {
        rows = await approvalsApi.listApprovals("PENDING", "", view);
      } catch {
        return; // a missed poll is retried on the next tick
      }
      for (const ap of rows) {
        if (!alive || seen.has(ap.id)) continue;
        // A takeover request, or a hold (egress to a named host) that has frozen the person's own run.
        const takeover = ap.kind === "human_takeover";
        const host = typeof ap.requested_scope.host === "string" ? ap.requested_scope.host : "";
        if (!takeover && !(isHeld(ap) && host)) continue;
        const run = await runsApi.getRun(ap.run_id).catch(() => undefined);
        if (!takeover && run?.created_by !== principal) {
          // Someone else's run is final, so it is fetched once; a run that could not be fetched is retried.
          if (run) {
            seen.add(ap.id);
            remember(seen);
          }
          continue;
        }
        seen.add(ap.id);
        remember(seen);
        const name = run ? runHeadline(run) : ap.run_id;
        const open = () => void navigate(runPath(view, ap.run_id));
        if (!takeover) {
          toast(NEEDS_YOU.toast.holdTitle(name, host), {
            id: `needs-you-${ap.id}`,
            duration: Infinity,
            action: { label: NEEDS_YOU.toast.review, onClick: open },
            cancel: { label: NEEDS_YOU.toast.later, onClick: () => undefined },
          });
          continue;
        }
        const reason = String(ap.requested_scope.reason ?? "");
        toast(NEEDS_YOU.toast.title(name), {
          id: `needs-you-${ap.id}`,
          description: reason,
          duration: Infinity,
          // The button only opens the run page, so it says so; a background run's notice carries it too.
          action: { label: NEEDS_YOU.toast.openRun, onClick: open },
          cancel: { label: NEEDS_YOU.toast.later, onClick: () => undefined },
        });
        if (typeof Notification !== "undefined" && Notification.permission === "granted" && document.hidden) {
          new Notification(NEEDS_YOU.toast.browserTitle(name), { body: reason, tag: `needs-you-${ap.id}` });
        }
      }
    };
    void check();
    const t = window.setInterval(() => void check(), POLL_MS);
    return () => {
      alive = false;
      window.clearInterval(t);
    };
  }, [navigate, view, principal]);
  return null;
}
