/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { useLocation, useNavigate } from "react-router-dom";
import { Button } from "../ui/button";
import { Refusal } from "./refusal";
import { PAGE_NOT_FOUND, REFUSAL, RUN_NOT_FOUND } from "./copy/refusal";

// #2884: an address the console does not know keeps its URL and says so, inside the shell. Back to Runs stays in the
// view the address belongs to.
export function NotFoundPage({ admin }: { admin: boolean }) {
  const { pathname } = useLocation();
  const navigate = useNavigate();
  return (
    <div className="flex justify-center p-6">
      <Refusal
        tone="absent"
        ariaLabel={PAGE_NOT_FOUND.TITLE}
        title={PAGE_NOT_FOUND.TITLE}
        chip={RUN_NOT_FOUND.CHIP}
        body={
          <>
            {PAGE_NOT_FOUND.BODY_BEFORE}
            <span className="break-all font-mono text-xs">{pathname}</span>
            {PAGE_NOT_FOUND.BODY_AFTER}
          </>
        }
        rows={[
          { label: REFUSAL.WALL, value: RUN_NOT_FOUND.WALL, muted: true },
          { label: REFUSAL.CAN_CHANGE, value: RUN_NOT_FOUND.CAN_CHANGE },
        ]}
      >
        <Button size="sm" onClick={() => navigate(admin ? "/admin/runs" : "/runs")}>
          {PAGE_NOT_FOUND.CTA}
        </Button>
      </Refusal>
    </div>
  );
}
