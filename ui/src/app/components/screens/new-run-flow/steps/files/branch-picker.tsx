/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The branch for one included repository. The list is read lazily, only for a
// repository that is included. A list that cannot be read, or a person who
// chooses "Other…" from a truncated one, gets a text field: the ref typed
// there is not checked until the run starts.

import * as React from "react";
import { NR_FILES } from "../../../../wardyn/copy/new-run/files";
import { Input } from "../../../../ui/input";
import { repoBranches, type RepoBranches } from "../../../../../lib/api/repo-branches";

const OTHER = "\u0000other";
const SELECT_CLASS =
  "h-[42px] w-full max-w-[220px] min-w-40 rounded-md border border-border-strong bg-input-background px-3 font-mono text-sm outline-none focus-visible:border-ring focus-visible:ring-[3px] focus-visible:ring-ring disabled:cursor-not-allowed disabled:opacity-50";

type Listing = { state: "idle" | "loading" | "failed" } | { state: "ready"; list: RepoBranches };

/** One lazy read per included repository; any failure is the same as an unavailable listing. */
function useBranches(workspaceId: string, repo: string, enabled: boolean): Listing {
  const [listing, setListing] = React.useState<Listing>({ state: "idle" });
  React.useEffect(() => {
    if (!enabled) return;
    let current = true;
    setListing({ state: "loading" });
    repoBranches.list(workspaceId, repo).then(
      (list) => current && setListing(list.listing === "listed" ? { state: "ready", list } : { state: "failed" }),
      () => current && setListing({ state: "failed" }),
    );
    return () => { current = false; };
  }, [workspaceId, repo, enabled]);
  return listing;
}

export function BranchPicker({ workspaceId, repo, name, included, sourceRef, value, typed, onPick, onType }: {
  workspaceId: string;
  repo: string;
  name: string;
  included: boolean;
  /** The source's own ref. */
  sourceRef?: string;
  /** The ref the draft carries; absent follows the source's ref, then the remote's default branch. */
  value?: string;
  /** The text in the field while it is shown. */
  typed: string;
  onPick: (ref: string | undefined) => void;
  onType: (text: string) => void;
}) {
  const listing = useBranches(workspaceId, repo, included);
  const [other, setOther] = React.useState(false);
  const hintId = React.useId();
  const aria = NR_FILES.BRANCH_ARIA(name);

  if (!included || listing.state === "idle" || listing.state === "loading") {
    return (
      <select className={SELECT_CLASS} aria-label={aria} aria-busy={included} disabled value={sourceRef ?? ""}>
        <option value={sourceRef ?? ""}>{sourceRef ?? NR_FILES.BRANCH_PLACEHOLDER}</option>
      </select>
    );
  }
  if (listing.state === "ready" && !other) {
    const { branches, default: remoteDefault, truncated } = listing.list;
    const baseline = sourceRef ?? remoteDefault;
    const shown = value ?? baseline ?? branches[0] ?? "";
    const options = shown && !branches.includes(shown) ? [shown, ...branches] : branches;
    return (
      <select
        className={SELECT_CLASS}
        aria-label={aria}
        value={shown}
        onChange={(e) => {
          if (e.target.value === OTHER) return setOther(true);
          onPick(e.target.value === baseline ? undefined : e.target.value);
        }}
      >
        {options.map((b) => <option key={b} value={b}>{b}</option>)}
        {truncated && <option value={OTHER}>{NR_FILES.BRANCH_OTHER}</option>}
      </select>
    );
  }
  return (
    <div className="flex min-w-60 flex-col gap-1">
      <Input
        className="font-mono"
        aria-label={aria}
        aria-describedby={hintId}
        placeholder={NR_FILES.BRANCH_PLACEHOLDER}
        value={typed}
        onChange={(e) => onType(e.target.value)}
      />
      <p id={hintId} className="text-xs text-muted-foreground">
        {!other && <span>{NR_FILES.BRANCH_FALLBACK} </span>}
        {typed.trim() !== "" && <span>{NR_FILES.REF_NOT_CHECKED}</span>}
      </p>
    </div>
  );
}
