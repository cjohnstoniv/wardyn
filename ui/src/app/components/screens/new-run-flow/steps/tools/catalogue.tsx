/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Add tools and access: the CLIs, MCP servers and APIs the person's organisation
// grants, and the doors for a secret, a host, an MCP server by URL and a custom
// component. What it adds shows under Added to this run, with the server's own
// state for each. An organisation row the person is not granted is never listed:
// the server says the same thing whether or not it exists.

import * as React from "react";
import { components as api } from "../../../../../lib/api/components";
import { secrets } from "../../../../../lib/api/secrets";
import reasons from "../../../../../lib/access-reasons.golden.json";
import type { CatalogueCLI, ComponentRef, MyComponents, OrgComponentView } from "../../../../../lib/types/components";
import type { AccessFacts, AccessRow } from "../../../../../lib/types/new-run-access";
import { Button } from "../../../../ui/button";
import { CUSTOM_COMPONENT as CC } from "../../../../wardyn/copy/components";
import { NR_SHARED } from "../../../../wardyn/copy/new-run/shared";
import { NR_TOOLS } from "../../../../wardyn/copy/new-run/tools";
import { Chip } from "../../../../wardyn/primitives";
import { Segmented } from "../../../../wardyn/segmented";
import type { RunIntent } from "../../draft/run-intent";
import { addAllowedHost } from "../../lenses/tools-lens";
import { useNewRunDispatch } from "../../new-run-route";
import { useAnnounce } from "../step-shell";
import { AddMcpDialog } from "./add-mcp-dialog";
import { AddSecretDialog, type SecretAddition } from "./add-secret-dialog";
import { AllowHostDialog } from "./allow-host-dialog";
import { addHeaderSecret, attachComponent, detachComponent, removeAllowedHost, removeHeaderSecret } from "./catalogue-edits";
import { CLI_CARD } from "./catalogue-pending";
import { CustomComponentDialog } from "./custom-component-dialog";

type Category = "all" | "cli" | "mcp" | "api" | "secret";
type Door = "secret" | "host" | "mcp" | "custom";

const SENTENCES = new Map(reasons.map((r) => [r.key, r.text]));
const SECRETS_PATH = "/secrets";
const FROM_KIND: Record<string, string> = {
  custom: NR_TOOLS.FROM_CUSTOM, cli: NR_TOOLS.FROM_CLI, mcp: NR_TOOLS.FROM_MCP, api: NR_TOOLS.FROM_API, secret: NR_TOOLS.FROM_SECRET, host: NR_TOOLS.FROM_HOST,
};

/** The server's sentence for a closed reason code; a code this console does not know says nothing. */
const sentence = (code: string | undefined): string | undefined => (code ? SENTENCES.get(code) : undefined);

function useMyComponents(): MyComponents | null {
  const [mine, setMine] = React.useState<MyComponents | null>(null);
  React.useEffect(() => {
    let live = true;
    api.mine().then((m) => live && setMine(m), () => {});
    return () => { live = false; };
  }, []);
  return mine;
}

function Card({ title, body, children }: { title: string; body?: string; children: React.ReactNode }) {
  return (
    <li className="flex flex-col gap-2 rounded-md border border-border p-3">
      <h3 className="font-medium">{title}</h3>
      {body && <p className="text-sm text-muted-foreground">{body}</p>}
      {children}
    </li>
  );
}

/** Add or Remove for a catalogue row; a refused or unavailable row shows its reason as text beside a disabled Add. */
function RowAction({ name, attached, reason, onAdd, onRemove }: { name: string; attached: boolean; reason?: string; onAdd: () => void; onRemove: () => void }) {
  return (
    <div className="flex flex-col gap-2">
      {reason && <p className="text-sm text-muted-foreground">{reason}</p>}
      {attached ? (
        <Button type="button" variant="ghost" size="sm" className="w-fit" aria-label={`${NR_TOOLS.CARD_REMOVE} ${name}`} onClick={onRemove}>{NR_TOOLS.CARD_REMOVE}</Button>
      ) : (
        <Button type="button" variant="outline" size="sm" className="w-fit" disabled={!!reason} aria-label={`${NR_TOOLS.CARD_ADD} ${name}`} onClick={onAdd}>{NR_TOOLS.CARD_ADD}</Button>
      )}
    </div>
  );
}

function Status({ status, answered }: { status: string; answered: boolean }) {
  // Until the server has answered this body, a state is only Requested.
  if (!answered) return <Chip>{NR_TOOLS.NET_REQUESTED}</Chip>;
  if (status === "ready") return <Chip tone="success" dot>{NR_TOOLS.STATUS_READY}</Chip>;
  if (status === "needs_setup") return <Chip tone="warning" dot>{NR_TOOLS.STATUS_NEEDS_SETUP}</Chip>;
  return null;
}

export interface AddToolsProps {
  /** The preview's `access` block: what the added things bring, and the state the server gives each. */
  access: AccessFacts | undefined;
  /** The server has answered exactly this body; until it has, rows are Requested, not Ready. */
  answered: boolean;
  intent: RunIntent;
}

export function AddTools({ access, answered, intent }: AddToolsProps) {
  const headingId = React.useId();
  const dispatch = useNewRunDispatch();
  const announce = useAnnounce();
  const mine = useMyComponents();
  const [category, setCategory] = React.useState<Category>("all");
  const [door, setDoor] = React.useState<Door | null>(null);
  const [names, setNames] = React.useState<string[] | null>(null);

  // The saved secret names a dialog offers are read when one opens.
  React.useEffect(() => {
    if (!door || door === "host") return;
    let live = true;
    secrets.listSecrets().then((n) => live && setNames(n), () => live && setNames([]));
    return () => { live = false; };
  }, [door]);

  const edit = (apply: (i: RunIntent) => RunIntent) => dispatch({ type: "edit", apply });
  const close = () => { setDoor(null); setNames(null); };
  const added = (message: string) => { announce(message); close(); };
  const attach = (ref: ComponentRef) => { edit((i) => attachComponent(i, ref)); added(ref.name ? NR_TOOLS.COMPONENT_ADDED_NAMED(ref.name) : NR_TOOLS.COMPONENT_ADDED); };
  const addSecret = (a: SecretAddition) => {
    if (a.kind === "resident") return attach(a.ref);
    const { host } = a.secret;
    edit((i) => {
      const next = addHeaderSecret(i, a.secret);
      return access?.hosts.some((h) => h.host === host) ? next : addAllowedHost(next, host);
    });
    added(NR_TOOLS.COMPONENT_ADDED);
  };

  const mayDefine = !!mine?.may_define;
  const residentAllowed = !!mine?.resident_delivery_allowed;
  const residentOff = !mine ? undefined : !mayDefine ? sentence("component_unavailable") : CC.ERR.RESIDENT_OFF;
  const attached = (id: string) => !!intent.components?.some((c) => c.id === id);
  const stateOf = (id: string) => access?.rows.find((r) => r.id === id);
  const org = (kind: OrgComponentView["category"]) => (mine?.org ?? []).filter((o) => o.category === kind);
  const show = (c: Exclude<Category, "all">) => category === "all" || category === c;

  const cliCards = (mine?.clis ?? []).flatMap((cli: CatalogueCLI) => {
    const generic = CLI_CARD[cli.kind];
    if (cli.state !== "available") return [{ key: cli.kind, title: generic.name, body: generic.body, cli, row: undefined as OrgComponentView | undefined }];
    return cli.rows.map((id) => ({ key: id, title: mine?.org.find((o) => o.id === id)?.name ?? generic.name, body: generic.body, cli, row: mine?.org.find((o) => o.id === id) }));
  });

  const orgCard = (row: OrgComponentView) => {
    const residentRow = row.secrets.some((s) => s.delivery.mode !== "header");
    const fact = stateOf(row.id);
    const reason = residentRow && !residentAllowed ? CC.ERR.RESIDENT_OFF : answered ? sentence(fact?.reason) : undefined;
    return (
      <Card key={row.id} title={row.name} body={row.hosts.join(", ")}>
        <div className="flex flex-wrap items-center gap-2">
          <Chip>{NR_TOOLS.FROM_ORG}</Chip>
          {attached(row.id) && fact && <Status status={fact.status} answered={answered} />}
        </div>
        <RowAction
          name={row.name}
          attached={attached(row.id)}
          reason={reason}
          onAdd={() => { edit((i) => attachComponent(i, { id: row.id })); announce(NR_TOOLS.CARD_ADDED(row.name)); }}
          onRemove={() => { edit((i) => detachComponent(i, row.id)); announce(NR_TOOLS.COMPONENT_REMOVED); }}
        />
      </Card>
    );
  };

  const removeAdded = (row: AccessRow) => {
    if (row.id.startsWith("host:")) edit((i) => removeAllowedHost(i, row.name));
    else if (row.id.startsWith("secret:")) edit((i) => removeHeaderSecret(i, row.id.slice("secret:".length)));
    else edit((i) => detachComponent(i, row.id, row.name));
    announce(NR_TOOLS.COMPONENT_REMOVED);
  };
  const addedRows = access?.rows.filter((r) => r.from === "added") ?? [];

  return (
    <section aria-labelledby={headingId} className="flex flex-col gap-3">
      <div>
        <h2 id={headingId} className="font-semibold">{NR_TOOLS.ADD_TITLE}</h2>
        <p className="text-sm text-muted-foreground">{NR_TOOLS.ADD_BODY}</p>
      </div>
      <div role="group" aria-label={NR_TOOLS.CATEGORY_ARIA}>
        <Segmented<Category>
          value={category}
          onChange={setCategory}
          options={[
            { value: "all", label: NR_TOOLS.CATEGORY_ALL },
            { value: "cli", label: NR_TOOLS.CATEGORY_CLI },
            { value: "mcp", label: NR_TOOLS.CATEGORY_MCP },
            { value: "api", label: NR_TOOLS.CATEGORY_API },
            { value: "secret", label: NR_TOOLS.CATEGORY_SECRET },
          ]}
        />
      </div>
      <ul className="grid gap-3 sm:grid-cols-2">
        {show("cli") && cliCards.map(({ key, title, body, cli, row }) => (
          <Card key={key} title={title} body={body}>
            {cli.state === "coming_soon" ? (
              <Chip>{NR_SHARED.COMING_SOON}</Chip>
            ) : cli.state === "not_granted" ? (
              <p className="text-sm text-muted-foreground">{sentence("cli_not_granted")}</p>
            ) : (
              <RowAction
                name={title}
                attached={!!row && attached(row.id)}
                reason={answered ? sentence(row ? stateOf(row.id)?.reason : undefined) : undefined}
                onAdd={() => { if (row) edit((i) => attachComponent(i, { id: row.id })); announce(NR_TOOLS.CARD_ADDED(title)); }}
                onRemove={() => { if (row) edit((i) => detachComponent(i, row.id)); announce(NR_TOOLS.COMPONENT_REMOVED); }}
              />
            )}
          </Card>
        ))}
        {show("mcp") && org("mcp").map(orgCard)}
        {show("api") && org("api").map(orgCard)}
        {show("mcp") && mayDefine && (
          <Card title={NR_TOOLS.MCP_CARD_TITLE} body={NR_TOOLS.MCP_CARD_BODY}>
            <Button type="button" variant="outline" size="sm" className="w-fit" onClick={() => setDoor("mcp")}>{NR_TOOLS.MCP_CARD_BUTTON}</Button>
          </Card>
        )}
        {show("api") && mayDefine && (
          <Card title={NR_TOOLS.CUSTOM_COMPONENT} body={NR_TOOLS.CC_INTRO}>
            <Button type="button" variant="outline" size="sm" className="w-fit" onClick={() => setDoor("custom")}>{NR_TOOLS.CUSTOM_COMPONENT}</Button>
          </Card>
        )}
        {show("secret") && (
          <>
            <Card title={NR_TOOLS.SECRET_CARD_TITLE} body={NR_TOOLS.SECRET_CARD_BODY}>
              <Button type="button" variant="outline" size="sm" className="w-fit" onClick={() => setDoor("secret")}>{NR_TOOLS.SECRET_CARD_BUTTON}</Button>
            </Card>
            <Card title={NR_TOOLS.HOST_CARD_TITLE} body={NR_TOOLS.HOST_CARD_BODY}>
              <Button type="button" variant="outline" size="sm" className="w-fit" onClick={() => setDoor("host")}>{NR_TOOLS.HOST_CARD_BUTTON}</Button>
            </Card>
          </>
        )}
      </ul>
      {addedRows.length > 0 && (
        <div className="flex flex-col gap-2">
          <h3 className="text-sm font-medium">{NR_TOOLS.ADDED_HEADING}</h3>
          <ul className="flex flex-col gap-2">
            {addedRows.map((row) => (
              <li key={row.id} className="flex flex-wrap items-center gap-2 rounded-md border border-border p-3 text-sm">
                <span className="min-w-0 flex-1 [overflow-wrap:anywhere]">{row.name}</span>
                <Chip>{FROM_KIND[row.kind] ?? NR_TOOLS.FROM_CUSTOM}</Chip>
                <Status status={row.status} answered={answered} />
                {answered && sentence(row.reason) && <span className="basis-full text-muted-foreground">{sentence(row.reason)}</span>}
                <Button type="button" variant="ghost" size="sm" aria-label={`${NR_TOOLS.ROW_REMOVE} ${row.name}`} onClick={() => removeAdded(row)}>{NR_TOOLS.ROW_REMOVE}</Button>
              </li>
            ))}
          </ul>
        </div>
      )}
      {door === "host" && <AllowHostDialog onAdd={(host) => { edit((i) => addAllowedHost(i, host)); added(NR_TOOLS.COMPONENT_ADDED); }} onClose={close} />}
      {door === "secret" && names && (
        <AddSecretDialog
          names={names}
          hosts={access?.hosts.map((h) => h.host) ?? []}
          residentAllowed={residentAllowed}
          mayDefine={mayDefine}
          residentOffReason={residentOff ?? ""}
          onAdd={addSecret}
          onClose={close}
        />
      )}
      {door === "mcp" && names && <AddMcpDialog names={names} onAdd={attach} onClose={close} />}
      {door === "custom" && names && <CustomComponentDialog names={names} residentAllowed={residentAllowed} secretsPath={SECRETS_PATH} onAttach={attach} onClose={close} />}
    </section>
  );
}
