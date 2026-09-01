/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// DirectoryCombobox (0.7 §I) — the typeahead every "who" field wears: the
// governance assignment's subject and the People step's mapping Value. It
// exists because an Entra `groups` claim carries an object GUID and an App Role
// arrives as its manifest value, so a hand-typed "Platform Engineering" binds
// NOTHING and fails open to whatever the unassigned default is. Picking a row
// inserts Entry.ClaimValue while the row shows Entry.DisplayName — that split
// is the whole point of the two fields.
//
// ABSENT MODE IS THE SPINE. With no directory configured this control IS the
// plain text input it replaced: no banner, no disabled state, no "directory
// unavailable" note — §7.9 deliberately freezes no string for that state. A
// "who" field stays free text forever, because the deployment that never turns
// this on is the common one. Which is why the input carries no combobox chrome
// at ANY time: the suggestions are an enhancement rendered beneath an ordinary
// text box, so absent mode is literally "the list never appears" rather than a
// second rendering path that has to be kept honest.
//
// Absent and broken are different answers and the endpoint says which: a
// distinct 503 code means unconfigured (lib/api/directory.ts turns it into a
// null, and latches — silent, for the rest of the session), anything else means
// the tenant is failing and the admin is told (LOOKUP_FAILED) while the text
// they typed stays exactly where it is.
//
// Every string comes from governance-copy.ts's DIRECTORY (§7.9); this file adds
// none — including MIN_CHARS_HINT, which is frozen but deliberately NOT
// rendered (§O Q7: "searching…" only, because the threshold is crossed before
// an admin finishes the first word and a second message is clutter).
import * as React from "react";
import {
  directory,
  MIN_QUERY_LEN,
  type DirectoryEntry,
  type DirectorySearchKind,
} from "../../lib/api/directory";
import { DIRECTORY } from "../../lib/governance-copy";
import { Input } from "../ui/input";
import { Mono } from "./code-block";
import { Chip } from "./primitives";

// Long enough that a fast typist sends one request per word rather than per
// keystroke, short enough that the answer arrives while the eye is still on the
// field. (safety-meter.tsx's 500 is a grader behind a slower edit.)
const DEBOUNCE_MS = 250;

type Status =
  | { kind: "quiet" }
  | { kind: "searching" }
  | { kind: "rows"; rows: DirectoryEntry[] }
  | { kind: "failed" };

const QUIET: Status = { kind: "quiet" };

export function DirectoryCombobox({
  id,
  label,
  value,
  onChange,
  kind = "any",
  disabled,
}: {
  /** Ties the control to a <Field htmlFor>. */
  id?: string;
  /** Accessible name, for a field whose label is not tied by htmlFor. */
  label?: string;
  value: string;
  onChange: (next: string) => void;
  /** Which class of subject the field holds; a kind-less field takes "any". */
  kind?: DirectorySearchKind;
  disabled?: boolean;
}) {
  const [status, setStatus] = React.useState<Status>(QUIET);
  const [absent, setAbsent] = React.useState(false);
  // The DisplayName of a picked group, for as long as the field still holds
  // that group's id. Only groups get one: a user's email is both rendered and
  // stored, so there is nothing for a chip to explain.
  const [pickedGroup, setPickedGroup] = React.useState<string | null>(null);
  // What we last inserted ourselves. Without it, filling the field from a pick
  // re-runs the search for the claim value just resolved and re-opens the list
  // under the admin's cursor.
  const picked = React.useRef<string | null>(null);

  const q = value.trim();
  React.useEffect(() => {
    if (absent || disabled || q.length < MIN_QUERY_LEN || q === picked.current) {
      setStatus(QUIET);
      return;
    }
    let live = true;
    const t = setTimeout(() => {
      setStatus({ kind: "searching" });
      directory
        .search(q, kind)
        .then((rows) => {
          if (!live) return;
          if (rows === null) {
            // No provider configured. Fall back to a plain input and never ask
            // again — nothing is rendered, because nothing is wrong.
            setAbsent(true);
            setStatus(QUIET);
            return;
          }
          setStatus({ kind: "rows", rows });
        })
        .catch(() => {
          if (live) setStatus({ kind: "failed" });
        });
    }, DEBOUNCE_MS);
    return () => {
      live = false;
      clearTimeout(t);
    };
  }, [q, kind, absent, disabled]);

  const pick = (e: DirectoryEntry) => {
    picked.current = e.claim_value;
    setPickedGroup(e.kind === "group" ? e.display_name : null);
    setStatus(QUIET);
    onChange(e.claim_value);
  };

  return (
    <div className="relative">
      <Input
        id={id}
        aria-label={label}
        value={value}
        // Typing invalidates a pick: the field no longer holds that group's id,
        // so the chip explaining it must go with it.
        onChange={(e) => {
          setPickedGroup(null);
          onChange(e.target.value);
        }}
        disabled={disabled}
        className="font-mono"
        autoComplete="off"
        spellCheck={false}
      />

      {/* Suggestions are plain buttons in a list, not a listbox/option widget:
          Tab reaches them natively, so the picker is keyboard-usable without a
          roving-focus implementation — and the input keeps the textbox role it
          has in absent mode, which is the same control. */}
      {status.kind === "rows" && status.rows.length > 0 && (
        <ul className="absolute inset-x-0 top-full z-20 mt-1 max-h-56 overflow-y-auto rounded-md border border-border bg-popover py-1 text-popover-foreground shadow-floating">
          {status.rows.map((e) => (
            <li key={`${e.kind}:${e.claim_value}`}>
              <button
                type="button"
                onClick={() => pick(e)}
                className="block w-full px-3 py-1.5 text-left text-xs hover:bg-muted"
              >
                {suggestion(e)}
              </button>
            </li>
          ))}
        </ul>
      )}

      {status.kind === "searching" && <Hint>{DIRECTORY.SEARCHING}</Hint>}
      {status.kind === "rows" && status.rows.length === 0 && <Hint>{DIRECTORY.NO_MATCHES}</Hint>}
      {status.kind === "failed" && <Hint>{DIRECTORY.LOOKUP_FAILED}</Hint>}

      {pickedGroup !== null && (
        <p className="mt-2 flex flex-wrap items-center gap-2 text-xs leading-snug text-muted-foreground">
          <Chip tone="neutral">{DIRECTORY.GROUP_PICKED_CHIP(pickedGroup)}</Chip>
          {DIRECTORY.GROUP_VALUE_NOTE}
        </p>
      )}
    </div>
  );
}

// The hint slot's shape (form-primitives.tsx's Field), so a lookup message sits
// exactly where a field's helper text would.
function Hint({ children }: { children: React.ReactNode }) {
  return <p className="mt-1 text-xs leading-snug text-muted-foreground">{children}</p>;
}

// One row: the display name, then its detail as the disambiguator (two people
// share a name; the detail is what tells them apart). §7.9 renders the detail
// half mono. The em dash between the two belongs to the FROZEN string, so the
// string is built first and the mono applied to a substring of it — the display
// -vs-canon discipline governance/display.tsx's withMono follows — never a
// separator retyped here.
function suggestion(e: DirectoryEntry): React.ReactNode {
  if (!e.detail) return e.display_name;
  const row = DIRECTORY.SUGGEST_ROW(e.display_name, e.detail);
  return (
    <>
      {row.slice(0, row.length - e.detail.length)}
      <Mono className="text-inherit">{e.detail}</Mono>
    </>
  );
}
