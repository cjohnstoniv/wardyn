/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { resolve } from "node:path";
import { describe, expect, it } from "vitest";
import { parseFrozenTables } from "./copy-doc-parity";
import { MODEL_LEDE, MODEL_PROVIDERS as M, PROVIDER_EDITOR } from "./model-providers-copy";

// The canon doc (docs/design/model-providers-canon.md, packet MP-A) parsed back
// out and compared whole: a changed string, a new doc row or a dropped one all
// fail here. process.cwd() is the vitest root, ui/.
const doc = parseFrozenTables(
  resolve(process.cwd(), "../docs/design/model-providers-canon.md"),
  /^## (Frozen|Implementation) strings/,
);

const rendered: Record<string, string> = {
  "MODEL_PROVIDERS.TITLE": M.TITLE,
  MODEL_LEDE,
  "MODEL_PROVIDERS.ADD_CTA": M.ADD_CTA,
  "MODEL_PROVIDERS.EMPTY_TITLE": M.EMPTY_TITLE,
  "MODEL_PROVIDERS.EMPTY_BODY": M.EMPTY_BODY,
  ...Object.fromEntries(Object.entries(M.KIND).map(([k, v]) => [`MODEL_PROVIDERS.KIND.${k}`, v])),
  "MODEL_PROVIDERS.PROVIDES.SSO": M.PROVIDES.SSO,
  "MODEL_PROVIDERS.PROVIDES.TOKEN": M.PROVIDES.TOKEN,
  "MODEL_PROVIDERS.PROVIDES.KEY": M.PROVIDES.KEY,
  "PROVIDER_EDITOR.PROVIDES_CLAUDE": PROVIDER_EDITOR.PROVIDES_CLAUDE,
  "MODEL_PROVIDERS.USED_BY(harness)": M.USED_BY(["{harness}"]),
  "MODEL_PROVIDERS.USED_BY(harness, harness)": M.USED_BY(["Claude Code", "Codex CLI"]),
  "MODEL_PROVIDERS.CHIP_DEFAULT_FOR(harness)": M.CHIP_DEFAULT_FOR(["{harness}"]),
  "MODEL_PROVIDERS.CHIP_DEFAULT_FOR_BOTH": M.CHIP_DEFAULT_FOR(["Claude Code", "Codex CLI"]),
  "MODEL_PROVIDERS.CONNECTED(n)": M.CONNECTED(2).replace("2", "{n}"),
  "MODEL_PROVIDERS.CONNECTED(1)": M.CONNECTED(1),
  "MODEL_PROVIDERS.CONNECTED(0)": M.CONNECTED(0),
  "MODEL_PROVIDERS.CHIP_OFF": M.CHIP_OFF,
  "MODEL_PROVIDERS.OFF_LINE": M.OFF_LINE,
  "MODEL_PROVIDERS.OFF_STILL_DEFAULT(harness)": M.OFF_STILL_DEFAULT("{harness}"),
  "MODEL_PROVIDERS.UNUSED": M.UNUSED,
  "MODEL_PROVIDERS.HARNESS_UNSERVED(harness)": M.HARNESS_UNSERVED("{harness}"),
  "MODEL_PROVIDERS.FETCH_FAILED_TITLE": M.FETCH_FAILED_TITLE,
  "MODEL_PROVIDERS.FETCH_FAILED_BODY": M.FETCH_FAILED_BODY,
};

describe("model-providers-copy — docs/design/model-providers-canon.md", () => {
  it("renders every frozen string byte for byte, and no key the doc lacks", () => {
    expect(Object.fromEntries(doc)).toEqual(rendered);
  });

  it("renders a plural count with the number itself", () => {
    expect(M.CONNECTED(12)).toBe("Connected by 12 people");
  });
});
