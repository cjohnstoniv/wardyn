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
import { secrets as secretsApi } from "../../../lib/api/secrets";
import { HttpError } from "../../../lib/api/core";

export function YourModelKey({
  llmReady,
  mine,
  variant,
  known = true,
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
  // Called after a successful Save/Remove so the parent refetches `mine`.
  onChanged: () => void;
}) {
  const [editing, setEditing] = React.useState(false);
  const [revealEmpty, setRevealEmpty] = React.useState(false);
  const [value, setValue] = React.useState("");
  const [busy, setBusy] = React.useState(false);
  const [error, setError] = React.useState<string | null>(null);

  const hasOwn = mine?.includes(T.SECRET_NAME) ?? false;
  const done = known && (hasOwn || (llmReady && !hasOwn));

  const save = async () => {
    setBusy(true);
    setError(null);
    try {
      await secretsApi.setSecret(T.SECRET_NAME, value);
      setValue("");
      setEditing(false);
      setRevealEmpty(false);
      onChanged();
    } catch (e) {
      setError(e instanceof HttpError && e.status === 400 ? T.REFUSED_SHORT : "Couldn't save this key.");
    } finally {
      setBusy(false);
    }
  };

  const remove = async () => {
    setBusy(true);
    setError(null);
    try {
      await secretsApi.deleteSecret(T.SECRET_NAME);
      onChanged();
    } catch {
      setError("Couldn't remove this key.");
    } finally {
      setBusy(false);
    }
  };

  const showEmptyForm = !hasOwn && (revealEmpty || !llmReady);
  // The header carries exactly one chip (CONSOLE-RULES: no second chip in the
  // body of a done section): the generic "Done" when the member holds their
  // own key, "Provided by your admin" when the admin's covers them instead —
  // never both, and none while the empty form is showing.
  const headerChip = showEmptyForm || !known ? undefined : hasOwn ? (
    <DoneChip />
  ) : done ? (
    <Chip tone="success">{T.PROVIDED_CHIP}</Chip>
  ) : undefined;

  return (
    <SectionCard title="Your model key" right={headerChip}>
      {hasOwn ? (
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
        <EmptyForm value={value} setValue={setValue} error={error} busy={busy} variant={variant} onSave={save} />
      ) : (
        <div>
          <p className="text-sm text-muted-foreground">{T.PROVIDED_BODY}</p>
          <Button variant="link" size="sm" className="mt-1 h-auto p-0" onClick={() => setRevealEmpty(true)}>
            {T.USE_OWN_KEY}
          </Button>
        </div>
      )}
    </SectionCard>
  );
}

function EmptyForm({
  value,
  setValue,
  error,
  busy,
  variant,
  onSave,
  onCancel,
}: {
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
      <Mono className="mt-2 block text-xs">{T.SECRET_NAME}</Mono>
      <Input
        type="password"
        placeholder="sk-ant-…"
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
