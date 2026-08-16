/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// SSH keys management (account menu -> "SSH keys"): the signed-in human's
// OWN registered public keys — list, add, remove. Not operator-gated: an SSH
// key is a personal credential binding, not deployment configuration (the
// server enforces the same scope independently — sshkeys.go's handlers key
// every read/write off the caller's own principal).
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
import { EmptyState, ErrorState, TableSkeleton } from "../wardyn/states";
import { PageHeader } from "../wardyn/page-header";
import { absoluteTime, relativeTime } from "../../lib/format";

// The page at /ssh-keys. The body is SshKeysPane so Settings can render the
// same card ("Your SSH keys") without a second copy of the list, the dialogs,
// or their reload wiring — the account menu keeps both entries, and they show
// exactly the same thing.
export function SSHKeysScreen() {
  return (
    <div className="mx-auto max-w-[900px] px-6 py-6">
      <SshKeysPane />
    </div>
  );
}

export function SshKeysPane({ heading = "h1" }: { heading?: "h1" | "h3" } = {}) {
  const [keys, setKeys] = React.useState<SSHPublicKey[]>([]);
  const [status, setStatus] = React.useState<"loading" | "error" | "ready">("loading");
  const [addOpen, setAddOpen] = React.useState(false);
  const [toDelete, setToDelete] = React.useState<SSHPublicKey | null>(null);

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

  return (
    <div>
      <PageHeader
        as={heading}
        title="Your SSH keys"
        description="Public keys only — Wardyn never stores or asks for a private key. Keys are yours alone; there is no admin view of anyone else's."
        actions={
          <Button onClick={() => setAddOpen(true)}>
            <Plus className="size-4" /> Add key
          </Button>
        }
      />

      <div className="overflow-hidden rounded-xl border border-border bg-card">
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
              <Button onClick={() => setAddOpen(true)}>
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

      <AddSSHKeyDialog open={addOpen} onOpenChange={setAddOpen} onAdded={load} />
      <RemoveSSHKeyDialog keyToDelete={toDelete} onOpenChange={(o) => !o && setToDelete(null)} onRemoved={load} />
    </div>
  );
}

function AddSSHKeyDialog({
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
function RemoveSSHKeyDialog({
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
              confirmRemove();
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
