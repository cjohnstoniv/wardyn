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
import { Lock, Plus } from "lucide-react";
import { components as api } from "../../../../../lib/api/components";
import { secrets } from "../../../../../lib/api/secrets";
import reasons from "../../../../../lib/access-reasons.golden.json";
import type { CatalogueCLI, CatalogueCLIKind, ComponentDeliveryMode, ComponentRef, MyComponents, OrgComponentView } from "../../../../../lib/types/components";
import type { AccessFacts, AccessRow } from "../../../../../lib/types/new-run-access";
import { Button } from "../../../../ui/button";
import { cn } from "../../../../ui/utils";
import { ADD_ACCESS, CUSTOM_COMPONENT as CC } from "../../../../wardyn/copy/components";
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
import { addedMark, CARD_ICON, CLI_CARD } from "./catalogue-pending";
import { IconTile } from "./icon-tile";
import { CustomComponentDialog } from "./custom-component-dialog";

type Category = "all" | "cli" | "mcp" | "api" | "secret";
type Door = "secret" | "host" | "mcp" | "custom";

const SENTENCES = new Map(reasons.map((r) => [r.key, r.text]));
const SECRETS_PATH = "/secrets";
const FROM_KIND: Record<string, string> = {
  custom: NR_TOOLS.FROM_CUSTOM, cli: NR_TOOLS.FROM_CLI, mcp: NR_TOOLS.FROM_MCP, api: NR_TOOLS.FROM_API, secret: NR_TOOLS.FROM_SECRET, host: NR_TOOLS.FROM_HOST,
};

const MODE_DELIVERY: Record<ComponentDeliveryMode, string> = {
  header: NR_TOOLS.DELIVERY_PROXY, env: NR_TOOLS.DELIVERY_ENV, file: NR_TOOLS.DELIVERY_FILE,
};
// How a CLI row's own credential reaches the run, as the server's access facts
// give it (internal/api/run_access_facts.go): the GitHub App token by the proxy,
// AWS role credentials in the sandbox. Azure's is not built yet, so it shows none.
const CLI_DELIVERY: Partial<Record<CatalogueCLIKind, string>> = {
  github: NR_TOOLS.DELIVERY_PROXY, aws: NR_TOOLS.DELIVERY_SHORT_LIVED,
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

type CardLook = "plain" | "added" | "off";

/** A catalogue card: icon, name, where it comes from, what it is, what it brings, then its action. */
function Card({ icon, title, org, body, brings, look = "plain", children }: { icon: string; title: string; org?: boolean; body?: string; brings?: React.ReactNode; look?: CardLook; children: React.ReactNode }) {
  return (
    <li className={cn("flex flex-col gap-2 rounded-lg border p-3", look === "added" ? "border-primary bg-primary/10" : "border-border bg-surface-2", look === "off" && "bg-muted/50 text-muted-foreground")}>
      <div className="flex items-center gap-2.5">
        <IconTile mark={icon} />
        <div className="min-w-0">
          <h3 className="font-medium">{title}</h3>
          {org && <p className="text-xs text-muted-foreground">{NR_TOOLS.FROM_ORG}</p>}
        </div>
      </div>
      {body && <p className="text-sm text-muted-foreground">{body}</p>}
      {brings}
      <div className="mt-0.5 flex flex-wrap items-center justify-between gap-2">{children}</div>
    </li>
  );
}

/** The hosts a card brings, then how its credential reaches the run. */
function Brings({ hosts, deliveries }: { hosts: readonly string[]; deliveries: readonly string[] }) {
  if (hosts.length === 0 && deliveries.length === 0) return null;
  return (
    <div className="flex flex-wrap gap-1.5">
      {hosts.map((h) => <Chip key={h} mono className="whitespace-normal text-foreground [overflow-wrap:anywhere]">{h}</Chip>)}
      {[...new Set(deliveries)].map((d) => <Chip key={d} className="whitespace-normal">{d}</Chip>)}
    </div>
  );
}

function Reason({ text, id }: { text: string; id?: string }) {
  return (
    <p id={id} className="flex items-start gap-1.5 text-xs text-muted-foreground">
      <Lock aria-hidden className="mt-px size-3.5 shrink-0" />
      {text}
    </p>
  );
}

/**
 * Add, or Remove once added. A row that cannot be added shows its reason in
 * place of Add; an added row keeps its reason beside Remove.
 */
function RowAction({ name, attached, reason, status, onAdd, onRemove }: { name: string; attached: boolean; reason?: string; status?: React.ReactNode; onAdd: () => void; onRemove: () => void }) {
  if (attached) {
    return (
      <>
        <div className="flex min-w-0 flex-col gap-1">{status}{reason && <Reason text={reason} />}</div>
        <Button type="button" variant="outline" size="sm" aria-label={`${NR_TOOLS.CARD_REMOVE} ${name}`} onClick={onRemove}>{NR_TOOLS.CARD_REMOVE}</Button>
      </>
    );
  }
  if (reason) return <Reason text={reason} />;
  return (
    <>
      <span />
      <Button type="button" size="sm" aria-label={`${NR_TOOLS.CARD_ADD} ${name}`} onClick={onAdd}>{NR_TOOLS.CARD_ADD}</Button>
    </>
  );
}

/** A door card: a dialog that adds something of the person's own. */
function DoorCard({ icon, title, body, button, reason, onOpen }: { icon: string; title: string; body: string; button: string; reason?: string; onOpen: () => void }) {
  const reasonId = React.useId();
  return (
    <Card icon={icon} title={title} body={body}>
      {reason ? <Reason text={reason} id={reasonId} /> : <span />}
      <Button type="button" variant="outline" size="sm" disabled={!!reason} aria-describedby={reason ? reasonId : undefined} onClick={onOpen}>{button}</Button>
    </Card>
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
  // Until the server has said, neither door claims a reason; once it says no, both say why.
  const noDefine = mine && !mayDefine ? ADD_ACCESS.NOT_ALLOWED : undefined;
  const noDefineId = React.useId();

  const cliCards = (mine?.clis ?? []).flatMap((cli: CatalogueCLI) => {
    const generic = CLI_CARD[cli.kind];
    if (cli.state !== "available") return [{ key: cli.kind, title: generic.name, cli, row: undefined as OrgComponentView | undefined }];
    return cli.rows.map((id) => ({ key: id, title: mine?.org.find((o) => o.id === id)?.name ?? generic.name, cli, row: mine?.org.find((o) => o.id === id) }));
  });

  const orgCard = (row: OrgComponentView) => {
    const residentRow = row.secrets.some((s) => s.delivery.mode !== "header");
    const fact = stateOf(row.id);
    const reason = residentRow && !residentAllowed ? CC.ERR.RESIDENT_OFF : answered ? sentence(fact?.reason) : undefined;
    const on = attached(row.id);
    return (
      <Card
        key={row.id}
        icon={row.category === "mcp" ? CARD_ICON.mcp : CARD_ICON.api}
        title={row.name}
        org
        brings={<Brings hosts={row.hosts} deliveries={row.secrets.map((s) => MODE_DELIVERY[s.delivery.mode])} />}
        look={on ? "added" : reason ? "off" : "plain"}
      >
        <RowAction
          name={row.name}
          attached={on}
          reason={reason}
          status={on && fact ? <Status status={fact.status} answered={answered} /> : undefined}
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
    <section aria-labelledby={headingId} className="flex flex-col gap-3.5 rounded-xl border border-border bg-card p-[18px]">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="min-w-0 flex-[1_1_20rem]">
          <h2 id={headingId} className="font-semibold">{NR_TOOLS.ADD_TITLE}</h2>
          <p className="text-sm text-muted-foreground">{NR_TOOLS.ADD_BODY}</p>
        </div>
        <div className="flex max-w-[16rem] flex-col items-end gap-1">
          <Button type="button" variant="outline" disabled={!mayDefine} aria-describedby={noDefine ? noDefineId : undefined} onClick={() => setDoor("custom")}>
            <Plus aria-hidden />
            {NR_TOOLS.CUSTOM_COMPONENT}
          </Button>
          {noDefine && <Reason text={noDefine} id={noDefineId} />}
        </div>
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
      <ul className="grid gap-2.5 [grid-template-columns:repeat(auto-fill,minmax(250px,1fr))]">
        {show("cli") && cliCards.map(({ key, title, cli, row }) => {
          const generic = CLI_CARD[cli.kind];
          const on = !!row && attached(row.id);
          const off = cli.state !== "available";
          const reason = cli.state === "not_granted" ? sentence("cli_not_granted") : answered ? sentence(row ? stateOf(row.id)?.reason : undefined) : undefined;
          const delivery = CLI_DELIVERY[cli.kind];
          return (
            <Card
              key={key}
              icon={generic.icon}
              title={title}
              body={generic.body}
              brings={row && <Brings hosts={row.hosts} deliveries={delivery ? [delivery] : []} />}
              look={on ? "added" : off || reason ? "off" : "plain"}
            >
              {cli.state === "coming_soon" ? (
                <Chip>{NR_SHARED.COMING_SOON}</Chip>
              ) : (
                <RowAction
                  name={title}
                  attached={on}
                  reason={reason}
                  status={on && row && stateOf(row.id) ? <Status status={stateOf(row.id)?.status ?? ""} answered={answered} /> : undefined}
                  onAdd={() => { if (row) edit((i) => attachComponent(i, { id: row.id })); announce(NR_TOOLS.CARD_ADDED(title)); }}
                  onRemove={() => { if (row) edit((i) => detachComponent(i, row.id)); announce(NR_TOOLS.COMPONENT_REMOVED); }}
                />
              )}
            </Card>
          );
        })}
        {show("mcp") && org("mcp").map(orgCard)}
        {show("api") && org("api").map(orgCard)}
        {show("secret") && (
          <>
            <DoorCard icon={CARD_ICON.secret} title={NR_TOOLS.SECRET_CARD_TITLE} body={NR_TOOLS.SECRET_CARD_BODY} button={NR_TOOLS.SECRET_CARD_BUTTON} onOpen={() => setDoor("secret")} />
            <DoorCard icon={CARD_ICON.host} title={NR_TOOLS.HOST_CARD_TITLE} body={NR_TOOLS.HOST_CARD_BODY} button={NR_TOOLS.HOST_CARD_BUTTON} onOpen={() => setDoor("host")} />
            <DoorCard icon={CARD_ICON.mcp} title={NR_TOOLS.MCP_CARD_TITLE} body={NR_TOOLS.MCP_CARD_BODY} button={NR_TOOLS.MCP_CARD_BUTTON} reason={noDefine} onOpen={() => setDoor("mcp")} />
          </>
        )}
      </ul>
      {addedRows.length > 0 && (
        <div className="flex flex-col gap-2">
          <h3 className="text-sm font-medium">{NR_TOOLS.ADDED_HEADING}</h3>
          <ul className="flex flex-col gap-2">
            {addedRows.map((row) => (
              <li key={row.id} className="flex flex-wrap items-center gap-2 rounded-lg border border-border bg-surface-2 p-3 text-sm">
                <IconTile mark={addedMark(row)} />
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
