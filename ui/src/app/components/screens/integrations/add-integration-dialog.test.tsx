/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// ONE dialog (B3), replacing the AddServiceDialog ⇄ AddIntegrationDialog
// double-mount: kind pick -> [flavor pick, AI kinds only] -> base form -> save
// -> Test. Every kind — closed or generic — walks the same ladder; AI kinds
// only change what the form is prefilled with.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { baseStatus } from "../setup/test-fixtures";
import { SUBSCRIPTION_LANE_META } from "../../../lib/integrations";
import { AddIntegrationDialog, buildIntegrationWrite, type ExistingIntegrationRef, type IntegrationFormValues } from "./add-integration-dialog";
import { integrationTypeById } from "../../../lib/integration-catalog";
import type { IntegrationWrite } from "../../../lib/api/integrations";

const getSetupStatusMock = vi.fn();
vi.mock("../../../lib/api/setup", () => ({ setup: { getSetupStatus: (...a: unknown[]) => getSetupStatusMock(...a) } }));

const listSecretsMock = vi.fn();
vi.mock("../../../lib/api/secrets", () => ({ secrets: { listSecrets: (...a: unknown[]) => listSecretsMock(...a) } }));

const putIntegrationMock = vi.fn();
const testIntegrationMock = vi.fn();
vi.mock("../../../lib/api/integrations", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../../lib/api/integrations")>();
  return {
    ...actual,
    genericIntegrationsApi: {
      ...actual.genericIntegrationsApi,
      put: (...a: unknown[]) => putIntegrationMock(...a),
      test: (...a: unknown[]) => testIntegrationMock(...a),
    },
  };
});

vi.mock("../setup/harness-login-pane", () => ({
  HarnessLoginPane: (p: { onDone: () => void; onCancel: () => void }) => (
    <div data-testid="harness-login-pane">
      <button onClick={p.onDone}>finish-login</button>
      <button onClick={p.onCancel}>cancel-login</button>
    </div>
  ),
}));

beforeEach(() => {
  getSetupStatusMock.mockReset().mockResolvedValue(baseStatus());
  listSecretsMock.mockReset().mockResolvedValue([]);
  putIntegrationMock.mockReset().mockResolvedValue(undefined);
  testIntegrationMock.mockReset().mockResolvedValue({ state: "not_tested" });
});

function renderDialog(existingRows: ExistingIntegrationRef[] = []) {
  const onOpenChange = vi.fn();
  const onSaved = vi.fn();
  render(<AddIntegrationDialog open onOpenChange={onOpenChange} existingRows={existingRows} onSaved={onSaved} />);
  return { onOpenChange, onSaved };
}

describe("AddIntegrationDialog — kind pick", () => {
  it("searches across every kind, closed and generic alike, from one box", async () => {
    const user = userEvent.setup();
    renderDialog();
    const search = await screen.findByRole("textbox", { name: /search integration types/i });

    await user.type(search, "jfrog");
    expect(await screen.findByText("JFrog Artifactory")).toBeInTheDocument();
  });

  it("browses by section, but never shows an empty section (the deleted notbuilt groups)", async () => {
    renderDialog();
    await screen.findByText("Model providers");
    expect(screen.queryByText("Cloud providers")).not.toBeInTheDocument();
    expect(screen.queryByText("Data stores")).not.toBeInTheDocument();
  });

  it("a single-lane kind (OpenAI) skips the flavor step and lands straight on the form", async () => {
    const user = userEvent.setup();
    renderDialog();
    await user.type(await screen.findByRole("textbox", { name: /search integration types/i }), "codex");
    await user.click(await screen.findByText("OpenAI", { exact: true }));

    expect(await screen.findByRole("heading", { name: /add integration — openai/i })).toBeInTheDocument();
  });
});

describe("AddIntegrationDialog — AI flavor step", () => {
  it("Claude subscription opens on the flavor panel, managed recommended", async () => {
    const user = userEvent.setup();
    renderDialog();
    await user.type(await screen.findByRole("textbox", { name: /search integration types/i }), "subscription");
    await user.click(await screen.findByText("Claude subscription"));

    expect(await screen.findByText(SUBSCRIPTION_LANE_META.managed.title)).toBeInTheDocument();
    expect(screen.getByText("Recommended")).toBeInTheDocument();
  });

  it("Bedrock opens on its four-lane flavor panel", async () => {
    const user = userEvent.setup();
    renderDialog();
    await user.type(await screen.findByRole("textbox", { name: /search integration types/i }), "bedrock");
    await user.click(await screen.findByText("AWS Bedrock", { exact: true }));

    expect(await screen.findByText("Bearer token")).toBeInTheDocument();
    expect(screen.getByText("AWS SSO")).toBeInTheDocument();
    expect(screen.getByText("Access keys")).toBeInTheDocument();
  });

  // ui-integrations-2: the 4-lane Bedrock picker used to signal selection with
  // a CSS class swap only — no aria-pressed/aria-checked, unlike the sibling
  // OptionCard picker (Subscription lane) one step earlier. Every lane button
  // now exposes its selected state via aria-pressed.
  it("the Bedrock lane picker exposes selected state via aria-pressed", async () => {
    const user = userEvent.setup();
    renderDialog();
    await user.type(await screen.findByRole("textbox", { name: /search integration types/i }), "bedrock");
    await user.click(await screen.findByText("AWS Bedrock", { exact: true }));
    await screen.findByText("Bearer token");

    const bearer = screen.getByRole("button", { name: /bearer token/i });
    const sso = screen.getByRole("button", { name: /aws sso/i });
    expect(bearer).toHaveAttribute("aria-pressed", "true");
    expect(sso).toHaveAttribute("aria-pressed", "false");

    await user.click(sso);
    expect(sso).toHaveAttribute("aria-pressed", "true");
    expect(bearer).toHaveAttribute("aria-pressed", "false");
  });

  it("Back from the flavor step returns to the pick panel, not straight to the form", async () => {
    const user = userEvent.setup();
    renderDialog();
    await user.type(await screen.findByRole("textbox", { name: /search integration types/i }), "bedrock");
    await user.click(await screen.findByText("AWS Bedrock", { exact: true }));
    await screen.findByText("Bearer token");

    await user.click(screen.getByRole("button", { name: "Back" }));
    expect(await screen.findByRole("textbox", { name: /search integration types/i })).toBeInTheDocument();
  });
});

describe("AddIntegrationDialog — base form and save", () => {
  it("a generic kind prefills name/hosts/secret and saves at the slugified id", async () => {
    const user = userEvent.setup();
    renderDialog();
    await user.type(await screen.findByRole("textbox", { name: /search integration types/i }), "jira");
    await user.click(await screen.findByText("Jira", { exact: true }));

    expect(await screen.findByDisplayValue("Jira")).toBeInTheDocument();
    expect(screen.getByDisplayValue("jira-token")).toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: /add integration/i }));

    await waitFor(() =>
      expect(putIntegrationMock).toHaveBeenCalledWith(
        "jira",
        expect.objectContaining({
          kind: "jira",
          egress: ["<site>.atlassian.net"],
          // Every saved row gets a probe against its own first egress host —
          // "Test" is never a dead button on a freshly-added integration.
          probe: { method: "GET", url: "https://<site>.atlassian.net/" },
        }),
      ),
    );
    // Landed on the post-save Test/Done panel, not closed outright.
    expect(await screen.findByRole("button", { name: "Test connection" })).toBeInTheDocument();
  });

  it("warns on a slug collision with a STORED row before Save silently replaces it", async () => {
    const user = userEvent.setup();
    renderDialog([{ id: "jira", name: "Old Jira", stored: true }]);
    await user.type(await screen.findByRole("textbox", { name: /search integration types/i }), "jira");
    await user.click(await screen.findByText("Jira", { exact: true }));

    expect(await screen.findByText(/Replaces the existing/)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /add integration/i })).toBeEnabled();
  });

  // The server now 409s a PUT whose id names a row that exists only as a
  // DERIVATION — adoption is explicit (POST {id}/adopt), never a side effect
  // of this dialog. Block Save rather than let the operator hit that wall.
  it("blocks Save on a collision with a DERIVED (not-yet-stored) row and points at Adopt to edit instead", async () => {
    const user = userEvent.setup();
    renderDialog([{ id: "jira", name: "Jira", stored: false }]);
    await user.type(await screen.findByRole("textbox", { name: /search integration types/i }), "jira");
    await user.click(await screen.findByText("Jira", { exact: true }));

    expect(await screen.findByText(/Adopt to edit/)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /add integration/i })).toBeDisabled();
    expect(putIntegrationMock).not.toHaveBeenCalled();
  });

  // Bug report 2026-08-15: a managed subscription is STRUCTURALLY derived — you
  // connect it by logging in (the pane), never by a PUT here (the server 409s
  // "exists only as a derived row"). The first fix gated the block on a captured
  // harness in `status`, but the dialog reads status once on open and a login
  // done in the pane never refreshes it — so a just-logged-in subscription
  // (status snapshot still empty) slipped through and 409'd. The block must fire
  // for the lane UNCONDITIONALLY, even with an empty harness in status.
  it("managed subscription shows an enabled Done (never Add) that closes the dialog without a PUT — even with nothing captured in status", async () => {
    const user = userEvent.setup();
    getSetupStatusMock.mockResolvedValue(baseStatus()); // no captured harness — the exact stale-status bug
    const { onOpenChange, onSaved } = renderDialog();
    await user.type(await screen.findByRole("textbox", { name: /search integration types/i }), "subscription");
    await user.click(await screen.findByText("Claude subscription"));
    // flavor panel, managed lane is the default — Continue to the base form
    await screen.findByText(SUBSCRIPTION_LANE_META.managed.title);
    await user.click(screen.getByRole("button", { name: /continue/i }));

    expect(await screen.findByText(/logging in above/i)).toBeInTheDocument();
    // No Add button at all on a derived lane — the primary action is an enabled Done
    expect(screen.queryByRole("button", { name: /add integration/i })).not.toBeInTheDocument();
    const done = screen.getByRole("button", { name: /^done$/i });
    expect(done).toBeEnabled();
    await user.click(done);
    // Done closes + reloads the list (finish) with no doomed PUT and no "Saved" step
    expect(onOpenChange).toHaveBeenCalledWith(false);
    expect(onSaved).toHaveBeenCalled();
    expect(putIntegrationMock).not.toHaveBeenCalled();
  });

  // ui-integrations-7: canSave used to check only name/hostList/isSubscription/
  // isBedrock — none of the per-kind required secret-name fields. Clearing a
  // prefilled required secret left Save clickable; the empty secret_name was
  // only ever caught by a raw server 400.
  it("clearing a required secret-name field (git host PAT) disables Save", async () => {
    const user = userEvent.setup();
    renderDialog();
    await user.type(await screen.findByRole("textbox", { name: /search integration types/i }), "gitlab");
    await user.click(await screen.findByText("GitLab", { exact: true }));

    const pat = await screen.findByDisplayValue("gitlab-pat");
    expect(screen.getByRole("button", { name: /add integration/i })).toBeEnabled();

    await user.clear(pat);
    expect(screen.getByRole("button", { name: /add integration/i })).toBeDisabled();

    await user.type(pat, "gitlab-pat");
    expect(screen.getByRole("button", { name: /add integration/i })).toBeEnabled();
  });

  // ui-integrations-1: Name, Region, Model, and every SecretRow secret-name
  // field render Label/Input as unlinked siblings — getByLabelText only
  // resolves when the pair is actually wired with id/htmlFor.
  it("Name and every SecretRow secret-name field are reachable by label", async () => {
    const user = userEvent.setup();
    renderDialog();
    await user.type(await screen.findByRole("textbox", { name: /search integration types/i }), "gitlab");
    await user.click(await screen.findByText("GitLab", { exact: true }));
    await screen.findByDisplayValue("GitLab");

    expect(screen.getByLabelText("Name")).toHaveValue("GitLab");
    expect(screen.getByLabelText("Personal access token")).toHaveValue("gitlab-pat");
  });

  it("Bedrock's Region/Model and its bearer-token SecretRow are reachable by label", async () => {
    const user = userEvent.setup();
    renderDialog();
    await user.type(await screen.findByRole("textbox", { name: /search integration types/i }), "bedrock");
    await user.click(await screen.findByText("AWS Bedrock", { exact: true }));
    await screen.findByText("Bearer token");
    await user.click(screen.getByRole("button", { name: "Continue" }));

    expect(await screen.findByLabelText("Bearer token")).toHaveValue("bedrock-api-key");
    expect(screen.getByLabelText("Region")).toBeInTheDocument();
    expect(screen.getByLabelText("Model")).toBeInTheDocument();
  });

  it("only an AI kind offers the Defaults section", async () => {
    const user = userEvent.setup();
    renderDialog();
    await user.type(await screen.findByRole("textbox", { name: /search integration types/i }), "jira");
    await user.click(await screen.findByText("Jira", { exact: true }));
    await screen.findByDisplayValue("Jira");
    expect(screen.queryByText("Default for agent runs")).not.toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "Back" }));
    await user.clear(screen.getByRole("textbox", { name: /search integration types/i }));
    await user.type(screen.getByRole("textbox", { name: /search integration types/i }), "codex");
    await user.click(await screen.findByText("OpenAI", { exact: true }));

    expect(await screen.findByText("Default for agent runs")).toBeInTheDocument();
  });

  it("GitHub App prefills two secret fields (App ID + Private key), never a header", async () => {
    const user = userEvent.setup();
    renderDialog();
    await user.type(await screen.findByRole("textbox", { name: /search integration types/i }), "github");
    await user.click(await screen.findByText("GitHub", { exact: true }));

    expect(await screen.findByDisplayValue("github-app-id")).toBeInTheDocument();
    expect(screen.getByDisplayValue("github-app-key")).toBeInTheDocument();
  });

  // Nit: Azure's only host is the "<resource>.openai.azure.com" TEMPLATE. Shown
  // as a placeholder with an empty value — prefilling it as a value made an
  // un-edited save 400 on the literal "<resource>".
  it("Azure OpenAI shows its <resource> host as a placeholder, never a pre-filled value that 400s", async () => {
    const user = userEvent.setup();
    renderDialog();
    await user.type(await screen.findByRole("textbox", { name: /search integration types/i }), "azure");
    await user.click(await screen.findByText("Azure OpenAI", { exact: true }));

    const hostBox = await screen.findByPlaceholderText("<resource>.openai.azure.com");
    expect(hostBox).toHaveValue("");
  });
});

describe("AddIntegrationDialog — post-save Test", () => {
  it("Test calls test() and renders the verdict its response body carries — never assumes a 2xx means passed", async () => {
    const user = userEvent.setup();
    renderDialog();
    await user.type(await screen.findByRole("textbox", { name: /search integration types/i }), "jira");
    await user.click(await screen.findByText("Jira", { exact: true }));
    await user.click(screen.getByRole("button", { name: /add integration/i }));
    await screen.findByRole("button", { name: "Test connection" });

    testIntegrationMock.mockResolvedValueOnce({ state: "passed" });
    await user.click(screen.getByRole("button", { name: "Test connection" }));

    await waitFor(() => expect(testIntegrationMock).toHaveBeenCalledWith("jira"));
    expect(await screen.findByText("verified")).toBeInTheDocument();
  });

  it("Done closes the dialog and calls onSaved so the list reloads", async () => {
    const user = userEvent.setup();
    const { onOpenChange, onSaved } = renderDialog();
    await user.type(await screen.findByRole("textbox", { name: /search integration types/i }), "jira");
    await user.click(await screen.findByText("Jira", { exact: true }));
    await user.click(screen.getByRole("button", { name: /add integration/i }));
    await user.click(await screen.findByRole("button", { name: "Done" }));

    expect(onOpenChange).toHaveBeenCalledWith(false);
    expect(onSaved).toHaveBeenCalled();
  });
});

// ── Non-mocked PUT-payload contract — defeats the put()-mock blind spot ───────
//
// Every test above mocks genericIntegrationsApi.put, so a payload the REAL
// server 400s (a proxy_header delivery with no header, a bedrock secret named
// something the transport never reads, a probe that brands a healthy AI row
// "failed") sails through unnoticed. This suite calls the SAME payload builder
// the dialog's save() uses (buildIntegrationWrite) for every flavor and asserts
// the body against a mirror of the server's write rules. No put() — mocked or
// otherwise — is involved: the object asserted IS the one save() hands to PUT.

// RFC 9110 tchar — the exact charset egress.ValidHeaderName accepts
// (internal/egress/egress.go): letters, digits and !#$%&'*+-.^_`|~ (no space,
// no ':'). ValidHeaderName("") is false, which is what defect 1 tripped.
const HEADER_TOKEN = /^[A-Za-z0-9!#$%&'*+.^_`|~-]+$/;

// The server's ClosedIntegrationKinds (internal/types/workspace.go): a closed
// kind MAY omit a secret's delivery (its bespoke transport carries it); a
// generic kind's secret row MUST declare one (validateIntegrationWrite).
const CLOSED_KINDS = new Set([
  "anthropic_api_key",
  "anthropic_subscription",
  "bedrock",
  "openai_api_key",
  "azure_openai",
  "github_app",
  "git_host",
]);

// Why the server would reject `body` on write, or null if it would accept it — a
// focused mirror of validateIntegrationWrite/validateIntegrationDelivery for the
// fields this dialog sets. It encodes the RULES the three defects violated, so
// the assertion is the contract, not a snapshot of today's output.
function serverRejection(body: IntegrationWrite): string | null {
  for (const s of body.secrets ?? []) {
    if (!s.secret_name) return `secret ${s.role}: empty secret_name`;
    if (!s.delivery) {
      if (!CLOSED_KINDS.has(body.kind)) return `secret ${s.role}: generic kind ${body.kind} needs a delivery`;
      continue; // closed kind: bespoke transport, delivery legitimately omitted
    }
    if (s.delivery.mode !== "proxy_header") return `secret ${s.role}: operator delivery.mode ${s.delivery.mode} is refused`;
    if (!s.delivery.header || !HEADER_TOKEN.test(s.delivery.header)) return `secret ${s.role}: invalid header ${JSON.stringify(s.delivery.header)}`;
    const fmt = s.delivery.format;
    if (fmt && ((fmt.match(/%s/g) ?? []).length !== 1 || (fmt.match(/%/g) ?? []).length !== 1)) return `secret ${s.role}: invalid format ${fmt}`;
  }
  if ((body.secrets ?? []).filter((s) => s.delivery?.mode === "proxy_header").length > 1) return "more than one proxy_header secret";
  if (body.probe) {
    if (body.probe.method !== "GET" && body.probe.method !== "HEAD") return `bad probe method ${body.probe.method}`;
    // Mirrors validSiteURL (internal/api/site_config.go): a wildcard or
    // port-qualified host makes url.Parse's Host invalid for
    // workspacescan.ValidApprovedHost — the server 400s "probe.url: invalid
    // URL" (W11-S1-3).
    const host = body.probe.url.replace(/^https?:\/\//, "").replace(/\/.*$/, "");
    if (host.startsWith("*.") || host.includes(":")) return `probe.url host ${JSON.stringify(host)} is a wildcard/port — validSiteURL rejects it`;
  }
  return null;
}

// Default form values for a picked catalog type — mirrors BaseForm's useState
// seeds (the secret-name defaults are the ones the operator sees prefilled). A
// "<…>"-templated host is dropped from hostList the way the operator can't save
// the literal template.
function values(catalogId: string, over: Partial<IntegrationFormValues> = {}): IntegrationFormValues {
  const type = integrationTypeById(catalogId)!;
  return {
    type,
    name: type.label,
    hostList: type.hosts.filter((h) => !h.includes("<")),
    docs: "",
    region: "",
    model: "",
    defAgent: false,
    defFeat: false,
    genericSecret: type.secret ?? "",
    bearerSecret: "bedrock-api-key",
    awsKeyId: "aws-access-key-id",
    awsSecret: "aws-secret-access-key",
    awsSession: "aws-session-token",
    appIdSecret: "github-app-id",
    appKeySecret: "github-app-key",
    ...over,
  };
}

describe("buildIntegrationWrite — the real PUT payload satisfies the server (no put() mock)", () => {
  it("bedrock bearer (the DEFAULT lane): NO delivery, secret_name bedrock-api-key, no probe", () => {
    const body = buildIntegrationWrite(values("bedrock", { bedrockLane: "bearer" }));
    expect(serverRejection(body)).toBeNull();
    expect(body.secrets).toEqual([{ role: "api_key", secret_name: "bedrock-api-key" }]);
    expect(body.secrets![0].delivery).toBeUndefined(); // bespoke transport, not proxy_header
    expect(body.probe).toBeUndefined(); // AI kind — no auto-probe
  });

  it("bedrock static: aws-* names, no delivery, session-token row dropped when empty", () => {
    const full = buildIntegrationWrite(values("bedrock", { bedrockLane: "static" }));
    expect(serverRejection(full)).toBeNull();
    expect(full.secrets).toEqual([
      { role: "access_key_id", secret_name: "aws-access-key-id" },
      { role: "secret_access_key", secret_name: "aws-secret-access-key" },
      { role: "session_token", secret_name: "aws-session-token" },
    ]);
    const noSession = buildIntegrationWrite(values("bedrock", { bedrockLane: "static", awsSession: "  " }));
    expect(serverRejection(noSession)).toBeNull();
    expect(noSession.secrets).toEqual([
      { role: "access_key_id", secret_name: "aws-access-key-id" },
      { role: "secret_access_key", secret_name: "aws-secret-access-key" },
    ]);
    expect(noSession.probe).toBeUndefined();
  });

  it("anthropic api key: proxy_header with a valid header, no probe", () => {
    const body = buildIntegrationWrite(values("anthropic"));
    expect(serverRejection(body)).toBeNull();
    expect(body.secrets![0].delivery).toEqual({ mode: "proxy_header", header: "x-api-key", format: "%s" });
    expect(body.probe).toBeUndefined();
  });

  it("anthropic subscription (managed): no secret rows, lane config, no probe", () => {
    const body = buildIntegrationWrite(values("claude_subscription", { hostCli: false }));
    expect(serverRejection(body)).toBeNull();
    expect(body.secrets).toBeUndefined();
    expect(body.config).toEqual({ lane: "managed" });
    expect(body.probe).toBeUndefined();
  });

  it("openai: proxy_header Bearer, no probe", () => {
    const body = buildIntegrationWrite(values("openai"));
    expect(serverRejection(body)).toBeNull();
    expect(body.secrets![0].delivery).toEqual({ mode: "proxy_header", header: "Authorization", format: "Bearer %s" });
    expect(body.probe).toBeUndefined();
  });

  it("azure openai: proxy_header api-key on the operator-supplied host, never the <resource> template, no probe", () => {
    const body = buildIntegrationWrite(values("azure", { hostList: ["acme.openai.azure.com"] }));
    expect(serverRejection(body)).toBeNull();
    expect(body.egress).toEqual(["acme.openai.azure.com"]);
    expect(body.secrets![0].delivery).toEqual({ mode: "proxy_header", header: "api-key", format: "%s" });
    expect(body.probe).toBeUndefined();
  });

  it("github app: two bespoke (delivery-less) secret rows, and DOES get a probe (scm host)", () => {
    const body = buildIntegrationWrite(values("github"));
    expect(serverRejection(body)).toBeNull();
    expect(body.secrets).toEqual([
      { role: "app_id", secret_name: "github-app-id" },
      { role: "app_key", secret_name: "github-app-key" },
    ]);
    expect(body.probe).toEqual({ method: "GET", url: "https://github.com/" });
  });

  // W11-S1-4: gitlab (PAT-over-HTTPS) and gitssh (Git over SSH) share
  // apiType "git_host", but the server's capability matrix
  // (internal/api/integrations.go RoleSecret("pat")/RoleSecret("ssh_key"))
  // keys clone:pat vs clone:ssh off the secret's role — mislabeling an SSH
  // key as "pat" reports the wrong clone lane.
  it("git_host: PAT lane (gitlab) writes role pat, SSH lane (gitssh) writes role ssh_key", () => {
    const pat = buildIntegrationWrite(values("gitlab"));
    expect(serverRejection(pat)).toBeNull();
    expect(pat.secrets).toEqual([{ role: "pat", secret_name: "gitlab-pat" }]);

    const ssh = buildIntegrationWrite(values("gitssh", { hostList: ["git.corp.internal"] }));
    expect(serverRejection(ssh)).toBeNull();
    expect(ssh.secrets).toEqual([{ role: "ssh_key", secret_name: "ssh-key-git-corp-internal" }]);
  });

  it("a generic kind (artifactory): proxy_header secret + a probe, both server-valid", () => {
    const body = buildIntegrationWrite(values("artifactory"));
    expect(serverRejection(body)).toBeNull();
    expect(body.secrets![0].delivery).toEqual({ mode: "proxy_header", header: "Authorization", format: "Bearer %s" });
    expect(body.probe).toEqual({ method: "GET", url: "https://artifactory.corp.internal/" });
  });

  it("REGRESSION (defects 1+2): the old header-less proxy_header + bedrock-bearer-token payload is gone", () => {
    const body = buildIntegrationWrite(values("bedrock", { bedrockLane: "bearer" }));
    // Old: {delivery:{mode:"proxy_header"}} 400s ValidHeaderName(""); name
    // "bedrock-bearer-token" is one the bedrock transport never reads.
    expect(body.secrets![0].delivery).toBeUndefined();
    expect(body.secrets![0].secret_name).toBe("bedrock-api-key");
    expect(body.secrets![0].secret_name).not.toBe("bedrock-bearer-token");
  });

  // W11-S1-3: HOSTS_HINT says "wildcards are fine" — the auto-probe must never
  // contradict it by 400ing Save when the FIRST host happens to be one.
  // genericSecret: "" (credential-less) isolates this from the server's
  // SEPARATE, legitimate "a credential header needs a bare host" rejection
  // (integrations_write.go's bareExactHost check, which only fires once a
  // header secret is actually configured) — this defect is the auto-probe
  // rejecting a wildcard even with no credential in play at all.
  it("a credential-less generic kind with a wildcard FIRST host: the probe targets the first BARE host instead, never the wildcard", () => {
    const body = buildIntegrationWrite(
      values("other", { hostList: ["*.example.com", "api.example.com"], genericSecret: "" }),
    );
    expect(serverRejection(body)).toBeNull();
    expect(body.secrets).toBeUndefined();
    expect(body.probe).toEqual({ method: "GET", url: "https://api.example.com/" });
  });

  it("a credential-less generic kind where EVERY host is a wildcard: no probe at all, never a malformed one", () => {
    const body = buildIntegrationWrite(
      values("other", { hostList: ["*.example.com", "*.other.example.com"], genericSecret: "" }),
    );
    expect(serverRejection(body)).toBeNull();
    expect(body.probe).toBeUndefined();
  });
});
