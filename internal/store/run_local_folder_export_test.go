// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

// ContainmentDueSQL is containmentDueSQL with its grace at $1 and its max age at $2.
func ContainmentDueSQL() string { return containmentDueSQL(1, 2) }

// MaxAgeSecs is maxAgeSecs.
var MaxAgeSecs = maxAgeSecs
