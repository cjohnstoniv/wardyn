// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package planted

// Planted violation for recguard: a raw read of the recorder's events.
func plantedRawRead(h *harness) int { return len(h.audit.events) }
