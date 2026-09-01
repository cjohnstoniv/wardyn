/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// AccessPanel — the People step's acting surface for console role mappings
// (0.7 SSO Phase 3), per the ADJUDICATED mock (docs/design/people-access-mock/
// index.html, Variant A) and its frozen strings (docs/design/
// people-access-prompt.md §7). Mounted by DeploymentStep's multi-user branch
// (step-bodies.tsx) below the existing Admins/Members card — this component
// owns none of that card, only what §6 lists: the merged role-mappings table +
// add form, the preview panel, and the IdP-duties note.
//
// EVERY user-visible string here comes from lib/people-access-copy.ts — the
// SAME discipline permissions.tsx documents at its own top (lines 11-13): this
// file adds no copy of its own.
//
// Data ownership: the ORCHESTRATOR (setup-screen.tsx) owns the GET /access
// fetch (gated on deploymentMode(status) === "multi-user", alongside its
// other status-derived fetches) and hands this component `access` + `state` +
// `onReload` — same shape as WorkspacesStep/ReviewStep already take their data
// from the caller rather than fetching their own. Every WRITE (add/delete
// mapping, preview) is local to this component, same split
// useSiteConfigStep's own doc describes for CorpNetworkStep.
import * as React from "react";
import { AlertTriangle, Info, Loader2, RotateCw, ShieldOff } from "lucide-react";
import { access as api, AccessCollisionError, AccessPostureFlipRequiredError } from "../../../lib/api/access";
import { getErrorMessage, relativeTime } from "../../../lib/format";
import type { AccessMapping, AccessResponse, AccessRole } from "../../../lib/types";
import { ACCESS_ERROR, ACCESS_STATE, GUARD, PEOPLE, PREVIEW } from "../../../lib/people-access-copy";
import { Button } from "../../ui/button";
import { Input } from "../../ui/input";
import { Textarea } from "../../ui/textarea";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "../../ui/alert-dialog";
import { cn } from "../../ui/utils";
import { Field } from "../../wardyn/form-primitives";
import { Mono } from "../../wardyn/code-block";
import { Chip } from "../../wardyn/primitives";
import { EmptyState } from "../../wardyn/states";
import { Segmented } from "../permissions";

// ------------------------------------------------------------
// Backtick-mono rendering (people-access-copy.ts's header note): a frozen
// string carries an env var name as plain text; this is the ONE place that
// decides which substrings get the mono treatment, applied uniformly
// wherever they recur.
// ------------------------------------------------------------
const MONO_TERMS = [
  "WARDYN_OIDC_ROLE_MAP",
  "WARDYN_OIDC_OPERATOR_EMAILS",
  "WARDYN_OIDC_DEFAULT_ROLE",
  "WARDYN_OIDC_EMAIL_DOMAINS",
  "WARDYN_OIDC_ALLOW_EMAIL_MAPPINGS",
  "email_verified",
];
const MONO_RE = new RegExp(`(${MONO_TERMS.join("|")})`, "g");

function withMono(text: string): React.ReactNode {
  return text.split(MONO_RE).map((part, i) =>
    MONO_TERMS.includes(part) ? (
      <Mono key={i} className="text-inherit">
        {part}
      </Mono>
    ) : (
      <React.Fragment key={i}>{part}</React.Fragment>
    ),
  );
}

// A quiet inline note — the ONLY error surface this panel uses (F-11's lesson:
// in-viewport, next to the control that raised it, never a toast that can
// scroll out of frame). Mirrors permissions.tsx's own private Note.
function Note({ tone = "plain", children }: { tone?: "plain" | "red"; children: React.ReactNode }) {
  return (
    <p
      className={cn(
        "mt-2 max-w-[78ch] rounded-lg px-3 py-2 text-xs leading-relaxed",
        tone === "red" ? "bg-danger-subtle text-danger" : "bg-muted text-muted-foreground",
      )}
    >
      {children}
    </p>
  );
}

export type AccessLoadState = "loading" | "ready" | "sso_unavailable" | "fetch_failed";

// The mock draws PEOPLE.DELETE_CONFIRM's ONE frozen string split across an
// <h3> and a <p> (docs/design/people-access-mock/index.html's plain-delete
// inset). The title half is deterministic given `value` — sliced by its own
// length rather than a regex against the value, so an unusual value (one
// containing `"` or `?`) can never break the split.
function plainDeleteTitle(value: string): string {
  return `Delete the mapping for "${value}"?`;
}
function plainDeleteBody(value: string): string {
  return PEOPLE.DELETE_CONFIRM(value).slice(plainDeleteTitle(value).length).trim();
}

// ------------------------------------------------------------
// Write-error classification. Collision and posture-flip 400s are
// STRUCTURED (AccessCollisionError / AccessPostureFlipRequiredError,
// lib/api/access.ts) as of commit 544467ed — keyed directly off their typed
// fields, no client-side reconstruction. Lockout and the stale-snapshot
// refusal are still plain {"error":"..."} bodies, but both server strings
// are now stable canon (docs/design/people-access-prompt.md's "Post-
// adjudication canon additions") — matched EXACTLY, no `.includes`, so an
// unrelated 400 can never be misclassified as one of these two.
// ------------------------------------------------------------
type WriteErrorKind =
  | { kind: "posture_flip"; before: string; after: string }
  | { kind: "email_refused" }
  | { kind: "lockout" }
  | { kind: "stale_snapshot" }
  | { kind: "collision_chart"; value: string }
  | { kind: "collision_operator"; value: string }
  | { kind: "raw"; message: string };

// The server's exact raw messages (access.go's accessLockoutErr) — distinct
// from the FROZEN copy rendered for each (writeErrorNote below), same
// pattern LOCKOUT_ERROR already used pre-canon: the shipped copy is fuller
// than what the server actually says.
const LOCKOUT_MESSAGE = "this change would remove your own admin access (checked against your last sign-in)";
const STALE_SNAPSHOT_MESSAGE =
  "your sign-in is too old to verify this change — sign in again before changing role mappings";

function classifyWriteError(e: unknown): WriteErrorKind {
  if (e instanceof AccessPostureFlipRequiredError) {
    return { kind: "posture_flip", before: e.before, after: e.after };
  }
  if (e instanceof AccessCollisionError) {
    return e.cause === "chart"
      ? { kind: "collision_chart", value: e.value }
      : { kind: "collision_operator", value: e.value };
  }
  const message = getErrorMessage(e);
  if (message === ACCESS_ERROR.EMAIL_KEY_REFUSED) return { kind: "email_refused" };
  if (message === LOCKOUT_MESSAGE) return { kind: "lockout" };
  if (message === STALE_SNAPSHOT_MESSAGE) return { kind: "stale_snapshot" };
  return { kind: "raw", message };
}

function writeErrorNote(err: WriteErrorKind): React.ReactNode {
  switch (err.kind) {
    case "email_refused":
      return withMono(ACCESS_ERROR.EMAIL_KEY_REFUSED);
    case "lockout":
      return ACCESS_ERROR.LOCKOUT_ERROR;
    case "stale_snapshot":
      return ACCESS_ERROR.STALE_SNAPSHOT_ERROR;
    case "collision_chart":
      return withMono(ACCESS_ERROR.COLLISION_ERROR_CHART(err.value));
    case "collision_operator":
      return withMono(ACCESS_ERROR.COLLISION_ERROR_OPERATOR(err.value));
    case "raw":
      return err.message;
    case "posture_flip":
      return null; // handled by re-opening the guard dialog instead — see callers
  }
}

// ============================================================
// AccessPanel
// ============================================================
export function AccessPanel({
  access,
  state,
  onReload,
}: {
  access: AccessResponse | null;
  state: AccessLoadState;
  onReload: () => void;
}) {
  if (state === "loading") {
    return <p className="text-sm text-muted-foreground">Loading role mappings…</p>;
  }
  if (state === "sso_unavailable") {
    return (
      <EmptyState
        icon={ShieldOff}
        title={ACCESS_STATE.SSO_UNAVAILABLE_TITLE}
        description={ACCESS_STATE.SSO_UNAVAILABLE_BODY}
      />
    );
  }
  if (state === "fetch_failed" || !access) {
    return (
      <EmptyState
        icon={AlertTriangle}
        title={ACCESS_STATE.FETCH_FAILED_TITLE}
        description={ACCESS_STATE.FETCH_FAILED_BODY}
        action={
          <Button variant="outline" size="sm" onClick={onReload}>
            <RotateCw className="size-3.5" />
            {ACCESS_STATE.FETCH_FAILED_RETRY}
          </Button>
        }
      />
    );
  }

  return (
    <div className="space-y-6">
      <MappingsTable access={access} onReload={onReload} />
      <PreviewPanel mapEmpty={access.posture.map_empty} />
      <p className="flex max-w-[82ch] items-start gap-2 text-body text-muted-foreground">
        <Info className="mt-0.5 size-3.5 shrink-0" />
        <span>{withMono(PEOPLE.IDP_NOTE)}</span>
      </p>
    </div>
  );
}

// ------------------------------------------------------------
// Role mappings table + add form + Defaults block
// ------------------------------------------------------------
function MappingsTable({ access, onReload }: { access: AccessResponse; onReload: () => void }) {
  const [toDelete, setToDelete] = React.useState<AccessMapping | null>(null);
  const [busy, setBusy] = React.useState(false);
  const [dialogError, setDialogError] = React.useState<WriteErrorKind | null>(null);
  const [ack, setAck] = React.useState(false);
  // SERVER-AUTHORITATIVE, not a client pre-check (§2.2's own precedent, now
  // extended to the reverse posture-flip guard too): map_empty is REAL
  // merged-map emptiness, which can diverge from a raw row count whenever a
  // row is shadowed (§2.1/commit 544467ed) — a client-side "is this the last
  // row" guess can therefore be WRONG in either direction. Every delete goes
  // straight to the API; only a 400 carrying the structured posture-flip body
  // switches this dialog into guard mode, using ITS before/after.
  const [reactiveGuard, setReactiveGuard] = React.useState<{ before: string; after: string } | null>(null);
  const showDeleteGuard = !!reactiveGuard;

  const openDelete = (m: AccessMapping) => {
    setToDelete(m);
    setAck(false);
    setDialogError(null);
    setReactiveGuard(null);
  };
  const closeDelete = () => {
    setToDelete(null);
    setDialogError(null);
    setAck(false);
    setReactiveGuard(null);
  };

  const confirmDelete = async () => {
    if (!toDelete?.id) return;
    setBusy(true);
    try {
      await api.deleteMapping(toDelete.id, showDeleteGuard ? ack : false);
      closeDelete();
      onReload();
    } catch (e) {
      const classified = classifyWriteError(e);
      if (classified.kind === "posture_flip") {
        setReactiveGuard({ before: classified.before, after: classified.after });
        setAck(false);
      } else {
        setDialogError(classified);
      }
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="rounded-xl border border-border bg-card">
      <div className="px-6 pt-5">
        <h3 className="text-sm font-medium text-foreground">{PEOPLE.TABLE_TITLE}</h3>
        <p className="mt-0.5 text-body text-muted-foreground">{PEOPLE.TABLE_LEAD}</p>
        <Note>{PEOPLE.EFFECT_NOTE}</Note>
      </div>

      <div className="mt-4 flex flex-col gap-2">
        {access.mappings.length === 0 ? (
          <EmptyState icon={ShieldOff} title={PEOPLE.EMPTY_TITLE} description={PEOPLE.EMPTY_BODY} />
        ) : (
          access.mappings.map((m) => (
            <MappingCard
              key={`${m.source}:${m.id ?? m.value}`}
              mapping={m}
              allowEmailMappings={access.allow_email_mappings}
              emailDomainsConfigured={access.email_domains_configured}
              onDelete={() => openDelete(m)}
            />
          ))
        )}
      </div>

      <div className="border-t border-border px-6 py-5" data-testid="access-defaults">
        <h3 className="text-sm font-medium text-foreground">{PEOPLE.DEFAULTS_TITLE}</h3>
        <dl className="mt-3 grid grid-cols-[160px_1fr] gap-x-4 gap-y-1 text-sm">
          <dt className="text-muted-foreground">{PEOPLE.DEFAULT_ROLE_LABEL}</dt>
          <dd className="text-foreground">
            {access.default_role ? (
              <Chip tone="neutral">
                {access.default_role === "admin" ? PEOPLE.ROLE_ADMIN : PEOPLE.ROLE_MEMBER}
              </Chip>
            ) : (
              <span className="text-muted-foreground">{PEOPLE.DEFAULT_ROLE_UNSET}</span>
            )}
          </dd>
          <dd className="col-span-2 -mt-0.5 text-xs text-muted-foreground">{withMono(PEOPLE.DEFAULT_ROLE_HINT)}</dd>
          <dt className="mt-2 text-muted-foreground">{PEOPLE.OPERATOR_EMAILS_LABEL}</dt>
          <dd className="mt-2 text-foreground">
            {/* Real addresses (GET /access's operator_emails), per commit 544467ed —
                operator_emails_present stays a separate field but isn't needed here
                now that the list itself is on the wire. */}
            {(access.operator_emails ?? []).length > 0 ? (
              <span className="flex flex-wrap gap-x-2 gap-y-1 font-mono text-xs">
                {(access.operator_emails ?? []).map((email) => (
                  <Mono key={email} className="text-foreground">
                    {email}
                  </Mono>
                ))}
              </span>
            ) : (
              <span className="text-muted-foreground">{PEOPLE.OPERATOR_EMAILS_EMPTY}</span>
            )}
          </dd>
          <dd className="col-span-2 -mt-0.5 text-xs text-muted-foreground">
            {withMono(PEOPLE.OPERATOR_EMAILS_HINT)}
          </dd>
        </dl>
      </div>

      <AddMappingForm access={access} onReload={onReload} />

      <AlertDialog open={!!toDelete} onOpenChange={(o) => !o && closeDelete()}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>
              {showDeleteGuard ? GUARD.LAST_ROW_TITLE : plainDeleteTitle(toDelete?.value ?? "")}
            </AlertDialogTitle>
            <AlertDialogDescription>
              {reactiveGuard
                ? GUARD.LAST_ROW_BODY(reactiveGuard.after, reactiveGuard.before)
                : toDelete
                  ? plainDeleteBody(toDelete.value)
                  : ""}
            </AlertDialogDescription>
          </AlertDialogHeader>
          {showDeleteGuard && access.operator_emails_present && <p className="text-xs text-muted-foreground">{GUARD.ALLOWLIST_NOTE}</p>}
          {showDeleteGuard && (
            <label className="flex items-start gap-2 text-xs text-foreground">
              <input
                type="checkbox"
                className="mt-0.5"
                checked={ack}
                onChange={(e) => setAck(e.target.checked)}
              />
              {GUARD.GUARD_ACK_LABEL}
            </label>
          )}
          {dialogError && <Note tone="red">{writeErrorNote(dialogError)}</Note>}
          <AlertDialogFooter>
            <AlertDialogCancel>{PEOPLE.CANCEL}</AlertDialogCancel>
            <AlertDialogAction
              disabled={busy || (showDeleteGuard && !ack)}
              className="bg-danger text-danger-foreground hover:bg-danger/90"
              onClick={(e) => {
                e.preventDefault();
                void confirmDelete();
              }}
            >
              {busy ? <Loader2 className="size-4 animate-spin" /> : null}
              {showDeleteGuard ? GUARD.LAST_ROW_CONFIRM : PEOPLE.DELETE}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}

// MappingCard — one role mapping, self-contained (approved redesign,
// docs/design/people-access-mock/role-mappings-redesign.html): value + role on
// the top line, source/badges + timestamp + delete on the next, the note (when
// there is one) grouped INSIDE the card. Replaces the earlier table, whose
// per-row notes had to float full-width between rows and misaligned the columns.
function MappingCard({
  mapping: m,
  allowEmailMappings,
  emailDomainsConfigured,
  onDelete,
}: {
  mapping: AccessMapping;
  allowEmailMappings: boolean;
  emailDomainsConfigured: boolean;
  onDelete: () => void;
}) {
  const isEmail = m.value.includes("@");
  const showEmailBadge = m.source === "console" && isEmail && allowEmailMappings;
  // Exactly one note ever applies to a mapping.
  let note: React.ReactNode = null;
  if (m.source === "chart") note = PEOPLE.CHART_HINT;
  else if (m.shadowed && m.shadow_cause === "chart") note = withMono(PEOPLE.SHADOWED_BODY);
  else if (m.shadowed && m.shadow_cause === "operator_allowlist") note = withMono(PEOPLE.SHADOWED_OPERATOR_BODY);
  else if (showEmailBadge) note = withMono(PEOPLE.EMAIL_KEY_BODY(emailDomainsConfigured));
  // Amber-tint a mapping that carries a caution (a shadowed row, or an
  // unverified email key) so the whole card reads as "look here", not just its
  // chip. Chart/plain console rows stay neutral.
  const caution = m.shadowed || showEmailBadge;
  return (
    <div
      className={cn(
        "rounded-xl border p-4",
        caution ? "border-warning/30 bg-warning-subtle" : "border-border bg-card",
      )}
    >
      <div className="flex items-start justify-between gap-3">
        <Mono className="min-w-0 break-all text-foreground">{m.value}</Mono>
        <Chip tone="neutral">{m.role === "admin" ? PEOPLE.ROLE_ADMIN : PEOPLE.ROLE_MEMBER}</Chip>
      </div>
      <div className="mt-2 flex flex-wrap items-center gap-2">
        <Chip tone="neutral">{m.source === "chart" ? PEOPLE.SOURCE_CHART : PEOPLE.SOURCE_CONSOLE}</Chip>
        {m.shadowed && m.shadow_cause === "chart" && <Chip tone="warning">{PEOPLE.SHADOWED_BADGE}</Chip>}
        {m.shadowed && m.shadow_cause === "operator_allowlist" && (
          <Chip tone="warning">{PEOPLE.SHADOWED_OPERATOR_BADGE}</Chip>
        )}
        {showEmailBadge && <Chip tone="warning">{PEOPLE.EMAIL_KEY_BADGE}</Chip>}
        <div className="ml-auto flex items-center gap-3">
          <span className="whitespace-nowrap text-meta text-muted-foreground">
            {m.source === "chart" ? PEOPLE.ADDED_CHART_NA : m.created_at ? relativeTime(m.created_at) : PEOPLE.ADDED_CHART_NA}
          </span>
          {m.source === "console" && (
            <Button variant="ghost" size="sm" onClick={onDelete} aria-label={`${PEOPLE.DELETE} ${m.value}`}>
              {PEOPLE.DELETE}
            </Button>
          )}
        </div>
      </div>
      {note && <p className="mt-2 text-meta text-muted-foreground">{note}</p>}
    </div>
  );
}

// ------------------------------------------------------------
// Add-mapping form — the FIRST_ROW posture guard runs here, pre-emptively,
// using GET /access's own posture (before/after) rather than waiting for the
// server to 400 (which access.go documents as the SAME data, exposed early —
// see AccessPanel's header comment).
// ------------------------------------------------------------
function AddMappingForm({
  access,
  onReload,
}: {
  access: AccessResponse;
  onReload: () => void;
}) {
  const [value, setValue] = React.useState("");
  const [role, setRole] = React.useState<AccessRole>("admin");
  const [saving, setSaving] = React.useState(false);
  const [error, setError] = React.useState<WriteErrorKind | null>(null);
  const [guardOpen, setGuardOpen] = React.useState(false);
  const [ack, setAck] = React.useState(false);
  const [guardBeforeAfter, setGuardBeforeAfter] = React.useState<{ before: string; after: string } | null>(null);

  const wouldFlipPosture = access.posture.map_empty && access.posture.changes;

  const doSubmit = async (acknowledge: boolean) => {
    setSaving(true);
    setError(null);
    try {
      await api.upsertMapping({
        value: value.trim(),
        role,
        acknowledge_access_change: acknowledge || undefined,
      });
      setValue("");
      setGuardOpen(false);
      setAck(false);
      onReload();
    } catch (e) {
      const classified = classifyWriteError(e);
      if (classified.kind === "posture_flip") {
        setGuardBeforeAfter({ before: classified.before, after: classified.after });
        setGuardOpen(true);
      } else {
        setError(classified);
      }
    } finally {
      setSaving(false);
    }
  };

  const onAddClick = () => {
    if (!value.trim()) return;
    setError(null);
    if (wouldFlipPosture) {
      setGuardBeforeAfter({ before: access.posture.before, after: access.posture.after });
      setGuardOpen(true);
      setAck(false);
      return;
    }
    void doSubmit(false);
  };

  return (
    <div className="border-t border-border px-6 py-5">
      <h3 className="text-sm font-medium text-foreground">{PEOPLE.ADD_TITLE}</h3>
      <div className="mt-4 grid gap-4 md:grid-cols-[1fr_auto_auto]">
        <Field label={PEOPLE.FIELD_VALUE} htmlFor="access-value" hint={PEOPLE.VALUE_HINT} required>
          <Input
            id="access-value"
            value={value}
            onChange={(e) => setValue(e.target.value)}
            className="font-mono"
            autoComplete="off"
            spellCheck={false}
          />
        </Field>
        <Field label={PEOPLE.FIELD_ROLE}>
          <Segmented
            value={role}
            onChange={setRole}
            options={[
              { value: "admin", label: PEOPLE.ROLE_ADMIN },
              { value: "member", label: PEOPLE.ROLE_MEMBER },
            ]}
          />
        </Field>
        <div className="flex items-end">
          <Button onClick={onAddClick} disabled={saving || !value.trim()}>
            {saving ? <Loader2 className="size-4 animate-spin" /> : null}
            {PEOPLE.ADD_CTA}
          </Button>
        </div>
      </div>
      {error && <Note tone="red">{writeErrorNote(error)}</Note>}

      <AlertDialog open={guardOpen} onOpenChange={(o) => !o && setGuardOpen(false)}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>{GUARD.FIRST_ROW_TITLE}</AlertDialogTitle>
            <AlertDialogDescription>
              {guardBeforeAfter ? GUARD.FIRST_ROW_BODY(guardBeforeAfter.before, guardBeforeAfter.after) : ""}
            </AlertDialogDescription>
          </AlertDialogHeader>
          {access.operator_emails_present && <p className="text-xs text-muted-foreground">{GUARD.ALLOWLIST_NOTE}</p>}
          <label className="flex items-start gap-2 text-xs text-foreground">
            <input type="checkbox" className="mt-0.5" checked={ack} onChange={(e) => setAck(e.target.checked)} />
            {GUARD.GUARD_ACK_LABEL}
          </label>
          {error && <Note tone="red">{writeErrorNote(error)}</Note>}
          <AlertDialogFooter>
            <AlertDialogCancel onClick={() => setGuardOpen(false)}>{PEOPLE.CANCEL}</AlertDialogCancel>
            <AlertDialogAction
              disabled={!ack || saving}
              onClick={(e) => {
                e.preventDefault();
                void doSubmit(true);
              }}
            >
              {saving ? <Loader2 className="size-4 animate-spin" /> : null}
              {GUARD.FIRST_ROW_CONFIRM}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}

// ------------------------------------------------------------
// Preview panel — a dry run, never saved (POST /access/preview).
// ------------------------------------------------------------
function PreviewPanel({ mapEmpty }: { mapEmpty: boolean }) {
  const [claims, setClaims] = React.useState("");
  const [running, setRunning] = React.useState(false);
  const [result, setResult] = React.useState<React.ReactNode | null>(null);

  const run = async (useSession: boolean) => {
    setRunning(true);
    setResult(null);
    try {
      const lines = claims
        .split("\n")
        .map((l) => l.trim())
        .filter(Boolean);
      const email = lines.find((l) => l.includes("@"));
      const res = useSession
        ? await api.previewRole({ use_session: true })
        : await api.previewRole({ roles: lines, groups: lines, email });
      setResult(renderPreviewResult(res));
    } catch {
      setResult(PREVIEW.RESULT_UNKNOWN);
    } finally {
      setRunning(false);
    }
  };

  // RESULT_LEGACY (§7.5, §2.1 arm 1) is the distinct EMPTY-combined-map arm —
  // ok=true with nothing matched there comes from HasOperatorEmails()/
  // emailInList alone, PreviewRole never reaching DefaultRole() at all — so it
  // must not be conflated with RESULT_DEFAULT, which names "your default role"
  // and is only true once a non-empty map's fallthrough decided it.
  function renderPreviewResult(res: Awaited<ReturnType<typeof api.previewRole>>): React.ReactNode {
    if (res.error) return PREVIEW.RESULT_UNKNOWN;
    const roleLabel = res.role === "admin" ? "admin" : "member";
    if (res.ok && res.matched.length > 0) {
      return PREVIEW.RESULT_MATCHED(roleLabel, res.matched.map((m) => m.value).join(", "));
    }
    if (res.ok) return mapEmpty ? PREVIEW.RESULT_LEGACY(roleLabel) : PREVIEW.RESULT_DEFAULT(roleLabel);
    return PREVIEW.RESULT_DENIED;
  }

  return (
    <div className="rounded-xl border border-border bg-card px-6 py-5">
      <h3 className="text-sm font-medium text-foreground">{PREVIEW.TITLE}</h3>
      <p className="mt-1.5 text-body text-muted-foreground">
        {PREVIEW.LEAD_PREFIX}
        <button
          type="button"
          className="text-muted-foreground underline underline-offset-2 hover:text-foreground"
          onClick={() => void run(true)}
          disabled={running}
        >
          {PREVIEW.OWN_SESSION_CTA}
        </button>
        {PREVIEW.LEAD_SUFFIX}
      </p>
      <div className="mt-4 grid gap-4 md:grid-cols-[1fr_auto]">
        <Field label={PREVIEW.FIELD_CLAIMS} htmlFor="access-preview-claims" hint={PREVIEW.FIELD_CLAIMS_HINT}>
          <Textarea
            id="access-preview-claims"
            value={claims}
            onChange={(e) => setClaims(e.target.value)}
            className="font-mono"
            rows={3}
          />
        </Field>
        <div className="flex items-end">
          <Button variant="outline" onClick={() => void run(false)} disabled={running || !claims.trim()}>
            {running ? <Loader2 className="size-4 animate-spin" /> : null}
            {PREVIEW.RUN_CTA}
          </Button>
        </div>
      </div>
      {result && <Note>{result}</Note>}
    </div>
  );
}

// Re-exported so unit tests can exercise the classification/render helpers
// without going through the whole component tree.
export { classifyWriteError, writeErrorNote };
export type { WriteErrorKind };
