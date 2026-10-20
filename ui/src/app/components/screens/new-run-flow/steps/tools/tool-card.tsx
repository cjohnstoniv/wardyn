/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The one tool card: icon, name, where it comes from, what it is, what it brings
// and its action. "What it brings" is the same four rows on every card (Network,
// Credentials, Files and drives, Other), one line each, with Details opening the
// whole of it. Every catalogue card and every added tool is this component; a
// caller supplies a ToolAccess, never markup of its own.

import * as React from "react";
import { Button } from "../../../../ui/button";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "../../../../ui/dialog";
import { cn } from "../../../../ui/utils";
import { NR_REVIEW } from "../../../../wardyn/copy/new-run/review";
import { NR_SHARED } from "../../../../wardyn/copy/new-run/shared";
import { NR_TOOLS } from "../../../../wardyn/copy/new-run/tools";
import { RUN_COCKPIT } from "../../../../wardyn/copy/run-cockpit";
import { Chip } from "../../../../wardyn/primitives";
import { IconTile } from "./icon-tile";

/** A host a tool brings, and the chip the dialog gives it (Allowed unless the server has judged this run's host). */
export interface ToolHost {
  host: string;
  chip?: { text: string; tone: "neutral" | "success" | "info" | "danger" };
}

/** A credential a tool brings: its name when the console is allowed to know it, and how it reaches the run. */
export interface ToolCredential {
  name?: string;
  delivery?: string;
}

/** What a tool brings, as the card and its Details dialog show it. An empty list or "" reads None. */
export interface ToolAccess {
  network: readonly ToolHost[];
  credentials: readonly ToolCredential[];
  /** Said once under the credentials, in the dialog. */
  credentialNote?: string;
  files: readonly string[];
  /** The card's one line. */
  other: string;
  /** The dialog's Other; the card's line when absent. */
  otherDetail?: React.ReactNode;
}

/** A tool that brings nothing: every row reads None and there is nothing more to open. */
export const NOTHING: ToolAccess = { network: [], credentials: [], files: [], other: "" };

const isNothing = (a: ToolAccess) => !a.network.length && !a.credentials.length && !a.files.length && !a.other;

/** A long Network row shows this many hosts, then "+{n} more". */
const HOSTS_SHOWN = 2;

const NONE = <span className="text-muted-foreground">{NR_TOOLS.DETAIL_NONE}</span>;

const SEP = NR_SHARED.JOIN("", "");

// A short fixed part ("Read only", "No exec") stays whole; a list or a long name may wrap, or a
// phone-width card would overflow.
const KEEP_WHOLE = 24;

/** The Other line, each short "·"-separated part kept whole so the line only wraps between parts. */
function Segments({ text }: { text: string }) {
  const parts = text.split(SEP);
  if (parts.length === 1) return <>{text}</>;
  const whole = (p: string) => p.length <= KEEP_WHOLE && !p.includes(", ");
  return <span>{parts.map((p, i) => <React.Fragment key={i}>{i > 0 && SEP}<span className={whole(p) ? "whitespace-nowrap" : "break-words"}>{p}</span></React.Fragment>)}</span>;
}

function Row({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div className="grid grid-cols-[108px_minmax(0,1fr)] items-start gap-2 border-b border-border px-2.5 py-[7px] last:border-b-0">
      <dt className="text-muted-foreground">{label}</dt>
      <dd className="flex min-w-0 flex-wrap items-center gap-1.5 [overflow-wrap:anywhere]">{children}</dd>
    </div>
  );
}

/** One entry per name; credentials with no name show each delivery once. */
function credentialItems(credentials: readonly ToolCredential[]): ToolCredential[] {
  const seen = new Set<string>();
  return credentials.filter((c) => {
    const key = `${c.name ?? ""}\u0000${c.delivery ?? ""}`;
    if (seen.has(key)) return false;
    seen.add(key);
    return true;
  });
}

/** The four rows, one line each. */
export function AccessBreakdown({ name, access }: { name: string; access: ToolAccess }) {
  const hosts = access.network.slice(0, HOSTS_SHOWN);
  const more = access.network.length - hosts.length;
  const credentials = credentialItems(access.credentials);
  return (
    <dl aria-label={NR_TOOLS.BRINGS_FOR(name)} className="flex flex-col rounded-lg border border-border bg-background text-body">
      <Row label={NR_TOOLS.NETWORK_TITLE}>
        {hosts.length ? hosts.map((h) => <span key={h.host} className="font-mono">{h.host}</span>) : NONE}
        {more > 0 && <span className="text-muted-foreground">{NR_TOOLS.MORE(more)}</span>}
      </Row>
      <Row label={NR_TOOLS.CREDENTIALS_TITLE}>
        {credentials.length ? credentials.map((c, i) => (
          <React.Fragment key={i}>
            {c.name && <span>{c.name}</span>}
            {c.delivery && <Chip className="whitespace-normal">{c.delivery}</Chip>}
          </React.Fragment>
        )) : NONE}
      </Row>
      <Row label={NR_TOOLS.DETAIL_FILES}>{access.files.length ? access.files.join(", ") : NONE}</Row>
      <Row label={NR_TOOLS.DETAIL_OTHER}>{access.other ? <Segments text={access.other} /> : NONE}</Row>
    </dl>
  );
}

function Section({ title, children }: { title: string; children: React.ReactNode }) {
  const id = React.useId();
  return (
    <section aria-labelledby={id} className="flex flex-col gap-2 border-t border-border pt-3.5">
      <h3 id={id} className="text-sm font-semibold">{title}</h3>
      {children}
    </section>
  );
}

/** The whole breakdown, with the same four sections in the same order. */
function DetailsDialog({ icon, title, org, body, access, onClose }: { icon: string; title: string; org?: boolean; body?: string; access: ToolAccess; onClose: () => void }) {
  return (
    <Dialog open onOpenChange={(open) => !open && onClose()}>
      <DialogContent {...(body ? {} : { "aria-describedby": undefined })}>
        <DialogHeader>
          <div className="flex items-center gap-2.5">
            <IconTile mark={icon} />
            <div className="min-w-0 text-left">
              <DialogTitle>{title}</DialogTitle>
              {org && <p className="text-xs text-muted-foreground">{NR_TOOLS.FROM_ORG}</p>}
            </div>
          </div>
          {body && <DialogDescription>{body}</DialogDescription>}
        </DialogHeader>
        <Section title={NR_TOOLS.NETWORK_TITLE}>
          {access.network.length ? (
            <ul className="flex flex-col gap-2">
              {access.network.map((h) => (
                <li key={h.host} className="flex flex-wrap items-center justify-between gap-2 text-sm">
                  <span className="font-mono [overflow-wrap:anywhere]">{h.host}</span>
                  <Chip tone={h.chip?.tone ?? "success"} className="whitespace-normal">{h.chip?.text ?? NR_TOOLS.NET_ALLOWED}</Chip>
                </li>
              ))}
            </ul>
          ) : <p className="text-sm text-muted-foreground">{NR_TOOLS.DETAIL_NONE}</p>}
        </Section>
        <Section title={NR_TOOLS.CREDENTIALS_TITLE}>
          {access.credentials.length ? (
            <ul className="flex flex-col gap-2">
              {credentialItems(access.credentials).map((c, i) => (
                <li key={i} className="flex flex-wrap items-center justify-between gap-2 text-sm">
                  <span>{c.name}</span>
                  {c.delivery && <Chip className="whitespace-normal">{c.delivery}</Chip>}
                </li>
              ))}
            </ul>
          ) : <p className="text-sm text-muted-foreground">{NR_TOOLS.DETAIL_NONE}</p>}
          {access.credentialNote && <p className="text-sm text-muted-foreground">{access.credentialNote}</p>}
        </Section>
        <Section title={NR_TOOLS.DETAIL_FILES}>
          {access.files.length ? (
            <ul className="flex flex-col gap-1 text-sm">{access.files.map((f) => <li key={f}>{f}</li>)}</ul>
          ) : <p className="text-sm text-muted-foreground">{NR_TOOLS.DETAIL_NONE}</p>}
        </Section>
        <Section title={NR_TOOLS.DETAIL_OTHER}>
          {access.otherDetail ?? (access.other ? <p className="text-sm"><Segments text={access.other} /></p> : <p className="text-sm text-muted-foreground">{NR_TOOLS.DETAIL_NONE}</p>)}
        </Section>
        <DialogFooter>
          <Button type="button" variant="ghost" onClick={onClose}>{NR_REVIEW.CLOSE}</Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

export type CardLook = "plain" | "added" | "off";

export interface ToolCardProps {
  icon: string;
  title: string;
  /** "From your organisation" under the name. */
  org?: boolean;
  /** The line under the name for a tool that is not the organisation's, such as "This run only". */
  sub?: string;
  /** The card's one line, repeated as the dialog's description. */
  body?: string;
  /** What the tool brings. Absent on a card that cannot be added (no breakdown, no Details). */
  access?: ToolAccess;
  /** Chips for a card that is not on the breakdown yet. */
  brings?: React.ReactNode;
  look?: CardLook;
  /** The action row. It is given the Details button, or undefined when the card has none to offer. */
  children: React.ReactNode | ((details: React.ReactNode) => React.ReactNode);
}

/** A catalogue card: icon, name, where it comes from, what it is, what it brings, then its action. */
export function ToolCard({ icon, title, org, sub, body, access, brings, look = "plain", children }: ToolCardProps) {
  const [open, setOpen] = React.useState(false);
  const details = access && !isNothing(access) ? (
    <>
      <Button type="button" variant="ghost" size="sm" aria-label={NR_TOOLS.DETAILS_FOR(title)} aria-haspopup="dialog" onClick={() => setOpen(true)}>{RUN_COCKPIT.credentialDetails}</Button>
      {open && <DetailsDialog icon={icon} title={title} org={org} body={body} access={access} onClose={() => setOpen(false)} />}
    </>
  ) : undefined;
  return (
    <li className={cn("flex flex-col gap-2 rounded-lg border p-3", look === "added" ? "border-primary bg-primary/10" : "border-border bg-surface-2", look === "off" && "bg-muted/50 text-muted-foreground")}>
      <div className="flex items-center gap-2.5">
        <IconTile mark={icon} />
        <div className="min-w-0">
          <h3 className="font-medium">{title}</h3>
          {(org || sub) && <p className="text-xs text-muted-foreground">{org ? NR_TOOLS.FROM_ORG : sub}</p>}
        </div>
      </div>
      {body && <p className="text-sm text-muted-foreground">{body}</p>}
      {access ? <AccessBreakdown name={title} access={access} /> : brings}
      <div className="mt-0.5 flex flex-wrap items-center justify-between gap-2">{typeof children === "function" ? children(details) : children}</div>
    </li>
  );
}
