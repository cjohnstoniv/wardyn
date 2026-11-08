/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Platform envelopes on Governance > Profiles. An envelope names a platform's root profile and the IdP
// groups security approved for it; the server decides what that changes for a unit admin below it. A
// security operator sets, edits and removes envelopes here. Anyone else sees a read-only card, drawn only
// when an envelope covers a profile they can list. Every string comes from ENVELOPE.
import * as React from "react";
import { Layers } from "lucide-react";
import { HttpError, PendingChangeError } from "../../../lib/api/core";
import { governance as api, type GovernanceSnapshot, type GovernanceUnit } from "../../../lib/api/governance";
import type { GovernanceEnvelope } from "../../../lib/types/governance-envelope";
import { absoluteTime, getErrorMessage, relativeTime } from "../../../lib/format";
import { ENVELOPE } from "../../wardyn/copy/envelope";
import { POOL_DRAWER } from "../../wardyn/copy/runner-pools";
import { Mono } from "../../wardyn/code-block";
import { INLINE_ACTIONS } from "../../wardyn/form-actions";
import { usePrincipal } from "../../wardyn/operator-context";
import { Chip } from "../../wardyn/primitives";
import { EmptyState } from "../../wardyn/states";
import { Button } from "../../ui/button";
import { Input } from "../../ui/input";
import { Label } from "../../ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "../../ui/select";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "../../ui/table";
import { Textarea } from "../../ui/textarea";
import { ConfirmDialog } from "../runners/confirm-dialog";
import { Sec } from "../runners/resource-table";
import { Note } from "./display";
import { SubmittedNote } from "./submitted-note";

// The envelope root at or above a unit: the unit itself, else the nearest ancestor with an envelope. The walk
// goes through the listed profiles; a parent that is not listed is still checked as a root, so a unit admin
// whose list starts below the root finds it.
function rootOf(id: string | undefined, profiles: GovernanceUnit[], roots: Set<string>): string | null {
  const byId = new Map(profiles.map((p) => [p.id, p]));
  let cur = id;
  for (let i = 0; cur && i <= profiles.length + 1; i++) {
    if (roots.has(cur)) return cur;
    cur = byId.get(cur)?.parent_id;
  }
  return null;
}

const rootSet = (envelopes: GovernanceEnvelope[]) => new Set(envelopes.map((e) => e.unit_id));

/** The Profiles table's Name-cell chip: Envelope root, or In envelope with the root's name, or nothing. */
export function EnvelopeChip({
  profile,
  profiles,
  envelopes,
  unitNames,
}: {
  profile: GovernanceUnit;
  profiles: GovernanceUnit[];
  envelopes: GovernanceEnvelope[];
  unitNames?: Record<string, string>;
}) {
  const root = rootOf(profile.id, profiles, rootSet(envelopes));
  if (root === null) return null;
  const name = profiles.find((p) => p.id === root)?.name ?? unitNames?.[root] ?? root;
  return (
    <div className="mt-1">
      <Chip tone="neutral">{root === profile.id ? ENVELOPE.CHIP_ROOT : ENVELOPE.CHIP_IN(name)}</Chip>
    </div>
  );
}

type Form = { kind: "set" } | { kind: "edit"; unitId: string };

// One name per line, trimmed, blanks and repeats dropped.
const splitGroups = (text: string) => [...new Set(text.split("\n").map((l) => l.trim()).filter(Boolean))];

export function EnvelopeCard({
  snapshot,
  securityOperator,
  onChanged,
}: {
  snapshot: GovernanceSnapshot;
  securityOperator: boolean;
  onChanged: () => void;
}) {
  const principal = usePrincipal();
  const [form, setForm] = React.useState<Form | null>(null);
  const [draft, setDraft] = React.useState<string[]>([]);
  const [name, setName] = React.useState("");
  const [root, setRoot] = React.useState("");
  const [text, setText] = React.useState("");
  const [error, setError] = React.useState("");
  const [busy, setBusy] = React.useState(false);
  const [submitted, setSubmitted] = React.useState(false);
  const [removing, setRemoving] = React.useState<string | null>(null);

  const envelopes = snapshot.envelopes;
  if (envelopes === undefined) return null;

  const profiles = snapshot.profiles;
  const roots = rootSet(envelopes);
  const nameOf = (id: string) => profiles.find((p) => p.id === id)?.name ?? snapshot.unit_names?.[id] ?? id;

  const closeForm = () => {
    setForm(null);
    setError("");
    setName("");
  };
  const openEdit = (e: GovernanceEnvelope) => {
    setDraft(e.approved_groups);
    setName("");
    setError("");
    setSubmitted(false);
    setForm({ kind: "edit", unitId: e.unit_id });
  };
  const openSet = () => {
    setRoot("");
    setText("");
    setName("");
    setError("");
    setSubmitted(false);
    setForm({ kind: "set" });
  };
  const addToList = () => {
    const g = name.trim();
    if (g && !draft.includes(g)) setDraft([...draft, g]);
    setName("");
  };

  const save = async () => {
    if (!form) return;
    const unitId = form.kind === "set" ? root : form.unitId;
    const groups = form.kind === "set" ? splitGroups(text) : draft;
    setBusy(true);
    setError("");
    try {
      await api.setEnvelope(unitId, groups);
      setSubmitted(false);
      closeForm();
      onChanged();
    } catch (e) {
      if (e instanceof PendingChangeError) {
        setSubmitted(true);
        closeForm();
        onChanged();
      } else {
        // The refusal belongs to Save, not to the input: the field is left empty and plain.
        setName("");
        setError(e instanceof HttpError ? e.message : getErrorMessage(e));
      }
    } finally {
      setBusy(false);
    }
  };

  const clear = async () => {
    try {
      await api.clearEnvelope(removing ?? "");
      setSubmitted(false);
    } catch (e) {
      if (!(e instanceof PendingChangeError)) throw e;
      setSubmitted(true);
    }
  };

  if (!securityOperator) {
    return <ReadOnlyCard snapshot={snapshot} envelopes={envelopes} principal={principal} nameOf={nameOf} />;
  }

  // Free to be a new root: not a root, and not inside another envelope.
  const freeProfiles = profiles.filter((p) => rootOf(p.id, profiles, roots) === null);

  return (
    <section className="mt-6 overflow-hidden rounded-xl border border-border bg-card">
      <div className="px-6 pt-5">
        <div className="flex flex-wrap items-start justify-between gap-3">
          <div className="min-w-0">
            <h2 className="text-sm font-medium text-foreground">{ENVELOPE.TITLE}</h2>
            <p className="mt-1 max-w-[82ch] text-body text-muted-foreground">{ENVELOPE.LEAD}</p>
            <p className="mt-1 text-xs text-muted-foreground">{ENVELOPE.HELP}</p>
          </div>
          <Button variant="outline" size="sm" onClick={openSet}>
            {ENVELOPE.SET}
          </Button>
        </div>
        {submitted && <SubmittedNote />}
      </div>
      <div className="mt-4">
        {envelopes.length === 0 ? (
          <EmptyState
            size="compact"
            icon={Layers}
            title={ENVELOPE.EMPTY_TITLE}
            description={ENVELOPE.EMPTY_BODY}
            action={
              <Button variant="outline" onClick={openSet}>
                {ENVELOPE.SET}
              </Button>
            }
          />
        ) : (
          <Table>
            <TableHeader>
              <TableRow className="hover:bg-transparent">
                <TableHead>{ENVELOPE.COL_PLATFORM}</TableHead>
                <TableHead>{ENVELOPE.COL_GROUPS}</TableHead>
                <TableHead>{ENVELOPE.COL_SET_BY}</TableHead>
                <TableHead className="w-[200px]" />
              </TableRow>
            </TableHeader>
            <TableBody>
              {envelopes.map((e) => (
                <TableRow key={e.unit_id}>
                  <TableCell className="font-medium">{nameOf(e.unit_id)}</TableCell>
                  <TableCell>
                    <Groups groups={e.approved_groups} />
                  </TableCell>
                  <TableCell>
                    <SetBy who={e.set_by} at={e.set_at} />
                  </TableCell>
                  <TableCell>
                    <div className="flex justify-end gap-1">
                      <Button variant="outline" size="sm" aria-label={`${ENVELOPE.EDIT} ${nameOf(e.unit_id)}`} onClick={() => openEdit(e)}>
                        {ENVELOPE.EDIT}
                      </Button>
                      <Button variant="ghost" size="sm" aria-label={`${ENVELOPE.REMOVE} ${nameOf(e.unit_id)}`} onClick={() => setRemoving(e.unit_id)}>
                        {ENVELOPE.REMOVE}
                      </Button>
                    </div>
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        )}
      </div>

      {form && (
        <div className="space-y-4 border-t border-border px-6 py-5">
          <div>
            <h3 className="text-sm font-medium text-foreground">
              {form.kind === "set" ? ENVELOPE.SET : ENVELOPE.EDIT_TITLE(nameOf(form.unitId))}
            </h3>
            {form.kind === "edit" && <p className="mt-1 max-w-[82ch] text-xs text-muted-foreground">{ENVELOPE.EDIT_LEAD}</p>}
          </div>

          {form.kind === "set" ? (
            <>
              <div className="space-y-1.5">
                <Label htmlFor="envelope-root">{ENVELOPE.ROOT_LABEL}</Label>
                <Select value={root} onValueChange={setRoot}>
                  <SelectTrigger id="envelope-root" className="max-w-[28rem]">
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    {freeProfiles.map((p) => (
                      <SelectItem key={p.id} value={p.id}>
                        {p.name}
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
                <p className="text-xs text-muted-foreground">{ENVELOPE.ROOT_HINT}</p>
              </div>
              <div className="space-y-1.5">
                <Label htmlFor="envelope-groups">{ENVELOPE.GROUPS_LABEL}</Label>
                <Textarea
                  id="envelope-groups"
                  className="max-w-[28rem] font-mono"
                  value={text}
                  autoComplete="off"
                  onChange={(e) => setText(e.target.value)}
                />
                <p className="text-xs text-muted-foreground">{ENVELOPE.GROUPS_HINT}</p>
              </div>
            </>
          ) : (
            <>
              <ul className="space-y-1">
                {draft.map((g, i) => (
                  <li key={g} className="flex flex-wrap items-center gap-2 py-1">
                    <Chip>{POOL_DRAWER.SUBJECT.group}</Chip>
                    <Mono className="break-all">{g}</Mono>
                    <Button
                      variant="ghost"
                      size="sm"
                      aria-label={POOL_DRAWER.REMOVE_SUBJECT(g)}
                      onClick={() => setDraft(draft.filter((_, j) => j !== i))}
                    >
                      {POOL_DRAWER.REMOVE}
                    </Button>
                  </li>
                ))}
              </ul>
              <div className="grid grid-cols-[1fr_auto] items-end gap-3 max-sm:grid-cols-1">
                <div className="space-y-1.5">
                  <Label htmlFor="envelope-add">{ENVELOPE.ADD_LABEL}</Label>
                  <Input
                    id="envelope-add"
                    value={name}
                    placeholder={ENVELOPE.ADD_PLACEHOLDER}
                    autoComplete="off"
                    onChange={(e) => setName(e.target.value)}
                    onKeyDown={(e) => {
                      if (e.key === "Enter") {
                        e.preventDefault();
                        addToList();
                      }
                    }}
                  />
                </div>
                <Button variant="outline" onClick={addToList}>
                  {POOL_DRAWER.ADD_TO_LIST}
                </Button>
              </div>
            </>
          )}

          {error && (
            <Note tone="red" role="alert">
              <b className="font-semibold">{ENVELOPE.REFUSED_TITLE}</b>
              <Mono className="text-inherit">{error}</Mono>
            </Note>
          )}
          <div className={INLINE_ACTIONS}>
            <Button variant="ghost" onClick={closeForm}>
              {ENVELOPE.CANCEL}
            </Button>
            <Button disabled={busy || (form.kind === "set" && !root)} onClick={() => void save()}>
              {form.kind === "set" ? ENVELOPE.SAVE_ENVELOPE : ENVELOPE.SAVE_GROUPS}
            </Button>
          </div>
        </div>
      )}

      <ConfirmDialog
        open={removing !== null}
        onOpenChange={(o) => !o && setRemoving(null)}
        title={ENVELOPE.REMOVE_TITLE(nameOf(removing ?? ""))}
        body={[ENVELOPE.REMOVE_BODY(nameOf(removing ?? ""))]}
        confirmLabel={ENVELOPE.REMOVE_CONFIRM}
        onConfirm={clear}
        onDone={onChanged}
      />
    </section>
  );
}

function Groups({ groups }: { groups: string[] }) {
  if (groups.length === 0) return <span className="text-muted-foreground">{ENVELOPE.NO_GROUPS}</span>;
  return (
    <span className="flex flex-wrap items-center gap-1.5">
      {groups.map((g) => (
        <Chip key={g} mono>
          {g}
        </Chip>
      ))}
    </span>
  );
}

function SetBy({ who, at }: { who: string; at: string }) {
  return (
    <div className="leading-tight">
      <div>{who === "security" ? ENVELOPE.SET_BY_SECURITY : who}</div>
      <div className="text-xs text-muted-foreground" title={absoluteTime(at)}>
        {relativeTime(at)}
      </div>
    </div>
  );
}

function ReadOnlyCard({
  snapshot,
  envelopes,
  principal,
  nameOf,
}: {
  snapshot: GovernanceSnapshot;
  envelopes: GovernanceEnvelope[];
  principal: string;
  nameOf: (id: string) => string;
}) {
  // Only an envelope that is the nearest at or above a profile the viewer can list.
  const roots = rootSet(envelopes);
  const covering = new Set(snapshot.profiles.map((p) => rootOf(p.id, snapshot.profiles, roots)));
  const rows = envelopes.filter((e) => covering.has(e.unit_id));
  if (rows.length === 0) return null;
  const platform = nameOf(rows[0].unit_id);
  const administersRoot =
    principal !== "" && (snapshot.unit_admins ?? []).some((a) => a.unit_id === rows[0].unit_id && a.subject === principal);
  const list = (items: string[]) => (
    <ul className="list-disc space-y-1 pl-5">
      {items.map((s) => (
        <li key={s}>{s}</li>
      ))}
    </ul>
  );

  return (
    <section className="mt-6 overflow-hidden rounded-xl border border-border bg-card">
      <div className="px-6 pt-5">
        <h2 className="text-sm font-medium text-foreground">{ENVELOPE.TITLE_UNIT_ADMIN}</h2>
        <p className="mt-1 max-w-[82ch] text-body text-muted-foreground">{ENVELOPE.LEAD_UNIT_ADMIN}</p>
        <p className="mt-1 text-xs text-muted-foreground">{ENVELOPE.HELP}</p>
      </div>
      <div className="mt-4">
        <Table>
          <TableHeader>
            <TableRow className="hover:bg-transparent">
              <TableHead>{ENVELOPE.COL_PLATFORM}</TableHead>
              <TableHead>{ENVELOPE.COL_GROUPS}</TableHead>
              <TableHead>{ENVELOPE.COL_SET_BY}</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {rows.map((e) => (
              <TableRow key={e.unit_id}>
                <TableCell className="font-medium">{nameOf(e.unit_id)}</TableCell>
                <TableCell>
                  <Groups groups={e.approved_groups} />
                </TableCell>
                <TableCell>
                  <SetBy who={e.set_by} at={e.set_at} />
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      </div>
      <div className="px-6 pb-5">
        {administersRoot && (
          <Note>
            <b className="font-semibold">{ENVELOPE.ROOT_ADMIN_TITLE}</b>
            <span>{ENVELOPE.ROOT_ADMIN_BODY(platform)}</span>
          </Note>
        )}
        <div className="mt-3 grid grid-cols-2 gap-x-8 max-md:grid-cols-1">
          <Sec label={ENVELOPE.APPLIES_TITLE}>{list(ENVELOPE.APPLIES(platform))}</Sec>
          <Sec label={ENVELOPE.SECOND_TITLE}>{list(ENVELOPE.SECOND(platform))}</Sec>
        </div>
        <Sec label={ENVELOPE.SECURITY_TITLE}>{list([ENVELOPE.SECURITY_ONLY(platform)])}</Sec>
      </div>
    </section>
  );
}
