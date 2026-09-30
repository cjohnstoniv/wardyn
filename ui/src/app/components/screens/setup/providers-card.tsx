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
// provider rows, never hosts (§7.5), and ENABLED agent rows beside them —
// CARD_SUMMARY(CARD_PROVIDERS(n), CARD_AGENTS(m)), the shape §7.5 froze.
//
// The agent count comes from the roster BOTH homes already hold (SetupStatus.
// harnesses, passed in) rather than a fetch of this card's own: `enabled` is the
// org's answer per row — may a run name this agent — and it is true for every
// catalog row in legacy-open mode, so a deployment that has saved no agent
// roster reads honestly instead of a false "0 agents". A second
// getSetupStatus() here would also re-fetch what the funnel walked in with, which
// setup-screen.test.tsx pins against. An older daemon omits the field: the
// summary then names git providers alone, as it did before W4.
//
// SUPER only, the same reason /drives has no nav item: a security admin's
// authority is the governance profile's two limit rows, not this registry.
// Anyone else gets the TIER, said as a tier (OperatorOnlyHint) rather than an
// empty card — the funnel walks its `providers` step for every role.
//
// Every product string comes from workspace-providers-copy.ts. This file adds
// none.
import * as React from "react";
import { useNavigate } from "react-router-dom";
import { ChevronRight } from "lucide-react";
import { providers as api, type GitProvider } from "../../../lib/api/providers";
import { ADO_PAT } from "../../../lib/ado-pat-copy";
import { adoRowNeedsChoice } from "../../../lib/ado-pat-display";
import { Button } from "../../ui/button";
import type { SetupHarnessTool } from "../../../lib/types";
import { PROVIDERS } from "../../../lib/workspace-providers-copy";
import { OperatorOnlyHint } from "../../wardyn/primitives";
import { useOperator } from "../../wardyn/operator-context";
import { CollapsibleCard } from "../../wardyn/collapsible-card";

export function ProvidersCard({
  /** The harness roster off SetupStatus — the SAME read both homes already made.
   *  Undefined is UNKNOWN (an older daemon omits it, or status hasn't landed):
   *  the summary then names git providers alone rather than a false "0 agents". */
  harnesses,
  /** #1200 compact cards — Settings' only. Getting started's `providers` step
   *  renders this same component and must stay fully open, so the collapse
   *  is an opt-in, never this component's own new default. */
  compact = false,
}: {
  harnesses?: SetupHarnessTool[];
  compact?: boolean;
} = {}) {
  const operator = useOperator();
  const navigate = useNavigate();
  const [count, setCount] = React.useState<number | null>(null);
  // The rows, for the Azure DevOps line that needs attention (#1428): a row the
  // upgrade switched off waits for the admin to choose how people connect.
  const [rows, setRows] = React.useState<GitProvider[]>([]);

  React.useEffect(() => {
    if (!operator) return;
    let live = true;
    api
      .getWorkspaceProviders()
      .then(({ providers }) => {
        if (!live) return;
        setCount((providers.git ?? []).filter((row) => !row.disabled).length);
        setRows(providers.git ?? []);
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

  // The tier said as a tier, never a blank body. This card IS the funnel's
  // `providers` step body and `providers` is in stepOrder unconditionally, so
  // returning null left a security admin or a member on a step with a heading,
  // an Optional badge and NOTHING underneath — the hidden-instead-of-disabled
  // shape §2.6's state list forbids. ProvidersScreen answers the same refusal
  // the same way (its forbidden arm). Still zero teal, and still no fetch.
  if (!operator) {
    if (compact) {
      return (
        <CollapsibleCard
          title={
            <span className="inline-flex items-center gap-1">
              {PROVIDERS.TITLE}
              <OperatorOnlyHint />
            </span>
          }
          testId="providers-card"
        >
          <p className="text-body leading-snug text-muted-foreground">{PROVIDERS.CARD_LEAD}</p>
        </CollapsibleCard>
      );
    }
    return (
      <section className="rounded-xl border border-border bg-card p-4" data-testid="providers-card">
        <h3 className="flex items-center text-sm font-medium text-foreground">
          {PROVIDERS.TITLE}
          <OperatorOnlyHint />
        </h3>
        <p className="mt-0.5 text-body leading-snug text-muted-foreground">{PROVIDERS.CARD_LEAD}</p>
      </section>
    );
  }

  // CARD_EMPTY only when there is genuinely nothing enabled on either side —
  // otherwise both counts ride, including a zero half (0 git providers IS the
  // legacy-open fact, and CARD_PROVIDERS(0) says it without claiming the agents
  // are gone too).
  // The agents half exists only once an agent POLICY is visible. The server
  // stamps `enabled: true` on every catalog row in open mode too
  // (setupHarnessTools: no block ⇒ everything is offered), so an all-enabled
  // roster cannot be told from "nobody has decided"; a turned-off row is the
  // one sign of a stored policy (a row carries no model credential since 0.8).
  // No such row ⇒ the summary names git providers alone (and CARD_EMPTY when
  // those are zero).
  const rosterHasPolicy = !!harnesses && harnesses.some((h) => h.enabled === false);
  const agents = rosterHasPolicy ? harnesses!.filter((h) => h.enabled !== false).length : null;
  const summary =
    count === null
      ? ""
      : agents === null
        ? count === 0
          ? PROVIDERS.CARD_EMPTY
          : PROVIDERS.CARD_PROVIDERS(count)
        : count === 0 && agents === 0
          ? PROVIDERS.CARD_EMPTY
          : PROVIDERS.CARD_SUMMARY(PROVIDERS.CARD_PROVIDERS(count), PROVIDERS.CARD_AGENTS(agents));

  // #1200 compact cards: compact mode already states `summary` in the
  // collapsed header, so the open-button's own copy of it would duplicate
  // the same text node twice once expanded — suppressed there, unchanged
  // (still the two-line button) in Getting started.
  // A warning, never a blocker: the row itself already refuses what it must,
  // with its reason. The organisation check's findings are the row's own, not
  // this card's: the server keeps no last answer to draw them from here.
  const attention = [
    ...(rows.some(adoRowNeedsChoice) ? [{ text: ADO_PAT.CONVERTED_CHECKLIST, choose: true }] : []),
  ];
  const attentionLines = attention.length > 0 && (
    <div className="mt-3 space-y-2" data-testid="ado-pat-checks">
      {attention.map((a) => (
        <div key={a.text} className="flex items-start gap-2 rounded-lg border border-warning/30 bg-warning-subtle px-3 py-2 text-body">
          <span aria-hidden="true" className="w-4 shrink-0 text-center font-bold text-warning">
            !
          </span>
          <span className="min-w-0 flex-1">{a.text}</span>
          {a.choose && (
            <Button size="sm" variant="outline" onClick={() => navigate("/admin/providers")}>
              {ADO_PAT.CONVERTED_CHOOSE}
            </Button>
          )}
        </div>
      ))}
    </div>
  );

  const openButton = (
    <button
      type="button"
      onClick={() => navigate("/admin/providers")}
      className="mt-3 flex w-full items-center justify-between rounded-lg border border-border px-3 py-2 text-left transition-colors hover:border-border-strong"
    >
      <span>
        <span className="block text-body font-medium text-foreground">{PROVIDERS.CARD_OPEN}</span>
        {summary && !compact && <span className="block text-meta text-muted-foreground">{summary}</span>}
      </span>
      <ChevronRight className="size-4 shrink-0 text-muted-foreground" />
    </button>
  );

  if (compact) {
    return (
      <CollapsibleCard title={PROVIDERS.TITLE} summary={summary || undefined} testId="providers-card">
        <p className="text-body leading-snug text-muted-foreground">{PROVIDERS.CARD_LEAD}</p>
        {attentionLines}
        {openButton}
      </CollapsibleCard>
    );
  }
  return (
    <section className="rounded-xl border border-border bg-card p-4" data-testid="providers-card">
      <h3 className="text-sm font-medium text-foreground">{PROVIDERS.TITLE}</h3>
      <p className="mt-0.5 text-body leading-snug text-muted-foreground">{PROVIDERS.CARD_LEAD}</p>
      {attentionLines}
      {openButton}
    </section>
  );
}
