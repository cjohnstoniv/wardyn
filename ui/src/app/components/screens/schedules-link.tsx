/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Runs header's link to Schedules, shown only while scheduled runs are on. Its own small module: the Runs
// screen is in the eager entry graph, and the Schedules screen is a lazy route that must stay out of it.

import { Link } from "react-router-dom";
import { scheduledRuns } from "../../lib/schedule";
import { Button } from "../ui/button";
import { RUN_SCHEDULE } from "../wardyn/copy/run-schedules";
import { useShellSetupStatus } from "../wardyn/model-access-context";

export function SchedulesLink({ admin }: { admin: boolean }) {
  const { status } = useShellSetupStatus();
  if (scheduledRuns(status) !== true) return null;
  return (
    <Button asChild variant="outline" size="sm">
      <Link to={admin ? "/admin/runs/schedules" : "/runs/schedules"}>{RUN_SCHEDULE.LINK}</Link>
    </Button>
  );
}
