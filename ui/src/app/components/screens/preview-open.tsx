/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// /previews/{run}/{port}: the link an Organisation preview's Copy link gives. It mints the caller's ticket for that one
// preview, binds it to this browser and enters, in this tab. A person the preview admits gets through. A 404 with reason
// run_not_found (the preview has ended, was revoked, or does not admit this person: the ticket route answers all three
// alike) shows the sentence a visitor of the preview's address would be told. Any other refusal shows the server's own.
import * as React from "react";
import { useParams } from "react-router-dom";
import { Loader2 } from "lucide-react";
import { getExpectedPrincipal, HttpError } from "../../lib/api/core";
import { health } from "../../lib/api/health";
import { previews } from "../../lib/api/previews";
import { PREVIEW_ENDED_REASON } from "../../lib/types/previews";
import { getErrorMessage } from "../../lib/format";
import { PREVIEW_REVOKED, PREVIEW_ROUTE as T, RUN_PREVIEWS } from "../wardyn/copy/run-previews";
import { STATES } from "../wardyn/states";
import { enterUIApp } from "./run-detail-ssh";

type Phase = { kind: "opening" } | { kind: "refused"; text: string };

/** The name of the app the UI gateway presents for a preview of one port (types.PreviewAppPrefix + port). */
const previewApp = (port: number) => `__preview-${port}`;

/** Only the ticket route's 404 run_not_found is the revoked sentence; every other answer is the server's, as it came. */
function refusalText(e: unknown): string {
  if (e instanceof HttpError && e.status === 404 && e.reason === PREVIEW_ENDED_REASON) return PREVIEW_REVOKED;
  return getErrorMessage(e);
}

export function PreviewOpenScreen() {
  const { run = "", port = "" } = useParams();
  const [phase, setPhase] = React.useState<Phase>({ kind: "opening" });
  const card = React.useRef<HTMLElement>(null);
  // A ticket is single-use: a second run of the effect for the same route (StrictMode) resumes the attempt already
  // in flight instead of minting another. live is the attempt this page still shows; an attempt for a route the
  // person has left is stale, so it never takes this tab anywhere and its error never replaces the current page.
  const started = React.useRef({ key: "", id: 0 });
  const live = React.useRef(0);

  React.useEffect(() => {
    const key = `${run}/${port}`;
    const fresh = started.current.key !== key;
    if (fresh) started.current = { key, id: started.current.id + 1 };
    const id = started.current.id;
    live.current = id;
    if (fresh) {
      const stale = () => live.current !== id;
      setPhase({ kind: "opening" });
      // The ticket follows the health read: it is minted for the person who opened the page, not whoever signs in.
      const person = getExpectedPrincipal();
      void (async () => {
        try {
          const h = await health.health();
          if (stale()) return;
          if (h.status !== "ok") {
            setPhase({ kind: "refused", text: STATES.ERROR_DEFAULT });
            return;
          }
          const ticket = await previews.ticket(run, Number(port), person);
          if (stale()) return;
          await enterUIApp(run, previewApp(Number(port)), h.ui_sandbox ?? null, { ticket, target: "_self", stale });
        } catch (e) {
          if (!stale()) setPhase({ kind: "refused", text: refusalText(e) });
        }
      })();
    }
    return () => {
      live.current = 0;
    };
  }, [run, port]);

  // The page is a console page, not a dialog: focus lands on it, and again on the refusal when that replaces "Opening".
  React.useEffect(() => card.current?.focus(), [phase.kind]);

  return (
    <div className="mx-auto w-full max-w-[560px] px-4 py-8 sm:px-6">
      <section
        ref={card}
        tabIndex={-1}
        className="flex min-h-[220px] flex-col items-center justify-center gap-3 rounded-xl border border-border bg-card p-6 text-center outline-none"
      >
        {phase.kind === "opening" ? (
          <p role="status" className="flex flex-col items-center gap-3 text-sm font-medium text-foreground">
            <Loader2 className="size-5 animate-spin text-muted-foreground" aria-hidden="true" />
            {T.OPENING}
          </p>
        ) : (
          <div role="alert" className="w-full rounded-lg border border-warning/30 bg-warning-subtle px-3 py-2.5 text-left">
            <p className="text-xs font-medium text-foreground">{RUN_PREVIEWS.OPEN_FAILED(Number(port))}</p>
            <p className="mt-1.5 font-mono text-meta leading-relaxed text-muted-foreground">{phase.text}</p>
          </div>
        )}
      </section>
    </div>
  );
}
