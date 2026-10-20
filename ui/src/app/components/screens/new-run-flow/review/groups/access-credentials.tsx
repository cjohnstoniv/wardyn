/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The credentials a run's tools bring, under the hosts in Access. It lists
// a kubectl cluster's service account, named after the cluster, with the
// namespaces and what the run may do there: the organisation row says those,
// and the preview's credential only says that one is delivered. While local MCP
// servers are on it also lists the secrets of a server that runs in the sandbox,
// named from the draft's own component.

import * as React from "react";
import { components as api } from "../../../../../lib/api/components";
import type { MyComponents } from "../../../../../lib/types/components";
import type { AccessFacts } from "../../../../../lib/types/new-run-access";
import type { RunIntent } from "../../draft/run-intent";
import { NR_REVIEW } from "../../../../wardyn/copy/new-run/review";
import { NR_TOOLS } from "../../../../wardyn/copy/new-run/tools";
import { Chip } from "../../../../wardyn/primitives";
import { clusterName, joined, kubectlLimits, type KubectlCLIView } from "../../steps/tools/tool-access";
import { isLocalMcp, secretNames, useHarnessNames, useLocalMcpOn } from "../../steps/tools/local-mcp";
import { rowLabel } from "../../lenses/tools-lens";
import { deliveryLabel } from "../../steps/tools/totals";

const KUBE_API = "kube_api";

/** The person's catalogue, read once and only when a credential needs a row of it. */
function useOrgRows(needed: boolean): MyComponents["org"] {
  const [org, setOrg] = React.useState<MyComponents["org"]>([]);
  React.useEffect(() => {
    if (!needed) return;
    let live = true;
    api.mine().then((m) => live && setOrg(m.org), () => undefined);
    return () => { live = false; };
  }, [needed]);
  return org;
}

export function AccessCredentials({ access, intent }: { access: AccessFacts | undefined; intent?: RunIntent }) {
  const localMcp = useLocalMcpOn();
  const harnessNames = useHarnessNames();
  const localRows = new Set((access?.rows ?? []).filter((r) => localMcp && isLocalMcp(r)).map((r) => r.id));
  const secrets = intent ? secretNames(access, intent) : new Map<string, string>();
  const credentials = (access?.credentials ?? []).filter((c) => c.kind === KUBE_API || localRows.has(c.row));
  const org = useOrgRows(credentials.length > 0);
  if (credentials.length === 0) return null;
  return (
    <>
      <h4 className="label-eyebrow mt-1">{NR_REVIEW.POLICY_CREDENTIALS}</h4>
      {credentials.map((c) => {
        const row = org.find((o) => o.id === c.row);
        const accessRow = access?.rows.find((r) => r.id === c.row);
        const forRow = accessRow && (localRows.has(accessRow.id) ? rowLabel(accessRow, harnessNames) : accessRow.name);
        const delivery = deliveryLabel(c.delivery);
        const namespaces = ((row?.cli as KubectlCLIView | undefined)?.namespaces ?? []).join(", ");
        return (
          <div key={c.id} className="flex items-start justify-between gap-2 text-sm">
            <span className="min-w-0">
              {row ? NR_TOOLS.KUBE_CREDENTIAL(clusterName(row.name)) : secrets.get(c.id) ?? c.label}
              {forRow && <span className="text-muted-foreground"> {NR_TOOLS.FOR_ROW(forRow)}</span>}
              {row && <span className="block text-muted-foreground">{joined([...(namespaces ? [NR_TOOLS.KUBE_NAMESPACES(namespaces)] : []), ...kubectlLimits(row.cli)])}</span>}
            </span>
            {delivery && <Chip className="whitespace-normal">{delivery}</Chip>}
          </div>
        );
      })}
    </>
  );
}
