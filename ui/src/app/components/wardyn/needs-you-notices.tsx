/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// "Needs you" notices (#2963, mock M8-Banner-A): a takeover request anywhere in the caller's runs
// raises a console toast that stays until they act, and — only if they allowed browser
// notifications (opt-in; this never asks) and the tab is in the background — a browser
// notification. Both carry the agent's reason and the run's name, never a screenshot or a value
// from the screen. The list is the server's own scope (a member sees their runs only).
import * as React from "react";
import { useNavigate } from "react-router-dom";
import { toast } from "sonner";
import { approvals as approvalsApi } from "../../lib/api/approvals";
import { runs as runsApi } from "../../lib/api/runs";
import { runHeadline } from "../../lib/types";
import { runPath, useConsoleMode } from "./console-view";
import { NEEDS_YOU } from "./copy/needs-you";

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
        if (!alive || ap.kind !== "human_takeover" || seen.has(ap.id)) continue;
        seen.add(ap.id);
        remember(seen);
        const run = await runsApi.getRun(ap.run_id).catch(() => undefined);
        const name = run ? runHeadline(run) : ap.run_id;
        const reason = String(ap.requested_scope.reason ?? "");
        toast(NEEDS_YOU.toast.title(name), {
          id: `needs-you-${ap.id}`,
          description: reason,
          duration: Infinity,
          action: { label: NEEDS_YOU.banner.takeOver, onClick: () => void navigate(runPath(view, ap.run_id)) },
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
  }, [navigate, view]);
  return null;
}
