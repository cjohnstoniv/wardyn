/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The hosted models a run's proxy holds every model request to, beside the Local models card. It reads only
// run.allowed_models, the list stored when the run started, so a member sees it too and a later change to the
// profile does not move it. It is never shown or hidden by the viewer's role: an administrator's run may be
// governed, and one that is not has no list. The link to the profile is offered to a caller who may read profiles.
import { ShieldCheck } from "lucide-react";
import { Link } from "react-router-dom";
import { ALLOWED_MODELS as T, MEMBER } from "../../../lib/governance-copy";
import type { RunDetail } from "../../../lib/types";
import { useOperatorResolved, useSecurityOperator } from "../../wardyn/operator-context";
import { WidgetCard } from "../../wardyn/primitives";

export function AllowedModelsCard({ run }: { run: RunDetail }) {
  // Resolved too: the security-operator flag reads true until /me answers.
  const security = useSecurityOperator();
  const resolved = useOperatorResolved();
  const allowed = run.allowed_models;
  if (!allowed || allowed.models.length === 0) return null;
  return (
    <div className="shrink-0 px-4 pt-3" data-testid="run-allowed-models">
      <WidgetCard
        title={T.CARD_TITLE}
        Icon={ShieldCheck}
        right={<span className="min-w-0 break-words text-xs text-muted-foreground">{MEMBER.GS_CHIP(allowed.unit.name)}</span>}
        bodyClassName="space-y-3 px-3 py-2.5 text-body"
      >
        <ul className="scroll-thin max-h-44 space-y-1.5 overflow-y-auto" data-testid="run-allowed-models-list">
          {allowed.models.map((m) => (
            <li key={m} className="break-all font-mono text-xs">
              {m}
            </li>
          ))}
        </ul>
        <p className="text-muted-foreground">{T.CARD_REFUSED}</p>
        <p className="text-muted-foreground">{T.CARD_FIXED}</p>
        {security && resolved && (
          <Link to={`/admin/governance?profile=${encodeURIComponent(allowed.unit.id)}`} className="inline-block text-info hover:underline">
            {T.OPEN_PROFILE}
          </Link>
        )}
      </WidgetCard>
    </div>
  );
}
