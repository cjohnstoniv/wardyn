/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { BarrierMark } from "../../../../wardyn/barrier-mark";
import { NR_REVIEW } from "../../../../wardyn/copy/new-run/review";
import { NR_RUN } from "../../../../wardyn/copy/new-run/run";
import { NR_SHARED } from "../../../../wardyn/copy/new-run/shared";
import { RECORDING_DISABLED_TITLE } from "../../../../wardyn/copy";
import { useRecordingDisabled } from "../../../../../lib/hooks/use-recording-disabled";
import { HOSTING_LABEL } from "../../../../../lib/runner-pool-refusals";
import { draftSource } from "../provenance";
import { ReviewGroup } from "../review-group";
import { onlineRunners } from "../review-seams";
import { stepLocation, type GroupProps } from "./group-props";
import { JumpButton } from "./jump-button";

export function WhereGroup({ draft, pre, docRows, go }: GroupProps) {
  const pool = pre?.runner_pool;
  const slots = pre?.runner_pool_slots;
  const online = onlineRunners(pre);
  const recordingOff = useRecordingDisabled();
  const hosting = pool ? HOSTING_LABEL[pool.hosting_type] : undefined;
  const poolLine = !hosting ? undefined
    : !slots ? hosting
    : slots.max === undefined ? NR_SHARED.JOIN(hosting, NR_RUN.RUNNING_ONLY(slots.used))
    : NR_REVIEW.WHERE_DETAIL(hosting, slots.used, slots.max);
  const detail = online === undefined ? poolLine : poolLine ? NR_SHARED.JOIN(poolLine, NR_REVIEW.WHERE_ONLINE(online)) : NR_REVIEW.WHERE_ONLINE(online);
  const cc = pre?.enforced_confinement_class ?? draft.intent.confinement_class;
  const provenance = pool
    ? pool.selection === "explicit" ? NR_REVIEW.PROV_SELECTED : pool.selection === "organisation_default" ? NR_REVIEW.PROV_ORG_DEFAULT : undefined
    : draftSource(draft, ["runner_pool_id"], NR_REVIEW.PROV_SELECTED);
  return (
    <ReviewGroup
      id="where"
      label={NR_REVIEW.GROUP_WHERE}
      value={pool && NR_REVIEW.WHERE_VALUE(pool.name)}
      mark={cc && <BarrierMark cc={cc} size="sm" />}
      detail={detail}
      provenance={provenance}
      docRows={docRows}
      editor={() => <JumpButton label={NR_REVIEW.EDIT_RUN} to={stepLocation("run")} go={go} />}
    >
      {recordingOff !== undefined && (
        <p data-review-recording className="text-sm">
          <strong className="font-medium">{NR_REVIEW.RECORDING_LABEL}</strong>{" "}
          <span className="text-muted-foreground">{recordingOff ? RECORDING_DISABLED_TITLE : NR_REVIEW.RECORDING_ON}</span>
        </p>
      )}
    </ReviewGroup>
  );
}
