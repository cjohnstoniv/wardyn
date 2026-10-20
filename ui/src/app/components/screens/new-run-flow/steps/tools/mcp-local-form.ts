/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The local tab of Add an MCP server as data: the command line as the server reads it
// (internal/types ComponentMCPLaunch: a command and arguments, no shell) and the pin it asks of an
// npx package. The server stays the authority; this lands the sentence on the field first.

/** One npm package at one exact version (the server's npmPinnedRE). */
const PINNED = /^(@[a-z0-9][a-z0-9._-]*\/)?[a-z0-9][a-z0-9._-]*@[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$/;

const MAX_ARG_BYTES = 1024;

export function splitCommand(text: string): { command: string; args: string[] } {
  const [command = "", ...args] = text.trim().split(/\s+/).filter(Boolean);
  return { command, args };
}

/** The package npx runs: its first argument after -y. */
const packageOf = (args: readonly string[]): string | undefined => args.find((a) => a !== "-y" && a !== "--yes");

/** A launch program the server accepts: npx, or a clean absolute path (the server's validateLocal: path.IsAbs and path.Clean(c) == c). */
const isProgram = (c: string): boolean =>
  new TextEncoder().encode(c).length <= MAX_ARG_BYTES &&
  /^[\p{L}\p{M}\p{N}\p{P}\p{S} ]*$/u.test(c) &&
  (c === "npx" || c === "/" || (c.startsWith("/") && c.slice(1).split("/").every((s) => s !== "" && s !== "." && s !== "..")));

/**
 * What the server's validateLocal would refuse about the command, in the order it checks: "empty"; "program" (not npx
 * or a clean absolute path); for npx, "npx-flag" (anything but -y before the package) or "unpinned" (no package, or
 * not one exact version). The caps on the number and size of the arguments are the server's to judge.
 */
export function commandProblem(text: string): "empty" | "program" | "npx-flag" | "unpinned" | undefined {
  const { command, args } = splitCommand(text);
  if (!command) return "empty";
  if (!isProgram(command)) return "program";
  if (command.split("/").pop() !== "npx") return undefined;
  for (const a of args) {
    if (a === "-y" || a === "--yes") continue;
    if (a.startsWith("-")) return "npx-flag";
    return PINNED.test(a) ? undefined : "unpinned";
  }
  return "unpinned";
}

/** A name for a server the person left unnamed: its package, or its program. */
export function defaultName(text: string): string {
  const { command, args } = splitCommand(text);
  const pkg = command.split("/").pop() === "npx" ? packageOf(args) : undefined;
  return (pkg ?? command).replace(/@[^@/]*$/, "").split("/").pop() ?? "";
}
