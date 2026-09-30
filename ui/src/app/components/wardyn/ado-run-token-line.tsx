/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// New Run's Azure DevOps lines for a row that creates a token for each run
// (#1428): what the run's own token carries, and, when the person has not
// connected (or their organisation blocks token creation), the launch note that
// says so before Launch is pressed. Both read the caller's own /me/scm-access
// answer; neither says anything for a row that creates no token.
import { Loader2 } from "lucide-react";
import { ADO_PAT } from "../../lib/ado-pat-copy";
import { newRunTokenCaps, type PatRefusalNote } from "../../lib/ado-pat-display";
import type { SCMAccessPAT } from "../../lib/types/ado-pat";
import { Button } from "../ui/button";

/** The line under the saved policy: "Azure DevOps: a token for this run with
 *  Read code, Push …. It's revoked when the run ends." Nothing when neither the
 *  policy nor the row names any access. */
export function AdoRunTokenLine({ policyCaps, defaults, className }: { policyCaps: unknown; defaults?: string[]; className?: string }) {
  const names = newRunTokenCaps(policyCaps, defaults);
  if (names.length === 0) return null;
  return (
    <p className={className ?? "text-xs text-muted-foreground"} data-testid="ado-run-token-line">
      {ADO_PAT.NEWRUN_LINE_PREFIX}
      <b className="font-semibold text-foreground">{names.join(", ")}</b>
      {ADO_PAT.NEWRUN_LINE_SUFFIX}
    </p>
  );
}

/** The note beside the token line when a launch on this row would be refused:
 *  connect once first, or the organisation blocks token creation. */
export function AdoLaunchNote({
  access,
  refusal,
  connecting,
  onConnect,
}: {
  access: SCMAccessPAT | undefined;
  /** What the last refused launch said about the token (patRefusalNote). */
  refusal?: PatRefusalNote | null;
  connecting: boolean;
  onConnect: () => void;
}) {
  // A refusal names the row as one that creates tokens even when the answer
  // above could not be read.
  if (access?.token_mode !== "minted_pat" && !refusal) return null;
  if (access?.state === "blocked" || refusal === "blocked") {
    return (
      <div role="alert" className="rounded-lg border border-warning/30 bg-warning-subtle px-3 py-2 text-body text-foreground" data-testid="ado-launch-note">
        {ADO_PAT.LAUNCH_POLICY_REFUSED}
      </div>
    );
  }
  if (refusal !== "connect" && access?.state !== "not_configured" && access?.state !== "expired_signin") return null;
  return (
    <div role="status" className="rounded-lg border border-info/30 bg-info-subtle px-3 py-2 text-body text-foreground" data-testid="ado-launch-note">
      <p className="font-medium">{ADO_PAT.LAUNCH_NOT_CONNECTED}</p>
      <Button type="button" size="sm" variant="outline" className="mt-2" disabled={connecting} onClick={onConnect}>
        <Loader2 className={connecting ? "size-4 animate-spin" : "hidden"} />
        {ADO_PAT.MEMBER_CONNECT}
      </Button>
    </div>
  );
}
