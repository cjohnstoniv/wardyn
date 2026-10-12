/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { setField } from "../../../../../test/set-field";
import { TEMPLATE_REASON, TEMPLATE_REFUSAL } from "../../../../lib/template-refusals";
import type { Template, TemplatePublish, TemplateSaveRequest } from "../../../../lib/types/templates";
import type { RunIntent } from "../draft/run-intent";
import { TemplateSaveError } from "./save-as-dialog.api";
import { SAVE_AS_COPY, SaveAsDialog, type SaveAsDialogProps } from "./save-as-dialog";

afterEach(cleanup);

const MEMBER: TemplatePublish = { org: true, groups: [], groups_unverified: false };
const MANAGER: TemplatePublish = { org: false, groups: ["g-1"], group_labels: { "g-1": "Platform" }, groups_unverified: false };
const INTENT = {
  experience: "background", runner_pool_id: "p1", no_end: false,
  inline_policy: { allowed_domains: ["pypi.org"], eligible_grants: [], tool_rules: [] },
} as unknown as RunIntent;
// The two sentences the canon lacks: fixtures here, empty strings in the module.
const SEAM_COPY = { ...SAVE_AS_COPY, nothingPicked: "Fixture: pick a part.", ownDrivesLeftOut: "Fixture: own drives are left out." };
const SAVED = { id: "t1" } as Template;

function setup(over: Partial<SaveAsDialogProps> = {}) {
  const save = vi.fn<(r: TemplateSaveRequest) => Promise<Template>>().mockResolvedValue(SAVED);
  const props: SaveAsDialogProps = {
    open: true, onOpenChange: vi.fn(), kind: "template", intent: INTENT, hasBlockers: false, publish: MEMBER, save, onSaved: vi.fn(), ...over,
  };
  const user = userEvent.setup();
  render(<SaveAsDialog {...props} />);
  return { user, props, save };
}
const saveButton = (kind = "template") => screen.getByRole("button", { name: SAVE_AS_COPY.save(kind as "template") });
const sent = (save: ReturnType<typeof setup>["save"], call = 0) => save.mock.calls[call][0];

describe("SaveAsDialog", () => {
  it("opens with the focus on Name (U20)", async () => {
    setup();
    await waitFor(() => expect(screen.getByLabelText("Name")).toHaveFocus());
  });

  it("keeps Save off until the name is typed, and says the name is required", async () => {
    setup();
    expect(screen.getByText("Required")).toBeVisible();
    expect(saveButton()).toBeDisabled();
    setField(screen.getByLabelText("Name"), "Nightly");
    expect(saveButton()).toBeEnabled();
  });

  it("shows template_name_taken under Name, tied to the field, and clears it on the next keystroke", async () => {
    const { user, save } = setup();
    save.mockRejectedValueOnce(new TemplateSaveError(409, TEMPLATE_REFUSAL.NAME_TAKEN(), TEMPLATE_REASON.NAME_TAKEN, []));
    const name = screen.getByLabelText("Name");
    setField(name, "Nightly");
    await user.click(saveButton());
    const alert = await screen.findByText(TEMPLATE_REFUSAL.NAME_TAKEN());
    expect(name).toHaveAttribute("aria-invalid", "true");
    expect(name).toHaveAccessibleDescription(TEMPLATE_REFUSAL.NAME_TAKEN());
    expect(alert).toBe(name.closest("div")?.querySelector("#save-as-name-error"));
    setField(name, "Nightly2");
    expect(screen.queryByText(TEMPLATE_REFUSAL.NAME_TAKEN())).toBeNull();
    expect(saveButton()).toBeEnabled();
  });

  it("shows any other refusal as an alert and keeps the dialog open", async () => {
    const { user, save, props } = setup();
    save.mockRejectedValueOnce(new TemplateSaveError(409, TEMPLATE_REFUSAL.LIMIT_REACHED("100"), TEMPLATE_REASON.LIMIT_REACHED, []));
    setField(screen.getByLabelText("Name"), "Nightly");
    await user.click(saveButton());
    expect(await screen.findByRole("alert")).toHaveTextContent(TEMPLATE_REFUSAL.LIMIT_REACHED("100"));
    expect(props.onOpenChange).not.toHaveBeenCalled();
  });

  it("offers Only me alone, with the plain helper, to a person who manages no group", () => {
    setup();
    const options = screen.getAllByRole("option").map((o) => o.textContent);
    expect(options).toEqual([SAVE_AS_COPY.onlyMe]);
    expect(screen.getByText(SAVE_AS_COPY.helperPlain)).toBeVisible();
  });

  it("offers the managed groups, never the organisation, and saves to the chosen group", async () => {
    const { user, save, props } = setup({ publish: { ...MANAGER, org: true } });
    expect(screen.getAllByRole("option").map((o) => o.textContent)).toEqual([SAVE_AS_COPY.onlyMe, "Platform group"]);
    expect(screen.getByText(SAVE_AS_COPY.helperManager)).toBeVisible();
    await user.selectOptions(screen.getByLabelText("Available to"), "Platform group");
    expect(screen.getByText(SAVE_AS_COPY.disclosure)).toBeVisible();
    setField(screen.getByLabelText("Name"), "Team");
    await user.click(saveButton());
    expect(sent(save)).toMatchObject({ scope: "group", group_id: "g-1", name: "Team", kind: "template" });
    expect(props.onSaved).toHaveBeenCalledWith(SAVED);
    expect(props.onOpenChange).toHaveBeenCalledWith(false);
  });

  it("tells the person to sign in again, and offers no group, when the sign-in's groups are unverified", () => {
    setup({ publish: { ...MANAGER, groups_unverified: true } });
    expect(screen.getAllByRole("option")).toHaveLength(1);
    expect(screen.getByText(TEMPLATE_REFUSAL.GROUP_UNVERIFIED())).toBeVisible();
  });

  it("does not show the shared-template disclosure for Only me", () => {
    setup();
    expect(screen.queryByText(SAVE_AS_COPY.disclosure)).toBeNull();
  });

  it("saves the whole draft as full when nothing blocks it, keeping explicit empties", async () => {
    const { user, save } = setup();
    setField(screen.getByLabelText("Name"), "Nightly");
    await user.click(saveButton());
    const req = sent(save);
    expect(req.document.coverage).toBe("full");
    expect(req.document.intent).toEqual(INTENT);
    expect(JSON.stringify(req)).not.toMatch(/title|description/);
  });

  it("saves a draft with blockers as partial", async () => {
    const { user, save } = setup({ hasBlockers: true });
    setField(screen.getByLabelText("Name"), "Nightly");
    await user.click(saveButton());
    expect(sent(save).document.coverage).toBe("partial");
  });

  it("saves only the checked groups, as partial", async () => {
    const { user, save } = setup();
    await user.click(screen.getByLabelText(SAVE_AS_COPY.subset));
    await user.click(screen.getByLabelText(SAVE_AS_COPY.partLabels.where));
    await user.click(screen.getByLabelText(SAVE_AS_COPY.partLabels.access));
    setField(screen.getByLabelText("Name"), "Slim");
    await user.click(saveButton());
    const req = sent(save);
    expect(req.document.coverage).toBe("partial");
    expect(req.document.intent).toEqual({ experience: "background", no_end: false });
  });

  it("blocks Save, with the reason in view, when every group is unchecked", async () => {
    const { user } = setup({ copy: SEAM_COPY });
    await user.click(screen.getByLabelText(SAVE_AS_COPY.subset));
    for (const id of ["run", "where", "access", "limits"] as const) await user.click(screen.getByLabelText(SAVE_AS_COPY.partLabels[id]));
    setField(screen.getByLabelText("Name"), "None");
    expect(saveButton()).toBeDisabled();
    expect(screen.getByText(SEAM_COPY.nothingPicked)).toBeVisible();
  });

  it("keeps Save off with nothing checked even while the canon has no sentence for it, and invents none", async () => {
    const { user } = setup();
    expect(SAVE_AS_COPY.nothingPicked).toBe("");
    await user.click(screen.getByLabelText(SAVE_AS_COPY.subset));
    for (const id of ["run", "where", "access", "limits"] as const) await user.click(screen.getByLabelText(SAVE_AS_COPY.partLabels[id]));
    setField(screen.getByLabelText("Name"), "None");
    expect(saveButton()).toBeDisabled();
    expect(document.querySelectorAll("fieldset p")).toHaveLength(1); // the omitted-values line only
  });

  it("lists a left-out dependency under needs_setup, in the server's words, and retries once", async () => {
    const { user, save } = setup({ intent: { model_provider: "p", experience: "background" } as unknown as RunIntent });
    const path = "intent.model_provider";
    save.mockRejectedValueOnce(new TemplateSaveError(422, TEMPLATE_REFUSAL.DEPENDENCY_MISSING(path, "agent"), TEMPLATE_REASON.DEPENDENCY_MISSING,
      [{ path, reason: TEMPLATE_REASON.DEPENDENCY_MISSING, message: TEMPLATE_REFUSAL.DEPENDENCY_MISSING(path, "agent") }]));
    await user.click(screen.getByLabelText(SAVE_AS_COPY.subset));
    setField(screen.getByLabelText("Name"), "Dep");
    await user.click(saveButton());
    await waitFor(() => expect(save).toHaveBeenCalledTimes(2));
    expect(sent(save, 0).document.needs_setup).toBeUndefined();
    expect(sent(save, 1).document.needs_setup).toEqual([{ field: "agent" }]);
  });

  describe("kind policy", () => {
    it("saves only the draft's policy keys as a partial policy, with the policy checklist", async () => {
      const { user, save } = setup({ kind: "policy" });
      expect(screen.getByRole("heading", { name: "Save as policy" })).toBeVisible();
      await user.click(screen.getByLabelText(SAVE_AS_COPY.subset));
      expect(screen.getAllByRole("checkbox").map((c) => c.id)).toEqual(["save-as-part-network", "save-as-part-credentials"]);
      await user.click(screen.getByLabelText(SAVE_AS_COPY.partLabels.credentials));
      setField(screen.getByLabelText("Name"), "Pkg");
      await user.click(saveButton("policy"));
      const req = sent(save);
      expect(req).toMatchObject({ kind: "policy", document: { coverage: "partial", intent: { inline_policy: { allowed_domains: ["pypi.org"] } } } });
      expect(req.document.needs_setup).toBeUndefined();
    });

    it("saves everything as the allowed policy keys alone, never the draft's other fields or tool rules", async () => {
      const { user, save } = setup({ kind: "policy" });
      setField(screen.getByLabelText("Name"), "Pkg");
      await user.click(saveButton("policy"));
      expect(sent(save).document.intent).toEqual({ inline_policy: { allowed_domains: ["pypi.org"], eligible_grants: [] } });
    });

    it("blocks Save, with the reason in view, when the draft changed no policy", async () => {
      setup({ kind: "policy", intent: { experience: "background" } as RunIntent });
      setField(screen.getByLabelText("Name"), "Pkg");
      expect(saveButton("policy")).toBeDisabled();
      expect(screen.getByText(SAVE_AS_COPY.nothingChanged)).toBeVisible();
    });
  });

  describe("own drives in a shared template (DR5-L-privacy)", () => {
    const withDrives = { experience: "background", drives: [{ drive_id: "mine" }, { drive_id: "team" }] } as unknown as RunIntent;

    it("leaves them out of the checklist and the saved document, and says so, once the audience is shared", async () => {
      const { user, save } = setup({ publish: MANAGER, intent: withDrives, ownDriveIds: new Set(["mine"]), copy: SEAM_COPY });
      expect(screen.queryByText(SEAM_COPY.ownDrivesLeftOut)).toBeNull();
      await user.selectOptions(screen.getByLabelText("Available to"), "Platform group");
      expect(screen.getByText(SEAM_COPY.ownDrivesLeftOut)).toBeVisible();
      setField(screen.getByLabelText("Name"), "Team");
      await user.click(saveButton());
      expect((sent(save).document.intent as { drives: unknown }).drives).toEqual([{ drive_id: "team" }]);
    });

    it("still leaves them out of the document while the canon has no sentence for it", async () => {
      const { user, save } = setup({ publish: MANAGER, intent: withDrives, ownDriveIds: new Set(["mine"]) });
      expect(SAVE_AS_COPY.ownDrivesLeftOut).toBe("");
      await user.selectOptions(screen.getByLabelText("Available to"), "Platform group");
      setField(screen.getByLabelText("Name"), "Team");
      await user.click(saveButton());
      expect((sent(save).document.intent as { drives: unknown }).drives).toEqual([{ drive_id: "team" }]);
    });

    it("saves a shared template that left them out as partial", async () => {
      const { user, save } = setup({ publish: MANAGER, intent: withDrives, ownDriveIds: new Set(["mine"]) });
      await user.selectOptions(screen.getByLabelText("Available to"), "Platform group");
      setField(screen.getByLabelText("Name"), "Team");
      await user.click(saveButton());
      expect(sent(save).document.coverage).toBe("partial");
    });

    it("keeps them for Only me", async () => {
      const { user, save } = setup({ publish: MANAGER, intent: withDrives, ownDriveIds: new Set(["mine"]) });
      setField(screen.getByLabelText("Name"), "Mine");
      await user.click(saveButton());
      expect((sent(save).document.intent as { drives: unknown }).drives).toEqual([{ drive_id: "mine" }, { drive_id: "team" }]);
    });
  });
});
