/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { Chip } from "../../../../wardyn/primitives";
import { NR_REVIEW } from "../../../../wardyn/copy/new-run/review";
import type { RunIntent } from "../../draft/run-intent";
import { appsOf, isServer } from "../../lenses/apps-lens";
import { ReviewGroup } from "../review-group";
import { stepLocation, type GroupProps } from "./group-props";
import { JumpButton } from "./jump-button";

const VALUE = { agent_task: NR_REVIEW.VALUE_AGENT, application: NR_REVIEW.VALUE_APPLICATION, environment: NR_REVIEW.VALUE_ENVIRONMENT } as const;

function timing(intent: RunIntent): string | undefined {
  if (isServer(intent)) return NR_REVIEW.TIMING_APPLICATION_SERVER;
  if (intent.shape === "application") return NR_REVIEW.TIMING_APPLICATION;
  if (intent.experience === "background") return NR_REVIEW.TIMING_NOW;
  return intent.experience === "interactive" ? NR_REVIEW.TIMING_INTERACTIVE : undefined;
}

export function RunGroup({ draft, preview, docRows, go }: GroupProps) {
  const detail = timing(draft.intent);
  // The ports the server will actually keep: once the policy preview answers, what governance left of them.
  const apps = isServer(draft.intent) ? (preview ? (preview.spec.ui_apps ?? []) : appsOf(draft.intent)) : [];
  return (
    <ReviewGroup
      id="run"
      label={NR_REVIEW.GROUP_RUN}
      value={draft.intent.shape && VALUE[draft.intent.shape]}
      detail={detail}
      provenance={NR_REVIEW.PROV_RUN_SETUP}
      docRows={docRows}
      editor={() => (
        <>
          {detail && <p className="text-sm text-muted-foreground">{detail}</p>}
          <JumpButton label={NR_REVIEW.EDIT_RUN} to={stepLocation("run")} go={go} />
        </>
      )}
    >
      {apps.length > 0 && (
        <div className="my-1.5 grid gap-1.5">
          {apps.map((a) => (
            <div key={`${a.name}:${a.port}`} data-app={a.name} className="grid grid-cols-[minmax(100px,1fr)_auto] items-center gap-x-2 gap-y-1 rounded-md border border-border bg-surface-2 px-2.5 py-2 text-sm max-[520px]:grid-cols-1 max-[520px]:justify-items-start">
              <strong className="font-medium">{a.name}</strong>
              <Chip mono>{NR_REVIEW.APP_PORT(a.port)}</Chip>
              <span className="col-span-full min-w-0 break-words text-xs text-muted-foreground">{NR_REVIEW.APP_OPENS(a.path || "/")}</span>
            </div>
          ))}
        </div>
      )}
    </ReviewGroup>
  );
}
