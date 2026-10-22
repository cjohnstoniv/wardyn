/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { NR_REVIEW } from "../../../../wardyn/copy/new-run/review";
import { NR_SHARED } from "../../../../wardyn/copy/new-run/shared";
import { useShellSetupStatus } from "../../../../wardyn/model-access-context";
import { scheduledRunsOn, startInstant, whenText } from "../../../../../lib/schedule";
import { launchStart, type LaunchStart } from "../../draft/launch-start";
import type { RunIntent } from "../../draft/run-intent";
import { ReviewGroup } from "../review-group";
import { stepLocation, type GroupProps } from "./group-props";
import { JumpButton } from "./jump-button";

const VALUE = { agent_task: NR_REVIEW.VALUE_AGENT, application: NR_REVIEW.VALUE_APPLICATION, environment: NR_REVIEW.VALUE_ENVIRONMENT } as const;

function timing(intent: RunIntent, start?: LaunchStart): string | undefined {
  if (intent.shape === "application") return NR_REVIEW.TIMING_APPLICATION;
  if (start) {
    const at = start.problem ? null : startInstant(start.choice);
    return at === null ? NR_REVIEW.TIMING_CHOOSE : NR_SHARED.JOIN(NR_REVIEW.TIMING_STARTS(whenText(at, start.choice.zone)), NR_REVIEW.TIMING_CHECKED);
  }
  if (intent.experience === "background") return NR_REVIEW.TIMING_NOW;
  return intent.experience === "interactive" ? NR_REVIEW.TIMING_INTERACTIVE : undefined;
}

export function RunGroup({ draft, docRows, go }: GroupProps) {
  const { status } = useShellSetupStatus();
  const detail = timing(draft.intent, launchStart(draft, scheduledRunsOn(status), Date.now()));
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
    />
  );
}
