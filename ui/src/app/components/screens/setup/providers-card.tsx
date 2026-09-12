/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The workspace-providers card — ONE component in two homes (workspace-
// providers-mock's State 1 / State 8): the setup funnel's `providers` step
// (its BODY — the step's footer Next is the step's own one affirmative
// action, so the card carries zero teal) and Settings, replacing the retired
// Git host card. The user-drives-card.tsx precedent, verbatim in shape.
//
// ZERO TEAL, both places — a two-line link button into /providers, where the
// tab forms and the one Save providers button live. It counts ENABLED git
// provider rows, never hosts (§7.5). The Agents-tab count (CARD_AGENTS) rides
// along once C-UI's agent-providers.ts lands (W4) — until then the summary
// names git providers alone rather than a false "0 agents".
//
// SUPER only, the same reason /drives has no nav item: a security admin's
// authority is the governance profile's two limit rows, not this registry.
//
// Every product string comes from workspace-providers-copy.ts. This file adds
// none.
import * as React from "react";
import { useNavigate } from "react-router-dom";
import { ChevronRight } from "lucide-react";
import { providers as api } from "../../../lib/api/providers";
import { PROVIDERS } from "../../../lib/workspace-providers-copy";
import { useOperator } from "../../wardyn/operator-context";

export function ProvidersCard() {
  const operator = useOperator();
  const navigate = useNavigate();
  const [count, setCount] = React.useState<number | null>(null);

  React.useEffect(() => {
    if (!operator) return;
    let live = true;
    api
      .getWorkspaceProviders()
      .then(({ providers }) => {
        if (!live) return;
        setCount((providers.git ?? []).filter((row) => !row.disabled).length);
      })
      // A failed read leaves the summary ABSENT rather than claiming
      // "No providers enabled." — a confident empty state over an unloaded
      // snapshot is a false claim, and the link still goes where the real
      // page is.
      .catch(() => {});
    return () => {
      live = false;
    };
  }, [operator]);

  if (!operator) return null;

  const summary = count === null ? "" : count === 0 ? PROVIDERS.CARD_EMPTY : PROVIDERS.CARD_PROVIDERS(count);

  return (
    <section className="rounded-xl border border-border bg-card p-4" data-testid="providers-card">
      <h3 className="text-sm font-medium text-foreground">{PROVIDERS.TITLE}</h3>
      <p className="mt-0.5 text-body leading-snug text-muted-foreground">{PROVIDERS.CARD_LEAD}</p>
      <button
        type="button"
        onClick={() => navigate("/providers")}
        className="mt-3 flex w-full items-center justify-between rounded-lg border border-border px-3 py-2 text-left transition-colors hover:border-border-strong"
      >
        <span>
          <span className="block text-body font-medium text-foreground">{PROVIDERS.CARD_OPEN}</span>
          {summary && <span className="block text-meta text-muted-foreground">{summary}</span>}
        </span>
        <ChevronRight className="size-4 shrink-0 text-muted-foreground" />
      </button>
    </section>
  );
}
