/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Settings — four cards, reached from the account menu (not the nav). This is
// the single home for "what is connected and how is this host set up", and it
// replaces /integrations entirely: the old page shipped an operator-
// extensibility framework (seven closed kinds plus a generic escape hatch, a
// probe system, an adopt/derive lifecycle) as the answer to two questions most
// operators answer once.
//
// Host · Model provider · Git host · Your SSH keys.
//
// Two of the four are components shared verbatim with the Getting Started
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
import { getDefaultCc, resolveDefaultCc, setDefaultCc } from "../../wardyn/default-confinement";
import type { ConfinementClass } from "../../../lib/types";
import { EnvironmentStep } from "../setup/environment-step";
import { isProxyConfigured } from "../setup/corp-network-proxy";
import { SshKeysPane } from "../ssh-keys";
import { ModelProviderCard, GitHostCard } from "./connection-cards";

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
  siteConfig: SiteConfig | null;
  onRecheck: () => void;
}) {
  const navigate = useNavigate();
  const [override, setOverride] = React.useState<ConfinementClass | null>(null);
  const selected = resolveDefaultCc(override ?? getDefaultCc(), status.runner.confinement_classes ?? []);

  const envBuilder = status.checks.find((c) => c.id === "env_builder");
  const proxied = isProxyConfigured(siteConfig);

  return (
    <section className="rounded-xl border border-border bg-card p-4">
      <h3 className="text-sm font-medium text-foreground">Host</h3>
      <p className="mt-0.5 text-body leading-snug text-muted-foreground">
        The barriers this machine can build, and what every run inherits by default.
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
              <span className="text-muted-foreground">Off — devcontainer builds and --image wraps are unavailable</span>
            )
          }
        />
        <Row
          label="Recording store"
          value={
            status.age_key?.durable ? (
              "Enabled"
            ) : (
              <span className="text-warn">Ephemeral — recordings are lost on restart</span>
            )
          }
        />
        <Row label="Internet" value={proxied ? "Through the corporate proxy" : "Direct"} />
      </div>

      {/* Delegated, not duplicated — see the file header. */}
      <button
        type="button"
        onClick={() => navigate("/setup?step=corp_network")}
        className="mt-3 flex w-full items-center justify-between rounded-lg border border-border px-3 py-2 text-left transition-colors hover:border-border-strong"
      >
        <span>
          <span className="block text-body font-medium text-foreground">Corporate proxy &amp; egress</span>
          <span className="block text-meta text-muted-foreground">
            {proxied ? (
              <>
                Upstream proxy set — <Mono>{siteConfig?.upstream_proxy_url || "configured"}</Mono>
              </>
            ) : (
              "Not configured — sandboxes go direct"
            )}
          </span>
        </span>
        <ChevronRight className="size-4 shrink-0 text-muted-foreground" />
      </button>
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
  const [state, setState] = React.useState<"loading" | "error" | "ready">("loading");
  const [status, setStatus] = React.useState<SetupStatus | null>(null);
  const [siteConfig, setSiteConfig] = React.useState<SiteConfig | null>(null);

  const load = React.useCallback(() => {
    Promise.all([setupApi.getSetupStatus(), health.getSiteConfig().catch(() => null)])
      .then(([s, cfg]) => {
        setStatus(s);
        setSiteConfig(cfg);
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
          <HostCard status={status} siteConfig={siteConfig} onRecheck={load} />
          <ModelProviderCard status={status} siteConfig={siteConfig} onChanged={load} />
          <GitHostCard status={status} siteConfig={siteConfig} onChanged={load} />
          <SshKeysPane heading="h3" />
        </div>
      )}
    </div>
  );
}
