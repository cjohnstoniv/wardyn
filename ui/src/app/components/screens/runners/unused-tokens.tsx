/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Admin -> Runners -> Unused registration tokens (M-RN-1 RA1): minted but not yet registered. Absent
// when there are none. Both admin tiers revoke (a leaver's token); only the super admin mints, and that
// is the page's button, not this section's. A token's value is never listed.
import * as React from "react";
import { toast } from "sonner";
import { runners as runnersApi } from "../../../lib/api/runners";
import { absoluteTime, relativeTime } from "../../../lib/format";
import type { RunnerRegistrationToken } from "../../../lib/types/runners";
import { RUNNERS } from "../../wardyn/copy/runners";
import { usePrincipal } from "../../wardyn/operator-context";
import { Button } from "../../ui/button";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "../../ui/table";
import { ConfirmDialog } from "./confirm-dialog";

export function UnusedTokens({ tokens, onChanged }: { tokens: RunnerRegistrationToken[]; onChanged: () => void }) {
  const me = usePrincipal();
  // The token stays set while the confirm closes, so its title does not blank during the exit.
  const [revoking, setRevoking] = React.useState<RunnerRegistrationToken | null>(null);
  const [confirming, setConfirming] = React.useState(false);
  if (tokens.length === 0) return null;
  return (
    <section className="mt-8" aria-labelledby="runners-unused">
      <h2 id="runners-unused" className="label-eyebrow">
        {RUNNERS.UNUSED_TITLE}
      </h2>
      <p className="mb-2 text-body text-muted-foreground">{RUNNERS.UNUSED_LEAD}</p>
      <div className="overflow-hidden rounded-xl border border-border bg-card">
        <Table>
          <TableHeader>
            <TableRow className="hover:bg-transparent">
              <TableHead>{RUNNERS.COL_FOR}</TableHead>
              <TableHead>{RUNNERS.COL_CREATED_BY}</TableHead>
              <TableHead>{RUNNERS.COL_EXPIRES}</TableHead>
              <TableHead className="w-24" />
            </TableRow>
          </TableHeader>
          <TableBody>
            {tokens.map((t) => (
              <TableRow key={t.id}>
                <TableCell className="font-medium">{t.owner}</TableCell>
                <TableCell className="text-muted-foreground">{me && t.minted_by === me ? RUNNERS.CREATED_BY_YOU : t.minted_by}</TableCell>
                <TableCell className="text-muted-foreground" title={absoluteTime(t.expires_at)}>
                  {relativeTime(t.expires_at)}
                </TableCell>
                <TableCell className="text-right">
                  <Button size="sm" variant="outline" onClick={() => {
                      setRevoking(t);
                      setConfirming(true);
                    }}>
                    {RUNNERS.REVOKE_TOKEN}
                  </Button>
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      </div>
      <ConfirmDialog
        open={confirming}
        onOpenChange={setConfirming}
        title={RUNNERS.REVOKE_TOKEN_TITLE(revoking?.owner ?? "")}
        body={[RUNNERS.REVOKE_TOKEN_BODY]}
        confirmLabel={RUNNERS.REVOKE_TOKEN}
        onConfirm={() => runnersApi.revokeToken(revoking?.id ?? "")}
        onDone={() => {
          toast.success(`${RUNNERS.REVOKE_TOKEN}: ${revoking?.owner}`);
          onChanged();
        }}
        onRaced={onChanged}
      />
    </section>
  );
}
