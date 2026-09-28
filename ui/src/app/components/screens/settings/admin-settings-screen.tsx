/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Admin view Settings (M-5, #636 — the settings split, §4.3): this
// deployment, never a person. Reached at /admin/settings, super admins only
// (S-5) — a security admin who types or bookmarks the URL gets a refusal,
// nothing fetched, rather than the tier-appropriate leftovers this page used
// to render for them.
//
// Host · Model providers · Model provider · Providers · User drives · Admin
// SSH keys. The personal cards (a person's own model connection, Azure
// DevOps, Your SSH keys) moved to Your account (your-account-screen.tsx) —
// nothing on this page belongs to the admin as a person.
//
// This file used to be settings-screen.tsx, mounted unchanged at BOTH
// /admin/settings and /account until M-5 split it (admin-member-modes-design.md
// §4.3). HostCard moved here with it; ModelProviderCard/ProvidersCard/
// UserDrivesCard/BrandingCard/ModelProvidersList are unchanged, shared
// components.
//
// #1200 compact cards (settings-compact-1200-packet.html, owner-approved):
// every card here collapses to a one-line summary and expands on click, none
// open by default — see collapsible-card.tsx for why that default is
// load-bearing. ModelProviderCard/ProvidersCard/UserDrivesCard also render in
// Getting started, which must stay fully open, so they take the collapse as
// an opt-in `compact` prop rather than a new always-on default.
//
// ponytail: the Corporate proxy & egress disclosure SUMMARIZES and links to the
// funnel's Corporate network step rather than re-mounting HostProxyTab here.
// That tab needs the step's whole mutate/saving/probe plumbing; duplicating it
// would be ~200 lines of second-copy state for a surface an operator visits
// once. The summary is read from the same SiteConfig, so it can't go stale.
import * as React from "react";
import { useNavigate } from "react-router-dom";
import { ChevronRight } from "lucide-react";
import { setup as setupApi } from "../../../lib/api/setup";
import { health } from "../../../lib/api/health";
import type { SetupStatus, SiteConfig } from "../../../lib/types";
import { CC_ORDER } from "../../../lib/types";
import { PageHeader } from "../../wardyn/page-header";
import { ErrorState, TableSkeleton } from "../../wardyn/states";
import { Mono } from "../../wardyn/code-block";
import { strongestAvailable } from "../../wardyn/default-confinement";
import { TierPicker } from "../../wardyn/tier-picker";
import { CC_META } from "../../wardyn/cc-meta";
import { K8sEnvironmentRows, NoRunnerCard, runnerAvailability } from "../setup/environment-step";
import { CollapsibleCard } from "../../wardyn/collapsible-card";
import { useOperator, useOperatorResolved } from "../../wardyn/operator-context";
import { isProxyConfigured } from "../setup/corp-network-proxy";
import { ModelProviderCard } from "./connection-cards";
import { UserDrivesCard } from "../setup/user-drives-card";
import { ProvidersCard } from "../setup/providers-card";
import { ModelProvidersList } from "./model-providers-list";
import { BrandingCard } from "./branding-card";
import { AdminSshKeysCard } from "./admin-ssh-keys-card";
import { ViewNotice } from "../../wardyn/console-view";
import { VIEW_REFUSAL, SETTINGS_SUPER_ONLY } from "../../wardyn/copy/console-view";
import { Button } from "../../ui/button";

function Row({ label, value }: { label: string; value: React.ReactNode }) {
  return (
    <div className="flex items-baseline justify-between gap-4 py-1.5">
      <span className="text-body text-muted-foreground">{label}</span>
      <span className="text-right text-body text-foreground">{value}</span>
    </div>
  );
}

function HostCard({
  status,
  siteConfig,
  onRecheck,
}: {
  status: SetupStatus;
  /**
   * The site config, `null` for "this caller may not read it" (a member), or
   * "error" for "the read FAILED" — three states, because a failed read and an
   * empty read are different facts, and the console must never speak for the
   * server in that difference (R4/F069).
   */
  siteConfig: SiteConfig | null | "error";
  onRecheck: () => void;
}) {
  const navigate = useNavigate();
  // The strongest installed class — the server-computed default, read
  // straight (no override state left to own here: there is nothing left to
  // persist, and this card is a read-only statement of "what every run
  // inherits by default", never a second place to pick it).
  const installed = CC_ORDER.filter((cc) => (status.runner.confinement_classes ?? []).includes(cc));
  const selected = strongestAvailable(status.runner.confinement_classes ?? []) ?? "CC1";
  // #1200 review P1-1 — the SAME no-runner facts EnvironmentStep reads, so
  // this second mount can render its canon danger card + fix line instead of
  // a compact-picker-only fallback, and the k8s Runner/Egress-containment
  // rows a driver:"k8s" host needs regardless of noRunner.
  const { noDriver, noRunner } = runnerAvailability(status);
  const k8s = status.runner.driver === "k8s";

  const envBuilder = status.checks.find((c) => c.id === "env_builder");
  // GET /api/v1/site-config is operatorOnly since R1 — it carries the upstream
  // proxy secret ref, the integration credential refs and the internal
  // proxy/SCM hostnames, which no member should be handed. Both callers of it
  // already .catch() into a null config, so a member reaches here with
  // siteConfig === null and `proxied` false. That is fine as ABSENCE and wrong
  // as a STATEMENT: rendering "Not configured — sandboxes go direct" to a member
  // of a deployment that IS behind a corporate proxy is a false claim, not a
  // redaction. So the two places that assert a proxy POSTURE are operator-only,
  // and a member simply does not see them — they also link into an operator
  // funnel step, which was never theirs to open.
  const operator = useOperator();
  // R4/F069 — the operator's own FAILED read is the case the reasoning above
  // misses, and the operator is the person the statement is addressed to.
  // AdminSettingsScreen.load swallows every site-config failure into null and
  // still resolves state="ready", so a 500 or a dead network rendered
  // "Internet: Direct" and "Not configured — sandboxes go direct" — two
  // POSITIVE claims about a deployment nothing had read. Same rule, one state
  // further: absence is honest, a statement is not, so a failed read shows
  // neither claim. The funnel link itself STAYS — it is how the operator goes
  // and finds out.
  const configUnknown = siteConfig === "error";
  const cfg: SiteConfig | null = configUnknown ? null : siteConfig;
  const proxied = !configUnknown && isProxyConfigured(cfg);
  // #1200 compact cards — the one line this card states while collapsed. The
  // tier is the headline fact ("what every run inherits by default", the
  // lede below); noRunner is stated as its own fact rather than naming a
  // tier that cannot actually launch anything.
  const hostSummary = noRunner
    ? "No barrier installed — runs can't launch"
    : `Runs default to ${CC_META[selected].label}`;

  return (
    <CollapsibleCard title="Host" summary={hostSummary} testId="host-card">
      <p className="text-body leading-snug text-muted-foreground">
        The barriers this machine can build, and what every run inherits by
        default.
      </p>

      <div className="mt-3 space-y-3">
        {/* #1200 review P1-1 — the canon no-runner card, byte-identical to
            Getting started's: a host with nothing to launch on is a fact
            about the HOST, not something a compact-picker fallback should
            restate in its own words. */}
        {noRunner && <NoRunnerCard noDriver={noDriver} />}
        {/* k8s Runner/Egress containment/Confinement classes/Agent images —
            unconditional on noRunner, exactly like EnvironmentStep: a
            driver:"k8s" host still gets these rows even when it reports
            zero classes. */}
        {k8s && <K8sEnvironmentRows status={status} />}
        {/* #1200 — the compact TierPicker, display mode: installed tiers
            ONLY (T-10), never the governance floor (that ceiling is stated
            in Governance, where it's set, and in the person's own picker,
            where it binds — a third place would be a third to keep in
            sync). Read-only: nobody picks a default here, this card states
            what every run inherits by default. Suppressed while noRunner —
            the danger card above is the honest statement then, not an
            empty "nothing installed" picker beside it. */}
        {!noRunner && <TierPicker tiers={installed} mode="display" recommended={selected} />}
      </div>

      <div className="mt-4 divide-y divide-border border-t border-border pt-1">
        {/* The check's LABEL is "Sandbox image builder" — echoing it next to a
            row already labelled "Image builder" says nothing. Its status is the
            fact: "ok" means the per-run builder is wired (devcontainer builds
            and --image wraps fire), "info" means it's off. */}
        {/* X3-F1: gated exactly like the Internet row below — a row that states
            a deployment fact is drawn only when this caller was actually told
            it. `checks_redacted` marks a body whose checks list was stripped
            for the reader's tier, so [] there means WITHHELD, not "no builder":
            rendering the Off sentence over it told a member their admin's
            builder was off. Absent/false (every operator body) is unchanged. */}
        {!status.checks_redacted && (
          <Row
            label="Image builder"
            value={
              envBuilder?.status === "ok" ? (
                "Wired"
              ) : (
                <span className="text-muted-foreground">
                  Off — devcontainer builds and --image wraps are unavailable
                </span>
              )
            }
          />
        )}
        <Row
          label="Recording store"
          value={
            status.age_key?.durable ? (
              "Enabled"
            ) : (
              <span className="text-warn">
                Ephemeral — recordings are lost on restart
              </span>
            )
          }
        />
        {operator && !configUnknown && (
          <Row
            label="Internet"
            value={proxied ? "Through the corporate proxy" : "Direct"}
          />
        )}
      </div>

      {/* Delegated, not duplicated — see the file header. Operator-only: it
          states the deployment's proxy posture and opens a setup step. */}
      {operator && (
        <button
          type="button"
          onClick={() => navigate("/admin/setup?step=corp_network")}
          className="mt-3 flex w-full items-center justify-between rounded-lg border border-border px-3 py-2 text-left transition-colors hover:border-border-strong"
        >
          <span>
            <span className="block text-body font-medium text-foreground">
              Corporate proxy &amp; egress
            </span>
            {!configUnknown && (
              <span className="block text-meta text-muted-foreground">
                {proxied ? (
                  <>
                    Upstream proxy set —{" "}
                    <Mono>{cfg?.upstream_proxy_url || "configured"}</Mono>
                  </>
                ) : (
                  "Not configured — sandboxes go direct"
                )}
              </span>
            )}
          </span>
          <ChevronRight className="size-4 shrink-0 text-muted-foreground" />
        </button>
      )}
      <button
        type="button"
        onClick={onRecheck}
        className="mt-2 text-meta text-muted-foreground underline-offset-2 hover:underline"
      >
        Re-check this host
      </button>
    </CollapsibleCard>
  );
}

export function AdminSettingsScreen() {
  const navigate = useNavigate();
  // S-5 (#636) — the SUPER tier, not the security one: this whole page is
  // "this deployment, never a person", and a security admin has no door onto
  // it (app-shell.tsx's navItemsForView already gives that tier `lower: []`).
  // A stale link or a typed URL still reaches the route, so the page itself
  // refuses rather than rendering the tier-appropriate leftovers it used to.
  const operator = useOperator();
  const operatorResolved = useOperatorResolved();
  const refused = operatorResolved && !operator;
  // Same fail-open rationale as the site-config read always had (R4/F069,
  // HostCard's own comment): during the cold-load window before /me
  // resolves, `operator` reads the fail-open default TRUE, so `adminReads`
  // (unlike `refused`) stays gated on `operatorResolved` too — a component
  // mounted before the real role is known must not fire this admin-only read
  // on the strength of a default that might still flip to "refused".
  const adminReads = operatorResolved && operator;

  const [state, setState] = React.useState<"loading" | "error" | "ready">(
    "loading",
  );
  const [status, setStatus] = React.useState<SetupStatus | null>(null);
  const [siteConfig, setSiteConfig] = React.useState<SiteConfig | null>(null);
  // R4/F069: the site-config read failing is NOT the same fact as it answering
  // "nothing is configured", and this screen turned both into `null`. Kept
  // beside the config rather than folded into it so the three other cards keep
  // the two-state prop they already reason about; HostCard is the one that
  // makes POSITIVE claims from it, so it is the one that is told.
  const [configFailed, setConfigFailed] = React.useState(false);

  const load = React.useCallback(() => {
    let failed = false;
    Promise.all([
      setupApi.getSetupStatus(),
      adminReads
        ? health.getSiteConfig().catch(() => {
            failed = true;
            return null;
          })
        : Promise.resolve(null),
    ])
      .then(([s, cfg]) => {
        setStatus(s);
        setSiteConfig(cfg);
        setConfigFailed(failed);
        setState("ready");
      })
      .catch(() => setState("error"));
  }, [adminReads]);
  // S-5: nothing is fetched for a refused caller — the effect below never
  // fires `load()` for one, not merely a page that fetches then hides itself.
  React.useEffect(() => {
    if (refused) return;
    load();
  }, [load, refused]);

  if (refused) {
    return (
      <ViewNotice title={VIEW_REFUSAL.TITLE} body={SETTINGS_SUPER_ONLY.BODY}>
        <Button size="sm" onClick={() => navigate("/admin/runs")}>
          {SETTINGS_SUPER_ONLY.CTA}
        </Button>
      </ViewNotice>
    );
  }

  return (
    <div className="mx-auto w-full max-w-[900px] px-6 py-8">
      <PageHeader
        title="Settings"
        description="This host, what runs your agents, and how Wardyn reaches your code."
      />
      {state === "loading" && <TableSkeleton />}
      {state === "error" && <ErrorState onRetry={load} />}
      {state === "ready" && status && (
        // space-y-2, not -4: seven collapsed cards plus this page's own
        // header must fit 744px (settings-compact-1200-packet.html §4).
        <div className="space-y-2">
          <HostCard
            status={status}
            siteConfig={configFailed ? "error" : siteConfig}
            onRecheck={load}
          />
          {/* #1125 (B-1): beside Host and Model providers, super admin only —
              the server refuses anyone else's save regardless. Unconditional
              on role now: this whole screen is super-admin-only (S-5). */}
          <BrandingCard />
          {/* #536: the Admin view only, and it stays there — admin
              configuration, never a person's own connection.
              #538: the Claude subscription kind is disabled on the editor's
              kind step until the sign-in image resolves — undefined status
              (an older daemon with no such check) reads as available. */}
          <ModelProvidersList
            harnesses={status.harnesses}
            subscriptionAvailable={status.checks.find((c) => c.id === "claude_signin_image")?.status !== "warn"}
          />
          {/* The shared credential lanes, as built — until MP-18 replaces this
              card (design §4.3). S-4 (#636): Your account mounts the SAME
              component for a person's own connection; this is the org one. */}
          <ModelProviderCard
            status={status}
            siteConfig={siteConfig}
            onChanged={load}
            compact
          />
          {/* The Providers card replaces Git host: the git credential
              lanes moved into a provider row on /providers, and this card is
              the same shared component the funnel's `providers` step body
              renders (setup/providers-card.tsx). */}
          <ProvidersCard harnesses={status?.harnesses} compact />
          {/* The FIFTH card, and so the last one (user-drives-prompt.md §6) —
              the SAME component the setup funnel's Workspaces step renders,
              summarising and linking exactly as the Corporate proxy disclosure
              above does. SUPER-only; it renders nothing for anyone else, which
              is why the position is pinned in the suite rather than left to
              read off the source. */}
          <UserDrivesCard compact />
          {/* M-5 (#636, packet S-1): the sixth and last card — where an admin
              adds an SSH key that reaches other people's runs, now that Your
              account is the only door left for a personal one. */}
          <AdminSshKeysCard />
        </div>
      )}
    </div>
  );
}
