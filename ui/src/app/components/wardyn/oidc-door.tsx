/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The sign-in door for a provider that signs people in at its own authority: Continue opens the authority's sign-in in
// a new window, the page waits, and the door closes when the person's own state for the provider shows a new
// sign-in. The window ends on the console (oidc-signin-result.tsx), which tells this one how it went. A browser
// that blocks the window gets a link to open it instead, and the wait is then bounded: nothing says that tab closed.
import * as React from "react";
import { Loader2 } from "lucide-react";
import { apiURL } from "../../lib/base-path";
import type { SetupModelProvider, SetupStatus } from "../../lib/types";
import { Button } from "../ui/button";
import { INLINE_ACTIONS, STICKY_FOOTER } from "./form-actions";
import { CONNECTIONS } from "./copy/door";
import { OIDC_RETURN, OIDC_SIGN_IN as T, oidcFailureKind, type OidcFailure } from "./copy/oidc-signin";

const POLL_MS = 1500;
const GIVE_UP_MS = 5 * 60 * 1000;
const CLOSE_GRACE_MS = 4000;
const WINDOW_FEATURES = "popup,width=520,height=720";
const WINDOW_NAME = "wardyn-provider-oidc";

type Phase = { name: "connect" } | { name: "waiting" } | { name: "blocked" } | { name: "fail"; failure: OidcFailure };

/** A per-attempt nonce. getRandomValues, since randomUUID exists only in secure contexts and a console can be served over http. */
function attemptNonce(): string {
  return Array.from(crypto.getRandomValues(new Uint8Array(8)), (b) => b.toString(16).padStart(2, "0")).join("");
}

type Access = NonNullable<SetupStatus["provider_access"]>[number];

/** What a sign-in already stored looked like when the door opened. added_at is when the credential was first stored
 *  and a replaced one keeps it, so it only moves for a first sign-in; signed_in_at moves on every sign-in. */
interface Stamp {
  addedAt: string;
  signedInAt: string;
}
const stampOf = (a: Access | undefined): Stamp => ({ addedAt: a?.added_at ?? "", signedInAt: a?.signed_in_at ?? "" });
const isLive = (a: Access | undefined) => a?.state === "live" || a?.state === "expiring";
/** A sign-in made since `from`: live now, with either stamp moved on. */
const signedInSince = (a: Access | undefined, from: Stamp) =>
  isLive(a) && ((a?.added_at ?? "") !== from.addedAt || (a?.signed_in_at ?? "") !== from.signedInAt);

export function OidcDoor({
  provider,
  access,
  onRecheck,
  onCancel,
  onDone,
}: {
  provider: SetupModelProvider;
  access: SetupStatus["provider_access"];
  onRecheck?: () => void | Promise<unknown>;
  onCancel: () => void;
  onDone: () => void;
}) {
  const name = provider.name || provider.id;
  const [phase, setPhase] = React.useState<Phase>({ name: "connect" });
  const accessRef = React.useRef(access);
  accessRef.current = access;
  // The parent's callbacks are new on every render; the wait below must not restart with them.
  const calls = React.useRef({ onRecheck, onDone });
  calls.current = { onRecheck, onDone };
  // The stored sign-in's stamps when this attempt began: "Sign in again" starts from a live state.
  const baseline = React.useRef<Stamp>({ addedAt: "", signedInAt: "" });
  // This attempt's own window said the sign-in succeeded; the door then ends once a re-read shows it live.
  const okWord = React.useRef(false);
  const popup = React.useRef<Window | null>(null);
  // This attempt's own name for its window, carried in the window's name: the return page echoes it.
  const attempt = React.useRef("");
  const finished = React.useRef(false);
  const settleRef = React.useRef<(() => boolean) | null>(null);
  // The sign-in route takes the provider row's uid, which the setup status carries; the admin's slug is not it.
  const url = provider.uid ? apiURL(`/model-providers/${encodeURIComponent(provider.uid)}/oidc/sign-in`) : "";
  const waiting = phase.name === "waiting" || phase.name === "blocked";

  const fail = React.useCallback((failure: OidcFailure) => {
    finished.current = true;
    setPhase({ name: "fail", failure });
  }, []);

  // While waiting: the window's own word (a message from this console's origin), the state it leaves behind, and a
  // window closed with neither. A sign-in the person abandons ends at GIVE_UP_MS.
  React.useEffect(() => {
    if (!waiting) return;
    const started = Date.now();
    let closedAt: number | undefined;
    const onMessage = (e: MessageEvent) => {
      const d = e.data as { type?: string; ok?: boolean; reason?: string; attempt?: string } | null;
      if (e.origin !== window.location.origin || d?.type !== OIDC_RETURN.MESSAGE || finished.current) return;
      // Only this attempt's window speaks for it: an earlier or another window of this origin is ignored. The
      // echoed name is checked too, but only when it came back (a browser may clear a window's name when it
      // crosses sites), since the window itself already says whose word this is.
      if (!popup.current || e.source !== popup.current) return;
      if (d.attempt && d.attempt !== attempt.current) return;
      if (d.ok) {
        okWord.current = true;
        void calls.current.onRecheck?.();
      } else fail(oidcFailureKind(d.reason ?? ""));
    };
    window.addEventListener("message", onMessage);
    // Done once this attempt's own window said so and the status shows a live sign-in, or, with no word (the
    // link a blocked browser gave), once a live sign-in's stamps have moved on from where the attempt began.
    const settled = (): boolean => {
      const a = accessRef.current?.find((x) => x.provider === provider.id);
      if (finished.current || !(okWord.current ? isLive(a) : signedInSince(a, baseline.current))) return false;
      finished.current = true;
      calls.current.onDone();
      return true;
    };
    settleRef.current = settled;
    const tick = window.setInterval(() => {
      if (finished.current) return;
      if (settled()) return;
      // A window closed with no word and no new sign-in is a failure, but only after a few more reads: the sign-in
      // may have landed as it closed, and the status is read again each tick.
      if (popup.current?.closed === true) closedAt ??= Date.now();
      if ((closedAt && Date.now() - closedAt > CLOSE_GRACE_MS) || Date.now() - started > GIVE_UP_MS) {
        fail("timeout");
        return;
      }
      void calls.current.onRecheck?.();
    }, POLL_MS);
    return () => {
      window.removeEventListener("message", onMessage);
      window.clearInterval(tick);
      settleRef.current = null;
    };
  }, [waiting, provider.id, fail]);
  // A re-read of the status ends the wait at once, not at the next tick.
  React.useEffect(() => {
    settleRef.current?.();
  }, [access]);

  // Leaving the door by any road takes its window with it, unless it finished.
  React.useEffect(
    () => () => {
      if (!finished.current) popup.current?.close();
    },
    [],
  );

  const start = () => {
    baseline.current = stampOf(access?.find((x) => x.provider === provider.id));
    okWord.current = false;
    finished.current = false;
    // An earlier attempt's window goes, so its late word has nowhere to land.
    popup.current?.close();
    attempt.current = `${WINDOW_NAME}-${attemptNonce()}`;
    popup.current = window.open(url, attempt.current, WINDOW_FEATURES);
    setPhase(popup.current ? { name: "waiting" } : { name: "blocked" });
  };

  return (
    <div className="space-y-4">
      {!provider.uid ? (
        <p role="alert" className="text-body text-danger">
          {T.NOT_READY}
        </p>
      ) : phase.name === "fail" ? (
        <div role="alert" className="space-y-1 rounded-lg border border-danger/30 bg-danger-subtle p-3 text-body text-danger">
          <b className="font-semibold">{T.FAIL_TITLE(name)}</b>
          <p>{T.FAIL[phase.failure](name)}</p>
        </div>
      ) : phase.name === "connect" ? (
        <p className="text-body">{T.INTRO}</p>
      ) : (
        <div role="status" className="flex items-start gap-2 text-body">
          <Loader2 className="mt-0.5 size-4 shrink-0 animate-spin text-muted-foreground" aria-hidden="true" />
          <div className="space-y-1">
            {phase.name === "blocked" && <p>{T.BLOCKED}</p>}
            {phase.name === "blocked" ? (
              <a href={url} target="_blank" rel="noreferrer" className="text-info underline underline-offset-2">
                {T.BLOCKED_LINK(name)}
              </a>
            ) : (
              <p>{T.WAITING}</p>
            )}
          </div>
        </div>
      )}
      <p className="text-xs text-muted-foreground">{CONNECTIONS.SENT_TO(provider.host)}</p>
      <div data-testid="oidc-door-footer" className={`${INLINE_ACTIONS} ${STICKY_FOOTER}`}>
        <Button type="button" variant="ghost" onClick={onCancel}>
          {T.CANCEL}
        </Button>
        {!waiting && provider.uid && (
          <Button type="button" onClick={start}>
            {phase.name === "fail" ? T.RETRY : T.CONTINUE}
          </Button>
        )}
      </div>
    </div>
  );
}
