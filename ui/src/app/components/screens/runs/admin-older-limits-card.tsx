/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// F2 (#580, PR #1317 review) — the mock's own "Admin" section
// (long-holds-packet.html:301-303, design.md §6 "Admin"): a chip naming how
// many live runs carry an out-of-window proxy release, and the bulk
// "Restart with current limits" action over exactly those ids. The listing
// (GET /admin/runs/proxy-window) and the restart (POST /admin/runs/restart)
// both already existed (RL-10, #575); this is their one console surface.
//
// Operator-only, like the routes themselves (routes.go's operatorOnly
// group) — mounted unconditionally by runs.tsx's Admin view, and renders
// nothing for a non-operator admin or once nothing is outside the window.
import * as React from "react";
import { toast } from "sonner";
import { runs as runsApi } from "../../../lib/api/runs";
import type { AdminRestartResult } from "../../../lib/types";
import { getErrorMessage } from "../../../lib/format";
import { useOperator } from "../../wardyn/operator-context";
import { Button } from "../../ui/button";
import { Chip } from "../../wardyn/primitives";
import * as RL from "../../wardyn/copy/run-lifetime";

export function AdminOlderLimitsCard() {
  const operator = useOperator();
  const [outsideIds, setOutsideIds] = React.useState<string[] | null>(null);
  const [busy, setBusy] = React.useState(false);
  const [results, setResults] = React.useState<AdminRestartResult[] | null>(null);

  const load = React.useCallback(() => {
    if (!operator) return;
    runsApi
      .getAdminProxyWindow()
      .then((res) => setOutsideIds(res.outside.map((r) => r.run_id)))
      .catch(() => setOutsideIds(null));
  }, [operator]);

  React.useEffect(load, [load]);

  if (!operator || !outsideIds || outsideIds.length === 0) return null;

  const restart = async () => {
    setBusy(true);
    try {
      const res = await runsApi.restartAdminRuns(outsideIds);
      setResults(res.results);
      load();
    } catch (err) {
      toast.error("Couldn't restart these runs", { description: getErrorMessage(err) });
    } finally {
      setBusy(false);
    }
  };

  return (
    <div data-testid="admin-older-limits-card" className="mb-4 rounded-xl border border-border bg-card p-3.5">
      <div className="flex flex-wrap items-center gap-2">
        <Chip tone="neutral">
          {RL.OLDER_LIMITS_CHIP} · {outsideIds.length}
        </Chip>
        <Button size="sm" onClick={() => void restart()} disabled={busy}>
          {RL.RESTART_WITH_CURRENT_LIMITS}
        </Button>
      </div>
      <p className="mt-2 text-meta text-muted-foreground">{RL.RESTART_HINT}</p>
      {results && (
        <ul className="mt-2 space-y-1 text-meta text-muted-foreground">
          {results.map((r) => (
            <li key={r.run_id}>
              {r.run_id.slice(0, 8)}: {r.ok ? "Restarted" : r.error}
              {r.lost_again && " — still lost"}
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}
