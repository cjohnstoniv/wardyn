/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { cn } from "../ui/utils";
import { CopyButton } from "./copy-button";

/** A mono code block with a copy button. `children` is the tinted rendering of
 *  `text`; without it the raw text is shown (arbitrary emitted-file contents). */
export function CodeBlock({
  text,
  className,
  children,
  copyLabel,
}: {
  text: string;
  className?: string;
  children?: React.ReactNode;
  /** Accessible name of the Copy button; says WHAT is copied when a card holds several blocks. */
  copyLabel?: string;
}) {
  return (
    <div className={cn("group relative rounded-lg border border-border bg-surface-2/60", className)}>
      <CopyButton
        text={text}
        label={copyLabel}
        className="absolute right-2 top-2 z-10 size-7 justify-center rounded-md border border-border bg-card text-muted-foreground opacity-0 transition focus-visible:opacity-100 group-hover:opacity-100 hover:text-foreground"
      />
      <pre className="scroll-thin overflow-x-auto p-3 text-xs leading-relaxed">
        <code className="font-mono">{children ?? text}</code>
      </pre>
    </div>
  );
}

/** Inline monospace identity / id token. */
export function Mono({ children, className, title }: { children: React.ReactNode; className?: string; title?: string }) {
  return (
    <span title={title} className={cn("font-mono text-xs leading-[inherit] text-muted-foreground", className)}>
      {children}
    </span>
  );
}

/**
 * makeMono builds the backtick-mono splitter four surfaces need: a frozen copy
 * string carries its literals as PLAIN TEXT (each copy module's own header
 * rule), and the CONSUMING component decides which substrings render mono.
 * Give it that surface's term list; it returns the renderer.
 *
 * Terms are regex-escaped, so a term carrying `.`, `-` or `/` matches itself
 * and nothing else — the drives list (`/home/agent/drive`, `disk_mib`, `. _ -`)
 * is the one that needs it, and escaping is a no-op for the env-var lists.
 * Longest-first so a term that is a prefix of another cannot win the split.
 */
export function makeMono(terms: string[]): (text: string) => React.ReactNode {
  const ordered = [...terms].sort((a, b) => b.length - a.length);
  const escape = (s: string) => s.replace(/[.*+?^${}()|[\]\\-]/g, "\\$&");
  const re = new RegExp(`(${ordered.map(escape).join("|")})`, "g");
  return function withMono(text: string): React.ReactNode {
    return text.split(re).map((part, i) =>
      terms.includes(part) ? (
        <Mono key={i} className="text-inherit">
          {part}
        </Mono>
      ) : (
        <React.Fragment key={i}>{part}</React.Fragment>
      ),
    );
  };
}
