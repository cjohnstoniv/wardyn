/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The note a covered write site shows when the server answered 202 instead of saving (M3 S4): the change
// is held for a second person, and nothing has changed yet. An inline amber Note, never a toast and never
// worded as a save. Every site that can receive a PendingChangeError renders this one. A site that knows the
// held change passes it, and the note offers Add a note (Edit note once one is saved).
import * as React from "react";
import { Link, useInRouterContext } from "react-router-dom";
import { CHANGES } from "../../../lib/governance-copy";
import type { GovernanceChange } from "../../../lib/types";
import { Button } from "../../ui/button";
import { CHANGE_NOTE } from "../../wardyn/copy/change-note";
import { ChangeNoteForm } from "./change-note-form";
import { Note } from "./display";

// The Changes tab, by URL: the Governance screen reads ?tab=. Under /admin because the securityOps
// screens, including this one, live in the Admin view.
export const CHANGES_TAB_PATH = "/admin/governance?tab=changes";

export function SubmittedNote({ change }: { change?: GovernanceChange } = {}) {
  // Some screens mount this without a router in their tests; the note is the same without the link.
  const routed = useInRouterContext();
  // The change as last saved here, so Edit note prefills from the note just written.
  const [current, setCurrent] = React.useState<GovernanceChange | undefined>(change);
  const [open, setOpen] = React.useState(false);
  // A different held change replaces the one this note was showing.
  React.useEffect(() => {
    setCurrent(change);
    setOpen(false);
  }, [change]);
  const noted = !!(current?.note?.reason || current?.note?.ticket);
  return (
    <>
      <Note tone="amber" role="status">
        <b className="font-semibold">{CHANGES.SUBMITTED_TITLE}</b>
        <span>{CHANGES.SUBMITTED_BODY}</span>
        <span className="flex flex-wrap items-center gap-x-4 gap-y-1">
          {routed && (
            <Link to={CHANGES_TAB_PATH} className="font-medium underline underline-offset-2">
              {CHANGES.SUBMITTED_LINK}
            </Link>
          )}
          {current && (
            <Button variant="outline" size="sm" onClick={() => setOpen(true)}>
              {noted ? CHANGE_NOTE.EDIT : CHANGE_NOTE.ADD}
            </Button>
          )}
        </span>
      </Note>
      {current && open && (
        <ChangeNoteForm
          change={current}
          onSaved={(c) => {
            setCurrent(c);
            setOpen(false);
          }}
          onCancel={() => setOpen(false)}
          onGone={() => {}}
        />
      )}
    </>
  );
}
