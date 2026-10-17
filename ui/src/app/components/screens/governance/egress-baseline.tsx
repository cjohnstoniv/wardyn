/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Governance -> Baseline hosts: the operator's extension of the built-in baseline the autonomy rubric and
// the risk grade read (egress_baseline.go). The same document shape as Runner credentials: a whole-document
// PUT with If-Match, held for a second person where one is required (202 -> SubmittedNote), a 412 reloads.
//
// EVERY user-visible string comes from governance-copy.ts's BASELINE block. The server validates each
// entry and its 400 sentence is shown as written.
import * as React from "react";
import { AlertTriangle, Loader2 } from "lucide-react";
import { HttpError, PendingChangeError } from "../../../lib/api/core";
import { egressBaseline, type EgressBaseline } from "../../../lib/api/egress-baseline";
import { getErrorMessage } from "../../../lib/format";
import { BASELINE } from "../../../lib/governance-copy";
import { ACCESS_STATE } from "../../../lib/people-access-copy";
import { useDeferredBusy } from "../../../lib/use-deferred-busy";
import { useUnsavedGuard } from "../../../lib/use-unsaved-guard";
import { SavedElsewhereBanner } from "../../wardyn/saved-elsewhere-banner";
import { Button } from "../../ui/button";
import { Label } from "../../ui/label";
import { Textarea } from "../../ui/textarea";
import { useSecurityOperator } from "../../wardyn/operator-context";
import { EmptyState, TableSkeleton } from "../../wardyn/states";
import { Note } from "./display";
import { SubmittedNote } from "./submitted-note";

interface Draft {
  hosts: string;
  suffixes: string;
}

const lines = (text: string) =>
  text
    .split("\n")
    .map((l) => l.trim())
    .filter((l) => l !== "");

const draftFrom = (b: EgressBaseline): Draft => ({ hosts: b.baseline_hosts.join("\n"), suffixes: b.internal_host_suffixes.join("\n") });
const bodyFrom = (d: Draft): EgressBaseline => ({ baseline_hosts: lines(d.hosts), internal_host_suffixes: lines(d.suffixes) });

export function EgressBaselineTab({ onSubmitted }: { onSubmitted?: () => void }) {
  const securityOperator = useSecurityOperator();
  const [status, setStatus] = React.useState<"loading" | "ready" | "error">("loading");
  const [stored, setStored] = React.useState<EgressBaseline>({ baseline_hosts: [], internal_host_suffixes: [] });
  const [draft, setDraft] = React.useState<Draft>({ hosts: "", suffixes: "" });
  const [outcome, setOutcome] = React.useState<{ kind: "saved" | "submitted" } | { kind: "refused"; message: string } | null>(null);
  const [busy, setBusy] = React.useState(false);
  const [savedElsewhere, setSavedElsewhere] = React.useState<string | null>(null);
  const etagRef = React.useRef<string | null>(null);
  // The declaration last held for a second person: until the draft changes again it is not a new change.
  const [submittedJson, setSubmittedJson] = React.useState<string | null>(null);
  const { showSpinner } = useDeferredBusy(busy);
  const refusedRef = React.useRef<HTMLHeadingElement>(null);

  const load = React.useCallback(() => {
    setStatus("loading");
    egressBaseline
      .get()
      .then((doc) => {
        const b = { baseline_hosts: doc.baseline_hosts ?? [], internal_host_suffixes: doc.internal_host_suffixes ?? [] };
        setStored(b);
        etagRef.current = doc.etag ?? null;
        setSubmittedJson(null);
        setDraft(draftFrom(b));
        setStatus("ready");
      })
      .catch(() => setStatus("error"));
  }, []);
  React.useEffect(load, [load]);

  const body = React.useMemo(() => bodyFrom(draft), [draft]);
  const json = JSON.stringify(body);
  const dirty = json !== JSON.stringify(bodyFrom(draftFrom(stored))) && json !== submittedJson;
  useUnsavedGuard("egress-baseline", dirty || savedElsewhere !== null, () => savedElsewhere ?? JSON.stringify(body, null, 2));

  React.useEffect(() => {
    if (outcome?.kind === "refused") refusedRef.current?.focus();
  }, [outcome]);

  const edit = (patch: Partial<Draft>) => {
    setOutcome(null);
    setDraft((d) => ({ ...d, ...patch }));
  };

  const save = async () => {
    setBusy(true);
    setOutcome(null);
    try {
      const doc = await egressBaseline.put(body, etagRef.current);
      const b = { baseline_hosts: doc.baseline_hosts ?? [], internal_host_suffixes: doc.internal_host_suffixes ?? [] };
      setStored(b);
      setDraft(draftFrom(b));
      etagRef.current = doc.etag ?? null;
      setOutcome({ kind: "saved" });
      setSavedElsewhere(null);
    } catch (e) {
      if (e instanceof HttpError && e.status === 412) {
        setSavedElsewhere(JSON.stringify(body, null, 2));
        load();
      } else if (e instanceof PendingChangeError) {
        setSubmittedJson(json);
        setOutcome({ kind: "submitted" });
        onSubmitted?.();
      } else {
        setOutcome({ kind: "refused", message: e instanceof HttpError ? e.message : getErrorMessage(e) });
      }
    } finally {
      setBusy(false);
    }
  };

  const disabled = !securityOperator || busy;
  return (
    <section className="mt-6 overflow-hidden rounded-xl border border-border bg-card">
      <div className="px-6 pt-5">
        <h2 className="text-sm font-medium text-foreground">{BASELINE.TAB}</h2>
        <p className="mt-1 max-w-[82ch] text-body text-muted-foreground">{BASELINE.LEAD}</p>
      </div>

      {status === "loading" && (
        <div className="mt-4">
          <TableSkeleton rows={2} cols={1} />
        </div>
      )}

      {status === "error" && (
        <EmptyState
          icon={AlertTriangle}
          title={BASELINE.FETCH_FAILED_TITLE}
          description={BASELINE.FETCH_FAILED_BODY}
          action={
            <Button variant="outline" size="sm" onClick={load}>
              {ACCESS_STATE.FETCH_FAILED_RETRY}
            </Button>
          }
        />
      )}

      {status === "ready" && (
        <>
          <div className="mt-4 grid gap-5 border-t border-border px-6 py-4 md:grid-cols-2">
            <div className="flex flex-col gap-1.5">
              <Label htmlFor="baseline-hosts">{BASELINE.HOSTS_LABEL}</Label>
              <Textarea
                id="baseline-hosts"
                rows={6}
                value={draft.hosts}
                disabled={disabled}
                aria-describedby="baseline-hosts-hint"
                onChange={(e) => edit({ hosts: e.target.value })}
              />
              <p id="baseline-hosts-hint" className="text-xs text-muted-foreground">
                {BASELINE.HOSTS_HINT}
              </p>
            </div>
            <div className="flex flex-col gap-1.5">
              <Label htmlFor="baseline-suffixes">{BASELINE.SUFFIXES_LABEL}</Label>
              <Textarea
                id="baseline-suffixes"
                rows={6}
                value={draft.suffixes}
                disabled={disabled}
                aria-describedby="baseline-suffixes-hint"
                onChange={(e) => edit({ suffixes: e.target.value })}
              />
              <p id="baseline-suffixes-hint" className="text-xs text-muted-foreground">
                {BASELINE.SUFFIXES_HINT}
              </p>
            </div>
          </div>

          <div className="flex flex-col gap-2 px-6 py-4">
            {savedElsewhere !== null && <SavedElsewhereBanner reloaded documentText={savedElsewhere} onDiscard={() => setSavedElsewhere(null)} />}
            {outcome?.kind === "saved" && <Note role="status">{BASELINE.SAVED}</Note>}
            {outcome?.kind === "submitted" && <SubmittedNote />}
            {outcome?.kind === "refused" && (
              <Note tone="red" role="alert">
                <h3 ref={refusedRef} tabIndex={-1} className="font-semibold outline-none">
                  {BASELINE.REFUSED_TITLE}
                </h3>
                <span>{outcome.message}</span>
              </Note>
            )}
            <div className="flex justify-end">
              <Button disabled={disabled || !dirty} onClick={() => void save()}>
                {showSpinner ? <Loader2 className="size-4 animate-spin" /> : null}
                {BASELINE.SAVE}
              </Button>
            </div>
          </div>
        </>
      )}
    </section>
  );
}
