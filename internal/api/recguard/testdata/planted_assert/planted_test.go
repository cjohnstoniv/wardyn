// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package planted

// Planted violation for recguard: a raw read through a type assertion.
func plantedAssertRead(srv *server) int { return len(srv.cfg.Audit.(*recRecorder).events) }
