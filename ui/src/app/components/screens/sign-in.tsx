/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import {
  AlertCircle,
  ArrowRight,
  Building2,
  Fingerprint,
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
import { Chip } from "../wardyn/primitives";
import { useTheme } from "../wardyn/theme-provider";
import { errText, HttpError, setToken, wfetch, withLimit } from "../../lib/api/core";
import { health } from "../../lib/api/health";

// W31-S1-5: the OIDC callback (internal/auth/oidc/oidc.go's CallbackHandler)
// redirects a user-actionable login denial to "/?auth_error=<code>" instead
// of dead-ending the browser on a bare http.Error text page — but a redirect
// nobody reads is no better: it silently bounces back to this exact screen
// with zero explanation. These are the stable, machine-readable codes
// redirectAuthError sends; keep in sync with oidc.go's authError* consts.
function authErrorMessage(code: string): string {
  switch (code) {
    case "email_unverified":
      return "Your identity provider reports this email as unverified. Verify your email with your identity provider, then try again.";
    case "email_domain":
      return "This email's domain isn't allowed to sign in to this console. Ask an operator to add it to WARDYN_OIDC_ALLOWED_EMAIL_DOMAINS.";
    case "no_role":
      return "Your account has no Wardyn role assigned. Ask an operator to map your role (WARDYN_OIDC_ROLE_MAP) or add your email to WARDYN_OIDC_OPERATOR_EMAILS.";
    default:
      return "Sign-in failed. Try again, or contact an operator.";
  }
}

export function SignIn({ onSignIn }: { onSignIn: () => void }) {
  const { theme, toggle } = useTheme();
  const [token, setTokenValue] = React.useState("");
  // Off by default: the token lives in sessionStorage (gone when the browser
  // closes). Opt in to persist it to localStorage across restarts.
  const [remember, setRemember] = React.useState(false);
  const [loading, setLoading] = React.useState<"token" | null>(null);
  const [error, setError] = React.useState<string | null>(null);
  // Trust boundary shown pre-auth — populated from /healthz when it responds.
  // Seeded EMPTY (not "wardyn.local"/"embedded", which are the DEFAULT-instance
  // values and would be WRONG on a SPIRE/custom-trust-domain host where health()
  // fails): we never assert a specific trust domain / provider that isn't real.
  const [trustDomain, setTrustDomain] = React.useState("");
  const [identityProvider, setIdentityProvider] = React.useState("");
  // Whether this control plane has OIDC configured (so GET /auth/login exists).
  // Defaults false: without the flow mounted the link would 404, and an older
  // server simply omits the field.
  const [sso, setSso] = React.useState(false);
  React.useEffect(() => {
    health.health().then((h) => {
      if (h.trust_domain) setTrustDomain(h.trust_domain);
      if (h.identity_provider) setIdentityProvider(h.identity_provider);
      setSso(!!h.sso);
    });
  }, []);

  // W31-S1-5: render the OIDC callback's ?auth_error=<code> (see
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
    window.history.replaceState({}, "", window.location.pathname + (qs ? `?${qs}` : ""));
  }, []);

  // W31-S1-4: probeAuth collapsed every failure — a rejected token (401), a
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
        setError("That admin token was rejected. Check the value and try again.");
      } else {
        setError("Could not reach the control plane.");
      }
    } finally {
      setLoading(null);
    }
  };

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
        {theme === "dark" ? <Sun className="size-4" /> : <Moon className="size-4" />}
      </Button>

      <div className="relative w-full max-w-[400px]">
        <div className="mb-7 flex flex-col items-center gap-3 text-center">
          <div className="flex size-12 items-center justify-center rounded-full border border-primary/25 bg-primary/12">
            <ShieldCheck className="size-6 text-primary" />
          </div>
          <h1 className="text-xl font-semibold tracking-tight text-foreground">Wardyn</h1>
          {(trustDomain || identityProvider) && (
            <div className="flex flex-wrap items-center justify-center gap-2">
              {trustDomain && (
                <Chip tone="success" dot mono>
                  {trustDomain}
                </Chip>
              )}
              {identityProvider && (
                <Chip tone="neutral" mono>
                  <Fingerprint className="size-3" />
                  identity: {identityProvider}
                </Chip>
              )}
            </div>
          )}
        </div>

        <div className="rounded-2xl border border-border bg-card p-6 shadow-xl">
          <div className="mb-4">
            <h2 className="text-base font-semibold text-foreground">Sign in</h2>
            <p className="mt-1 text-sm text-muted-foreground">
              This console governs the agents on this host.
            </p>
          </div>

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
                placeholder="demo-admin-token"
                value={token}
                onChange={(e) => {
                  setTokenValue(e.target.value);
                  if (error) setError(null);
                }}
                className="pl-9 font-mono"
                autoComplete="off"
              />
            </div>
            <p className="text-xs text-muted-foreground">
              Paste the token this control plane was started with (WARDYN_ADMIN_TOKEN; the
              compose demo uses demo-admin-token).
            </p>
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
            <Button type="submit" className="mt-1 w-full" disabled={!token || loading !== null}>
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

          {error && (
            <div
              role="alert"
              className="mt-3 flex items-start gap-2 rounded-md border border-danger/30 bg-danger-subtle px-3 py-2 text-[0.7813rem] text-danger"
            >
              <AlertCircle className="mt-0.5 size-4 shrink-0" />
              <span>{error}</span>
            </div>
          )}

          <div className="my-5 flex items-center gap-3">
            <div className="h-px flex-1 bg-border" />
            <span className="text-xs uppercase tracking-wide text-muted-foreground">or</span>
            <div className="h-px flex-1 bg-border" />
          </div>

          {/* SSO sign-in. The server-side OIDC flow (PKCE; GET /auth/login) ships
              whenever WARDYN_OIDC_* is configured, and its session cookie
              authenticates the whole API — so when /healthz reports sso we link
              straight to it (a plain GET redirect, no JS handshake). Without OIDC
              configured there is nothing to link to; the button stays disabled and
              the admin token is the supported path. What is still missing is per-user
              RBAC, not the login — hence the caveat below, kept in the enabled state
              rather than dropped along with the disabled attribute. */}
          {sso ? (
            <>
              <Button asChild variant="outline" className="w-full">
                <a href="/auth/login">
                  <Building2 className="size-4" />
                  Sign in with SSO
                </a>
              </Button>
              <p className="mt-2 text-center text-xs text-muted-foreground">
                Your role — admin or member — comes from your SSO role assignment. Everyone is
                an admin only when neither a role map nor the operator allowlist is set.
              </p>
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
                Sign in with SSO
              </Button>
              <p className="mt-2 text-center text-xs text-muted-foreground">
                SSO sign-in isn&apos;t configured on this control plane — use an admin token.
              </p>
            </>
          )}
        </div>

        <p className="mt-5 text-center text-xs text-muted-foreground">
          Run identities are minted under this trust domain.
        </p>
      </div>
    </div>
  );
}
