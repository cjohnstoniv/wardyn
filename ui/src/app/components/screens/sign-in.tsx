/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import {
  AlertCircle,
  ArrowRight,
  Building2,
  KeyRound,
  Loader2,
  Moon,
  ShieldCheck,
  Sun,
} from "lucide-react";
import { Button } from "../ui/button";
import { Checkbox } from "../ui/checkbox";
import { Input } from "../ui/input";
import { Label } from "../ui/label";
import { useTheme } from "../wardyn/theme-provider";
import {
  errText,
  HttpError,
  setToken,
  wfetch,
  withLimit,
} from "../../lib/api/core";
import { health } from "../../lib/api/health";
import { SIGNIN } from "../../lib/sign-in-copy";
import { SIGNIN_HELP_REFUSALS } from "../../lib/people-access-copy";
import { SignInHelp } from "../wardyn/sign-in-help";
import { usePoll } from "../../lib/use-poll";

// How often the gate re-asks /healthz for `sso` (R4/F027). Slower than the
// shell's 5s health poll: nothing here is live data, this only has to notice a
// daemon that came up after the gate rendered.
const SSO_POLL_MS = 10000;

// #457: how many unanswered reads before the checking state says so out
// loud (Q457-2) — see the `checking`/`unansweredReads` state below.
const STILL_CHECKING_AFTER_READS = 3;

// The OIDC callback (internal/auth/oidc/oidc.go's CallbackHandler)
// redirects a user-actionable login denial to "/?auth_error=<code>" instead
// of dead-ending the browser on a bare http.Error text page — but a redirect
// nobody reads is no better: it silently bounces back to this exact screen
// with zero explanation. These are the stable, machine-readable codes
// redirectAuthError sends; keep in sync with oidc.go's authError* consts.
//
// #457 (docs/design/signin-first-contact-canon.md) reworded every arm below
// except email_unverified: no env var names, no "operator" — "your Wardyn
// admin" throughout, since a locked-out reader cannot reach a chart value.
// claims_overage is NOT a "try again": retrying resends the identical token.
function authErrorMessage(code: string): string {
  switch (code) {
    case "email_unverified":
      return "Your identity provider reports this email as unverified. Verify your email with your identity provider, then try again.";
    case "email_verified_absent":
      return SIGNIN.EMAIL_VERIFIED_ABSENT;
    case "email_domain":
      return SIGNIN.EMAIL_DOMAIN;
    case "no_role":
      return SIGNIN.NO_ROLE;
    case "role_check_unavailable":
      return SIGNIN.ROLE_CHECK_UNAVAILABLE;
    case "user_type_ambiguous":
      return SIGNIN.USER_TYPE_AMBIGUOUS;
    case "user_type_unknown":
      return SIGNIN.USER_TYPE_UNKNOWN;
    case "claims_overage":
      return SIGNIN.CLAIMS_OVERAGE;
    case "oidc_transient":
      return SIGNIN.OIDC_TRANSIENT;
    case "oidc_config":
      return SIGNIN.OIDC_CONFIG;
    default:
      return SIGNIN.AUTH_FAILED;
  }
}

export function SignIn({
  onSignIn,
  // X3-F7: why the gate reopened — App.tsx's onUnauthorized handler, for a
  // mid-session expiry (a revoked token, a dead SSO session). Undefined on
  // the ordinary mount-probe gate (never signed in this tab at all), which is
  // why this is the INITIAL error state, not a separate alert slot: the same
  // box submitToken's own failures render below.
  reason,
}: {
  onSignIn: () => void;
  reason?: string;
}) {
  const { theme, toggle } = useTheme();
  const [token, setTokenValue] = React.useState("");
  // Off by default: the token lives in sessionStorage (gone when the browser
  // closes). Opt in to persist it to localStorage across restarts.
  const [remember, setRemember] = React.useState(false);
  const [loading, setLoading] = React.useState<"token" | null>(null);
  const [error, setError] = React.useState<string | null>(reason ?? null);
  // Whether this control plane has OIDC configured (so GET /auth/login exists).
  // Defaults false: without the flow mounted the link would 404, and an older
  // server simply omits the field.
  const [sso, setSso] = React.useState(false);
  // #378/#379: whether the admin-token form can work at all. Defaults true
  // (today's behaviour: the form renders until the daemon says otherwise) —
  // an older daemon that omits the field must read exactly as it does today.
  const [tokenLogin, setTokenLogin] = React.useState(true);
  // #484 — the admin-written help (public, from /healthz) and the auth_error
  // code it is keyed on: shown only under the four refusals in
  // SIGNIN_HELP_REFUSALS, beneath Wardyn's own sentence, never instead of it.
  const [help, setHelp] = React.useState<{ text?: string; url?: string }>({});
  const [refusalCode, setRefusalCode] = React.useState<string | null>(null);
  // #457: whether /healthz has EVER answered (as opposed to the {} R4/F027
  // already treats as "no answer" — see refreshSso below). Before the first
  // real answer neither door is known, so neither renders (Q457-1) — a
  // token field or an SSO button would be a claim about what's configured,
  // and none has been earned yet. unansweredReads only matters while this is
  // false; capped rather than left to grow forever on a daemon that never
  // answers.
  const [answered, setAnswered] = React.useState(false);
  const [unansweredReads, setUnansweredReads] = React.useState(0);
  const checking = !answered;
  const stillChecking = checking && unansweredReads >= STILL_CHECKING_AFTER_READS;

  // R4/F027: health() resolves `{}` on a network error or ANY non-2xx, so a
  // single mount fetch against a daemon that is merely starting (or briefly
  // 5xx-ing) read as `sso: false` and left the SSO button unrendered — on an
  // SSO-only deployment that is a bare token field and no way in, forever,
  // because nothing ever asked again. So: only believe an answer that actually
  // came back, leave the last known value alone otherwise, and keep asking.
  const refreshSso = React.useCallback(() => {
    // Still fetched for `sso` (plus token_login, #378/#379): together they
    // decide what the screen offers. trust_domain / identity_provider are
    // deliberately NOT read here any more.
    void health.health().then((h) => {
      // health() resolves the EMPTY object for "no answer" (health.ts:237-243:
      // `if (!res.ok) return {}` / `catch { return {} }`), and a real /healthz
      // body is never empty — it always carries at least status and version. So
      // an empty object is the outage, and the last known answer survives it.
      if (Object.keys(h).length === 0) {
        setUnansweredReads((n) => Math.min(n + 1, STILL_CHECKING_AFTER_READS));
        return;
      }
      setAnswered(true);
      setSso(!!h.sso);
      // token_login is undefined on an older daemon — read that as `true`, not
      // `false`: the form is that daemon's only path in, and dropping it on a
      // missing field would be a regression, not a safer default.
      setTokenLogin(h.token_login !== false);
      setHelp({ text: h.sign_in_help_text, url: h.sign_in_help_url });
    });
  }, []);
  React.useEffect(refreshSso, [refreshSso]);
  usePoll(refreshSso, SSO_POLL_MS, false);

  // Render the OIDC callback's ?auth_error=<code> (see
  // authErrorMessage above) inline, reusing the same alert box submitToken's
  // own failures render below — a redirect back to this screen with no
  // explanation is the same dead end the bare http.Error page used to be.
  // Runs once on mount; strips the param from the URL bar so a refresh (or
  // the user navigating away and back) doesn't keep re-showing a stale error.
  React.useEffect(() => {
    const params = new URLSearchParams(window.location.search);
    const code = params.get("auth_error");
    if (!code) return;
    setError(authErrorMessage(code));
    setRefusalCode(code);
    params.delete("auth_error");
    const qs = params.toString();
    window.history.replaceState(
      {},
      "",
      window.location.pathname + (qs ? `?${qs}` : ""),
    );
  }, []);

  // probeAuth collapsed every failure — a rejected token (401), a
  // daemon 5xx, and an unreachable control plane (network error) — to the
  // same boolean `false`, so every one of them rendered "That admin token
  // was rejected", even when the token was fine and the daemon just wasn't
  // up yet. Call wfetch directly so the three cases can be told apart, and
  // only clear the stored token on a REAL 401 (a 5xx/network blip shouldn't
  // discard a token that may be perfectly valid).
  const submitToken = async () => {
    if (!token) return;
    setLoading("token");
    setError(null);
    setRefusalCode(null);
    // Store the admin token (sessionStorage, or localStorage when "remember" is
    // checked), then verify it against a protected endpoint.
    setToken(token, remember);
    try {
      const res = await wfetch(withLimit("/runs", 1), { method: "GET" });
      if (res.ok) {
        onSignIn();
        return;
      }
      setError(await errText(res));
    } catch (e) {
      if (e instanceof HttpError && e.status === 401) {
        setToken(null); // a real rejection — don't keep carrying a bad token
        setError(SIGNIN.TOKEN_REJECTED);
      } else {
        setError(SIGNIN.UNREACHABLE_ERROR);
      }
    } finally {
      setLoading(null);
    }
  };

  // #378/#379: show the admin-token form when the daemon says a token can
  // actually work (tokenLogin), OR when neither login path is known to work
  // at all (!sso && !tokenLogin) — a local-mode desktop reaches this screen
  // reporting both false when a request is refused for a non-loopback peer or
  // host, and "no sign-in is configured" would be the wrong advice there.
  const showTokenForm = tokenLogin || !sso;
  // #457: the OR divider (and the SSO door itself) only makes sense once
  // checking is done — see the `checking` guard on both below.
  const showSso = !checking && sso;
  const showDivider = !checking && showTokenForm && showSso;

  return (
    <div className="relative flex min-h-screen items-center justify-center overflow-hidden bg-background px-4">
      {/* ambient backdrop — subtle radial teal glow at the top */}
      <div
        className="pointer-events-none absolute inset-0 opacity-60"
        style={{
          backgroundImage:
            "radial-gradient(60% 50% at 50% 0%, color-mix(in oklab, var(--primary) 16%, transparent), transparent 70%)",
        }}
      />
      <div
        className="pointer-events-none absolute inset-0 opacity-[0.35]"
        style={{
          backgroundImage:
            "linear-gradient(var(--border) 1px, transparent 1px), linear-gradient(90deg, var(--border) 1px, transparent 1px)",
          backgroundSize: "44px 44px",
          maskImage: "radial-gradient(60% 60% at 50% 40%, black, transparent)",
        }}
      />

      <Button
        variant="ghost"
        size="icon"
        onClick={toggle}
        aria-label="Toggle theme"
        className="absolute right-4 top-4"
      >
        {theme === "dark" ? (
          <Sun className="size-4" />
        ) : (
          <Moon className="size-4" />
        )}
      </Button>

      <div className="relative w-full max-w-[400px]">
        <div className="mb-7 flex flex-col items-center gap-3 text-center">
          <div className="flex size-12 items-center justify-center rounded-full border border-primary/25 bg-primary/12">
            <ShieldCheck className="size-6 text-primary" />
          </div>
          <h1 className="text-xl font-semibold tracking-tight text-foreground">
            Wardyn
          </h1>
          {/* No trust-domain / identity-provider chips here. Someone at a sign-in
              form cannot act on either, has not been taught the vocabulary, and on
              a default install both are constants (wardyn.local / embedded). The
              app chrome surfaces them where they are NON-default, which is the only
              case worth a reader's attention. */}
        </div>

        <div className="rounded-xl border border-border bg-card p-6 shadow-floating">
          <div className="mb-4">
            <h2 className="text-base font-semibold text-foreground">Sign in</h2>
            <p className="mt-1 text-sm text-muted-foreground">{SIGNIN.LEAD}</p>
          </div>

          {/* #457 (Q457-1): before /healthz has answered even once, neither
              door is known to exist — no token field, no SSO button, no claim
              either way, just an honest "checking" row. After
              STILL_CHECKING_AFTER_READS unanswered reads (Q457-2), say the
              checking is still going rather than sit silent. */}
          {checking ? (
            <div role="status" className="flex items-center gap-2 text-sm text-muted-foreground">
              <Loader2 className="size-4 animate-spin" aria-hidden="true" />
              <span>{stillChecking ? SIGNIN.STILL_CHECKING : SIGNIN.CHECKING}</span>
            </div>
          ) : (
            // #378/#379: the admin-token form renders only when the daemon
            // says a token can actually work (showTokenForm) — never on an
            // SSO-only deployment, where a live token is a second front
            // door the posture asserts does not exist. Every other cell
            // (today's combined deployment, a token-only deployment, and
            // the "neither is known to work" local-mode/misconfigured
            // case) keeps rendering it.
            showTokenForm && (
              <form
                onSubmit={(e) => {
                  e.preventDefault();
                  void submitToken();
                }}
                className="space-y-2"
              >
                <Label htmlFor="token" className="text-foreground">
                  Admin token
                </Label>
                <div className="relative">
                  <KeyRound className="absolute left-3 top-1/2 size-4 -translate-y-1/2 text-muted-foreground" />
                  <Input
                    id="token"
                    type="password"
                    value={token}
                    onChange={(e) => {
                      setTokenValue(e.target.value);
                      if (error) setError(null);
                    }}
                    className="pl-9 font-mono"
                    autoComplete="off"
                  />
                </div>
                <p className="text-xs text-muted-foreground">{SIGNIN.TOKEN_HINT}</p>
                <label className="mt-1 flex items-center gap-2 text-xs text-muted-foreground">
                  <Checkbox
                    checked={remember}
                    onCheckedChange={(v) => setRemember(v === true)}
                    aria-label="Remember on this device"
                  />
                  Remember on this device
                  <span className="text-muted-foreground">
                    (keeps the token after the browser closes)
                  </span>
                </label>
                <Button
                  type="submit"
                  className="mt-1 w-full"
                  disabled={!token || loading !== null}
                >
                  {loading === "token" ? (
                    <Loader2 className="size-4 animate-spin" />
                  ) : (
                    <>
                      Sign in
                      <ArrowRight className="size-4" />
                    </>
                  )}
                </Button>
              </form>
            )
          )}

          {error && (
            <div
              role="alert"
              className="mt-3 flex items-start gap-2 rounded-md border border-danger/30 bg-danger-subtle px-3 py-2 text-xs text-danger"
            >
              <AlertCircle className="mt-0.5 size-4 shrink-0" />
              <span>{error}</span>
            </div>
          )}
          {error && refusalCode && SIGNIN_HELP_REFUSALS.has(refusalCode) && (
            <SignInHelp text={help.text} url={help.url} />
          )}

          {showDivider && (
            <div className="my-5 flex items-center gap-3">
              <div className="h-px flex-1 bg-border" />
              <span className="text-xs uppercase tracking-wide text-muted-foreground">
                or
              </span>
              <div className="h-px flex-1 bg-border" />
            </div>
          )}

          {/* SSO sign-in. The server-side OIDC flow (PKCE; GET /auth/login)
              ships whenever OIDC is configured, and its session cookie
              authenticates the whole API — so when /healthz reports sso we
              link straight to it (a plain GET redirect, no JS handshake).
              #457: without sso there is nothing to render here at all — no
              disabled stub, no "isn't configured" caveat naming a chart
              value a reader here cannot reach. */}
          {showSso && (
            <Button asChild variant="outline" className="w-full">
              <a href="/auth/login">
                <Building2 className="size-4" />
                Sign in with SSO
              </a>
            </Button>
          )}
        </div>
      </div>
    </div>
  );
}
