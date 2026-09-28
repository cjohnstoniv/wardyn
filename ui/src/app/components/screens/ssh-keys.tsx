/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// SSH keys management: the signed-in human's OWN registered public keys —
// list, add, remove. Not operator-gated: an SSH key is a personal credential
// binding, not deployment configuration (the server enforces the same scope
// independently — sshkeys.go's handlers key every read/write off the
// caller's own principal).
//
// M-5 (#636): /ssh-keys is gone (deleted with no alias) — SshKeysPane is now
// mounted once, in Your account (your-account-screen.tsx). AddSSHKeyDialog
// and RemoveSSHKeyDialog are exported so admin-ssh-keys-card.tsx's "Admin SSH
// keys" card (S-1) can reuse the same add/remove dialogs rather than a second
// copy — the server stamps a key's role off the caller's session, so the two
// cards differ only in which keys they list, never in how a key is added or
// removed.
import * as React from "react";
import { KeyRound, Plus, Trash2 } from "lucide-react";
import { toast } from "sonner";
import { sshKeys as sshKeysApi } from "../../lib/api/ssh-keys";
import { getErrorMessage } from "../../lib/format";
import type { SSHPublicKey } from "../../lib/types";
import { Button } from "../ui/button";
import { Input } from "../ui/input";
import { Textarea } from "../ui/textarea";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "../ui/table";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "../ui/alert-dialog";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "../ui/dialog";
import { Field } from "../wardyn/form-primitives";
import { Mono } from "../wardyn/code-block";
import { Chip } from "../wardyn/primitives";
import { EmptyState, ErrorState, STATES, TableSkeleton } from "../wardyn/states";
import { PageHeader } from "../wardyn/page-header";
import { CollapsibleCard } from "../wardyn/collapsible-card";
import { absoluteTime, relativeTime } from "../../lib/format";
import { capabilityAllowed, useMyCapabilities } from "../../lib/capabilities";
import { DENIED } from "../../lib/permissions-copy";
import { useOperator } from "../wardyn/operator-context";

const SSH_KEYS_DESCRIPTION =
  "Public keys only — Wardyn never stores or asks for a private key. Keys are yours alone; admins can't list anyone else's.";

export function SshKeysPane({ heading = "h1" }: { heading?: "h1" | "h3" } = {}) {
  const [keys, setKeys] = React.useState<SSHPublicKey[]>([]);
  const [status, setStatus] = React.useState<"loading" | "error" | "ready">("loading");
  const [addOpen, setAddOpen] = React.useState(false);
  const [toDelete, setToDelete] = React.useState<SSHPublicKey | null>(null);
  // Advisory: the server's `feature` check is the wall (handleAddSSHKey). A
  // super admin is exempt there, so the set is not even fetched for one.
  const operator = useOperator();
  const caps = useMyCapabilities(!operator);
  const addBlocked = !capabilityAllowed(caps, "feature", "ssh_key");

  const load = React.useCallback(() => {
    setStatus("loading");
    sshKeysApi
      .listKeys()
      .then((k) => {
        setKeys(k);
        setStatus("ready");
      })
      .catch(() => setStatus("error"));
  }, []);
  React.useEffect(load, [load]);

  // #1200 compact cards — one line, absent while unloaded. "0 keys", not the
  // EmptyState's own "No keys yet." — both render at once once expanded, and
  // a query for that canon string must still resolve to a single node. The
  // error case reuses ErrorState's own default heading (review L4) rather
  // than inventing a new "Couldn't load" phrase the packet never named.
  const summary =
    status === "loading" ? undefined : status === "error" ? STATES.ERROR_TITLE : `${keys.length} ${keys.length === 1 ? "key" : "keys"}`;

  // review L2 — nested inside CollapsibleCard on Your account (h3), this
  // table wrapper must not repeat the card's own border+bg-card (CONSOLE-
  // RULES §9 "never nest a card in a card"); admin-ssh-keys-card.tsx's own
  // table wrapper sets the same precedent. The standalone page (h1, no live
  // route) keeps the full card treatment, since nothing wraps it.
  const tableWrapperClass =
    heading === "h3" ? "overflow-hidden rounded-lg border border-border" : "overflow-hidden rounded-xl border border-border bg-card";

  const table = (
    <>
      {addBlocked && (
        <p role="status" className="mb-3 text-sm text-muted-foreground">
          {DENIED.SSH_KEY_FEATURE}
        </p>
      )}
      <div className={tableWrapperClass}>
        {status === "loading" ? (
          <TableSkeleton rows={3} cols={4} />
        ) : status === "error" ? (
          <ErrorState onRetry={load} />
        ) : keys.length === 0 ? (
          <EmptyState
            icon={KeyRound}
            title="No keys yet."
            description="Add your public key to connect over SSH."
            action={
              <Button onClick={() => setAddOpen(true)} disabled={addBlocked}>
                <Plus className="size-4" /> Add key
              </Button>
            }
          />
        ) : (
          <Table>
            <TableHeader>
              <TableRow className="hover:bg-transparent">
                <TableHead>Name</TableHead>
                <TableHead>Fingerprint</TableHead>
                <TableHead>Added</TableHead>
                <TableHead className="w-[44px]" />
              </TableRow>
            </TableHeader>
            <TableBody>
              {keys.map((k) => (
                <TableRow key={k.fingerprint}>
                  <TableCell>
                    <span className="inline-flex items-center gap-2">
                      <KeyRound className="size-3.5 text-cyan" />
                      {k.name || <span className="text-muted-foreground">(unnamed)</span>}
                      {/* k.role is the KEY's stamped role, NOT the viewer's
                          session role: it means exactly "this key reaches runs
                          you do not own" and can only ever be admin or member
                          (internal/api/sshkeys.go never stamps security_admin,
                          deliberately). Unchanged by the three-tier model. */}
                      {k.role === "admin" && (
                        <Chip
                          tone="warning"
                          title="Registered while you were an admin, so this key reaches runs you do not own. Refreshed automatically each time you sign in, and expires on its own if you don't; delete and re-register the key to drop the override immediately."
                        >
                          Admin override
                        </Chip>
                      )}
                      {/* #584: added in the user view, so capped at member
                          rights for good (docs/SSH.md §Bounds). Frozen strings:
                          docs/design/admin-access-canon.md. M-5 (#636, S-2):
                          "Member access" pointed at a door M-5 removed (the
                          user view was the only place a member could add ANY
                          key); rewritten to "User access", tooltip pointing
                          at S-1's new Admin SSH keys card instead. */}
                      {k.capped && (
                        <Chip title="Added in the user view, so it keeps user rights. To reach other people's runs over SSH, add a key in Settings in the admin view.">
                          User access
                        </Chip>
                      )}
                    </span>
                  </TableCell>
                  <TableCell>
                    <Mono className="text-muted-foreground" title={k.fingerprint}>
                      {k.fingerprint}
                    </Mono>
                  </TableCell>
                  <TableCell>
                    <span className="text-xs text-muted-foreground" title={absoluteTime(k.created_at)}>
                      {relativeTime(k.created_at)}
                    </span>
                  </TableCell>
                  <TableCell onClick={(e) => e.stopPropagation()}>
                    <Button
                      variant="ghost"
                      size="icon"
                      className="size-8 text-muted-foreground hover:text-danger"
                      aria-label={`Remove key ${k.name || k.fingerprint}`}
                      onClick={() => setToDelete(k)}
                    >
                      <Trash2 className="size-4" />
                    </Button>
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        )}
      </div>
    </>
  );

  const dialogs = (
    <>
      <AddSSHKeyDialog open={addOpen} onOpenChange={setAddOpen} onAdded={load} />
      <RemoveSSHKeyDialog keyToDelete={toDelete} onOpenChange={(o) => !o && setToDelete(null)} onRemoved={load} />
    </>
  );

  // #1200 compact cards — Your account's card form. The bare page (heading
  // "h1") has no live route any more (M-5 deleted /ssh-keys with no alias)
  // but stays exactly as it rendered before: nothing collapses a page.
  if (heading === "h3") {
    return (
      <>
        <CollapsibleCard title="Your SSH keys" summary={summary} testId="ssh-keys-pane">
          <p className="text-body leading-snug text-muted-foreground">{SSH_KEYS_DESCRIPTION}</p>
          <div className="mt-3">
            <Button variant="outline" size="sm" onClick={() => setAddOpen(true)} disabled={addBlocked}>
              <Plus className="size-4" /> Add key
            </Button>
          </div>
          <div className="mt-3">{table}</div>
        </CollapsibleCard>
        {dialogs}
      </>
    );
  }

  return (
    <div>
      <PageHeader
        as={heading}
        title="Your SSH keys"
        description={SSH_KEYS_DESCRIPTION}
        actions={
          <Button onClick={() => setAddOpen(true)} disabled={addBlocked}>
            <Plus className="size-4" /> Add key
          </Button>
        }
      />
      {table}
      {dialogs}
    </div>
  );
}

export function AddSSHKeyDialog({
  open,
  onOpenChange,
  onAdded,
}: {
  open: boolean;
  onOpenChange: (o: boolean) => void;
  onAdded: () => void;
}) {
  const [name, setName] = React.useState("");
  const [publicKey, setPublicKey] = React.useState("");
  const [error, setError] = React.useState<string | null>(null);
  const [saving, setSaving] = React.useState(false);

  React.useEffect(() => {
    if (open) {
      setName("");
      setPublicKey("");
      setError(null);
      setSaving(false);
    }
  }, [open]);

  const save = async () => {
    setError(null);
    if (!publicKey.trim()) {
      setError("Paste your public key.");
      return;
    }
    setSaving(true);
    try {
      await sshKeysApi.addKey(name.trim(), publicKey.trim());
      onOpenChange(false);
      onAdded();
      toast.success("SSH key added");
    } catch (e) {
      setError(getErrorMessage(e) || "Failed to add key.");
    } finally {
      setSaving(false);
    }
  };

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>Add key</DialogTitle>
          <DialogDescription>Public key only — never paste a private key.</DialogDescription>
        </DialogHeader>

        <div className="space-y-4 py-1">
          <Field label="Name" htmlFor="ssh-key-name" hint="Something to recognize this key by later, e.g. the machine it's on.">
            <Input
              id="ssh-key-name"
              placeholder="laptop"
              value={name}
              onChange={(e) => setName(e.target.value)}
              autoComplete="off"
            />
          </Field>
          <Field label="Public key" htmlFor="ssh-key-value" required>
            <Textarea
              id="ssh-key-value"
              placeholder="ssh-ed25519 AAAA…"
              value={publicKey}
              onChange={(e) => setPublicKey(e.target.value)}
              rows={4}
              spellCheck={false}
              autoComplete="off"
              required
              className="font-mono text-xs"
            />
          </Field>
          {error && (
            <div className="rounded-lg border border-danger/30 bg-danger-subtle px-3 py-2 text-xs text-danger">
              {error}
            </div>
          )}
        </div>

        <DialogFooter>
          <Button variant="ghost" onClick={() => onOpenChange(false)}>
            Cancel
          </Button>
          <Button onClick={save} disabled={saving || !publicKey.trim()} variant="info">
            <KeyRound className="size-4" /> Add key
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

// A dedicated (not the shared wardyn/delete-confirm-dialog) confirm dialog:
// that shared one hard-codes the operator-only gate every OTHER delete in
// the console needs, but an SSH key is the signed-in human's own — removing
// it is never an operator-only act.
export function RemoveSSHKeyDialog({
  keyToDelete,
  onOpenChange,
  onRemoved,
}: {
  keyToDelete: SSHPublicKey | null;
  onOpenChange: (open: boolean) => void;
  onRemoved: () => void;
}) {
  const [removing, setRemoving] = React.useState(false);
  const label = keyToDelete?.name || keyToDelete?.fingerprint || "";

  const confirmRemove = async () => {
    if (!keyToDelete) return;
    setRemoving(true);
    try {
      await sshKeysApi.deleteKey(keyToDelete.fingerprint);
      toast.success(`Key “${label}” removed`);
      onRemoved();
      onOpenChange(false);
    } catch (e) {
      toast.error(`Failed to remove "${label}"`, { description: getErrorMessage(e) });
    } finally {
      setRemoving(false);
    }
  };

  return (
    <AlertDialog open={!!keyToDelete} onOpenChange={(o) => !o && onOpenChange(false)}>
      <AlertDialogContent>
        <AlertDialogHeader>
          <AlertDialogTitle>Remove key “{label}”?</AlertDialogTitle>
          <AlertDialogDescription>
            You will no longer be able to connect over SSH with this key. This cannot be undone.
          </AlertDialogDescription>
        </AlertDialogHeader>
        <AlertDialogFooter>
          <AlertDialogCancel>Cancel</AlertDialogCancel>
          <AlertDialogAction
            onClick={(e) => {
              e.preventDefault();
              void confirmRemove();
            }}
            disabled={removing}
            className="bg-danger text-danger-foreground hover:bg-danger/90"
          >
            <Trash2 className="size-4" /> Remove key
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  );
}
