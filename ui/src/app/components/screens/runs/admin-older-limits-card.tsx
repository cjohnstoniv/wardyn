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
// nothing for a non-operator admin, or before the first load ever finds
// anything outside the window.
import * as React from "react";
import { toast } from "sonner";
import { runs as runsApi } from "../../../lib/api/runs";
import type { AdminRestartResult } from "../../../lib/types";
import { getErrorMessage } from "../../../lib/format";
import { useOperator } from "../../wardyn/operator-context";
import { Button } from "../../ui/button";
import { Chip } from "../../wardyn/primitives";
import * as RL from "../../wardyn/copy/run-lifetime";

// reviveBulkMax (run_revive.go): the server 400s a restart request naming
// more ids than this — a fleet with more out-of-window runs than one batch
// covers must go in several requests, not fail outright.
const RESTART_BATCH_MAX = 100;

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

  // R2-4 (PR #1317 round-2 review): a successful restart rewrites each run's
  // proxy_release (store_run_revive.go), so the very next listing this same
  // reload asks for comes back with NOTHING outside the window — the run
  // that just succeeded. Returning null here on an EMPTY outsideIds, with no
  // exception for a result just shown, made a fully successful bulk restart
  // erase its own confirmation the instant it finished. Render nothing only
  // when there is truly nothing to say: no runs outside, and no result from
  // the last restart either.
  if (!operator) return null;
  const nothingOutside = !outsideIds || outsideIds.length === 0;
  if (nothingOutside && !results) return null;

  const restart = async () => {
    if (!outsideIds) return;
    setBusy(true);
    try {
      // Batched, never one request over RESTART_BATCH_MAX ids (the server's
      // own reviveBulkMax would 400 the whole thing otherwise) — each
      // batch's own failures don't stop the rest.
      const collected: AdminRestartResult[] = [];
      for (let i = 0; i < outsideIds.length; i += RESTART_BATCH_MAX) {
        const batch = outsideIds.slice(i, i + RESTART_BATCH_MAX);
        const res = await runsApi.restartAdminRuns(batch);
        collected.push(...res.results);
      }
      setResults(collected);
      load();
    } catch (err) {
      toast.error("Couldn't restart these runs", { description: getErrorMessage(err) });
    } finally {
      setBusy(false);
    }
  };

  return (
    <div data-testid="admin-older-limits-card" className="mb-4 rounded-xl border border-border bg-card p-3.5">
      {!nothingOutside && (
        <>
          <div className="flex flex-wrap items-center gap-2">
            <Chip tone="neutral">
              {RL.OLDER_LIMITS_CHIP} · {outsideIds.length}
            </Chip>
            <Button size="sm" onClick={() => void restart()} disabled={busy}>
              {RL.RESTART_WITH_CURRENT_LIMITS}
            </Button>
          </div>
          <p className="mt-2 text-meta text-muted-foreground">{RL.RESTART_HINT}</p>
        </>
      )}
      {results && (
        // R2-4: neither the mock nor the design draws per-run restart result
        // words — "Restarted" is the closest drawn wording (the past tense of
        // RESTART_WITH_CURRENT_LIMITS's own verb), and a failure shows the
        // server's OWN refusal text verbatim (e.g. reviveEligible's "revive
        // it from the run's page" for a rebooted run the bulk path can never
        // start again), never a second, invented "still lost" gloss on it.
        <ul className="mt-2 space-y-1 text-meta text-muted-foreground">
          {results.map((r) => (
            <li key={r.run_id}>
              {r.run_id.slice(0, 8)}: {r.ok ? "Restarted" : r.error}
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}
