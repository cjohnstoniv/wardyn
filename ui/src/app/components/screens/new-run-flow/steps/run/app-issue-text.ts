/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { NR_RUN } from "../../../../wardyn/copy/new-run/run";
import type { AppProblemCode } from "../../lenses/step-issues";

/** The sentence for one port-row problem, said on the row and, from the same issue, on Review. */
export function appIssueText(p: { code: AppProblemCode; name?: string; port?: number }): string {
  switch (p.code) {
    case "app_name_invalid": return NR_RUN.APP_NAME_INVALID;
    case "app_name_duplicate": return NR_RUN.APP_NAME_DUPLICATE(p.name ?? "");
    case "app_port_invalid": return NR_RUN.PORT_INVALID;
    case "app_port_duplicate": return NR_RUN.PORT_DUPLICATE(p.port ?? "");
    case "app_path_invalid": return NR_RUN.APP_PATH_INVALID;
  }
}
