/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// M-5 (#636, packet S-1, approved 2026-09-27) — "Admin SSH keys": the Admin
// view Settings card that lists and adds the keys that reach OTHER people's
// runs. D4 caps every key added in the user view at user rights, and after
// M-5 the user view (Your account) is the only console door left to add a
// key at all — without this card, M-5 would remove the last place an
// override key can be added.
//
// It reuses GET/POST/DELETE /me/ssh-keys verbatim: the server stamps a key's
// role off the caller's OWN session (an Admin view session isn't clamped),
// not off which card asked, so no server change was needed for this card —
// it is the SAME list SshKeysPane reads, filtered to the keys whose `role` is
// "admin". AddSSHKeyDialog/RemoveSSHKeyDialog are the exact dialogs
// ssh-keys.tsx already ships (Name, Public key, "never paste a private key").
//
// Mounted only from AdminSettingsScreen, which is itself reachable by a
// super admin alone (S-5's refusal keeps a security admin off the whole
// page) — so unlike UserDrivesCard this card carries no operator check of
// its own; there would be nothing left to gate.
import * as React from "react";
import { KeyRound, Plus, Trash2 } from "lucide-react";
import { sshKeys as sshKeysApi } from "../../../lib/api/ssh-keys";
import type { SSHPublicKey } from "../../../lib/types";
import { Button } from "../../ui/button";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "../../ui/table";
import { Mono } from "../../wardyn/code-block";
import { Chip } from "../../wardyn/primitives";
import { EmptyState, ErrorState, STATES, TableSkeleton } from "../../wardyn/states";
import { CollapsibleCard } from "../../wardyn/collapsible-card";
import { absoluteTime, relativeTime } from "../../../lib/format";
import { AddSSHKeyDialog, RemoveSSHKeyDialog } from "../ssh-keys";

export const ADMIN_SSH_KEYS = {
  TITLE: "Admin SSH keys",
  LEDE: "Keys added here reach runs you don't own. Each is refreshed when you sign in, and expires on its own if you don't.",
  EMPTY_TITLE: "No admin keys",
  EMPTY_BODY: "Add one only if you need to reach other people's runs over SSH.",
} as const;

export function AdminSshKeysCard() {
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

  // The card's whole point: only the keys that reach OTHER people's runs.
  // Your account's SshKeysPane lists every key this caller holds, admin or
  // not — this one narrows to the "Admin override" set.
  const adminKeys = keys.filter((k) => k.role === "admin");
  // #1200 compact cards — absent while unloaded. A distinct phrase from
  // ADMIN_SSH_KEYS.EMPTY_TITLE, not that constant itself: the header summary
  // and the expanded body's EmptyState both render at once, and a query for
  // the canon empty-state string must still resolve to a single node. The
  // error case reuses ErrorState's own default heading (review L4) rather
  // than inventing a new "Couldn't load" phrase the packet never named.
  const summary =
    status === "loading" ? undefined : status === "error" ? STATES.ERROR_TITLE : `${adminKeys.length} admin ${adminKeys.length === 1 ? "key" : "keys"}`;

  return (
    <CollapsibleCard title={ADMIN_SSH_KEYS.TITLE} summary={summary} testId="admin-ssh-keys-card">
      <div className="flex items-start justify-between gap-3">
        <p className="text-body leading-snug text-muted-foreground">{ADMIN_SSH_KEYS.LEDE}</p>
        {/* Outline, not the page's one teal button — that stays "Add model
            provider" (mock colfoot, packet MP-A precedent). */}
        <Button variant="outline" size="sm" onClick={() => setAddOpen(true)}>
          <Plus className="size-4" /> Add key
        </Button>
      </div>

      <div className="mt-3 overflow-hidden rounded-lg border border-border">
        {status === "loading" ? (
          <TableSkeleton rows={1} cols={4} />
        ) : status === "error" ? (
          <ErrorState onRetry={load} />
        ) : adminKeys.length === 0 ? (
          <EmptyState icon={KeyRound} title={ADMIN_SSH_KEYS.EMPTY_TITLE} description={ADMIN_SSH_KEYS.EMPTY_BODY} />
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
              {adminKeys.map((k) => (
                <TableRow key={k.fingerprint}>
                  <TableCell>
                    <span className="inline-flex items-center gap-2">
                      <KeyRound className="size-3.5 text-cyan" />
                      {k.name || <span className="text-muted-foreground">(unnamed)</span>}
                      <Chip
                        tone="warning"
                        title="Registered while you were an admin, so this key reaches runs you do not own. Refreshed automatically each time you sign in, and expires on its own if you don't; delete and re-register the key to drop the override immediately."
                      >
                        Admin override
                      </Chip>
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
    </CollapsibleCard>
  );
}
