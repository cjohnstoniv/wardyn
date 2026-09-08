/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Settings — five cards, reached from the account menu (not the nav). This is
// the single home for "what is connected and how is this host set up", and it
// replaces /integrations entirely: the old page shipped an operator-
// extensibility framework (seven closed kinds plus a generic escape hatch, a
// probe system, an adopt/derive lifecycle) as the answer to two questions most
// operators answer once.
//
// Host · Model provider · Git host · Your SSH keys · Drives (last, §6's "fifth card").
//
// Two of them are components shared verbatim with the Getting Started
// funnel (connection-cards.tsx) and one is the barrier picker shared with its
// Environment step (EnvironmentStep) — so Settings and the tour cannot drift.
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
import { PageHeader } from "../../wardyn/page-header";
import { ErrorState, TableSkeleton } from "../../wardyn/states";
import { Mono } from "../../wardyn/code-block";
import {
  getDefaultCc,
  resolveDefaultCc,
  setDefaultCc,
} from "../../wardyn/default-confinement";
import type { ConfinementClass } from "../../../lib/types";
import { EnvironmentStep } from "../setup/environment-step";
import { useOperator } from "../../wardyn/operator-context";
import { isProxyConfigured } from "../setup/corp-network-proxy";
import { SshKeysPane } from "../ssh-keys";
import { ModelProviderCard, GitHostCard } from "./connection-cards";
import { UserDrivesCard } from "../setup/user-drives-card";

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
   * "error" for "the read FAILED" — three states, because the last two used to
   * be one and the console spoke for the server in the difference (R4/F069).
   */
  siteConfig: SiteConfig | null | "error";
  onRecheck: () => void;
}) {
  const navigate = useNavigate();
  const [override, setOverride] = React.useState<ConfinementClass | null>(null);
  const selected = resolveDefaultCc(
    override ?? getDefaultCc(),
    status.runner.confinement_classes ?? [],
  );

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
  // SettingsScreen.load swallows every site-config failure into null and still
  // resolves state="ready", so a 500 or a dead network rendered "Internet:
  // Direct" and "Not configured — sandboxes go direct" — two POSITIVE claims
  // about a deployment nothing had read. Same rule, one state further: absence
  // is honest, a statement is not, so a failed read shows neither claim. The
  // funnel link itself STAYS — it is how the operator goes and finds out.
  const configUnknown = siteConfig === "error";
  const cfg: SiteConfig | null = configUnknown ? null : siteConfig;
  const proxied = !configUnknown && isProxyConfigured(cfg);

  return (
    <section className="rounded-xl border border-border bg-card p-4">
      <h3 className="text-sm font-medium text-foreground">Host</h3>
      <p className="mt-0.5 text-body leading-snug text-muted-foreground">
        The barriers this machine can build, and what every run inherits by
        default.
      </p>

      <div className="mt-3">
        <EnvironmentStep
          status={status}
          selected={selected}
          onSelect={(cc) => {
            setOverride(cc);
            setDefaultCc(cc);
          }}
        />
      </div>

      <div className="mt-4 divide-y divide-border border-t border-border pt-1">
        {/* The check's LABEL is "Sandbox image builder" — echoing it next to a
            row already labelled "Image builder" says nothing. Its status is the
            fact: "ok" means the per-run builder is wired (devcontainer builds
            and --image wraps fire), "info" means it's off. */}
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
          onClick={() => navigate("/setup?step=corp_network")}
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
    </section>
  );
}

export function SettingsScreen() {
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
      health.getSiteConfig().catch(() => {
        failed = true;
        return null;
      }),
    ])
      .then(([s, cfg]) => {
        setStatus(s);
        setSiteConfig(cfg);
        setConfigFailed(failed);
        setState("ready");
      })
      .catch(() => setState("error"));
  }, []);
  React.useEffect(load, [load]);

  return (
    <div className="mx-auto w-full max-w-[900px] px-6 py-8">
      <PageHeader
        title="Settings"
        description="This host, what runs your agents, and how Wardyn reaches your code."
      />
      {state === "loading" && <TableSkeleton />}
      {state === "error" && <ErrorState onRetry={load} />}
      {state === "ready" && status && (
        <div className="space-y-4">
          <HostCard
            status={status}
            siteConfig={configFailed ? "error" : siteConfig}
            onRecheck={load}
          />
          <ModelProviderCard
            status={status}
            siteConfig={siteConfig}
            onChanged={load}
          />
          <GitHostCard
            status={status}
            siteConfig={siteConfig}
            onChanged={load}
          />
          <SshKeysPane heading="h3" />
          {/* The FIFTH card, and so the last one (user-drives-prompt.md §6) —
              the SAME component the setup funnel's Workspaces step renders,
              summarising and linking exactly as the Corporate proxy disclosure
              above does. SUPER-only; it renders nothing for anyone else, which
              is why the position is pinned in the suite rather than left to
              read off the source. */}
          <UserDrivesCard />
        </div>
      )}
    </div>
  );
}
