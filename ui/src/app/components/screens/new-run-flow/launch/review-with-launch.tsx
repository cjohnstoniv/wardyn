/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Review with its Launch wired: Launch opens the Name dialog, and a create lands
// on Launched, carrying the run it made so the screen can show it at once.

import * as React from "react";
import { useNavigate } from "react-router-dom";
import type { CreateRunResult } from "../../../../lib/types/run-create";
import { useRequestIdentity } from "../../../wardyn/operator-context";
import { newRunHref } from "../draft/new-run-url";
import { useNewRunDraft, type LaunchedState, type NewRunScreenProps } from "../new-run-route";
import { useDriveCatalogue } from "../steps/files/file-sources";
import { ReviewScreen } from "../review/review-screen";
import { NameRunDialog } from "./name-run-dialog";
import { usePendingLaunch } from "./use-launch-run";

export function ReviewWithLaunch(props: NewRunScreenProps) {
  const [naming, setNaming] = React.useState(false);
  const navigate = useNavigate();
  const pending = usePendingLaunch();
  const { principal } = useRequestIdentity();
  const { catalogue } = useDriveCatalogue(useNewRunDraft().intent.runner_pool_id);
  // A catalogue still loading, or that failed, is unknown, not "no drives": pass nothing then.
  const ownDriveIds = React.useMemo(() => (catalogue ? new Set(catalogue.drives.filter((d) => d.owned).map((d) => d.id)) : undefined), [catalogue]);
  const created = (result: CreateRunResult) => {
    const state: LaunchedState = { created: result, owner: principal };
    void navigate(newRunHref({ screen: "launched", runId: result.id }), { state });
  };
  return (
    <>
      <ReviewScreen {...props} ownDriveIds={ownDriveIds} onLaunch={() => setNaming(true)} />
      {naming && <NameRunDialog open onOpenChange={setNaming} onCreated={created} pending={pending} />}
    </>
  );
}
