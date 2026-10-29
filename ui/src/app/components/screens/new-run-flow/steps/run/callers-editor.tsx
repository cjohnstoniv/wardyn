/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { KeyRound, Plus, Search, User, Users, X, Check } from "lucide-react";
import { Button } from "../../../../ui/button";
import { Input } from "../../../../ui/input";
import { Popover, PopoverContent, PopoverTrigger } from "../../../../ui/popover";
import { NR_RUN } from "../../../../wardyn/copy/new-run/run";
import { useSecurityOperator } from "../../../../wardyn/operator-context";
import { directory, MIN_QUERY_LEN, type DirectoryEntry } from "../../../../../lib/api/directory";
import { getErrorMessage } from "../../../../../lib/format";
import type { UIApp } from "../../../../../lib/types/runs";
import { callerKind, callerKindLabel, callerValue, groupCaller, MAX_CALLERS, personCaller, serviceCaller, userCaller } from "./callers-model";
import { useCallerSources } from "./use-caller-sources";

export interface CallersEditorProps {
  /** The port's draft, with its `callers` (user:, group: or service: entries) as they stand. */
  app: UIApp;
  /** The port's row; its fields are `nr-app-<field>-<index>`. */
  index: number;
  /** The port's name for a screen reader, or its place when it has none yet. */
  who: string;
  /** Replaces the port's callers. An empty list leaves only the run's owner. */
  onChange: (callers: string[]) => void;
}

const MENU_HEADING = "px-3 pt-2.5 pb-1 text-xs font-medium text-muted-foreground";
const ITEM =
  "flex w-full items-center gap-2 rounded-md px-3 py-1.5 text-left text-sm hover:bg-accent focus-visible:bg-accent focus-visible:outline-none disabled:cursor-default disabled:hover:bg-transparent";
const MENU_NOTE = "px-3 pb-2 text-xs text-muted-foreground";

/** One pickable row: an icon, the value in the mono face, and "Added" once the caller is chosen. */
function Item({ icon, text, mono, aside, added, onPick }: { icon: React.ReactNode; text: string; mono?: boolean; aside?: string; added: boolean; onPick: () => void }) {
  return (
    <button type="button" className={ITEM} disabled={added} onClick={onPick}>
      <span className="text-muted-foreground" aria-hidden="true">{icon}</span>
      <span className={mono ? "min-w-0 flex-1 truncate font-mono text-[13px]" : "min-w-0 flex-1 truncate"}>{text}</span>
      {added ? (
        <span className="flex items-center gap-1 text-xs text-muted-foreground"><Check size={13} aria-hidden="true" />{NR_RUN.CALLER_ADDED}</span>
      ) : (
        aside && <span className="truncate text-xs text-muted-foreground">{aside}</span>
      )}
    </button>
  );
}

/** A security admin's search of the people and groups the directory knows; a deployment with no directory draws nothing. */
function DirectorySearch({ has, onPick }: { has: (caller: string) => boolean; onPick: (caller: string, name: string) => void }) {
  const [q, setQ] = React.useState("");
  const [found, setFound] = React.useState<DirectoryEntry[] | null | undefined>(undefined);
  const [failure, setFailure] = React.useState("");
  React.useEffect(() => {
    if (q.trim().length < MIN_QUERY_LEN) {
      setFound(undefined);
      setFailure("");
      return;
    }
    let live = true;
    const timer = setTimeout(() => {
      directory.search(q.trim()).then(
        (entries) => { if (live) { setFound(entries); setFailure(""); } },
        (e) => { if (live) setFailure(getErrorMessage(e)); },
      );
    }, 250);
    return () => { live = false; clearTimeout(timer); };
  }, [q]);
  if (found === null) return null;
  const rows = (found ?? []).flatMap((entry) => {
    const caller = entry.kind === "group" ? groupCaller(entry.claim_value) : entry.kind === "user" ? userCaller(entry.claim_value) : undefined;
    return caller ? [{ entry, caller }] : [];
  });
  return (
    <div className="border-b border-border pb-1">
      <p className={MENU_HEADING}>{NR_RUN.CALLERS_MENU_DIRECTORY}</p>
      <div className="relative px-3 pb-2">
        <Search size={14} className="pointer-events-none absolute top-2.5 left-5 text-muted-foreground" aria-hidden="true" />
        <Input aria-label={NR_RUN.CALLERS_MENU_DIRECTORY} className="pl-8" value={q} onChange={(e) => setQ(e.target.value)} />
      </div>
      {failure && <p role="alert" className="px-3 pb-2 text-xs text-danger">{failure}</p>}
      {rows.map(({ entry, caller }) => (
        <Item
          key={caller}
          icon={entry.kind === "group" ? <Users size={15} /> : <User size={15} />}
          text={entry.display_name}
          aside={entry.kind === "group" ? NR_RUN.CALLER_GROUP : entry.detail}
          added={has(caller)}
          onPick={() => onPick(caller, entry.display_name)}
        />
      ))}
    </div>
  );
}

/** A person by email: nothing is added until the address is whole, and the error says so. */
function PersonField({ full, onAdd }: { full: boolean; onAdd: (caller: string) => boolean }) {
  const [text, setText] = React.useState("");
  const [bad, setBad] = React.useState(false);
  const errorId = React.useId();
  const submit = () => {
    const caller = personCaller(text);
    if (!caller) return setBad(true);
    if (onAdd(caller)) setText("");
  };
  return (
    <div className="px-3 pb-3">
      <p className="pt-2.5 pb-1.5 text-xs font-medium text-muted-foreground">{NR_RUN.CALLERS_MENU_PERSON}</p>
      <div className="flex items-center gap-2">
        <Input
          type="email"
          aria-label={NR_RUN.CALLERS_MENU_PERSON}
          aria-invalid={bad || undefined}
          aria-describedby={bad ? errorId : undefined}
          placeholder={NR_RUN.CALLER_EMAIL_PLACEHOLDER}
          value={text}
          onChange={(e) => { setText(e.target.value); setBad(false); }}
          onKeyDown={(e) => { if (e.key === "Enter") { e.preventDefault(); submit(); } }}
        />
        <Button type="button" variant="outline" size="sm" disabled={full} onClick={submit}>{NR_RUN.CALLER_ADD_PERSON}</Button>
      </div>
      {bad && <p id={errorId} role="alert" className="pt-1.5 text-sm text-danger">{NR_RUN.CALLER_EMAIL_INVALID}</p>}
    </div>
  );
}

/**
 * "Who can call it" (r17): chips for the chosen callers and an "Add a caller" menu of what the console already knows,
 * so nobody types a prefix. The wire is unchanged: `callers` holds user:, group: and service: strings.
 */
export function CallersEditor({ app, index, onChange }: CallersEditorProps) {
  const callers = app.callers ?? [];
  const labelId = `nr-app-callers-${index}`;
  const [open, setOpen] = React.useState(false);
  // What a directory pick is called, for its chip: the wire holds the claim value, and the person chose a name.
  const [names, setNames] = React.useState<Record<string, string>>({});
  const sources = useCallerSources(open);
  const securityOperator = useSecurityOperator();
  const full = callers.length >= MAX_CALLERS;
  const has = (caller: string) => callers.includes(caller);
  // False for a caller already chosen or past the cap, so the person's text stays where it is.
  const add = (caller: string, name?: string): boolean => {
    if (has(caller) || full) return false;
    if (name) setNames((n) => ({ ...n, [caller]: name }));
    onChange([...callers, caller]);
    return true;
  };
  const groups = sources.groups && [...new Set(sources.groups.flatMap((g) => groupCaller(g) ?? []))];
  const services = sources.services?.flatMap((s) => serviceCaller(s) ?? []);
  return (
    <div role="group" aria-labelledby={labelId} className="flex flex-col gap-1.5">
      <p id={labelId} className="text-sm font-medium text-foreground">{NR_RUN.CALLERS_LABEL}</p>
      <p className="text-xs leading-snug text-muted-foreground">{callers.length ? NR_RUN.CALLERS_HELP : NR_RUN.CALLERS_NONE}</p>
      <div className="flex flex-wrap items-center gap-2">
        {callers.map((caller) => {
          const value = names[caller] ?? callerValue(caller);
          const mono = !names[caller];
          return (
            <span key={caller} data-caller={caller} className="inline-flex max-w-full items-center gap-1.5 rounded-md border border-border bg-muted/40 py-1 pr-1 pl-2 text-sm">
              <span className="text-xs text-muted-foreground">{callerKindLabel(callerKind(caller))}</span>
              <span className={mono ? "min-w-0 truncate font-mono text-[13px]" : "min-w-0 truncate"}>{value}</span>
              <button
                type="button"
                aria-label={NR_RUN.CALLER_REMOVE_ARIA(value)}
                className="rounded p-0.5 text-muted-foreground hover:text-foreground focus-visible:outline-2"
                onClick={() => onChange(callers.filter((c) => c !== caller))}
              >
                <X size={14} aria-hidden="true" />
              </button>
            </span>
          );
        })}
        <Popover open={open} onOpenChange={setOpen}>
          <PopoverTrigger asChild>
            <Button type="button" variant="outline" size="sm" disabled={full}>
              <Plus size={15} aria-hidden="true" />{NR_RUN.CALLERS_ADD}
            </Button>
          </PopoverTrigger>
          <PopoverContent align="start" className="w-80 p-0">
            {securityOperator && <DirectorySearch has={has} onPick={add} />}
            <div className="border-b border-border pb-1">
              <p className={MENU_HEADING}>{NR_RUN.CALLERS_MENU_GROUPS}</p>
              {groups && !groups.length && <p className={MENU_NOTE}>{NR_RUN.CALLERS_NO_GROUPS}</p>}
              {groups?.map((c) => <Item key={c} icon={<Users size={15} />} text={callerValue(c)} mono added={has(c)} onPick={() => add(c)} />)}
            </div>
            <div className="border-b border-border pb-1">
              <p className={MENU_HEADING}>{NR_RUN.CALLERS_MENU_SERVICES}</p>
              {services && !services.length && <p className={MENU_NOTE}>{NR_RUN.CALLERS_NO_SERVICES}</p>}
              {services?.map((c) => <Item key={c} icon={<KeyRound size={15} />} text={callerValue(c)} mono added={has(c)} onPick={() => add(c)} />)}
            </div>
            <PersonField full={full} onAdd={(c) => add(c)} />
          </PopoverContent>
        </Popover>
      </div>
    </div>
  );
}
