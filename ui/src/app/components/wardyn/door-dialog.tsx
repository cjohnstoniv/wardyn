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
// Two shapes, one dialog:
//  · signin — a bedrock_sso or anthropic_subscription provider's sign-in
//    (/model-providers/{id}/sign-in), framed as packet E draws it.
//  · key — a typed key or token for a provider (/model-providers/{id}/credential).

import * as React from "react";
import { Loader2, Lock, TriangleAlert } from "lucide-react";

import { Button } from "../ui/button";
import { Input } from "../ui/input";
import { Dialog, DialogContent, DialogDescription, DialogTitle } from "../ui/dialog";
import type { HarnessLoginPaneHandle } from "../screens/settings/harness-login-pane";
import { modelProviderCredentials } from "../../lib/api/model-provider-credentials";
import { getErrorMessage } from "../../lib/format";
import type { DoorTarget } from "../../lib/model-access";
import type { SetupModelProvider, SetupStatus } from "../../lib/types";
import { MODEL_ACCESS_BANNER } from "./model-access-copy";
import { CLAUDE_DOOR, CRED_NOTICE, DOOR, KEY_DOOR, REMOVE_CONFIRM, WRITE_ONLY } from "./copy/door";

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

function doorTitle(t: DoorTarget, confirmingRemove: boolean): string {
  if (t.kind === "key") {
    // The remove confirm gets its OWN title (packet F §3) —
    // not the Add/Replace title behind it, which the confirm has replaced.
    if (confirmingRemove) return REMOVE_CONFIRM.TITLE(t.token, providerName(t.provider));
    // A Replace open (a credential is already stored) gets its own title
    // (packet F §1) — the Add title would claim there is nothing there yet.
    const title = t.stored ? KEY_DOOR.TITLE_REPLACE : KEY_DOOR.TITLE;
    return title(t.token, providerName(t.provider));
  }
  return t.login === "aws" ? MODEL_ACCESS_BANNER.DIALOG_TITLE : CLAUDE_DOOR.TITLE;
}

// credNoticeLine2 is the key door's notice's second line (packet F §1),
// keyed off /setup/status's credential_storage (design F-3): which kind of
// store holds the value, named for a person (F-4) — never a host, path or
// vault name. Undefined (an older daemon, or the read hasn't resolved yet)
// reads as local, the safest default: it claims no external store that may
// not exist.
function credNoticeLine2(storage: SetupStatus["credential_storage"]): string {
  switch (storage) {
    case "key_service":
      return CRED_NOTICE.KEY_SERVICE("Vault");
    case "vault":
      return CRED_NOTICE.KEK("Vault");
    case "key_vault":
      return CRED_NOTICE.KEK("Key Vault");
    default:
      return CRED_NOTICE.LOCAL;
  }
}

/** The toast a completed sign-in or save shows (CONSOLE-RULES §9's transient
 *  case: the strip vanishing is not a confirmation). */
export function doorToast(t: DoorTarget): string {
  if (t.kind === "key") return KEY_DOOR.SAVED_TOAST(t.token);
  return t.login === "aws" ? MODEL_ACCESS_BANNER.SIGNED_IN_TOAST : CLAUDE_DOOR.SIGNED_IN_TOAST;
}

function SignInHeader({ target }: { target: DoorTarget & { kind: "signin" } }) {
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
  credentialStorage,
  confirmingRemove,
  setConfirmingRemove,
  onCancel,
  onSaved,
  onRemoved,
}: {
  target: DoorTarget & { kind: "key" };
  /** /setup/status's credential_storage (design F-3) — which store-mode line
   *  the notice's second line shows (F-4). */
  credentialStorage: SetupStatus["credential_storage"];
  /** Lifted to DoorDialog: the confirm needs its OWN
   *  DialogTitle, which only the parent can set. */
  confirmingRemove: boolean;
  setConfirmingRemove: (v: boolean) => void;
  onCancel: () => void;
  onSaved: () => void;
  /** Fires with the toast the confirm earns (packet F §3). */
  onRemoved: (toast: string) => void;
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
  // Default focus: Cancel (packet F §3) — the confirm's
  // OWN title now carries REMOVE_CONFIRM.TITLE (doorTitle, DoorDialog).
  // Unconditional (Rules of Hooks): KeyDoor returns two different subtrees
  // below, so a hook cannot live inside either branch.
  const cancelRef = React.useRef<HTMLButtonElement>(null);
  React.useEffect(() => {
    if (confirmingRemove) cancelRef.current?.focus();
  }, [confirmingRemove]);

  if (confirmingRemove) {
    return (
      <div className="space-y-3">
        <p className="text-body text-muted-foreground">{removeConfirmBody(credentialStorage)}</p>
        <p className="text-body text-muted-foreground">{REMOVE_CONFIRM.UPSTREAM(target.provider.host)}</p>
        {error && (
          <p role="alert" className="flex items-start gap-2 text-body text-danger">
            <TriangleAlert className="mt-0.5 size-4 shrink-0" />
            {error}
          </p>
        )}
        <div className="flex justify-end gap-2">
          <Button ref={cancelRef} type="button" variant="ghost" size="sm" disabled={busy} onClick={() => setConfirmingRemove(false)}>
            {REMOVE_CONFIRM.CANCEL}
          </Button>
          <Button
            type="button"
            variant="destructive"
            size="sm"
            disabled={busy}
            onClick={() =>
              void run(
                () => modelProviderCredentials.deleteCredential(id),
                () => onRemoved(REMOVE_CONFIRM.REMOVED_TOAST(target.token)),
              )
            }
          >
            {busy && <Loader2 className="size-3.5 animate-spin" />}
            {REMOVE_CONFIRM.CONFIRM}
          </Button>
        </div>
      </div>
    );
  }

  return (
    <form
      className="space-y-3"
      onSubmit={(e) => {
        e.preventDefault();
        if (value.trim()) void run(() => modelProviderCredentials.putCredential(id, value.trim()), onSaved);
      }}
    >
      <div className="space-y-1.5">
        <div className="flex items-center justify-between gap-2">
          <label htmlFor="key-door-value" className="block text-body font-medium text-foreground">
            {KEY_DOOR.FIELD(target.token)}
          </label>
          <span
            title={WRITE_ONLY.TOOLTIP}
            className="inline-flex items-center gap-1 rounded-full bg-muted px-2 py-0.5 text-meta text-muted-foreground"
          >
            <Lock className="size-3" aria-hidden />
            {WRITE_ONLY.CHIP}
          </span>
        </div>
        <Input
          id="key-door-value"
          type="password"
          autoComplete="off"
          value={value}
          onChange={(e) => setValue(e.target.value)}
          className="font-mono"
        />
        {/* A stored value is never shown, dots or otherwise — Wardyn has no
            route that could read it back (packet F §1 b). */}
        {target.stored && (
          <p className="text-body text-muted-foreground">{CRED_NOTICE.STORED_HINT(target.token)}</p>
        )}
      </div>
      {error && (
        <p role="alert" className="flex items-start gap-2 text-body text-danger">
          <TriangleAlert className="mt-0.5 size-4 shrink-0" />
          {error}
        </p>
      )}
      <p className="text-body text-muted-foreground">{KEY_DOOR.DESTINATION(target.provider.host)}</p>
      {/* Packet E's refused state drops the storage note for the refusal —
          the notice gives way to it, same rule as here (packet F §1 d). */}
      {!error && (
        <div className="space-y-1">
          <DialogDescription className="text-body">{KEY_DOOR.NOTE}</DialogDescription>
          <p className="text-body text-muted-foreground">{credNoticeLine2(credentialStorage)}</p>
          <p className="text-body text-muted-foreground">{CRED_NOTICE.ADMINS}</p>
        </div>
      )}
      <div className="flex flex-wrap items-center gap-2">
        {target.stored && (
          <Button type="button" variant="ghost" size="sm" disabled={busy} onClick={() => setConfirmingRemove(true)}>
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

// removeConfirmBody is the remove-confirm's retention line (packet F §3):
// which kind of store, keyed off the SAME credential_storage the notice's
// line 2 reads — key_service still retains locally (no external store holds
// the value), so it takes the local wording, same split as CRED_NOTICE's own
// LOCAL/KEK.
function removeConfirmBody(storage: SetupStatus["credential_storage"]): string {
  switch (storage) {
    case "vault":
      return REMOVE_CONFIRM.BODY_VAULT;
    case "key_vault":
      return REMOVE_CONFIRM.BODY_KEY_VAULT;
    default:
      return REMOVE_CONFIRM.BODY;
  }
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
  credentialStorage,
  focusSeq,
  onCancel,
  onDone,
  onRemoved,
  onCloseAutoFocus,
}: {
  target: DoorTarget | null;
  /** /setup/status's credential_storage (design F-3) — the key door's
   *  store-mode notice line and remove-confirm retention line key off it.
   *  Undefined reads as local, the same default credNoticeLine2 takes. */
  credentialStorage?: SetupStatus["credential_storage"];
  focusSeq: number;
  onCancel: () => void;
  /** A completed sign-in or save, with the toast it earns. */
  onDone: (toast: string) => void;
  /** A completed remove, with the toast it earns (packet F §3). */
  onRemoved: (toast: string) => void;
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

  // Lifted out of KeyDoor: the confirm needs its OWN
  // DialogTitle, which only THIS component can set (KeyDoor sits below it).
  //
  // Keyed on TARGET, not `t`: `t` falls back to `shown.current`
  // once the dialog closes, so it never actually becomes null and never
  // changes on a same-provider reopen — the effect below then never re-ran,
  // and a closed confirm (Escape, or a completed Remove) stuck the NEXT open
  // of the same provider on "Remove your token for …?". `target` genuinely
  // goes null while closed, so this resets both then and on any subsequent
  // open (same provider or a different one).
  const [confirmingRemove, setConfirmingRemove] = React.useState(false);
  const targetKeyDoorId = target?.kind === "key" ? target.provider.id : null;
  React.useEffect(() => {
    setConfirmingRemove(false);
  }, [targetKeyDoorId]);

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
        {t && <DialogTitle>{doorTitle(t, confirmingRemove)}</DialogTitle>}
        {t?.kind === "key" && target && (
          <KeyDoor
            key={t.provider.id}
            target={t}
            credentialStorage={credentialStorage}
            confirmingRemove={confirmingRemove}
            setConfirmingRemove={setConfirmingRemove}
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
                modelProvider={signIn.provider.id}
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
