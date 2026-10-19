// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// internalAudience is the audience run tokens are verified against on the
// internal (sidecar) endpoints (RFC 8707 discipline).
const internalAudience = "wardyn-internal"

// groundtruthAudience is the SEPARATE audience the host-scoped eBPF
// ground-truth sensor's token is verified against (RFC 8707 discipline). It is
// distinct from internalAudience on purpose: a token minted for this audience
// is accepted ONLY on POST /api/v1/internal/groundtruth (audit-write), and is
// rejected by every mint/approval endpoint (which verify internalAudience).
// This bounds the host sensor's authority to exactly "write ground-truth audit
// events" and nothing else.
const groundtruthAudience = "wardyn-groundtruth"
