/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// THE door (#544, design §5.9, packet MP-E): the one sign-in / key dialog the
// console mounts, keyed by the provider it was opened for. The strip mounts it
// once (model-access-banner.tsx) and every entrance — the strip, Settings, the
// Agents tab, Getting started, the rail, the failure block, a held approval —
// opens it through the context; none mounts a pane of its own.
//
// Three shapes, one dialog:
//  · legacy — today's door (/setup/harness-*), unchanged: the only door on an
//    install with no model providers, and in the Admin view.
//  · signin — a bedrock_sso or anthropic_subscription provider's sign-in
//    (/model-providers/{id}/sign-in), framed as packet E draws it.
//  · key — a typed key or token for a provider (/model-providers/{id}/credential).

import * as React from "react";
import { Loader2, TriangleAlert } from "lucide-react";

import { Button } from "../ui/button";
import { Input } from "../ui/input";
import { Dialog, DialogContent, DialogDescription, DialogTitle } from "../ui/dialog";
import type { HarnessLoginPaneHandle } from "../screens/settings/harness-login-pane";
import { modelProviderCredentials } from "../../lib/api/model-provider-credentials";
import { getErrorMessage } from "../../lib/format";
import type { DoorTarget } from "../../lib/model-access";
import type { SetupModelProvider } from "../../lib/types";
import { MODEL_ACCESS_BANNER } from "./model-access-copy";
import { CLAUDE_DOOR, DOOR, KEY_DOOR } from "./copy/door";

// Lazy, and that is a gate rather than a nicety: the strip that mounts this is
// in the shell, and the login pane drags xterm + addon-fit + its stylesheet
// behind it — bundle-split.test.ts fails on a static import. The chunk is
// fetched when somebody opens a sign-in door.
const HarnessLoginPane = React.lazy(() =>
  import("../screens/settings/harness-login-pane").then((m) => ({ default: m.HarnessLoginPane })),
);

// The id focus goes to when an entrance asks for the door while it is already
// open (context focusSeq): DialogContent takes no ref in React 18.
const DOOR_ID = "model-access-door";

const providerName = (p: SetupModelProvider) => p.name || p.id;

function doorTitle(t: DoorTarget): string {
  if (t.kind === "key") return KEY_DOOR.TITLE(t.token, providerName(t.provider));
  return t.login === "aws" ? MODEL_ACCESS_BANNER.DIALOG_TITLE : CLAUDE_DOOR.TITLE;
}

/** The toast a completed sign-in or save shows (CONSOLE-RULES §9's transient
 *  case: the strip vanishing is not a confirmation). */
export function doorToast(t: DoorTarget): string {
  if (t.kind === "key") return KEY_DOOR.SAVED_TOAST(t.token);
  return t.login === "aws" ? MODEL_ACCESS_BANNER.SIGNED_IN_TOAST : CLAUDE_DOOR.SIGNED_IN_TOAST;
}

function SignInHeader({ target }: { target: DoorTarget & { kind: "legacy" | "signin" } }) {
  if (target.kind === "legacy") {
    // Today's door, as it was: the title alone, the description for a screen
    // reader before the terminal starts writing.
    return (
      <DialogDescription className="sr-only">
        {target.login === "aws" ? MODEL_ACCESS_BANNER.DIALOG_DESCRIPTION : CLAUDE_DOOR.DESCRIPTION}
      </DialogDescription>
    );
  }
  const forLine = DOOR.FOR(providerName(target.provider));
  return (
    <div className="space-y-1 text-body text-muted-foreground">
      {target.login === "aws" ? (
        <DialogDescription className="text-body">{forLine}</DialogDescription>
      ) : (
        <>
          <p>{forLine}</p>
          <DialogDescription className="text-body">{CLAUDE_DOOR.DESCRIPTION}</DialogDescription>
        </>
      )}
      <p>{MODEL_ACCESS_BANNER.DIALOG_CLEANUP_NOTE}</p>
    </div>
  );
}

/** The key or token door: one field, where it goes (D7), and how it is kept. */
function KeyDoor({
  target,
  onCancel,
  onSaved,
  onRemoved,
}: {
  target: DoorTarget & { kind: "key" };
  onCancel: () => void;
  onSaved: () => void;
  onRemoved: () => void;
}) {
  const [value, setValue] = React.useState("");
  const [error, setError] = React.useState("");
  const [busy, setBusy] = React.useState(false);
  const id = target.provider.id;
  const run = async (write: () => Promise<void>, after: () => void) => {
    setBusy(true);
    setError("");
    try {
      await write();
      after();
    } catch (e) {
      // The server's refusal, verbatim — Wardyn never dials the provider to
      // test a key, so this is the only verdict there is.
      setError(getErrorMessage(e));
      setBusy(false);
    }
  };
  return (
    <form
      className="space-y-3"
      onSubmit={(e) => {
        e.preventDefault();
        if (value.trim()) void run(() => modelProviderCredentials.putCredential(id, value.trim()), onSaved);
      }}
    >
      <div className="space-y-1.5">
        <label htmlFor="key-door-value" className="block text-body font-medium text-foreground">
          {KEY_DOOR.FIELD(target.token)}
        </label>
        <Input
          id="key-door-value"
          type="password"
          autoComplete="off"
          value={value}
          onChange={(e) => setValue(e.target.value)}
          className="font-mono"
        />
      </div>
      {error && (
        <p role="alert" className="flex items-start gap-2 text-body text-danger">
          <TriangleAlert className="mt-0.5 size-4 shrink-0" />
          {error}
        </p>
      )}
      <p className="text-body text-muted-foreground">{KEY_DOOR.DESTINATION(target.provider.host)}</p>
      {/* Packet E's refused state drops the storage note for the refusal. */}
      {!error && <DialogDescription className="text-body">{KEY_DOOR.NOTE}</DialogDescription>}
      <div className="flex flex-wrap items-center gap-2">
        {target.stored && (
          <Button
            type="button"
            variant="ghost"
            size="sm"
            disabled={busy}
            onClick={() => void run(() => modelProviderCredentials.deleteCredential(id), onRemoved)}
          >
            {KEY_DOOR.REMOVE}
          </Button>
        )}
        <div className="ml-auto flex gap-2">
          <Button type="button" variant="outline" size="sm" disabled={busy} onClick={onCancel}>
            {KEY_DOOR.CANCEL}
          </Button>
          <Button type="submit" size="sm" disabled={busy || !value.trim()}>
            {busy && <Loader2 className="size-3.5 animate-spin" />}
            {KEY_DOOR.SAVE}
          </Button>
        </div>
      </div>
    </form>
  );
}

/**
 * DoorDialog renders the door for `target`, closed while it is null.
 *
 * The sign-in shapes carry connection-cards.tsx's old style override, for its
 * reasons: DialogContent's own `sm:max-w-lg` wins the cascade against any
 * class, `translate`/`transform` are separate CSS properties in Tailwind v4 (a
 * transformed ancestor also traps the terminal's `fixed` fullscreen), and
 * `min-w-0` lets the 512-column login terminal shrink inside the grid.
 */
export function DoorDialog({
  target,
  perUser,
  focusSeq,
  onCancel,
  onDone,
  onRemoved,
  onCloseAutoFocus,
}: {
  target: DoorTarget | null;
  /** Today's AWS door only: the org's access portal is stored (a per_user
   *  claude-code row), so the pane asks for none. */
  perUser: boolean;
  focusSeq: number;
  onCancel: () => void;
  /** A completed sign-in or save, with the toast it earns. */
  onDone: (toast: string) => void;
  onRemoved: () => void;
  /** Where focus goes when the dialog closes — the one callback that fires
   *  after Radix's FocusScope has let go (see model-access-banner.tsx). */
  onCloseAutoFocus: (event: Event) => void;
}) {
  const paneRef = React.useRef<HarnessLoginPaneHandle>(null);
  // What the door showed, kept through the close animation so the title does
  // not blank while the dialog fades out.
  const shown = React.useRef(target);
  if (target) shown.current = target;
  const t = target ?? shown.current;

  // A second entrance while the door is open: no second door, the open one
  // takes focus (context openDoor).
  React.useEffect(() => {
    if (focusSeq > 0) document.getElementById(DOOR_ID)?.focus();
  }, [focusSeq]);

  const signIn = t && t.kind !== "key" ? t : null;
  return (
    <Dialog
      open={target !== null}
      onOpenChange={(next) => {
        if (next) return;
        // Escape and an overlay click close the parent, and the pane's
        // onCancel is child-to-parent: without routing through the pane's own
        // handle a dismissal would orphan a live sign-in run for up to 30
        // minutes.
        if (paneRef.current) paneRef.current.cancel();
        else onCancel();
      }}
    >
      <DialogContent
        id={DOOR_ID}
        onCloseAutoFocus={onCloseAutoFocus}
        className={signIn ? "scroll-thin inset-0 top-0 left-0 m-auto h-fit max-h-[92vh] overflow-y-auto" : undefined}
        style={
          signIn
            ? { width: "min(96vw, 72rem)", maxWidth: "min(96vw, 72rem)", translate: "none", transform: "none" }
            : undefined
        }
      >
        {t && <DialogTitle>{doorTitle(t)}</DialogTitle>}
        {t?.kind === "key" && target && (
          <KeyDoor
            key={t.provider.id}
            target={t}
            onCancel={onCancel}
            onSaved={() => onDone(doorToast(t))}
            onRemoved={onRemoved}
          />
        )}
        {signIn && <SignInHeader target={signIn} />}
        {signIn && target && (
          <div className="min-w-0">
            {/* The same mark + spinner App.tsx's RouteFallback shows for a
                lazy route: a chunk in flight reads as the console still
                connecting, never as a broken dialog. */}
            <React.Suspense
              fallback={
                <div className="flex min-h-[8rem] items-center justify-center" role="status" aria-live="polite">
                  <Loader2 className="size-5 animate-spin text-muted-foreground" />
                  <span className="sr-only">Loading…</span>
                </div>
              }
            >
              <HarnessLoginPane
                provider={signIn.login}
                modelProvider={signIn.kind === "signin" ? signIn.provider.id : undefined}
                // A provider's portal is on the provider; today's door knows
                // the org's only under a per_user row (the rule agents-tab.tsx
                // and connection-cards.tsx followed when each mounted a pane).
                startURLManaged={signIn.kind === "signin" || perUser}
                paneRef={paneRef}
                onDone={() => onDone(doorToast(signIn))}
                onCancel={onCancel}
              />
            </React.Suspense>
          </div>
        )}
      </DialogContent>
    </Dialog>
  );
}
