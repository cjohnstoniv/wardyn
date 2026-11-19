/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { Chip } from "../../../../wardyn/primitives";
import { NR_REVIEW } from "../../../../wardyn/copy/new-run/review";
import { NR_RUN } from "../../../../wardyn/copy/new-run/run";
import { NR_SHARED } from "../../../../wardyn/copy/new-run/shared";
import { useShellSetupStatus } from "../../../../wardyn/model-access-context";
import { scheduledRuns, startInstant, whenText } from "../../../../../lib/schedule";
import { launchStart, startHeld, startHeldText, type LaunchStart, type StartHeld } from "../../draft/launch-start";
import type { RunIntent } from "../../draft/run-intent";
import { appsOf, hasPorts, isServer } from "../../lenses/apps-lens";
import { mayKeepRunning } from "../../lenses/service-lens";
import { serviceReviewLine } from "../../../../wardyn/copy/run-service";
import { ReviewGroup } from "../review-group";
import { stepLocation, type GroupProps } from "./group-props";
import { JumpButton } from "./jump-button";

const VALUE = { agent_task: NR_REVIEW.VALUE_AGENT, application: NR_REVIEW.VALUE_APPLICATION, environment: NR_REVIEW.VALUE_ENVIRONMENT } as const;

function timing(intent: RunIntent, start?: LaunchStart, held?: StartHeld | null): string | undefined {
  if (isServer(intent)) return NR_REVIEW.TIMING_APPLICATION_SERVER;
  if (intent.shape === "application") return NR_REVIEW.TIMING_APPLICATION;
  if (start) {
    const at = start.problem ? null : startInstant(start.choice);
    return at === null ? NR_REVIEW.TIMING_CHOOSE : NR_SHARED.JOIN(NR_REVIEW.TIMING_STARTS(whenText(at, start.choice.zone)), NR_REVIEW.TIMING_CHECKED);
  }
  // A Later start the switch does not keep reads as held, in the Name dialog's words, never as Run now.
  if (held) return NR_SHARED.JOIN(NR_RUN.MODE_AUTONOMOUS, startHeldText(held));
  if (intent.experience === "background") return NR_REVIEW.TIMING_NOW;
  return intent.experience === "interactive" ? NR_REVIEW.TIMING_INTERACTIVE : undefined;
}

export function RunGroup({ draft, preview, docRows, go }: GroupProps) {
  const { status } = useShellSetupStatus();
  const on = scheduledRuns(status);
  const held = startHeld(draft, on);
  const detail = timing(draft.intent, launchStart(draft, on, Date.now()), held);
  // The ports the server will actually keep: once the policy preview answers, what governance left of them.
  const apps = hasPorts(draft.intent) ? (preview ? (preview.spec.ui_apps ?? []) : appsOf(draft.intent)) : [];
  const service = mayKeepRunning(draft.intent) ? draft.intent.service : undefined;
  return (
    <ReviewGroup
      id="run"
      label={NR_REVIEW.GROUP_RUN}
      value={draft.intent.shape && VALUE[draft.intent.shape]}
      detail={detail}
      provenance={NR_REVIEW.PROV_RUN_SETUP}
      attention={held !== null}
      docRows={docRows}
      editor={() => (
        <>
          {detail && <p className="text-sm text-muted-foreground">{detail}</p>}
          <JumpButton label={NR_REVIEW.EDIT_RUN} to={stepLocation("run")} go={go} />
        </>
      )}
    >
      {service && <p data-service className="my-1.5 rounded-md border border-border bg-surface-2 px-2.5 py-2 text-sm">{serviceReviewLine(service)}</p>}
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
