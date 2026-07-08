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
import { Input } from "../ui/input";
import { Label } from "../ui/label";
import { Chip } from "../wardyn/primitives";
import { useTheme } from "../wardyn/theme-provider";
import { setToken, probeAuth, api } from "../../lib/api";

export function SignIn({ onSignIn }: { onSignIn: () => void }) {
  const { theme, toggle } = useTheme();
  const [token, setTokenValue] = React.useState("");
  const [loading, setLoading] = React.useState<"token" | "sso" | null>(null);
  const [error, setError] = React.useState<string | null>(null);
  // Trust boundary shown pre-auth — populated from /healthz when it responds.
  // Seeded EMPTY (not "wardyn.local"/"embedded", which are the DEFAULT-instance
  // values and would be WRONG on a SPIRE/custom-trust-domain host where health()
  // fails): we never assert a specific trust domain / provider that isn't real.
  const [trustDomain, setTrustDomain] = React.useState("");
  const [identityProvider, setIdentityProvider] = React.useState("");
  React.useEffect(() => {
    api.health().then((h) => {
      if (h.trust_domain) setTrustDomain(h.trust_domain);
      if (h.identity_provider) setIdentityProvider(h.identity_provider);
    });
  }, []);

  const submitToken = async () => {
    if (!token) return;
    setLoading("token");
    setError(null);
    // Persist the admin token, then verify it against a protected endpoint.
    setToken(token);
    const ok = await probeAuth();
    if (ok) {
      onSignIn();
    } else {
      // Bad token — clear it so subsequent requests don't carry it.
      setToken(null);
      setLoading(null);
      setError("That admin token was rejected. Check the value and try again.");
    }
  };

  const submitSso = () => {
    setLoading("sso");
    setError(null);
    // Hand off to the OIDC login flow; the server sets a session cookie
    // and redirects back, at which point App's mount probe takes over.
    window.location.href = "/auth/login";
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
                placeholder="wardyn_admin_••••••••••••••••"
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
              Paste the admin token wardynd printed on startup.
            </p>
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
              className="mt-3 flex items-start gap-2 rounded-md border border-danger/30 bg-danger-subtle px-3 py-2 text-[12.5px] text-danger"
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

          <Button variant="outline" className="w-full" onClick={submitSso} disabled={loading !== null}>
            {loading === "sso" ? <Loader2 className="size-4 animate-spin" /> : <Building2 className="size-4" />}
            Sign in with SSO
          </Button>
        </div>

        <p className="mt-5 text-center text-xs text-muted-foreground">
          Run identities are minted under this trust domain.
        </p>
      </div>
    </div>
  );
}
