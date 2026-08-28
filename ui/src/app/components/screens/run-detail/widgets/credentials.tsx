/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Credentials widget — ports the honesty model from run-detail.tsx's
// GrantsCard verbatim: grants are ELIGIBILITY records (what the run MAY
// request), never live credentials. Only a `credential.mint` audit event with
// outcome === "success" is a real issued credential — the broker also audits
// DENIED mint attempts under that same action, so the outcome filter is
// load-bearing, not decorative (rendering a denied mint as "brokered" would
// claim a credential that was never issued).
import { KeyRound } from "lucide-react";
import type { AuditEvent, CredentialGrant } from "../../../../lib/types";
import { relativeTime } from "../../../../lib/format";
import { RUN_COCKPIT } from "../../../wardyn/copy";
import { Chip, WidgetCard } from "../../../wardyn/primitives";

export function CredentialsWidget({ grants, audit }: { grants: CredentialGrant[]; audit: AuditEvent[] }) {
  const minted = audit.filter((e) => e.action === "credential.mint" && e.outcome === "success");

  return (
    <WidgetCard
      title="Credentials"
      Icon={KeyRound}
      right={
        <span className="font-mono text-meta text-muted-foreground">
          {grants.length} eligible · {minted.length} minted
        </span>
      }
    >
      <p className="mb-2 text-meta leading-relaxed text-muted-foreground">
        {RUN_COCKPIT.credentialsEligibility}
      </p>

      {grants.length === 0 ? (
        // Reused verbatim from run-detail.tsx's GrantsCard — same fact, same words.
        <p className="text-xs text-muted-foreground">No credential grants are configured for this run.</p>
      ) : (
        <div className="flex flex-col gap-1.5">
          {grants.map((g) => (
            <div key={g.id} className="flex items-center gap-2">
              <Chip tone="neutral">eligible</Chip>
              <span className="min-w-0 flex-1 truncate font-mono text-xs text-foreground" title={g.scope}>
                {g.scope}
              </span>
            </div>
          ))}
        </div>
      )}

      {minted.length > 0 && (
        <div className="mt-1.5 flex flex-col gap-1.5">
          {minted.map((e) => (
            <div key={e.id} className="flex items-center gap-2">
              <Chip tone="info">brokered</Chip>
              <span
                className="min-w-0 flex-1 truncate font-mono text-xs text-muted-foreground"
                title={e.target}
              >
                {e.target || "credential"}
              </span>
              <span className="shrink-0 font-mono text-meta text-muted-foreground">
                {relativeTime(e.time)}
              </span>
            </div>
          ))}
        </div>
      )}
    </WidgetCard>
  );
}
