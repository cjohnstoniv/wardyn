/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The note a covered write site shows when the server answered 202 instead of saving (M3 S4): the change
// is held for a second person, and nothing has changed yet. An inline amber Note, never a toast and never
// worded as a save. Every site that can receive a PendingChangeError renders this one.
import { Link, useInRouterContext } from "react-router-dom";
import { CHANGES } from "../../../lib/governance-copy";
import { Note } from "./display";

// The Changes tab, by URL: the Governance screen reads ?tab=. Under /admin because the securityOps
// screens, including this one, live in the Admin view.
export const CHANGES_TAB_PATH = "/admin/governance?tab=changes";

export function SubmittedNote() {
  // Some screens mount this without a router in their tests; the note is the same without the link.
  const routed = useInRouterContext();
  return (
    <Note tone="amber" role="status">
      <b className="font-semibold">{CHANGES.SUBMITTED_TITLE}</b>
      <span>{CHANGES.SUBMITTED_BODY}</span>
      {routed && (
        <Link to={CHANGES_TAB_PATH} className="font-medium underline underline-offset-2">
          {CHANGES.SUBMITTED_LINK}
        </Link>
      )}
    </Note>
  );
}
