/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The secrets API's create-only refusal (internal/api reasons.go, reasonSecretExists): a PUT sent with
// If-None-Match: * named a secret its namespace already holds, so nothing was stored. A module of its own so the
// screens that react to it read it even where a test mocks lib/api/secrets.
export const SECRET_EXISTS_REASON = "secret_exists";
