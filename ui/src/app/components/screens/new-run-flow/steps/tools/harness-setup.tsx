/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Harness setup: per harness, its model provider, what happens to a tool with no
// rule, its tool rules and its MCP servers. Every choice offered is one the
// server's `access.harnesses` fact says it will accept; a choice it will refuse
// is greyed with the server's sentence.

import * as React from "react";
import type { ToolEffect } from "../../../../../lib/tool-effect";
import type { AccessFacts, AccessRow, ToolPolicyFact } from "../../../../../lib/types/new-run-access";
import type { HarnessSetFact } from "../../../../../lib/types/new-run-contract";
import type { SetupStatus } from "../../../../../lib/types/setup";
import { Button } from "../../../../ui/button";
import { Label } from "../../../../ui/label";
import { NR_TOOLS } from "../../../../wardyn/copy/new-run/tools";
import { Chip } from "../../../../wardyn/primitives";
import { signsIn } from "../../../../../lib/model-access";
import { useModelAccessDoor, useShellSetupStatus } from "../../../../wardyn/model-access-context";
import type { RunIntent } from "../../draft/run-intent";
import { providerControlId, setModelProvider, startsWithRun, type HarnessNames } from "../../lenses/tools-lens";
import { useNewRunDispatch } from "../../new-run-route";
import { useAnnounce } from "../step-shell";
import { EffectChoice } from "./effect-choice";
import { addToolRule, personalRules, removeToolRule, setDefaultEffect, mcpComponentIndex, setMcpHarnesses, type McpComponentRef } from "./harness-edits";
import { McpToggles } from "./mcp-toggles";
import { defaultChoices, reasonText } from "./tool-facts";
import { ToolRules } from "./tool-rules";

const SELECT_CLASS =
  "h-9 w-full max-w-sm rounded-md border border-input bg-input-background px-3 text-sm outline-none focus-visible:border-ring focus-visible:ring-[3px] focus-visible:ring-ring";
const NEEDS_SIGN_IN = ["not_configured", "expired_signin"];

export interface HarnessSetupProps {
  /** The preview's `access` block; nothing shows until the server has answered once. */
  access: AccessFacts | undefined;
  /** The preview's `harness_set`: the provider the server chose for a harness whose draft entry names none. */
  harnessSet?: HarnessSetFact;
  intent: RunIntent;
  /** The draft's components that `access` answered; its "inline:<n>" rows point into this list. */
  answeredFor?: RunIntent["components"];
  /** The server has answered exactly this body; until it has, the last answer is held, not claimed. */
  answered: boolean;
  names?: HarnessNames;
  /** A provider sign-in finished: the server is asked again. */
  onSignedIn: () => void;
}

/** The provider a harness's model credential comes from, by name: the person's pick, else the server's own choice. */
export function providerNameFor(harness: string, intent: RunIntent, harnessSet: HarnessSetFact | undefined, status: SetupStatus | null): string | undefined {
  const id = (intent.tools ?? []).find((t) => t.id === harness)?.model_provider || harnessSet?.harnesses.find((h) => h.id === harness)?.model_credential?.provider;
  return id ? (status?.model_providers?.find((p) => p.id === id)?.name ?? id) : undefined;
}

function ProviderPicker({ fact, access, harnessSet, intent, status, onSignedIn }: { fact: ToolPolicyFact; access: AccessFacts; harnessSet?: HarnessSetFact; intent: RunIntent; status: SetupStatus | null; onSignedIn: () => void }) {
  const dispatch = useNewRunDispatch();
  const announce = useAnnounce();
  const door = useModelAccessDoor();
  const providers = (status?.model_providers ?? []).filter((p) => p.harnesses.includes(fact.harness) && !p.disabled);
  const picked = (intent.tools ?? []).find((t) => t.id === fact.harness)?.model_provider ?? "";
  // The server's own choice (the roster default or the only candidate) stands in until the person picks; it is shown, never written to the draft.
  const chosenId = harnessSet?.harnesses.find((h) => h.id === fact.harness)?.model_credential?.provider ?? "";
  const selected = picked || (providers.some((p) => p.id === chosenId) ? chosenId : "");
  const current = providers.find((p) => p.id === selected);
  const asked = access.setup.some((i) => i.fix?.action === "choose_model_provider" && i.fix.tool === fact.harness);
  // A harness Wardyn sets nothing up for has no provider to pick.
  if (fact.lane === "unsupported") return null;

  const id = providerControlId(fact.harness);
  const name = current?.name ?? selected;
  const credential = access.rows
    .find((r) => r.kind === "harness" && r.name === fact.harness)
    ?.credentials.map((c) => access.credentials.find((x) => x.id === c))
    .find((c) => c?.id.endsWith(":model"));
  const delivery = credential?.delivery === "proxy" ? NR_TOOLS.PROVIDER_PROXY : credential?.delivery === "sandbox_short_lived" ? NR_TOOLS.DELIVERY_SHORT_LIVED : undefined;
  const state = status?.provider_access?.find((a) => a.provider === selected)?.state ?? "";
  const signedOut = NEEDS_SIGN_IN.includes(state);
  // A stored sign-in keeps its door: AWS can refuse to renew a session that still reads as signed in.
  const stored = !!current && signsIn(current.kind) && (state === "live" || state === "expiring");

  return (
    <div className="flex flex-col gap-2">
      <Label htmlFor={id}>{NR_TOOLS.PROVIDER_LABEL}</Label>
      <select
        id={id}
        className={SELECT_CLASS}
        value={selected}
        disabled={providers.length === 0}
        aria-describedby={`${id}-help`}
        onChange={(e) => {
          const provider = e.target.value;
          dispatch({ type: "edit", apply: (i) => setModelProvider(i, fact.harness, provider) });
          const chosen = providers.find((p) => p.id === provider);
          if (chosen) announce(NR_TOOLS.PROVIDER_SET(chosen.name ?? chosen.id));
        }}
      >
        <option value="">{NR_TOOLS.PROVIDER_CHOOSE}</option>
        {providers.map((p) => <option key={p.id} value={p.id}>{p.name ?? p.id}</option>)}
      </select>
      <p id={`${id}-help`} className="text-sm text-muted-foreground">{selected ? delivery : asked ? NR_TOOLS.PROVIDER_REQUIRED : null}</p>
      {selected && (signedOut || stored) && (
        <Button
          type="button"
          variant="outline"
          size="sm"
          className="w-fit"
          onClick={(e) => door.openDoor({ for: { provider: selected }, returnTo: e.currentTarget, onSignedIn: () => { announce(NR_TOOLS.SIGNED_IN); onSignedIn(); } })}
        >
          {signedOut ? NR_TOOLS.SETUP_SIGN_IN_TO(name) : NR_TOOLS.SETUP_SIGN_IN_AGAIN}
        </Button>
      )}
    </div>
  );
}

function HarnessCard({ fact, access, harnessSet, intent, answeredFor, name, answered, status, onSignedIn }: { fact: ToolPolicyFact; access: AccessFacts; harnessSet?: HarnessSetFact; intent: RunIntent; answeredFor?: RunIntent["components"]; name: string; answered: boolean; status: SetupStatus | null; onSignedIn: () => void }) {
  const dispatch = useNewRunDispatch();
  const announce = useAnnounce();
  const door = useModelAccessDoor();
  const labelId = React.useId();
  const tool = (intent.tools ?? []).find((t) => t.id === fact.harness);
  const edit = (apply: (i: RunIntent) => RunIntent) => dispatch({ type: "edit", apply });
  const unsupported = fact.lane === "unsupported";
  // Run it removes the personal default, so until the server answers again its own echo of the old default must not win.
  const [picked, setPicked] = React.useState<ToolEffect>();
  React.useEffect(() => { if (answered) setPicked(undefined); }, [answered]);
  const selected = (!answered && picked) || tool?.default_effect || fact.default.effective;
  const help = fact.lane === "attended" ? NR_TOOLS.DEFAULT_HELP_ATTENDED : door.operator ? NR_TOOLS.DEFAULT_HELP_OPERATOR : NR_TOOLS.DEFAULT_HELP_GATED;
  const servers = access.rows.filter((r) => r.kind === "mcp" && r.mcp);

  const componentId = (server: AccessRow) => server.source.id ?? server.id;
  // The person's own choice for the server stands until the server's next answer; before one, the server's fact does.
  const harnessesOf = (server: AccessRow): string[] =>
    ((intent.components ?? []) as McpComponentRef[])[mcpComponentIndex(intent, componentId(server), server.name, answeredFor)]?.mcp_harnesses ?? server.mcp?.harnesses ?? [];
  const toggle = (server: AccessRow, on: boolean) => {
    const now = harnessesOf(server);
    const next = on ? [...new Set([...now, fact.harness])] : now.filter((h) => h !== fact.harness);
    edit((i) => setMcpHarnesses(i, componentId(server), server.name, next, answeredFor));
  };

  return (
    <section aria-label={name} className="flex flex-col gap-3.5 rounded-lg border border-border bg-surface-2 p-3.5">
      <div className="flex flex-wrap items-center justify-between gap-2.5">
        <h3 className="font-medium">{name}</h3>
        <Chip>{startsWithRun(intent, fact.harness) ? NR_TOOLS.SUB_STARTS : NR_TOOLS.SUB_AVAILABLE}</Chip>
      </div>
      <div className="grid gap-3 [grid-template-columns:repeat(auto-fit,minmax(230px,1fr))]">
        <ProviderPicker fact={fact} access={access} harnessSet={harnessSet} intent={intent} status={status} onSignedIn={onSignedIn} />
        {!unsupported && (
          <div className="flex flex-col gap-2">
            <span id={labelId} className="text-sm font-medium">{NR_TOOLS.DEFAULT_LABEL}</span>
            <EffectChoice
              name={`nr-default-${fact.harness}`}
              labelledBy={labelId}
              value={selected as ToolEffect}
              choices={defaultChoices(fact, name)}
              onChange={(effect) => { setPicked(effect); edit((i) => setDefaultEffect(i, fact.harness, effect)); }}
            />
            <p className="text-sm text-muted-foreground">{help}</p>
            {fact.rule_tools === "mcp_only" && <p className="text-sm text-muted-foreground">{NR_TOOLS.CODEX_NOTE}</p>}
          </div>
        )}
      </div>
      {unsupported ? (
        <p className="text-sm text-muted-foreground">{reasonText(fact.default.options[0]?.reason ?? "custom_harness_rules", { harness: name })}</p>
      ) : (
        <ToolRules
          fact={fact}
          harnessName={name}
          personal={personalRules(intent, fact.harness)}
          onAdd={(rule) => { edit((i) => addToolRule(i, fact.harness, rule)); announce(NR_TOOLS.RULE_ADDED); }}
          onRemove={(t) => edit((i) => removeToolRule(i, fact.harness, t))}
        />
      )}
      <McpToggles fact={fact} harnessName={name} servers={servers} isOn={(s) => harnessesOf(s).includes(fact.harness)} onToggle={toggle} />
    </section>
  );
}

export function HarnessSetup({ access, harnessSet, intent, answeredFor, answered, names, onSignedIn }: HarnessSetupProps) {
  const headingId = React.useId();
  const { status } = useShellSetupStatus();
  if (!access) return null;
  return (
    <section aria-labelledby={headingId} className="flex flex-col gap-3.5 rounded-xl border border-border bg-card p-[18px]">
      <div>
        <h2 id={headingId} className="font-semibold">{NR_TOOLS.HARNESS_SETUP_TITLE}</h2>
        <p className="text-sm text-muted-foreground">{NR_TOOLS.HARNESS_SETUP_BODY}</p>
      </div>
      {access.harnesses.length === 0 ? (
        <p className="text-sm text-muted-foreground">{NR_TOOLS.NO_HARNESSES}</p>
      ) : (
        access.harnesses.map((fact) => (
          <HarnessCard key={fact.harness} fact={fact} access={access} harnessSet={harnessSet} intent={intent} answeredFor={answeredFor} name={names?.get(fact.harness) ?? fact.harness} answered={answered} status={status} onSignedIn={onSignedIn} />
        ))
      )}
    </section>
  );
}
