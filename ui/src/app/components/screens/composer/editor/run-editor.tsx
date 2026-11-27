/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The run editor: the person's own draft of a run, beside the assistant's chat. The assistant proposes parts and a
// part is in the draft only once it is accepted. Each accept, discard or edit posts the draft and what changed,
// then asks the server for its checks after a short pause. The draft goes on to New run's review, where the person
// launches as they do today; nothing is saved from here.
import * as React from "react";
import { useNavigate } from "react-router-dom";
import { assistant, type AssistantChecks, type AssistantDropped, type AssistantTarget } from "../../../../lib/api/assistant";
import { getErrorMessage } from "../../../../lib/format";
import { CHANGES } from "../../../../lib/governance-copy";
import { Button } from "../../../ui/button";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "../../../ui/table";
import { ASSISTANT as T } from "../../../wardyn/copy/assistant";
import { NR_SHARED } from "../../../wardyn/copy/new-run/shared";
import { RAIL_CHECK } from "../../../wardyn/copy/new-run-rail";
import { NEW_RUN_PATH } from "../../new-run-flow/draft/new-run-url";
import { ChecksStrip } from "./checks-strip";
import { flushEdits, postEdit } from "./edit-queue";
import { DroppedHosts, type DroppedItem } from "./dropped-hosts";
import { PartCard, sectionLabel, valueLines } from "./part-card";

/** A part as the server proposes it; `from_build_file` is the server's mark, never read from the assistant's text. */
export interface EditorPart {
  readonly section: string;
  readonly value: unknown;
  readonly from_build_file?: boolean;
}
export interface EditorProposal {
  readonly parts: readonly EditorPart[];
  readonly dropped: readonly DroppedItem[];
}

export interface PartState {
  readonly section: string;
  readonly value: unknown;
  /** The assistant's last proposal of this part, which an edit is compared with. */
  readonly proposed: unknown;
  readonly status: "proposed" | "accepted";
  readonly edited: boolean;
  readonly fromBuildFile: boolean;
  /** A newer proposal says something other than what the person accepted or edited; their value is kept. */
  readonly changedSince?: boolean;
}

const same = (a: unknown, b: unknown) => JSON.stringify(a ?? null) === JSON.stringify(b ?? null);

/** The draft: the accepted parts only, each with the person's own value. A proposed part is never in it. */
export function draftOf(parts: readonly PartState[]): Record<string, unknown> {
  return Object.fromEntries(parts.filter((p) => p.status === "accepted").map((p) => [p.section, p.value]));
}

/**
 * Lays a new proposal over what the person has. A part they accepted or edited stays theirs, value and mark: a proposal
 * that now says something else is flagged on the part (changedSince), never laid over their value. Any other proposed
 * part becomes the new proposal. Merging the same proposal again, as a reopened sheet does, changes nothing.
 */
export function mergeProposal(current: readonly PartState[], proposal: EditorProposal | null): PartState[] {
  if (!proposal) return [...current];
  const incoming = new Map(proposal.parts.map((p) => [p.section, p]));
  const fresh = (next: EditorPart): PartState => ({ section: next.section, value: next.value, proposed: next.value, status: "proposed", edited: false, fromBuildFile: next.from_build_file === true });
  const kept = current.filter((p) => p.status === "accepted" || incoming.has(p.section));
  const merged = kept.map((p): PartState => {
    const next = incoming.get(p.section);
    if (!next) return p;
    if (p.status !== "accepted") return fresh(next);
    const moved = !same(next.value, p.proposed) && !same(next.value, p.value);
    // The build-file mark describes the value it came with: it moves to the newer proposal only when the part holds that value.
    return { ...p, fromBuildFile: same(next.value, p.value) ? next.from_build_file === true : p.fromBuildFile, changedSince: moved || undefined };
  });
  for (const next of proposal.parts) {
    if (!merged.some((p) => p.section === next.section)) merged.push(fresh(next));
  }
  return merged;
}

// What the person has accepted or edited, kept while the session lives, so closing and opening the draft sheet on a
// phone does not lose it.
const kept = new Map<string, PartState[]>();

/** A new session takes over from another (Try again): what the person accepted and edited goes with it. */
export function carryKept(from: string, to: string): void {
  const parts = kept.get(from);
  if (!parts) return;
  kept.set(to, parts);
  // The new session has never seen the draft: it is told once, through the session's own queue, so its next turn does.
  const draft = draftOf(parts);
  if (Object.keys(draft).length > 0) postEdit(to, draft, new Map(Object.entries(draft).map(([k, v]) => [k, JSON.stringify(v)])), () => undefined);
}

export { flushEdits };

/** The left-out entries of the proposal, each with the count the server's checks gave for that unit when they did. */
export function mergeDropped(proposed: readonly DroppedItem[], fromChecks: readonly AssistantDropped[] | null | undefined): DroppedItem[] {
  const checked = fromChecks ?? [];
  const key = (d: { host?: string; field?: string; unit?: string }) => `${d.host ?? ""}|${d.field ?? ""}|${d.unit ?? ""}`;
  const seen = new Set<string>();
  const out: DroppedItem[] = [];
  for (const d of proposed) {
    const match = checked.find((c) => key(c) === key(d));
    seen.add(key(d));
    out.push(d.admin_change_approvals === undefined && match ? { ...d, admin_change_approvals: match.admin_change_approvals } : d);
  }
  for (const c of checked) if (!seen.has(key(c))) out.push(c);
  return out;
}

export interface RunEditorProps {
  proposal: EditorProposal | null;
  /** The assistant session the draft is for; edits are posted to it and the launch names it. */
  sessionId?: string;
  target?: AssistantTarget;
  /** Milliseconds between the last change and the server's checks. */
  debounceMs?: number;
}

export function RunEditor({ proposal, sessionId, target = { kind: "run" }, debounceMs = 400 }: RunEditorProps) {
  const navigate = useNavigate();
  const [parts, setParts] = React.useState<PartState[]>(() => mergeProposal((sessionId && kept.get(sessionId)) || [], proposal));
  const [checks, setChecks] = React.useState<AssistantChecks | null>(null);
  const [busy, setBusy] = React.useState(false);
  // The checks could not be read, and the session could not take an edit: each stays until its own next success.
  const [failed, setFailed] = React.useState<string | null>(null);
  const [editFailed, setEditFailed] = React.useState<string | null>(null);
  // Bumped by a change the person made or a new proposal: the checks run after it. `answered` is the change the last
  // answer was for, and `editedAt` the change each edit was, so "Checked again" is only said once its check answered.
  const [changes, setChanges] = React.useState(0);
  const [answered, setAnswered] = React.useState(0);
  const [editedAt, setEditedAt] = React.useState<Record<string, number>>({});
  const changed = React.useRef(0);
  const latest = React.useRef(parts);
  latest.current = parts;
  const proposed = React.useRef("");

  React.useEffect(() => {
    setParts((cur) => mergeProposal(cur, proposal));
    // A proposal that says something new asks again, so the strip is never the answer for an older draft.
    const said = JSON.stringify(proposal);
    if (proposal && said !== proposed.current) {
      setChecks(null);
      setChanges((changed.current += 1));
    }
    proposed.current = said;
  }, [proposal]);
  React.useEffect(() => {
    if (sessionId) kept.set(sessionId, parts);
  }, [sessionId, parts]);

  const act = (section: string, change: (p: PartState) => PartState | null, edit = false) => {
    const next = latest.current.flatMap((p) => {
      if (p.section !== section) return [p];
      const changed = change(p);
      return changed ? [changed] : [];
    });
    latest.current = next;
    setParts(next);
    const stamp = (changed.current += 1);
    setChanges(stamp);
    if (edit) setEditedAt((m) => ({ ...m, [section]: stamp }));
    setChecks(null);
    if (sessionId) {
      const after = next.find((p) => p.section === section);
      postEdit(sessionId, draftOf(next), new Map([[section, JSON.stringify(after ? after.value : null)]]), setEditFailed);
    }
  };

  const draft = draftOf(parts);
  const accepted = Object.keys(draft).length;
  React.useEffect(() => {
    if (changes === 0) return;
    if (accepted === 0) {
      setChecks(null);
      setBusy(false);
      return;
    }
    const controller = new AbortController();
    const stamp = changes;
    setBusy(true);
    const timer = setTimeout(() => {
      assistant
        .postChecks({ target_kind: target.kind, ...(target.id ? { target: target.id } : {}), draft: draftOf(latest.current) }, controller.signal)
        .then((c) => {
          if (controller.signal.aborted) return;
          setChecks(c);
          setFailed(null);
          setAnswered(stamp);
          setBusy(false);
        })
        .catch((e: unknown) => {
          if (controller.signal.aborted) return;
          setFailed(getErrorMessage(e));
          setBusy(false);
        });
    }, debounceMs);
    return () => {
      clearTimeout(timer);
      controller.abort();
    };
    // Only a change the person made asks again; the target is fixed for the screen.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [changes]);

  const edited = parts.filter((p) => p.status === "accepted" && p.edited && !same(p.value, p.proposed));
  const checkedAgain = edited.filter((p) => (editedAt[p.section] ?? Infinity) <= answered);
  const left = mergeDropped(proposal?.dropped ?? [], checks?.dropped);

  // New run gets only what the person saw checked: the checks of this very draft answered valid and left nothing out.
  // The answer carries no clamped policy to hand on instead, so a draft with a left-out host waits for the person.
  // A host the ceiling refused is the person's to take out; any other entry (a field the server set itself) is shown and
  // changes nothing they can do.
  const checkedLeftOut = (checks?.dropped ?? []).some((d) => !!d.host);
  const reviewable = checks?.valid === true && !checkedLeftOut;

  const checkAgain = () => {
    setChecks(null);
    setFailed(null);
    setChanges((changed.current += 1));
  };

  const review = () =>
    navigate(NEW_RUN_PATH, {
      state: { prefill: { source: "assistant", document: { inline_policy: draft }, ...(sessionId ? { assistantSession: sessionId } : {}) } },
    });

  return (
    <div data-testid="composer-run-editor" className="min-w-0 space-y-3">
      {parts.map((p) => (
        <PartCard
          key={p.section}
          section={p.section}
          value={p.value}
          status={p.status}
          edited={p.edited}
          fromBuildFile={p.fromBuildFile}
          changedSince={p.changedSince === true}
          onAccept={() => act(p.section, (c) => ({ ...c, status: "accepted" }))}
          onDiscard={() => act(p.section, () => null)}
          onEdit={(value) => act(p.section, (c) => ({ ...c, value, status: "accepted", edited: !same(value, c.proposed) }), true)}
        />
      ))}
      <DroppedHosts items={left} />
      {edited.length > 0 && (
        <div data-testid="composer-edit-diff" className="space-y-1.5">
          <h3 className="text-body font-medium text-foreground">{T.CHANGED_SINCE}</h3>
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>{CHANGES.DIFF_FIELD}</TableHead>
                <TableHead>{CHANGES.DIFF_BEFORE}</TableHead>
                <TableHead>{CHANGES.DIFF_AFTER}</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {edited.map((p) => (
                <TableRow key={p.section}>
                  <TableCell>{sectionLabel(p.section)}</TableCell>
                  <TableCell className="mono whitespace-normal break-words">{valueLines(p.proposed).join(", ")}</TableCell>
                  <TableCell className="mono whitespace-normal break-words">{valueLines(p.value).join(", ")}</TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
          {checkedAgain.map((p) => (
            <p key={p.section} role="status" className="text-meta text-muted-foreground">
              {T.YOU_CHANGED(sectionLabel(p.section))}
            </p>
          ))}
        </div>
      )}
      {(accepted > 0 || checks || failed || editFailed) && <ChecksStrip checks={checks} subject="" kind={target.kind} busy={busy} failed={[editFailed, failed]} leftOut={left.length > 0} />}
      {accepted > 0 && failed && (
        <Button type="button" variant="outline" size="sm" onClick={checkAgain}>
          {RAIL_CHECK.BUTTON}
        </Button>
      )}
      {accepted > 0 && (
        <>
          <Button type="button" disabled={!reviewable} onClick={review}>
            {NR_SHARED.REVIEW_RUN}
          </Button>
          {!reviewable && (
            <p className="text-meta text-muted-foreground">{checkedLeftOut ? T.REVIEW_HELD_LEFT_OUT : T.REVIEW_HELD_CHECKING}</p>
          )}
        </>
      )}
    </div>
  );
}
