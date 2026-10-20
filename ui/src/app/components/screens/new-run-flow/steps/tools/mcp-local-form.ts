/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The local tab of Add an MCP server as data: the command line as the server reads it
// (internal/types ComponentMCPLaunch: a command and arguments, no shell) and the pin it asks of an
// npx package. The server stays the authority; this lands the sentence on the field first.

/** One npm package at one exact version (the server's npmPinnedRE). */
const PINNED = /^(@[a-z0-9][a-z0-9._-]*\/)?[a-z0-9][a-z0-9._-]*@[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$/;

export function splitCommand(text: string): { command: string; args: string[] } {
  const [command = "", ...args] = text.trim().split(/\s+/).filter(Boolean);
  return { command, args };
}

/** The package npx runs: its first argument after -y. */
const packageOf = (args: readonly string[]): string | undefined => args.find((a) => a !== "-y" && a !== "--yes");

/** "empty", or "unpinned" for an npx package without one exact version; any other command is the server's to judge. */
export function commandProblem(text: string): "empty" | "unpinned" | undefined {
  const { command, args } = splitCommand(text);
  if (!command) return "empty";
  if (command.split("/").pop() !== "npx") return undefined;
  const pkg = packageOf(args);
  return pkg?.startsWith("-") || (pkg && PINNED.test(pkg)) ? undefined : "unpinned";
}

/** A name for a server the person left unnamed: its package, or its program. */
export function defaultName(text: string): string {
  const { command, args } = splitCommand(text);
  const pkg = command.split("/").pop() === "npx" ? packageOf(args) : undefined;
  return (pkg ?? command).replace(/@[^@/]*$/, "").split("/").pop() ?? "";
}
