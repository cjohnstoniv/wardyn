/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// New Run's Azure DevOps lines for a row that creates a token for each run
// (#1428): what the run's own token carries, and, when the person has not
// connected (or their organisation blocks token creation), the launch note that
// says so. Tools and access draws both in its Credentials card, on a run that
// carries an Azure DevOps credential; the Name dialog draws the note under a
// launch the token refused. Neither says anything for a row that creates no token.
import { Loader2 } from "lucide-react";
import { ADO } from "../../lib/ado-entra-copy";
import { ADO_PAT } from "../../lib/ado-pat-copy";
import { joinOrgs, newRunTokenCaps, type PatRefusalNote } from "../../lib/ado-pat-display";
import { ownTokenOrgsNeedingToken } from "../../lib/scm-access-display";
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

/** Which note a row that creates a token for each run calls for: the
 *  organisation blocks token creation ("blocked"), the person has to connect
 *  ("connect"), or nothing. A refusal names the row as one that creates tokens
 *  even when the answer above could not be read. */
export function adoLaunchNoteKind(access: SCMAccessPAT | undefined, refusal?: PatRefusalNote | null): PatRefusalNote | null {
  if (access?.token_mode !== "minted_pat" && !refusal) return null;
  if ((access?.state === "expired_signin" && access.cause === "blocked") || refusal === "blocked") return "blocked";
  // A row the console cannot redeem is the admin's to fix: no Connect to press.
  if (access?.cause === "ado_pat_needs_console_app") return null;
  if (refusal !== "connect" && access?.state !== "not_configured" && access?.state !== "expired_signin") return null;
  return "connect";
}

/** The note beside the token line when a launch on this row would be refused:
 *  connect once first, or the organisation blocks token creation. Drawn as a
 *  row of whatever holds it: the sentence, and Connect at its right. */
export function AdoLaunchNote({
  access,
  refusal,
  connecting,
  onConnect,
  blockedUrl,
  onFallbackClick,
}: {
  access: SCMAccessPAT | undefined;
  /** What the last refused launch said about the token (patRefusalNote). */
  refusal?: PatRefusalNote | null;
  connecting: boolean;
  onConnect: () => void;
  /** The sign-in URL when the browser blocked the popup: a plain link to it. */
  blockedUrl?: string | null;
  onFallbackClick?: () => void;
}) {
  const kind = adoLaunchNoteKind(access, refusal);
  if (!kind) return null;
  if (kind === "blocked") {
    return (
      <p role="alert" className="text-sm" data-testid="ado-launch-note">
        {ADO_PAT.LAUNCH_POLICY_REFUSED}
      </p>
    );
  }
  return (
    <div role="status" className="flex flex-wrap items-center justify-between gap-2 text-sm" data-testid="ado-launch-note">
      <p className="min-w-0 flex-1 basis-60">{ADO_PAT.LAUNCH_NOT_CONNECTED}</p>
      <Button type="button" size="sm" variant="outline" disabled={connecting} onClick={onConnect}>
        <Loader2 className={connecting ? "size-4 animate-spin" : "hidden"} />
        {ADO_PAT.MEMBER_CONNECT}
      </Button>
      {blockedUrl && (
        <p className="basis-full text-muted-foreground">
          {ADO.CONNECT_POPUP_BLOCKED}{" "}
          <a href={blockedUrl} target="_blank" rel="noopener noreferrer" className="underline" onClick={onFallbackClick}>{ADO.CONNECT_POPUP_OPEN}</a>
        </p>
      )}
    </div>
  );
}

/** With own-token rows for several organisations (#2146): the organisations a launch needs a token added for.
 *  None with one organisation (its page is unchanged) or when every token is in place. */
export function ownTokenNoteOrgs(rows: readonly SCMAccessPAT[] | undefined): string[] {
  return rows && rows.length >= 2 ? ownTokenOrgsNeedingToken(rows) : [];
}

export function AdoOwnTokenNote({ rows }: { rows: readonly SCMAccessPAT[] | undefined }) {
  const orgs = ownTokenNoteOrgs(rows);
  if (orgs.length === 0) return null;
  return (
    <p role="status" className="text-sm" data-testid="ado-own-token-note">
      {ADO_PAT.OWN_ORGS_ADD_FIRST(joinOrgs(orgs))}
    </p>
  );
}
