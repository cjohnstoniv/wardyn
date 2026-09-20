/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Member Getting Started's "Your model key" section (6c BYOK) — bring your
// own `anthropic-api-key` when your admin hasn't provided one. Four states:
// empty (nothing set, nothing provided) / set (your own row exists) /
// provided (your admin's model access covers you) / refused (the 400 from
// secretmask.MinLen). `mine` is owned by the PARENT (member-getting-started.tsx)
// and passed down — it also drives that page's "Model access" summary chip,
// so there is one fetch and one source of truth; a save/remove here calls
// `onChanged()` to make the parent refetch rather than re-fetching locally
// (two independent copies of `mine` is how "Provided by your admin" survived
// a Save, and "Your key" survived a Remove).
import * as React from "react";
import { Button } from "../../ui/button";
import { Input } from "../../ui/input";
import { Chip, DoneChip, SectionCard } from "../../wardyn/primitives";
import { Mono } from "../../wardyn/code-block";
import { YOUR_MODEL_KEY as T } from "../../wardyn/copy";
import { AGENTS, modelAccessChipBare } from "../../../lib/workspace-providers-copy";
import type { SetupHarnessTool, SetupModelAccess } from "../../../lib/types";
import { secrets as secretsApi } from "../../../lib/api/secrets";
import { HttpError } from "../../../lib/api/core";
import { modelKeyState } from "./model-key-state";

// X3-F3 — which provider's key this member should store. The roster is the
// org's answer to "which coding agents may a run name"; a row this pane knows
// how to store a key for wins, in roster (catalog) order, so a deployment that
// offers Claude Code keeps today's anthropic name and a codex-only one stops
// asking for a key its harness can never use. No roster (an older daemon, or a
// legacy open install) falls back to anthropic, exactly as before.
export function modelKeyProvider(harnesses?: SetupHarnessTool[]) {
  const known = Object.keys(T.BY_AGENT);
  const row = (harnesses ?? []).find((h) => h.enabled !== false && known.includes(h.id));
  return {
    ...T.BY_AGENT[(row?.id ?? "claude-code") as keyof typeof T.BY_AGENT],
    // Appendix A finding 2 — the governing row's declared lane, undefined
    // when it falls back to the default with no row at all. "per_user" is
    // the only value model-key-state.ts branches on.
    credentialSource: row?.credential_source,
    // The SAME row's declared mechanism, for ownKeyApplies: a "bedrock_"-
    // prefixed mechanism refuses a member's own API key even under a SHARED
    // (non-per_user) row.
    mechanism: row?.mechanism,
  };
}

export function YourModelKey({
  llmReady,
  mine,
  variant,
  known = true,
  harnesses,
  modelAccess,
  onSignInAws,
  signInOpen = false,
  onChanged,
}: {
  llmReady: boolean;
  // null while the parent's fetch is in flight — treated as "no own key yet"
  // (same fail-closed default as everywhere else on this page).
  mine: string[] | null;
  variant: "default" | "outline";
  // The page's own setup-status fetch can be unreachable — a fact `mine`
  // (a DIFFERENT endpoint) has no way to see. `known = false` suppresses the
  // done chip regardless of what `mine` says: "nothing on the page is marked
  // done" while the daemon can't be reached is a page-wide guarantee, not a
  // per-section one.
  known?: boolean;
  /** The org's agent roster (SetupStatus.harnesses) — see modelKeyProvider.
   *  Absent/empty keeps the anthropic name every existing caller had. */
  harnesses?: SetupHarnessTool[];
  /** THIS PRINCIPAL's model-access state (Appendix A finding 2) — read
   *  instead of `llmReady` whenever the governing harness row is per_user;
   *  see model-key-state.ts's total truth table. */
  modelAccess?: SetupModelAccess;
  /** Opens the SAME HarnessLoginPane the chip row above already mounts (one
   *  pane, two buttons) — required whenever a per_user state is actionable. */
  onSignInAws?: () => void;
  /** U-13 (a11y): whether that pane is already open. It mounts in a DIFFERENT
   *  card above this one and moves no focus, so a second enabled button saying
   *  the same words is a no-op offering itself as a way in. Defaulted, so a
   *  caller that does not own the pane's state is unchanged. */
  signInOpen?: boolean;
  // Called after a successful Save/Remove so the parent refetches `mine`.
  onChanged: () => void;
}) {
  const provider = modelKeyProvider(harnesses);
  const [editing, setEditing] = React.useState(false);
  const [revealEmpty, setRevealEmpty] = React.useState(false);
  const [value, setValue] = React.useState("");
  const [busy, setBusy] = React.useState(false);
  const [error, setError] = React.useState<string | null>(null);

  const hasOwn = mine?.includes(provider.secretName) ?? false;
  const state = modelKeyState({
    hasOwn,
    llmReady,
    modelAccess,
    credentialSource: provider.credentialSource,
    mechanism: provider.mechanism,
  });

  const save = async () => {
    setBusy(true);
    setError(null);
    try {
      await secretsApi.setSecret(provider.secretName, value);
      setValue("");
      setEditing(false);
      setRevealEmpty(false);
      onChanged();
    } catch (e) {
      setError(e instanceof HttpError && e.status === 400 ? T.REFUSED_SHORT : T.SAVE_ERROR);
    } finally {
      setBusy(false);
    }
  };

  const remove = async () => {
    setBusy(true);
    setError(null);
    try {
      await secretsApi.deleteSecret(provider.secretName);
      onChanged();
    } catch {
      setError(T.REMOVE_ERROR);
    } finally {
      setBusy(false);
    }
  };

  // Appendix A finding 2 — governing row is NOT per_user AND (revealEmpty OR
  // result is unknown). Under per_user this is ALWAYS false: a member's own
  // API key can never satisfy a bedrock_sso lane, so the form never shows.
  const showEmptyForm = state.revealAllowed && (revealEmpty || state.result === "unknown");
  // U-10 (W6 blind lens) — `expired_signin` and `not_configured` both grade
  // `not_signed_in`, but they are not the same fact: the chip row above says
  // "Model access · Signed out" over a card saying "Not signed in" and a body
  // saying "Nothing is configured for you until you do", while the server's own
  // action line on the same page says a stored session exists. One label per
  // state — this one taken from the SAME table the chip row reads.
  const expiredSignIn = state.result === "not_signed_in" && modelAccess?.state === "expired_signin";
  // The header carries exactly one chip (CONSOLE-RULES: no second chip in the
  // body of a done section) — never while the empty form is showing, and
  // never a claim `known=false` (an unreachable page) can't back.
  const headerChip = showEmptyForm || !known ? undefined : (
    {
      own: <DoneChip />,
      signed_in: <Chip tone="success">{T.SIGNED_IN_CHIP}</Chip>,
      expiring: <Chip tone="warning">{T.EXPIRING_CHIP}</Chip>,
      not_signed_in: (
        <Chip tone="warning">
          {expiredSignIn ? modelAccessChipBare("expired_signin") : T.NOT_SIGNED_IN_CHIP}
        </Chip>
      ),
      // U-15: bare inside this card — the heading already names the subject.
      shared_expired: <Chip tone="warning">{modelAccessChipBare("shared_expired")}</Chip>,
      provided: <Chip tone="success">{T.PROVIDED_CHIP}</Chip>,
      unknown: undefined,
    }[state.result]
  );

  return (
    <SectionCard title="Your model key" right={headerChip}>
      {state.result === "own" ? (
        <div>
          <div className="flex items-center gap-2">
            <Mono className="text-sm">••••••••••••</Mono>
          </div>
          <div className="mt-2 flex gap-2">
            <Button
              variant="outline"
              size="sm"
              onClick={() => {
                setEditing(true);
                setError(null);
              }}
            >
              Rotate
            </Button>
            <Button variant="ghost" size="sm" onClick={remove} disabled={busy}>
              Remove
            </Button>
          </div>
          <p className="mt-2 text-xs text-muted-foreground">{T.SET_HINT}</p>
          {!editing && error && <p className="mt-1 text-xs text-danger">{error}</p>}
          {editing && (
            <EmptyForm
              provider={provider}
              value={value}
              setValue={setValue}
              error={error}
              busy={busy}
              variant={variant}
              onSave={save}
              onCancel={() => {
                setEditing(false);
                setError(null);
              }}
            />
          )}
        </div>
      ) : showEmptyForm ? (
        <EmptyForm provider={provider} value={value} setValue={setValue} error={error} busy={busy} variant={variant} onSave={save} />
      ) : state.result === "signed_in" ? (
        <p className="text-sm text-muted-foreground">{T.SIGNED_IN_BODY}</p>
      ) : state.result === "expiring" ? (
        <div>
          <p className="text-sm text-muted-foreground">{T.SIGNED_IN_BODY}</p>
          <SignInAwsButton variant={variant} open={signInOpen} onSignInAws={onSignInAws} />
        </div>
      ) : state.result === "not_signed_in" ? (
        <div>
          {/* U-10: an expired session is not "nothing is configured" — something
              IS stored for this member and it stopped working. */}
          <p className="text-sm text-muted-foreground">
            {expiredSignIn ? T.EXPIRED_SIGNIN_BODY : T.NOT_SIGNED_IN_BODY}
          </p>
          <SignInAwsButton variant={variant} open={signInOpen} onSignInAws={onSignInAws} />
        </div>
      ) : state.result === "shared_expired" ? (
        <div>
          <p className="text-sm text-muted-foreground">{T.SHARED_EXPIRED_BODY}</p>
          {/* Hidden under shared_bedrock: a member's own key can never
              satisfy a Bedrock mechanism even on a shared row. */}
          {state.revealAllowed && (
            <Button variant="link" size="sm" className="mt-1 h-auto p-0" onClick={() => setRevealEmpty(true)}>
              {T.USE_OWN_KEY}
            </Button>
          )}
        </div>
      ) : state.result === "provided" ? (
        <div>
          <p className="text-sm text-muted-foreground">{T.PROVIDED_BODY}</p>
          {state.revealAllowed && (
            <Button variant="link" size="sm" className="mt-1 h-auto p-0" onClick={() => setRevealEmpty(true)}>
              {T.USE_OWN_KEY}
            </Button>
          )}
        </div>
      ) : (
        // result === "unknown" and showEmptyForm is unreachable here
        // (ownKeyApplies(row) is false in both bands that land on this
        // branch): a per_user row with a state the card takes no action on
        // (band "per_user" — not_applicable is the common real case, the
        // admin-token principal on an enabled per_user row), or a shared row
        // whose mechanism is Bedrock but llmReady is false (band
        // "shared_bedrock"). A neutral sentence per band, never a bare title.
        //
        // U-14 (W6 blind lens): under per_user that sentence is the
        // `not_applicable` ANSWER ("this is a shared token, not a person") —
        // it must never also be the fallthrough for an absent or unrecognised
        // state, or a member whose state simply could not be read reads as if
        // there is nothing to set up, beside a lede saying model access uses
        // their own AWS sign-in. An unknown state claims nothing at all.
        unknownBody(state.band, modelAccess?.state) && (
          <p className="text-sm text-muted-foreground">{unknownBody(state.band, modelAccess?.state)}</p>
        )
      )}
    </SectionCard>
  );
}

// The `unknown` result's body, or undefined when the card must claim nothing
// (U-14). Split out so the branch above stays one expression.
function unknownBody(band: string, state?: string): string | undefined {
  if (band !== "per_user") return T.ADMIN_NOT_READY_BODY;
  return state === "not_applicable" ? T.PER_PERSON_NA_BODY : undefined;
}

// The card's own "Sign in to AWS" (U-13). It opens the SAME pane the chip row's
// button does — which mounts in the card ABOVE this one — so while that pane is
// open this button is hidden rather than left enabled as a silent no-op, and its
// accessible name says which of the two it is.
function SignInAwsButton({
  variant,
  open,
  onSignInAws,
}: {
  variant: "default" | "outline";
  open: boolean;
  onSignInAws?: () => void;
}) {
  if (open) return null;
  return (
    <Button
      variant={variant}
      size="sm"
      className="mt-3"
      aria-label={T.SIGN_IN_AWS_ARIA_CARD}
      onClick={onSignInAws}
    >
      {AGENTS.SIGN_IN_AWS}
    </Button>
  );
}

function EmptyForm({
  provider,
  value,
  setValue,
  error,
  busy,
  variant,
  onSave,
  onCancel,
}: {
  provider: (typeof T.BY_AGENT)[keyof typeof T.BY_AGENT];
  value: string;
  setValue: (v: string) => void;
  error: string | null;
  busy: boolean;
  variant: "default" | "outline";
  onSave: () => void;
  // Only passed for a Rotate-in-progress — the genuinely empty state has
  // nothing to cancel back to.
  onCancel?: () => void;
}) {
  return (
    <div className="mt-2">
      <p className="text-sm text-muted-foreground">{T.EMPTY_BODY}</p>
      <Mono className="mt-2 block text-xs">{provider.secretName}</Mono>
      <Input
        type="password"
        placeholder={provider.placeholder}
        value={value}
        onChange={(e) => setValue(e.target.value)}
        aria-invalid={!!error}
        className="mt-2"
      />
      {error && <p className="mt-1 text-xs text-danger">{error}</p>}
      <div className="mt-2 flex gap-2">
        <Button variant={variant} size="sm" onClick={onSave} disabled={busy || value.length === 0}>
          Save key
        </Button>
        {onCancel && (
          <Button variant="ghost" size="sm" onClick={onCancel} disabled={busy}>
            Cancel
          </Button>
        )}
      </div>
    </div>
  );
}
