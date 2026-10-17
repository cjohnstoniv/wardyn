/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Setup still needed: the server's `access.setup`, each item with the door
// that finishes it here. What is left when the person goes on becomes a Review
// blocker that links back to its row.

import * as React from "react";
import { Check, TriangleAlert } from "lucide-react";
import { ADO } from "../../../../../lib/ado-entra-copy";
import { ADO_PAT } from "../../../../../lib/ado-pat-copy";
import { secrets } from "../../../../../lib/api/secrets";
import { useAdoConnect } from "../../../../../lib/hooks/use-ado-connect";
import { getErrorMessage } from "../../../../../lib/format";
import type { SetupItem } from "../../../../../lib/types/preflight";
import { Button } from "../../../../ui/button";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "../../../../ui/dialog";
import { Input } from "../../../../ui/input";
import { Label } from "../../../../ui/label";
import { cn } from "../../../../ui/utils";
import { useModelAccessDoor, useShellSetupStatus } from "../../../../wardyn/model-access-context";
import { NR_SHARED } from "../../../../wardyn/copy/new-run/shared";
import { NR_TOOLS } from "../../../../wardyn/copy/new-run/tools";
import { Chip } from "../../../../wardyn/primitives";
import type { RunIntent } from "../../draft/run-intent";
import { providerControlId, setupDoor, setupRowId, signInProvider, type HarnessNames, type SetupDoor } from "../../lenses/tools-lens";
import { useAnnounce } from "../step-shell";

const DOOR_LABEL: Record<SetupDoor, string> = { sign_in: NR_TOOLS.SETUP_SIGN_IN, choose: NR_TOOLS.SETUP_CHOOSE, add: NR_TOOLS.SETUP_ADD, connect: ADO_PAT.MEMBER_CONNECT };

/** One saved secret's value, pasted once. The value is write-only: nothing is shown back. */
function AddTokenDialog({ item, onClose, onSaved }: { item: SetupItem; onClose: () => void; onSaved: () => void }) {
  const inputId = React.useId();
  const [value, setValue] = React.useState("");
  const [busy, setBusy] = React.useState(false);
  const [error, setError] = React.useState("");
  const name = item.fix?.secret_name ?? "";

  const save = async () => {
    setBusy(true);
    setError("");
    try {
      await secrets.setSecret(name, value);
      onSaved();
    } catch (e) {
      setError(getErrorMessage(e));
      setBusy(false);
    }
  };

  return (
    <Dialog open onOpenChange={(open) => !open && !busy && onClose()}>
      <DialogContent onOpenAutoFocus={(e) => { e.preventDefault(); document.getElementById(inputId)?.focus(); }}>
        <DialogHeader>
          <DialogTitle>{item.label}</DialogTitle>
          <DialogDescription>{NR_TOOLS.TOKEN_INTRO}</DialogDescription>
        </DialogHeader>
        <div className="flex flex-col gap-2">
          <Label htmlFor={inputId}>{NR_TOOLS.TOKEN_LABEL}</Label>
          <Input
            id={inputId}
            type="password"
            autoComplete="off"
            value={value}
            aria-invalid={error ? true : undefined}
            onChange={(e) => setValue(e.target.value)}
            onKeyDown={(e) => { if (e.key === "Enter" && value.trim() && !busy) void save(); }}
          />
          {error && <p role="alert" className="text-sm text-danger">{error}</p>}
        </div>
        <DialogFooter>
          <Button type="button" variant="ghost" disabled={busy} onClick={onClose}>{NR_SHARED.CANCEL}</Button>
          <Button type="button" disabled={busy || !value.trim()} onClick={() => void save()}>{NR_TOOLS.TOKEN_CONFIRM}</Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

export interface SetupNeededProps {
  items: readonly SetupItem[];
  /** Ids finished here that wait for the server's next answer: no door, no verdict yet. */
  pending: ReadonlySet<string>;
  /** Ids the server's answer now reports satisfied. */
  done: ReadonlySet<string>;
  intent: RunIntent;
  names?: HarnessNames;
  /** The server has answered exactly this body. While it has not, the last all-clear is held, not claimed. Default true. */
  answered?: boolean;
  /** A door finished: the item is Done and the server is asked again. */
  onDone: (id: string) => void;
}

export function SetupNeeded({ items, pending, done, intent, names, answered = true, onDone }: SetupNeededProps) {
  const headingId = React.useId();
  const announce = useAnnounce();
  const door = useModelAccessDoor();
  const { status } = useShellSetupStatus();
  const [adding, setAdding] = React.useState<SetupItem | null>(null);
  const ado = useAdoConnect();

  const providerName = (id: string) => status?.model_providers?.find((p) => p.id === id)?.name;
  const sentence = (item: SetupItem): string => {
    const harness = item.fix?.action === "choose_model_provider" ? names?.get(item.fix.tool ?? "") : undefined;
    if (harness) return NR_TOOLS.SETUP_CHOOSE_PROVIDER(harness);
    const provider = item.fix?.action === "sign_in" ? providerName(signInProvider(intent, item) ?? "") : undefined;
    return provider ? NR_TOOLS.SETUP_SIGN_IN_TO(provider) : item.label;
  };
  // The server names a harness's setup by its id until the shell's names load: say nothing rather than the id.
  const requiredBy = (item: SetupItem): string | undefined => names?.get(item.required_by) ?? (item.kind === "llm_access" ? undefined : item.required_by);

  // The connect popup resolves true once the person's own state reads live; a blocked popup (null) leaves the fallback link.
  const connectAdo = async (id: string, connect = ado.connect) => {
    if (await connect()) { announce(NR_TOOLS.SIGNED_IN); onDone(id); }
  };

  const open = (item: SetupItem, kind: SetupDoor, trigger: HTMLElement) => {
    if (kind === "add") setAdding(item);
    else if (kind === "choose") document.getElementById(providerControlId(item.fix?.tool ?? ""))?.focus();
    else if (kind === "connect") void connectAdo(item.id);
    else {
      const provider = signInProvider(intent, item);
      if (provider) {
        door.openDoor({ for: { provider }, returnTo: trigger, onSignedIn: () => { announce(NR_TOOLS.SIGNED_IN); onDone(item.id); } });
      }
    }
  };

  return (
    <section aria-labelledby={headingId} className="flex flex-col gap-3.5 rounded-xl border border-border bg-card p-[18px]">
      <div>
        <h2 id={headingId} className="font-semibold">{NR_TOOLS.SETUP_TITLE}</h2>
        <p className="text-sm text-muted-foreground">{NR_TOOLS.SETUP_BODY}</p>
      </div>
      {items.length === 0 ? (
        answered && <p className="flex items-center gap-2 text-sm text-muted-foreground"><Check aria-hidden className="size-4 text-success" />{NR_TOOLS.SETUP_ALL_SET}</p>
      ) : (
        <ul className="flex flex-col gap-2">
          {items.map((item) => {
            const finished = done.has(item.id);
            const doorKind = setupDoor(item);
            const kind = pending.has(item.id) || (doorKind === "sign_in" && !signInProvider(intent, item)) ? undefined : doorKind;
            return (
              <li key={item.id} id={setupRowId(item)} tabIndex={-1} className={cn("flex flex-wrap items-center gap-3 rounded-lg border p-3 outline-none", finished ? "border-border bg-surface-2" : "border-warning bg-warning-subtle")}>
                {finished ? <Check aria-hidden className="size-4 text-success" /> : <TriangleAlert aria-hidden className="size-4 text-warning" />}
                <span className="min-w-0 flex-1 [overflow-wrap:anywhere]">
                  <strong className="font-medium">{sentence(item)}</strong>{" "}
                  <span className="text-sm text-muted-foreground">{requiredBy(item)}</span>
                </span>
                {finished ? <Chip tone="success">{NR_TOOLS.SETUP_DONE}</Chip> : kind && (
                  <Button type="button" variant="outline" size="sm" disabled={kind === "connect" && ado.connecting} onClick={(e) => open(item, kind, e.currentTarget)}>{DOOR_LABEL[kind]}</Button>
                )}
                {!finished && kind === "connect" && ado.blockedUrl && (
                  <p className="basis-full text-sm text-muted-foreground">
                    {ADO.CONNECT_POPUP_BLOCKED}{" "}
                    <a href={ado.blockedUrl} target="_blank" rel="noopener noreferrer" className="underline" onClick={() => void connectAdo(item.id, ado.connectFallback)}>{ADO.CONNECT_POPUP_OPEN}</a>
                  </p>
                )}
              </li>
            );
          })}
        </ul>
      )}
      {adding && (
        <AddTokenDialog
          item={adding}
          onClose={() => setAdding(null)}
          onSaved={() => { announce(NR_TOOLS.TOKEN_SAVED); onDone(adding.id); setAdding(null); }}
        />
      )}
    </section>
  );
}
