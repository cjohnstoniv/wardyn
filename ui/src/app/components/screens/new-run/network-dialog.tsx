/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// "Network for this run" — one flat surface, no steps.
//
// Ported from the Figma Make redesign (src/components/NetworkDialog.tsx in the
// "Wardyn Simplified" file). The design brief's own diagnosis of what it
// replaced: a stack of disclosures, jargon, and — the important one — the run's
// single most consequential choice buried in a dropdown of dense phrases.
//
// Every control here writes a field WizardState already carries, so this edits
// the same state buildSpec() serialises and adds no new model:
//
//   Allowlist / Open       -> allowAllEgress
//   Hosts this run reaches -> allowedDomains
//   Never allow these      -> deniedDomains  (server: "always wins over allowed")
//   Unlisted-host rule     -> firstUseApproval (FirstUseMode, 3 values)
//
// The three rule cards are NOT a simplification of the enum — they are it,
// one card per value, each stating its consequence rather than naming a mode:
//   Hold it for approval -> wait_for_review  (holds the connection open)
//   Deny, but ask        -> deny_with_review (refuses now, a retry passes)
//   Deny silently        -> always_deny      (no prompt, no wait)
import * as React from "react";
import { X } from "lucide-react";
import type { FirstUseMode } from "../../../lib/types";
import { Button } from "../../ui/button";
import { Input } from "../../ui/input";
import { Dialog, DialogContent, DialogTitle } from "../../ui/dialog";
import { Field } from "../../wardyn/form-primitives";
import { Mono } from "../../wardyn/code-block";
import { cn } from "../../ui/utils";

// Grouped so the list reads as a few decisions instead of one wall of chips.
// The union of every group is exactly PRESET_DOMAINS (wizard-types.ts) — pinned
// by a test, so a host added there can never silently vanish from this dialog.
export const HOST_GROUPS: { group: string; hosts: string[] }[] = [
  { group: "Source", hosts: ["github.com", "*.githubusercontent.com"] },
  {
    group: "Package registries",
    hosts: [
      "registry.npmjs.org",
      "registry.yarnpkg.com",
      "pypi.org",
      "files.pythonhosted.org",
      "proxy.golang.org",
      "repo.maven.apache.org",
      "services.gradle.org",
      "plugins.gradle.org",
      "crates.io",
      "rubygems.org",
    ],
  },
  { group: "Model providers", hosts: ["api.anthropic.com", "api.openai.com"] },
];

export const UNLISTED_RULES: { id: FirstUseMode; title: string; body: string }[] = [
  {
    id: "wait_for_review",
    title: "Hold it for approval",
    body: "The connection waits, live, for the standard 30-second window. Decide in time and it goes through; miss it and it's refused — the approval itself stays open for you to decide.",
  },
  {
    id: "deny_with_review",
    title: "Deny, but ask",
    body: "Refused right away and raised for review. Approve it once and a retry gets through.",
  },
  {
    id: "always_deny",
    title: "Deny silently",
    body: "Refused outright — no prompt, no wait. The run sees a blocked connection.",
  },
];

export interface NetworkSelection {
  allowAllEgress: boolean;
  allowedDomains: string[];
  deniedDomains: string[];
  firstUseApproval: FirstUseMode;
}

function HostChip({ host, on, onToggle }: { host: string; on: boolean; onToggle: () => void }) {
  return (
    <button
      type="button"
      aria-pressed={on}
      onClick={onToggle}
      className={cn(
        "inline-flex items-center gap-1 rounded-md border px-2 py-0.5 font-mono text-[0.6875rem] transition-colors",
        // Selected reads as a SELECTION (the sanctioned teal), never as an action.
        on
          ? "border-primary bg-primary/10 text-primary"
          : "border-border bg-surface-2 text-muted-foreground hover:border-border-strong",
      )}
    >
      {on && <span aria-hidden="true">✓</span>}
      {host}
    </button>
  );
}

export function NetworkDialog({
  open,
  onOpenChange,
  value,
  onSave,
}: {
  open: boolean;
  onOpenChange: (o: boolean) => void;
  value: NetworkSelection;
  onSave: (next: NetworkSelection) => void;
}) {
  // Draft state: the dialog is a transaction. Cancel must leave the run's real
  // selection untouched, so nothing here writes through until Save.
  const [allowAll, setAllowAll] = React.useState(value.allowAllEgress);
  const [allowed, setAllowed] = React.useState<string[]>(value.allowedDomains);
  const [denied, setDenied] = React.useState<string[]>(value.deniedDomains);
  const [rule, setRule] = React.useState<FirstUseMode>(value.firstUseApproval);
  const [customDraft, setCustomDraft] = React.useState("");
  const [denyDraft, setDenyDraft] = React.useState("");

  // Re-seed from the run's current selection each time the dialog OPENS, so a
  // cancelled edit never leaks into the next one.
  //
  // `open` is deliberately the ONLY dependency. `value` is rebuilt as a fresh
  // object literal on every parent render (new-run-screen's netValue), so with
  // it in the deps this effect re-fired on every poll the parent runs — the
  // health probe, the workspace list, the title datalist — and each firing
  // WIPED THE DRAFT MID-EDIT back to the committed state. That is the race
  // that filmed as "the Hold it for approval pick didn't stick": click the
  // rule, a poll lands, the draft resets, Save saves the reset. A dialog's
  // draft belongs to the dialog for as long as it is open.
  //
  // eslint-disable-next-line react-hooks/exhaustive-deps -- see above: re-seed on open only
  React.useEffect(() => {
    if (!open) return;
    setAllowAll(value.allowAllEgress);
    setAllowed(value.allowedDomains);
    setDenied(value.deniedDomains);
    setRule(value.firstUseApproval);
    setCustomDraft("");
    setDenyDraft("");
  }, [open]);

  const presetHosts = React.useMemo(() => HOST_GROUPS.flatMap((g) => g.hosts), []);
  // Anything allowed that isn't a known preset is the operator's own — shown in
  // its own "Yours" group rather than lost among the presets.
  const yours = allowed.filter((h) => !presetHosts.includes(h));

  const toggle = (host: string) =>
    setAllowed((prev) => (prev.includes(host) ? prev.filter((h) => h !== host) : [...prev, host]));

  const addCustom = () => {
    const h = customDraft.trim();
    if (!h || allowed.includes(h)) return setCustomDraft("");
    setAllowed((prev) => [...prev, h]);
    setCustomDraft("");
  };

  const addDenied = () => {
    const h = denyDraft.trim();
    if (!h || denied.includes(h)) return setDenyDraft("");
    setDenied((prev) => [...prev, h]);
    setDenyDraft("");
  };

  const ruleTitle = UNLISTED_RULES.find((r) => r.id === rule)?.title.toLowerCase() ?? "";
  const summary = allowAll
    ? "Any public host allowed · private and internal ranges blocked"
    : `${allowed.length} host${allowed.length === 1 ? "" : "s"} allowed · everything else: ${ruleTitle}`;

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      {/* Same geometry rules as the login dialog (settings/connection-cards.tsx):
          inline translate/transform clearing so nothing inside is trapped by a
          transformed ancestor, an inline width no utility-order race can lose,
          and a height cap that scrolls. */}
      <DialogContent
        className="inset-0 top-0 left-0 m-auto h-fit max-h-[92vh] overflow-y-auto"
        style={{
          width: "min(96vw, 42rem)",
          maxWidth: "min(96vw, 42rem)",
          translate: "none",
          transform: "none",
        }}
      >
        <DialogTitle>Network for this run</DialogTitle>
        <p className="text-[0.8125rem] leading-relaxed text-muted-foreground">
          Choose the hosts this run can reach. Nothing leaves the sandbox except what you allow here — private and
          internal addresses stay blocked no matter what you pick.
        </p>

        {/* Mode — replaces the ambiguous "allow all egress" toggle. Open states
            what it does NOT stop, which the toggle never did. */}
        <div role="radiogroup" aria-label="Network mode" className="grid grid-cols-2 gap-2">
          {[
            { on: !allowAll, label: "Allowlist — only the hosts I pick", set: () => setAllowAll(false) },
            { on: allowAll, label: "Open — any public host", set: () => setAllowAll(true) },
          ].map((m) => (
            <button
              key={m.label}
              type="button"
              role="radio"
              aria-checked={m.on}
              onClick={m.set}
              className={cn(
                "rounded-lg border px-3 py-2 text-left text-[0.8125rem] font-medium transition-colors",
                m.on ? "border-primary bg-primary/10 text-primary" : "border-border text-foreground hover:border-border-strong",
              )}
            >
              {m.label}
            </button>
          ))}
        </div>

        {allowAll ? (
          <section className="rounded-lg border border-border p-3">
            <p className="text-[0.8125rem] leading-relaxed text-muted-foreground">
              Every public host is reachable. Private and internal ranges, and the cloud metadata endpoint, stay
              blocked. This doesn&apos;t stop a run from reaching a brand-new public host — use the block list below
              for anything you never want it to touch.
            </p>
          </section>
        ) : (
          <>
            <section className="rounded-lg border border-border">
              <div className="border-b border-border px-3 py-2">
                <h3 className="text-[0.8125rem] font-medium text-foreground">
                  Hosts this run can reach · {allowed.length}
                </h3>
              </div>
              <div className="space-y-3.5 p-3">
                {HOST_GROUPS.map((g) => (
                  <div key={g.group}>
                    <p className="mb-2 text-[0.625rem] font-medium tracking-wide text-muted-foreground uppercase">
                      {g.group}
                    </p>
                    <div className="flex flex-wrap gap-1.5">
                      {g.hosts.map((h) => (
                        <HostChip key={h} host={h} on={allowed.includes(h)} onToggle={() => toggle(h)} />
                      ))}
                    </div>
                  </div>
                ))}

                {yours.length > 0 && (
                  <div>
                    <p className="mb-2 text-[0.625rem] font-medium tracking-wide text-muted-foreground uppercase">
                      Yours
                    </p>
                    <div className="flex flex-wrap gap-1.5">
                      {yours.map((h) => (
                        <HostChip key={h} host={h} on onToggle={() => toggle(h)} />
                      ))}
                    </div>
                  </div>
                )}

                <Field
                  label="Add a host"
                  htmlFor="net-add"
                  hint={
                    <>
                      A bare host, with an optional leading <Mono>*.</Mono> wildcard or <Mono>:port</Mono> — e.g.{" "}
                      <Mono>*.acme.com</Mono> or <Mono>registry:5000</Mono>.
                    </>
                  }
                >
                  <div className="flex gap-2">
                    <Input
                      id="net-add"
                      className="font-mono"
                      placeholder="api.internal.acme.com"
                      value={customDraft}
                      onChange={(e) => setCustomDraft(e.target.value)}
                      onKeyDown={(e) => {
                        if (e.key === "Enter") {
                          e.preventDefault();
                          addCustom();
                        }
                      }}
                    />
                    <Button type="button" variant="secondary" onClick={addCustom} disabled={!customDraft.trim()}>
                      Add
                    </Button>
                  </div>
                </Field>
              </div>
            </section>

            {/* The run's most consequential choice, out of the dropdown. */}
            <section className="rounded-lg border border-border">
              <div className="border-b border-border px-3 py-2">
                <h3 className="text-[0.8125rem] font-medium text-foreground">
                  When a run reaches a host that isn&apos;t on the list
                </h3>
              </div>
              <div role="radiogroup" aria-label="Unlisted host rule" className="space-y-2 p-3">
                {UNLISTED_RULES.map((r) => (
                  <button
                    key={r.id}
                    type="button"
                    role="radio"
                    aria-checked={rule === r.id}
                    onClick={() => setRule(r.id)}
                    className={cn(
                      "flex w-full items-start gap-2.5 rounded-lg border p-3 text-left transition-colors",
                      rule === r.id ? "border-primary bg-primary/5" : "border-border hover:border-border-strong",
                    )}
                  >
                    <span
                      aria-hidden="true"
                      className={cn(
                        "mt-0.5 grid size-4 shrink-0 place-items-center rounded-full border",
                        rule === r.id ? "border-primary" : "border-border-strong",
                      )}
                    >
                      {rule === r.id && <span className="size-2 rounded-full bg-primary" />}
                    </span>
                    <span>
                      <span className="block text-sm font-medium text-foreground">{r.title}</span>
                      <span className="mt-0.5 block text-[0.6875rem] leading-snug text-muted-foreground">{r.body}</span>
                    </span>
                  </button>
                ))}
              </div>
            </section>
          </>
        )}

        {/* Applies in BOTH modes — the server's denied_domains always wins. */}
        <section className="rounded-lg border border-border">
          <div className="border-b border-border px-3 py-2">
            <h3 className="text-[0.8125rem] font-medium text-foreground">Never allow these · {denied.length}</h3>
          </div>
          <div className="space-y-3 p-3">
            <p className="text-[0.8125rem] text-muted-foreground">
              Blocked even if an allow rule above would otherwise match.
            </p>
            {denied.length > 0 && (
              <div className="flex flex-wrap gap-1.5">
                {denied.map((h) => (
                  <span
                    key={h}
                    className="inline-flex items-center gap-1 rounded-md border border-danger/25 bg-danger-subtle px-2 py-0.5 font-mono text-[0.6875rem] text-danger"
                  >
                    {h}
                    <button
                      type="button"
                      aria-label={`Remove ${h}`}
                      onClick={() => setDenied((list) => list.filter((d) => d !== h))}
                      className="transition-colors hover:text-foreground"
                    >
                      <X className="size-3" />
                    </button>
                  </span>
                ))}
              </div>
            )}
            <div className="flex gap-2">
              <Input
                aria-label="Block a host"
                className="font-mono"
                placeholder="telemetry.example.com"
                value={denyDraft}
                onChange={(e) => setDenyDraft(e.target.value)}
                onKeyDown={(e) => {
                  if (e.key === "Enter") {
                    e.preventDefault();
                    addDenied();
                  }
                }}
              />
              <Button type="button" variant="secondary" onClick={addDenied} disabled={!denyDraft.trim()}>
                Add
              </Button>
            </div>
          </div>
        </section>

        <div className="flex flex-wrap items-center justify-between gap-3">
          <p className="text-[0.75rem] text-muted-foreground">{summary}</p>
          <div className="flex gap-2">
            <Button variant="ghost" onClick={() => onOpenChange(false)}>
              Cancel
            </Button>
            <Button
              onClick={() =>
                onSave({
                  allowAllEgress: allowAll,
                  allowedDomains: allowed,
                  deniedDomains: denied,
                  firstUseApproval: rule,
                })
              }
            >
              Save hosts
            </Button>
          </div>
        </div>
      </DialogContent>
    </Dialog>
  );
}
