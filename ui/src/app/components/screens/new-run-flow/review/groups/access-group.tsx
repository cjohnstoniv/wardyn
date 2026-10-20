/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { Button } from "../../../../ui/button";
import { Chip } from "../../../../wardyn/primitives";
import { NR_REVIEW } from "../../../../wardyn/copy/new-run/review";
import { NR_SHARED } from "../../../../wardyn/copy/new-run/shared";
import { NR_TOOLS } from "../../../../wardyn/copy/new-run/tools";
import { rowLabel } from "../../lenses/tools-lens";
import { useHarnessNames, useLocalMcpOn, secretNames } from "../../steps/tools/local-mcp";
import { credentialLabel, deliveryLabel } from "../../steps/tools/totals";
import { ReviewGroup } from "../review-group";
import { stepLocation, type GroupProps } from "./group-props";
import { JumpButton } from "./jump-button";

export function AccessGroup({ draft, pre, preview, docRows, go }: GroupProps) {
  const access = pre?.access ?? preview?.access;
  const localMcp = useLocalMcpOn();
  const harnessNames = useHarnessNames();
  const secrets = secretNames(access, draft.intent);
  const rowOf = new Map((access?.rows ?? []).map((r) => [r.id, r]));
  const provider = pre?.model_credential?.provider;
  const hosts = access?.hosts ?? [];
  const source = preview?.source.name;
  return (
    <ReviewGroup
      id="access"
      label={NR_REVIEW.GROUP_ACCESS}
      value={access && (hosts.length === 1 ? NR_REVIEW.POLICY_HOST_ONE : NR_REVIEW.POLICY_HOSTS(hosts.length))}
      provenance={source ? NR_SHARED.JOIN(NR_REVIEW.PROV_SOURCE_POLICY, source) : NR_REVIEW.PROV_SOURCE_POLICY}
      docRows={docRows}
      editor={(close) => (
        <>
          <p className="text-sm text-muted-foreground">{NR_REVIEW.ACCESS_NOTE}</p>
          {hosts.map((h) => (
            <div key={h.host} className="flex items-center justify-between gap-2 text-sm">
              <code className="font-mono text-xs">{h.host}</code>
              {h.source.name && <Chip>{h.source.name}</Chip>}
            </div>
          ))}
          {localMcp && access && access.credentials.length > 0 && (
            <>
              <h4 className="label-eyebrow">{NR_TOOLS.CREDENTIALS_TITLE}</h4>
              {access.credentials.map((c) => {
                const row = rowOf.get(c.row);
                const label = secrets.get(c.id) ?? credentialLabel(c, row?.name, provider && row ? new Map([[row.name, provider]]) : undefined);
                const delivery = deliveryLabel(c.delivery);
                return (
                  <div key={c.id} className="flex items-center justify-between gap-2 text-sm">
                    <span className="min-w-0 [overflow-wrap:anywhere]">
                      {label && <span>{label} </span>}
                      {row && <span className="text-muted-foreground">{NR_TOOLS.FOR_ROW(rowLabel(row, harnessNames))}</span>}
                    </span>
                    {delivery && <Chip>{delivery}</Chip>}
                  </div>
                );
              })}
            </>
          )}
          <JumpButton label={NR_REVIEW.EDIT_TOOLS_ACCESS} to={stepLocation("tools")} go={go} />
          <Button type="button" variant="ghost" className="w-fit" onClick={close}>{NR_REVIEW.DONE}</Button>
        </>
      )}
    />
  );
}
