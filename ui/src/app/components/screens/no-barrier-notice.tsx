/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { Link } from "react-router-dom";
import { RotateCw } from "lucide-react";
import { Button } from "../ui/button";
import { Mono } from "../wardyn/code-block";
import { CopyButton } from "../wardyn/copy-button";
import { NO_BARRIER } from "../wardyn/copy";
import { Notice } from "../wardyn/notice";

/**
 * The one place the console says no barrier can be built. Someone who can reach the Environment step gets the
 * command, Re-check and the link; everyone else gets who can fix it and nothing to click.
 */
export function NoBarrierNotice({ canFix, onRecheck }: { canFix: boolean; onRecheck?: () => void }) {
  return (
    <Notice
      tone="danger"
      live
      title={NO_BARRIER.BANNER_TITLE}
      actions={
        canFix && (
          <>
            <div className="flex items-center gap-1.5 rounded-md border border-border bg-card px-2 py-1">
              <Mono className="text-foreground">{NO_BARRIER.SETUP_COMMAND}</Mono>
              <CopyButton
                text={NO_BARRIER.SETUP_COMMAND}
                label="Copy setup command"
                className="rounded p-0.5 text-muted-foreground hover:text-foreground"
              />
            </div>
            {onRecheck && (
              <Button size="sm" variant="outline" onClick={onRecheck}>
                <RotateCw className="size-3.5" /> {NO_BARRIER.RECHECK}
              </Button>
            )}
            <Link to={NO_BARRIER.ADMIN_ROUTE} className="text-sm font-medium underline underline-offset-2">
              {NO_BARRIER.CTA}
            </Link>
          </>
        )
      }
    >
      {canFix ? NO_BARRIER.BANNER_BODY : NO_BARRIER.BANNER_MEMBER}
    </Notice>
  );
}
