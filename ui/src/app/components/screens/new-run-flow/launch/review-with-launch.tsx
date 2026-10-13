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
import type { LaunchedState, NewRunScreenProps } from "../new-run-route";
import { ReviewScreen } from "../review/review-screen";
import { NameRunDialog } from "./name-run-dialog";

export function ReviewWithLaunch(props: NewRunScreenProps) {
  const [naming, setNaming] = React.useState(false);
  const navigate = useNavigate();
  const { principal } = useRequestIdentity();
  const created = (result: CreateRunResult) => {
    const state: LaunchedState = { created: result, owner: principal };
    void navigate(newRunHref({ screen: "launched", runId: result.id }), { state });
  };
  return (
    <>
      <ReviewScreen {...props} onLaunch={() => setNaming(true)} />
      {naming && <NameRunDialog open onOpenChange={setNaming} onCreated={created} />}
    </>
  );
}
