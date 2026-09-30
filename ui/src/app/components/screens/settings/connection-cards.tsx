/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Lane/SecretLane/HostSummary primitives the Workspace Providers Git tab
// reuses for ITS credential lanes (screens/providers/git-tab.tsx).
//
// This file also used to carry GitHostCard, the Git host card: its free-text
// Host field could store a git-pat-<slug> for a host no provider admitted, a
// credential that clones nothing. The three git Lanes it rendered move INTO a
// provider row on /providers instead; this file keeps the shared shell
// (Lane/SecretLane/HostSummary) EXPORTED rather than re-typed there.
//
// The Model provider card that used to live here held the deployment's own
// model credentials; since 0.8 (#548) none of those credential a run, and
// model access is a model provider (Admin Settings' Model providers list,
// each person's own connection on Your account), so the card is retired.
import * as React from "react";
import { toast } from "sonner";
import { Check, Loader2 } from "lucide-react";
import { secrets as secretsApi } from "../../../lib/api/secrets";
import { getErrorMessage } from "../../../lib/format";
import { Button } from "../../ui/button";
import { Input } from "../../ui/input";
import { Field } from "../../wardyn/form-primitives";
import { Mono } from "../../wardyn/code-block";
import { cn } from "../../ui/utils";

// Canon strings, reviewed against the settings mock. Kept here
// rather than in lib/integrations.ts's T, which belongs to the page being
// deleted and shrinks with it.
export const S = {
  // The three lanes differ in WHERE the credential goes, and the footer owns
  // that split so no lane has to overclaim: only the App lane keeps the token
  // outside the sandbox (proxy broker); a PAT or SSH key enters it for the
  // clone — helper stdout and a shredded 0400 file respectively
  // (ARCHITECTURE.md invariant 1 and the git-egress table are the source).
  STORE_NOTE: "Wardyn stores this — it doesn't dial the provider to check it.",
  GIT_FOOTER:
    "Only the GitHub App lane keeps its token outside the sandbox — a PAT or SSH key enters it for the clone, then is wiped. Public repos clone with no credential at all.",
} as const;

// Lane rows.

// One lane's RADIO button only: it must never also render the expanded form
// (`children`) INSIDE itself, or a selected lane's Input + Save button ends
// up nested inside the `role="radiogroup"` DOM subtree — an ARIA violation
// (a radiogroup's children must be `role="radio"` nodes, never a form). The
// caller renders the active lane's body separately with LaneBody below,
// OUTSIDE the radiogroup. Radio semantics
// (not aria-pressed) because these are mutually-exclusive choices within one
// group, which is what a screen reader needs to announce "2 of 3". `tabIndex`
// / `radioRef` are the roving-tabindex wiring (wardyn/use-roving-radio.ts) —
// optional so a caller with no group (there is none today) still compiles.
export function Lane({
  id,
  title,
  hint,
  connected,
  connectedDetail,
  selected,
  onSelect,
  disabled,
  tabIndex,
  radioRef,
}: {
  id: string;
  title: string;
  hint: string;
  connected: boolean;
  connectedDetail?: string;
  selected: boolean;
  onSelect: () => void;
  /** Not selectable, and so never expandable — the Git tab passes this when the
   *  row names no host to key a credential by (git-tab.tsx's `host`). A lane
   *  that cannot be opened cannot Save, which is the point: the alternative was
   *  a Save that wrote the secret of a DIFFERENT host. */
  disabled?: boolean;
  tabIndex?: number;
  radioRef?: (el: HTMLButtonElement | null) => void;
}) {
  return (
    <div
      className={cn(
        "rounded-lg border transition-colors",
        selected ? "border-primary bg-primary/5" : "border-border",
        disabled && "opacity-60",
      )}
    >
      <button
        type="button"
        role="radio"
        aria-checked={selected}
        id={id}
        disabled={disabled}
        onClick={onSelect}
        tabIndex={tabIndex}
        ref={radioRef}
        className="flex w-full items-start gap-2.5 p-3 text-left"
      >
        <span
          aria-hidden="true"
          className={cn(
            "mt-0.5 grid size-4 shrink-0 place-items-center rounded-full border",
            selected ? "border-primary" : "border-border-strong",
          )}
        >
          {selected && <span className="size-2 rounded-full bg-primary" />}
        </span>
        <span className="min-w-0 flex-1">
          <span className="flex items-center gap-1.5">
            <span className="text-sm font-medium text-foreground">{title}</span>
            {connected && (
              <span className="inline-flex items-center gap-0.5 rounded-md border border-ok/25 bg-ok-subtle px-1.5 py-px text-meta font-medium text-ok">
                <Check className="size-2.5" />
                Connected
              </span>
            )}
          </span>
          <span className="mt-0.5 block text-meta leading-snug text-muted-foreground">
            {connected && connectedDetail ? connectedDetail : hint}
          </span>
        </span>
      </button>
    </div>
  );
}

// The selected lane's expanded form — a SIBLING of the radiogroup, never a
// descendant (see Lane's own note). One rounded card, the same visual
// language `Lane` itself used to carry the body in.
export function LaneBody({ children }: { children: React.ReactNode }) {
  return <div className="rounded-lg border border-border px-3 py-3">{children}</div>;
}

// A one-secret lane form: a single write-only value + Save, and Disconnect once
// stored. Every key/token lane in both cards is this shape.
export function SecretLane({
  label,
  placeholder,
  hint,
  secretName,
  stored,
  onChanged,
  extra,
  summary,
  disabled,
  saveVariant = "default",
}: {
  label: string;
  placeholder: string;
  hint?: React.ReactNode;
  secretName: string;
  stored: boolean;
  onChanged: () => void;
  /** Rendered above the value field while editing (e.g. the Host input). */
  extra?: React.ReactNode;
  /** Rendered INSTEAD of the form once stored — the facts worth keeping on
   *  screen (which host, which secret name) without an input to mistake for
   *  unsaved work. */
  summary?: React.ReactNode;
  disabled?: boolean;
  /** Save/Save replacement's Button variant. Default "default" (teal) keeps
   *  the original lane look unchanged; the
   *  Workspace Providers Git tab passes "secondary" — that screen's one teal
   *  is its own Save providers button (CONSOLE-RULES §6), so a lane's own
   *  Save must not compete with it. */
  saveVariant?: "default" | "secondary";
}) {
  // Two lanes can share a secret name (two rows on one host), never a DOM id.
  const uid = React.useId();
  const [value, setValue] = React.useState("");
  const [busy, setBusy] = React.useState(false);
  // Only ever true after an explicit Replace: a stored secret is write-only, so
  // the field starts hidden rather than empty-and-ambiguous.
  const [editing, setEditing] = React.useState(false);

  const save = async () => {
    setBusy(true);
    try {
      await secretsApi.setSecret(secretName, value.trim());
      setValue("");
      setEditing(false);
      toast.success(`Saved ${secretName}`);
      onChanged();
    } catch (e) {
      toast.error(getErrorMessage(e));
    } finally {
      setBusy(false);
    }
  };

  const disconnect = async () => {
    setBusy(true);
    try {
      await secretsApi.deleteSecret(secretName);
      toast.success(`Removed ${secretName}`);
      onChanged();
    } catch (e) {
      toast.error(getErrorMessage(e));
    } finally {
      setBusy(false);
    }
  };

  // A STORED secret shows a summary, not an empty box. The box was read as
  // "your key didn't save" — the value is write-only, so there is nothing to
  // prefill it with, and an always-visible empty field next to a "Connected"
  // badge is a straight contradiction. Replace reveals it deliberately.
  if (stored && !editing) {
    return (
      <div className="space-y-2">
        {summary}
        <div className="flex items-center gap-2">
          <Button size="sm" variant="secondary" disabled={disabled} onClick={() => setEditing(true)}>
            Replace
          </Button>
          <Button size="sm" variant="ghost" disabled={disabled || busy} onClick={disconnect}>
            {busy && <Loader2 className="size-3.5 animate-spin" />}
            Disconnect
          </Button>
          <span className="text-meta text-muted-foreground">
            Stored as <Mono>{secretName}</Mono>
          </span>
        </div>
      </div>
    );
  }

  return (
    <div className="space-y-3">
      {extra}
      <Field label={label} htmlFor={`${uid}v-${secretName}`} hint={hint}>
        <Input
          id={`${uid}v-${secretName}`}
          type="password"
          autoComplete="off"
          value={value}
          placeholder={placeholder}
          disabled={disabled || busy}
          onChange={(e) => setValue(e.target.value)}
        />
      </Field>
      <div className="flex items-center gap-2">
        <Button size="sm" variant={saveVariant} disabled={disabled || busy || !value.trim()} onClick={save}>
          {busy && <Loader2 className="size-3.5 animate-spin" />}
          {stored ? "Save replacement" : "Save"}
        </Button>
        {stored && (
          <Button
            size="sm"
            variant="ghost"
            disabled={busy}
            onClick={() => {
              setValue("");
              setEditing(false);
            }}
          >
            Cancel
          </Button>
        )}
        {/* #355: this branch also renders the UNSTORED form (stored=false —
            the early return above only covers stored && !editing), where
            nothing is stored yet. "Stored as <name>" here read as "already
            stored" beside an empty Save button. Only the Replace flow
            (stored && editing) has a name worth naming ahead of Save. */}
        {stored && (
          <span className="text-meta text-muted-foreground">
            Replaces <Mono>{secretName}</Mono>
          </span>
        )}
      </div>
    </div>
  );
}

// The one fact a stored git credential still needs on screen: which host it
// clones. (The secret name rides on the Replace/Disconnect row below it.)
export function HostSummary({ host }: { host: string }) {
  return (
    <p className="text-body text-muted-foreground">
      Clones <Mono>{host}</Mono> over an injected credential — the value itself is write-only and never read back.
    </p>
  );
}

// GitHostCard is retired — see the file header. Its Lane/SecretLane/
// HostSummary shell lives above, exported; the row shell and the free-text
// Host field are screens/providers/git-tab.tsx's now (the row's own host,
// never a second text field).
