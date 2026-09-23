/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// #484 — "When someone can't sign in": the admin's own request-access help,
// under Role mappings on the People step (DeploymentStep's multi-user branch).
// Two site-config fields (sign_in_help_text / sign_in_help_url), saved through
// PUT /site-config with the GET's ETag as If-Match, so a save can never spread
// a stale document over someone else's newer one. Every string comes from
// access-posture-copy.ts's SIGNIN_HELP; this file adds none.
//
// Admin only, like the write it makes (PUT /site-config is operatorOnly): a
// member never reaches the People step, and a resolved non-admin renders
// nothing here rather than a form the server would refuse.
import * as React from "react";
import { AlertCircle, Loader2, RotateCw } from "lucide-react";
import { toast } from "sonner";

import { HttpError } from "../../../lib/api/core";
import { health as api } from "../../../lib/api/health";
import { getErrorMessage } from "../../../lib/format";
import { SIGNIN_HELP, SIGNIN_HELP_TEXT_MAX } from "../../../lib/access-posture-copy";
import { SIGNIN } from "../../../lib/people-access-copy";
import type { SiteConfig } from "../../../lib/types";
import { Button } from "../../ui/button";
import { Input } from "../../ui/input";
import { Textarea } from "../../ui/textarea";
import { cn } from "../../ui/utils";
import { Field } from "../../wardyn/form-primitives";
import { useOperator, useOperatorResolved } from "../../wardyn/operator-context";
import { SignInHelp } from "../../wardyn/sign-in-help";

// The server refuses control characters (line breaks included), line/paragraph
// separators and invisible format characters (validateSignInHelp) — the text
// renders as one plain paragraph. A paste carrying them is cleaned as it
// arrives rather than refused at Save: breaks and controls fold to spaces,
// format characters (bidi overrides, zero-width spaces) are dropped.
const BREAKS = /[\u0000-\u001f\u007f-\u009f\u2028\u2029]/g;
const FORMAT = /\p{Cf}/gu;
// A courtesy only: the server's validSiteURL is the gate.
const HTTP_URL = /^https?:\/\/[^\s/?#]+/i;

// Characters, not UTF-16 units — the server counts runes.
export function helpTextLength(text: string): number {
  return Array.from(text).length;
}

export function SignInHelpCard() {
  const operator = useOperator();
  const resolved = useOperatorResolved();
  const admin = resolved && operator;

  const [base, setBase] = React.useState<SiteConfig | null>(null);
  const [etag, setEtag] = React.useState<string | null>(null);
  const [saved, setSaved] = React.useState({ text: "", url: "" });
  const [text, setText] = React.useState("");
  const [url, setUrl] = React.useState("");
  const [typing, setTyping] = React.useState(false);
  const [state, setState] = React.useState<"loading" | "ready" | "failed">("loading");
  const [saving, setSaving] = React.useState(false);
  const [refused, setRefused] = React.useState<string | null>(null);
  const [savedElsewhere, setSavedElsewhere] = React.useState(false);

  // keepDraft: the 412 path reloads the document underneath without touching
  // what the admin typed.
  const load = React.useCallback((keepDraft = false) => {
    return api
      .getSiteConfigSnapshot()
      .then(({ siteConfig, etag: tag }) => {
        const next = { text: siteConfig.sign_in_help_text ?? "", url: siteConfig.sign_in_help_url ?? "" };
        setBase(siteConfig);
        setEtag(tag);
        setSaved(next);
        if (!keepDraft) {
          setText(next.text);
          setUrl(next.url);
        }
        setState("ready");
      })
      .catch(() => setState("failed"));
  }, []);
  React.useEffect(() => {
    if (admin) void load();
  }, [admin, load]);

  if (!admin) return null;

  const count = helpTextLength(text);
  const tooLong = count > SIGNIN_HELP_TEXT_MAX;
  const badUrl = url.trim() !== "" && !HTTP_URL.test(url.trim());
  const dirty = text.trim() !== saved.text || url.trim() !== saved.url;
  const previewText = text.trim();
  const previewUrl = badUrl ? "" : url.trim();

  const save = async () => {
    if (!base) return;
    setSaving(true);
    setRefused(null);
    setSavedElsewhere(false);
    try {
      const result = await api.putSiteConfig(
        { ...base, sign_in_help_text: text.trim(), sign_in_help_url: url.trim() },
        etag,
      );
      const next = {
        text: result.siteConfig.sign_in_help_text ?? "",
        url: result.siteConfig.sign_in_help_url ?? "",
      };
      setBase(result.siteConfig);
      setEtag(result.etag);
      setSaved(next);
      setText(next.text);
      setUrl(next.url);
      toast.success(SIGNIN_HELP.SAVED_TOAST);
    } catch (e) {
      if (e instanceof HttpError && e.status === 412) {
        setSavedElsewhere(true);
        void load(true);
      } else if (e instanceof HttpError && e.status === 400) {
        // The server's own refusal, verbatim (validateSignInHelp's sentences).
        setRefused(e.message);
      } else {
        toast.error(SIGNIN_HELP.SAVE_ERROR, { description: getErrorMessage(e) });
      }
    } finally {
      setSaving(false);
    }
  };

  return (
    <section className="rounded-xl border border-border bg-card px-6 py-5" aria-labelledby="sign-in-help-title">
      <h3 id="sign-in-help-title" className="text-sm font-medium text-foreground">
        {SIGNIN_HELP.TITLE}
      </h3>
      <p className="mt-1.5 text-body text-muted-foreground">{SIGNIN_HELP.LEAD}</p>

      {state === "loading" && <Loader2 className="mt-4 size-4 animate-spin text-muted-foreground" />}
      {state === "failed" && (
        <div className="mt-4 flex flex-wrap items-center gap-2">
          <p className="text-sm text-muted-foreground">{SIGNIN_HELP.LOAD_FAILED}</p>
          <Button variant="outline" size="sm" onClick={() => void load()}>
            <RotateCw className="size-3.5" />
            {SIGNIN_HELP.RETRY}
          </Button>
        </div>
      )}

      {state === "ready" && (
        <div className="mt-4 space-y-4">
          <Field label={SIGNIN_HELP.TEXT_LABEL} htmlFor="sign-in-help-text" hint={SIGNIN_HELP.TEXT_HINT}>
            <Textarea
              id="sign-in-help-text"
              value={text}
              placeholder={SIGNIN_HELP.TEXT_PLACEHOLDER}
              aria-invalid={tooLong || undefined}
              rows={3}
              onChange={(e) => {
                setTyping(true);
                setText(e.target.value.replace(BREAKS, " ").replace(FORMAT, ""));
              }}
            />
          </Field>
          {typing && (
            <p className={cn("-mt-2 text-right text-xs", tooLong ? "text-danger" : "text-muted-foreground")}>
              {SIGNIN_HELP.COUNTER(count)}
            </p>
          )}
          <Field label={SIGNIN_HELP.URL_LABEL} htmlFor="sign-in-help-url" hint={SIGNIN_HELP.URL_HINT}>
            <Input
              id="sign-in-help-url"
              type="url"
              value={url}
              placeholder={SIGNIN_HELP.URL_PLACEHOLDER}
              aria-invalid={badUrl || undefined}
              className="font-mono"
              onChange={(e) => setUrl(e.target.value)}
            />
          </Field>

          {!previewText && !previewUrl && <p className="text-xs text-muted-foreground">{SIGNIN_HELP.EMPTY_NOTE}</p>}

          <div className="rounded-lg border border-border bg-surface-2 p-4">
            <p className="text-meta font-semibold uppercase tracking-wider text-muted-foreground">
              {SIGNIN_HELP.PREVIEW_HEADING}
            </p>
            <div className="mt-2 flex items-start gap-2 rounded-md border border-danger/30 bg-danger-subtle px-3 py-2 text-xs text-danger">
              <AlertCircle className="mt-0.5 size-4 shrink-0" />
              <span>{SIGNIN.NO_ROLE}</span>
            </div>
            <SignInHelp text={previewText} url={previewUrl} />
            <p className="mt-3 text-xs text-muted-foreground">{SIGNIN_HELP.APPLIES_NOTE}</p>
          </div>

          {savedElsewhere && (
            <p role="status" className="rounded-md border border-warning/30 bg-warning-subtle px-3 py-2 text-xs text-warning">
              {SIGNIN_HELP.SAVED_ELSEWHERE}
            </p>
          )}
          {refused && (
            <p role="alert" className="text-xs text-danger">
              {refused}
            </p>
          )}

          <div className="flex justify-end">
            <Button variant="outline" size="sm" onClick={() => void save()} disabled={saving || !dirty || tooLong || badUrl}>
              {saving ? <Loader2 className="size-4 animate-spin" /> : null}
              {SIGNIN_HELP.SAVE}
            </Button>
          </div>
        </div>
      )}
    </section>
  );
}
