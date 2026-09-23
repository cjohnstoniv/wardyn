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
import * as React from "react";
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
import { useReauth } from "../../lib/reauth";
import { REAUTH_BAR, REAUTH_DIALOG, REAUTH_DRAFT } from "../../lib/reauth-copy";
import { unsavedSnapshot } from "../../lib/unsaved-registry";
import { useCopyToClipboard } from "../../lib/use-copy-to-clipboard";
import { PROVIDERS_DRAFT } from "../../lib/workspace-providers-copy";
import { SSO_SIGN_IN, TOKEN_HINT, TOKEN_LABEL, TOKEN_REJECTED } from "../screens/sign-in";
import { MODEL_ACCESS_BANNER } from "./model-access-copy";
import { useOperatorResolved, usePrincipal } from "./operator-context";

const SSO_LOGIN_URL = "/auth/login";
const POLL_MS = 1500;
// The fallback link opens a tab this page holds no handle on, so nothing
// says when it closes — the poll it starts is bounded instead.
const FALLBACK_POLL_TIMEOUT_MS = 5 * 60 * 1000;

// Can THIS role open `path`? Mirrors App.tsx's <Route> tiers: a member's
// reachable surface is wider than their nav (/secrets, /settings and
// /ssh-keys have no sidebar entry but are theirs), and /drives + /providers
// are admin-only for everyone else. Not a route guard — the server is the
// gate — only the answer to "does this page still belong to who signed in".
const MEMBER_REACHABLE_PREFIXES = ["/runs", "/approvals", "/workspaces", "/secrets", "/settings", "/ssh-keys"];
const OPERATOR_ONLY_PREFIXES = ["/drives", "/providers"];
export function roleCanReach(path: string, role: string): boolean {
  const under = (prefixes: string[]) => prefixes.some((p) => path === p || path.startsWith(`${p}/`));
  if (role === "member") return under(MEMBER_REACHABLE_PREFIXES);
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

export function ReauthLayer({ onResumed }: { onResumed: (me: Me) => void }) {
  const reauth = useReauth();
  const principal = usePrincipal();
  // SF-29: whether `principal` is a settled fact rather than app-shell's
  // still-loading "…" or its own fail-open "unknown" (health.ts's whoami()
  // returns null, so identityResolved/operatorResolved stays false, for both
  // cases — see OperatorResolvedContext's own R4-F110 precedent for this same
  // class of bug). Comparing against either placeholder always differs from a
  // real signed-in principal, so every re-sign-in read as "someone else".
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
  const { copied, copy } = useCopyToClipboard();

  React.useEffect(() => {
    if (copied) toast.success(PROVIDERS_DRAFT.CONFLICT_COPIED_TOAST);
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

  // Read through a ref: the poll that calls it was started renders ago.
  const succeed = React.useRef((_me: Me) => {});
  succeed.current = (me: Me) => {
    stopPoll();
    // Owner ruling (Q457-12): someone else signed in. Nothing of the first
    // person's page is shown, saved or submitted as them — it loads fresh.
    // SF-29: only once `principal` is a settled fact — an unresolved identity
    // (principalResolved false) makes every re-sign-in look like a stranger,
    // reloading the page and losing the draft this dialog just promised
    // nothing here had lost.
    if (principalResolved && me.principal !== principal) {
      reauth.reloadAs(roleCanReach(location.pathname, me.role) ? location.pathname + location.search : "/runs");
      return;
    }
    if (!roleCanReach(location.pathname, me.role)) {
      setNarrowed(me);
      return;
    }
    // A save refused in the lapse is said beside that screen's own Save when
    // it offers to (useWriteDropped); anywhere else, here.
    if (reauth.writeDropped && !reauth.writeDroppedClaimed()) {
      toast.warning(REAUTH_DIALOG.WRITE_DROPPED);
      reauth.clearWriteDropped();
    }
    onResumed(me);
    reauth.setPhase("none");
  };

  const startPoll = (giveUp: () => boolean, onGiveUp: () => void, onLive?: () => void) => {
    stopPoll();
    setStatus("waiting");
    let inFlight = false;
    const id = window.setInterval(() => {
      if (inFlight) return;
      inFlight = true;
      // Asked BEFORE the read, so a window closed the instant sign-in
      // finished still gets one last look at the server.
      const gaveUp = giveUp();
      void readSession().then((s) => {
        inFlight = false;
        if (pollRef.current !== id) return;
        if (typeof s === "object") {
          onLive?.();
          succeed.current(s);
        } else if (gaveUp) {
          stopPoll();
          onGiveUp();
        } else {
          setStatus(s === "unreachable" ? "unreachable" : "waiting");
        }
      });
    }, POLL_MS);
    pollRef.current = id;
  };

  const signInWithSso = () => {
    const popup = window.open("about:blank", "wardyn-reauth", "width=520,height=680");
    if (!popup) {
      stopPoll();
      setStatus("blocked");
      return;
    }
    popup.opener = null;
    popup.location.href = SSO_LOGIN_URL;
    startPoll(
      () => popup.closed,
      () => setStatus("closed"),
      () => popup.close(),
    );
  };

  const signInInTab = () => {
    const deadline = Date.now() + FALLBACK_POLL_TIMEOUT_MS;
    startPoll(
      () => Date.now() > deadline,
      () => setStatus("blocked"),
    );
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
    if (narrowed) onResumed(narrowed);
    reauth.clearWriteDropped();
    reauth.setPhase("none");
    navigate("/runs", { replace: true });
  };

  const copyButton = unsavedSnapshot() !== null && (
    <Button type="button" variant="outline" size="sm" onClick={() => copy(unsavedSnapshot() ?? "")}>
      <Copy className="size-3.5" /> {PROVIDERS_DRAFT.CONFLICT_COPY}
    </Button>
  );

  const note: Partial<Record<Status, [string, string]>> = {
    waiting: [REAUTH_DIALOG.WAITING, "text-muted-foreground"],
    closed: [REAUTH_DIALOG.CLOSED_WITHOUT, "text-warning"],
    unreachable: [REAUTH_DIALOG.UNREACHABLE, "text-warning"],
    rejected: [TOKEN_REJECTED, "text-danger"],
  };
  const line = note[status];

  return (
    <>
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
                {REAUTH_DRAFT.GO_TO_RUNS}
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
                  <p className="text-xs text-muted-foreground">{TOKEN_HINT}</p>
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
                    href={SSO_LOGIN_URL}
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
