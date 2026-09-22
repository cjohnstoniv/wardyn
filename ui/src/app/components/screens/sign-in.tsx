/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import {
  AlertCircle,
  AlertTriangle,
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
import { SIGNIN } from "../../lib/people-access-copy";
import { usePoll } from "../../lib/use-poll";

// How often the gate re-asks /healthz for `sso` (R4/F027). Slower than the
// shell's 5s health poll: nothing here is live data, this only has to notice a
// daemon that came up after the gate rendered.
const SSO_POLL_MS = 10000;

// Unreachable (#212, design/first-contact-prototype) — was "Could not reach
// the control plane.", which named the product nothing and gave no next
// step. Now names Wardyn and names one thing to check.
export const UNREACHABLE_ERROR =
  "Wardyn isn't answering at this address. Check that the wardynd daemon is running, then try again.";

// Token hint (#212, design/first-contact-prototype) — was "Paste the token
// this control plane was started with (WARDYN_ADMIN_TOKEN; the compose demo
// uses demo-admin-token).": it named an env var and a working demo
// credential on a screen reachable by anyone, authenticated or not. Says
// what belongs in the field and where the person saw it instead.
export const TOKEN_HINT =
  "The token this Wardyn daemon was started with. Your install printed it when it finished.";

// Shared with the in-place sign-in dialog (#483, wardyn/reauth-layer.tsx).
export const TOKEN_LABEL = "Admin token";
export const TOKEN_REJECTED = "That admin token was rejected. Check the value and try again.";
export const SSO_SIGN_IN = "Sign in with SSO";

// The OIDC callback (internal/auth/oidc/oidc.go's CallbackHandler)
// redirects a user-actionable login denial to "/?auth_error=<code>" instead
// of dead-ending the browser on a bare http.Error text page — but a redirect
// nobody reads is no better: it silently bounces back to this exact screen
// with zero explanation. These are the stable, machine-readable codes
// redirectAuthError sends; keep in sync with oidc.go's authError* consts.
//
// no_role/email_verified_absent are REWORDED (0.7 SSO Phase 3,
// docs/design/people-access-prompt.md §7.7) — both now name "your Wardyn
// admin" as who to ask, env var(s) demoted to a parenthetical, per that
// section's rule that a signed-in human should never be told to go set an env
// var themselves. claims_overage is the newest arm (an Entra groups/App-Roles
// overage the server refuses rather than defaulting through — see
// authErrorClaimsOverage in oidc.go); the generic fallback would have told this
// human to try again, which can never work. role_check_unavailable is the one NEW arm (the People
// step's preview panel and this screen share the same "couldn't check"
// language). All three come from lib/people-access-copy.ts's SIGNIN table —
// every other arm below is unchanged and out of scope this round.
//
// email_domain (#212, design/first-contact-prototype) — was "Ask an operator
// to add it to WARDYN_OIDC_EMAIL_DOMAINS": an environment variable this
// reader, who is not signed in, cannot reach, on a screen that must not
// advertise a deployment's internals to someone unauthenticated. The remedy
// belongs to the admin, not this person.
export const EMAIL_DOMAIN_REFUSAL =
  "This email's domain isn't allowed to sign in to this console. Ask your Wardyn admin to allow it.";
function authErrorMessage(code: string): string {
  switch (code) {
    case "email_unverified":
      return "Your identity provider reports this email as unverified. Verify your email with your identity provider, then try again.";
    case "email_verified_absent":
      return SIGNIN.EMAIL_VERIFIED_ABSENT;
    case "email_domain":
      return EMAIL_DOMAIN_REFUSAL;
    case "no_role":
      return SIGNIN.NO_ROLE;
    case "role_check_unavailable":
      return SIGNIN.ROLE_CHECK_UNAVAILABLE;
    // claims_overage: the IdP withheld the groups/App Roles claim entirely
    // (too many groups to fit a sign-in token), so the role map matched
    // nothing and the server refused to let the default role decide. NOT a
    // "try again" — retrying sends the same token — which is exactly why it
    // must not fall through to the generic arm below.
    case "claims_overage":
      return "Your identity provider sent too many groups to list in a sign-in token, so this console can't tell what access you should have — and won't guess. Ask your Wardyn admin to map your role by App Role or email instead (the People step, or WARDYN_OIDC_ROLE_MAP). Trying again won't help.";
    case "oidc_transient":
      return "Your identity provider didn't respond in time. This is usually temporary — try signing in again.";
    case "oidc_config":
      return "Sign-in with your identity provider failed. Try again; if it keeps happening, ask an operator to check the OIDC client configuration.";
    default:
      return "Sign-in failed. Try again, or contact an operator.";
  }
}

export function SignIn({
  onSignIn,
  // #483: a session this browser held was refused on load — an amber
  // warning (it is news, not a failure of anything typed here), never the
  // error box below. Undefined on a first visit and after a sign-out.
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
  const [error, setError] = React.useState<string | null>(null);
  // Whether this control plane has OIDC configured (so GET /auth/login exists).
  // Defaults false: without the flow mounted the link would 404, and an older
  // server simply omits the field.
  const [sso, setSso] = React.useState(false);
  // #378/#379: whether the admin-token form can work at all, and whether SSO
  // is the ONLY way in. tokenLogin defaults true (today's behaviour: the form
  // always renders until the daemon says otherwise) and ssoOnly defaults
  // false (today's caveat always renders under an enabled SSO button) — an
  // older daemon that omits both fields must read exactly as it does today.
  const [tokenLogin, setTokenLogin] = React.useState(true);
  const [ssoOnly, setSsoOnly] = React.useState(false);
  // R4/F027: health() resolves `{}` on a network error or ANY non-2xx, so a
  // single mount fetch against a daemon that is merely starting (or briefly
  // 5xx-ing) read as `sso: false` and left the SSO button unrendered — on an
  // SSO-only deployment that is a bare token field and no way in, forever,
  // because nothing ever asked again. So: only believe an answer that actually
  // came back, leave the last known value alone otherwise, and keep asking.
  const refreshSso = React.useCallback(() => {
    // Still fetched for `sso` (plus token_login/sso_only, #378/#379): together
    // they decide what the screen offers. trust_domain / identity_provider are
    // deliberately NOT read here any more.
    void health.health().then((h) => {
      // health() resolves the EMPTY object for "no answer" (health.ts:237-243:
      // `if (!res.ok) return {}` / `catch { return {} }`), and a real /healthz
      // body is never empty — it always carries at least status and version. So
      // an empty object is the outage, and the last known answer survives it.
      if (Object.keys(h).length === 0) return;
      setSso(!!h.sso);
      // token_login is undefined on an older daemon — read that as `true`, not
      // `false`: the form is that daemon's only path in, and dropping it on a
      // missing field would be a regression, not a safer default.
      setTokenLogin(h.token_login !== false);
      setSsoOnly(!!h.sso_only);
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
        setError(TOKEN_REJECTED);
      } else {
        setError(UNREACHABLE_ERROR);
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
  // Reduces to !ssoOnly whenever tokenLogin is false with sso true (the
  // sso-only shape, where the daemon has already refused every other way in).
  const showTokenForm = tokenLogin || !sso;

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
            <p className="mt-1 text-sm text-muted-foreground">
              This console governs the agents on this host.
            </p>
          </div>

          {reason && (
            <div
              role="status"
              className="mb-4 flex items-start gap-2 rounded-md border border-warning/30 bg-warning-subtle px-3 py-2 text-xs text-warning"
            >
              <AlertTriangle className="mt-0.5 size-4 shrink-0" />
              <span>{reason}</span>
            </div>
          )}

          {/* #378/#379: the admin-token form renders only when the daemon says a
              token can actually work (showTokenForm) — never on an SSO-only
              deployment, where a live token is a second front door the posture
              asserts does not exist. Every other cell (today's combined
              deployment, a token-only deployment, and the "neither is known to
              work" local-mode/misconfigured case) keeps rendering it. */}
          {showTokenForm && (
            <form
              onSubmit={(e) => {
                e.preventDefault();
                void submitToken();
              }}
              className="space-y-2"
            >
              <Label htmlFor="token" className="text-foreground">
                {TOKEN_LABEL}
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
              <p className="text-xs text-muted-foreground">{TOKEN_HINT}</p>
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

          {showTokenForm && (
            <div className="my-5 flex items-center gap-3">
              <div className="h-px flex-1 bg-border" />
              <span className="text-xs uppercase tracking-wide text-muted-foreground">
                or
              </span>
              <div className="h-px flex-1 bg-border" />
            </div>
          )}

          {/* SSO sign-in. The server-side OIDC flow (PKCE; GET /auth/login) ships
              whenever WARDYN_OIDC_* is configured, and its session cookie
              authenticates the whole API — so when /healthz reports sso we link
              straight to it (a plain GET redirect, no JS handshake). Without OIDC
              configured there is nothing to link to; the button stays disabled and
              the admin token is the supported path. What is still missing is per-user
              RBAC, not the login — hence the caveat below, kept in the enabled state
              rather than dropped along with the disabled attribute — UNLESS sso_only
              (#379) says that branch of role derivation is unreachable here, in which
              case the caveat is noise on the front door and is dropped too. */}
          {sso ? (
            <>
              <Button asChild variant="outline" className="w-full">
                <a href="/auth/login">
                  <Building2 className="size-4" />
                  {SSO_SIGN_IN}
                </a>
              </Button>
              {!ssoOnly && (
                <p className="mt-2 text-center text-xs text-muted-foreground">
                  {SIGNIN.ROLE_SOURCE}
                </p>
              )}
            </>
          ) : (
            <>
              <Button
                variant="outline"
                className="w-full"
                disabled
                title="SSO sign-in needs OIDC configured on this control plane (WARDYN_OIDC_*)"
              >
                <Building2 className="size-4" />
                {SSO_SIGN_IN}
              </Button>
              <p className="mt-2 text-center text-xs text-muted-foreground">
                SSO sign-in isn&apos;t configured on this control plane — use an
                admin token.
              </p>
            </>
          )}
        </div>
      </div>
    </div>
  );
}
