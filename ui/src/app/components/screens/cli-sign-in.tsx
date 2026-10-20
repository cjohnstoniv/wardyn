/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// /cli/sign-in (DESIGN-ATTACH 3.3 steps 2-5; boards MATT2-*): the page `wardyn login` opens. It shows the
// computer, the code the terminal printed, where the request came from and when, and the person approves or
// denies. Approve is a fetch POST (not a form); after its 200 the page itself moves the browser to the CLI's
// loopback address (no HTTP 302 is involved). In paste mode (wardyn login --no-browser, off unless the operator
// turned it on) the page shows the code once instead. The mode is the request's own, so this page offers paste
// only for a request the server made in that mode.
//
// The page reads the address-mismatch verdict from the server and does not decide it: it is advisory, and behind
// an unlisted proxy both sides read the proxy and always match.
import * as React from "react";
import { useSearchParams } from "react-router-dom";
import { Check, Copy } from "lucide-react";
import { HttpError } from "../../lib/api/core";
import { cliSignIn } from "../../lib/api/cli-sign-in";
import { health } from "../../lib/api/health";
import { getErrorMessage } from "../../lib/format";
import { useCopyToClipboard } from "../../lib/use-copy-to-clipboard";
import type { CLISignInView } from "../../lib/types/cli-sign-in";
import { Button } from "../ui/button";
import { approveRefusal, SIGN_IN as T, stampOf } from "../wardyn/copy/cli-sign-in";
import { Notice } from "../wardyn/notice";
import { RouteFallback } from "../wardyn/route-fallback";
import { ErrorState } from "../wardyn/states";

type Phase =
  | { kind: "loading" }
  | { kind: "ready"; view: CLISignInView }
  | { kind: "pasted"; code: string; lifeSeconds: number }
  | { kind: "denied" }
  | { kind: "used" }
  | { kind: "expired" }
  | { kind: "error"; message: string };

/** Only the CLI's own loopback callback is somewhere this page moves the browser to. */
export function isLoopbackCallback(raw: string): boolean {
  try {
    const u = new URL(raw);
    return u.protocol === "http:" && u.hostname === "127.0.0.1" && u.pathname === "/callback";
  } catch {
    return false;
  }
}

function goneKind(e: unknown): "used" | "expired" | null {
  if (!(e instanceof HttpError) || e.status !== 410) return null;
  return e.reason === "cli_sign_in_expired" ? "expired" : "used";
}

export function CliSignInScreen() {
  const id = useSearchParams()[0].get("id") ?? "";
  const [phase, setPhase] = React.useState<Phase>({ kind: "loading" });
  const [who, setWho] = React.useState("");
  const [busy, setBusy] = React.useState(false);
  const [refusal, setRefusal] = React.useState("");

  const load = React.useCallback(() => {
    setPhase({ kind: "loading" });
    Promise.all([cliSignIn.get(id), health.whoami()])
      .then(([view, me]) => {
        setWho(me?.email || me?.principal || "");
        setPhase({ kind: "ready", view });
      })
      .catch((e: unknown) => setPhase(goneKind(e) ? { kind: goneKind(e)! } : { kind: "error", message: getErrorMessage(e) }));
  }, [id]);
  React.useEffect(load, [load]);

  const decide = async (view: CLISignInView, approve: boolean) => {
    if (busy) return;
    setBusy(true);
    setRefusal("");
    try {
      if (approve) {
        const r = await cliSignIn.approve(view.id);
        if (view.mode === "paste") setPhase({ kind: "pasted", code: r.code, lifeSeconds: r.code_expires_in });
        else if (r.redirect_url && isLoopbackCallback(r.redirect_url)) window.location.assign(r.redirect_url);
        else setRefusal(T.NO_CALLBACK);
      } else {
        const r = await cliSignIn.deny(view.id);
        // The CLI is told through its callback, which prints the refusal in the terminal; paste mode has none.
        if (view.mode === "loopback" && r.redirect_url && isLoopbackCallback(r.redirect_url)) window.location.assign(r.redirect_url);
        else setPhase({ kind: "denied" });
      }
    } catch (e) {
      const gone = goneKind(e);
      if (gone) setPhase({ kind: gone });
      else setRefusal((e instanceof HttpError && approveRefusal(e.reason)) || getErrorMessage(e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="mx-auto w-full max-w-[560px] px-4 py-8 sm:px-6">
      {phase.kind === "loading" && <RouteFallback />}
      {phase.kind === "error" && <ErrorState message={phase.message} onRetry={load} />}
      {phase.kind === "ready" && <Request view={phase.view} who={who} busy={busy} refusal={refusal} onDecide={decide} />}
      {phase.kind === "pasted" && <Pasted code={phase.code} lifeSeconds={phase.lifeSeconds} />}
      {phase.kind === "denied" && <Plain title={T.DENIED_TITLE} body={T.DENIED_BODY} />}
      {phase.kind === "used" && <Plain title={T.USED_TITLE} body={T.USED_BODY} />}
      {phase.kind === "expired" && <Plain title={T.EXPIRED_TITLE} body={T.EXPIRED_BODY} />}
    </div>
  );
}

function Card({ children }: { children: React.ReactNode }) {
  return <section className="space-y-5 rounded-xl border border-border bg-card p-5 sm:p-6">{children}</section>;
}

function Plain({ title, body }: { title: string; body: string }) {
  return (
    <Card>
      <div className="space-y-1.5">
        <p className="label-eyebrow">{T.EYEBROW}</p>
        <h1 className="text-xl font-semibold text-foreground">{title}</h1>
        <p className="text-sm text-muted-foreground">{body}</p>
      </div>
    </Card>
  );
}

function Field({ label, children, note }: { label: string; children: React.ReactNode; note?: string }) {
  return (
    <div className="space-y-0.5">
      <dt className="text-xs text-muted-foreground">{label}</dt>
      <dd className="text-sm text-foreground">{children}</dd>
      {note && <dd className="text-xs text-muted-foreground">{note}</dd>}
    </div>
  );
}

function Request({
  view,
  who,
  busy,
  refusal,
  onDecide,
}: {
  view: CLISignInView;
  who: string;
  busy: boolean;
  refusal: string;
  onDecide: (view: CLISignInView, approve: boolean) => void;
}) {
  const lifeSeconds = Math.round((Date.parse(view.expires_at) - Date.parse(view.created_at)) / 1000);
  return (
    <Card>
      <div className="space-y-1.5">
        <p className="label-eyebrow">{T.EYEBROW}</p>
        <h1 className="text-xl font-semibold text-foreground">{view.admin ? T.ADMIN_TITLE(view.access_ttl_seconds) : T.TITLE}</h1>
        <p className="text-sm text-muted-foreground">{view.admin ? T.ADMIN_LEAD(view.access_ttl_seconds) : T.LEAD(who)}</p>
      </div>

      {!view.address_match && (
        <div role="alert" data-testid="cli-address-mismatch">
          <Notice tone="warning" title={T.MISMATCH_TITLE}>
            <p>{T.MISMATCH_BODY}</p>
            <p className="text-xs text-muted-foreground">{T.BROWSER_ADDRESS(view.approver_address)}</p>
          </Notice>
        </div>
      )}

      <dl className="space-y-4">
        <Field label={T.COMPUTER}>
          <span className="font-mono">{view.machine}</span>
        </Field>
        <Field label={T.FROM} note={view.address_match ? undefined : T.NOT_THIS_BROWSER}>
          <span className="font-mono">{view.request_address}</span>
        </Field>
        <Field label={T.REQUESTED} note={T.EXPIRES_IN(lifeSeconds)}>
          {stampOf(view.created_at)}
        </Field>
        <Field label={T.CODE} note={T.CODE_NOTE}>
          <span className="font-mono text-2xl font-medium tracking-widest" data-testid="cli-user-code">
            {view.user_code}
          </span>
        </Field>
        <Field label={T.ALLOWS}>
          {view.admin
            ? T.ADMIN_GRANT(view.access_ttl_seconds)
            : T.GRANT(view.access_ttl_seconds, view.session_ttl_seconds, view.session_idle_seconds)}
        </Field>
      </dl>

      {refusal && (
        <div role="alert">
          <Notice tone="danger">{refusal}</Notice>
        </div>
      )}

      <div className="flex flex-wrap gap-2">
        <Button type="button" className="h-11 sm:h-9" disabled={busy} onClick={() => onDecide(view, true)}>
          {T.APPROVE}
        </Button>
        <Button type="button" variant="outline" className="h-11 sm:h-9" disabled={busy} onClick={() => onDecide(view, false)}>
          {T.DENY}
        </Button>
      </div>
    </Card>
  );
}

function Pasted({ code, lifeSeconds }: { code: string; lifeSeconds: number }) {
  const { copied, copy } = useCopyToClipboard();
  return (
    <Card>
      <div className="space-y-3">
        <p className="label-eyebrow">{T.EYEBROW}</p>
        <h1 className="text-xl font-semibold text-foreground">{T.PASTE_TITLE}</h1>
        <div className="flex flex-wrap items-center gap-3">
          <span className="font-mono text-2xl font-medium tracking-widest" data-testid="cli-paste-code">
            {code}
          </span>
          <Button type="button" variant="outline" className="h-11 sm:h-9" onClick={() => copy(code)}>
            {copied ? <Check className="size-4 text-success" aria-hidden /> : <Copy className="size-4" aria-hidden />}
            {T.PASTE_COPY}
          </Button>
        </div>
        <Notice tone="warning">{T.PASTE_WARN}</Notice>
        <p className="text-sm text-muted-foreground">{T.PASTE_ONCE(lifeSeconds)}</p>
      </div>
    </Card>
  );
}
