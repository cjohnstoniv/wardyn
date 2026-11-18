/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";

// True on a run page whose signed-in person is the helper of an accepted support grant that opens the terminal
// door. The terminal then opens for them though they did not start the run, over the ticket lane (the server's
// support arm admits the ticket), and speaks as a helper: it never offers Take over. Default false: every
// other terminal is as it was.
export const SupportHelperContext = React.createContext(false);

export const useSupportHelper = (): boolean => React.useContext(SupportHelperContext);
