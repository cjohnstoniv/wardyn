/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { Button } from "../../../../ui/button";
import { Chip } from "../../../../wardyn/primitives";
import { NR_REVIEW } from "../../../../wardyn/copy/new-run/review";
import { NR_SHARED } from "../../../../wardyn/copy/new-run/shared";
import { ReviewGroup } from "../review-group";
import { AccessCredentials } from "./access-credentials";
import { stepLocation, type GroupProps } from "./group-props";
import { JumpButton } from "./jump-button";

export function AccessGroup({ draft, pre, preview, docRows, go }: GroupProps) {
  const access = pre?.access ?? preview?.access;
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
          <AccessCredentials access={access} intent={draft.intent} />
          <JumpButton label={NR_REVIEW.EDIT_TOOLS_ACCESS} to={stepLocation("tools")} go={go} />
          <Button type="button" variant="ghost" className="w-fit" onClick={close}>{NR_REVIEW.DONE}</Button>
        </>
      )}
    />
  );
}
