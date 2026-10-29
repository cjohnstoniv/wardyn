/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// /open/{run}/{app} (r16 T2-b): the link people are given for an inbound app. It mints the caller's ticket for
// that one app, binds it to this browser and enters, in this tab. The owner and a person the app's callers name
// get through; any other answer is one of the sentences below. A person who is not a caller hears what a missing
// run says, so the page never confirms that a run exists.
import * as React from "react";
import { useParams } from "react-router-dom";
import { CircleX, Loader2 } from "lucide-react";
import { HttpError } from "../../lib/api/core";
import { health } from "../../lib/api/health";
import { runs } from "../../lib/api/runs";
import { getErrorMessage } from "../../lib/format";
import { OPEN_APP as T } from "../wardyn/copy/ui-apps";
import { STATES } from "../wardyn/states";
import { enterUIApp } from "./run-detail-ssh";

type Phase = { kind: "opening" } | { kind: "refused"; text: string };

// The server's reasons for a run that is not up (attach_not_running, attach_run_kept) and for a named caller on a
// shared host (inbound_needs_host_mode), as internal/api/inbound_ticket.go answers them.
const NOT_RUNNING = new Set(["attach_not_running", "attach_run_kept"]);
const NEEDS_HOST_MODE = "inbound_needs_host_mode";

function refusalText(e: unknown): string {
  if (e instanceof HttpError) {
    if (e.reason === NEEDS_HOST_MODE) return T.SHARED_HOST;
    if (NOT_RUNNING.has(e.reason)) return T.NOT_RUNNING;
    if (e.status === 404) return T.NOT_SHARED;
  }
  return getErrorMessage(e);
}

export function OpenAppScreen() {
  const { run = "", app = "" } = useParams();
  const [phase, setPhase] = React.useState<Phase>({ kind: "opening" });
  // A ticket is single-use: a second run of the effect (StrictMode) must not mint another.
  const started = React.useRef("");

  React.useEffect(() => {
    const key = `${run}/${app}`;
    if (started.current === key) return;
    started.current = key;
    setPhase({ kind: "opening" });
    void (async () => {
      try {
        const h = await health.health();
        if (h.status !== "ok") throw new Error(STATES.ERROR_DEFAULT);
        if (!h.ui_sandbox?.enabled) {
          setPhase({ kind: "refused", text: T.GATEWAY_OFF });
          return;
        }
        const ticket = await runs.appTicket(run, app);
        await enterUIApp(run, app, h.ui_sandbox, { ticket, target: "_self" });
      } catch (e) {
        setPhase({ kind: "refused", text: refusalText(e) });
      }
    })();
  }, [run, app]);

  return (
    <div className="mx-auto w-full max-w-[560px] px-4 py-8 sm:px-6">
      <section className="flex min-h-[220px] flex-col items-center justify-center gap-3 rounded-xl border border-border bg-card p-6 text-center">
        {phase.kind === "opening" ? (
          <p role="status" className="flex flex-col items-center gap-3 text-sm font-medium text-foreground">
            <Loader2 className="size-5 animate-spin text-muted-foreground" aria-hidden="true" />
            {T.OPENING(app)}
          </p>
        ) : (
          <p role="alert" className="flex flex-col items-center gap-3 text-sm text-foreground">
            <CircleX className="size-5 text-danger" aria-hidden="true" />
            {phase.text}
          </p>
        )}
      </section>
    </div>
  );
}
