/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Each kind's icon, colour and words, as the owner-approved Files prototype
// draws them. A kind's colour is two CSS variables set once on its element.

import type * as React from "react";
import { cn } from "../../../../ui/utils";
import { NR_FILES } from "../../../../wardyn/copy/new-run/files";
import type { FileKind } from "./file-items";

const COLOUR: Record<FileKind, { kind: string; soft: string }> = {
  repo: { kind: "var(--info)", soft: "var(--info-subtle)" },
  userDrive: { kind: "var(--primary)", soft: "color-mix(in srgb, var(--primary) 13%, transparent)" },
  sharedDrive: { kind: "var(--violet)", soft: "var(--violet-subtle)" },
  folder: { kind: "var(--warning)", soft: "var(--warning-subtle)" },
  computer: { kind: "var(--muted-foreground)", soft: "var(--muted)" },
};

/** The two variables a kind's tile, badge and card border read. */
export const kindStyle = (kind: FileKind): React.CSSProperties =>
  ({ "--kind": COLOUR[kind].kind, "--kind-soft": COLOUR[kind].soft }) as React.CSSProperties;

export const KIND_LABEL: Record<Exclude<FileKind, "computer">, string> = {
  repo: NR_FILES.REPOSITORY_BADGE,
  userDrive: NR_FILES.KIND_USER_DRIVE,
  sharedDrive: NR_FILES.KIND_SHARED_DRIVE,
  folder: NR_FILES.KIND_FOLDER,
};

export const KIND_INFO: Record<FileKind, { title: string; body: string; sub?: string }> = {
  repo: { title: NR_FILES.REPOSITORY_BADGE, body: NR_FILES.REPOS_BODY },
  userDrive: { title: NR_FILES.KIND_USER_DRIVE, body: NR_FILES.DRIVES_BODY },
  sharedDrive: { title: NR_FILES.KIND_SHARED_DRIVE, body: NR_FILES.DRIVES_BODY, sub: NR_FILES.SHARED_SUB },
  folder: { title: NR_FILES.FOLDER_TITLE, body: NR_FILES.FOLDER_BODY },
  computer: { title: NR_FILES.COMPUTER_TITLE, body: NR_FILES.COMPUTER_BODY },
};

const ICON: Record<FileKind, React.ReactNode> = {
  repo: (
    <>
      <circle cx="6" cy="4.5" r="2" />
      <circle cx="6" cy="15.5" r="2" />
      <circle cx="14.5" cy="6.5" r="2" />
      <path d="M6 6.5v7M14.5 8.5c0 3.5-5 3-7.6 5.4" />
    </>
  ),
  userDrive: (
    <>
      <rect x="3" y="10.5" width="14" height="6.5" rx="1.8" />
      <path d="M6.5 13.75h.01" />
      <circle cx="10" cy="4.6" r="2.1" />
      <path d="M6.6 9c.6-1.4 1.8-2 3.4-2s2.8.6 3.4 2" />
    </>
  ),
  sharedDrive: (
    <>
      <rect x="3" y="10.5" width="14" height="6.5" rx="1.8" />
      <path d="M6.5 13.75h.01" />
      <circle cx="7" cy="4.8" r="1.8" />
      <circle cx="13" cy="4.8" r="1.8" />
      <path d="M4.2 8.8c.5-1 1.5-1.6 2.8-1.6s2.3.6 2.8 1.6M10.2 8.8c.5-1 1.5-1.6 2.8-1.6s2.3.6 2.8 1.6" />
    </>
  ),
  folder: <path d="M2.5 6a1.8 1.8 0 0 1 1.8-1.8h3.2l2 2h6.2a1.8 1.8 0 0 1 1.8 1.8v6.2a1.8 1.8 0 0 1-1.8 1.8H4.3a1.8 1.8 0 0 1-1.8-1.8Z" />,
  computer: (
    <>
      <rect x="2.5" y="3.5" width="15" height="10" rx="1.5" />
      <path d="M7 17h6M10 13.5V17" />
    </>
  ),
};

export function KindTile({ kind, className }: { kind: FileKind; className?: string }) {
  return (
    <span
      aria-hidden="true"
      style={kindStyle(kind)}
      className={cn(
        "grid size-[30px] flex-none place-items-center rounded-lg border border-[color-mix(in_srgb,var(--kind)_40%,var(--border))] bg-[var(--kind-soft)] text-[var(--kind)]",
        className,
      )}
    >
      <svg viewBox="0 0 20 20" className="size-[17px]" fill="none" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" strokeLinejoin="round">
        {ICON[kind]}
      </svg>
    </span>
  );
}

export function KindBadge({ kind }: { kind: Exclude<FileKind, "computer"> }) {
  return (
    <span
      style={kindStyle(kind)}
      className="inline-flex items-center whitespace-nowrap rounded-full border border-[color-mix(in_srgb,var(--kind)_45%,var(--border))] bg-[var(--kind-soft)] px-[7px] py-[3px] text-[10px] font-semibold leading-[1.3] text-[var(--kind)]"
    >
      {KIND_LABEL[kind]}
    </span>
  );
}

/** A path that wraps only after "/", never inside a name; the whole path is its tooltip. */
export function PathText({ path, className }: { path: string; className?: string }) {
  const parts = path.split("/");
  return (
    <code className={cn("font-mono text-[13px] [overflow-wrap:normal] [word-break:normal]", className)} title={path}>
      {parts.map((part, i) => (
        <span key={i}>
          {i > 0 && "/"}
          {part}
          {i > 0 && i < parts.length - 1 && <wbr />}
        </span>
      ))}
    </code>
  );
}

/** A section of the step: Make's bordered group with an uppercase heading row. */
export function FilesGroup({ titleId, title, body, aside, children, className }: {
  titleId: string;
  title: string;
  body: string;
  aside?: React.ReactNode;
  children?: React.ReactNode;
  className?: string;
}) {
  return (
    <section aria-labelledby={titleId} className={cn("rounded-[10px] border border-border bg-card", className)}>
      <div className="flex min-h-[65px] items-center justify-between gap-3 border-b border-border px-[15px] py-[13px] max-[520px]:items-start">
        <div>
          <h2 id={titleId} className="mb-1 text-sm font-normal uppercase tracking-[0.06em]">{title}</h2>
          <p className="text-sm leading-[1.4] text-muted-foreground">{body}</p>
        </div>
        {aside}
      </div>
      {children}
    </section>
  );
}
