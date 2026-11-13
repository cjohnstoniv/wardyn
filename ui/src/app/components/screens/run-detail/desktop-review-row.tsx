/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// One file in the Files panel's From tab under desktop.download: review. The row shows the state of the file's
// newest request (none, waiting, approved, denied, expired, or released once) and the one action that state allows.
// The server decides every state and every refusal; the row prints its sentences as they come.
import * as React from "react";
import { desktopFiles } from "../../../lib/api/desktop-files";
import { absoluteTime, shortTime } from "../../../lib/format";
import type { ApprovalRequest } from "../../../lib/types/approvals";
import { Button } from "../../ui/button";
import { Chip } from "../../wardyn/primitives";
import { DESKTOP_FILES as C } from "../../wardyn/copy";
import { DECIDERS_INBOX } from "../../wardyn/copy/deciders-inbox";
import { deciderPhrase, joinOr } from "../../wardyn/decider-lines";
import { transferSize } from "./desktop-files-panel";

// The clock the rest of the console uses (shortTime); a request from an earlier day also names the day, as the
// run page's identity card does, so "17:04" is never a time with no date.
const clockOf = (iso?: string) => {
  const at = iso ? new Date(iso) : new Date();
  return at.toDateString() === new Date().toDateString() ? shortTime(at.toISOString()) : absoluteTime(at.toISOString());
};
const message = (e: unknown) => (e instanceof Error ? e.message : String(e));
const shaOf = (r: ApprovalRequest) => String(r.requested_scope.sha256 ?? "");

export interface DesktopReviewRowProps {
  runId: string;
  file: { name: string; bytes: number };
  /** The newest file_download request for this name, if any. */
  latest?: ApprovalRequest;
  /** The PENDING request a successful ask returned. */
  onAsked: (request: ApprovalRequest) => void;
  /** The row's view of the requests may be stale: read them again. */
  onRefused: () => void;
}

export function DesktopReviewRow({ runId, file, latest, onAsked, onRefused }: DesktopReviewRowProps) {
  const [asking, setAsking] = React.useState(false);
  const [notAsked, setNotAsked] = React.useState<string | null>(null);
  const [failed, setFailed] = React.useState<string | null>(null);
  // An approved download in flight: a second click must not ask for the file again. A ref as well as the state,
  // so two clicks in one tick fetch once.
  const [fetching, setFetching] = React.useState(false);
  const fetchingRef = React.useRef(false);
  // The release this browser just made: the request reads "released" only after the next read of the requests.
  const [released, setReleased] = React.useState<{ id: string; at: string; sha: string } | null>(null);

  const state = latest?.state;
  const releasedHere = latest !== undefined && released?.id === latest.id;
  const spent = state === "APPROVED" && (Boolean(latest?.minted_jti) || releasedHere);

  async function ask() {
    setAsking(true);
    setNotAsked(null);
    setFailed(null);
    try {
      onAsked(await desktopFiles.review(runId, file.name));
    } catch (e) {
      setNotAsked(message(e));
      if (e instanceof Error && "reason" in e && e.reason === "desktop_review_pending") onRefused();
    } finally {
      setAsking(false);
    }
  }

  async function fetchApproved(request: ApprovalRequest) {
    if (fetchingRef.current) return;
    fetchingRef.current = true;
    setFetching(true);
    setFailed(null);
    try {
      const { blob, sha256 } = await desktopFiles.download(runId, file.name, request.id);
      const url = URL.createObjectURL(blob);
      const a = document.createElement("a");
      a.href = url;
      a.download = file.name;
      a.click();
      URL.revokeObjectURL(url);
      setReleased({ id: request.id, at: clockOf(), sha: sha256 });
      onRefused();
    } catch (e) {
      setFailed(message(e));
    } finally {
      fetchingRef.current = false;
      setFetching(false);
    }
  }

  const requestButton = (
    <Button size="sm" variant="outline" disabled={asking} onClick={() => void ask()}>
      {asking ? C.asking : C.requestDownload}
    </Button>
  );

  let chip: React.ReactNode = null;
  let action: React.ReactNode = requestButton;
  let line: React.ReactNode = null;
  if (latest && state === "PENDING") {
    chip = <Chip tone="warning" dot>{C.stateWaiting}</Chip>;
    action = null;
    const deciders = latest.deciders?.length ? joinOr(latest.deciders.map((e) => deciderPhrase(e, true))) : DECIDERS_INBOX.SECURITY;
    line = (
      <>
        <p className="font-mono text-meta text-muted-foreground">{C.askedLine(clockOf(latest.requested_at), shaOf(latest))}</p>
        <p className="text-meta text-muted-foreground">
          <span className="font-medium text-foreground">{DECIDERS_INBOX.DECIDED_BY_LABEL}</span> {deciders}
        </p>
      </>
    );
  } else if (latest && state === "APPROVED" && !spent) {
    chip = <Chip tone="success" dot>{C.stateApproved}</Chip>;
    action = (
      <Button size="sm" disabled={fetching} onClick={() => void fetchApproved(latest)}>
        {C.downloadApproved}
      </Button>
    );
    line = <p className="text-meta text-muted-foreground">{C.approvedLine(latest.decided_by ?? "", clockOf(latest.decided_at))}</p>;
  } else if (latest && spent) {
    line = (
      <p className="font-mono text-meta text-muted-foreground">
        {C.downloaded(releasedHere ? released.at : clockOf(latest.decided_at), releasedHere ? released.sha : shaOf(latest))}
      </p>
    );
  } else if (latest && state === "DENIED") {
    chip = <Chip tone="danger" dot>{C.stateDenied}</Chip>;
    line = <p className="text-meta text-muted-foreground">{C.deniedLine(latest.decided_by ?? "", latest.reason ?? "", clockOf(latest.decided_at))}</p>;
  } else if (latest && state === "EXPIRED") {
    chip = <Chip tone="neutral" dot>{C.stateExpired}</Chip>;
    line = <p className="text-meta text-muted-foreground">{C.expiredLine(clockOf(latest.decided_at ?? latest.requested_at))}</p>;
  }

  return (
    <li data-testid="desktop-download">
      <div className="flex flex-wrap items-center justify-between gap-x-3 gap-y-1.5 text-xs">
        <span className="min-w-0 flex-1 truncate text-foreground max-[399px]:basis-full">{file.name}</span>
        <span className="flex shrink-0 flex-wrap items-center gap-2 text-meta text-muted-foreground">
          {transferSize(file.bytes)}
          {chip}
          {action}
        </span>
      </div>
      {line}
      {notAsked !== null && (
        <p className="text-meta text-muted-foreground">
          {C.notAsked}: {notAsked}
        </p>
      )}
      {failed !== null && (
        <p className="text-meta text-muted-foreground">
          {C.downloadFailed}: {failed}
        </p>
      )}
    </li>
  );
}
