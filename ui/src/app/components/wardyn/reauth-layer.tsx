/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// #483 — the "Sign in to continue" dialog and the signed-out bar, drawn over
// a page that stays mounted when its session ends (state: lib/reauth.ts).
// Loaded lazily by the shell: it pulls two lazy-side copy modules, and the
// entry chunk has no room for them (bundle-split.test.ts).
//
// The SSO door is the Azure DevOps connect door's popup (use-ado-connect.ts),
// copied rather than shared: that hook polls /me/scm-access, this one /me.
// about:blank first, the opener severed, then navigated; nothing the popup
// says is trusted — the SERVER is polled until a session is live. Whatever
// request got the 401 is never re-sent from here.
//
// It also draws the renewal strip: the expiry banner's "Sign in again" has
// already opened the window (lib/use-session-renew.ts), and the strip waits
// here, in the banner's place, on a session that is still live — so a live
// /me proves nothing until renewVerdict below has measured it. The page keeps
// working all the while, so once a renewal has begun a sign-in, who answers
// /me goes on being measured after the strip stops waiting for it — a closed
// window, Cancel — for as long as that sign-in could still land.
import * as React from "react";
import { createPortal } from "react-dom";
import { useLocation, useNavigate } from "react-router-dom";
import { AlertTriangle, Building2, Copy, KeyRound, Loader2 } from "lucide-react";
import { toast } from "sonner";
import { Button } from "../ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "../ui/dialog";
import { Input } from "../ui/input";
import { Label } from "../ui/label";
import { HttpError, setToken, wfetch } from "../../lib/api/core";
import { health, type Me } from "../../lib/api/health";
import { shortTime } from "../../lib/format";
import { useReauth, type Renewal } from "../../lib/reauth";
import { REAUTH_BAR, REAUTH_DIALOG, REAUTH_EXTRA, REAUTH_RENEW } from "../../lib/reauth-copy";
import { openSignInWindow, RENEW_STRIP_SLOT } from "../../lib/use-session-renew";
import { unsavedSnapshot } from "../../lib/unsaved-registry";
import { useCopyToClipboard } from "../../lib/use-copy-to-clipboard";
import { PROVIDERS_EXTRA } from "../../lib/workspace-providers-copy";
import { SIGNIN } from "../../lib/sign-in-copy";
import { SSO_SIGN_IN, TOKEN_LABEL } from "../screens/sign-in";
import { MODEL_ACCESS_BANNER } from "./model-access-copy";
import { useOperatorResolved, usePrincipal } from "./operator-context";
import { appURL } from "../../lib/base-path";

// A function, not a constant: the base path is read when the link is used.
const ssoLoginURL = () => appURL("/auth/login");
const POLL_MS = 1500;
// The fallback link opens a tab this page holds no handle on, so nothing
// says when it closes — the poll it starts is bounded instead.
const FALLBACK_POLL_TIMEOUT_MS = 5 * 60 * 1000;
// How long a sign-in this page began can still end in a session, counted from
// when the page stopped waiting for it. The server honours a started login
// for ten minutes (internal/auth/oidc/cookies.go, loginCookie's MaxAge) and
// restarts one, once, for ten more when the provider refuses its extra scopes
// (login_grant.go, retryLoginUnwidened); the last minute is the callback's
// own work. A closed window is no proof either way: the callback may already
// be with the server, and a window can read closed while it is still open.
const LOGIN_REDEEMABLE_MS = (2 * 10 + 1) * 60 * 1000;
const whileRedeemable = () => Date.now() + LOGIN_REDEEMABLE_MS;

// The sign-in in a new tab, opened from the click so this page holds a handle
// Cancel can close; null when the browser refuses that as well. Severed like
// the window (lib/use-session-renew.ts).
function openSignInTab(): Window | null {
  const tab = window.open("about:blank", "_blank");
  if (!tab) return null;
  tab.opener = null;
  tab.location.href = ssoLoginURL();
  return tab;
}

/** The visible part of a poll: until over() the status line follows it, then
 *  onOver() says how it ended. onLive runs before a live session is acted on. */
interface Wait {
  over: () => boolean;
  onOver: () => void;
  onLive?: () => void;
}

// Can THIS role open `path`? Mirrors App.tsx's <Route> tiers: a user's
// reachable surface is wider than their nav (/secrets and /account have no
// sidebar entry but are theirs), nothing under /admin is theirs, and
// /admin/providers is for an actual admin only — a security admin manages
// Drives, so /admin/drives stays theirs. Not a route guard — the server is the
// gate — only the answer to "does this page still belong to who signed in".
// M-5 (#636) deleted /ssh-keys with no alias — SshKeysPane is mounted once
// now, in /account, which already covers it.
const MEMBER_REACHABLE_PREFIXES = ["/runs", "/approvals", "/workspaces", "/secrets", "/account"];
const OPERATOR_ONLY_PREFIXES = ["/admin/providers"];
export function roleCanReach(path: string, role: string): boolean {
  const under = (prefixes: string[]) => prefixes.some((p) => path === p || path.startsWith(`${p}/`));
  if (role === "user") return under(MEMBER_REACHABLE_PREFIXES);
  if (under(OPERATOR_ONLY_PREFIXES)) return role === "admin";
  return true;
}

type Session = Me | "unauthed" | "unreachable";

// One GET /me answers both "is there a session" and "whose": a 401 is signed
// out, anything else that is not a body is an outage — never a sign-out.
async function readSession(): Promise<Session> {
  try {
    const res = await wfetch("/me", { method: "GET" });
    if (res.ok) return (await res.json()) as Me;
    await res.text().catch(() => "");
    return "unreachable";
  } catch (e) {
    return e instanceof HttpError && e.status === 401 ? "unauthed" : "unreachable";
  }
}

type Status = "idle" | "waiting" | "blocked" | "closed" | "unreachable" | "rejected";

// A live /me while a renewal waits. Identity and authority come FIRST, on
// every answer: someone else's sign-in, or this person's with other authority,
// is live in this browser whatever its expiry says (a replacement session's
// can be equal or earlier), so it is "other" and goes straight to succeed().
// Only for the same person with the same authority does the expiry decide,
// and one that is not later is the old session answering: keep waiting.
function renewVerdict(from: Renewal, me: Me, principalResolved: boolean): "other" | "waiting" | "renewed" {
  if (
    !principalResolved ||
    me.principal !== from.principal ||
    me.role !== from.role ||
    me.operator !== from.operator ||
    me.security_operator !== from.securityOperator
  ) {
    return "other";
  }
  const until = Date.parse(me.session_expires_at ?? "");
  if (Number.isNaN(until)) return "waiting";
  return until > from.expiresAt ? "renewed" : "waiting";
}

export function ReauthLayer({ onResumed }: { onResumed: (me: Me) => void }) {
  const reauth = useReauth();
  const principal = usePrincipal();
  // SF-29: whether `principal` is a settled fact rather than app-shell's
  // still-loading "…" or its own fail-open "unknown" (health.ts's whoami()
  // returns null, so identityResolved/operatorResolved stays false, for both
  // cases — see OperatorResolvedContext's own R4-F110 precedent for this same
  // class of bug). An unsettled principal cannot prove who signs back in is
  // the same person, so it counts as someone else (fail closed, below).
  const principalResolved = useOperatorResolved();
  const location = useLocation();
  const navigate = useNavigate();
  const [status, setStatus] = React.useState<Status>("idle");
  // Same person, narrower role: this page is no longer theirs.
  const [narrowed, setNarrowed] = React.useState<Me | null>(null);
  const [token, setTokenValue] = React.useState("");
  const [busy, setBusy] = React.useState(false);
  // Same defaults and rule as sign-in.tsx: the token form until the daemon
  // says otherwise, SSO only once it says so.
  const [doors, setDoors] = React.useState({ sso: false, token: true });
  const pollRef = React.useRef<number | null>(null);
  const cancelRef = React.useRef<HTMLButtonElement>(null);
  const { renewal, watch, setWatch } = reauth;
  // Whether a renewal has begun a sign-in at all — a window or a tab that
  // opened — since this layer mounted, and it stays mounted for as long as one
  // is watched. A renewal whose window was refused, and nothing else, has
  // nothing that can land.
  const began = React.useRef(false);
  const { copied, copy } = useCopyToClipboard();

  React.useEffect(() => {
    if (copied) toast.success(PROVIDERS_EXTRA.CONFLICT_COPIED_TOAST);
  }, [copied]);

  React.useEffect(() => {
    let alive = true;
    void health.health().then((h) => {
      if (!alive || Object.keys(h).length === 0) return;
      setDoors({ sso: !!h.sso, token: h.token_login !== false || !h.sso });
    });
    return () => {
      alive = false;
    };
  }, []);

  const stopPoll = React.useCallback(() => {
    if (pollRef.current !== null) window.clearInterval(pollRef.current);
    pollRef.current = null;
  }, []);
  React.useEffect(() => stopPoll, [stopPoll]);

  // Both read through a ref: the poll that calls them was started renders ago.
  // Outside a renewal nobody was signed in, so any live /me is the answer —
  // except under a watch, whose renewal the person backed out of: the same
  // person with the same authority changes nothing there, whatever the expiry.
  const watching = watch !== null && reauth.phase === "none";
  const verdict = React.useRef((_me: Me): ReturnType<typeof renewVerdict> => "other");
  verdict.current = (me: Me) => {
    if (renewal) return renewVerdict(renewal, me, principalResolved);
    return watching && renewVerdict(watch.from, me, principalResolved) !== "other" ? "waiting" : "other";
  };
  // The page carries on as `me`: a watch measures against them from here on.
  const carryOn = (me: Me) => {
    onResumed(me);
    if (!watch) return;
    const { principal, role, operator, security_operator: securityOperator } = me;
    setWatch({ ...watch, from: { ...watch.from, principal, role, operator, securityOperator } });
  };
  const succeed = React.useRef((_me: Me) => {});
  succeed.current = (me: Me) => {
    stopPoll();
    // Owner ruling (Q457-12): someone else signed in. Nothing of the first
    // person's page is shown, saved or submitted as them — it loads fresh.
    // Sameness needs a settled first identity: with none (/me never answered
    // before the lapse), whoever signs in is treated as someone else — the
    // same person losing a draft in that rare case is the accepted cost.
    if (!principalResolved || me.principal !== principal) {
      reauth.reloadAs(roleCanReach(location.pathname, me.role) ? location.pathname + location.search : "/runs");
      return;
    }
    if (!roleCanReach(location.pathname, me.role)) {
      setNarrowed(me);
      // A renewal or a watch has no dialog up yet, and the dialog is what says so.
      if (reauth.phase !== "dialog") reauth.setPhase("dialog");
      return;
    }
    // A save refused in the lapse is said beside that screen's own Save when
    // it offers to (useWriteDropped); anywhere else, here.
    if (reauth.writeDropped && !reauth.writeDroppedClaimed()) {
      toast.warning(REAUTH_DIALOG.WRITE_DROPPED);
      reauth.clearWriteDropped();
    }
    if (renewal) {
      if (verdict.current(me) === "renewed") {
        toast.success(REAUTH_RENEW.RENEWED(shortTime(me.session_expires_at ?? "")));
      }
      // The strip goes, and Cancel with it: focus lands on the page, not the document.
      document.getElementById("main-content")?.focus();
    }
    carryOn(me);
    reauth.setPhase("none");
  };

  // One poll at a time. With a `wait` it drives the status line until the
  // wait is over; with `quietUntil` it then carries on — or, with no wait,
  // starts — in silence until that moment: who answers /me is still measured,
  // and only the status is left alone. A held page needs no quiet part.
  const startPoll = React.useCallback(
    (wait: Wait | null, quietUntil: (() => number) | null, onEnd?: () => void) => {
      stopPoll();
      if (wait) setStatus("waiting");
      let inFlight = false;
      let quiet = wait ? 0 : (quietUntil?.() ?? 0);
      const tick = () => {
        if (inFlight) return;
        if (quiet && Date.now() > quiet) {
          stopPoll();
          onEnd?.();
          return;
        }
        inFlight = true;
        // Asked BEFORE the read, so a window closed the instant sign-in
        // finished still gets one last look at the server.
        const over = !quiet && !!wait?.over();
        void readSession().then((s) => {
          inFlight = false;
          if (pollRef.current !== id) return;
          if (typeof s === "object" && verdict.current(s) !== "waiting") {
            wait?.onLive?.();
            succeed.current(s);
          } else if (over) {
            wait?.onOver();
            if (quietUntil) quiet = quietUntil();
            else stopPoll();
          } else if (!quiet) {
            setStatus(s === "unreachable" ? "unreachable" : "waiting");
          }
        });
      };
      const id = window.setInterval(tick, POLL_MS);
      pollRef.current = id;
      // Nothing to wait for: the first look is now, not a poll from now.
      if (!wait) tick();
    },
    [stopPoll],
  );

  // A renewal begins with its window already open (or refused): only the wait
  // starts here. However it ends — renewed, someone else, Cancel, a new
  // attempt — the window and the wait end with it.
  React.useEffect(() => {
    if (!renewal) return;
    const { popup } = renewal;
    if (popup) {
      began.current = true;
      startPoll({ over: () => popup.closed, onOver: () => setStatus("closed") }, whileRedeemable);
    } else {
      setStatus("blocked");
      // A window opened earlier, by this renewal or one cancelled before it,
      // may still finish its sign-in.
      if (began.current) startPoll(null, whileRedeemable);
    }
    cancelRef.current?.focus();
    return () => {
      stopPoll();
      popup?.close();
    };
  }, [renewal, startPoll, stopPoll]);

  // A watch: the renewal was cancelled, the page works, and nothing shows.
  // Paused while the page is held — nothing is written then, and whoever
  // signs in is measured before it carries on — and over at its bound.
  React.useEffect(() => {
    if (!watch || !watching) return;
    startPoll(
      null,
      () => watch.until,
      () => setWatch(null),
    );
    return stopPoll;
  }, [watch, watching, setWatch, startPoll, stopPoll]);

  // A watch keeps the layer mounted from one lapse to the next: each starts clean.
  React.useEffect(() => {
    if (reauth.phase !== "none") return;
    setStatus("idle");
    setNarrowed(null);
    setTokenValue("");
  }, [reauth.phase]);

  // Cancel closes the window (the renewal's effect), and that proves nothing:
  // a sign-in it began can still land, so who answers /me is watched from here.
  const cancelRenew = () => {
    setStatus("idle");
    if (renewal && began.current) setWatch({ from: renewal, until: whileRedeemable() });
    reauth.endRenew();
  };

  const signInWithSso = () => {
    const popup = openSignInWindow();
    if (!popup) {
      stopPoll();
      setStatus("blocked");
      return;
    }
    startPoll({ over: () => popup.closed, onOver: () => setStatus("closed"), onLive: () => popup.close() }, null);
  };

  // The link's own tab, which this page holds no handle on. Under a renewal
  // the page is working, so the poll goes quiet at its bound instead of ending.
  const signInInTab = () => {
    const deadline = Date.now() + FALLBACK_POLL_TIMEOUT_MS;
    startPoll(
      { over: () => Date.now() > deadline, onOver: () => setStatus("blocked") },
      renewal ? whileRedeemable : null,
    );
  };

  // The strip's link. A tab opened from the click carries on as the renewal's
  // window, so Cancel closes it; refused, the link opens its own.
  const renewInTab = (e: React.MouseEvent) => {
    if (!renewal) return;
    const tab = openSignInTab();
    if (tab) {
      e.preventDefault();
      reauth.startRenew({ ...renewal, popup: tab });
    } else {
      began.current = true;
      signInInTab();
    }
  };

  const submitToken = async () => {
    stopPoll();
    setBusy(true);
    setStatus("idle");
    setToken(token);
    const s = await readSession();
    setBusy(false);
    if (s === "unauthed") setStatus("rejected");
    else if (s === "unreachable") setStatus("unreachable");
    else succeed.current(s);
  };

  const notNow = () => {
    stopPoll();
    setStatus("idle");
    setTokenValue("");
    reauth.setPhase("bar");
  };

  const goToRuns = () => {
    if (narrowed) carryOn(narrowed);
    reauth.clearWriteDropped();
    reauth.setPhase("none");
    void navigate("/runs", { replace: true });
  };

  const copyButton = unsavedSnapshot() !== null && (
    <Button type="button" variant="outline" size="sm" onClick={() => copy(unsavedSnapshot() ?? "")}>
      <Copy className="size-3.5" /> {PROVIDERS_EXTRA.CONFLICT_COPY}
    </Button>
  );

  const note: Partial<Record<Status, [string, string]>> = {
    waiting: [REAUTH_DIALOG.WAITING, "text-muted-foreground"],
    closed: [REAUTH_DIALOG.CLOSED_WITHOUT, "text-warning"],
    unreachable: [REAUTH_DIALOG.UNREACHABLE, "text-warning"],
    rejected: [SIGNIN.TOKEN_REJECTED, "text-danger"],
  };
  const line = note[status];

  // role="status" for the states that follow; the first is heard through
  // Cancel, which takes focus and is described by the status text.
  const strip = renewal && (
    <div
      role="status"
      className="relative z-50 flex shrink-0 flex-wrap items-center gap-2 border-b border-border bg-warning-subtle px-4 py-2 text-sm text-warning"
      onKeyDown={(e) => {
        if (e.key !== "Escape") return;
        // Prevented, so a page's own Escape (New Run leaves on it) stays put.
        e.preventDefault();
        cancelRenew();
      }}
    >
      <AlertTriangle className="size-4 shrink-0" />
      {(status === "waiting" || status === "unreachable") && <Loader2 className="size-4 shrink-0 animate-spin" />}
      <span id="reauth-renew-status">
        {status === "blocked"
          ? REAUTH_DIALOG.POPUP_BLOCKED
          : status === "closed"
            ? REAUTH_DIALOG.CLOSED_WITHOUT
            : status === "unreachable"
              ? REAUTH_DIALOG.UNREACHABLE
              : REAUTH_DIALOG.WAITING}
      </span>
      {/* First in the tab order, last on the line. */}
      <Button
        ref={cancelRef}
        type="button"
        variant="ghost"
        size="sm"
        className="order-last ml-auto"
        aria-describedby="reauth-renew-status"
        onClick={cancelRenew}
      >
        {REAUTH_RENEW.CANCEL}
      </Button>
      {status === "blocked" && (
        <a
          href={ssoLoginURL()}
          target="_blank"
          rel="noopener noreferrer"
          className="font-medium text-info hover:underline"
          onClick={renewInTab}
        >
          {REAUTH_DIALOG.POPUP_FALLBACK}
        </a>
      )}
      {status === "closed" && (
        <button
          type="button"
          className="font-medium underline underline-offset-2"
          onClick={() => reauth.startRenew({ ...renewal, popup: openSignInWindow() })}
        >
          {REAUTH_RENEW.CTA}
        </button>
      )}
    </div>
  );
  const stripSlot = document.getElementById(RENEW_STRIP_SLOT);

  return (
    <>
      {strip && (stripSlot ? createPortal(strip, stripSlot) : strip)}
      {reauth.phase === "bar" && (
        <div
          role="status"
          className="relative z-50 flex shrink-0 flex-wrap items-center gap-2 border-b border-border bg-warning-subtle px-4 py-2 text-sm text-warning"
        >
          <AlertTriangle className="size-4 shrink-0" />
          <span>{REAUTH_BAR.BODY}</span>
          <div className="ml-auto flex gap-2">
            {copyButton}
            <Button type="button" size="sm" onClick={() => reauth.setPhase("dialog")}>
              {REAUTH_BAR.CTA}
            </Button>
          </div>
        </div>
      )}
      <Dialog open={reauth.phase === "dialog"} onOpenChange={(open) => !open && (narrowed ? goToRuns() : notNow())}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>{REAUTH_DIALOG.TITLE}</DialogTitle>
            <DialogDescription>{narrowed ? REAUTH_DIALOG.ROLE_CHANGED_BODY : REAUTH_DIALOG.BODY}</DialogDescription>
          </DialogHeader>
          {narrowed ? (
            <DialogFooter>
              {copyButton}
              <Button type="button" onClick={goToRuns}>
                {REAUTH_EXTRA.GO_TO_RUNS}
              </Button>
            </DialogFooter>
          ) : (
            <>
              {/* Same order as sign-in.tsx: token form (its submit the one
                  primary), OR, then SSO. */}
              {doors.token && (
                <form
                  onSubmit={(e) => {
                    e.preventDefault();
                    if (token) void submitToken();
                  }}
                  className="space-y-2"
                >
                  <Label htmlFor="reauth-token" className="text-foreground">
                    {TOKEN_LABEL}
                  </Label>
                  <div className="relative">
                    <KeyRound className="absolute left-3 top-1/2 size-4 -translate-y-1/2 text-muted-foreground" />
                    <Input
                      id="reauth-token"
                      type="password"
                      value={token}
                      onChange={(e) => setTokenValue(e.target.value)}
                      className="pl-9 font-mono"
                      autoComplete="off"
                    />
                  </div>
                  <p className="text-xs text-muted-foreground">{SIGNIN.TOKEN_HINT}</p>
                  <Button type="submit" className="w-full" disabled={!token || busy}>
                    {busy ? <Loader2 className="size-4 animate-spin" /> : REAUTH_BAR.CTA}
                  </Button>
                </form>
              )}
              {doors.token && doors.sso && (
                <div className="flex items-center gap-3">
                  <div className="h-px flex-1 bg-border" />
                  <span className="text-xs uppercase tracking-wide text-muted-foreground">or</span>
                  <div className="h-px flex-1 bg-border" />
                </div>
              )}
              {doors.sso && (
                <Button
                  type="button"
                  variant={doors.token ? "outline" : "default"}
                  className="w-full"
                  onClick={signInWithSso}
                >
                  <Building2 className="size-4" />
                  {SSO_SIGN_IN}
                </Button>
              )}
              {status === "blocked" && (
                <p role="status" className="text-xs text-warning">
                  {REAUTH_DIALOG.POPUP_BLOCKED}{" "}
                  <a
                    href={ssoLoginURL()}
                    target="_blank"
                    rel="noopener noreferrer"
                    className="font-medium text-info hover:underline"
                    onClick={signInInTab}
                  >
                    {REAUTH_DIALOG.POPUP_FALLBACK}
                  </a>
                </p>
              )}
              {line && (
                <p role="status" className={`flex items-center gap-2 text-xs ${line[1]}`}>
                  {status === "waiting" && <Loader2 className="size-3.5 animate-spin" />}
                  {line[0]}
                </p>
              )}
              <DialogFooter>
                <Button type="button" variant="ghost" onClick={notNow}>
                  {MODEL_ACCESS_BANNER.NOT_NOW}
                </Button>
              </DialogFooter>
            </>
          )}
        </DialogContent>
      </Dialog>
    </>
  );
}
