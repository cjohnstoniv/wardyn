/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// One line per Azure DevOps token the run held, oldest first (#1428), under an
// "Azure DevOps" heading in the run's Credentials widget: when it was created
// and expires, and, once revoked, when and why. Renewal, a pause and an
// approval each add or close one. The token itself is never on the wire, so
// nobody sees it here. Draws nothing for a run that holds none.
import { ADO_PAT } from "../../../lib/ado-pat-copy";
import { runTokenView } from "../../../lib/ado-pat-display";
import { useRunAdoTokens } from "../../../lib/hooks/use-run-ado-tokens";
import { clsx } from "clsx";

function Amber({ children }: { children: React.ReactNode }) {
  return (
    <p role="alert" className="mt-1.5 rounded-md border border-warning/30 bg-warning-subtle px-2 py-1.5 text-xs text-foreground">
      {children}
    </p>
  );
}

export function AdoRunTokens({ runId, paused, live }: { runId: string; paused: boolean; live: boolean }) {
  const tokens = useRunAdoTokens(runId, live);
  if (tokens.length === 0) return null;
  const view = runTokenView(tokens, paused);
  return (
    <div className="mt-2.5 border-t border-border pt-2" data-testid="ado-run-tokens">
      <h5 className="text-meta font-medium text-muted-foreground">{ADO_PAT.RUN_TOKEN_TITLE}</h5>
      {view.lines.map((l, i) => (
        <p key={i} className={clsx("mt-1 text-xs", l.old ? "text-muted-foreground" : "text-foreground")}>
          {l.text}
        </p>
      ))}
      {view.paused && <p className="mt-1 text-xs text-muted-foreground">{ADO_PAT.RUN_PAUSED}</p>}
      {view.revokeFailedAt.map((t, i) => (
        <Amber key={i}>{ADO_PAT.RUN_REVOKE_FAILED(t)}</Amber>
      ))}
    </div>
  );
}
