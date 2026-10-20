/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// What each kind of tool card brings, read from what the server already says of
// it: the organisation row's non-secret face (OrgComponentView) and, for the
// GitHub CLI, the repositories the preview's facts name. Nothing here asks the
// server for more, and a row's secret names are never on the wire.

import type * as React from "react";
import type { CLIView, ComponentDeliveryMode, ComponentFact, OrgComponentView } from "../../../../../lib/types/components";
import type { AccessFacts } from "../../../../../lib/types/new-run-access";
import type { SetupStatus } from "../../../../../lib/types/setup";
import { NR_SHARED } from "../../../../wardyn/copy/new-run/shared";
import { NR_TOOLS } from "../../../../wardyn/copy/new-run/tools";
import { hostBadge } from "../../lenses/tools-lens";
import { BADGE } from "./totals";
import { NOTHING, type ToolAccess, type ToolHost } from "./tool-card";

export const MODE_DELIVERY: Record<ComponentDeliveryMode, string> = {
  header: NR_TOOLS.DELIVERY_PROXY, env: NR_TOOLS.DELIVERY_ENV, file: NR_TOOLS.DELIVERY_FILE,
};

/** The host the GitHub CLI reaches; the server adds it to the row's own hosts (ghscope.APIHost). */
const GITHUB_API_HOST = "api.github.com";
/** A long repository list shows this many, then "+{n} more". */
const REPOS_SHOWN = 2;

export const joined = (parts: readonly string[]): string => parts.reduce((a, b) => NR_SHARED.JOIN(a, b));

/** The hosts, each carrying the server's verdict for this run once the row is on it and the server has answered. */
function hostsOf(hosts: readonly string[], facts?: { access: AccessFacts | undefined; answered: boolean }): ToolHost[] {
  const answered = facts?.answered ?? false;
  return [...new Set(hosts)].map((host) => {
    const fact = facts?.access?.hosts.find((h) => h.host === host.replace(/:\d+$/, ""));
    return fact ? { host, chip: BADGE[hostBadge(fact, answered)] } : { host };
  });
}

/** Label/value lines for a dialog's Other. */
function Facts({ rows }: { rows: readonly (readonly [string, React.ReactNode])[] }) {
  return (
    <dl className="grid grid-cols-[110px_minmax(0,1fr)] gap-x-3 gap-y-1.5 text-sm">
      {rows.map(([label, value]) => (
        <div key={label} className="contents">
          <dt className="text-muted-foreground">{label}</dt>
          <dd className="[overflow-wrap:anywhere]">{value}</dd>
        </div>
      ))}
    </dl>
  );
}

/**
 * What the kubectl backend adds to a CLI row's face (pkg/client CLIView.Cluster and .Namespaces: the API server's
 * host and the namespaces it reaches; `access` is read or write). Declared here until that lane's Go and its TS
 * mirror are on this branch, which wire-parity.test.ts needs to pin them.
 */
export type KubectlCLIView = CLIView & { cluster?: string; namespaces?: string[] };

/** The server's `features.kubectl_cli` on /setup/status, which the same lane adds. Absent reads as off. */
export const kubectlSwitchOn = (status: SetupStatus | null): boolean => !!(status as unknown as { features?: { kubectl_cli?: boolean } } | null)?.features?.kubectl_cli;

/** What the run's facts say once this row is on it; absent on a row that is not. */
export interface RunFacts {
  access: AccessFacts | undefined;
  answered: boolean;
}

/** "kubectl · staging" is the cluster "staging"; a name that does not start that way is the cluster's own. */
export const clusterName = (name: string): string => name.replace(/^kubectl\s*·\s*/i, "") || name;

/** What a kubectl row lets the run do, in words: read only or read and write when the row says, then no exec (the door always refuses it). */
export function kubectlLimits(view: OrgComponentView["cli"]): string[] {
  const level = view?.access === "write" ? NR_TOOLS.KUBE_READ_WRITE : view?.access === "read" ? NR_TOOLS.KUBE_READ_ONLY : undefined;
  return [...(level ? [level] : []), NR_TOOLS.KUBE_NO_EXEC];
}

export function kubectlAccess(row: OrgComponentView, facts?: RunFacts): ToolAccess {
  const view = row.cli as KubectlCLIView | undefined;
  const cluster = clusterName(row.name);
  const write = view?.access === "write";
  const known = write || view?.access === "read";
  const namespaces = (view?.namespaces ?? []).join(", ");
  const server = view?.cluster;
  return {
    network: hostsOf(server ? [...row.hosts, server] : row.hosts, facts),
    credentials: [{ name: NR_TOOLS.KUBE_CREDENTIAL(cluster), delivery: NR_TOOLS.DELIVERY_PROXY }],
    credentialNote: NR_TOOLS.KUBE_CREDENTIAL_NOTE,
    files: [],
    other: joined([...(namespaces ? [NR_TOOLS.KUBE_NAMESPACES(namespaces)] : []), ...kubectlLimits(view)]),
    otherDetail: (
      <Facts
        rows={[
          [NR_TOOLS.KUBE_CLUSTER, <>{cluster}{server && <> <span className="font-mono text-body text-muted-foreground">{server}</span></>}</>],
          ...(namespaces ? [[NR_TOOLS.KUBE_NAMESPACES_LABEL, namespaces] as const] : []),
          ...(known ? [[NR_TOOLS.KUBE_ACCESS, write ? NR_TOOLS.KUBE_WRITE_DETAIL : NR_TOOLS.KUBE_READ_DETAIL] as const] : []),
          [NR_TOOLS.KUBE_EXEC, NR_TOOLS.KUBE_EXEC_DETAIL],
        ]}
      />
    ),
  };
}

/** The GitHub CLI acts only on the run's GitHub repositories; with none it brings nothing. */
export function githubAccess(row: OrgComponentView, repos: readonly string[], facts?: RunFacts): ToolAccess {
  if (repos.length === 0) return NOTHING;
  const more = repos.length - REPOS_SHOWN;
  const shown = NR_TOOLS.GITHUB_REPOS(repos.slice(0, REPOS_SHOWN).join(", "));
  return {
    network: hostsOf([...row.hosts, GITHUB_API_HOST], facts),
    credentials: [{ name: NR_TOOLS.GITHUB_APP, delivery: NR_TOOLS.DELIVERY_PROXY }],
    credentialNote: NR_TOOLS.DELIVERY_GITHUB,
    files: [],
    other: more > 0 ? `${shown} ${NR_TOOLS.MORE(more)}` : shown,
    otherDetail: (
      <>
        <p className="text-sm">{NR_TOOLS.GITHUB_REPOS_LABEL}</p>
        <ul className="flex flex-col gap-1">{repos.map((r) => <li key={r}><code className="font-mono text-sm">{r}</code></li>)}</ul>
      </>
    ),
  };
}

/** A remote MCP server an organisation grants: its host, and a credential only as the way it is delivered. */
export function mcpAccess(row: OrgComponentView, facts?: RunFacts): ToolAccess {
  return {
    network: hostsOf(row.hosts, facts),
    credentials: row.secrets.map((s) => ({ delivery: MODE_DELIVERY[s.delivery.mode] })),
    files: [],
    other: NR_SHARED.JOIN(NR_TOOLS.FROM_MCP, NR_TOOLS.MCP_REMOTE),
    otherDetail: (
      <Facts
        rows={[
          [NR_TOOLS.DETAIL_WHERE, NR_TOOLS.MCP_REMOTE],
          ...(row.mcp ? [[NR_TOOLS.MCP_URL, <span key="url" className="font-mono text-body">{row.mcp.url}</span>] as const] : []),
          [NR_TOOLS.DETAIL_MCP, NR_TOOLS.DETAIL_MCP_BODY],
        ]}
      />
    ),
  };
}

/** "github.com/acme/web-app", "https://github.com/acme/web-app.git" and "acme/web-app" are all acme/web-app. */
const repoSlug = (url: string): string => url.replace(/^https?:\/\//, "").replace(/^github\.com\//i, "").replace(/\.git$/, "");

/**
 * The GitHub repositories the run's clones go through the App for, from the preview's facts. The GitHub CLI
 * acts on exactly those (the server's githubCLIRepos), and its row says nothing of its own.
 */
export function githubRepos(components: readonly ComponentFact[] | undefined): string[] {
  const repos = (components ?? []).filter((c) => c.kind === "git_provider" && c.provider === "github" && c.lane === "app").flatMap((c) => c.repos ?? []);
  return [...new Set(repos.map(repoSlug))];
}
